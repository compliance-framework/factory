// Package scheduler is the registered-pool scheduler (design/workstreams/dispatcher-runtime/
// LLD/scheduler-and-pools.md): agent registration and tokens, the long-poll lease matcher
// (queues, budgets, lending, harness resolution, anti-affinity, fairness), heartbeats, lease
// expiry with the queue's retry policy, and abandon.
//
// It has no HTTP: the API server (RT-S2-T03) calls Register, Authenticate, UpdateAgent,
// Deregister, Lease, Heartbeat, Complete and Abandon. Run drives the lease controller (expiry,
// revocation) and the 1 s matching tick.
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/store"
)

// Defaults for Options and for pools without a lease policy.
const (
	DefaultTick             = time.Second
	DefaultTokenTTL         = 24 * time.Hour
	DefaultTokenRenewEvery  = 10 * time.Minute
	DefaultGroupCacheTTL    = 10 * time.Minute
	DefaultAntiAffinityWait = 10 * time.Minute
	DefaultMaxWait          = 60 * time.Second
	DefaultLeaseRetention   = time.Hour
	DefaultThrottle         = 5 * time.Minute
	DefaultLeaseTTL         = 2 * time.Minute
	DefaultHeartbeat        = 30 * time.Second
	// CredentialRefreshBefore is how long before its expiry a lease's git credential is
	// re-minted on a heartbeat.
	CredentialRefreshBefore = 10 * time.Minute
	// MaxCredentialTTL caps a git credential (design/02 §Safety rails: ≤ 1h).
	MaxCredentialTTL = time.Hour
)

// Pool fairness values (Pool.spec.fairness).
const (
	FairnessPreferLimited   = "prefer-limited"
	FairnessPreferUnlimited = "prefer-unlimited"
)

// Options configure the scheduler. Zero fields take the defaults.
type Options struct {
	// Clock times leases, tokens, hours and budgets, and arms the long-poll timers.
	Clock clock.Clock
	// Logger. Default slog.Default().
	Logger *slog.Logger
	// Rand is the source of tokens and lease ids. Default crypto/rand.
	Rand io.Reader
	// Tick is the matching tick: parked polls retry and pending tasks' reasons are
	// refreshed this often. Default 1 s.
	Tick time.Duration
	// TokenTTL is an agent token's lifetime, renewed by use. Default 24 h.
	TokenTTL time.Duration
	// TokenRenewEvery bounds how often a token's expiry is written. Default 10 min.
	TokenRenewEvery time.Duration
	// GroupCacheTTL caches the owners Group's resolution. Default 10 min.
	GroupCacheTTL time.Duration
	// AntiAffinityWait is how long a reviewer task waits for another agent before its
	// author's agent may take it (soft anti-affinity). Default 10 min.
	AntiAffinityWait time.Duration
	// MaxWait caps a long poll. Default 60 s.
	MaxWait time.Duration
	// LeaseRetention keeps ended leases, so a retried complete is answered. Default 1 h.
	LeaseRetention time.Duration
	// Throttle is how long an agent throttles when it gives no until. Default 5 min.
	Throttle time.Duration
	// Workers are the lease controller's workers. Default 2.
	Workers int
}

func (o Options) withDefaults() Options {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Rand == nil {
		o.Rand = rand.Reader
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.Tick, DefaultTick)
	def(&o.TokenTTL, DefaultTokenTTL)
	def(&o.TokenRenewEvery, DefaultTokenRenewEvery)
	def(&o.GroupCacheTTL, DefaultGroupCacheTTL)
	def(&o.AntiAffinityWait, DefaultAntiAffinityWait)
	def(&o.MaxWait, DefaultMaxWait)
	def(&o.LeaseRetention, DefaultLeaseRetention)
	def(&o.Throttle, DefaultThrottle)
	return o
}

// Deps are what the scheduler works with.
type Deps struct {
	Store   store.Store
	Catalog controllers.CatalogProvider
	// Conns resolves owners Groups and mints git credentials.
	Conns controllers.ConnSource
	// Runs is the run controller: lease issued, complete, abandon, cancel.
	Runs controllers.RunAPI
	// Secrets resolves Pool.admission.tokenRef (the join token).
	Secrets provider.SecretResolver
	// Owners verifies owner tokens.
	Owners OwnerVerifier
}

// Scheduler is the registered-pool scheduler. It is safe for concurrent use.
type Scheduler struct {
	d      Deps
	opts   Options
	clk    clock.Clock
	log    *slog.Logger
	groups *groupCache
	ctrl   *controllers.Controller

	// mu serialises matching and lease commits (so queue concurrency, budgets and lending
	// are checked and charged atomically in this process) and guards the in-memory state.
	mu sync.Mutex
	// waiting counts the parked long polls per agent (fairness only picks among them).
	waiting map[string]int
	// lastLeased is when each agent last got a lease (least-recently-leased first).
	lastLeased map[string]time.Time
	// heldSince is when anti-affinity first held a task back with no other agent able to
	// take it (the 10-minute soft fallback).
	heldSince map[string]time.Time

	wakeMu sync.Mutex
	wakeCh chan struct{}
	// parked counts the long polls blocked waiting for a change (for tests and metrics).
	parked atomic.Int64
}

