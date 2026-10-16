package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/store"
)

// FactoryEventPrefix starts the id of every event the built-in factory provider emits.
const FactoryEventPrefix = v1alpha1.FactoryProvider + "/"

// FactoryEvent is one factory.* event to emit.
type FactoryEvent struct {
	Type v1alpha1.EventType
	// WorkItem is the work item the event is about; factory events always carry one.
	WorkItem string
	// Key makes the event id ("factory/<Key>"): emitting the same Key twice stores one event,
	// so a retried emitter never duplicates it.
	Key string
	// Facts are the event's facts (run, approval, command args, …).
	Facts map[string]any
	// Actor is who caused it (a command's user); nil for the factory itself.
	Actor *v1alpha1.Actor
}

// FactoryProvider is the built-in `factory` provider: the only writer of factory.* Events. The
// run controller emits factory.run.*, the work-item controller factory.workitem.resynced, and
// the API server's commands factory.command.* (RT-S2-T03). It is safe for concurrent use.
type FactoryProvider struct {
	s   store.Store
	clk clock.Clock
}

// NewFactoryProvider returns the factory provider writing to s.
func NewFactoryProvider(s store.Store, clk clock.Clock) *FactoryProvider {
	if clk == nil {
		clk = clock.Real{}
	}
	return &FactoryProvider{s: s, clk: clk}
}

// Emit stores the event and returns it. Only factory.* types of the vocabulary are accepted.
// An event whose id already exists is not stored again (the existing one is returned).
func (f *FactoryProvider) Emit(ctx context.Context, e FactoryEvent) (v1alpha1.Event, error) {
	switch {
	case !e.Type.IsFactory() || !e.Type.Known():
		return v1alpha1.Event{}, fmt.Errorf("factory provider: %q is not a factory event type", e.Type)
	case e.WorkItem == "":
		return v1alpha1.Event{}, fmt.Errorf("factory provider: %s has no work item", e.Type)
	case e.Key == "":
		return v1alpha1.Event{}, fmt.Errorf("factory provider: %s has no key", e.Type)
	}
	ev := v1alpha1.Event{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEvent},
		ID:       FactoryEventPrefix + e.Key,
		Source:   v1alpha1.FactoryProvider,
		Type:     e.Type,
		Subject:  e.WorkItem,
		Time:     f.clk.Now(),
		Data:     v1alpha1.EventData{Provider: v1alpha1.FactoryProvider, Actor: e.Actor, Facts: e.Facts},
		WorkItem: e.WorkItem,
	}
	err := f.s.Create(ctx, &ev)
	if errors.Is(err, store.ErrAlreadyExists) {
		var got v1alpha1.Event
		if gerr := f.s.Get(ctx, store.Key{Kind: v1alpha1.KindEvent, Name: ev.ID}, &got); gerr != nil {
			return v1alpha1.Event{}, fmt.Errorf("factory provider: emit %s: %w", ev.ID, gerr)
		}
		return got, nil
	}
	if err != nil {
		return v1alpha1.Event{}, fmt.Errorf("factory provider: emit %s: %w", ev.ID, err)
	}
	return ev, nil
}

// EmitCommand emits a factory.command.* event (pause, resume, redispatch, cancel) for the API
// server's commands. key identifies the command (its request id).
func (f *FactoryProvider) EmitCommand(ctx context.Context, typ v1alpha1.EventType, workItem, key string, args map[string]any, actor *v1alpha1.Actor) (v1alpha1.Event, error) {
	if !strings.HasPrefix(string(typ), "factory.command.") {
		return v1alpha1.Event{}, fmt.Errorf("factory provider: %q is not a command event", typ)
	}
	return f.Emit(ctx, FactoryEvent{Type: typ, WorkItem: workItem, Key: "command/" + key, Facts: map[string]any{"command": args}, Actor: actor})
}
