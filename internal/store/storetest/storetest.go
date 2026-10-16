// Package storetest is the conformance suite for store implementations: the in-memory store
// passes it today and a Postgres store must pass it later.
//
//	func TestConformance(t *testing.T) {
//		storetest.Run(t, func(o store.Options) storetest.Subject { return memory.New(o) })
//	}
//
// The suite passes each new store its Options: a clock.Fake (which drives CreatedAt, lock
// expiry and retention) and, for some tests, smaller caps and buffers. Run it under -race.
package storetest

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/store"
)

// Subject is what an implementation provides: the Store, its retention sweeper and its
// transcript blobs.
type Subject interface {
	store.Store
	store.Sweeper
	store.TranscriptStore
}

// NewStore returns an empty store configured with opts.
type NewStore func(opts store.Options) Subject

// Epoch is the fake clock's starting time.
var Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Run runs the conformance suite against stores made by newStore.
func Run(t *testing.T, newStore NewStore) {
	t.Helper()
	for _, g := range []struct {
		name  string
		tests []test
	}{
		{"CRUD", crudTests},
		{"List", listTests},
		{"Watch", watchTests},
		{"Lock", lockTests},
		{"Retention", retentionTests},
		{"Transcripts", transcriptTests},
		{"Concurrency", concurrencyTests},
	} {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			for _, tc := range g.tests {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					tc.run(t, newHarness(t, newStore, tc.opts))
				})
			}
		})
	}
}

type test struct {
	name string
	opts func(*store.Options)
	run  func(t *testing.T, h *harness)
}

// harness is one test's store and clock.
type harness struct {
	s     Subject
	clock *clock.Fake
	opts  store.Options
	ctx   context.Context
}

func newHarness(t *testing.T, newStore NewStore, mutate func(*store.Options)) *harness {
	clk := clock.NewFake(Epoch)
	opts := store.Options{Clock: clk, Logger: slog.New(slog.DiscardHandler)}
	if mutate != nil {
		mutate(&opts)
	}
	return &harness{s: newStore(opts), clock: clk, opts: opts.WithDefaults(), ctx: t.Context()}
}

// ---- fixtures --------------------------------------------------------------------------------

func newTask(id, workItem, queue string, phase v1alpha1.TaskPhase, labels map[string]string) *v1alpha1.Task {
	return &v1alpha1.Task{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindTask},
		Metadata: v1alpha1.TaskMeta{ID: id, WorkItem: workItem, Labels: labels},
		Spec:     v1alpha1.TaskRunSpec{TaskSpec: v1alpha1.TaskSpecRef{Name: "triage"}, Queue: queue},
		Status:   &v1alpha1.TaskStatus{Phase: phase},
	}
}

func newRun(name, task string) *v1alpha1.Run {
	return &v1alpha1.Run{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindRun},
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.RunSpec{Task: task, Attempt: 1, Agent: "agent-1"},
	}
}

func newEvent(id string) *v1alpha1.Event {
	return &v1alpha1.Event{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEvent},
		ID:       id,
		Source:   "github-public",
		Type:     v1alpha1.EventIssueOpened,
		Time:     Epoch,
		Data:     v1alpha1.EventData{Provider: "github-public"},
	}
}

func newWorkItem(name string, labels map[string]string, links ...v1alpha1.Link) *v1alpha1.WorkItem {
	return &v1alpha1.WorkItem{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
		Metadata: v1alpha1.ObjectMeta{Name: name, Labels: labels},
		Spec:     v1alpha1.WorkItemSpec{Links: links},
	}
}

func newEffect(name, task, idem string) *v1alpha1.Effect {
	return &v1alpha1.Effect{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEffect},
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.EffectSpec{
			Task: task, Op: v1alpha1.OpComment, IdempotencyKey: idem,
			Target: v1alpha1.ObjectRef{Ref: "compliance-framework/docs#1"},
		},
	}
}

func newCounter(name string) *v1alpha1.BudgetCounter {
	return &v1alpha1.BudgetCounter{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindBudgetCounter},
		Metadata: v1alpha1.ObjectMeta{Name: name},
	}
}

func newAgent(name string) *v1alpha1.Agent {
	return &v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindAgent},
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.AgentSpec{Pool: "default", Owner: "octocat"},
	}
}

// ---- assertions ------------------------------------------------------------------------------

func (h *harness) mustCreate(t *testing.T, objs ...store.Object) {
	t.Helper()
	for _, o := range objs {
		if err := h.s.Create(h.ctx, o); err != nil {
			t.Fatalf("Create %s: %v", store.KeyOf(o), err)
		}
	}
}

func (h *harness) mustUpdate(t *testing.T, o store.Object) {
	t.Helper()
	if err := h.s.Update(h.ctx, o); err != nil {
		t.Fatalf("Update %s: %v", store.KeyOf(o), err)
	}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func listNames(t *testing.T, h *harness, kind string, opts store.ListOptions) []string {
	t.Helper()
	var objs []store.Object
	if _, err := h.s.List(h.ctx, kind, opts, &objs); err != nil {
		t.Fatalf("List %s: %v", kind, err)
	}
	names := make([]string, len(objs))
	for i, o := range objs {
		names[i] = o.GetName()
	}
	return names
}
