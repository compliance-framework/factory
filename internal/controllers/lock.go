package controllers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/compliance-framework/factory/internal/store"
)

// ErrLockDeadline is returned when a section under a work item lock ran out of its time: it
// is retried, by which time the lease is free or reclaimable.
var ErrLockDeadline = errors.New("work item lock section ran out of time")

// lockSectionShare is the part of the lock's lease a locked section may use: its deadline
// sits well inside the lease, so the lease can't expire under a section still running (and a
// second worker can't take the item concurrently).
const lockSectionShare = 3.0 / 4

// withItemLock runs fn holding the work item's lock ("wi/<id>") for ttl, under a context whose
// deadline is 3/4 of ttl. Waiting for the lock uses ctx; the deadline starts once it is held.
// A section that hits the deadline returns an error wrapping ErrLockDeadline (and
// context.DeadlineExceeded), so the reconcile is retried; the lock is released either way.
//
// The deadline is on the wall clock (the provider calls it bounds are); the lease is on the
// store's clock, which is the same clock outside tests.
func withItemLock(ctx context.Context, s store.Store, id string, ttl time.Duration, fn func(ctx context.Context) error) error {
	unlock, err := s.Lock(ctx, "wi/"+id, ttl)
	if err != nil {
		return fmt.Errorf("lock work item %s: %w", id, err)
	}
	defer unlock.Unlock()
	sctx, cancel := context.WithTimeout(ctx, time.Duration(float64(ttl)*lockSectionShare))
	defer cancel()
	err = fn(sctx)
	if err != nil && ctx.Err() == nil && errors.Is(sctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("work item %s: %w after %s: %w", id, ErrLockDeadline, time.Duration(float64(ttl)*lockSectionShare), err)
	}
	return err
}
