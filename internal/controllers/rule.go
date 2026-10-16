package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/rules"
	"github.com/compliance-framework/factory/internal/store"
)

// DefaultLockTTL is the lease of a work item lock ("wi/<id>"). A reconcile holds it for one
// event; an expired lock is reclaimable, so a stuck holder can't block the item for long.
const DefaultLockTTL = 2 * time.Minute

// RuleOptions configure the rule controller. Zero fields take the defaults.
type RuleOptions struct {
	// Workers reconciles that many events at once (different work items run in parallel;
	// one work item's events are serialised by its lock). Default 2.
	Workers int
	// Clock stamps decisions and is the engine's `now`. Default the wall clock.
	Clock clock.Clock
	// Logger. Default slog.Default().
	Logger *slog.Logger
	// Cache is the observation cache, shared with the work-item controller. Default a new
	// one with DefaultObservationTTL.
	Cache *ObservationCache
	// KillSwitches are the pause flags. Default StoreKillSwitches over the controller's store
	// (the flags the pause/resume commands write).
	KillSwitches KillSwitches
	// Skills resolves TaskSpec skill refs. Default KeepSkills.
	Skills SkillResolver
	// RunCap is the per-item task cap. Default DefaultRunCap.
	RunCap int
	// LockTTL is the work item lock's lease. Default DefaultLockTTL.
	LockTTL time.Duration
	// UIBaseURL is the Factory UI's public URL, for templates' links.ui.
	UIBaseURL string
	// Identity is the factory identity of rule-level notifications and effects. Default
	// DefaultRuleIdentity.
	Identity string
}

func (o RuleOptions) withDefaults(conns ConnSource) RuleOptions {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Cache == nil {
		o.Cache = NewObservationCache(conns, o.Clock, 0)
	}
	if o.KillSwitches == nil {
		o.KillSwitches = NoKillSwitches{}
	}
	if o.Skills == nil {
		o.Skills = KeepSkills{}
	}
	if o.LockTTL <= 0 {
		o.LockTTL = DefaultLockTTL
	}
	if o.Identity == "" {
		o.Identity = DefaultRuleIdentity
	}
	return o
}

// RuleController turns Events into decisions (controllers.md §rule): for each unprocessed
// Event it finds or creates the work item, builds its WorkItemView, runs the engine, applies
// the decisions in order under the work item's lock, appends the decision records and marks
// the Event processed. Its key is the event id.
type RuleController struct {
	s      store.Store
	cat    CatalogProvider
	engine *rules.Engine
	opts   RuleOptions
	apply  *applier
	log    *slog.Logger
	ctrl   *Controller
}

// NewRuleController returns the rule controller, watching the store's Events.
func NewRuleController(s store.Store, cat CatalogProvider, conns ConnSource, opts RuleOptions) (*RuleController, error) {
	if opts.KillSwitches == nil {
		opts.KillSwitches = StoreKillSwitches{Store: s}
	}
	opts = opts.withDefaults(conns)
	r := &RuleController{
		s:      s,
		cat:    cat,
		engine: &rules.Engine{Clock: opts.Clock},
		opts:   opts,
		log:    opts.Logger.With("controller", "rule"),
		apply: &applier{
			s: s, conns: conns, skills: opts.Skills, clk: opts.Clock,
			log: opts.Logger.With("controller", "rule"), uiBase: opts.UIBaseURL, identity: opts.Identity,
		},
	}
	r.ctrl = NewController("rule", r, Options{Workers: opts.Workers, Clock: opts.Clock, Logger: opts.Logger})
	if err := r.ctrl.Watch(s, v1alpha1.KindEvent, nil, InformerOptions{}); err != nil {
		return nil, err
	}
	return r, nil
}

// Controller returns the underlying controller (its queue, for tests and the manager).
func (r *RuleController) Controller() *Controller { return r.ctrl }

// Run runs the controller until ctx is done.
func (r *RuleController) Run(ctx context.Context) error { return r.ctrl.Run(ctx) }

// Reconcile implements Reconciler for one event id.
func (r *RuleController) Reconcile(ctx context.Context, id string) (Result, error) {
	var ev v1alpha1.Event
	err := r.s.Get(ctx, store.Key{Kind: v1alpha1.KindEvent, Name: id}, &ev)
	if errors.Is(err, store.ErrNotFound) {
		// The event aged out: drop its processed mark with it.
		if err := r.s.Delete(ctx, store.Key{Kind: KindEventStatus, Name: id}); err != nil && !errors.Is(err, store.ErrNotFound) {
			return Result{}, fmt.Errorf("delete status of event %s: %w", id, err)
		}
		return Result{}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("get event %s: %w", id, err)
	}
	if done, err := r.processed(ctx, id); err != nil || done {
		return Result{}, err
	}
	cat := r.cat.Current()
	if cat == nil {
		return Result{}, errors.New("no catalog loaded yet")
	}
	wiID, err := r.workItemFor(ctx, cat, ev)
	if err != nil {
		return Result{}, err
	}
	if wiID == "" {
		return Result{}, r.markProcessed(ctx, ev.ID, "")
	}
	return Result{}, r.process(ctx, cat, ev, wiID)
}

