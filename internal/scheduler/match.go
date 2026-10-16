package scheduler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/patterns"
	"github.com/compliance-framework/factory/internal/store"
)

// Rows of the LLD's matching table, in the order they are checked. A verdict's row is the
// first that failed (rowMatch when none did).
const (
	rowPool         = 1 // the task's queue's pool is the agent's (else not a candidate)
	rowQueue        = 2 // queue concurrency → queue-full
	rowBudget       = 3 // queue and RuleSet budgets → budget
	rowAgent        = 4 // agent phase, throttling, capacity → agent-busy / agent-throttled
	rowLending      = 5 // the owner's lending → lending
	rowLabels       = 6 // requires.labels ⊆ agent labels → no-capable-agent
	rowHarness      = 7 // patterns.Resolve → no-capable-agent
	rowAntiAffinity = 8 // a reviewer task on its author's agent → anti-affinity
	rowLocalStack   = 9 // one has-local-stack task per agent → agent-busy
	rowMatch        = 10
)

// ReasonNotInPool is Explain's answer for an agent that isn't a candidate at all: the task's
// queue feeds another pool (or the queue or pool is gone from the catalog).
const ReasonNotInPool = "not-in-pool"

// Names the matcher knows.
const (
	// LabelLocalStack marks tasks that need the agent's local stack; an agent runs one at a
	// time.
	LabelLocalStack = "has-local-stack"
	// IdentityReviewer and IdentityAuthor are the TaskSpec identities anti-affinity compares.
	IdentityReviewer = "reviewer"
	IdentityAuthor   = "author"
)

// verdict is one (task, agent) check.
type verdict struct {
	row      int
	reason   string
	resolved *v1alpha1.HarnessResolved
	offer    v1alpha1.HarnessOffer
}

func (v verdict) ok() bool { return v.row == rowMatch }

// errNoCatalog is a snapshot taken before the first catalog loaded: nothing matches.
var errNoCatalog = errors.New("no catalog loaded yet")

// snapshot is what one matching pass reads: the catalog, the agents, the pending and active
// tasks, and (lazily) budget counters and author agents. The scheduler's mu is held while a
// snapshot is used.
type snapshot struct {
	ctx context.Context
	s   *Scheduler
	now time.Time
	cat *catalog.Catalog

	agents         map[string]*v1alpha1.Agent
	pending        []*v1alpha1.Task
	runningByAgent map[string][]*v1alpha1.Task
	runningByQueue map[string]int

	counters   map[string]v1alpha1.BudgetCounterStatus
	lastAuthor map[string]string // work item → agent of its last author run
	taskLevel  map[string]verdict
}

// loadSnapshot reads the store. The caller holds s.mu.
func (s *Scheduler) loadSnapshot(ctx context.Context) (*snapshot, error) {
	cat := s.d.Catalog.Current()
	if cat == nil {
		return nil, errNoCatalog
	}
	sn := &snapshot{
		ctx: ctx, s: s, now: s.clk.Now(), cat: cat,
		agents:         map[string]*v1alpha1.Agent{},
		runningByAgent: map[string][]*v1alpha1.Task{},
		runningByQueue: map[string]int{},
		counters:       map[string]v1alpha1.BudgetCounterStatus{},
		lastAuthor:     map[string]string{},
		taskLevel:      map[string]verdict{},
	}
	var agents []*v1alpha1.Agent
	if _, err := s.d.Store.List(ctx, v1alpha1.KindAgent, store.ListOptions{}, &agents); err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	for _, a := range agents {
		ensureAgentStatus(a)
		sn.agents[a.Metadata.Name] = a
	}
	for _, phase := range []v1alpha1.TaskPhase{v1alpha1.TaskPending, v1alpha1.TaskLeased, v1alpha1.TaskRunning, v1alpha1.TaskCancelling} {
		var tasks []*v1alpha1.Task
		if _, err := s.d.Store.List(ctx, v1alpha1.KindTask, store.ListOptions{
			Field: map[string]string{store.FieldTaskPhase: string(phase)},
		}, &tasks); err != nil {
			return nil, fmt.Errorf("list %s tasks: %w", phase, err)
		}
		for _, t := range tasks {
			ensureTaskStatus(t)
			if phase == v1alpha1.TaskPending {
				sn.pending = append(sn.pending, t)
				continue
			}
			sn.runningByQueue[t.Spec.Queue]++
			if t.Status.Agent != nil {
				sn.runningByAgent[*t.Status.Agent] = append(sn.runningByAgent[*t.Status.Agent], t)
			}
		}
	}
	slices.SortStableFunc(sn.pending, func(a, b *v1alpha1.Task) int {
		return cmp.Or(
			cmp.Compare(sn.priority(b), sn.priority(a)), // higher priority first
			a.Metadata.CreatedAt.Compare(b.Metadata.CreatedAt),
			cmp.Compare(a.Metadata.ID, b.Metadata.ID),
		)
	})
	return sn, nil
}

