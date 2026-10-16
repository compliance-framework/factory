package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/store"
)

// DebounceController fires debounced rules (controllers.md §debounce, part of the rule
// controller): at a DebounceRecord's Until it re-checks the rails of each held action, applies
// the allowed ones under the work item's lock, records the decision and deletes the record.
// Its key is the record's name; the queue's delayed requeue is the timer wheel.
type DebounceController struct {
	rule *RuleController
	ctrl *Controller
}

// NewDebounceController returns the debounce controller of a rule controller (same store,
// catalog, providers and options).
func NewDebounceController(rule *RuleController) (*DebounceController, error) {
	d := &DebounceController{rule: rule}
	d.ctrl = NewController("debounce", d, Options{Workers: 1, Clock: rule.opts.Clock, Logger: rule.opts.Logger})
	if err := d.ctrl.Watch(rule.s, KindDebounce, nil, InformerOptions{}); err != nil {
		return nil, err
	}
	return d, nil
}

// Controller returns the underlying controller.
func (d *DebounceController) Controller() *Controller { return d.ctrl }

// Run runs the controller until ctx is done.
func (d *DebounceController) Run(ctx context.Context) error { return d.ctrl.Run(ctx) }

// Reconcile implements Reconciler for one DebounceRecord.
func (d *DebounceController) Reconcile(ctx context.Context, name string) (Result, error) {
	r := d.rule
	var rec DebounceRecord
	err := r.s.Get(ctx, store.Key{Kind: KindDebounce, Name: name}, &rec)
	if errors.Is(err, store.ErrNotFound) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("get debounce %s: %w", name, err)
	}
	now := r.opts.Clock.Now()
	if wait := rec.Spec.Until.Sub(now); wait > 0 {
		return Result{RequeueAfter: wait}, nil
	}
	cat := r.cat.Current()
	if cat == nil {
		return Result{}, errors.New("no catalog loaded yet")
	}
	if err := d.fire(ctx, cat, &rec); err != nil {
		return Result{}, err
	}
	if err := r.s.Delete(ctx, store.Key{Kind: KindDebounce, Name: name}); err != nil && !errors.Is(err, store.ErrNotFound) {
		return Result{}, fmt.Errorf("delete debounce %s: %w", name, err)
	}
	return Result{}, nil
}

func (d *DebounceController) fire(ctx context.Context, cat *catalog.Catalog, rec *DebounceRecord) error {
	return withItemLock(ctx, d.rule.s, rec.Spec.WorkItem, d.rule.opts.LockTTL, func(ctx context.Context) error {
		return d.fireLocked(ctx, cat, rec)
	})
}

// fireLocked is fire with the work item's lock held.
func (d *DebounceController) fireLocked(ctx context.Context, cat *catalog.Catalog, rec *DebounceRecord) error {
	r := d.rule
	var wi v1alpha1.WorkItem
	switch err := r.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: rec.Spec.WorkItem}, &wi); {
	case errors.Is(err, store.ErrNotFound):
		return nil // the item is gone: nothing to apply
	case err != nil:
		return fmt.Errorf("get work item %s: %w", rec.Spec.WorkItem, err)
	}
	ev := v1alpha1.Event{ID: rec.Spec.Event}
	if err := r.s.Get(ctx, store.Key{Kind: v1alpha1.KindEvent, Name: rec.Spec.Event}, &ev); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("get event %s: %w", rec.Spec.Event, err)
	}
	tasks, err := listTasks(ctx, r.s, wi.Metadata.Name)
	if err != nil {
		return err
	}
	obs, err := observeLinks(ctx, r.opts.Cache, wi.Spec.Links)
	if err != nil {
		return err
	}
	now := r.opts.Clock.Now()
	rails, err := newStoreRails(ctx, r.s, r.opts.KillSwitches, railsInput{
		cat: cat, event: rec.Spec.Event, workItem: &wi, tasks: tasks, obs: obs, runCap: r.opts.RunCap, now: now,
	})
	if err != nil {
		return err
	}
	ac := &applyCtx{cat: cat, ev: ev, wi: &wi, now: now}
	record := v1alpha1.Decision{At: now, Event: rec.Spec.Event, RuleSet: rec.Spec.RuleSet, Rule: rec.Spec.Rule, Fired: true, Reason: "debounced until " + rec.Spec.Until.Format(time.RFC3339)}
	var notes []string
	for i, intent := range rec.Spec.Then {
		verdict := rails.Check(intent)
		record.Actions = append(record.Actions, intent.String())
		record.Rails = append(record.Rails, verdict)
		note, err := r.apply.apply(ctx, ac, applied{
			ruleSet: rec.Spec.RuleSet, rule: rec.Spec.Rule, action: intent, verdict: verdict,
			key: fmt.Sprintf("dbn/%s/%d", rec.Metadata.Name, i),
		})
		if err != nil {
			return fmt.Errorf("debounce %s: %s: %w", rec.Spec.Key, intent, err)
		}
		if note != "" {
			notes = append(notes, intent.String()+": "+note)
		}
	}
	if len(notes) > 0 {
		record.Reason += "; apply failed: " + strings.Join(notes, "; ")
	}
	appendDecisions(&wi, []v1alpha1.Decision{record})
	if err := r.s.Update(ctx, &wi); err != nil {
		return fmt.Errorf("update work item %s: %w", wi.Metadata.Name, err)
	}
	return nil
}
