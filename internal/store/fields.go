package store

import (
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Indexed fields, for ListOptions.Field.
const (
	FieldTaskPhase     = "status.phase"        // Task
	FieldTaskQueue     = "spec.queue"          // Task
	FieldTaskWorkItem  = "metadata.workItem"   // Task
	FieldRunTask       = "spec.task"           // Run
	FieldWorkItemLink  = "spec.links"          // WorkItem: LinkFieldValue(provider, ref), one per link
	FieldEffectIdemKey = "spec.idempotencyKey" // Effect
	FieldEffectTask    = "spec.task"           // Effect
)

var indexedFields = map[string][]string{
	v1alpha1.KindTask:     {FieldTaskPhase, FieldTaskQueue, FieldTaskWorkItem},
	v1alpha1.KindRun:      {FieldRunTask},
	v1alpha1.KindWorkItem: {FieldWorkItemLink},
	v1alpha1.KindEffect:   {FieldEffectIdemKey, FieldEffectTask},
}

// IndexedFields lists the fields kind can be listed by.
func IndexedFields(kind string) []string {
	return append([]string(nil), indexedFields[kind]...)
}

// LinkFieldValue is the FieldWorkItemLink value of the link {provider, ref}. Provider names
// have no "/", so the first "/" separates the two.
func LinkFieldValue(provider, ref string) string { return provider + "/" + ref }

// FieldValues returns obj's indexed field values (several for a multi-valued field such as
// WorkItem links). Kinds without indexed fields return nil.
func FieldValues(obj Object) map[string][]string {
	switch o := obj.(type) {
	case *v1alpha1.Task:
		phase := ""
		if o.Status != nil {
			phase = string(o.Status.Phase)
		}
		return map[string][]string{
			FieldTaskPhase:    {phase},
			FieldTaskQueue:    {o.Spec.Queue},
			FieldTaskWorkItem: {o.Metadata.WorkItem},
		}
	case *v1alpha1.Run:
		return map[string][]string{FieldRunTask: {o.Spec.Task}}
	case *v1alpha1.WorkItem:
		links := make([]string, 0, len(o.Spec.Links))
		for _, l := range o.Spec.Links {
			links = append(links, LinkFieldValue(l.Provider, l.Ref))
		}
		return map[string][]string{FieldWorkItemLink: links}
	case *v1alpha1.Effect:
		return map[string][]string{
			FieldEffectIdemKey: {o.Spec.IdempotencyKey},
			FieldEffectTask:    {o.Spec.Task},
		}
	}
	return nil
}

// ValidateFields checks that every field in fields is indexed for kind.
func ValidateFields(kind string, fields map[string]string) error {
	for f := range fields {
		known := false
		for _, g := range indexedFields[kind] {
			if f == g {
				known = true
				break
			}
		}
		if !known {
			return Errorf("list", Key{Kind: kind}, ErrInvalid, "field %q is not indexed for %s (have %v)", f, kind, indexedFields[kind])
		}
	}
	return nil
}
