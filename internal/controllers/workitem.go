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
	"strings"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/store"
)

// Work-item controller defaults.
const (
	// DefaultResyncInterval is how often a work item's links are re-observed when their
	// Provider sets no resync.every.
	DefaultResyncInterval = 10 * time.Minute
	// DefaultRebuildTimeout bounds the startup rebuild.
	DefaultRebuildTimeout = 5 * time.Minute
)

// WorkItemOptions configure the work-item controller. Zero fields take the defaults.
type WorkItemOptions struct {
	Workers int
	// Clock times resyncs and retention. Default the wall clock.
	Clock clock.Clock
	// Logger. Default slog.Default().
	Logger *slog.Logger
	// Cache is the observation cache, shared with the rule controller (resync refreshes it).
	// Default a new one.
	Cache *ObservationCache
	// Retention: a work item whose links are all closed is deleted this long after its last
	// activity (`--retention`). Default store.DefaultRetention (90 days).
	Retention time.Duration
	// ResyncInterval applies when a Provider has no resync.every. Default 10 m.
	ResyncInterval time.Duration
	// RebuildTimeout bounds Rebuild. Default 5 m.
	RebuildTimeout time.Duration
	// LockTTL is the work item lock's lease. Default DefaultLockTTL.
	LockTTL time.Duration
}

// WorkItemController keeps work items and their provider objects in line
// (controllers.md §work-item). Per work item (its key is the id) it:
//
//   - projects state, counters and links onto every linked object (Conn.Project) when the
//     projection changed;
//   - re-observes the links every resync interval (refreshing the observation cache) and
//     emits factory.workitem.resynced when an observation changed out of band;
//   - deletes the item once all its links are closed and its last activity is older than the
//     retention (the store never sweeps WorkItems).
//
// Rebuild recreates the work items from the providers' projections on startup.
type WorkItemController struct {
	s       store.Store
	cat     CatalogProvider
	conns   ConnSource
	factory *FactoryProvider
	opts    WorkItemOptions
	log     *slog.Logger
	ctrl    *Controller

	mu         sync.Mutex
	projected  map[string]string    // wi|link → digest of the last projection written
	observed   map[string]string    // wi|link → digest of the last observation
	lastResync map[string]time.Time // wi → when its links were last observed
}

// NewWorkItemController returns the work-item controller, watching WorkItems.
func NewWorkItemController(s store.Store, cat CatalogProvider, conns ConnSource, factory *FactoryProvider, opts WorkItemOptions) (*WorkItemController, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Cache == nil {
		opts.Cache = NewObservationCache(conns, opts.Clock, 0)
	}
	if opts.Retention <= 0 {
		opts.Retention = store.DefaultRetention
	}
	if opts.ResyncInterval <= 0 {
		opts.ResyncInterval = DefaultResyncInterval
	}
	if opts.RebuildTimeout <= 0 {
		opts.RebuildTimeout = DefaultRebuildTimeout
	}
	if opts.LockTTL <= 0 {
		opts.LockTTL = DefaultLockTTL
	}
	w := &WorkItemController{
		s: s, cat: cat, conns: conns, factory: factory, opts: opts,
		log:        opts.Logger.With("controller", "work-item"),
		projected:  map[string]string{},
		observed:   map[string]string{},
		lastResync: map[string]time.Time{},
	}
	w.ctrl = NewController("work-item", w, Options{Workers: opts.Workers, Clock: opts.Clock, Logger: opts.Logger})
	if err := w.ctrl.Watch(s, v1alpha1.KindWorkItem, nil, InformerOptions{}); err != nil {
		return nil, err
	}
	return w, nil
}

// Controller returns the underlying controller.
func (w *WorkItemController) Controller() *Controller { return w.ctrl }

// Run runs the controller until ctx is done.
func (w *WorkItemController) Run(ctx context.Context) error { return w.ctrl.Run(ctx) }

func linkKey(wi string, l v1alpha1.Link) string { return wi + "|" + cacheKey(l) }

func digestOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// forget drops what the controller remembers of a work item.
func (w *WorkItemController) forget(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.lastResync, id)
	for _, m := range []map[string]string{w.projected, w.observed} {
		for k := range m {
			if strings.HasPrefix(k, id+"|") {
				delete(m, k)
			}
		}
	}
}

