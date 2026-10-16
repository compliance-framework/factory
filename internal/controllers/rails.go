package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/rules"
	"github.com/compliance-framework/factory/internal/store"
)

// StopLabel on any object linked to a work item stops the factory for that item (the per-item
// kill switch; design/02-architecture.md §Safety rails).
const StopLabel = "factory:stop"

// DefaultRunCap is the hard maximum of tasks per work item that no rule can override.
const DefaultRunCap = 20

// Kill-switch scopes (the pause/resume command's target.scope).
const (
	ScopeGlobal   = "global"
	ScopeProvider = "provider"
	ScopeQueue    = "queue"
	ScopeWorkItem = "workitem"
)

// KillSwitches reports the pause flags. The pause/resume commands set them in the store
// (RT-S2-T03); until then NoKillSwitches is the default and only the StopLabel stops an item.
type KillSwitches interface {
	// Paused reports whether scope/name (name is empty for ScopeGlobal) is paused.
	Paused(ctx context.Context, scope, name string) (bool, error)
}

// NoKillSwitches is KillSwitches with nothing paused.
type NoKillSwitches struct{}

// Paused implements KillSwitches.
func (NoKillSwitches) Paused(context.Context, string, string) (bool, error) { return false, nil }

// railsInput is what the rails read, gathered before Evaluate (the engine is pure: Check
// does no I/O).
type railsInput struct {
	cat      *catalog.Catalog
	event    string
	workItem *v1alpha1.WorkItem
	tasks    []*v1alpha1.Task
	obs      observations
	runCap   int
	now      time.Time
}

// storeRails implements rules.Rails for one evaluation of one (event, work item):
//
//   - kill switch: the item's StopLabel, a global, provider (of the primary link) or work-item
//     pause suppresses every action; a queue pause suppresses dispatches to that queue;
//   - run cap: a dispatch when the item already has runCap tasks is suppressed;
//   - single active run: a dispatch while a task of that TaskSpec is active is suppressed;
//   - budget: a dispatch to a queue whose daily budget (BudgetCounter) is spent is deferred.
//
// It is stateful within the evaluation: an allowed cancel frees its TaskSpec for a later
// dispatch (cancel-then-dispatch supersedes), and an allowed or deferred dispatch counts
// towards the cap and the active run. Tasks the same event created (a retried reconcile) are
// not counted, so a retry reaches the same verdicts.
type storeRails struct {
	stopped      string // the reason every action is suppressed, if any
	pausedQueues map[string]bool
	overBudget   map[string]string // queue → reason
	active       map[string]int    // active tasks by TaskSpec
	tasks        int
	runCap       int
}

var _ rules.Rails = (*storeRails)(nil)

func newStoreRails(ctx context.Context, s store.Store, ks KillSwitches, in railsInput) (*storeRails, error) {
	r := &storeRails{pausedQueues: map[string]bool{}, overBudget: map[string]string{}, active: map[string]int{}, runCap: in.runCap}
	if r.runCap <= 0 {
		r.runCap = DefaultRunCap
	}
	for _, t := range in.tasks {
		if t.Metadata.Provenance != nil && t.Metadata.Provenance.Event == in.event {
			continue
		}
		r.tasks++
		if activePhase(taskPhase(t)) && taskPhase(t) != v1alpha1.TaskCancelling {
			r.active[t.Spec.TaskSpec.Name]++
		}
	}
	if in.workItem != nil {
		for k, o := range in.obs.byLink {
			if slices.Contains(o.Labels, StopLabel) {
				r.stopped = fmt.Sprintf("%s has the %s label", k, StopLabel)
				break
			}
		}
	}
	if ks == nil {
		ks = NoKillSwitches{}
	}
	paused := func(scope, name string) (bool, error) {
		p, err := ks.Paused(ctx, scope, name)
		if err != nil {
			return false, fmt.Errorf("kill switch %s/%s: %w", scope, name, err)
		}
		return p, nil
	}
	if r.stopped == "" {
		checks := [][2]string{{ScopeGlobal, ""}}
		if in.workItem != nil {
			checks = append(checks, [2]string{ScopeWorkItem, in.workItem.Metadata.Name})
			if l, ok := primaryLink(in.workItem.Spec.Links); ok {
				checks = append(checks, [2]string{ScopeProvider, l.Provider})
			}
		}
		for _, c := range checks {
			p, err := paused(c[0], c[1])
			if err != nil {
				return nil, err
			}
			if p {
				r.stopped = "paused: " + c[0] + " " + c[1]
				break
			}
		}
	}
	day := in.now.UTC().Format(time.DateOnly)
	for _, name := range sortedNames(in.cat.Queues) {
		p, err := paused(ScopeQueue, name)
		if err != nil {
			return nil, err
		}
		r.pausedQueues[name] = p
		q := in.cat.Queues[name]
		if q.Spec.Budget == nil || q.Spec.Budget.Daily == nil {
			continue
		}
		reason, err := budgetSpent(ctx, s, "budget.queue."+name+"."+day, *q.Spec.Budget.Daily)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			r.overBudget[name] = "queue " + name + ": " + reason
		}
	}
	return r, nil
}

// budgetSpent reads a BudgetCounter and returns which limit it reached ("" for none).
func budgetSpent(ctx context.Context, s store.Store, name string, lim v1alpha1.BudgetLimit) (string, error) {
	var bc v1alpha1.BudgetCounter
	err := s.Get(ctx, store.Key{Kind: v1alpha1.KindBudgetCounter, Name: name}, &bc)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("budget %s: %w", name, err)
	}
	switch {
	case lim.Runs > 0 && bc.Status.Runs >= lim.Runs:
		return fmt.Sprintf("%d of %d daily runs", bc.Status.Runs, lim.Runs), nil
	case lim.Tokens > 0 && bc.Status.Tokens >= lim.Tokens:
		return fmt.Sprintf("%d of %d daily tokens", bc.Status.Tokens, lim.Tokens), nil
	case lim.USD > 0 && bc.Status.USD >= lim.USD:
		return fmt.Sprintf("$%.2f of $%.2f daily", bc.Status.USD, lim.USD), nil
	}
	return "", nil
}

// Check implements rules.Rails.
func (r *storeRails) Check(a rules.ActionIntent) v1alpha1.RailVerdict {
	if r.stopped != "" {
		return rules.Suppressed(rules.RailKillSwitch, r.stopped)
	}
	switch {
	case a.Cancel != nil:
		r.active[a.Cancel.TaskSpec] = 0
	case a.Dispatch != nil:
		d := a.Dispatch
		switch {
		case r.pausedQueues[d.Queue]:
			return rules.Suppressed(rules.RailKillSwitch, "paused: queue "+d.Queue)
		case r.tasks >= r.runCap:
			return rules.Suppressed(rules.RailRunCap, fmt.Sprintf("the work item has %d tasks (cap %d)", r.tasks, r.runCap))
		case r.active[d.TaskSpec] > 0:
			return rules.Suppressed(rules.RailActiveRun, "a "+d.TaskSpec+" task is already active")
		}
		r.tasks++
		r.active[d.TaskSpec]++
		if reason, ok := r.overBudget[d.Queue]; ok {
			return rules.Deferred(rules.RailBudget, reason)
		}
	}
	return rules.Allowed()
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
