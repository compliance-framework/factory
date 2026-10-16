package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
)

// MaxWebhookBytes bounds a posted webhook body, like the API server's body limit.
const MaxWebhookBytes = 1 << 20

// Webhook is a scripted delivery: the body of a fake webhook, which Normalise turns into
// Events. Tests build it directly.
type Webhook struct {
	// Delivery is the provider's delivery ID; one is generated when empty. The RawEvent's ID
	// is DeliveryPrefix + Delivery.
	Delivery string             `json:"delivery,omitzero"`
	Type     v1alpha1.EventType `json:"type"`
	Sender   Sender             `json:"sender,omitzero"`
	// Object is the event's object. FanOut adds more: one Event per object, numbered in ref
	// order (like GitHub's check_suite → one Event per PR).
	Object *v1alpha1.ObjectRef  `json:"object,omitzero"`
	FanOut []v1alpha1.ObjectRef `json:"fanOut,omitzero"`
	// Repo defaults to the object's repo.
	Repo       string         `json:"repo,omitzero"`
	Subject    string         `json:"subject,omitzero"`
	ContentRef string         `json:"contentRef,omitzero"`
	Facts      map[string]any `json:"facts,omitzero"`
	// Time defaults to when the delivery was received.
	Time time.Time `json:"time,omitzero"`
}

// Sender is who caused a delivery.
type Sender struct {
	Login string `json:"login"`
	Bot   bool   `json:"bot,omitzero"`
	// Association is GitHub's author association (MEMBER, CONTRIBUTOR, …), for trust rules.
	Association string `json:"association,omitzero"`
	// Tier, when set, is used as is instead of the Provider's trust rules.
	Tier string `json:"tier,omitzero"`
}

// RawEvent builds the delivery for w without recording or sending it.
func (c *Conn) RawEvent(w Webhook) (provider.RawEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rawEventLocked(w, nil)
}

func (c *Conn) rawEventLocked(w Webhook, headers http.Header) (provider.RawEvent, error) {
	if w.Delivery == "" {
		c.seq++
		w.Delivery = strconv.Itoa(c.seq)
	}
	body, err := json.Marshal(w)
	if err != nil {
		return provider.RawEvent{}, fmt.Errorf("fake: encode webhook: %w", err)
	}
	return provider.RawEvent{
		ID:         DeliveryPrefix + w.Delivery,
		Provider:   c.p.Metadata.Name,
		Type:       string(w.Type),
		Headers:    headers,
		Body:       body,
		ReceivedAt: c.opts.Clock.Now(),
	}, nil
}

// Emit delivers each webhook to sink in order, as the provider would, and then records it
// for Recover (whatever Accept returned: the provider keeps failed deliveries too). It stops at the first error Accept returns (ErrDuplicateDelivery included) and
// returns the deliveries made so far.
func (c *Conn) Emit(ctx context.Context, sink provider.IngressSink, ws ...Webhook) ([]provider.RawEvent, error) {
	out := make([]provider.RawEvent, 0, len(ws))
	for _, w := range ws {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		c.mu.Lock()
		ev, err := c.rawEventLocked(w, nil)
		c.mu.Unlock()
		if err != nil {
			return out, err
		}
		err = sink.Accept(ctx, cloneRawEvent(ev))
		c.record(ev)
		if err != nil {
			return out, fmt.Errorf("fake: emit %s: %w", ev.ID, err)
		}
		out = append(out, ev)
	}
	return out, nil
}

// record adds a delivery to the log Recover reads, once per ID. Handler and Emit record a
// delivery after Accept returns, so a concurrent Recover never delivers it first.
func (c *Conn) record(ev provider.RawEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.ContainsFunc(c.deliveries, func(d provider.RawEvent) bool { return d.ID == ev.ID }) {
		c.deliveries = append(c.deliveries, cloneRawEvent(ev))
	}
}

// Undelivered records webhooks the provider made but the dispatcher never received (the
// restart gap): only Recover delivers them.
func (c *Conn) Undelivered(ws ...Webhook) ([]provider.RawEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]provider.RawEvent, 0, len(ws))
	for _, w := range ws {
		ev, err := c.rawEventLocked(w, nil)
		if err != nil {
			return out, err
		}
		c.deliveries = append(c.deliveries, cloneRawEvent(ev))
		out = append(out, ev)
	}
	return out, nil
}

