package scheduler

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/store"
)

// TestLeaseExpiry: a lease with no heartbeat for its TTL (2m) expires; the queue's retry
// policy (poc: max 1 on agent-lost) gives one more attempt, then the task fails. A throttled
// abandon never consumes an attempt.
func TestLeaseExpiry(t *testing.T) {
	e := newEnv(t)
	a, _ := e.register("gus", "mbp", lendQueues(pocQueue))
	e.task("tsk_a", "implement", nil)

	// First lease: a heartbeat renews it, then the agent goes silent.
	g := e.mustLease(a, "tsk_a")
	e.clk.Advance(time.Minute)
	hb, err := e.s.Heartbeat(e.ctx, a, g.Lease.ID, HeartbeatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.getTask("tsk_a").Status.Phase; got != v1alpha1.TaskRunning {
		t.Fatalf("after the first heartbeat: phase %s, want Running", got)
	}
	if want := e.clk.Now().Add(2 * time.Minute); !hb.ExpiresAt.Equal(want) {
		t.Fatalf("heartbeat expiresAt = %s, want %s", hb.ExpiresAt, want)
	}
	e.clk.Advance(90 * time.Second)
	if res := e.reconcile("tsk_a"); res.RequeueAfter != 30*time.Second {
		t.Fatalf("before expiry: requeue after %s, want 30s", res.RequeueAfter)
	}
	e.clk.Advance(30 * time.Second)
	e.reconcile("tsk_a")
	got := e.getTask("tsk_a")
	if got.Status.Phase != v1alpha1.TaskPending || got.Status.Attempts != 1 || got.Status.Reason != ReasonAgentLost || got.Status.Agent != nil {
		t.Fatalf("after expiry: %+v, want Pending, 1 attempt, agent-lost, no agent", got.Status)
	}
	if l := e.getLease(g.Lease.ID); l.Status.End != LeaseExpired {
		t.Fatalf("lease end = %q, want expired", l.Status.End)
	}
	if _, err := e.s.Heartbeat(e.ctx, a, g.Lease.ID, HeartbeatRequest{}); !errors.Is(err, ErrLeaseGone) {
		t.Fatalf("heartbeat on an expired lease: %v, want ErrLeaseGone", err)
	}
	if got := e.getAgent(a).Status.Running; len(got) != 0 {
		t.Fatalf("agent still runs %v", got)
	}

	// A throttled abandon: back to Pending without an attempt, the agent throttled.
	g = e.mustLease(a, "tsk_a")
	until := e.clk.Now().Add(time.Hour)
	if err := e.s.Abandon(e.ctx, a, g.Lease.ID, AbandonRequest{Reason: controllers.AbandonThrottled, Until: until}); err != nil {
		t.Fatal(err)
	}
	got = e.getTask("tsk_a")
	if got.Status.Phase != v1alpha1.TaskPending || got.Status.Attempts != 1 || got.Status.Reason != controllers.ReasonThrottled {
		t.Fatalf("after a throttled abandon: %+v, want Pending, still 1 attempt, agent-throttled", got.Status)
	}
	ag := e.getAgent(a)
	if ag.Status.Phase != v1alpha1.AgentThrottled || ag.Status.ThrottledUntil == nil || !ag.Status.ThrottledUntil.Equal(until) {
		t.Fatalf("agent status = %+v, want Throttled until %s", ag.Status, until)
	}
	if r := e.explain("tsk_a", a); r != ReasonAgentThrottled {
		t.Fatalf("Explain = %q, want agent-throttled", r)
	}
	e.clk.Advance(time.Hour)

	// Second expiry: the retry policy is spent, the task fails.
	g = e.mustLease(a, "tsk_a")
	e.clk.Advance(2 * time.Minute)
	e.reconcile("tsk_a")
	got = e.getTask("tsk_a")
	if got.Status.Phase != v1alpha1.TaskFailed || got.Status.Reason != ReasonAgentLost {
		t.Fatalf("after the second expiry: %+v, want Failed agent-lost", got.Status)
	}
	if l := e.getLease(g.Lease.ID); l.Status.End != LeaseExpired {
		t.Fatalf("second lease end = %q, want expired", l.Status.End)
	}
	// The run controller then closes the run as failed.
	if _, err := e.runs.Reconcile(e.ctx, "tsk_a"); err != nil {
		t.Fatal(err)
	}
	run, err := e.s.openRun(e.ctx, "tsk_a")
	if err != nil || run != nil {
		t.Fatalf("open run after the failure: %v, %v", run, err)
	}

	// Ended leases are deleted after the retention.
	e.clk.Advance(DefaultLeaseRetention)
	e.reconcile("tsk_a")
	var leases []*Lease
	if _, err := e.st.List(e.ctx, KindLease, store.ListOptions{}, &leases); err != nil || len(leases) != 0 {
		t.Fatalf("leases after retention: %d (%v)", len(leases), err)
	}
}

// TestLeaseRevokedOnTimeout: the run controller times a run out (Failed timeout); the
// scheduler revokes its lease, and the agent's next heartbeat is told the lease is gone.
func TestLeaseRevokedOnTimeout(t *testing.T) {
	e := newEnv(t)
	a, _ := e.register("gus", "mbp", lendQueues(pocQueue))
	e.task("tsk_a", "implement", func(t *v1alpha1.Task) { t.Spec.Limits = &v1alpha1.Limits{MaxWallClock: "1m"} })
	g := e.mustLease(a, "tsk_a")
	for range 6 { // 3m = maxWallClock + 2m grace, heartbeating every 30s
		e.clk.Advance(30 * time.Second)
		if _, err := e.s.Heartbeat(e.ctx, a, g.Lease.ID, HeartbeatRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.runs.Reconcile(e.ctx, "tsk_a"); err != nil {
		t.Fatal(err)
	}
	if got := e.getTask("tsk_a").Status; got.Phase != v1alpha1.TaskFailed || got.Reason != controllers.ReasonTimeout {
		t.Fatalf("task = %+v, want Failed timeout", got)
	}
	e.reconcile("tsk_a")
	if l := e.getLease(g.Lease.ID); l.Status.End != LeaseRevoked {
		t.Fatalf("lease end = %q, want revoked", l.Status.End)
	}
	if _, err := e.s.Heartbeat(e.ctx, a, g.Lease.ID, HeartbeatRequest{}); !errors.Is(err, ErrLeaseGone) {
		t.Fatalf("heartbeat after the timeout: %v, want ErrLeaseGone", err)
	}
	if got := e.getAgent(a).Status; got.Phase != v1alpha1.AgentReady || len(got.Running) != 0 {
		t.Fatalf("agent = %+v, want Ready with nothing running", got)
	}
}

// TestCancel: a Cancelling task's heartbeat says cancel; an ack (abandon) cancels it, and a
// lease that expires while Cancelling cancels it too.
func TestCancel(t *testing.T) {
	e := newEnv(t)
	a, _ := e.register("gus", "mbp", lendQueues(pocQueue))
	cancel := func(id string) {
		if _, err := update(e.ctx, e.st, taskKey(id), newTask, func(t *v1alpha1.Task) (bool, error) {
			t.Status.Phase = v1alpha1.TaskCancelling
			return true, nil
		}); err != nil {
			e.t.Fatal(err)
		}
	}
	e.task("tsk_a", "implement", nil)
	g := e.mustLease(a, "tsk_a")
	cancel("tsk_a")
	hb, err := e.s.Heartbeat(e.ctx, a, g.Lease.ID, HeartbeatRequest{})
	if err != nil || !hb.Cancel {
		t.Fatalf("heartbeat = %+v, %v; want cancel", hb, err)
	}
	if err := e.s.Abandon(e.ctx, a, g.Lease.ID, AbandonRequest{Reason: controllers.AbandonShutdown}); err != nil {
		t.Fatal(err)
	}
	if got := e.getTask("tsk_a").Status.Phase; got != v1alpha1.TaskCancelled {
		t.Fatalf("after the ack: %s, want Cancelled", got)
	}

	e.task("tsk_b", "implement", nil)
	e.mustLease(a, "tsk_b")
	cancel("tsk_b")
	e.clk.Advance(2 * time.Minute)
	e.reconcile("tsk_b")
	if got := e.getTask("tsk_b").Status.Phase; got != v1alpha1.TaskCancelled {
		t.Fatalf("after expiry while cancelling: %s, want Cancelled", got)
	}
}

// TestHeartbeatAndComplete: progress goes to the run, deltas to the budgets and the agent's
// day, and complete adds only what the deltas missed. USD counts for apiKey runs only.
func TestHeartbeatAndComplete(t *testing.T) {
	e := newEnv(t)
	a, _ := e.register("gus", "mbp", &v1alpha1.Lending{Unlimited: true})
	e.task("tsk_r", "docs-review", nil) // claude-code, apiKey
	g := e.mustLease(a, "tsk_r")
	if g.Lease.Run == "" || g.Lease.HeartbeatEvery != 30*time.Second {
		t.Fatalf("lease info = %+v", g.Lease)
	}
	for _, d := range []UsageDelta{{InputTokens: 100, OutputTokens: 20, CostUSD: ptr(0.5)}, {InputTokens: 50, OutputTokens: 30, CostUSD: ptr(0.25)}} {
		if _, err := e.s.Heartbeat(e.ctx, a, g.Lease.ID, HeartbeatRequest{
			Usage:    &d,
			Progress: []v1alpha1.ProgressEvent{{At: e.clk.Now(), Kind: "step", Message: "working"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if c := e.counter(ScopeQueue, pocQueue); c.Runs != 1 || c.Tokens != 200 || c.USD != 0.75 {
		t.Fatalf("queue counter after heartbeats = %+v", c)
	}
	run, err := e.s.openRun(e.ctx, "tsk_r")
	if err != nil || run == nil || len(run.Status.Progress) != 2 {
		t.Fatalf("run progress: %+v, %v", run, err)
	}
	if _, err := e.s.Heartbeat(e.ctx, "agt_ana-box", g.Lease.ID, HeartbeatRequest{}); !errors.Is(err, ErrNotLeaseHolder) {
		t.Fatalf("someone else's heartbeat: %v, want ErrNotLeaseHolder", err)
	}

	res := v1alpha1.Result{
		Status: v1alpha1.ResultDone, Outcome: "approved",
		Harness: &v1alpha1.ResultHarness{Usage: v1alpha1.Usage{InputTokens: 300, OutputTokens: 60, CostUSD: ptr(1.0)}},
	}
	phase, err := e.s.Complete(e.ctx, a, g.Lease.ID, res)
	if err != nil {
		t.Fatal(err)
	}
	if c := e.counter(ScopeQueue, pocQueue); c.Tokens != 360 || c.USD != 1.0 {
		t.Fatalf("queue counter after complete = %+v, want 360 tokens, $1", c)
	}
	if c := e.counter(ScopeRuleSet, pocRuleSet); c.Runs != 1 || c.Tokens != 360 {
		t.Fatalf("ruleset counter = %+v", c)
	}
	if got := e.getAgent(a).Status.Today; got == nil || got.Runs != 1 || got.Tokens != 360 {
		t.Fatalf("agent today = %+v", got)
	}
	again, err := e.s.Complete(e.ctx, a, g.Lease.ID, res)
	if err != nil || again != phase {
		t.Fatalf("retried complete: %s, %v; want %s", again, err, phase)
	}
	if c := e.counter(ScopeQueue, pocQueue); c.Tokens != 360 {
		t.Fatalf("a retried complete charged again: %+v", c)
	}

	// A subscription run (codex) counts tokens, not USD.
	e.task("tsk_i", "implement", nil)
	g = e.mustLease(a, "tsk_i")
	if _, err := e.s.Heartbeat(e.ctx, a, g.Lease.ID, HeartbeatRequest{Usage: &UsageDelta{InputTokens: 10, CostUSD: ptr(9.0)}}); err != nil {
		t.Fatal(err)
	}
	if c := e.counter(ScopeQueue, pocQueue); c.Tokens != 370 || c.USD != 1.0 {
		t.Fatalf("queue counter after a subscription heartbeat = %+v", c)
	}
	if _, err := e.s.Heartbeat(e.ctx, a, g.Lease.ID, HeartbeatRequest{Usage: &UsageDelta{InputTokens: -1}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative delta: %v, want ErrInvalid", err)
	}
}

// TestCredentialMintedPerLeaseNeverStored: each lease mints its own credential (refreshed on
// a heartbeat near its expiry); no token reaches the store.
func TestCredentialMintedPerLeaseNeverStored(t *testing.T) {
	e := newEnv(t)
	a, _ := e.register("gus", "mbp", lendQueues(pocQueue))
	e.task("tsk_a", "implement", nil)
	g := e.mustLease(a, "tsk_a")
	if g.GitCredential == nil || g.GitCredential.Token == "" || g.GitCredential.Repo != docsRepo {
		t.Fatalf("credential = %v", g.GitCredential)
	}
	if mints := e.gh.Mints(); len(mints) != 1 || mints[0].Request.TTL != 55*time.Minute || mints[0].Request.Identity != "author" {
		t.Fatalf("mints = %+v", mints)
	}
	tokens := []string{g.GitCredential.Token}
	// Heartbeat every 30s until the credential has under 10 minutes left: it is re-minted.
	var refreshed bool
	for range 100 {
		e.clk.Advance(30 * time.Second)
		hb, err := e.s.Heartbeat(e.ctx, a, g.Lease.ID, HeartbeatRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if hb.RefreshedGitCredential != nil {
			if left := g.GitCredential.ExpiresAt.Sub(e.clk.Now()); left >= CredentialRefreshBefore {
				t.Fatalf("refreshed with %s left", left)
			}
			tokens = append(tokens, hb.RefreshedGitCredential.Token)
			refreshed = true
			break
		}
	}
	if !refreshed {
		t.Fatal("credential never refreshed")
	}
	dump := e.dump()
	for _, tok := range tokens {
		if strings.Contains(dump, tok) {
			t.Fatalf("credential token %q found in the store", tok)
		}
	}
}

// TestCredentialFailureLeavesTaskPending: a task whose credential can't be minted isn't
// leased; the next task is.
func TestCredentialFailureLeavesTaskPending(t *testing.T) {
	e := newEnv(t)
	a, _ := e.register("gus", "mbp", lendQueues(pocQueue))
	e.task("tsk_a", "implement", func(t *v1alpha1.Task) { t.Spec.Credentials.Git.Identity = "nobody" })
	e.task("tsk_b", "implement", nil)
	e.mustLease(a, "tsk_b")
	if got := e.getTask("tsk_a").Status; got.Phase != v1alpha1.TaskPending || got.Reason != ReasonGitCredential {
		t.Fatalf("task a = %+v, want Pending git-credential", got)
	}
}

// TestLendingLimits: maxConcurrent, maxRunsPerDay (reset on the next UTC day) and hours that
// cross midnight in a timezone.
func TestLendingLimits(t *testing.T) {
	t.Run("maxConcurrent", func(t *testing.T) {
		e := newEnv(t)
		e.cat.Pools[pocPool].Spec.Lease.MaxTasksPerAgent = 3
		a, _ := e.register("gus", "mbp", &v1alpha1.Lending{Queues: []string{pocQueue}, MaxConcurrent: 2})
		e.task("tsk_a", "implement", nil)
		e.task("tsk_b", "implement", nil)
		e.cat.Queues[pocQueue].Spec.Concurrency = 5
		e.task("tsk_c", "implement", nil)
		e.mustLease(a, "tsk_a")
		if got := e.getAgent(a).Status.Phase; got != v1alpha1.AgentReady {
			t.Fatalf("one of two slots used: %s, want Ready", got)
		}
		e.mustLease(a, "tsk_b")
		if got := e.getAgent(a).Status; got.Phase != v1alpha1.AgentBusy || len(got.Running) != 2 {
			t.Fatalf("both slots used: %+v, want Busy with 2 running", got)
		}
		if r := e.explain("tsk_c", a); r != ReasonAgentBusy {
			t.Fatalf("Explain = %q, want agent-busy", r)
		}
	})

	t.Run("maxRunsPerDay", func(t *testing.T) {
		e := newEnv(t)
		e.clk.Set(time.Date(2026, 10, 16, 22, 0, 0, 0, time.UTC))
		a, _ := e.register("gus", "mbp", &v1alpha1.Lending{Queues: []string{pocQueue}, MaxRunsPerDay: 2})
		for _, id := range []string{"tsk_a", "tsk_b", "tsk_c"} {
			e.task(id, "implement", nil)
		}
		for _, id := range []string{"tsk_a", "tsk_b"} {
			g := e.mustLease(a, id)
			if _, err := e.s.Complete(e.ctx, a, g.Lease.ID, v1alpha1.Result{Status: v1alpha1.ResultDone, Outcome: "pr-opened"}); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.getAgent(a).Status.Today; got == nil || got.Runs != 2 {
			t.Fatalf("today = %+v, want 2 runs", got)
		}
		if r := e.explain("tsk_c", a); r != ReasonLending {
			t.Fatalf("Explain = %q, want lending", r)
		}
		e.clk.Set(time.Date(2026, 10, 17, 0, 0, 1, 0, time.UTC)) // next UTC day: a new counter
		e.mustLease(a, "tsk_c")
	})

	t.Run("hours across midnight in a timezone", func(t *testing.T) {
		e := newEnv(t)
		// Weeknights from 22:00 to 06:00 Lisbon time (UTC+1 in October until the 25th).
		a, _ := e.register("gus", "mbp", &v1alpha1.Lending{Queues: []string{pocQueue}, Hours: "Mon-Fri 22:00-06:00 Europe/Lisbon"})
		e.task("tsk_a", "implement", nil)
		for _, c := range []struct {
			at   time.Time
			want string
		}{
			{time.Date(2026, 10, 16, 20, 59, 0, 0, time.UTC), ReasonLending}, // Fri 21:59 Lisbon
			{time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC), ""},             // Fri 22:00
			{time.Date(2026, 10, 17, 2, 0, 0, 0, time.UTC), ""},              // Sat 03:00: Friday's night
			{time.Date(2026, 10, 17, 5, 0, 0, 0, time.UTC), ReasonLending},   // Sat 06:00
			{time.Date(2026, 10, 17, 21, 30, 0, 0, time.UTC), ReasonLending}, // Sat 22:30: not a weekday
			{time.Date(2026, 10, 19, 2, 0, 0, 0, time.UTC), ReasonLending},   // Mon 03:00: Sunday's night
		} {
			e.clk.Set(c.at)
			if r := e.explain("tsk_a", a); r != c.want {
				t.Fatalf("at %s: Explain = %q, want %q", c.at.In(mustLoc(t, "Europe/Lisbon")), r, c.want)
			}
		}
		e.clk.Set(time.Date(2026, 10, 19, 22, 30, 0, 0, time.UTC)) // Mon 23:30 Lisbon
		e.mustLease(a, "tsk_a")
	})
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestBudgetDeferral: once the queue's daily runs are spent, tasks stay Pending with reason
// budget until the next UTC day.
func TestBudgetDeferral(t *testing.T) {
	e := newEnv(t)
	e.cat.Queues[pocQueue].Spec.Budget = &v1alpha1.Budget{Daily: &v1alpha1.BudgetLimit{Runs: 1}}
	a, _ := e.register("gus", "mbp", &v1alpha1.Lending{Unlimited: true})
	e.task("tsk_a", "implement", nil)
	e.task("tsk_b", "implement", nil)
	g := e.mustLease(a, "tsk_a")
	if _, err := e.s.Complete(e.ctx, a, g.Lease.ID, v1alpha1.Result{Status: v1alpha1.ResultDone, Outcome: "pr-opened"}); err != nil {
		t.Fatal(err)
	}
	if g := e.lease(a); g != nil {
		t.Fatalf("leased %s over budget", grantTask(g))
	}
	if err := e.s.Refresh(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.getTask("tsk_b").Status; got.Phase != v1alpha1.TaskPending || got.Reason != ReasonBudget {
		t.Fatalf("task b = %+v, want Pending budget", got)
	}
	e.clk.Set(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
	e.mustLease(a, "tsk_b")
}

// TestBudgetCounterCAS: concurrent charges to one counter all count.
func TestBudgetCounterCAS(t *testing.T) {
	e := newEnv(t)
	const n = 50
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if _, err := addCounter(e.ctx, e.st, CounterName(ScopeQueue, pocQueue, t0), v1alpha1.BudgetCounterStatus{Runs: 1, Tokens: 10, USD: 0.5}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if c := e.counter(ScopeQueue, pocQueue); c.Runs != n || c.Tokens != 10*n || c.USD != 0.5*n {
		t.Fatalf("counter = %+v, want %d runs", c, n)
	}
	if got := CounterName(ScopeRuleSet, "docs", t0); got != "budget.ruleset.docs.2026-10-16" {
		t.Fatalf("counter name = %q", got)
	}
}

// TestConcurrentPolls: agents polling at once never lease one task twice nor exceed the
// queue's concurrency.
func TestConcurrentPolls(t *testing.T) {
	e := newEnv(t)
	e.cat.Queues[pocQueue].Spec.Concurrency = 3
	var agents []string
	for _, login := range []string{"gus", "ana", "bob"} {
		for _, host := range []string{"h1", "h2"} {
			id, _ := e.register(login, host, &v1alpha1.Lending{Unlimited: true})
			agents = append(agents, id)
		}
	}
	for _, id := range []string{"tsk_1", "tsk_2", "tsk_3", "tsk_4", "tsk_5"} {
		e.task(id, "implement", nil)
	}
	var (
		mu  sync.Mutex
		got = map[string]string{}
		wg  sync.WaitGroup
	)
	for _, a := range agents {
		wg.Go(func() {
			g, err := e.s.Lease(e.ctx, a, 0)
			if err != nil {
				t.Error(err)
				return
			}
			if g != nil {
				mu.Lock()
				defer mu.Unlock()
				if prev, dup := got[g.Task.Metadata.ID]; dup {
					t.Errorf("task %s leased to %s and %s", g.Task.Metadata.ID, prev, a)
				}
				got[g.Task.Metadata.ID] = a
			}
		})
	}
	wg.Wait()
	if len(got) != 3 {
		t.Fatalf("leased %d tasks (%v), want the queue's concurrency 3", len(got), got)
	}
}

// TestLongPollWakes: a parked poll returns as soon as a task arrives (store watch) or a slot
// frees up, and empty when its wait runs out.
func TestLongPollWakes(t *testing.T) {
	e := newEnv(t)
	e.run()
	a, _ := e.register("gus", "mbp", lendQueues(pocQueue))
	poll := func() <-chan *Grant {
		ch := make(chan *Grant, 1)
		before := e.s.parked.Load()
		go func() {
			g, err := e.s.Lease(e.ctx, a, 30*time.Second)
			if err != nil {
				t.Error(err)
			}
			ch <- g
		}()
		eventually(t, "the poll to park", func() bool { return e.s.parked.Load() > before })
		return ch
	}

	// A new Pending task wakes it.
	ch := poll()
	e.task("tsk_a", "implement", nil)
	g := recv(t, ch, "the lease")
	if grantTask(g) != "tsk_a" {
		t.Fatalf("got %s, want tsk_a", grantTask(g))
	}

	// The agent is busy; completing its task frees the slot and wakes the next poll.
	e.task("tsk_b", "implement", nil)
	ch = poll()
	if _, err := e.s.Complete(e.ctx, a, g.Lease.ID, v1alpha1.Result{Status: v1alpha1.ResultDone, Outcome: "pr-opened"}); err != nil {
		t.Fatal(err)
	}
	g = recv(t, ch, "the lease after the slot freed")
	if grantTask(g) != "tsk_b" {
		t.Fatalf("got %s, want tsk_b", grantTask(g))
	}

	// Nothing to take: the poll returns empty after its wait.
	if _, err := e.s.Complete(e.ctx, a, g.Lease.ID, v1alpha1.Result{Status: v1alpha1.ResultDone, Outcome: "pr-opened"}); err != nil {
		t.Fatal(err)
	}
	ch = poll()
	e.clk.Advance(31 * time.Second)
	if g := recv(t, ch, "the empty poll"); g != nil {
		t.Fatalf("got %s, want nothing", grantTask(g))
	}
}
