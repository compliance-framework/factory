package controllers

import (
	"context"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/store"
	"github.com/compliance-framework/factory/internal/store/memory"
)

func TestFactoryProvider(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(t0)
	st := memory.New(store.Options{Clock: clk, Logger: discardLogger()})
	f := NewFactoryProvider(st, clk)

	for _, bad := range []FactoryEvent{
		{Type: v1alpha1.EventIssueOpened, WorkItem: "wi", Key: "k"}, // not factory.*
		{Type: "factory.nope", WorkItem: "wi", Key: "k"},            // not in the vocabulary
		{Type: v1alpha1.EventFactoryRunCompleted, Key: "k"},         // no work item
		{Type: v1alpha1.EventFactoryRunCompleted, WorkItem: "wi"},   // no key
	} {
		if _, err := f.Emit(ctx, bad); err == nil {
			t.Errorf("Emit(%+v) succeeded", bad)
		}
	}

	ev, err := f.Emit(ctx, FactoryEvent{Type: v1alpha1.EventFactoryWorkItemResynced, WorkItem: "wi_1", Key: "resync/wi_1/a"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != "factory/resync/wi_1/a" || ev.Source != v1alpha1.FactoryProvider || ev.WorkItem != "wi_1" || !ev.Time.Equal(t0) {
		t.Errorf("event = %+v", ev)
	}
	clk.Advance(1)
	again, err := f.Emit(ctx, FactoryEvent{Type: v1alpha1.EventFactoryWorkItemResynced, WorkItem: "wi_1", Key: "resync/wi_1/a"})
	if err != nil || !again.Time.Equal(t0) {
		t.Errorf("re-emit = %+v, %v; want the stored event", again, err)
	}

	cmd, err := f.EmitCommand(ctx, v1alpha1.EventFactoryCommandPause, "wi_1", "req-1", map[string]any{"scope": "workitem"}, &v1alpha1.Actor{ID: "github-public:gus"})
	if err != nil || cmd.ID != "factory/command/req-1" || cmd.Data.Actor.ID != "github-public:gus" {
		t.Errorf("command = %+v, %v", cmd, err)
	}
	if _, err := f.EmitCommand(ctx, v1alpha1.EventFactoryRunCompleted, "wi_1", "req-2", nil, nil); err == nil {
		t.Error("EmitCommand accepted a run event")
	}
}
