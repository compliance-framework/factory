package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
)

// Budget counter scopes: budget.<scope>.<name>.<YYYY-MM-DD>. The agent scope holds an
// agent's daily usage, checked against its lending (maxRunsPerDay, maxTokensPerDay) and
// mirrored on Agent.status.today.
const (
	ScopeQueue   = "queue"
	ScopeRuleSet = "ruleset"
	ScopeGlobal  = "global"
	ScopeAgent   = "agent"
)

// maxCAS bounds the compare-and-swap retries of one update.
const maxCAS = 32

// CounterName is the BudgetCounter of scope/name on t's UTC day.
func CounterName(scope, name string, t time.Time) string {
	return "budget." + scope + "." + name + "." + t.UTC().Format(time.DateOnly)
}

// readCounter returns a counter's status; zero when it doesn't exist yet.
func readCounter(ctx context.Context, s store.Store, name string) (v1alpha1.BudgetCounterStatus, error) {
	var bc v1alpha1.BudgetCounter
	err := s.Get(ctx, store.Key{Kind: v1alpha1.KindBudgetCounter, Name: name}, &bc)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return v1alpha1.BudgetCounterStatus{}, nil
	case err != nil:
		return v1alpha1.BudgetCounterStatus{}, fmt.Errorf("read budget counter %s: %w", name, err)
	}
	return bc.Status, nil
}

// addCounter adds d to a counter with compare-and-swap (creating it on first use) and
// returns the new totals.
func addCounter(ctx context.Context, s store.Store, name string, d v1alpha1.BudgetCounterStatus) (v1alpha1.BudgetCounterStatus, error) {
	k := store.Key{Kind: v1alpha1.KindBudgetCounter, Name: name}
	for range maxCAS {
		var bc v1alpha1.BudgetCounter
		err := s.Get(ctx, k, &bc)
		switch {
		case errors.Is(err, store.ErrNotFound):
			bc = v1alpha1.BudgetCounter{
				TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindBudgetCounter},
				Metadata: v1alpha1.ObjectMeta{Name: name},
				Status:   d,
			}
			err = s.Create(ctx, &bc)
			if errors.Is(err, store.ErrAlreadyExists) {
				continue
			}
		case err != nil:
			return v1alpha1.BudgetCounterStatus{}, fmt.Errorf("budget counter %s: %w", name, err)
		default:
			bc.Status.Runs += d.Runs
			bc.Status.Tokens += d.Tokens
			bc.Status.USD += d.USD
			err = s.Update(ctx, &bc)
			if errors.Is(err, store.ErrConflict) {
				continue
			}
		}
		if err != nil {
			return v1alpha1.BudgetCounterStatus{}, fmt.Errorf("budget counter %s: %w", name, err)
		}
		return bc.Status, nil
	}
	return v1alpha1.BudgetCounterStatus{}, fmt.Errorf("budget counter %s: %d conflicting updates: %w", name, maxCAS, store.ErrConflict)
}

// spent says which limit of lim c reached ("" for none). An unset limit is unlimited.
func spent(c v1alpha1.BudgetCounterStatus, lim *v1alpha1.Budget) string {
	if lim == nil || lim.Daily == nil {
		return ""
	}
	d := lim.Daily
	switch {
	case d.Runs > 0 && c.Runs >= d.Runs:
		return fmt.Sprintf("%d of %d daily runs", c.Runs, d.Runs)
	case d.Tokens > 0 && c.Tokens >= d.Tokens:
		return fmt.Sprintf("%d of %d daily tokens", c.Tokens, d.Tokens)
	case d.USD > 0 && c.USD >= d.USD:
		return fmt.Sprintf("$%.2f of $%.2f daily", c.USD, d.USD)
	}
	return ""
}

// budgetScopes are the counters a lease of t is charged to: its queue, its RuleSet (from the
// provenance) and its agent. There is no global budget in the catalog yet.
func budgetScopes(queue, ruleSet, agent string, now time.Time) []string {
	names := []string{CounterName(ScopeQueue, queue, now)}
	if ruleSet != "" {
		names = append(names, CounterName(ScopeRuleSet, ruleSet, now))
	}
	return append(names, CounterName(ScopeAgent, agent, now))
}

// charge adds d to every counter in names. It goes on after a failure so one bad counter
// doesn't lose the others' usage, and returns the first error.
func charge(ctx context.Context, s store.Store, names []string, d v1alpha1.BudgetCounterStatus) error {
	var first error
	for _, n := range names {
		if _, err := addCounter(ctx, s, n, d); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// update reads the object at k into a fresh obj, applies mutate and writes it back with
// compare-and-swap, retrying on a conflict. mutate returns false to leave it unchanged.
func update[T store.Object](ctx context.Context, s store.Store, k store.Key, fresh func() T, mutate func(T) (bool, error)) (T, error) {
	var zero T
	for range maxCAS {
		obj := fresh()
		if err := s.Get(ctx, k, obj); err != nil {
			return zero, err
		}
		changed, err := mutate(obj)
		if err != nil || !changed {
			return obj, err
		}
		err = s.Update(ctx, obj)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return zero, err
		}
		return obj, nil
	}
	return zero, fmt.Errorf("update %s: %d conflicting updates: %w", k, maxCAS, store.ErrConflict)
}
