package controllers

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
)

// blockingConn is a provider whose Apply hangs until its context ends while block is set (a
// provider that stopped answering).
type blockingConn struct {
	provider.Conn
	block   atomic.Bool
	blocked atomic.Int32
}

func (b *blockingConn) Apply(ctx context.Context, e provider.EffectRequest) (provider.EffectResult, error) {
	if b.block.Load() {
		b.blocked.Add(1)
		<-ctx.Done()
		return provider.EffectResult{}, ctx.Err()
	}
	return b.Conn.Apply(ctx, e)
}

func TestLockedSectionEndsInsideTheLease(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	e.lease(plan.Metadata.ID)
	if _, err := e.run.Complete(e.ctx, plan.Metadata.ID, doneResult("decomposed", planData)); err != nil {
		t.Fatal(err)
	}
	done := e.events(v1alpha1.EventFactoryRunCompleted)[0]

	// A rule controller whose provider hangs on the epic-plan comment, with a short lease.
	const ttl = 400 * time.Millisecond
	bc := &blockingConn{Conn: e.gh}
	bc.block.Store(true)
	rule, err := NewRuleController(e.st, staticCatalog{e.cat}, StaticConns{ghProvider: bc},
		RuleOptions{Clock: e.clk, Logger: discardLogger(), LockTTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = rule.Reconcile(e.ctx, done.ID)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrLockDeadline) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reconcile = %v, want ErrLockDeadline", err)
	}
	if bc.blocked.Load() != 1 || elapsed >= ttl {
		t.Fatalf("blocked applies = %d, section took %s (lease %s)", bc.blocked.Load(), elapsed, ttl)
	}
	// The lock was released: it is free at once.
	lctx, cancel := context.WithTimeout(e.ctx, 50*time.Millisecond)
	defer cancel()
	u, err := e.st.Lock(lctx, "wi/"+wi.Metadata.Name, time.Minute)
	if err != nil {
		t.Fatalf("the work item lock is still held: %v", err)
	}
	u.Unlock()
	if ok, _ := rule.processed(e.ctx, done.ID); ok {
		t.Fatal("the event was marked processed")
	}

	// The provider answers again: the retry applies everything once.
	bc.block.Store(false)
	if _, err := rule.Reconcile(e.ctx, done.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n := len(e.tasksOf(wi.Metadata.Name, "implement")); n != 1 {
		t.Errorf("implement tasks = %d, want 1", n)
	}
	if n := len(e.applies(v1alpha1.OpComment)); n != 1 {
		t.Errorf("comments = %d, want 1", n)
	}
	if ok, _ := rule.processed(e.ctx, done.ID); !ok {
		t.Error("the event is not marked processed after the retry")
	}
}