func (r *RuleController) processed(ctx context.Context, id string) (bool, error) {
	var st EventStatus
	err := r.s.Get(ctx, store.Key{Kind: KindEventStatus, Name: id}, &st)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("get status of event %s: %w", id, err)
	}
}

func (r *RuleController) markProcessed(ctx context.Context, id, workItem string) error {
	st := &EventStatus{
		TypeMeta: v1alpha1.TypeMeta{Kind: KindEventStatus},
		Metadata: v1alpha1.ObjectMeta{Name: id},
		WorkItem: workItem, ProcessedAt: r.opts.Clock.Now(),
	}
	if err := r.s.Create(ctx, st); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("mark event %s processed: %w", id, err)
	}
	return nil
}

// eventLink is the link a provider event is about: its object, else its repo.
func eventLink(ev v1alpha1.Event) (v1alpha1.Link, bool) {
	if o := ev.Data.Object; o != nil && o.Ref != "" {
		p := o.Provider
		if p == "" {
			p = ev.Source
		}
		return v1alpha1.Link{Provider: p, Ref: o.Ref, Kind: o.Kind, Primary: true}, true
	}
	if ev.Data.Repo != "" {
		return v1alpha1.Link{Provider: ev.Source, Ref: ev.Data.Repo, Kind: "repo", Primary: true}, true
	}
	return v1alpha1.Link{}, false
}

// workItemFor finds the event's work item, or creates one when at least one rule would fire
// for the event (asked with the new item's view: no state, its object observed, permissive
// rails). "" means no work item: the event changes nothing.
func (r *RuleController) workItemFor(ctx context.Context, cat *catalog.Catalog, ev v1alpha1.Event) (string, error) {
	if ev.Source == v1alpha1.FactoryProvider {
		if ev.WorkItem == "" {
			return "", nil
		}
		var wi v1alpha1.WorkItem
		switch err := r.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: ev.WorkItem}, &wi); {
		case errors.Is(err, store.ErrNotFound):
			r.log.InfoContext(ctx, "factory event for a missing work item", "event", ev.ID, "workItem", ev.WorkItem)
			return "", nil
		case err != nil:
			return "", fmt.Errorf("get work item %s: %w", ev.WorkItem, err)
		}
		return ev.WorkItem, nil
	}
	link, hasLink := eventLink(ev)
	if hasLink {
		var found []*v1alpha1.WorkItem
		if _, err := r.s.List(ctx, v1alpha1.KindWorkItem, store.ListOptions{
			Field: map[string]string{store.FieldWorkItemLink: store.LinkFieldValue(link.Provider, link.Ref)},
		}, &found); err != nil {
			return "", fmt.Errorf("find work item of %s: %w", link.Ref, err)
		}
		if len(found) > 0 {
			return found[0].Metadata.Name, nil // name order: the oldest id wins if two claim it
		}
	}
	// A deterministic id: two events about the same new object create one work item.
	seed := "event/" + ev.ID
	if hasLink {
		seed = "link/" + store.LinkFieldValue(link.Provider, link.Ref)
	}
	wi := &v1alpha1.WorkItem{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
		Metadata: v1alpha1.ObjectMeta{Name: objectID("wi", seed)},
		Status:   &v1alpha1.WorkItemStatus{State: map[string]any{}},
	}
	if hasLink {
		wi.Spec.Links = []v1alpha1.Link{link}
	}
	if a := ev.Data.Actor; a != nil {
		rep := *a
		wi.Status.Reporter = &rep
	}
	// Would any rule fire? Ask with the work item as it would be: no state, no runs, its
	// object observed (rules read item.pr and the like), and permissive rails.
	obs, err := observeLinks(ctx, r.opts.Cache, wi.Spec.Links)
	if err != nil {
		return "", err
	}
	eval, err := r.engine.Evaluate(ctx, cat, ev, buildView(wi, nil, obs), rules.AllowAll)
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(eval.Matches, rules.Match.Fired) {
		return "", nil
	}
	if err := r.s.Create(ctx, wi); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return "", fmt.Errorf("create work item: %w", err)
	}
	r.log.InfoContext(ctx, "work item created", "workItem", wi.Metadata.Name, "event", ev.ID, "link", link.Ref)
	return wi.Metadata.Name, nil
}

// process evaluates and applies one event for one work item under the item's lock.
func (r *RuleController) process(ctx context.Context, cat *catalog.Catalog, ev v1alpha1.Event, wiID string) error {
	return withItemLock(ctx, r.s, wiID, r.opts.LockTTL, func(ctx context.Context) error {
		return r.processLocked(ctx, cat, ev, wiID)
	})
}

