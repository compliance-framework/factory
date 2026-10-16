package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/store"
)

// KindDelivery is a raw provider delivery, stored before the webhook is acknowledged and
// normalised into Events from the store (controllers.md §provider).
const KindDelivery = "Delivery"

// Delivery phases.
const (
	DeliveryReceived   = "received"
	DeliveryNormalised = "normalised"
	// DeliveryFailed is a delivery the adapter rejected as invalid: it is never retried.
	DeliveryFailed = "failed"
)

// DefaultDeliveryRetention is how long a handled Delivery record is kept. Duplicates arriving
// later are still deduped by their Events' ids.
const DefaultDeliveryRetention = 24 * time.Hour

// Delivery is the internal Delivery record: the RawEvent as received, then its outcome. Once
// handled, the body and headers are dropped (they are untrusted and may be large).
type Delivery struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
	Spec     provider.RawEvent   `json:"spec"`
	Status   DeliveryStatus      `json:"status"`
}

// DeliveryStatus is what became of a delivery.
type DeliveryStatus struct {
	Phase string `json:"phase"`
	// Events are the ids of the Events it was normalised into.
	Events []string `json:"events,omitzero"`
	// Reason is why it failed.
	Reason    string    `json:"reason,omitzero"`
	HandledAt time.Time `json:"handledAt,omitzero"`
}

var _ store.MetaObject = (*Delivery)(nil)

// GetName returns metadata.name (the delivery id).
func (d *Delivery) GetName() string { return d.Metadata.Name }

// GetObjectMeta implements store.MetaObject.
func (d *Delivery) GetObjectMeta() *v1alpha1.ObjectMeta { return &d.Metadata }

// CatalogSnapshotter is the live catalog and a channel closed at its next swap
// (*CatalogSync).
type CatalogSnapshotter interface {
	CatalogProvider
	Snapshot() (*catalog.Catalog, <-chan struct{})
}

// ProviderOptions configure the provider controller. Zero fields take the defaults.
type ProviderOptions struct {
	Workers int
	Clock   clock.Clock
	Logger  *slog.Logger
	// Secrets resolve the Providers' secretRefs (FACTORY_SECRET_<NAME>_<KEY>).
	Secrets provider.SecretResolver
	// DeliveryRetention is how long a handled Delivery is kept. Default
	// DefaultDeliveryRetention.
	DeliveryRetention time.Duration
}

// ProviderController connects one Conn per catalog Provider through a provider.Registry
// (reconnecting a Provider whose definition changed, dropping a removed one), is the
// ConnSource of the other controllers and the API server, and is the webhooks' IngressSink:
// Accept stores a Delivery record, and its reconcile normalises the Delivery into Events with
// the Provider's Conn. Its key is the delivery id.
type ProviderController struct {
	s    store.Store
	cat  CatalogSnapshotter
	reg  *provider.Registry
	opts ProviderOptions
	log  *slog.Logger
	ctrl *Controller

	mu    sync.RWMutex
	conns map[string]connEntry
}

type connEntry struct {
	conn   provider.Conn
	digest string
}

var (
	_ ConnSource           = (*ProviderController)(nil)
	_ provider.IngressSink = (*ProviderController)(nil)
)

