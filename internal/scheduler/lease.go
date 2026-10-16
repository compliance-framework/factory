package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/store"
)

// maxProgress bounds a run's progress ring buffer (as the run controller does).
const maxProgress = 200

// errSkip makes the matcher try the next task: this one couldn't be committed (its
// credential failed, or it changed under the snapshot).
var errSkip = errors.New("skip task")

func leaseKey(id string) store.Key { return store.Key{Kind: KindLease, Name: id} }

func newLease() *Lease { return &Lease{} }

// Lease is leaseTask: a long poll for a task agentID may take. It returns at once with a
// grant when one matches, else waits up to wait (capped at Options.MaxWait) and tries again
// on every task, lease or agent change and on the tick. No task within wait is (nil, nil).
func (s *Scheduler) Lease(ctx context.Context, agentID string, wait time.Duration) (*Grant, error) {
	wait = min(max(wait, 0), s.opts.MaxWait)
	deadline := s.clk.Now().Add(wait)
	s.mu.Lock()
	s.waiting[agentID]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.waiting[agentID]--; s.waiting[agentID] <= 0 {
			delete(s.waiting, agentID)
		}
		s.mu.Unlock()
	}()
	var timer clock.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		changed := s.changed() // before trying, so a change during the try isn't missed
		g, err := s.tryLease(ctx, agentID)
		if err != nil || g != nil {
			return g, err
		}
		if !s.clk.Now().Before(deadline) {
			return nil, nil
		}
		if timer == nil {
			timer = s.clk.NewTimerAt(deadline)
		}
		s.parked.Add(1)
		select {
		case <-ctx.Done():
			s.parked.Add(-1)
			return nil, ctx.Err()
		case <-timer.C():
			s.parked.Add(-1)
			return nil, nil
		case <-changed:
			s.parked.Add(-1)
		}
	}
}

// tryLease runs one matching pass for agentID and commits the first task it may take.
func (s *Scheduler) tryLease(ctx context.Context, agentID string) (*Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn, err := s.loadSnapshot(ctx)
	if errors.Is(err, errNoCatalog) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a := sn.agents[agentID]
	if a == nil {
		return nil, fmt.Errorf("agent %s: %w", agentID, ErrNotFound)
	}
	// Fairness: walk the tasks in order, giving each to the fairest waiting agent not yet
	// given one in this pass; this poll takes the first task it is given.
	given := map[string]bool{}
	for _, t := range sn.pending {
		p, err := sn.preferred(t, given)
		if err != nil {
			return nil, err
		}
		if p == "" {
			continue
		}
		if p != agentID {
			given[p] = true // that agent's own poll takes it
			continue
		}
		v, err := sn.check(t, a)
		if err != nil {
			return nil, err
		}
		g, err := s.commit(ctx, sn, t, a, v)
		if errors.Is(err, errSkip) {
			continue
		}
		return g, err
	}
	return nil, nil
}

