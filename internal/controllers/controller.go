// Package controllers is the dispatcher's controller runtime (keyed work queues, rate-limited
// retries, store informers) and its controllers, starting with catalog-sync.
//
// Design: design/workstreams/dispatcher-runtime/LLD/controllers.md.
package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/store"
)

// Reconciler brings the world in line with the desired state of one key. Reconcile must be
// idempotent: it re-reads state and never trusts the event that queued the key (P4).
type Reconciler interface {
	Reconcile(ctx context.Context, key string) (Result, error)
}

// ReconcilerFunc adapts a function to Reconciler.
type ReconcilerFunc func(ctx context.Context, key string) (Result, error)

// Reconcile calls f.
func (f ReconcilerFunc) Reconcile(ctx context.Context, key string) (Result, error) {
	return f(ctx, key)
}

// Result says what to do with a key after a successful Reconcile.
type Result struct {
	// RequeueAfter, when positive, reconciles the key again after this delay (and resets its
	// backoff); zero forgets it until it is added again.
	RequeueAfter time.Duration
}

// DefaultWorkers is the default number of workers per controller.
const DefaultWorkers = 2

// Options configure a Controller. Zero fields take the defaults.
type Options struct {
	// Workers is the number of keys reconciled concurrently. Default 2.
	Workers int
	// Clock times retries, delayed requeues and informer retries. Default the wall clock.
	Clock clock.Clock
	// Backoff is the per-key retry backoff after an error. Default 5 ms → 5 min with 10%
	// jitter (a zero Backoff); set any field to configure it (Jitter 0 is then no jitter).
	Backoff Backoff
	// Logger is the controller's logger (it adds controller=<name>). Default slog.Default().
	Logger *slog.Logger
}

// Controller runs a Reconciler over a keyed work queue fed by store watches (Watch) and
// direct adds (Enqueue). A key is reconciled by at most one worker at a time; a failed
// Reconcile (an error or a panic) is retried with exponential backoff.
type Controller struct {
	name    string
	rec     Reconciler
	workers int
	clk     clock.Clock
	log     *slog.Logger
	queue   *Queue

	mu        sync.Mutex
	started   bool
	informers []*Informer

	panics atomic.Uint64
}

// NewController returns a controller named name (used in logs) that reconciles with rec.
// Its queue exists from the start, so keys can be enqueued before Run.
func NewController(name string, rec Reconciler, opts Options) *Controller {
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if b := opts.Backoff; b.Base == 0 && b.Max == 0 && b.Jitter == 0 && b.Rand == nil {
		opts.Backoff.Jitter = DefaultJitter
	}
	return &Controller{
		name:    name,
		rec:     rec,
		workers: opts.Workers,
		clk:     opts.Clock,
		log:     opts.Logger.With("controller", name),
		queue:   NewQueue(opts.Clock, opts.Backoff),
	}
}

// Name returns the controller's name.
func (c *Controller) Name() string { return c.name }

// Queue returns the controller's work queue.
func (c *Controller) Queue() *Queue { return c.queue }

// Enqueue adds key to the work queue.
func (c *Controller) Enqueue(key string) { c.queue.Add(key) }

// Panics returns how many Reconcile calls panicked (controller_panics_total).
func (c *Controller) Panics() uint64 { return c.panics.Load() }

// KeyFunc maps a changed object to the keys to reconcile (none to ignore it).
type KeyFunc func(store.Object) []string

// NameKey is the KeyFunc that reconciles the object itself, by name.
func NameKey(obj store.Object) []string { return []string{obj.GetName()} }

// Watch feeds the queue from kind's objects in s: every object on start and after each
// resync, then every change, mapped to keys by keyFn (NameKey when nil). The informer starts
// with Run and stops with it. Watch must be called before Run.
func (c *Controller) Watch(s store.Store, kind string, keyFn KeyFunc, opts InformerOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return fmt.Errorf("controller %s: Watch(%s) after Run", c.name, kind)
	}
	if keyFn == nil {
		keyFn = NameKey
	}
	if opts.Clock == nil {
		opts.Clock = c.clk
	}
	if opts.Logger == nil {
		opts.Logger = c.log
	}
	c.informers = append(c.informers, NewInformer(s, kind, func(obj store.Object) {
		for _, k := range keyFn(obj) {
			c.queue.Add(k)
		}
	}, opts))
	return nil
}

// Run starts the informers and the workers and blocks until ctx is done; then it shuts the
// queue down, waits for in-flight reconciles and informers to return, and returns nil. Run
// may be called once.
func (c *Controller) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return fmt.Errorf("controller %s: already started", c.name)
	}
	c.started = true
	informers := c.informers
	c.mu.Unlock()

	var wg sync.WaitGroup
	for _, inf := range informers {
		wg.Go(func() { inf.Run(ctx) })
	}
	for range c.workers {
		wg.Go(func() {
			for c.processNext(ctx) {
			}
		})
	}
	c.log.Debug("controller started", "workers", c.workers, "watches", len(informers))
	<-ctx.Done()
	c.queue.ShutDown()
	wg.Wait()
	c.log.Debug("controller stopped")
	return nil
}

// processNext reconciles one key; false once the queue is shut down.
func (c *Controller) processNext(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	res, err := c.reconcile(ctx, key)
	switch {
	case err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()):
		// Stopping: don't retry.
	case err != nil:
		c.log.Warn("reconcile failed; retrying", "key", key, "attempt", c.queue.NumRequeues(key)+1, "err", err)
		c.queue.AddRateLimited(key)
	case res.RequeueAfter > 0:
		c.queue.Forget(key)
		c.queue.AddAfter(key, res.RequeueAfter)
	default:
		c.queue.Forget(key)
	}
	return true
}

// reconcile calls the Reconciler, turning a panic into an error.
func (c *Controller) reconcile(ctx context.Context, key string) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			c.panics.Add(1)
			c.log.Error("reconcile panicked", "key", key, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = fmt.Errorf("reconcile %s: panic: %v", key, r)
		}
	}()
	return c.rec.Reconcile(ctx, key)
}
