package store

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

func TestCursor(t *testing.T) {
	c := EncodeCursor("Run", "r/1 with spaces")
	got, err := DecodeCursor("Run", c)
	if err != nil || got != "r/1 with spaces" {
		t.Fatalf("round trip = %q, %v", got, err)
	}
	if got, err := DecodeCursor("Run", ""); err != nil || got != "" {
		t.Fatalf("empty = %q, %v", got, err)
	}
	for _, bad := range []string{"%%%", EncodeCursor("Task", "x"), "e30"} { // e30 = "{}"
		if _, err := DecodeCursor("Run", bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("DecodeCursor(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestFieldValues(t *testing.T) {
	task := &v1alpha1.Task{
		Metadata: v1alpha1.TaskMeta{ID: "t", WorkItem: "wi"},
		Spec:     v1alpha1.TaskRunSpec{Queue: "q"},
	}
	if got := FieldValues(task); got[FieldTaskPhase][0] != "" || got[FieldTaskQueue][0] != "q" || got[FieldTaskWorkItem][0] != "wi" {
		t.Fatalf("task without status = %v", got)
	}
	wi := &v1alpha1.WorkItem{Spec: v1alpha1.WorkItemSpec{Links: []v1alpha1.Link{{Provider: "gh", Ref: "o/r#1"}, {Provider: "slack", Ref: "C1/123"}}}}
	if got := FieldValues(wi)[FieldWorkItemLink]; len(got) != 2 || got[0] != "gh/o/r#1" || got[1] != "slack/C1/123" {
		t.Fatalf("links = %v", got)
	}
	if got := FieldValues(&v1alpha1.Agent{}); got != nil {
		t.Fatalf("agent = %v", got)
	}
	if err := ValidateFields(v1alpha1.KindRun, map[string]string{FieldRunTask: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFields(v1alpha1.KindRun, map[string]string{FieldTaskQueue: "q"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	if got := IndexedFields(v1alpha1.KindTask); len(got) != 3 {
		t.Fatalf("IndexedFields(Task) = %v", got)
	}
}

func TestMeta(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, obj := range []Object{
		&v1alpha1.Task{}, &v1alpha1.WorkItem{}, &v1alpha1.Run{}, &v1alpha1.Agent{},
		&v1alpha1.Effect{}, &v1alpha1.BudgetCounter{}, &v1alpha1.Approval{}, &internalKind{},
	} {
		if !SetResourceVersion(obj, "7") || ResourceVersion(obj) != "7" {
			t.Fatalf("%T: rv not set", obj)
		}
		if !SetCreatedAt(obj, at) || !CreatedAt(obj).Equal(at) {
			t.Fatalf("%T: createdAt not set", obj)
		}
	}
	ev := &v1alpha1.Event{}
	if SetResourceVersion(ev, "7") || ResourceVersion(ev) != "" || HasResourceVersion(ev) || Labels(ev) != nil {
		t.Fatal("Event has metadata")
	}
	run := &v1alpha1.Run{Metadata: v1alpha1.ObjectMeta{Labels: map[string]string{"a": "b"}}}
	if Labels(run)["a"] != "b" {
		t.Fatal("labels")
	}
}

type internalKind struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
}

func (i *internalKind) GetName() string                     { return i.Metadata.Name }
func (i *internalKind) GetObjectMeta() *v1alpha1.ObjectMeta { return &i.Metadata }

func TestCheckObject(t *testing.T) {
	var nilRun *v1alpha1.Run
	for _, tc := range []struct {
		name string
		obj  Object
		ok   bool
	}{
		{"ok", &v1alpha1.Run{TypeMeta: v1alpha1.TypeMeta{Kind: "Run"}, Metadata: v1alpha1.ObjectMeta{Name: "r"}}, true},
		{"internal kind", &internalKind{TypeMeta: v1alpha1.TypeMeta{Kind: "Lease"}, Metadata: v1alpha1.ObjectMeta{Name: "l"}}, true},
		{"nil", nil, false},
		{"typed nil", nilRun, false},
		{"no kind", &v1alpha1.Run{Metadata: v1alpha1.ObjectMeta{Name: "r"}}, false},
		{"no name", &v1alpha1.Run{TypeMeta: v1alpha1.TypeMeta{Kind: "Run"}}, false},
		{"config kind", &v1alpha1.Pool{TypeMeta: v1alpha1.TypeMeta{Kind: "Pool"}, Metadata: v1alpha1.ObjectMeta{Name: "p"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CheckObject("create", tc.obj)
			if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrInvalid)) {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestRetention(t *testing.T) {
	task := func(p v1alpha1.TaskPhase) *v1alpha1.Task {
		return &v1alpha1.Task{Status: &v1alpha1.TaskStatus{Phase: p}}
	}
	for _, tc := range []struct {
		obj  Object
		want Retention
	}{
		{&v1alpha1.Run{}, SinceCreated},
		{&v1alpha1.Event{}, SinceCreated},
		{&v1alpha1.Effect{}, SinceCreated},
		{&v1alpha1.BudgetCounter{}, SinceCreated},
		{&v1alpha1.Task{}, Keep},
		{task(v1alpha1.TaskRunning), Keep},
		{task(v1alpha1.TaskCancelling), Keep},
		{task(v1alpha1.TaskSucceeded), SinceUpdated},
		{task(v1alpha1.TaskFailed), SinceUpdated},
		{task(v1alpha1.TaskCancelled), SinceUpdated},
		{&v1alpha1.WorkItem{}, Keep},
		{&v1alpha1.Agent{}, Keep},
		{&v1alpha1.Approval{}, Keep},
	} {
		if got := RetentionOf(tc.obj); got != tc.want {
			t.Errorf("RetentionOf(%T %+v) = %v, want %v", tc.obj, tc.obj, got, tc.want)
		}
	}

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ret := time.Hour
	if Expired(SinceCreated, t0, t0.Add(time.Hour), t0.Add(59*time.Minute), ret) ||
		!Expired(SinceCreated, t0, t0.Add(time.Hour), t0.Add(time.Hour), ret) {
		t.Error("SinceCreated")
	}
	if Expired(SinceUpdated, t0, t0.Add(time.Hour), t0.Add(time.Hour), ret) ||
		!Expired(SinceUpdated, t0, t0.Add(time.Hour), t0.Add(2*time.Hour), ret) {
		t.Error("SinceUpdated")
	}
	if Expired(Keep, t0, t0, t0.Add(1000*time.Hour), ret) {
		t.Error("Keep")
	}
}

type countingSweeper struct{ n atomic.Int32 }

func (c *countingSweeper) Sweep(context.Context) (SweepResult, error) {
	c.n.Add(1)
	return SweepResult{}, errors.New("boom") // logged, not fatal
}

func TestRunSweeper(t *testing.T) {
	var s countingSweeper
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		RunSweeper(ctx, &s, time.Millisecond, slog.New(slog.DiscardHandler))
		close(done)
	}()
	deadline := time.After(5 * time.Second)
	for s.n.Load() < 3 {
		select {
		case <-deadline:
			t.Fatal("sweeper did not tick")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestErrorFormat(t *testing.T) {
	for _, tc := range []struct {
		err  *Error
		want string
	}{
		{Errorf("get", Key{Kind: "Run", Name: "r1"}, ErrNotFound, ""), "store: get Run/r1: not found"},
		{Errorf("list", Key{Kind: "Run"}, ErrInvalid, "bad %s", "cursor"), "store: list Run: invalid: bad cursor"},
		{Errorf("lock", Key{Name: "wi/1"}, ErrLockLost, ""), "store: lock wi/1: lock lost"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}

func TestOptionsDefaults(t *testing.T) {
	o := Options{}.WithDefaults()
	if o.Clock == nil || o.Logger == nil || o.Retention != 90*24*time.Hour || o.WatchBuffer != 256 ||
		o.History != 10_000 || o.TranscriptMaxBytes != 20<<20 || o.TranscriptTotalBytes != 2<<30 {
		t.Fatalf("defaults = %+v", o)
	}
}
