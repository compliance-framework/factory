package clock

import (
	"sync"
	"testing"
	"time"
)

func TestFake(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := NewFake(start)
	if got := f.Now(); !got.Equal(start) {
		t.Fatalf("Now = %v, want %v", got, start)
	}
	if got := f.Advance(time.Hour); !got.Equal(start.Add(time.Hour)) {
		t.Fatalf("Advance = %v", got)
	}
	f.Set(start)
	if got := f.Now(); !got.Equal(start) {
		t.Fatalf("after Set: %v", got)
	}

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Advance(time.Second)
			_ = f.Now()
		}()
	}
	wg.Wait()
	if got := f.Now(); !got.Equal(start.Add(10 * time.Second)) {
		t.Fatalf("after concurrent Advance: %v", got)
	}
}

func TestReal(t *testing.T) {
	var c Clock = Real{}
	if c.Now().IsZero() {
		t.Fatal("Real.Now is zero")
	}
}

func fired(tm Timer) bool {
	select {
	case <-tm.C():
		return true
	default:
		return false
	}
}

func TestFakeTimers(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := NewFake(start)

	late := f.NewTimer(time.Minute)
	early := f.NewTimerAt(start.Add(time.Second))
	stopped := f.NewTimer(time.Second)
	if n := f.Timers(); n != 3 {
		t.Fatalf("Timers = %d, want 3", n)
	}
	if !stopped.Stop() || stopped.Stop() {
		t.Fatal("Stop: want true once, then false")
	}
	if now := f.NewTimer(0); !fired(now) {
		t.Fatal("NewTimer(0) didn't fire at once")
	}
	if past := f.NewTimerAt(start.Add(-time.Hour)); !fired(past) {
		t.Fatal("NewTimerAt(past) didn't fire at once")
	}

	f.Advance(time.Second - 1)
	if fired(early) || fired(late) {
		t.Fatal("a timer fired before its deadline")
	}
	f.Advance(1)
	if !fired(early) || fired(late) {
		t.Fatal("want only the 1s timer fired at 1s")
	}
	if early.Stop() {
		t.Fatal("Stop after firing returned true")
	}
	f.Set(start.Add(time.Hour))
	if !fired(late) || fired(stopped) {
		t.Fatal("want the 1m timer fired by Set, the stopped one never")
	}
	if n := f.Timers(); n != 0 {
		t.Fatalf("Timers = %d after all fired, want 0", n)
	}
}

// TestFakeNewTimerAtIsAbsolute: a deadline computed before an Advance still fires at that
// deadline (NewTimer(deadline.Sub(now)) computed from a stale now would fire late).
func TestFakeNewTimerAtIsAbsolute(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := NewFake(start)
	deadline := f.Now().Add(time.Second)
	f.Advance(500 * time.Millisecond) // time passes between reading Now and arming the timer
	tm := f.NewTimerAt(deadline)
	f.Advance(500 * time.Millisecond)
	if !fired(tm) {
		t.Fatal("NewTimerAt fired late: the deadline moved with the Advance")
	}
}

func TestFakeBlockUntil(t *testing.T) {
	f := NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	done := make(chan struct{})
	go func() {
		f.BlockUntil(2)
		close(done)
	}()
	f.NewTimer(time.Second)
	f.NewTimer(time.Minute)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("BlockUntil(2) didn't return with two timers armed")
	}
}

func TestRealTimers(t *testing.T) {
	var c Clock = Real{}
	tm := c.NewTimerAt(c.Now().Add(-time.Second))
	select {
	case <-tm.C():
	case <-time.After(10 * time.Second):
		t.Fatal("Real.NewTimerAt(past) didn't fire")
	}
	if !c.NewTimer(time.Hour).Stop() {
		t.Fatal("Stop on a pending real timer returned false")
	}
}