// Deliveries returns every delivery the provider made (emitted, posted or undelivered), in
// order.
func (c *Conn) Deliveries() []provider.RawEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]provider.RawEvent, len(c.deliveries))
	for i, ev := range c.deliveries {
		out[i] = cloneRawEvent(ev)
	}
	return out
}

// Handler implements provider.Conn. It accepts a POSTed Webhook as JSON (there is no
// signature to verify), calls sink.Accept and only then answers: 202, 200 for a duplicate
// delivery, 503 when the sink fails. A bad method is 405, a bad body 400.
func (c *Conn) Handler(sink provider.IngressSink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWebhookBytes))
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		hook, err := decodeWebhook(body)
		if err != nil {
			http.Error(w, "invalid webhook", http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		ev, err := c.rawEventLocked(hook, r.Header.Clone())
		c.mu.Unlock()
		if err != nil {
			http.Error(w, "invalid webhook", http.StatusBadRequest)
			return
		}
		err = sink.Accept(r.Context(), cloneRawEvent(ev))
		c.record(ev)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusAccepted)
		case errors.Is(err, provider.ErrDuplicateDelivery):
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "delivery not stored", http.StatusServiceUnavailable)
		}
	})
}

// Recover implements provider.Conn: it sends every recorded delivery received at or after
// since to sink, skipping the ones the sink reports as duplicates.
func (c *Conn) Recover(ctx context.Context, since time.Time, sink provider.IngressSink) error {
	if err := c.begin(ctx, MethodRecover); err != nil {
		return err
	}
	var todo []provider.RawEvent
	for _, ev := range c.deliveries {
		if !ev.ReceivedAt.Before(since) {
			todo = append(todo, cloneRawEvent(ev))
		}
	}
	c.mu.Unlock()
	for _, ev := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sink.Accept(ctx, ev); err != nil && !errors.Is(err, provider.ErrDuplicateDelivery) {
			return fmt.Errorf("fake: recover %s: %w", ev.ID, err)
		}
	}
	return nil
}

func decodeWebhook(body []byte) (Webhook, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var w Webhook
	if err := dec.Decode(&w); err != nil {
		return Webhook{}, fmt.Errorf("decode webhook: %w: %w", err, provider.ErrInvalid)
	}
	if dec.More() {
		return Webhook{}, fmt.Errorf("decode webhook: trailing data: %w", provider.ErrInvalid)
	}
	return w, nil
}