// commit leases t to a: mint the git credential, move the task to Leased (compare-and-swap
// on the version the snapshot read), record the lease, charge the budgets and the agent's
// day, and tell the run controller. The caller holds s.mu.
func (s *Scheduler) commit(ctx context.Context, sn *snapshot, t *v1alpha1.Task, a *v1alpha1.Agent, v verdict) (*Grant, error) {
	_, pool := sn.pool(t)
	ttl, heartbeat := leaseTimes(pool)
	now := s.clk.Now()
	agentID, taskID := a.Metadata.Name, t.Metadata.ID

	// The credential first: a task whose credential can't be minted is left Pending.
	cred, err := s.mint(ctx, t)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.log.WarnContext(ctx, "git credential not minted; task left pending", "task", taskID, "err", err)
		s.setReason(ctx, taskID, ReasonGitCredential)
		return nil, errSkip
	}
	leaseID, err := s.newLeaseID()
	if err != nil {
		return nil, err
	}

	st := ensureTaskStatus(t)
	st.Phase = v1alpha1.TaskLeased
	st.Agent = &agentID
	st.Reason = ""
	st.LeaseExpiresAt = now.Add(ttl)
	if t.Spec.Harness == nil {
		t.Spec.Harness = &v1alpha1.TaskHarness{}
	}
	t.Spec.Harness.Resolved = v.resolved
	if err := s.d.Store.Update(ctx, t); err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			return nil, errSkip // changed since the snapshot (cancelled, deleted): next pass
		}
		return nil, fmt.Errorf("lease task %s: %w", taskID, err)
	}

	l := &Lease{
		TypeMeta: v1alpha1.TypeMeta{Kind: KindLease},
		Metadata: v1alpha1.ObjectMeta{Name: leaseID, Labels: map[string]string{LabelTask: taskID, LabelAgent: agentID}},
		Spec: LeaseSpec{
			Task: taskID, Agent: agentID, Pool: a.Spec.Pool, Queue: t.Spec.Queue, RuleSet: ruleSetOf(t),
			Auth: v.offer.Auth, IssuedAt: now, TTL: ttl, Heartbeat: heartbeat,
		},
		Status: LeaseStatus{ExpiresAt: st.LeaseExpiresAt},
	}
	if cred != nil {
		l.Status.CredentialExpiresAt = cred.ExpiresAt
	}
	if err := s.d.Store.Create(ctx, l); err != nil {
		s.unlease(ctx, taskID, agentID)
		return nil, fmt.Errorf("record lease of %s: %w", taskID, err)
	}

	s.lastLeased[agentID] = now
	delete(s.heldSince, taskID)
	if err := charge(ctx, s.d.Store, budgetScopes(t.Spec.Queue, ruleSetOf(t), agentID, now), v1alpha1.BudgetCounterStatus{Runs: 1}); err != nil {
		s.log.WarnContext(ctx, "charge lease to budgets", "task", taskID, "err", err)
	}
	if _, err := s.syncAgent(ctx, agentID, nil); err != nil {
		s.log.WarnContext(ctx, "update agent after lease", "agent", agentID, "err", err)
	}
	if err := s.d.Runs.LeaseIssued(ctx, taskID); err != nil {
		s.log.WarnContext(ctx, "create run", "task", taskID, "err", err) // the run controller creates it from the watch
	}
	run, err := s.openRun(ctx, taskID)
	if err != nil {
		s.log.WarnContext(ctx, "find run", "task", taskID, "err", err)
	}
	s.log.InfoContext(ctx, "task leased", "task", taskID, "agent", agentID, "lease", leaseID,
		"harness", v.resolved.Name, "model", v.resolved.Model)
	g := &Grant{
		Lease: LeaseInfo{ID: leaseID, Agent: agentID, Task: taskID, ExpiresAt: st.LeaseExpiresAt, HeartbeatEvery: heartbeat},
		Task:  t, GitCredential: cred,
	}
	if run != nil {
		g.Lease.Run = run.Metadata.Name
	}
	return g, nil
}

// unlease returns a task leased to agent to Pending without an attempt (its lease couldn't be
// recorded). If this fails too, the lease expires.
func (s *Scheduler) unlease(ctx context.Context, taskID, agent string) {
	_, err := update(ctx, s.d.Store, taskKey(taskID), newTask, func(t *v1alpha1.Task) (bool, error) {
		if !heldBy(t, agent) {
			return false, nil
		}
		t.Status.Phase, t.Status.Agent, t.Status.LeaseExpiresAt = v1alpha1.TaskPending, nil, time.Time{}
		return true, nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "return task to pending", "task", taskID, "err", err)
	}
}

// mint mints a task's git credential (nil when it asks for none): TTL = its
// credentials.git.ttl (min(1h, maxWallClock + 10m) at compile time), capped at 1 h.
func (s *Scheduler) mint(ctx context.Context, t *v1alpha1.Task) (*provider.Credential, error) {
	if t.Spec.Credentials == nil || t.Spec.Credentials.Git == nil {
		return nil, nil
	}
	g := t.Spec.Credentials.Git
	conn, ok := s.d.Conns.Conn(g.Provider)
	if !ok {
		return nil, fmt.Errorf("mint credential: no connection for provider %q", g.Provider)
	}
	ttl := MaxCredentialTTL
	if g.TTL != "" {
		if d, err := g.TTL.Parse(); err == nil && d > 0 {
			ttl = min(ttl, d)
		}
	}
	cred, err := conn.MintGitCredential(ctx, provider.GitCredentialRequest{
		Identity: g.Identity, Repo: g.Repo, ReadRepos: g.ReadRepos, Permissions: g.Permissions, TTL: ttl,
	})
	if err != nil {
		return nil, fmt.Errorf("mint credential for %s: %w", t.Metadata.ID, err)
	}
	return &cred, nil
}

