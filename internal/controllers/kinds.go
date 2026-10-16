package controllers

import (
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/rules"
	"github.com/compliance-framework/factory/internal/store"
)

// Internal kinds the controllers keep in the store. They are not part of v1alpha1 (no API, no
// schema): the store reaches their metadata through store.MetaObject.
const (
	// KindEventStatus marks an Event as processed by the rule controller. v1alpha1.Event has
	// no status, so the mark is a record named by the event id; it is deleted when its Event is.
	KindEventStatus = "EventStatus"
	// KindDebounce is a debounced rule's held actions (the LLD's internal Debounce object).
	KindDebounce = "Debounce"
	// KindTaskPrompt is a Task's rendered prompt (the LLD's blob prompt/<taskID>), named by the
	// task id. The agent fetches it through the API server.
	KindTaskPrompt = "TaskPrompt"
)

// EventStatus records that the rule controller processed an Event.
type EventStatus struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
	// WorkItem is the work item the event was applied to; empty when no rule matched.
	WorkItem    string    `json:"workItem,omitzero"`
	ProcessedAt time.Time `json:"processedAt"`
}

var _ store.MetaObject = (*EventStatus)(nil)

// GetName returns metadata.name (the event id).
func (e *EventStatus) GetName() string { return e.Metadata.Name }

// GetObjectMeta implements store.MetaObject.
func (e *EventStatus) GetObjectMeta() *v1alpha1.ObjectMeta { return &e.Metadata }

// DebounceRecord is a pending debounced rule: the first match of Key holds the rule's actions
// until Until; later matches of the same Key change nothing (first wins).
type DebounceRecord struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
	Spec     DebounceSpec        `json:"spec"`
}

// DebounceSpec is what a DebounceRecord holds.
type DebounceSpec struct {
	// Key is the engine's debounce key ("<rule>/<value>").
	Key      string `json:"key"`
	WorkItem string `json:"workItem"`
	// Event, RuleSet and Rule are where the held actions come from (task provenance, records).
	Event   string    `json:"event"`
	RuleSet string    `json:"ruleSet"`
	Rule    string    `json:"rule"`
	Until   time.Time `json:"until"`
	// Then are the rule's materialised actions, in order.
	Then []rules.ActionIntent `json:"then"`
}

var _ store.MetaObject = (*DebounceRecord)(nil)

// GetName returns metadata.name.
func (d *DebounceRecord) GetName() string { return d.Metadata.Name }

// GetObjectMeta implements store.MetaObject.
func (d *DebounceRecord) GetObjectMeta() *v1alpha1.ObjectMeta { return &d.Metadata }

// TaskPrompt is a Task's rendered prompt.
type TaskPrompt struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
	Text     string              `json:"text"`
	Digest   string              `json:"digest"`
}

var _ store.MetaObject = (*TaskPrompt)(nil)

// GetName returns metadata.name (the task id).
func (p *TaskPrompt) GetName() string { return p.Metadata.Name }

// GetObjectMeta implements store.MetaObject.
func (p *TaskPrompt) GetObjectMeta() *v1alpha1.ObjectMeta { return &p.Metadata }
