package controllers

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/store"
	"github.com/compliance-framework/factory/internal/store/memory"
)

// noJitter is a backoff with the default delays and no randomness.
var noJitter = Backoff{Base: DefaultBaseDelay, Max: DefaultMaxDelay}

func TestControllerRetriesWithBackoff(t *testing.T) {
	clk := clock.NewFake(t0)
	calls := make(chan int, 10)
	var n atomic.Int32
	c := NewController("test", ReconcilerFunc(func(context.Context, string) (Result, error) {
		i := int(n.Add(1))
		calls <- i
		if i <= 2 {
			return Result{}, errors.New("transient")
		}
		return Result{}, nil
	}), Options{Workers: 1, Clock: clk, Backoff: noJitter, Logger: discardLogger()})
	stop := runInBackground(t, c.Run)

	c.Enqueue("k")
	recv(t, calls, "the first reconcile")
	clk.BlockUntil(1)
	clk.Advance(5*time.Millisecond - 1)
	if clk.Timers() != 1 {
		t.Fatal("retried before the first backoff delay")
	}
	clk.Advance(1)
	recv(t, calls, "the first retry (5 ms)")
	clk.BlockUntil(1)
	clk.Advance(10 * time.Millisecond)
	recv(t, calls, "the second retry (10 ms)")

	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if r := c.Queue().NumRequeues("k"); r != 0 {
		t.Errorf("NumRequeues = %d after success, want 0 (forgotten)", r)
	}
	if got := n.Load(); got != 3 {
		t.Errorf("%d reconciles, want 3", got)
	}
}

func TestControllerRequeueAfter(t *testing.T) {
	clk := clock.NewFake(t0)
	calls := make(chan struct{}, 10)
	c := NewController("test", ReconcilerFunc(func(context.Context, string) (Result, error) {
		calls <- struct{}{}
		return Result{RequeueAfter: time.Minute}, nil
	}), Options{Clock: clk, Logger: discardLogger()})
	runInBackground(t, c.Run)

	c.Enqueue("k")
	recv(t, calls, "the first reconcile")
	clk.BlockUntil(1)
	clk.Advance(time.Minute)
	recv(t, calls, "the requeued reconcile")
}

func TestControllerRecoversPanics(t *testing.T) {
	clk := clock.NewFake(t0)
	calls := make(chan struct{}, 10)
	var n atomic.Int32
	c := NewController("test", ReconcilerFunc(func(context.Context, string) (Result, error) {
		calls <- struct{}{}
		if n.Add(1) == 1 {
			panic("boom")
		}
		return Result{}, nil
	}), Options{Workers: 1, Clock: clk, Backoff: noJitter, Logger: discardLogger()})
	runInBackground(t, c.Run)

	c.Enqueue("k")
	recv(t, calls, "the panicking reconcile")
	clk.BlockUntil(1) // retried with backoff, not dropped
	if p := c.Panics(); p != 1 {
		t.Fatalf("Panics = %d, want 1", p)
	}
	clk.Advance(5 * time.Millisecond)
	recv(t, calls, "the retry after the panic")
}

func TestControllerOneWorkerPerKey(t *testing.T) {
	started := make(chan int32, 10)
	bDone := make(chan struct{}, 10)
	release := make(chan struct{})
	var callsA, active atomic.Int32
	var overlap atomic.Bool
	c := NewController("test", ReconcilerFunc(func(_ context.Context, key string) (Result, error) {
		if key == "b" {
			bDone <- struct{}{}
			return Result{}, nil
		}
		if active.Add(1) > 1 {
			overlap.Store(true)
		}
		defer active.Add(-1)
		n := callsA.Add(1)
		started <- n
		if n == 1 {
			<-release
		}
		return Result{}, nil
	}), Options{Workers: 4, Clock: clock.NewFake(t0), Logger: discardLogger()})
	stop := runInBackground(t, c.Run)

	c.Enqueue("a")
	recv(t, started, "a to start")
	for range 10 {
		c.Enqueue("a")
	}
	c.Enqueue("b")
	recv(t, bDone, "b to run on another worker while a is in progress")
	close(release)
	if n := recv(t, started, "a's single requeue"); n != 2 {
		t.Fatalf("a's call #%d, want 2", n)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if overlap.Load() {
		t.Error("a was reconciled by two workers at once")
	}
	if n := callsA.Load(); n != 2 {
		t.Errorf("a reconciled %d times, want 2 (11 adds deduped to one requeue)", n)
	}
}

func TestControllerStopsCleanly(t *testing.T) {
	inFlight := make(chan struct{})
	c := NewController("test", ReconcilerFunc(func(ctx context.Context, _ string) (Result, error) {
		close(inFlight)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}), Options{Workers: 1, Clock: clock.NewFake(t0), Logger: discardLogger()})
	stop := runInBackground(t, c.Run)

	c.Enqueue("k")
	recv(t, inFlight, "the reconcile to start")
	if err := stop(); err != nil {
		t.Fatalf("Run = %v, want nil on cancellation", err)
	}
	if r := c.Queue().NumRequeues("k"); r != 0 {
		t.Errorf("NumRequeues = %d: a reconcile cancelled by shutdown was scheduled for retry", r)
	}
	if !c.Queue().ShuttingDown() {
		t.Error("queue still running after Run returned")
	}
	if err := c.Run(context.Background()); err == nil {
		t.Error("second Run succeeded")
	}
	if err := c.Watch(memory.New(store.Options{}), "Agent", nil, InformerOptions{}); err == nil {
		t.Error("Watch after Run succeeded")
	}
}