// Reconcile implements Reconciler for one work item.
func (w *WorkItemController) Reconcile(ctx context.Context, id string) (Result, error) {
	var wi v1alpha1.WorkItem
	err := w.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: id}, &wi)
	if errors.Is(err, store.ErrNotFound) {
		w.forget(id)
		return Result{}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("get work item %s: %w", id, err)
	}
	if err := w.project(ctx, &wi); err != nil {
		return Result{}, err
	}
	interval := w.resyncInterval(&wi)
	now := w.opts.Clock.Now()
	w.mu.Lock()
	last, seen := w.lastResync[id]
	w.mu.Unlock()
	if seen && now.Before(last.Add(interval)) {
		return Result{RequeueAfter: last.Add(interval).Sub(now)}, nil
	}
	deleted, err := w.resync(ctx, &wi, now)
	if err != nil || deleted {
		return Result{}, err
	}
	w.mu.Lock()
	w.lastResync[id] = now
	w.mu.Unlock()
	return Result{RequeueAfter: interval}, nil
}

// projection is what the work item writes onto its linked objects.
func projection(wi *v1alpha1.WorkItem) provider.Projection {
	p := provider.Projection{WorkItemID: wi.Metadata.Name, Links: slices.Clone(wi.Spec.Links)}
	if st := wi.Status; st != nil {
		if len(st.State) > 0 {
			p.State, _ = jsonValue(st.State).(map[string]any)
		}
		if len(st.Counters) > 0 {
			p.Counters = maps.Clone(st.Counters)
		}
	}
	return p
}

// project writes the projection onto every link whose provider is connected, when it changed
// since the last write. An object that no longer exists is skipped.
func (w *WorkItemController) project(ctx context.Context, wi *v1alpha1.WorkItem) error {
	p := projection(wi)
	d := digestOf(p)
	for _, l := range wi.Spec.Links {
		if l.Provider == v1alpha1.FactoryProvider {
			continue
		}
		conn, ok := w.conns.Conn(l.Provider)
		if !ok {
			continue
		}
		k := linkKey(wi.Metadata.Name, l)
		w.mu.Lock()
		same := w.projected[k] == d
		w.mu.Unlock()
		if same {
			continue
		}
		err := conn.Project(ctx, v1alpha1.ObjectRef{Provider: l.Provider, Kind: l.Kind, Ref: l.Ref}, p)
		switch {
		case isNotFound(err):
			w.log.InfoContext(ctx, "projection skipped: object gone", "workItem", wi.Metadata.Name, "ref", l.Ref)
		case err != nil:
			return fmt.Errorf("project %s onto %s: %w", wi.Metadata.Name, l.Ref, err)
		}
		w.mu.Lock()
		w.projected[k] = d
		w.mu.Unlock()
	}
	return nil
}

// resyncInterval is the shortest resync.every of the item's providers, else the default.
func (w *WorkItemController) resyncInterval(wi *v1alpha1.WorkItem) time.Duration {
	interval := time.Duration(0)
	if cat := w.cat.Current(); cat != nil {
		for _, l := range wi.Spec.Links {
			p := cat.Providers[l.Provider]
			if p == nil || p.Spec.Resync == nil {
				continue
			}
			if d, err := p.Spec.Resync.Every.Parse(); err == nil && d > 0 && (interval == 0 || d < interval) {
				interval = d
			}
		}
	}
	if interval == 0 {
		interval = w.opts.ResyncInterval
	}
	return interval
}

// observationDigest digests what an out-of-band change can move (not the timestamp, nor the
// projections the factory itself writes).
func observationDigest(o provider.Observation) string {
	o.UpdatedAt = time.Time{}
	o.Projections = nil
	return digestOf(o)
}