func (sn *snapshot) priority(t *v1alpha1.Task) int {
	if q := sn.cat.Queues[t.Spec.Queue]; q != nil {
		return q.Spec.Priority
	}
	return 0
}

// counter reads (once per snapshot) a budget counter.
func (sn *snapshot) counter(name string) (v1alpha1.BudgetCounterStatus, error) {
	if c, ok := sn.counters[name]; ok {
		return c, nil
	}
	c, err := readCounter(sn.ctx, sn.s.d.Store, name)
	if err != nil {
		return c, err
	}
	sn.counters[name] = c
	return c, nil
}

// pool returns the registered pool a task's queue feeds, or nil.
func (sn *snapshot) pool(t *v1alpha1.Task) (*v1alpha1.Queue, *v1alpha1.Pool) {
	q := sn.cat.Queues[t.Spec.Queue]
	if q == nil {
		return nil, nil
	}
	p := sn.cat.Pools[q.Spec.Pool]
	if p == nil || p.Spec.Mode != v1alpha1.PoolRegistered {
		return q, nil
	}
	return q, p
}

// check runs the matching table for (t, a).
func (sn *snapshot) check(t *v1alpha1.Task, a *v1alpha1.Agent) (verdict, error) {
	q, pool := sn.pool(t)
	if pool == nil || q.Spec.Pool != a.Spec.Pool {
		return verdict{row: rowPool, reason: ReasonNotInPool}, nil
	}
	if v, err := sn.taskVerdict(t, q); err != nil || !v.ok() {
		return v, err
	}
	if r := sn.agentReason(a, pool); r != "" {
		return verdict{row: rowAgent, reason: r}, nil
	}
	ts := sn.cat.TaskSpecs[t.Spec.TaskSpec.Name]
	if ok, err := sn.lends(a, t, ts, false); err != nil || !ok {
		return verdict{row: rowLending, reason: ReasonLending}, err
	}
	if !hasLabels(a, t) {
		return verdict{row: rowLabels, reason: ReasonNoCapableAgent}, nil
	}
	resolved, offer := resolveHarness(ts, t, a)
	if resolved == nil {
		return verdict{row: rowHarness, reason: ReasonNoCapableAgent}, nil
	}
	if ok, err := sn.antiAffinity(t, ts, a); err != nil || !ok {
		return verdict{row: rowAntiAffinity, reason: ReasonAntiAffinity}, err
	}
	if slices.Contains(requiredLabels(t), LabelLocalStack) {
		for _, r := range sn.runningByAgent[a.Metadata.Name] {
			if r.Metadata.ID != t.Metadata.ID && slices.Contains(requiredLabels(r), LabelLocalStack) {
				return verdict{row: rowLocalStack, reason: ReasonAgentBusy}, nil
			}
		}
	}
	return verdict{row: rowMatch, resolved: resolved, offer: offer}, nil
}

// taskVerdict is rows 2 and 3, which don't depend on the agent (memoised per snapshot).
func (sn *snapshot) taskVerdict(t *v1alpha1.Task, q *v1alpha1.Queue) (verdict, error) {
	if v, ok := sn.taskLevel[t.Metadata.ID]; ok {
		return v, nil
	}
	v := verdict{row: rowMatch}
	if c := q.Spec.Concurrency; c > 0 && sn.runningByQueue[q.Metadata.Name] >= c {
		v = verdict{row: rowQueue, reason: ReasonQueueFull}
	} else {
		over, err := sn.overBudget(t, q)
		if err != nil {
			return verdict{}, err
		}
		if over != "" {
			v = verdict{row: rowBudget, reason: ReasonBudget}
		}
	}
	sn.taskLevel[t.Metadata.ID] = v
	return v, nil
}