// setReason records why a pending task waits, if it is still Pending and the reason changed.
func (s *Scheduler) setReason(ctx context.Context, taskID, reason string) {
	_, err := update(ctx, s.d.Store, taskKey(taskID), newTask, func(t *v1alpha1.Task) (bool, error) {
		st := ensureTaskStatus(t)
		if st.Phase != v1alpha1.TaskPending || st.Reason == reason {
			return false, nil
		}
		st.Reason = reason
		return true, nil
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.WarnContext(ctx, "record task reason", "task", taskID, "reason", reason, "err", err)
	}
}

// openRun is the task's run that hasn't ended, or nil.
func (s *Scheduler) openRun(ctx context.Context, taskID string) (*v1alpha1.Run, error) {
	var runs []*v1alpha1.Run
	if _, err := s.d.Store.List(ctx, v1alpha1.KindRun, store.ListOptions{
		Field: map[string]string{store.FieldRunTask: taskID},
	}, &runs); err != nil {
		return nil, fmt.Errorf("list runs of %s: %w", taskID, err)
	}
	var open *v1alpha1.Run
	for _, r := range runs {
		if (r.Status == nil || r.Status.End == "") && (open == nil || r.Spec.Attempt > open.Spec.Attempt) {
			open = r
		}
	}
	return open, nil
}

// heldLease returns agentID's lease leaseID, ErrNotFound or ErrNotLeaseHolder.
func (s *Scheduler) heldLease(ctx context.Context, agentID, leaseID string) (*Lease, error) {
	var l Lease
	err := s.d.Store.Get(ctx, leaseKey(leaseID), &l)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("lease %s: %w", leaseID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get lease %s: %w", leaseID, err)
	}
	if l.Spec.Agent != agentID {
		return nil, fmt.Errorf("lease %s: %w", leaseID, ErrNotLeaseHolder)
	}
	return &l, nil
}

// leasedTask returns the lease's task if the lease still holds it; otherwise it revokes the
// lease and returns ErrLeaseGone.
func (s *Scheduler) leasedTask(ctx context.Context, l *Lease) (*v1alpha1.Task, error) {
	var t v1alpha1.Task
	err := s.d.Store.Get(ctx, taskKey(l.Spec.Task), &t)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("get task %s: %w", l.Spec.Task, err)
	}
	if err != nil || !heldBy(&t, l.Spec.Agent) {
		s.endLease(ctx, l, LeaseRevoked)
		return nil, fmt.Errorf("lease %s: %w", l.Metadata.Name, ErrLeaseGone)
	}
	return &t, nil
}

