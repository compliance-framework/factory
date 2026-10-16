// Package clock is the injectable time source for logic packages: they take a Clock instead
// of calling time.Now or time.NewTimer, and tests drive a Fake.
//
// rules.Clock has the same Now() time.Time method, so a clock.Clock satisfies it; the two
// should converge on this package.
package clock

import (
	"sort"
	"sync"
	"time"
)

// Clock reads the time and arms timers.
type Clock interface {
	Now() time.Time
	// NewTimer returns a timer that fires once, d from now (at once when d <= 0).
	NewTimer(d time.Duration) Timer
	// NewTimerAt returns a timer that fires once the clock reaches deadline (at once if it
	// already has). Code that computed a deadline from an earlier Now uses it, so the time
	// that passed since isn't added to the wait.
	NewTimerAt(deadline time.Time) Timer
}

// Timer is a one-shot timer from a Clock.
type Timer interface {
	// C receives the time once the timer fires.
	C() <-chan time.Time
	// Stop prevents the timer from firing; false when it already fired or was stopped.
	Stop() bool
}

// Real is the wall clock.
type Real struct{}

var _ Clock = Real{}

// Now returns time.Now().
func (Real) Now() time.Time { return time.Now() }

// NewTimer wraps time.NewTimer.
func (Real) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

// NewTimerAt is NewTimer(time.Until(deadline)).
func (Real) NewTimerAt(deadline time.Time) Timer {
	return realTimer{time.NewTimer(time.Until(deadline))}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop() bool          { return r.t.Stop() }

// Fake is a clock that only moves when told to. Its timers fire during Advance or Set, in
// deadline order. It is safe for concurrent use.
type Fake struct {
	mu     sync.Mutex
	cond   *sync.Cond // created lazily: use condLocked
	now    time.Time
	timers []*fakeTimer
}

var _ Clock = (*Fake)(nil)

// NewFake returns a Fake set to t.
func NewFake(t time.Time) *Fake { return &Fake{now: t} }

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Set moves the clock to t and fires the timers that are due.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setLocked(t)
}

// Advance moves the clock forward by d, fires the timers that are due, and returns the new
// time.
func (f *Fake) Advance(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setLocked(f.now.Add(d))
	return f.now
}

// NewTimer returns a timer that fires once the clock reaches now+d (at once when d <= 0).
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newTimerLocked(f.now.Add(d))
}

// NewTimerAt returns a timer that fires once the clock reaches deadline.
func (f *Fake) NewTimerAt(deadline time.Time) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newTimerLocked(deadline)
}

// Timers returns the number of timers waiting to fire.
func (f *Fake) Timers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// BlockUntil waits until at least n timers are waiting to fire, so a test can Advance only
// once the code under test is waiting on the clock.
func (f *Fake) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.timers) < n {
		f.condLocked().Wait()
	}
}

func (f *Fake) condLocked() *sync.Cond {
	if f.cond == nil {
		f.cond = sync.NewCond(&f.mu)
	}
	return f.cond
}

func (f *Fake) newTimerLocked(at time.Time) Timer {
	t := &fakeTimer{f: f, at: at, ch: make(chan time.Time, 1)}
	if !at.After(f.now) {
		t.ch <- f.now
		return t
	}
	f.timers = append(f.timers, t)
	f.condLocked().Broadcast()
	return t
}

func (f *Fake) setLocked(t time.Time) {
	f.now = t
	var due, keep []*fakeTimer
	for _, tm := range f.timers {
		if tm.at.After(t) {
			keep = append(keep, tm)
		} else {
			due = append(due, tm)
		}
	}
	if len(due) == 0 {
		return
	}
	f.timers = keep
	sort.SliceStable(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
	for _, tm := range due {
		tm.ch <- t // buffered 1 and sent once: never blocks
	}
	f.condLocked().Broadcast()
}

type fakeTimer struct {
	f  *Fake
	at time.Time
	ch chan time.Time
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	for i, tm := range t.f.timers {
		if tm == t {
			t.f.timers = append(t.f.timers[:i], t.f.timers[i+1:]...)
			t.f.condLocked().Broadcast()
			return true
		}
	}
	return false
}