// overBudget says which of the queue's and the RuleSet's daily budgets t's lease would
// exceed ("" for none).
func (sn *snapshot) overBudget(t *v1alpha1.Task, q *v1alpha1.Queue) (string, error) {
	if q.Spec.Budget != nil {
		c, err := sn.counter(CounterName(ScopeQueue, q.Metadata.Name, sn.now))
		if err != nil {
			return "", err
		}
		if r := spent(c, q.Spec.Budget); r != "" {
			return "queue " + q.Metadata.Name + ": " + r, nil
		}
	}
	if name := ruleSetOf(t); name != "" {
		if rs := sn.cat.RuleSets[name]; rs != nil && rs.Spec != nil && rs.Spec.Spec.Budget != nil {
			c, err := sn.counter(CounterName(ScopeRuleSet, name, sn.now))
			if err != nil {
				return "", err
			}
			if r := spent(c, rs.Spec.Spec.Budget); r != "" {
				return "ruleset " + name + ": " + r, nil
			}
		}
	}
	return "", nil
}

// agentReason is row 4: the agent takes new tasks, isn't throttled and has a free slot.
func (sn *snapshot) agentReason(a *v1alpha1.Agent, pool *v1alpha1.Pool) string {
	switch {
	case throttled(a, sn.now):
		return ReasonAgentThrottled
	case !serving(a, sn.now):
		return ReasonAgentBusy // Draining or Offline: no new tasks
	case len(sn.runningByAgent[a.Metadata.Name]) >= capacity(a, pool):
		return ReasonAgentBusy
	}
	return ""
}

// lends is row 5: the owner's lending covers the task. With static, only the queue, RuleSet
// and harness lists are checked (not the hours or the daily caps), which is what "could this
// agent ever take it" needs.
func (sn *snapshot) lends(a *v1alpha1.Agent, t *v1alpha1.Task, ts *catalog.CompiledTaskSpec, static bool) (bool, error) {
	l := a.Spec.Lending
	switch {
	case l == nil:
		return false, nil // no consent
	case l.Unlimited:
		return true, nil
	case !slices.Contains(l.Queues, t.Spec.Queue):
		return false, nil
	case len(l.RuleSets) > 0 && !slices.Contains(l.RuleSets, ruleSetOf(t)):
		return false, nil
	}
	if h := requestedHarness(t, ts); len(l.Harnesses) > 0 && h != "" && !slices.Contains(l.Harnesses, h) {
		return false, nil
	}
	if static {
		return true, nil
	}
	if l.Hours != "" {
		h, err := ParseHours(l.Hours)
		if err != nil || !h.Contains(sn.now) {
			return false, nil // hours are validated at registration; unparsable ones lend nothing
		}
	}
	if l.MaxRunsPerDay > 0 || l.MaxTokensPerDay > 0 {
		c, err := sn.counter(CounterName(ScopeAgent, a.Metadata.Name, sn.now))
		if err != nil {
			return false, err
		}
		if (l.MaxRunsPerDay > 0 && c.Runs >= l.MaxRunsPerDay) || (l.MaxTokensPerDay > 0 && c.Tokens >= l.MaxTokensPerDay) {
			return false, nil
		}
	}
	return true, nil
}

func requiredLabels(t *v1alpha1.Task) []string {
	if t.Spec.Requires == nil {
		return nil
	}
	return t.Spec.Requires.Labels
}

// hasLabels is row 6: requires.labels ⊆ the agent's labels.
func hasLabels(a *v1alpha1.Agent, t *v1alpha1.Task) bool {
	for _, l := range requiredLabels(t) {
		if !slices.Contains(a.Spec.Capabilities.Labels, l) {
			return false
		}
	}
	return true
}

// requestedHarness is the harness a task asks for: its dispatch request, else the TaskSpec's
// default; "" when neither names one.
func requestedHarness(t *v1alpha1.Task, ts *catalog.CompiledTaskSpec) string {
	if t.Spec.Harness != nil && t.Spec.Harness.Request != nil {
		return t.Spec.Harness.Request.Name
	}
	if ts != nil && ts.Default != nil {
		return ts.Default.Harness
	}
	return ""
}