// Normalise implements provider.Conn. Each object of the webhook (Object, then FanOut, sorted
// by ref) becomes one Event with ID EventID(ev.ID, n); a webhook without objects becomes one
// Event. The actor's ID is "<provider>:<login>", its tier comes from Sender.Tier or the
// Provider's trust rules (DefaultTier when none matches), and a factory App login sets
// IsFactory and Identity.
func (c *Conn) Normalise(ctx context.Context, ev provider.RawEvent) ([]v1alpha1.Event, error) {
	if err := c.begin(ctx, MethodNormalise); err != nil {
		return nil, err
	}
	p := c.p
	ptype := c.providerTypeLocked()
	c.mu.Unlock()

	if ev.Provider != p.Metadata.Name {
		return nil, fmt.Errorf("fake: normalise %s: delivery is for provider %q, not %q: %w",
			ev.ID, ev.Provider, p.Metadata.Name, provider.ErrInvalid)
	}
	w, err := decodeWebhook(ev.Body)
	if err != nil {
		return nil, fmt.Errorf("fake: normalise %s: %w", ev.ID, err)
	}
	if !w.Type.Known() || !w.Type.EmittedBy(ptype) {
		return nil, fmt.Errorf("fake: normalise %s: provider type %q doesn't emit %q: %w",
			ev.ID, ptype, w.Type, provider.ErrInvalid)
	}
	actor, err := c.actor(ctx, p, w.Sender)
	if err != nil {
		return nil, fmt.Errorf("fake: normalise %s: %w", ev.ID, err)
	}

	var objects []*v1alpha1.ObjectRef
	if w.Object != nil {
		objects = append(objects, cloneRef(w.Object))
	}
	for i := range w.FanOut {
		objects = append(objects, cloneRef(&w.FanOut[i]))
	}
	for _, o := range objects {
		if o.Provider == "" {
			o.Provider = p.Metadata.Name
		}
	}
	slices.SortStableFunc(objects, func(a, b *v1alpha1.ObjectRef) int { return strings.Compare(a.Ref, b.Ref) })
	if len(objects) == 0 {
		objects = []*v1alpha1.ObjectRef{nil}
	}

	at := w.Time
	if at.IsZero() {
		at = ev.ReceivedAt
	}
	out := make([]v1alpha1.Event, len(objects))
	for n, obj := range objects {
		repo, subject := w.Repo, w.Subject
		if obj != nil {
			if repo == "" {
				repo = RepoOf(obj.Ref)
			}
			if subject == "" {
				subject = obj.Ref
			}
		}
		var a *v1alpha1.Actor
		if actor != nil {
			cp := *actor
			a = &cp
		}
		out[n] = v1alpha1.Event{
			TypeMeta: v1alpha1.TypeMeta{Kind: "Event"},
			ID:       provider.EventID(ev.ID, n),
			Source:   p.Metadata.Name,
			Type:     w.Type,
			Subject:  subject,
			Time:     at.UTC(),
			Data: v1alpha1.EventData{
				Provider:   p.Metadata.Name,
				Actor:      a,
				Object:     obj,
				Repo:       repo,
				ContentRef: w.ContentRef,
				Facts:      cloneMap(w.Facts),
			},
		}
	}
	return out, nil
}

// actor resolves the sender into an Actor; nil when the webhook has no sender.
func (c *Conn) actor(ctx context.Context, p v1alpha1.Provider, s Sender) (*v1alpha1.Actor, error) {
	if s.Login == "" {
		return nil, nil
	}
	a := &v1alpha1.Actor{ID: p.Metadata.Name + ":" + s.Login, IsBot: s.Bot}
	if id, ok := c.opts.Identities[s.Login]; ok {
		a.IsFactory, a.Identity, a.IsBot = true, id, true
	}
	a.Tier = s.Tier
	if a.Tier == "" {
		tier, err := c.tier(ctx, p, s)
		if err != nil {
			return nil, err
		}
		a.Tier = tier
	}
	return a, nil
}

// tier is the first trust rule the sender matches (its matchers ANDed; a rule with none
// matches anyone), else DefaultTier. workspaceMember is not modelled: such a rule never
// matches.
func (c *Conn) tier(ctx context.Context, p v1alpha1.Provider, s Sender) (string, error) {
	for _, r := range p.Spec.Trust {
		ok, err := c.matchTrust(ctx, p, r, s)
		if err != nil {
			return "", err
		}
		if ok {
			return r.Tier, nil
		}
	}
	return DefaultTier, nil
}

func (c *Conn) matchTrust(ctx context.Context, p v1alpha1.Provider, r v1alpha1.TrustRule, s Sender) (bool, error) {
	if r.WorkspaceMember {
		return false, nil
	}
	if len(r.AuthorAssociation) > 0 && !slices.Contains(r.AuthorAssociation, s.Association) {
		return false, nil
	}
	if len(r.Bots) > 0 && !slices.Contains(r.Bots, s.Login) {
		return false, nil
	}
	if r.Group != "" {
		return c.inGroup(ctx, p, r.Group, s.Login)
	}
	return true, nil
}

// inGroup reports whether login is in the catalog Group named group, through its member
// sources on this provider.
func (c *Conn) inGroup(ctx context.Context, p v1alpha1.Provider, group, login string) (bool, error) {
	if c.opts.Groups == nil {
		return false, nil
	}
	g, ok := c.opts.Groups(group)
	if !ok {
		return false, nil
	}
	for _, src := range g.Members {
		if src.Provider != p.Metadata.Name {
			continue
		}
		members, err := c.ResolveGroup(ctx, src)
		if errors.Is(err, provider.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("resolve group %q: %w", group, err)
		}
		if slices.Contains(members, login) {
			return true, nil
		}
	}
	return false, nil
}