// New returns a scheduler. Call Run to start the lease controller and the tick.
func New(d Deps, opts Options) (*Scheduler, error) {
	switch {
	case d.Store == nil:
		return nil, errors.New("scheduler: no store")
	case d.Catalog == nil:
		return nil, errors.New("scheduler: no catalog")
	case d.Conns == nil:
		return nil, errors.New("scheduler: no provider connections")
	case d.Runs == nil:
		return nil, errors.New("scheduler: no run API")
	case d.Secrets == nil:
		return nil, errors.New("scheduler: no secret resolver")
	case d.Owners == nil:
		return nil, errors.New("scheduler: no owner verifier")
	}
	opts = opts.withDefaults()
	s := &Scheduler{
		d:          d,
		opts:       opts,
		clk:        opts.Clock,
		log:        opts.Logger.With("component", "scheduler"),
		groups:     newGroupCache(d.Conns, opts.Clock, opts.GroupCacheTTL),
		waiting:    map[string]int{},
		lastLeased: map[string]time.Time{},
		heldSince:  map[string]time.Time{},
		wakeCh:     make(chan struct{}),
	}
	s.ctrl = controllers.NewController("scheduler", controllers.ReconcilerFunc(s.Reconcile),
		controllers.Options{Workers: opts.Workers, Clock: opts.Clock, Logger: opts.Logger})
	// Tasks: a task that left its lease is revoked, an expired one expired; any task change
	// may make a parked poll match.
	if err := s.ctrl.Watch(d.Store, v1alpha1.KindTask, func(obj store.Object) []string {
		s.wake()
		return []string{obj.GetName()}
	}, controllers.InformerOptions{}); err != nil {
		return nil, err
	}
	if err := s.ctrl.Watch(d.Store, KindLease, func(obj store.Object) []string {
		if l, ok := obj.(*Lease); ok {
			s.wake() // capacity changed
			return []string{l.Spec.Task}
		}
		return nil
	}, controllers.InformerOptions{}); err != nil {
		return nil, err
	}
	if err := s.ctrl.Watch(d.Store, v1alpha1.KindAgent, func(store.Object) []string {
		s.wake()
		return nil
	}, controllers.InformerOptions{}); err != nil {
		return nil, err
	}
	return s, nil
}

// Controller returns the lease controller (keyed by task id).
func (s *Scheduler) Controller() *controllers.Controller { return s.ctrl }

// Run runs the lease controller and the matching tick until ctx is done.
func (s *Scheduler) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Go(func() { s.tickLoop(ctx) })
	err := s.ctrl.Run(ctx)
	wg.Wait()
	return err
}

func (s *Scheduler) tickLoop(ctx context.Context) {
	for {
		t := s.clk.NewTimer(s.opts.Tick)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
		s.wake()
		if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, "refresh failed", "err", err)
		}
	}
}

// wake tells every parked poll to try again.
func (s *Scheduler) wake() {
	s.wakeMu.Lock()
	close(s.wakeCh)
	s.wakeCh = make(chan struct{})
	s.wakeMu.Unlock()
}

// changed returns the channel the next wake closes.
func (s *Scheduler) changed() <-chan struct{} {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	return s.wakeCh
}

// randomBytes returns n random bytes from Options.Rand.
func (s *Scheduler) randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(s.opts.Rand, b); err != nil {
		return nil, fmt.Errorf("random: %w", err)
	}
	return b, nil
}

// newLeaseID returns a lease id: "lse_" and 16 random bytes in hex.
func (s *Scheduler) newLeaseID() (string, error) {
	b, err := s.randomBytes(16)
	if err != nil {
		return "", err
	}
	return "lse_" + hex.EncodeToString(b), nil
}

// ---- Agents -----------------------------------------------------------------------------------

func agentKey(id string) store.Key { return store.Key{Kind: v1alpha1.KindAgent, Name: id} }

func newAgent() *v1alpha1.Agent { return &v1alpha1.Agent{} }

func (s *Scheduler) getAgent(ctx context.Context, id string) (*v1alpha1.Agent, error) {
	var a v1alpha1.Agent
	err := s.d.Store.Get(ctx, agentKey(id), &a)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("agent %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get agent %s: %w", id, err)
	}
	ensureAgentStatus(&a)
	return &a, nil
}

func ensureAgentStatus(a *v1alpha1.Agent) *v1alpha1.AgentStatus {
	if a.Status == nil {
		a.Status = &v1alpha1.AgentStatus{Phase: v1alpha1.AgentReady}
	}
	return a.Status
}

