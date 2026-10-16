package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Retention says how an object ages out.
type Retention int

// Retention rules (LLD store.md §Retention).
const (
	// Keep: never swept (WorkItem, Agent, Approval, internal kinds).
	Keep Retention = iota
	// SinceCreated: swept Options.Retention after creation (Run, Event, Effect, BudgetCounter;
	// transcripts likewise).
	SinceCreated
	// SinceUpdated: swept Options.Retention after its last write (a Task in a terminal phase,
	// Succeeded, Failed or Cancelled: its last write is when it got there).
	SinceUpdated
)

// RetentionOf returns obj's retention rule. It depends on obj's state (a Task's phase), so a
// store evaluates it on every write.
func RetentionOf(obj Object) Retention {
	switch o := obj.(type) {
	case *v1alpha1.Run, *v1alpha1.Event, *v1alpha1.Effect, *v1alpha1.BudgetCounter:
		return SinceCreated
	case *v1alpha1.Task:
		if o.Status != nil {
			switch o.Status.Phase {
			case v1alpha1.TaskSucceeded, v1alpha1.TaskFailed, v1alpha1.TaskCancelled:
				return SinceUpdated
			}
		}
	}
	return Keep
}

// Expired reports whether an object under rule r, created and last written at the given
// times, is past retention at now.
func Expired(r Retention, created, updated, now time.Time, retention time.Duration) bool {
	switch r {
	case SinceCreated:
		return !now.Before(created.Add(retention))
	case SinceUpdated:
		return !now.Before(updated.Add(retention))
	}
	return false
}

// RunSweeper calls s.Sweep every interval (DefaultSweepInterval when 0) until ctx ends. Errors
// are logged, not returned: the next tick tries again.
func RunSweeper(ctx context.Context, s Sweeper, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				log.ErrorContext(ctx, "store: sweep failed", "err", err)
			}
		}
	}
}
