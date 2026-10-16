package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
)

// waitTimeout bounds every wait for something that should happen.
const waitTimeout = 5 * time.Second

var watchTests = []test{
	{name: "Ordering", run: testWatchOrdering},
	{name: "Selector", run: testWatchSelector},
	{name: "SinceVersion", run: testWatchSinceVersion},
	{name: "SinceVersionExpired", opts: func(o *store.Options) { o.History = 4 }, run: testWatchSinceVersionExpired},
	{name: "SlowConsumerResync", run: testWatchSlowConsumerResync},
	{name: "StopAndContext", run: testWatchStopAndContext},
}

// next receives the next event, failing if the watch ends or nothing comes.
func next(t *testing.T, w store.Watcher) store.WatchEvent {
	t.Helper()
	select {
	case ev, ok := <-w.Events():
		if !ok {
			t.Fatalf("watch ended: %v", w.Err())
		}
		return ev
	case <-time.After(waitTimeout):
		t.Fatal("no watch event")
	}
	return store.WatchEvent{}
}

// closed waits for the watch to end and returns Err, failing on an event.
func closed(t *testing.T, w store.Watcher) error {
	t.Helper()
	select {
	case ev, ok := <-w.Events():
		if ok {
			t.Fatalf("unexpected event %s %s", ev.Type, store.KeyOf(ev.Object))
		}
		return w.Err()
	case <-time.After(waitTimeout):
		t.Fatal("watch did not end")
	}
	return nil
}

// quiet checks that no event is pending.
func quiet(t *testing.T, w store.Watcher) {
	t.Helper()
	select {
	case ev, ok := <-w.Events():
		if ok {
			t.Fatalf("unexpected event %s %s", ev.Type, store.KeyOf(ev.Object))
		}
		t.Fatalf("watch ended: %v", w.Err())
	case <-time.After(20 * time.Millisecond):
	}
}

func expect(t *testing.T, ev store.WatchEvent, typ store.WatchEventType, name string) {
	t.Helper()
	if ev.Type != typ || ev.Object == nil || ev.Object.GetName() != name {
		t.Fatalf("event = %s %v, want %s %s", ev.Type, ev.Object, typ, name)
	}
	if ev.Version == "" {
		t.Fatal("event without a version")
	}
	if store.HasResourceVersion(ev.Object) && store.ResourceVersion(ev.Object) != ev.Version {
		t.Fatalf("event version %s, object rv %s", ev.Version, store.ResourceVersion(ev.Object))
	}
}

func testWatchOrdering(t *testing.T, h *harness) {
	w, err := h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	r := newRun("r1", "t1")
	h.mustCreate(t, r)
	h.mustCreate(t, newAgent("ignored-kind"))
	r.Spec.Attempt = 2
	h.mustUpdate(t, r)
	h.mustCreate(t, newRun("r2", "t1"))
	if err := h.s.Delete(h.ctx, store.KeyOf(r)); err != nil {
		t.Fatal(err)
	}

	var last uint64
	for _, want := range []struct {
		typ  store.WatchEventType
		name string
	}{{store.Added, "r1"}, {store.Modified, "r1"}, {store.Added, "r2"}, {store.Deleted, "r1"}} {
		ev := next(t, w)
		expect(t, ev, want.typ, want.name)
		if n := v(t, ev.Version); n <= last {
			t.Fatalf("version %d after %d", n, last)
		} else {
			last = n
		}
		if want.typ == store.Modified && ev.Object.(*v1alpha1.Run).Spec.Attempt != 2 {
			t.Fatalf("modified event carries %+v", ev.Object)
		}
		if want.typ == store.Deleted && ev.Object.(*v1alpha1.Run).Spec.Attempt != 2 {
			t.Fatalf("deleted event carries %+v, want the last state", ev.Object)
		}
	}
	quiet(t, w)
}

