package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/store"
)

// Reconcile is the lease controller, keyed by task id. It
//
//   - revokes the leases whose task left them: the run timed out or was cancelled, the task
//     was finished or re-leased (the hook RT-S2-T01 left for timeouts);
//   - expires the lease of a task whose leaseExpiresAt passed without a heartbeat: back to
//     Pending (attempts + 1, agent-lost) while the queue's retry policy allows, else Failed
//     (agent-lost); a Cancelling task becomes Cancelled;
//   - deletes ended leases after LeaseRetention.
func (s *Scheduler) Reconcile(ctx context.Context, taskID string) (controllers.Result, error) {
	// Leases first, then the task: a lease is recorded after its task is Leased, so a lease
	// seen here has its task already Leased below (no fresh lease is revoked by mistake).
	var leases []*Lease
	if _, err := s.d.Store.List(ctx, KindLease, store.ListOptions{
		LabelSelector: &v1alpha1.LabelSelector{MatchLabels: map[string]string{LabelTask: taskID}},
	}, &leases); err != nil {
		return controllers.Result{}, fmt.Errorf("list leases of %s: %w", taskID, err)
	}
	var t v1alpha1.Task
	err := s.d.Store.Get(ctx, taskKey(taskID), &t)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return controllers.Result{}, fmt.Errorf("get task %s: %w", taskID, err)
	}
	held := err == nil && active(&t)
	now := s.clk.Now()

	// The current lease is the newest open one of the task's agent; the others are revoked.
	var current *Lease
	for _, l := range leases {
		if l.Status.End == "" && held && l.Spec.Agent == *t.Status.Agent &&
			(current == nil || l.Spec.IssuedAt.After(current.Spec.IssuedAt)) {
			current = l
		}
	}
	var requeue time.Duration
	after := func(d time.Duration) {
		if d > 0 && (requeue == 0 || d < requeue) {
			requeue = d
		}
	}
	for _, l := range leases {
		switch {
		case l.Status.End != "":
			if gone := l.Status.EndedAt.Add(s.opts.LeaseRetention); now.Before(gone) {
				after(gone.Sub(now))
			} else if err := s.d.Store.Delete(ctx, leaseKey(l.Metadata.Name)); err != nil && !errors.Is(err, store.ErrNotFound) {
				return controllers.Result{}, fmt.Errorf("delete lease %s: %w", l.Metadata.Name, err)
			}
		case l != current:
			s.endLease(ctx, l, LeaseRevoked)
			after(s.opts.LeaseRetention)
		}
	}
	if !held || t.Status.LeaseExpiresAt.IsZero() {
		return controllers.Result{RequeueAfter: requeue}, nil
	}
	if wait := t.Status.LeaseExpiresAt.Sub(now); wait > 0 {
		after(wait)
		return controllers.Result{RequeueAfter: requeue}, nil
	}
	if err := s.expire(ctx, taskID); err != nil {
		return controllers.Result{}, err
	}
	if current != nil {
		s.endLease(ctx, current, LeaseExpired)
		after(s.opts.LeaseRetention)
	}
	return controllers.Result{RequeueAfter: requeue}, nil
}

