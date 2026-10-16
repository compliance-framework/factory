package controllers

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/store"
	"github.com/compliance-framework/factory/internal/store/memory"
)

func newAgent(name string) *v1alpha1.Agent {
	return &v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindAgent},
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.AgentSpec{Pool: "p"},
	}
}

func mustCreate(t *testing.T, s store.Store, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := s.Create(context.Background(), newAgent(n)); err != nil {
			t.Fatal(err)
		}
	}
}

// waitSeen receives names from seen until every one of want has arrived.
func waitSeen(t *testing.T, seen <-chan string, want ...string) {
	t.Helper()
	missing := map[string]bool{}
	for _, w := range want {
		missing[w] = true
	}
	for len(missing) > 0 {
		delete(missing, recv(t, seen, fmt.Sprintf("keys %v", want)))
	}
}

func TestControllerWatchListsThenWatches(t *testing.T) {
	ctx := context.Background()
	s := memory.New(store.Options{})
	mustCreate(t, s, "a", "b")

	seen := make(chan string, 100)
	c := NewController("test", ReconcilerFunc(func(_ context.Context, key string) (Result, error) {
		seen <- key
		return Result{}, nil
	}), Options{Clock: clock.NewFake(t0), Logger: discardLogger()})
	if err := c.Watch(s, v1alpha1.KindAgent, func(o store.Object) []string { return []string{"agent/" + o.GetName()} }, InformerOptions{}); err != nil {
		t.Fatal(err)
	}
	runInBackground(t, c.Run)

	waitSeen(t, seen, "agent/a", "agent/b") // the initial list
	mustCreate(t, s, "c")
	waitSeen(t, seen, "agent/c")
	var a v1alpha1.Agent
	if err := s.Get(ctx, store.Key{Kind: v1alpha1.KindAgent, Name: "a"}, &a); err != nil {
		t.Fatal(err)
	}
	a.Spec.Pool = "q"
	if err := s.Update(ctx, &a); err != nil {
		t.Fatal(err)
	}
	waitSeen(t, seen, "agent/a")
	if err := s.Delete(ctx, store.Key{Kind: v1alpha1.KindAgent, Name: "b"}); err != nil {
		t.Fatal(err)
	}
	waitSeen(t, seen, "agent/b")
}

// TestInformerResyncsOnWatchExpiry stalls the handler so the store expires the watch (buffer
// of one), then checks that the informer re-lists everything it missed and keeps watching.
func TestInformerResyncsOnWatchExpiry(t *testing.T) {
	s := memory.New(store.Options{})
	mustCreate(t, s, "a0")

	seen := make(chan string, 1000)
	gate := make(chan struct{})
	var gated atomic.Bool
	inf := NewInformer(s, v1alpha1.KindAgent, func(o store.Object) {
		seen <- o.GetName()
		if o.GetName() == "block" && gated.CompareAndSwap(false, true) {
			<-gate
		}
	}, InformerOptions{Buffer: 1, PageSize: 2, Clock: clock.NewFake(t0), Logger: discardLogger()})
	runInBackground(t, func(ctx context.Context) error { inf.Run(ctx); return nil })

	recv(t, inf.Synced(), "the first list")
	waitSeen(t, seen, "a0")
	mustCreate(t, s, "block")
	waitSeen(t, seen, "block") // the handler is now stuck
	missed := []string{"x1", "x2", "x3", "x4", "x5"}
	mustCreate(t, s, missed...) // overflows the buffer: the store expires the watch, never blocking
	if r := inf.Resyncs(); r != 0 {
		t.Fatalf("Resyncs = %d before the handler resumed", r)
	}
	close(gate)

	waitSeen(t, seen, missed...)
	if r := inf.Resyncs(); r != 1 {
		t.Errorf("Resyncs = %d, want 1", r)
	}
	mustCreate(t, s, "after")
	waitSeen(t, seen, "after") // watching again, from the re-list's version
}

// flakyStore fails its first List calls.
type flakyStore struct {
	store.Store
	failures atomic.Int32
}

func (f *flakyStore) List(ctx context.Context, kind string, opts store.ListOptions, out any) (store.ListMeta, error) {
	if f.failures.Add(-1) >= 0 {
		return store.ListMeta{}, errors.New("unavailable")
	}
	return f.Store.List(ctx, kind, opts, out)
}

func TestInformerRetriesFailedListWithBackoff(t *testing.T) {
	clk := clock.NewFake(t0)
	s := &flakyStore{Store: memory.New(store.Options{})}
	s.failures.Store(2)
	mustCreate(t, s, "a")
	seen := make(chan string, 10)
	inf := NewInformer(s, v1alpha1.KindAgent, func(o store.Object) { seen <- o.GetName() },
		InformerOptions{Clock: clk, Retry: Backoff{Base: 100 * time.Millisecond, Max: time.Second}, Logger: discardLogger()})
	runInBackground(t, func(ctx context.Context) error { inf.Run(ctx); return nil })

	clk.BlockUntil(1)
	clk.Advance(100 * time.Millisecond)
	clk.BlockUntil(1)
	clk.Advance(200*time.Millisecond - 1)
	select {
	case <-inf.Synced():
		t.Fatal("listed before the second backoff delay")
	default:
	}
	clk.Advance(1)
	recv(t, inf.Synced(), "the list after two failures")
	waitSeen(t, seen, "a")
}