func testWatchSelector(t *testing.T, h *harness) {
	w, err := h.s.Watch(h.ctx, v1alpha1.KindWorkItem, store.WatchOptions{
		LabelSelector: &v1alpha1.LabelSelector{MatchLabels: map[string]string{"team": "core"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	in := newWorkItem("in", map[string]string{"team": "core"})
	out := newWorkItem("out", map[string]string{"team": "web"})
	h.mustCreate(t, in, out)
	expect(t, next(t, w), store.Added, "in")

	out.Metadata.Labels["team"] = "core" // starts matching
	h.mustUpdate(t, out)
	expect(t, next(t, w), store.Added, "out")

	in.Metadata.Labels["team"] = "web" // stops matching
	h.mustUpdate(t, in)
	expect(t, next(t, w), store.Deleted, "in")

	in.Metadata.Labels["x"] = "y" // still not matching
	h.mustUpdate(t, in)
	if err := h.s.Delete(h.ctx, store.KeyOf(in)); err != nil {
		t.Fatal(err)
	}
	quiet(t, w)
	if err := h.s.Delete(h.ctx, store.KeyOf(out)); err != nil {
		t.Fatal(err)
	}
	expect(t, next(t, w), store.Deleted, "out")
}

func testWatchSinceVersion(t *testing.T, h *harness) {
	r1 := newRun("r1", "t1")
	h.mustCreate(t, r1)
	since := r1.Metadata.ResourceVersion
	r1.Spec.Attempt = 2
	h.mustUpdate(t, r1)
	h.mustCreate(t, newAgent("a1"), newRun("r2", "t1"))

	w, err := h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{SinceVersion: since})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	expect(t, next(t, w), store.Modified, "r1")
	expect(t, next(t, w), store.Added, "r2")
	quiet(t, w)
	h.mustCreate(t, newRun("r3", "t1"))
	expect(t, next(t, w), store.Added, "r3")

	// From "0", the whole history.
	all, err := h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{SinceVersion: "0"})
	if err != nil {
		t.Fatal(err)
	}
	defer all.Stop()
	for _, n := range []string{"r1", "r1", "r2", "r3"} {
		if ev := next(t, all); ev.Object.GetName() != n {
			t.Fatalf("replay: %s %s, want %s", ev.Type, ev.Object.GetName(), n)
		}
	}

	// From the current version: nothing to replay.
	var runs []v1alpha1.Run
	meta, err := h.s.List(h.ctx, v1alpha1.KindRun, store.ListOptions{}, &runs)
	if err != nil {
		t.Fatal(err)
	}
	now, err := h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{SinceVersion: meta.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer now.Stop()
	quiet(t, now)

	_, err = h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{SinceVersion: "x"})
	wantErr(t, err, store.ErrInvalid)
	_, err = h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{SinceVersion: "999999"})
	wantErr(t, err, store.ErrInvalid)
	_, err = h.s.Watch(h.ctx, "", store.WatchOptions{})
	wantErr(t, err, store.ErrInvalid)
}

func testWatchSinceVersionExpired(t *testing.T, h *harness) {
	// History is 4 changes: version 1 drops out after 5 writes.
	r := newRun("r1", "t1")
	h.mustCreate(t, r)
	first := r.Metadata.ResourceVersion
	for i := range 5 {
		r.Spec.Attempt = i + 2
		h.mustUpdate(t, r)
	}
	_, err := h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{SinceVersion: first})
	wantErr(t, err, store.ErrWatchExpired)
	_, err = h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{SinceVersion: "0"})
	wantErr(t, err, store.ErrWatchExpired)

	// A recent enough version still replays.
	w, err := h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{SinceVersion: store.FormatVersion(v(t, r.Metadata.ResourceVersion) - 2)})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	expect(t, next(t, w), store.Modified, "r1")
	expect(t, next(t, w), store.Modified, "r1")
	quiet(t, w)
}

func testWatchSlowConsumerResync(t *testing.T, h *harness) {
	slow, err := h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{Buffer: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Stop()
	fast, err := h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{Buffer: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Stop()

	// The writer never blocks on the slow watcher.
	done := make(chan error, 1)
	go func() {
		for i := range 10 {
			if err := h.s.Create(h.ctx, newRun(string(rune('a'+i)), "t")); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("writer blocked on a slow watcher")
	}

	// The slow watcher gets what fit in its buffer, then its channel closes with
	// ErrWatchExpired.
	n := 0
	for range slow.Events() {
		n++
	}
	if n > 2 {
		t.Fatalf("slow watcher got %d events through a buffer of 2", n)
	}
	wantErr(t, slow.Err(), store.ErrWatchExpired)

	// The fast one is unaffected.
	for range 10 {
		next(t, fast)
	}

	// Resync: re-list, then watch from the list's version.
	var runs []v1alpha1.Run
	meta, err := h.s.List(h.ctx, v1alpha1.KindRun, store.ListOptions{}, &runs)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 10 {
		t.Fatalf("re-list got %d runs", len(runs))
	}
	again, err := h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{SinceVersion: meta.ResourceVersion, Buffer: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Stop()
	h.mustCreate(t, newRun("z", "t"))
	expect(t, next(t, again), store.Added, "z")
}

func testWatchStopAndContext(t *testing.T, h *harness) {
	w, err := h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w.Stop()
	w.Stop() // idempotent
	if err := closed(t, w); err != nil {
		t.Fatalf("Err after Stop = %v, want nil", err)
	}
	h.mustCreate(t, newRun("after-stop", "t")) // no send on a stopped watcher

	ctx, cancel := context.WithCancel(h.ctx)
	w, err = h.s.Watch(ctx, v1alpha1.KindRun, store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	wantErr(t, closed(t, w), context.Canceled)
	w.Stop() // after the context ended: still fine
	h.mustCreate(t, newRun("after-cancel", "t"))
}