// resolveHarness is row 7: patterns.Resolve(TaskSpec constraints, the task's request, the
// agent's offer). A task without a request (and a TaskSpec without a default) takes the
// first offer the constraints allow, in the agent's order, within its lending's harnesses.
func resolveHarness(ts *catalog.CompiledTaskSpec, t *v1alpha1.Task, a *v1alpha1.Agent) (*v1alpha1.HarnessResolved, v1alpha1.HarnessOffer) {
	if ts == nil {
		return nil, v1alpha1.HarnessOffer{}
	}
	var reqs []patterns.Request
	switch {
	case t.Spec.Harness != nil && t.Spec.Harness.Request != nil:
		r, err := patterns.RequestFrom(*t.Spec.Harness.Request)
		if err != nil {
			return nil, v1alpha1.HarnessOffer{}
		}
		reqs = []patterns.Request{r}
	case ts.Default != nil:
		reqs = []patterns.Request{*ts.Default}
	default:
		for _, off := range a.Spec.Capabilities.Harnesses {
			if l := a.Spec.Lending; l != nil && !l.Unlimited && len(l.Harnesses) > 0 && !slices.Contains(l.Harnesses, off.Name) {
				continue
			}
			reqs = append(reqs, patterns.Request{Harness: off.Name})
		}
	}
	for _, req := range reqs {
		for _, off := range a.Spec.Capabilities.Harnesses {
			if off.Name != req.Harness {
				continue
			}
			r, err := patterns.Resolve(ts.Constraints, req, patterns.OfferFrom(off))
			if err == nil {
				hr := r.HarnessResolved(off)
				return &hr, off
			}
		}
	}
	return nil, v1alpha1.HarnessOffer{}
}

// antiAffinity is row 8: a reviewer task skips the agent that ran the work item's last
// author run, unless no other agent could take it and it has waited AntiAffinityWait.
func (sn *snapshot) antiAffinity(t *v1alpha1.Task, ts *catalog.CompiledTaskSpec, a *v1alpha1.Agent) (bool, error) {
	if ts == nil || ts.Spec.Spec.Identity != IdentityReviewer {
		return true, nil
	}
	author, err := sn.lastAuthorAgent(t.Metadata.WorkItem)
	if err != nil || author != a.Metadata.Name {
		return err == nil, err
	}
	held := sn.s.heldSince
	if other, err := sn.otherCapable(t, ts, a); err != nil || other {
		delete(held, t.Metadata.ID)
		return false, err
	}
	since, ok := held[t.Metadata.ID]
	if !ok {
		since = sn.now
		held[t.Metadata.ID] = since
	}
	return sn.now.Sub(since) >= sn.s.opts.AntiAffinityWait, nil
}

// otherCapable reports whether an agent other than a, serving in the task's pool, could take
// t at all (lending lists, labels, harness): the "only Ready agent" test of anti-affinity.
func (sn *snapshot) otherCapable(t *v1alpha1.Task, ts *catalog.CompiledTaskSpec, a *v1alpha1.Agent) (bool, error) {
	for _, b := range sn.agents {
		if b.Metadata.Name == a.Metadata.Name || b.Spec.Pool != a.Spec.Pool || !serving(b, sn.now) {
			continue
		}
		ok, err := sn.lends(b, t, ts, true)
		if err != nil {
			return false, err
		}
		if !ok || !hasLabels(b, t) {
			continue
		}
		if r, _ := resolveHarness(ts, t, b); r != nil {
			return true, nil
		}
	}
	return false, nil
}

// lastAuthorAgent is the agent of the work item's most recent run of an author task.
func (sn *snapshot) lastAuthorAgent(workItem string) (string, error) {
	if a, ok := sn.lastAuthor[workItem]; ok {
		return a, nil
	}
	var tasks []*v1alpha1.Task
	if _, err := sn.s.d.Store.List(sn.ctx, v1alpha1.KindTask, store.ListOptions{
		Field: map[string]string{store.FieldTaskWorkItem: workItem},
	}, &tasks); err != nil {
		return "", fmt.Errorf("list tasks of %s: %w", workItem, err)
	}
	var last *v1alpha1.Run
	for _, t := range tasks {
		ts := sn.cat.TaskSpecs[t.Spec.TaskSpec.Name]
		if ts == nil || ts.Spec.Spec.Identity != IdentityAuthor {
			continue
		}
		var runs []*v1alpha1.Run
		if _, err := sn.s.d.Store.List(sn.ctx, v1alpha1.KindRun, store.ListOptions{
			Field: map[string]string{store.FieldRunTask: t.Metadata.ID},
		}, &runs); err != nil {
			return "", fmt.Errorf("list runs of %s: %w", t.Metadata.ID, err)
		}
		for _, r := range runs {
			if last == nil || runStart(r).After(runStart(last)) {
				last = r
			}
		}
	}
	agent := ""
	if last != nil {
		agent = last.Spec.Agent
	}
	sn.lastAuthor[workItem] = agent
	return agent, nil
}