// NewProviderController returns the provider controller, watching Deliveries.
func NewProviderController(s store.Store, cat CatalogSnapshotter, reg *provider.Registry, opts ProviderOptions) (*ProviderController, error) {
	if reg == nil {
		return nil, errors.New("provider controller: no registry")
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Secrets == nil {
		opts.Secrets = provider.SecretResolverFunc(func(_ context.Context, ref v1alpha1.SecretRef) ([]byte, error) {
			return nil, fmt.Errorf("secret %s/%s: no secret resolver", ref.Name, ref.Key)
		})
	}
	if opts.DeliveryRetention <= 0 {
		opts.DeliveryRetention = DefaultDeliveryRetention
	}
	p := &ProviderController{
		s: s, cat: cat, reg: reg, opts: opts,
		log:   opts.Logger.With("controller", "provider"),
		conns: map[string]connEntry{},
	}
	p.ctrl = NewController("provider", p, Options{Workers: opts.Workers, Clock: opts.Clock, Logger: opts.Logger})
	if err := p.ctrl.Watch(s, KindDelivery, nil, InformerOptions{}); err != nil {
		return nil, err
	}
	return p, nil
}

// Controller returns the underlying controller.
func (p *ProviderController) Controller() *Controller { return p.ctrl }

// Conn implements ConnSource.
func (p *ProviderController) Conn(name string) (provider.Conn, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.conns[name]
	return e.conn, ok
}

// Names returns the connected Providers, sorted.
func (p *ProviderController) Names() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.conns))
	for n := range p.conns {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Connect connects every Provider of the live catalog. It is the startup step: the first
// Provider that can't be connected (no adapter for its type, a missing secret) fails it, and
// nothing is connected then. Run keeps the connections in line with later catalogs.
func (p *ProviderController) Connect(ctx context.Context) error {
	cat := p.cat.Current()
	if cat == nil {
		return errors.New("connect providers: no catalog loaded yet")
	}
	next := map[string]connEntry{}
	for _, name := range sortedNames(cat.Providers) {
		pr := cat.Providers[name]
		c, err := p.reg.Connect(ctx, *pr, p.opts.Secrets)
		if err != nil {
			if errors.Is(err, provider.ErrUnknownType) {
				return fmt.Errorf("%w (registered adapter types: %v)", err, p.reg.Types())
			}
			return err
		}
		next[name] = connEntry{conn: c, digest: providerDigest(pr)}
	}
	p.mu.Lock()
	p.conns = next
	p.mu.Unlock()
	return nil
}

// sync reconnects the Providers of cat that are new or changed and drops the removed ones. A
// Provider that fails to connect keeps its previous connection (logged).
func (p *ProviderController) sync(ctx context.Context, cat *catalog.Catalog) {
	if cat == nil {
		return
	}
	p.mu.RLock()
	cur := maps.Clone(p.conns)
	p.mu.RUnlock()
	next := map[string]connEntry{}
	for _, name := range sortedNames(cat.Providers) {
		pr := cat.Providers[name]
		d := providerDigest(pr)
		if old, ok := cur[name]; ok && old.digest == d {
			next[name] = old
			continue
		}
		c, err := p.reg.Connect(ctx, *pr, p.opts.Secrets)
		if err != nil {
			p.log.Error("connect provider", "provider", name, "err", err)
			if old, ok := cur[name]; ok {
				next[name] = old
			}
			continue
		}
		p.log.Info("provider connected", "provider", name, "type", pr.Spec.Type)
		next[name] = connEntry{conn: c, digest: d}
	}
	for name := range cur {
		if _, ok := next[name]; !ok {
			p.log.Info("provider removed", "provider", name)
		}
	}
	p.mu.Lock()
	p.conns = next
	p.mu.Unlock()
}

func providerDigest(pr *v1alpha1.Provider) string {
	data, err := json.Marshal(pr.Spec)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Recover asks every connection to re-ingest the deliveries it made since since (the restart
// gap). Failures are logged and joined; the deliveries the store already has are skipped.
func (p *ProviderController) Recover(ctx context.Context, since time.Time) error {
	var errs []error
	for _, name := range p.Names() {
		c, ok := p.Conn(name)
		if !ok {
			continue
		}
		if err := c.Recover(ctx, since, p); err != nil {
			p.log.Warn("recover deliveries", "provider", name, "err", err)
			errs = append(errs, fmt.Errorf("recover %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Run normalises deliveries and follows the catalog (reconnecting changed Providers) until
// ctx is done.
func (p *ProviderController) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			cat, changed := p.cat.Snapshot()
			p.sync(ctx, cat)
			select {
			case <-ctx.Done():
				return
			case <-changed:
			}
		}
	}()
	err := p.ctrl.Run(ctx)
	wg.Wait()
	return err
}

// Accept implements provider.IngressSink: it stores the delivery as a Delivery record named
// by its id. A delivery already stored is provider.ErrDuplicateDelivery.
func (p *ProviderController) Accept(ctx context.Context, ev provider.RawEvent) error {
	if ev.ID == "" || ev.Provider == "" {
		return fmt.Errorf("accept delivery: no id or provider: %w", provider.ErrInvalid)
	}
	if ev.ReceivedAt.IsZero() {
		ev.ReceivedAt = p.opts.Clock.Now()
	}
	d := &Delivery{
		TypeMeta: v1alpha1.TypeMeta{Kind: KindDelivery},
		Metadata: v1alpha1.ObjectMeta{Name: ev.ID},
		Spec:     ev,
		Status:   DeliveryStatus{Phase: DeliveryReceived},
	}
	err := p.s.Create(ctx, d)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrAlreadyExists):
		return fmt.Errorf("delivery %s: %w", ev.ID, provider.ErrDuplicateDelivery)
	default:
		return fmt.Errorf("store delivery %s: %w", ev.ID, err)
	}
}

// Reconcile implements Reconciler for one delivery id: it normalises a received delivery
// into Events (each created once), marks it handled, and deletes it after the retention.
func (p *ProviderController) Reconcile(ctx context.Context, id string) (Result, error) {
	var d Delivery
	err := p.s.Get(ctx, store.Key{Kind: KindDelivery, Name: id}, &d)
	if errors.Is(err, store.ErrNotFound) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("get delivery %s: %w", id, err)
	}
	if d.Status.Phase != DeliveryReceived {
		return p.expire(ctx, &d)
	}
	conn, ok := p.Conn(d.Spec.Provider)
	if !ok {
		return Result{}, fmt.Errorf("delivery %s: no connection for provider %q", id, d.Spec.Provider)
	}
	evs, err := conn.Normalise(ctx, d.Spec)
	switch {
	case errors.Is(err, provider.ErrInvalid), errors.Is(err, provider.ErrUnsupported):
		p.log.Warn("delivery rejected", "delivery", id, "provider", d.Spec.Provider, "err", err)
		d.Status = DeliveryStatus{Phase: DeliveryFailed, Reason: err.Error()}
	case err != nil:
		return Result{}, fmt.Errorf("normalise delivery %s: %w", id, err)
	default:
		ids := make([]string, 0, len(evs))
		for i := range evs {
			if err := p.s.Create(ctx, &evs[i]); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
				return Result{}, fmt.Errorf("store event %s: %w", evs[i].ID, err)
			}
			ids = append(ids, evs[i].ID)
		}
		p.log.Debug("delivery normalised", "delivery", id, "provider", d.Spec.Provider, "events", len(ids))
		d.Status = DeliveryStatus{Phase: DeliveryNormalised, Events: ids}
	}
	d.Status.HandledAt = p.opts.Clock.Now()
	d.Spec.Body, d.Spec.Headers = nil, nil
	if err := p.s.Update(ctx, &d); err != nil {
		return Result{}, fmt.Errorf("mark delivery %s %s: %w", id, d.Status.Phase, err)
	}
	return Result{RequeueAfter: p.opts.DeliveryRetention}, nil
}

// expire deletes a handled delivery once it is older than the retention, else requeues it
// for then.
func (p *ProviderController) expire(ctx context.Context, d *Delivery) (Result, error) {
	at := d.Status.HandledAt
	if at.IsZero() {
		at = d.Spec.ReceivedAt
	}
	left := at.Add(p.opts.DeliveryRetention).Sub(p.opts.Clock.Now())
	if left > 0 {
		return Result{RequeueAfter: left}, nil
	}
	if err := p.s.Delete(ctx, store.Key{Kind: KindDelivery, Name: d.Metadata.Name}); err != nil && !errors.Is(err, store.ErrNotFound) {
		return Result{}, fmt.Errorf("delete delivery %s: %w", d.Metadata.Name, err)
	}
	return Result{}, nil
}
