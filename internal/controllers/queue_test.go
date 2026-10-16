package controllers

import (
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/clock"
)

func TestBackoffDelay(t *testing.T) {
	half := func() float64 { return 0.5 }
	tests := []struct {
		name     string
		b        Backoff
		failures int
		want     time.Duration
	}{
		{"defaults first", Backoff{Jitter: 0}, 1, 5 * time.Millisecond},
		{"zero failures is the first", Backoff{}, 0, 5 * time.Millisecond},
		{"doubles", Backoff{Base: 5 * time.Millisecond}, 2, 10 * time.Millisecond},
		{"doubles again", Backoff{Base: 5 * time.Millisecond}, 3, 20 * time.Millisecond},
		{"tenth", Backoff{Base: 5 * time.Millisecond}, 10, 2560 * time.Millisecond},
		{"capped at the default max", Backoff{}, 17, 5 * time.Minute},
		{"capped, huge exponent", Backoff{}, 10_000, 5 * time.Minute},
		{"custom cap", Backoff{Base: time.Second, Max: 3 * time.Second}, 5, 3 * time.Second},
		{"jitter adds a fraction", Backoff{Base: 100 * time.Millisecond, Jitter: 0.1, Rand: half}, 1, 105 * time.Millisecond},
		{"jitter never passes the cap", Backoff{Base: time.Second, Max: time.Second, Jitter: 0.5, Rand: half}, 1, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.Delay(tt.failures); got != tt.want {
				t.Errorf("Delay(%d) = %v, want %v", tt.failures, got, tt.want)
			}
		})
	}
}

func TestQueueDedupeAndRequeueAfterDone(t *testing.T) {
	q := NewQueue(clock.NewFake(t0), Backoff{})
	defer q.ShutDown()

	q.Add("a")
	q.Add("a")
	q.Add("b")
	if n := q.Len(); n != 2 {
		t.Fatalf("Len = %d after adding a, a, b; want 2 (deduped)", n)
	}
	if k := getKey(t, q); k != "a" {
		t.Fatalf("Get = %q, want a (FIFO)", k)
	}
	// a is being processed: adding it again must not hand it to another worker.
	q.Add("a")
	q.Add("a")
	if n := q.Len(); n != 1 {
		t.Fatalf("Len = %d while a is processing; want 1 (b only)", n)
	}
	if k := getKey(t, q); k != "b" {
		t.Fatalf("Get = %q, want b", k)
	}
	if n := q.Len(); n != 0 {
		t.Fatalf("Len = %d; want 0 until a is done", n)
	}
	q.Done("a")
	if n := q.Len(); n != 1 {
		t.Fatalf("Len = %d after Done(a); want 1 (a requeued once)", n)
	}
	if k := getKey(t, q); k != "a" {
		t.Fatalf("Get = %q, want a", k)
	}
	q.Done("a")
	q.Done("b")
	if n := q.Len(); n != 0 {
		t.Fatalf("Len = %d after Done without re-adds; want 0", n)
	}
}

func TestQueueRateLimitedBackoff(t *testing.T) {
	clk := clock.NewFake(t0)
	q := NewQueue(clk, Backoff{Base: 5 * time.Millisecond, Max: 20 * time.Millisecond})
	defer q.ShutDown()

	for i, want := range []time.Duration{5, 10, 20, 20} {
		want *= time.Millisecond
		q.AddRateLimited("a")
		if n := q.NumRequeues("a"); n != i+1 {
			t.Fatalf("NumRequeues = %d, want %d", n, i+1)
		}
		clk.BlockUntil(1)
		clk.Advance(want - time.Millisecond)
		if q.Len() != 0 || clk.Timers() != 1 {
			t.Fatalf("retry %d ready before %v (len %d, timers %d)", i+1, want, q.Len(), clk.Timers())
		}
		clk.Advance(time.Millisecond)
		if k := getKey(t, q); k != "a" {
			t.Fatalf("Get = %q, want a", k)
		}
		q.Done("a")
	}

	q.Forget("a")
	if n := q.NumRequeues("a"); n != 0 {
		t.Fatalf("NumRequeues = %d after Forget, want 0", n)
	}
	q.AddRateLimited("a")
	clk.BlockUntil(1)
	clk.Advance(5 * time.Millisecond)
	if k := getKey(t, q); k != "a" {
		t.Fatalf("Get = %q after Forget, want a at the base delay", k)
	}
}

func TestQueueAddAfterKeepsEarliest(t *testing.T) {
	clk := clock.NewFake(t0)
	q := NewQueue(clk, Backoff{})
	defer q.ShutDown()

	q.AddAfter("a", 10*time.Millisecond)
	q.AddAfter("a", 50*time.Millisecond) // later: ignored
	clk.BlockUntil(1)
	clk.Advance(10 * time.Millisecond)
	if k := getKey(t, q); k != "a" {
		t.Fatalf("Get = %q, want a", k)
	}
	if n := clk.Timers(); n != 0 {
		t.Fatalf("%d timers left; the later deadline should have been dropped", n)
	}
	q.Done("a")

	q.AddAfter("b", time.Hour)
	q.AddAfter("b", time.Minute) // earlier: replaces
	clk.BlockUntil(1)
	clk.Advance(time.Minute)
	if k := getKey(t, q); k != "b" {
		t.Fatalf("Get = %q, want b at the earlier deadline", k)
	}
	q.Done("b")

	q.AddAfter("c", 0) // no delay: at once
	if n := q.Len(); n != 1 {
		t.Fatalf("Len = %d after AddAfter(0), want 1", n)
	}
}

func TestQueueShutDown(t *testing.T) {
	clk := clock.NewFake(t0)
	q := NewQueue(clk, Backoff{})

	got := make(chan bool, 1)
	go func() {
		_, shutdown := q.Get()
		got <- shutdown
	}()
	q.AddAfter("later", time.Hour)
	q.ShutDown()
	if !recv(t, got, "a blocked Get to return") {
		t.Fatal("blocked Get returned a key, want shutdown")
	}
	q.ShutDown() // idempotent

	q.Add("a")
	q.AddAfter("b", time.Millisecond)
	q.AddRateLimited("c")
	if n := q.Len(); n != 0 {
		t.Fatalf("Len = %d after ShutDown, want adds ignored", n)
	}
	if _, shutdown := q.Get(); !shutdown {
		t.Fatal("Get after ShutDown returned a key")
	}
	if !q.ShuttingDown() {
		t.Fatal("ShuttingDown = false")
	}
}