func runStart(r *v1alpha1.Run) time.Time {
	if r.Status != nil && !r.Status.StartedAt.IsZero() {
		return r.Status.StartedAt
	}
	return r.Metadata.CreatedAt
}

// preferred is the agent fairness picks for t among the agents in a long poll (minus those
// in skip) that can take it now: least-recently-leased first, with unlimited agents last
// (prefer-limited, the default) or first (prefer-unlimited). "" when none can.
func (sn *snapshot) preferred(t *v1alpha1.Task, skip map[string]bool) (string, error) {
	_, pool := sn.pool(t)
	if pool == nil {
		return "", nil
	}
	unlimitedFirst := pool.Spec.Fairness == FairnessPreferUnlimited
	var best *v1alpha1.Agent
	for id, n := range sn.s.waiting {
		a := sn.agents[id]
		if n <= 0 || a == nil || skip[id] {
			continue
		}
		v, err := sn.check(t, a)
		if err != nil {
			return "", err
		}
		if v.ok() && (best == nil || sn.fairer(a, best, unlimitedFirst)) {
			best = a
		}
	}
	if best == nil {
		return "", nil
	}
	return best.Metadata.Name, nil
}

// fairer reports whether a comes before b.
func (sn *snapshot) fairer(a, b *v1alpha1.Agent, unlimitedFirst bool) bool {
	ua, ub := unlimited(a), unlimited(b)
	if ua != ub {
		return ua == unlimitedFirst
	}
	la, lb := sn.s.lastLeased[a.Metadata.Name], sn.s.lastLeased[b.Metadata.Name]
	if !la.Equal(lb) {
		return la.Before(lb)
	}
	return a.Metadata.Name < b.Metadata.Name
}

func unlimited(a *v1alpha1.Agent) bool { return a.Spec.Lending != nil && a.Spec.Lending.Unlimited }

// blockingReason is why no agent can take a pending task now: the reason of the agent that
// got furthest down the table ("" when some agent can, or when the task has no queue left).
func (sn *snapshot) blockingReason(t *v1alpha1.Task) (string, error) {
	q, pool := sn.pool(t)
	if pool == nil {
		return ReasonNoCapableAgent, nil
	}
	if v, err := sn.taskVerdict(t, q); err != nil || !v.ok() {
		return v.reason, err
	}
	best, found := verdict{}, false
	for _, a := range sn.agents {
		if a.Spec.Pool != q.Spec.Pool || a.Status.Phase == v1alpha1.AgentOffline {
			continue
		}
		found = true
		v, err := sn.check(t, a)
		if err != nil {
			return "", err
		}
		if v.ok() {
			return "", nil
		}
		if v.row > best.row {
			best = v
		}
	}
	if !found {
		return ReasonNoCapableAgent, nil
	}
	return best.reason, nil
}

// Explain runs the matching table for a task and an agent now and returns the first reason
// it fails ("" when the agent could take it; ReasonNotInPool when it isn't a candidate). It
// changes nothing but the anti-affinity wait.
func (s *Scheduler) Explain(ctx context.Context, taskID, agentID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn, err := s.loadSnapshot(ctx)
	if err != nil {
		return "", err
	}
	a := sn.agents[agentID]
	if a == nil {
		return "", fmt.Errorf("agent %s: %w", agentID, ErrNotFound)
	}
	var t v1alpha1.Task
	if err := s.d.Store.Get(ctx, taskKey(taskID), &t); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("task %s: %w", taskID, ErrNotFound)
		}
		return "", fmt.Errorf("get task %s: %w", taskID, err)
	}
	ensureTaskStatus(&t)
	v, err := sn.check(&t, a)
	if err != nil || v.ok() {
		return "", err
	}
	return v.reason, nil
}
