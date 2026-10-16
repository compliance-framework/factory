package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/store"
)

// blockedFor is how long a Lock that must wait is given before the test calls it blocked.
const blockedFor = 50 * time.Millisecond

var lockTests = []test{
	{name: "Contention", run: testLockContention},
	{name: "Expiry", run: testLockExpiry},
	{name: "Renewal", run: testLockRenewal},
	{name: "Invalid", run: testLockInvalid},
}

// mustLock takes name or fails.
func (h *harness) mustLock(t *testing.T, name string, ttl time.Duration) store.Unlock {
	t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, waitTimeout)
	defer cancel()
	u, err := h.s.Lock(ctx, name, ttl)
	if err != nil {
		t.Fatalf("Lock %s: %v", name, err)
	}
	return u
}

// blocked checks that name can't be taken now.
func (h *harness) blocked(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, blockedFor)
	defer cancel()
	u, err := h.s.Lock(ctx, name, time.Minute)
	if err == nil {
		u.Unlock()
		t.Fatalf("Lock %s succeeded, want it held", name)
	}
	wantErr(t, err, context.DeadlineExceeded)
}

func testLockContention(t *testing.T, h *harness) {
	a := h.mustLock(t, "wi/1", time.Minute)
	h.blocked(t, "wi/1")
	other := h.mustLock(t, "wi/2", time.Minute) // other names are independent
	other.Unlock()

	// A waiter gets the lock once it is released.
	got := make(chan store.Unlock, 1)
	errs := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(h.ctx, waitTimeout)
		defer cancel()
		u, err := h.s.Lock(ctx, "wi/1", time.Minute)
		if err != nil {
			errs <- err
			return
		}
		got <- u
	}()
	select {
	case <-got:
		t.Fatal("waiter got a held lock")
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(blockedFor):
	}
	a.Unlock()
	var b store.Unlock
	select {
	case b = <-got:
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(waitTimeout):
		t.Fatal("waiter not woken by Unlock")
	}

	// Unlock is idempotent and never releases someone else's lock.
	a.Unlock()
	h.blocked(t, "wi/1")
	b.Unlock()
	b.Unlock()
	h.mustLock(t, "wi/1", time.Minute).Unlock()
}

func testLockExpiry(t *testing.T, h *harness) {
	a := h.mustLock(t, "wi/1", time.Minute)
	h.clock.Advance(59 * time.Second)
	h.blocked(t, "wi/1")

	// A waiter takes the lock once the clock passes the lease, without any Unlock.
	got := make(chan store.Unlock, 1)
	go func() {
		ctx, cancel := context.WithTimeout(h.ctx, waitTimeout)
		defer cancel()
		if u, err := h.s.Lock(ctx, "wi/1", time.Minute); err == nil {
			got <- u
		}
		close(got)
	}()
	h.clock.Advance(time.Second)
	b, ok := <-got
	if !ok {
		t.Fatal("expired lock not reclaimed")
	}

	// The old holder lost it: Renew says so and Unlock leaves the new holder alone.
	wantErr(t, a.Renew(h.ctx, time.Minute), store.ErrLockLost)
	a.Unlock()
	h.blocked(t, "wi/1")
	b.Unlock()
}

func testLockRenewal(t *testing.T, h *harness) {
	a := h.mustLock(t, "lease/1", time.Minute)
	h.clock.Advance(40 * time.Second)
	if err := a.Renew(h.ctx, time.Minute); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	h.clock.Advance(40 * time.Second) // 80 s after Lock, 40 s after Renew
	h.blocked(t, "lease/1")
	if err := a.Renew(h.ctx, time.Minute); err != nil {
		t.Fatalf("second Renew: %v", err)
	}

	// Past the renewed lease, Renew fails even though nobody took the lock.
	h.clock.Advance(time.Minute)
	wantErr(t, a.Renew(h.ctx, time.Minute), store.ErrLockLost)
	h.mustLock(t, "lease/1", time.Minute).Unlock()

	// Renew after Unlock fails too.
	c := h.mustLock(t, "lease/2", time.Minute)
	c.Unlock()
	wantErr(t, c.Renew(h.ctx, time.Minute), store.ErrLockLost)
}

func testLockInvalid(t *testing.T, h *harness) {
	_, err := h.s.Lock(h.ctx, "", time.Minute)
	wantErr(t, err, store.ErrInvalid)
	_, err = h.s.Lock(h.ctx, "x", 0)
	wantErr(t, err, store.ErrInvalid)
	u := h.mustLock(t, "x", time.Minute)
	wantErr(t, u.Renew(h.ctx, -time.Second), store.ErrInvalid)
	u.Unlock()
}
