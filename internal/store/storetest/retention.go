package storetest

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
)

const day = 24 * time.Hour

var retentionTests = []test{
	{name: "Default90Days", run: testRetentionDefault},
	{name: "Configurable", opts: func(o *store.Options) { o.Retention = time.Hour }, run: testRetentionConfigurable},
	{name: "TerminalTasks", run: testRetentionTerminalTasks},
	{name: "Watchers", run: testRetentionWatchers},
}

var transcriptTests = []test{
	{name: "PutGet", run: testTranscriptPutGet},
	{name: "PerTranscriptCap", opts: func(o *store.Options) { o.TranscriptMaxBytes = 10 }, run: testTranscriptPerCap},
	{name: "TotalCapEvictsOldest", opts: func(o *store.Options) {
		o.TranscriptMaxBytes = 10
		o.TranscriptTotalBytes = 25
	}, run: testTranscriptTotalCap},
	{name: "DeletedWithRun", run: testTranscriptDeletedWithRun},
	{name: "Retention", run: testTranscriptRetention},
}

// names lists every object of kind.
func (h *harness) names(t *testing.T, kind string) []string {
	t.Helper()
	return listNames(t, h, kind, store.ListOptions{})
}

func (h *harness) sweep(t *testing.T) store.SweepResult {
	t.Helper()
	res, err := h.s.Sweep(h.ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return res
}

func testRetentionDefault(t *testing.T, h *harness) {
	if h.opts.Retention != 90*day {
		t.Fatalf("default retention %s", h.opts.Retention)
	}
	h.mustCreate(t,
		newRun("r-old", "t"), newEvent("e-old"), newEffect("x-old", "t", "k"), newCounter("budget.global.all.2026-01-01"),
		newWorkItem("wi", nil), newAgent("agent"),
		newTask("t-running", "wi", "q", v1alpha1.TaskRunning, nil),
	)
	h.clock.Advance(10 * day)
	h.mustCreate(t, newRun("r-new", "t"), newEvent("e-new"))

	h.clock.Set(Epoch.Add(90*day - time.Second))
	if res := h.sweep(t); len(res.Objects) != 0 {
		t.Fatalf("swept %v before 90 days", res.Objects)
	}

	h.clock.Set(Epoch.Add(90 * day))
	res := h.sweep(t)
	want := map[string]int{v1alpha1.KindRun: 1, v1alpha1.KindEvent: 1, v1alpha1.KindEffect: 1, v1alpha1.KindBudgetCounter: 1}
	for k, n := range want {
		if res.Objects[k] != n {
			t.Fatalf("swept %v, want %v", res.Objects, want)
		}
	}
	for kind, names := range map[string][]string{
		v1alpha1.KindRun:           {"r-new"},
		v1alpha1.KindEvent:         {"e-new"},
		v1alpha1.KindEffect:        {},
		v1alpha1.KindBudgetCounter: {},
		v1alpha1.KindWorkItem:      {"wi"},
		v1alpha1.KindAgent:         {"agent"},
		v1alpha1.KindTask:          {"t-running"},
	} {
		if got := h.names(t, kind); !slices.Equal(got, names) {
			t.Fatalf("%s after sweep = %v, want %v", kind, got, names)
		}
	}

	h.clock.Set(Epoch.Add(100 * day))
	h.sweep(t)
	if got := h.names(t, v1alpha1.KindRun); len(got) != 0 {
		t.Fatalf("runs = %v", got)
	}
}

func testRetentionConfigurable(t *testing.T, h *harness) {
	h.mustCreate(t, newRun("r1", "t"))
	h.clock.Advance(59 * time.Minute)
	h.sweep(t)
	if got := h.names(t, v1alpha1.KindRun); len(got) != 1 {
		t.Fatalf("runs = %v", got)
	}
	h.clock.Advance(time.Minute)
	h.sweep(t)
	if got := h.names(t, v1alpha1.KindRun); len(got) != 0 {
		t.Fatalf("runs = %v", got)
	}
}

func testRetentionTerminalTasks(t *testing.T, h *harness) {
	done := newTask("t-done", "wi", "q", v1alpha1.TaskRunning, nil)
	h.mustCreate(t, done, newTask("t-pending", "wi", "q", v1alpha1.TaskPending, nil))
	// It reaches a terminal phase 50 days in: it is kept 90 days from then.
	h.clock.Set(Epoch.Add(50 * day))
	done.Status.Phase = v1alpha1.TaskSucceeded
	h.mustUpdate(t, done)

	h.clock.Set(Epoch.Add(139 * day))
	h.sweep(t)
	if got := h.names(t, v1alpha1.KindTask); !slices.Equal(got, []string{"t-done", "t-pending"}) {
		t.Fatalf("tasks at day 139 = %v", got)
	}
	h.clock.Set(Epoch.Add(140 * day))
	h.sweep(t)
	if got := h.names(t, v1alpha1.KindTask); !slices.Equal(got, []string{"t-pending"}) {
		t.Fatalf("tasks at day 140 = %v", got)
	}
	for _, phase := range []v1alpha1.TaskPhase{v1alpha1.TaskFailed, v1alpha1.TaskCancelled} {
		if store.RetentionOf(newTask("x", "wi", "q", phase, nil)) != store.SinceUpdated {
			t.Fatalf("%s task is not swept", phase)
		}
	}
}

func testRetentionWatchers(t *testing.T, h *harness) {
	h.mustCreate(t, newRun("r1", "t"))
	w, err := h.s.Watch(h.ctx, v1alpha1.KindRun, store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	h.clock.Advance(90 * day)
	h.sweep(t)
	expect(t, next(t, w), store.Deleted, "r1")
}

func testTranscriptPutGet(t *testing.T, h *harness) {
	blob := []byte("\x1f\x8bgzip-bytes")
	if err := h.s.PutTranscript(h.ctx, "r1", blob); err != nil {
		t.Fatal(err)
	}
	blob[0] = 'X' // the store kept its own copy
	got, err := h.s.GetTranscript(h.ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("\x1f\x8bgzip-bytes")) {
		t.Fatalf("got %q", got)
	}
	got[0] = 'Y' // and returns a copy
	again, _ := h.s.GetTranscript(h.ctx, "r1")
	if again[0] != 0x1f {
		t.Fatal("GetTranscript returned the stored slice")
	}
	if err := h.s.PutTranscript(h.ctx, "r1", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.s.GetTranscript(h.ctx, "r1"); string(got) != "v2" {
		t.Fatalf("replaced transcript = %q", got)
	}
	_, err = h.s.GetTranscript(h.ctx, "nope")
	wantErr(t, err, store.ErrNotFound)
	wantErr(t, h.s.PutTranscript(h.ctx, "", blob), store.ErrInvalid)
}

func testTranscriptPerCap(t *testing.T, h *harness) {
	if err := h.s.PutTranscript(h.ctx, "r1", make([]byte, 10)); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
	wantErr(t, h.s.PutTranscript(h.ctx, "r2", make([]byte, 11)), store.ErrTooLarge)
	// A rejected replacement keeps the old transcript.
	wantErr(t, h.s.PutTranscript(h.ctx, "r1", make([]byte, 11)), store.ErrTooLarge)
	if got, err := h.s.GetTranscript(h.ctx, "r1"); err != nil || len(got) != 10 {
		t.Fatalf("r1 = %d bytes, %v", len(got), err)
	}
}

func testTranscriptTotalCap(t *testing.T, h *harness) {
	put := func(run string, n int) {
		t.Helper()
		if err := h.s.PutTranscript(h.ctx, run, bytes.Repeat([]byte{run[0]}, n)); err != nil {
			t.Fatalf("Put %s: %v", run, err)
		}
		h.clock.Advance(time.Second)
	}
	has := func(runs ...string) {
		t.Helper()
		for _, r := range []string{"a", "b", "c", "d"} {
			_, err := h.s.GetTranscript(h.ctx, r)
			if want := slices.Contains(runs, r); want != (err == nil) {
				t.Fatalf("transcript %s present=%v, want %v (%v)", r, err == nil, want, err)
			}
		}
	}
	put("a", 10)
	put("b", 10)
	has("a", "b")
	put("c", 10) // 30 > 25: the oldest (a) goes
	has("b", "c")
	put("b", 5) // replacing b frees its old bytes: 15, nothing evicted
	has("b", "c")
	put("d", 10) // 25: fits
	has("b", "c", "d")
	put("a", 10) // 35: c (oldest now) goes, then 25 fits
	has("b", "d", "a")
}

func testTranscriptDeletedWithRun(t *testing.T, h *harness) {
	r := newRun("r1", "t")
	h.mustCreate(t, r)
	if err := h.s.PutTranscript(h.ctx, "r1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Delete(h.ctx, store.KeyOf(r)); err != nil {
		t.Fatal(err)
	}
	_, err := h.s.GetTranscript(h.ctx, "r1")
	wantErr(t, err, store.ErrNotFound)
}

func testTranscriptRetention(t *testing.T, h *harness) {
	if err := h.s.PutTranscript(h.ctx, "old", []byte("x")); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(30 * day)
	if err := h.s.PutTranscript(h.ctx, "new", []byte("y")); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(60 * day)
	if res := h.sweep(t); res.Transcripts != 1 {
		t.Fatalf("swept %d transcripts, want 1", res.Transcripts)
	}
	_, err := h.s.GetTranscript(h.ctx, "old")
	wantErr(t, err, store.ErrNotFound)
	if _, err := h.s.GetTranscript(h.ctx, "new"); err != nil {
		t.Fatalf("new transcript: %v", err)
	}
}