// resync re-observes the item's links. When an observation changed since the last resync it
// emits factory.workitem.resynced; when every link is closed and the item has been idle past
// the retention it deletes the item (true).
func (w *WorkItemController) resync(ctx context.Context, wi *v1alpha1.WorkItem, now time.Time) (bool, error) {
	var changed []string
	open := false
	digests := map[string]string{}
	for _, l := range wi.Spec.Links {
		if _, ok := w.conns.Conn(l.Provider); !ok {
			open = true // can't tell: keep the item
			continue
		}
		obs, err := w.opts.Cache.Refresh(ctx, l)
		k := linkKey(wi.Metadata.Name, l)
		switch {
		case isNotFound(err):
			digests[k] = "gone"
		case err != nil:
			return false, fmt.Errorf("resync %s: %w", l.Ref, err)
		default:
			digests[k] = observationDigest(obs)
			if obs.State == provider.StateOpen {
				open = true
			}
		}
		w.mu.Lock()
		prev, ok := w.observed[k]
		w.observed[k] = digests[k]
		w.mu.Unlock()
		if ok && prev != digests[k] {
			changed = append(changed, l.Provider+"/"+l.Ref)
		}
	}
	if len(changed) > 0 {
		if _, err := w.factory.Emit(ctx, FactoryEvent{
			Type: v1alpha1.EventFactoryWorkItemResynced, WorkItem: wi.Metadata.Name,
			Key:   "resync/" + wi.Metadata.Name + "/" + digestOf(digests)[:16],
			Facts: map[string]any{"resync": map[string]any{"changed": jsonValue(changed)}},
		}); err != nil {
			return false, err
		}
		w.log.InfoContext(ctx, "work item resynced", "workItem", wi.Metadata.Name, "changed", changed)
	}
	if open || len(wi.Spec.Links) == 0 {
		return false, nil
	}
	last, err := w.lastActivity(ctx, wi)
	if err != nil {
		return false, err
	}
	if now.Sub(last) < w.opts.Retention {
		return false, nil
	}
	return w.retire(ctx, wi.Metadata.Name, last)
}

// lastActivity is the latest of the item's creation, decisions, tasks and runs.
func (w *WorkItemController) lastActivity(ctx context.Context, wi *v1alpha1.WorkItem) (time.Time, error) {
	last := wi.Metadata.CreatedAt
	if st := wi.Status; st != nil {
		for _, d := range st.Decisions {
			if d.At.After(last) {
				last = d.At
			}
		}
	}
	tasks, err := listTasks(ctx, w.s, wi.Metadata.Name)
	if err != nil {
		return time.Time{}, err
	}
	for _, t := range tasks {
		if t.Metadata.CreatedAt.After(last) {
			last = t.Metadata.CreatedAt
		}
		if activePhase(taskPhase(t)) {
			return w.opts.Clock.Now(), nil // still working on it
		}
		runs, err := listRuns(ctx, w.s, t.Metadata.ID)
		if err != nil {
			return time.Time{}, err
		}
		for _, r := range runs {
			if r.Status != nil && r.Status.EndedAt.After(last) {
				last = r.Status.EndedAt
			}
		}
	}
	return last, nil
}

// retire deletes an idle, closed work item under its lock, re-checking that nothing happened
// to it in between.
func (w *WorkItemController) retire(ctx context.Context, id string, last time.Time) (bool, error) {
	var retired bool
	err := withItemLock(ctx, w.s, id, w.opts.LockTTL, func(ctx context.Context) error {
		var err error
		retired, err = w.retireLocked(ctx, id, last)
		return err
	})
	return retired, err
}

// retireLocked is retire with the work item's lock held.
func (w *WorkItemController) retireLocked(ctx context.Context, id string, last time.Time) (bool, error) {
	var wi v1alpha1.WorkItem
	switch err := w.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: id}, &wi); {
	case errors.Is(err, store.ErrNotFound):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("get work item %s: %w", id, err)
	}
	if again, err := w.lastActivity(ctx, &wi); err != nil || again.After(last) {
		return false, err
	}
	if err := w.s.Delete(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: id}); err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, fmt.Errorf("delete work item %s: %w", id, err)
	}
	w.forget(id)
	w.log.InfoContext(ctx, "work item retired", "workItem", id, "lastActivity", last)
	return true, nil
}

// RebuildResult reports a Rebuild.
type RebuildResult struct {
	// Objects is how many provider objects were listed and observed.
	Objects int
	// Created are the work items recreated, by id; Existing counts those already stored.
	Created  []string
	Existing int
}

type rebuilt struct {
	links    []v1alpha1.Link
	state    map[string]any
	stateAt  time.Time
	counters map[string]int
}

