package storetest

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
)

const goroutines = 50

var concurrencyTests = []test{
	{name: "OneWinnerPerVersion", run: testOneWinnerPerVersion},
	{name: "CASRetryCounter", run: testCASRetryCounter},
	{name: "ParallelReadersWritersWatchers", run: testParallel},
}

// testOneWinnerPerVersion: 50 goroutines update the same version of one object; exactly one
// wins and the rest get ErrConflict.
func testOneWinnerPerVersion(t *testing.T, h *harness) {
	k := store.Key{Kind: v1alpha1.KindTask, Name: "t1"}
	h.mustCreate(t, newTask("t1", "wi", "q", v1alpha1.TaskPending, nil))
	for round := range 3 {
		var base v1alpha1.Task
		if err := h.s.Get(h.ctx, k, &base); err != nil {
			t.Fatal(err)
		}
		var (
			wg        sync.WaitGroup
			mu        sync.Mutex
			wins      []string
			conflicts int
			other     []error
		)
		start := make(chan struct{})
		var ready sync.WaitGroup
		for i := range goroutines {
			wg.Add(1)
			ready.Add(1)
			go func() {
				defer wg.Done()
				var mine v1alpha1.Task
				err := h.s.Get(h.ctx, k, &mine)
				ready.Done()
				if err != nil {
					mu.Lock()
					other = append(other, err)
					mu.Unlock()
					return
				}
				if mine.Metadata.ResourceVersion != base.Metadata.ResourceVersion {
					mu.Lock()
					other = append(other, fmt.Errorf("read rv %s, want %s", mine.Metadata.ResourceVersion, base.Metadata.ResourceVersion))
					mu.Unlock()
					return
				}
				mine.Status.Reason = fmt.Sprintf("round %d goroutine %d", round, i)
				<-start
				err = h.s.Update(h.ctx, &mine)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					wins = append(wins, mine.Status.Reason)
				case errors.Is(err, store.ErrConflict):
					conflicts++
				default:
					other = append(other, err)
				}
			}()
		}
		ready.Wait() // every goroutine read the same version before any writes
		close(start)
		wg.Wait()
		if len(other) > 0 || len(wins) != 1 || conflicts != goroutines-1 {
			t.Fatalf("round %d: %d wins %v, %d conflicts, errors %v", round, len(wins), wins, conflicts, other)
		}
		var got v1alpha1.Task
		if err := h.s.Get(h.ctx, k, &got); err != nil {
			t.Fatal(err)
		}
		if got.Status.Reason != wins[0] {
			t.Fatalf("stored %q, winner %q", got.Status.Reason, wins[0])
		}
	}
}

// testCASRetryCounter: the scheduler's BudgetCounter pattern, get-increment-update retried on
// ErrConflict, loses no increment.
func testCASRetryCounter(t *testing.T, h *harness) {
	k := store.Key{Kind: v1alpha1.KindBudgetCounter, Name: "budget.global.all.2026-01-01"}
	h.mustCreate(t, newCounter(k.Name))
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				var c v1alpha1.BudgetCounter
				if err := h.s.Get(h.ctx, k, &c); err != nil {
					errs <- err
					return
				}
				c.Status.Runs++
				c.Status.Tokens += 10
				err := h.s.Update(h.ctx, &c)
				if err == nil {
					return
				}
				if !errors.Is(err, store.ErrConflict) {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var c v1alpha1.BudgetCounter
	if err := h.s.Get(h.ctx, k, &c); err != nil {
		t.Fatal(err)
	}
	if c.Status.Runs != goroutines || c.Status.Tokens != 10*goroutines {
		t.Fatalf("counter = %+v, want %d runs", c.Status, goroutines)
	}
}

// testParallel mixes every operation, for the race detector, and checks the watch saw every
// write in version order.
func testParallel(t *testing.T, h *harness) {
	w, err := h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{Buffer: 10 * goroutines})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	var wg sync.WaitGroup
	errs := make(chan error, 4*goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := newRun(fmt.Sprintf("r%02d", i), "t")
			r.Metadata.Labels = map[string]string{"n": fmt.Sprint(i % 3)}
			if err := h.s.Create(h.ctx, r); err != nil {
				errs <- err
				return
			}
			r.Spec.Attempt = 2
			if err := h.s.Update(h.ctx, r); err != nil {
				errs <- err
			}
			var list []*v1alpha1.Run
			if _, err := h.s.List(h.ctx, v1alpha1.KindRun, store.ListOptions{
				LabelSelector: &v1alpha1.LabelSelector{MatchLabels: map[string]string{"n": "1"}}, Limit: 5,
			}, &list); err != nil {
				errs <- err
			}
			u, err := h.s.Lock(h.ctx, "shared", time.Minute)
			if err != nil {
				errs <- err
				return
			}
			u.Unlock()
			if err := h.s.PutTranscript(h.ctx, r.Metadata.Name, []byte("t")); err != nil {
				errs <- err
			}
			if i%2 == 0 {
				if err := h.s.Delete(h.ctx, store.KeyOf(r)); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	want := goroutines*2 + goroutines/2
	var last uint64
	for range want {
		ev := next(t, w)
		if n := v(t, ev.Version); n <= last {
			t.Fatalf("version %d after %d", n, last)
		} else {
			last = n
		}
	}
	quiet(t, w)
	if got := h.names(t, v1alpha1.KindRun); len(got) != goroutines/2 {
		t.Fatalf("%d runs left, want %d", len(got), goroutines/2)
	}
}