// Heartbeat is heartbeatLease: it renews the lease (the task becomes Running on the first
// one), appends the progress to the run, charges the usage deltas to the budgets and the
// agent's day, re-mints the git credential when it has under 10 minutes left, and says
// whether to cancel. A lease whose task left it (timeout, cancel, expiry) is ErrLeaseGone.
func (s *Scheduler) Heartbeat(ctx context.Context, agentID, leaseID string, req HeartbeatRequest) (*HeartbeatResponse, error) {
	if u := req.Usage; u != nil && (u.InputTokens < 0 || u.OutputTokens < 0 || (u.CostUSD != nil && *u.CostUSD < 0)) {
		return nil, invalid("usage", "deltas must not be negative")
	}
	l, err := s.heldLease(ctx, agentID, leaseID)
	if err != nil {
		return nil, err
	}
	if l.Status.End != "" {
		return nil, fmt.Errorf("lease %s %s: %w", leaseID, l.Status.End, ErrLeaseGone)
	}
	now := s.clk.Now()
	expires := now.Add(l.Spec.TTL)
	gone := false
	t, err := update(ctx, s.d.Store, taskKey(l.Spec.Task), newTask, func(t *v1alpha1.Task) (bool, error) {
		if !heldBy(t, agentID) {
			gone = true
			return false, nil
		}
		if t.Status.Phase == v1alpha1.TaskLeased {
			t.Status.Phase = v1alpha1.TaskRunning
		}
		t.Status.LeaseExpiresAt = expires
		return true, nil
	})
	if errors.Is(err, store.ErrNotFound) {
		gone, err = true, nil
	}
	if err != nil {
		return nil, fmt.Errorf("renew lease %s: %w", leaseID, err)
	}
	if gone {
		s.endLease(ctx, l, LeaseRevoked)
		return nil, fmt.Errorf("lease %s: the task left it: %w", leaseID, ErrLeaseGone)
	}

	resp := &HeartbeatResponse{ExpiresAt: expires}
	credExpires := l.Status.CredentialExpiresAt
	if !credExpires.IsZero() && credExpires.Sub(now) < CredentialRefreshBefore {
		cred, err := s.mint(ctx, t)
		switch {
		case err != nil:
			s.log.WarnContext(ctx, "refresh git credential", "lease", leaseID, "err", err)
		case cred != nil:
			resp.RefreshedGitCredential = cred
			credExpires = cred.ExpiresAt
		}
	}
	var delta LeaseUsage
	if u := req.Usage; u != nil {
		delta = LeaseUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
		if u.CostUSD != nil {
			delta.CostUSD = *u.CostUSD
		}
	}
	if _, err := update(ctx, s.d.Store, leaseKey(leaseID), newLease, func(x *Lease) (bool, error) {
		x.Status.ExpiresAt, x.Status.LastHeartbeat, x.Status.CredentialExpiresAt = expires, now, credExpires
		x.Status.Usage.InputTokens += delta.InputTokens
		x.Status.Usage.OutputTokens += delta.OutputTokens
		x.Status.Usage.CostUSD += delta.CostUSD
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("renew lease %s: %w", leaseID, err)
	}
	s.chargeUsage(ctx, l, delta)
	if _, err := s.syncAgent(ctx, agentID, func(a *v1alpha1.Agent) bool {
		a.Status.LastHeartbeat = now
		return true
	}); err != nil {
		s.log.WarnContext(ctx, "update agent on heartbeat", "agent", agentID, "err", err)
	}
	if len(req.Progress) > 0 {
		if err := s.appendProgress(ctx, l.Spec.Task, req.Progress); err != nil {
			s.log.WarnContext(ctx, "append progress", "task", l.Spec.Task, "err", err)
		}
	}
	resp.Cancel, err = s.d.Runs.CancelRequested(ctx, l.Spec.Task)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// chargeUsage adds usage to the lease's budgets (USD only for apiKey runs, per the LLD) and
// to the agent's day.
func (s *Scheduler) chargeUsage(ctx context.Context, l *Lease, u LeaseUsage) {
	d := v1alpha1.BudgetCounterStatus{Tokens: u.InputTokens + u.OutputTokens}
	if l.Spec.Auth == "apiKey" {
		d.USD = u.CostUSD
	}
	if d.Tokens == 0 && d.USD == 0 {
		return
	}
	if err := charge(ctx, s.d.Store, budgetScopes(l.Spec.Queue, l.Spec.RuleSet, l.Spec.Agent, s.clk.Now()), d); err != nil {
		s.log.WarnContext(ctx, "charge usage to budgets", "lease", l.Metadata.Name, "err", err)
	}
}

// appendProgress appends events to the task's open run (bounded ring buffer).
func (s *Scheduler) appendProgress(ctx context.Context, taskID string, events []v1alpha1.ProgressEvent) error {
	run, err := s.openRun(ctx, taskID)
	if err != nil || run == nil {
		return err
	}
	_, err = update(ctx, s.d.Store, store.KeyOf(run), func() *v1alpha1.Run { return &v1alpha1.Run{} }, func(r *v1alpha1.Run) (bool, error) {
		if r.Status == nil {
			r.Status = &v1alpha1.RunStatus{}
		}
		if r.Status.End != "" {
			return false, nil
		}
		p := append(r.Status.Progress, events...)
		if over := len(p) - maxProgress; over > 0 {
			p = slices.Clone(p[over:])
		}
		r.Status.Progress = p
		return true, nil
	})
	return err
}

// Complete is completeLease: it hands the Result to the run controller (RunAPI.Complete),
// then charges the usage the heartbeats didn't report (max(0, total − Σ deltas) per field)
// and ends the lease. A retried complete of a completed lease returns the task's phase.
func (s *Scheduler) Complete(ctx context.Context, agentID, leaseID string, res v1alpha1.Result) (v1alpha1.TaskPhase, error) {
	l, err := s.heldLease(ctx, agentID, leaseID)
	if err != nil {
		return "", err
	}
	switch l.Status.End {
	case "":
	case LeaseCompleted:
		return s.d.Runs.Complete(ctx, l.Spec.Task, res) // a finished task: its phase, nothing changes
	default:
		return "", fmt.Errorf("lease %s %s: %w", leaseID, l.Status.End, ErrLeaseGone)
	}
	if _, err := s.leasedTask(ctx, l); err != nil {
		return "", err
	}
	phase, err := s.d.Runs.Complete(ctx, l.Spec.Task, res)
	if errors.Is(err, controllers.ErrNotLeased) || errors.Is(err, controllers.ErrUnknownTask) {
		s.endLease(ctx, l, LeaseRevoked)
		return "", fmt.Errorf("lease %s: %w: %w", leaseID, ErrLeaseGone, err)
	}
	if err != nil {
		return "", err
	}
	if res.Harness != nil {
		s.chargeUsage(ctx, l, s.topUp(ctx, l, res.Harness.Usage))
	}
	s.endLease(ctx, l, LeaseCompleted)
	return phase, nil
}

// topUp is the usage the Result's cumulative total adds to the heartbeats' deltas.
func (s *Scheduler) topUp(ctx context.Context, l *Lease, total v1alpha1.Usage) LeaseUsage {
	sum := l.Status.Usage
	cost := 0.0
	if total.CostUSD != nil {
		cost = *total.CostUSD
	}
	if sum.InputTokens > total.InputTokens || sum.OutputTokens > total.OutputTokens || (total.CostUSD != nil && sum.CostUSD > cost) {
		s.log.WarnContext(ctx, "heartbeat usage exceeds the result's total", "lease", l.Metadata.Name,
			"deltaInput", sum.InputTokens, "totalInput", total.InputTokens,
			"deltaOutput", sum.OutputTokens, "totalOutput", total.OutputTokens)
	}
	u := LeaseUsage{
		InputTokens:  max(0, total.InputTokens-sum.InputTokens),
		OutputTokens: max(0, total.OutputTokens-sum.OutputTokens),
	}
	if total.CostUSD != nil {
		u.CostUSD = max(0, cost-sum.CostUSD)
	}
	return u
}

// Abandon is abandonLease: the run controller returns the task to Pending (throttled: without
// an attempt, and the agent is Throttled until req.Until) or retries or fails it per the
// queue's retry policy; the lease ends.
func (s *Scheduler) Abandon(ctx context.Context, agentID, leaseID string, req AbandonRequest) error {
	switch req.Reason {
	case controllers.AbandonThrottled, controllers.AbandonAgentError, controllers.AbandonShutdown:
	default:
		return invalid("reason", "%q is not one of throttled, agent-error, shutdown", req.Reason)
	}
	l, err := s.heldLease(ctx, agentID, leaseID)
	if err != nil {
		return err
	}
	if l.Status.End != "" {
		if l.Status.End == LeaseAbandoned {
			return nil // a retried abandon
		}
		return fmt.Errorf("lease %s %s: %w", leaseID, l.Status.End, ErrLeaseGone)
	}
	if _, err := s.leasedTask(ctx, l); err != nil {
		return err
	}
	err = s.d.Runs.Abandon(ctx, l.Spec.Task, controllers.Abandonment{Reason: req.Reason, Until: req.Until, Message: req.Message})
	if errors.Is(err, controllers.ErrNotLeased) || errors.Is(err, controllers.ErrUnknownTask) {
		s.endLease(ctx, l, LeaseRevoked)
		return fmt.Errorf("lease %s: %w: %w", leaseID, ErrLeaseGone, err)
	}
	if err != nil {
		return err
	}
	s.endLease(ctx, l, LeaseAbandoned)
	if req.Reason == controllers.AbandonThrottled {
		until := req.Until
		if !until.After(s.clk.Now()) {
			until = s.clk.Now().Add(s.opts.Throttle)
		}
		if _, err := s.syncAgent(ctx, agentID, func(a *v1alpha1.Agent) bool {
			a.Status.Phase, a.Status.ThrottledUntil = v1alpha1.AgentThrottled, &until
			return true
		}); err != nil {
			return err
		}
		s.log.InfoContext(ctx, "agent throttled", "agent", agentID, "until", until)
	}
	return nil
}

// endLease marks a lease ended (once), refreshes its agent and wakes the parked polls.
func (s *Scheduler) endLease(ctx context.Context, l *Lease, end string) {
	now := s.clk.Now()
	_, err := update(ctx, s.d.Store, leaseKey(l.Metadata.Name), newLease, func(x *Lease) (bool, error) {
		if x.Status.End != "" {
			return false, nil
		}
		x.Status.End, x.Status.EndedAt = end, now
		return true, nil
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.WarnContext(ctx, "end lease", "lease", l.Metadata.Name, "err", err)
	}
	l.Status.End, l.Status.EndedAt = end, now
	if _, err := s.syncAgent(ctx, l.Spec.Agent, nil); err != nil && !errors.Is(err, ErrNotFound) {
		s.log.WarnContext(ctx, "update agent after lease end", "agent", l.Spec.Agent, "err", err)
	}
	s.log.InfoContext(ctx, "lease ended", "lease", l.Metadata.Name, "task", l.Spec.Task, "agent", l.Spec.Agent, "end", end)
	s.wake()
}

// syncAgent applies mutate (optional) to the agent and refreshes what the scheduler derives:
// status.running (its active tasks), status.today (its day's counter) and the phase
// (Ready/Busy by capacity, Throttled ended).
func (s *Scheduler) syncAgent(ctx context.Context, agentID string, mutate func(*v1alpha1.Agent) bool) (*v1alpha1.Agent, error) {
	running, err := s.runningTasks(ctx, agentID)
	if err != nil {
		return nil, err
	}
	now := s.clk.Now()
	today, err := readCounter(ctx, s.d.Store, CounterName(ScopeAgent, agentID, now))
	if err != nil {
		return nil, err
	}
	var pools map[string]*v1alpha1.Pool
	if cat := s.d.Catalog.Current(); cat != nil {
		pools = cat.Pools
	}
	return s.updateAgent(ctx, agentID, func(a *v1alpha1.Agent) (bool, error) {
		changed := mutate != nil && mutate(a)
		st := a.Status
		if !slices.Equal(st.Running, running) {
			st.Running, changed = running, true
		}
		if u := (v1alpha1.AgentUsage{Runs: today.Runs, Tokens: today.Tokens}); st.Today == nil || *st.Today != u {
			st.Today, changed = &u, true
		}
		if settlePhase(a, pools[a.Spec.Pool], now) {
			changed = true
		}
		return changed, nil
	})
}

// runningTasks are the ids of the tasks leased to agentID, sorted.
func (s *Scheduler) runningTasks(ctx context.Context, agentID string) ([]string, error) {
	var out []string
	for _, phase := range []v1alpha1.TaskPhase{v1alpha1.TaskLeased, v1alpha1.TaskRunning, v1alpha1.TaskCancelling} {
		var tasks []*v1alpha1.Task
		if _, err := s.d.Store.List(ctx, v1alpha1.KindTask, store.ListOptions{
			Field: map[string]string{store.FieldTaskPhase: string(phase)},
		}, &tasks); err != nil {
			return nil, fmt.Errorf("list %s tasks: %w", phase, err)
		}
		for _, t := range tasks {
			if heldBy(t, agentID) {
				out = append(out, t.Metadata.ID)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}