// updateAgent applies mutate to the stored agent with compare-and-swap.
func (s *Scheduler) updateAgent(ctx context.Context, id string, mutate func(*v1alpha1.Agent) (bool, error)) (*v1alpha1.Agent, error) {
	a, err := update(ctx, s.d.Store, agentKey(id), newAgent, func(a *v1alpha1.Agent) (bool, error) {
		ensureAgentStatus(a)
		return mutate(a)
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("agent %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("update agent %s: %w", id, err)
	}
	return a, nil
}

// capacity is how many tasks an agent may run at once: lending.maxConcurrent and the pool's
// maxTasksPerAgent, the smaller of those set; 1 when neither is.
func capacity(a *v1alpha1.Agent, pool *v1alpha1.Pool) int {
	c := 0
	if l := a.Spec.Lending; l != nil && l.MaxConcurrent > 0 {
		c = l.MaxConcurrent
	}
	if pool != nil && pool.Spec.Lease != nil && pool.Spec.Lease.MaxTasksPerAgent > 0 {
		if m := pool.Spec.Lease.MaxTasksPerAgent; c == 0 || m < c {
			c = m
		}
	}
	return max(c, 1)
}

// settlePhase moves an agent between Ready and Busy by its running tasks, and back from
// Throttled once its until has passed. Draining and Offline are left alone. It reports
// whether anything changed.
func settlePhase(a *v1alpha1.Agent, pool *v1alpha1.Pool, now time.Time) bool {
	st := ensureAgentStatus(a)
	before, beforeUntil := st.Phase, st.ThrottledUntil
	switch st.Phase {
	case v1alpha1.AgentThrottled:
		if st.ThrottledUntil != nil && now.Before(*st.ThrottledUntil) {
			return false
		}
		st.ThrottledUntil = nil
		st.Phase = v1alpha1.AgentReady
	case v1alpha1.AgentReady, v1alpha1.AgentBusy, "":
	default:
		return false
	}
	if len(st.Running) >= capacity(a, pool) {
		st.Phase = v1alpha1.AgentBusy
	} else {
		st.Phase = v1alpha1.AgentReady
	}
	return st.Phase != before || st.ThrottledUntil != beforeUntil
}

// throttled reports whether an agent is throttled at now (a Throttled agent whose until has
// passed is not).
func throttled(a *v1alpha1.Agent, now time.Time) bool {
	st := a.Status
	if st == nil || st.Phase != v1alpha1.AgentThrottled {
		return false
	}
	return st.ThrottledUntil == nil || now.Before(*st.ThrottledUntil)
}

// serving reports whether an agent takes new tasks at all (Ready or Busy, or Throttled past
// its until).
func serving(a *v1alpha1.Agent, now time.Time) bool {
	if a.Status == nil {
		return true
	}
	switch a.Status.Phase {
	case v1alpha1.AgentReady, v1alpha1.AgentBusy, "":
		return true
	case v1alpha1.AgentThrottled:
		return !throttled(a, now)
	}
	return false
}

// leaseTimes returns a pool's lease TTL and heartbeat interval.
func leaseTimes(pool *v1alpha1.Pool) (ttl, heartbeat time.Duration) {
	ttl, heartbeat = DefaultLeaseTTL, DefaultHeartbeat
	if pool == nil || pool.Spec.Lease == nil {
		return ttl, heartbeat
	}
	if d, err := pool.Spec.Lease.TTL.Parse(); err == nil && d > 0 {
		ttl = d
	}
	if d, err := pool.Spec.Lease.Heartbeat.Parse(); err == nil && d > 0 {
		heartbeat = d
	}
	return ttl, heartbeat
}

// ---- Tasks ------------------------------------------------------------------------------------

func taskKey(id string) store.Key { return store.Key{Kind: v1alpha1.KindTask, Name: id} }

func newTask() *v1alpha1.Task { return &v1alpha1.Task{} }

func ensureTaskStatus(t *v1alpha1.Task) *v1alpha1.TaskStatus {
	if t.Status == nil {
		t.Status = &v1alpha1.TaskStatus{Phase: v1alpha1.TaskPending}
	}
	return t.Status
}

// active reports whether a task holds a lease: Leased, Running or Cancelling with an agent.
func active(t *v1alpha1.Task) bool {
	if t.Status == nil || t.Status.Agent == nil {
		return false
	}
	switch t.Status.Phase {
	case v1alpha1.TaskLeased, v1alpha1.TaskRunning, v1alpha1.TaskCancelling:
		return true
	}
	return false
}

// heldBy reports whether a task's lease is agent's.
func heldBy(t *v1alpha1.Task, agent string) bool {
	return active(t) && *t.Status.Agent == agent
}

func ruleSetOf(t *v1alpha1.Task) string {
	if t.Metadata.Provenance == nil {
		return ""
	}
	return t.Metadata.Provenance.RuleSet
}