// processLocked is process with the work item's lock held.
func (r *RuleController) processLocked(ctx context.Context, cat *catalog.Catalog, ev v1alpha1.Event, wiID string) error {
	var wi v1alpha1.WorkItem
	switch err := r.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: wiID}, &wi); {
	case errors.Is(err, store.ErrNotFound):
		return r.markProcessed(ctx, ev.ID, "") // deleted (retention) since it was found
	case err != nil:
		return fmt.Errorf("get work item %s: %w", wiID, err)
	}
	tasks, err := listTasks(ctx, r.s, wiID)
	if err != nil {
		return err
	}
	runs, err := runsBySpec(ctx, r.s, tasks)
	if err != nil {
		return err
	}
	obs, err := observeLinks(ctx, r.opts.Cache, wi.Spec.Links)
	if err != nil {
		return err
	}
	now := r.opts.Clock.Now()
	rails, err := newStoreRails(ctx, r.s, r.opts.KillSwitches, railsInput{
		cat: cat, event: ev.ID, workItem: &wi, tasks: tasks, obs: obs, runCap: r.opts.RunCap, now: now,
	})
	if err != nil {
		return err
	}
	eval, err := r.engine.Evaluate(ctx, cat, ev, buildView(&wi, runs, obs), rails)
	if err != nil {
		return err
	}

	ac := &applyCtx{cat: cat, ev: ev, wi: &wi, now: now}
	notes := map[[2]string][]string{}
	perRule := map[string]int{}
	for _, d := range eval.Decisions {
		i := perRule[d.Rule]
		perRule[d.Rule]++
		note, err := r.apply.apply(ctx, ac, applied{
			ruleSet: d.RuleSet, rule: d.Rule, action: d.Action, verdict: d.Rail,
			key: fmt.Sprintf("evt/%s/%s/%d", ev.ID, d.Rule, i),
		})
		if err != nil {
			return fmt.Errorf("event %s, rule %s: %s: %w", ev.ID, d.Rule, d.Action, err)
		}
		if note != "" {
			k := [2]string{d.RuleSet, d.Rule}
			notes[k] = append(notes[k], d.Action.String()+": "+note)
		}
	}
	recs := eval.Records(now)
	for i := range recs {
		if n := notes[[2]string{recs[i].RuleSet, recs[i].Rule}]; len(n) > 0 {
			recs[i].Reason = strings.TrimPrefix(recs[i].Reason+"; apply failed: "+strings.Join(n, "; "), "; ")
		}
	}
	appendDecisions(&wi, recs)
	if err := r.s.Update(ctx, &wi); err != nil {
		return fmt.Errorf("update work item %s: %w", wiID, err)
	}
	r.log.DebugContext(ctx, "event applied", "event", ev.ID, "type", ev.Type, "workItem", wiID, "decisions", len(eval.Decisions))
	return r.markProcessed(ctx, ev.ID, wiID)
}

// DryRun evaluates ev as the rule controller would, but applies nothing and writes nothing to
// the store (the API's dry-run): against the work item wiID as it is now (state, runs,
// observations and the real rails, kill switches included), or, when wiID is empty, against
// the work item the event would create (its object observed, no state). A missing work item
// is store.ErrNotFound.
func (r *RuleController) DryRun(ctx context.Context, ev v1alpha1.Event, wiID string) (rules.Evaluation, error) {
	cat := r.cat.Current()
	if cat == nil {
		return rules.Evaluation{}, errors.New("no catalog loaded yet")
	}
	wi := &v1alpha1.WorkItem{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
		Status:   &v1alpha1.WorkItemStatus{State: map[string]any{}},
	}
	var (
		existing *v1alpha1.WorkItem
		tasks    []*v1alpha1.Task
		runs     map[string][]rules.RunSummary
	)
	if wiID != "" {
		if err := r.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: wiID}, wi); err != nil {
			return rules.Evaluation{}, fmt.Errorf("get work item %s: %w", wiID, err)
		}
		existing = wi
		var err error
		if tasks, err = listTasks(ctx, r.s, wiID); err != nil {
			return rules.Evaluation{}, err
		}
		if runs, err = runsBySpec(ctx, r.s, tasks); err != nil {
			return rules.Evaluation{}, err
		}
	} else if link, ok := eventLink(ev); ok {
		wi.Spec.Links = []v1alpha1.Link{link}
	}
	obs, err := observeLinks(ctx, r.opts.Cache, wi.Spec.Links)
	if err != nil {
		return rules.Evaluation{}, err
	}
	rails, err := newStoreRails(ctx, r.s, r.opts.KillSwitches, railsInput{
		cat: cat, event: ev.ID, workItem: existing, tasks: tasks, obs: obs, runCap: r.opts.RunCap, now: r.opts.Clock.Now(),
	})
	if err != nil {
		return rules.Evaluation{}, err
	}
	return r.engine.Evaluate(ctx, cat, ev, buildView(wi, runs, obs), rails)
}