// expire ends a lease that got no heartbeat for its TTL.
func (s *Scheduler) expire(ctx context.Context, taskID string) error {
	now := s.clk.Now()
	cat := s.d.Catalog.Current()
	cancelling := false
	t, err := update(ctx, s.d.Store, taskKey(taskID), newTask, func(t *v1alpha1.Task) (bool, error) {
		st := ensureTaskStatus(t)
		if !active(t) || st.LeaseExpiresAt.IsZero() || now.Before(st.LeaseExpiresAt) {
			return false, nil // renewed or moved on meanwhile
		}
		if st.Phase == v1alpha1.TaskCancelling {
			cancelling = true // the run controller cancels it (below)
			return false, nil
		}
		st.LeaseExpiresAt = time.Time{}
		st.Reason = ReasonAgentLost
		if mayRetry(cat, t, v1alpha1.RetryAgentLost) {
			st.Phase, st.Agent = v1alpha1.TaskPending, nil
			st.Attempts++
		} else {
			st.Phase = v1alpha1.TaskFailed
		}
		return true, nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("expire lease of %s: %w", taskID, err)
	}
	if cancelling {
		// Cancelling ──lease expiry──▶ Cancelled, as an acknowledgement would.
		if err := s.d.Runs.AckCancel(ctx, taskID); err != nil && !errors.Is(err, controllers.ErrNotLeased) {
			return err
		}
		s.log.InfoContext(ctx, "lease expired; task cancelled", "task", taskID)
		return nil
	}
	if t.Status.Phase == v1alpha1.TaskPending || t.Status.Phase == v1alpha1.TaskFailed {
		s.log.InfoContext(ctx, "lease expired", "task", taskID, "phase", t.Status.Phase, "attempts", t.Status.Attempts)
	}
	return nil
}

// mayRetry reports whether the task's queue retry policy allows another attempt for reason
// (the run controller applies the same policy to abandons).
func mayRetry(cat *catalog.Catalog, t *v1alpha1.Task, reason v1alpha1.RetryReason) bool {
	if cat == nil {
		return false
	}
	q := cat.Queues[t.Spec.Queue]
	if q == nil || q.Spec.Retry == nil {
		return false
	}
	p := q.Spec.Retry
	if len(p.On) > 0 && !slices.Contains(p.On, reason) {
		return false
	}
	return t.Status.Attempts+1 <= p.Max
}

// Refresh is the tick's housekeeping: agents whose token expired go Offline, throttles that
// passed end, the agents' today mirrors follow the day, and each pending task's reason says
// why no agent can take it now (the reason of the agent that got furthest down the table).
func (s *Scheduler) Refresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn, err := s.loadSnapshot(ctx)
	if errors.Is(err, errNoCatalog) {
		return nil
	}
	if err != nil {
		return err
	}
	var tokens []*AgentToken
	if _, err := s.d.Store.List(ctx, KindAgentToken, store.ListOptions{}, &tokens); err != nil {
		return fmt.Errorf("list agent tokens: %w", err)
	}
	for _, tok := range tokens {
		if sn.now.Before(tok.ExpiresAt) {
			continue
		}
		id := tok.Metadata.Name
		if err := s.d.Store.Delete(ctx, store.KeyOf(tok)); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("delete expired token of %s: %w", id, err)
		}
		if a := sn.agents[id]; a != nil {
			if _, err := s.syncAgent(ctx, id, func(a *v1alpha1.Agent) bool {
				a.Status.Phase = v1alpha1.AgentOffline
				return true
			}); err != nil {
				return err
			}
			a.Status.Phase = v1alpha1.AgentOffline
			s.log.InfoContext(ctx, "agent token expired; agent offline", "agent", id)
		}
	}
	for id, a := range sn.agents {
		today, err := sn.counter(CounterName(ScopeAgent, id, sn.now))
		if err != nil {
			return err
		}
		stale := a.Status.Today == nil || a.Status.Today.Runs != today.Runs || a.Status.Today.Tokens != today.Tokens
		if stale || (a.Status.Phase == v1alpha1.AgentThrottled && !throttled(a, sn.now)) {
			if _, err := s.syncAgent(ctx, id, nil); err != nil {
				return err
			}
		}
	}
	pending := map[string]bool{}
	for _, t := range sn.pending {
		pending[t.Metadata.ID] = true
		reason, err := sn.blockingReason(t)
		if err != nil {
			return err
		}
		if reason != "" && reason != t.Status.Reason {
			s.setReason(ctx, t.Metadata.ID, reason)
		}
	}
	for id := range s.heldSince {
		if !pending[id] {
			delete(s.heldSince, id)
		}
	}
	return nil
}