// Rebuild recreates work items from the providers' projections (startup, before the rule
// controller starts): for every RuleSet scope it lists each bound provider's objects, observes
// them and reads their projections (the markers). Projections of one work item on several
// objects are merged: links are united, counters take the maximum, and state comes from the
// most recently updated object. Work items already in the store are left alone. It is
// bounded by RebuildTimeout.
func (w *WorkItemController) Rebuild(ctx context.Context) (RebuildResult, error) {
	cat := w.cat.Current()
	if cat == nil {
		return RebuildResult{}, errors.New("rebuild: no catalog loaded yet")
	}
	ctx, cancel := context.WithTimeout(ctx, w.opts.RebuildTimeout)
	defer cancel()

	var res RebuildResult
	seen := map[string]bool{}
	items := map[string]*rebuilt{}
	for _, name := range sortedNames(cat.RuleSets) {
		rs := cat.RuleSets[name]
		scope := v1alpha1.Scope{}
		if rs.Spec.Spec.Scope != nil {
			scope = *rs.Spec.Spec.Scope
		}
		for _, pname := range rs.Providers {
			if pname == v1alpha1.FactoryProvider {
				continue
			}
			conn, ok := w.conns.Conn(pname)
			if !ok {
				continue
			}
			refs, err := conn.List(ctx, scope)
			if err != nil {
				return res, fmt.Errorf("rebuild: list %s for RuleSet %s: %w", pname, name, err)
			}
			for _, ref := range refs {
				if ref.Provider == "" {
					ref.Provider = pname
				}
				k := ref.Provider + "/" + ref.Ref
				if seen[k] {
					continue
				}
				seen[k] = true
				obs, err := w.opts.Cache.Refresh(ctx, v1alpha1.Link{Provider: ref.Provider, Ref: ref.Ref, Kind: ref.Kind})
				if isNotFound(err) {
					continue
				}
				if err != nil {
					return res, fmt.Errorf("rebuild: observe %s: %w", ref.Ref, err)
				}
				res.Objects++
				mergeProjections(items, ref, obs)
			}
		}
	}
	for _, id := range sortedNames(items) {
		it := items[id]
		wi := &v1alpha1.WorkItem{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
			Metadata: v1alpha1.ObjectMeta{Name: id},
			Spec:     v1alpha1.WorkItemSpec{Links: it.links},
			Status:   &v1alpha1.WorkItemStatus{State: it.state, Counters: it.counters},
		}
		if !slices.ContainsFunc(wi.Spec.Links, func(l v1alpha1.Link) bool { return l.Primary }) && len(wi.Spec.Links) > 0 {
			wi.Spec.Links[0].Primary = true
		}
		err := w.s.Create(ctx, wi)
		switch {
		case errors.Is(err, store.ErrAlreadyExists):
			res.Existing++
		case err != nil:
			return res, fmt.Errorf("rebuild: create work item %s: %w", id, err)
		default:
			res.Created = append(res.Created, id)
		}
	}
	w.log.InfoContext(ctx, "work items rebuilt", "objects", res.Objects, "created", len(res.Created), "existing", res.Existing)
	return res, nil
}

func mergeProjections(items map[string]*rebuilt, ref v1alpha1.ObjectRef, obs provider.Observation) {
	for _, p := range obs.Projections {
		if p.WorkItemID == "" {
			continue
		}
		it := items[p.WorkItemID]
		if it == nil {
			it = &rebuilt{counters: map[string]int{}}
			items[p.WorkItemID] = it
		}
		add := func(l v1alpha1.Link) {
			i := slices.IndexFunc(it.links, func(x v1alpha1.Link) bool { return x.Provider == l.Provider && x.Ref == l.Ref })
			if i < 0 {
				it.links = append(it.links, l)
			} else if l.Primary {
				it.links[i].Primary = true
			}
		}
		for _, l := range p.Links {
			add(l)
		}
		add(v1alpha1.Link{Provider: ref.Provider, Ref: ref.Ref, Kind: obs.Ref.Kind})
		for k, n := range p.Counters {
			it.counters[k] = max(it.counters[k], n)
		}
		if it.state == nil || obs.UpdatedAt.After(it.stateAt) {
			it.state = maps.Clone(p.State)
			if it.state == nil {
				it.state = map[string]any{}
			}
			it.stateAt = obs.UpdatedAt
		}
	}
}
