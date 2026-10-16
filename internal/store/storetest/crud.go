package storetest

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
)

var crudTests = []test{
	{name: "CreateGet", run: testCreateGet},
	{name: "CreateExisting", run: testCreateExisting},
	{name: "UpdateCAS", run: testUpdateCAS},
	{name: "UpdateKeepsCreatedAt", run: testUpdateKeepsCreatedAt},
	{name: "UpdateMissing", run: testUpdateMissing},
	{name: "UnversionedKind", run: testUnversionedKind},
	{name: "Delete", run: testDelete},
	{name: "Invalid", run: testInvalid},
	{name: "DeepCopy", run: testDeepCopy},
	{name: "Context", run: testContext},
}

func testCreateGet(t *testing.T, h *harness) {
	in := newTask("t1", "wi-1", "triage", v1alpha1.TaskPending, map[string]string{"a": "1"})
	h.mustCreate(t, in)
	if in.Metadata.ResourceVersion == "" {
		t.Fatal("Create did not set resourceVersion on the object")
	}
	if !in.Metadata.CreatedAt.Equal(Epoch) {
		t.Fatalf("CreatedAt = %v, want the clock's %v", in.Metadata.CreatedAt, Epoch)
	}

	var got v1alpha1.Task
	if err := h.s.Get(h.ctx, store.Key{Kind: v1alpha1.KindTask, Name: "t1"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata.ResourceVersion != in.Metadata.ResourceVersion || !got.Metadata.CreatedAt.Equal(Epoch) {
		t.Fatalf("Get metadata = %+v, want rv %s createdAt %v", got.Metadata, in.Metadata.ResourceVersion, Epoch)
	}
	if got.Metadata.WorkItem != "wi-1" || got.Spec.Queue != "triage" || got.Status.Phase != v1alpha1.TaskPending ||
		got.Metadata.Labels["a"] != "1" || got.Kind != v1alpha1.KindTask {
		t.Fatalf("Get = %+v", got)
	}

	// Versions are monotonic across kinds.
	r := newRun("r1", "t1")
	h.mustCreate(t, r)
	if v(t, r.Metadata.ResourceVersion) <= v(t, in.Metadata.ResourceVersion) {
		t.Fatalf("rv %s after %s", r.Metadata.ResourceVersion, in.Metadata.ResourceVersion)
	}

	// Get into a reused object replaces it entirely.
	stale := v1alpha1.Task{Spec: v1alpha1.TaskRunSpec{Pool: "stale"}}
	if err := h.s.Get(h.ctx, store.Key{Kind: v1alpha1.KindTask, Name: "t1"}, &stale); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stale.Spec.Pool != "" {
		t.Fatalf("Get kept a stale field: %+v", stale.Spec)
	}

	wantErr(t, h.s.Get(h.ctx, store.Key{Kind: v1alpha1.KindTask, Name: "nope"}, &got), store.ErrNotFound)
	wantErr(t, h.s.Get(h.ctx, store.Key{Kind: v1alpha1.KindRun, Name: "t1"}, &got), store.ErrNotFound)
}

func testCreateExisting(t *testing.T, h *harness) {
	h.mustCreate(t, newRun("r1", "t1"))
	wantErr(t, h.s.Create(h.ctx, newRun("r1", "t2")), store.ErrAlreadyExists)
	// Same name, other kind: fine.
	h.mustCreate(t, newAgent("r1"))
}

func testUpdateCAS(t *testing.T, h *harness) {
	k := store.Key{Kind: v1alpha1.KindTask, Name: "t1"}
	h.mustCreate(t, newTask("t1", "wi-1", "triage", v1alpha1.TaskPending, nil))

	var a, b v1alpha1.Task
	if err := h.s.Get(h.ctx, k, &a); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Get(h.ctx, k, &b); err != nil {
		t.Fatal(err)
	}
	before := a.Metadata.ResourceVersion
	a.Status.Phase = v1alpha1.TaskLeased
	h.mustUpdate(t, &a)
	if v(t, a.Metadata.ResourceVersion) <= v(t, before) {
		t.Fatalf("Update rv %s, was %s", a.Metadata.ResourceVersion, before)
	}

	b.Status.Phase = v1alpha1.TaskCancelled
	err := h.s.Update(h.ctx, &b)
	wantErr(t, err, store.ErrConflict)
	if b.Metadata.ResourceVersion != before {
		t.Fatalf("failed Update changed the object's rv to %s", b.Metadata.ResourceVersion)
	}

	b.Metadata.ResourceVersion = ""
	wantErr(t, h.s.Update(h.ctx, &b), store.ErrConflict)

	var got v1alpha1.Task
	if err := h.s.Get(h.ctx, k, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.TaskLeased || got.Metadata.ResourceVersion != a.Metadata.ResourceVersion {
		t.Fatalf("stored = %+v / %+v", got.Metadata, got.Status)
	}
}

func testUpdateKeepsCreatedAt(t *testing.T, h *harness) {
	r := newRun("r1", "t1")
	h.mustCreate(t, r)
	h.clock.Advance(time.Hour)
	r.Metadata.CreatedAt = Epoch.Add(-24 * time.Hour)
	r.Spec.Attempt = 2
	h.mustUpdate(t, r)
	if !r.Metadata.CreatedAt.Equal(Epoch) {
		t.Fatalf("Update set CreatedAt %v, want the original %v", r.Metadata.CreatedAt, Epoch)
	}
	var got v1alpha1.Run
	if err := h.s.Get(h.ctx, store.KeyOf(r), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Metadata.CreatedAt.Equal(Epoch) || got.Spec.Attempt != 2 {
		t.Fatalf("stored = %+v", got)
	}
}

func testUpdateMissing(t *testing.T, h *harness) {
	wantErr(t, h.s.Update(h.ctx, newRun("r1", "t1")), store.ErrNotFound)
}

func testUnversionedKind(t *testing.T, h *harness) {
	// Event has no metadata: the store keeps its version beside it and updates it
	// unconditionally.
	e := newEvent("d1/0")
	h.mustCreate(t, e)
	e.Subject = "issue 1"
	h.mustUpdate(t, e)
	var got v1alpha1.Event
	if err := h.s.Get(h.ctx, store.KeyOf(e), &got); err != nil {
		t.Fatal(err)
	}
	if got.Subject != "issue 1" || got.Source != "github-public" || !got.Time.Equal(Epoch) {
		t.Fatalf("Get = %+v", got)
	}
	if store.HasResourceVersion(&got) {
		t.Fatal("Event reports a resourceVersion")
	}
}

func testDelete(t *testing.T, h *harness) {
	r := newRun("r1", "t1")
	h.mustCreate(t, r)
	if err := h.s.Delete(h.ctx, store.KeyOf(r)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var got v1alpha1.Run
	wantErr(t, h.s.Get(h.ctx, store.KeyOf(r), &got), store.ErrNotFound)
	wantErr(t, h.s.Delete(h.ctx, store.KeyOf(r)), store.ErrNotFound)
	// The name is free again.
	h.mustCreate(t, newRun("r1", "t2"))
}

func testInvalid(t *testing.T, h *harness) {
	wantErr(t, h.s.Create(h.ctx, nil), store.ErrInvalid)
	var nilRun *v1alpha1.Run
	wantErr(t, h.s.Create(h.ctx, nilRun), store.ErrInvalid)
	wantErr(t, h.s.Create(h.ctx, newRun("", "t1")), store.ErrInvalid)
	noKind := newRun("r1", "t1")
	noKind.Kind = ""
	wantErr(t, h.s.Create(h.ctx, noKind), store.ErrInvalid)
	wantErr(t, h.s.Create(h.ctx, &v1alpha1.Queue{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindQueue},
		Metadata: v1alpha1.ObjectMeta{Name: "q"},
	}), store.ErrInvalid)

	h.mustCreate(t, newRun("r1", "t1"))
	var wrong v1alpha1.Task
	wantErr(t, h.s.Get(h.ctx, store.Key{Kind: v1alpha1.KindRun, Name: "r1"}, &wrong), store.ErrInvalid)
	wantErr(t, h.s.Get(h.ctx, store.Key{Kind: v1alpha1.KindRun}, &wrong), store.ErrInvalid)
	wantErr(t, h.s.Delete(h.ctx, store.Key{Name: "r1"}), store.ErrInvalid)
}

func testDeepCopy(t *testing.T, h *harness) {
	k := store.Key{Kind: v1alpha1.KindWorkItem, Name: "wi-1"}
	in := newWorkItem("wi-1", map[string]string{"team": "a"}, v1alpha1.Link{Provider: "gh", Ref: "o/r#1"})
	in.Status = &v1alpha1.WorkItemStatus{State: map[string]any{"phase": "triage"}}
	h.mustCreate(t, in)

	// Mutating the caller's object after Create doesn't reach the store.
	in.Metadata.Labels["team"] = "mutated"
	in.Spec.Links[0].Ref = "mutated"
	in.Status.State["phase"] = "mutated"

	check := func(what string) {
		t.Helper()
		var got v1alpha1.WorkItem
		if err := h.s.Get(h.ctx, k, &got); err != nil {
			t.Fatal(err)
		}
		if got.Metadata.Labels["team"] != "a" || got.Spec.Links[0].Ref != "o/r#1" || got.Status.State["phase"] != "triage" {
			t.Fatalf("%s reached the store: %+v", what, got)
		}
	}
	check("mutating the created object")

	// Nor does mutating what Get, List or Watch returned.
	var got v1alpha1.WorkItem
	if err := h.s.Get(h.ctx, k, &got); err != nil {
		t.Fatal(err)
	}
	got.Metadata.Labels["team"] = "mutated"
	got.Status.State["phase"] = "mutated"
	check("mutating a Get result")

	var list []*v1alpha1.WorkItem
	if _, err := h.s.List(h.ctx, v1alpha1.KindWorkItem, store.ListOptions{}, &list); err != nil {
		t.Fatal(err)
	}
	list[0].Metadata.Labels["team"] = "mutated"
	list[0].Spec.Links[0].Ref = "mutated"
	check("mutating a List result")

	w, err := h.s.Watch(h.ctx, v1alpha1.KindWorkItem, store.WatchOptions{SinceVersion: "0"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	ev := next(t, w)
	ev.Object.(*v1alpha1.WorkItem).Metadata.Labels["team"] = "mutated"
	check("mutating a watch event")
}

func testContext(t *testing.T, h *harness) {
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	r := newRun("r1", "t1")
	wantErr(t, h.s.Create(ctx, r), context.Canceled)
	var got v1alpha1.Run
	wantErr(t, h.s.Get(ctx, store.KeyOf(r), &got), context.Canceled)
	var list []v1alpha1.Run
	_, err := h.s.List(ctx, v1alpha1.KindRun, store.ListOptions{}, &list)
	wantErr(t, err, context.Canceled)
	_, err = h.s.Watch(ctx, v1alpha1.KindRun, store.WatchOptions{})
	wantErr(t, err, context.Canceled)
	_, err = h.s.Lock(ctx, "l", time.Minute)
	wantErr(t, err, context.Canceled)
}

// v parses a resourceVersion as a number, for ordering checks. The contract only promises
// decimal, monotonic versions.
func v(t *testing.T, rv string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(rv, 10, 64)
	if err != nil {
		t.Fatalf("resourceVersion %q is not a decimal counter: %v", rv, err)
	}
	return n
}
