package store

import (
	"reflect"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// MetaObject is implemented by stored kinds outside v1alpha1 (the internal Lease, Debounce,
// Session and Delivery records) to give the store their ObjectMeta.
type MetaObject interface {
	Object
	GetObjectMeta() *v1alpha1.ObjectMeta
}

// meta points at an object's store-managed metadata fields.
type meta struct {
	rv      *string
	created *time.Time
	labels  map[string]string
}

// metaOf returns obj's metadata fields; ok is false for kinds without metadata (Event, whose
// version and creation time the store keeps beside it).
func metaOf(obj Object) (m meta, ok bool) {
	om := func(o *v1alpha1.ObjectMeta) (meta, bool) {
		return meta{rv: &o.ResourceVersion, created: &o.CreatedAt, labels: o.Labels}, true
	}
	switch o := obj.(type) {
	case *v1alpha1.Task:
		return meta{rv: &o.Metadata.ResourceVersion, created: &o.Metadata.CreatedAt, labels: o.Metadata.Labels}, true
	case *v1alpha1.WorkItem:
		return om(&o.Metadata)
	case *v1alpha1.Run:
		return om(&o.Metadata)
	case *v1alpha1.Agent:
		return om(&o.Metadata)
	case *v1alpha1.Effect:
		return om(&o.Metadata)
	case *v1alpha1.BudgetCounter:
		return om(&o.Metadata)
	case *v1alpha1.Approval:
		return om(&o.Metadata.ObjectMeta)
	case MetaObject:
		return om(o.GetObjectMeta())
	}
	return meta{}, false
}

// ResourceVersion returns obj's resourceVersion ("" for kinds without one).
func ResourceVersion(obj Object) string {
	if m, ok := metaOf(obj); ok {
		return *m.rv
	}
	return ""
}

// SetResourceVersion sets obj's resourceVersion; false for kinds without one.
func SetResourceVersion(obj Object, rv string) bool {
	m, ok := metaOf(obj)
	if ok {
		*m.rv = rv
	}
	return ok
}

// CreatedAt returns obj's creation time (zero for kinds without one).
func CreatedAt(obj Object) time.Time {
	if m, ok := metaOf(obj); ok {
		return *m.created
	}
	return time.Time{}
}

// SetCreatedAt sets obj's creation time; false for kinds without one.
func SetCreatedAt(obj Object, t time.Time) bool {
	m, ok := metaOf(obj)
	if ok {
		*m.created = t
	}
	return ok
}

// HasResourceVersion reports whether obj's kind carries a resourceVersion, i.e. whether Update
// is compare-and-swap for it.
func HasResourceVersion(obj Object) bool {
	_, ok := metaOf(obj)
	return ok
}

// Labels returns obj's labels (nil for kinds without labels). Don't modify them.
func Labels(obj Object) map[string]string {
	if m, ok := metaOf(obj); ok {
		return m.labels
	}
	return nil
}

// CheckObject validates obj for a write by op and returns its key: a non-nil pointer with a
// kind (not a config kind) and a name.
func CheckObject(op string, obj Object) (Key, error) {
	if obj == nil {
		return Key{}, Errorf(op, Key{}, ErrInvalid, "nil object")
	}
	if v := reflect.ValueOf(obj); v.Kind() != reflect.Pointer || v.IsNil() {
		return Key{}, Errorf(op, Key{}, ErrInvalid, "object must be a non-nil pointer, got %T", obj)
	}
	k := KeyOf(obj)
	if k.Kind == "" {
		return k, Errorf(op, k, ErrInvalid, "missing kind")
	}
	if v1alpha1.IsConfigKind(k.Kind) {
		return k, Errorf(op, k, ErrInvalid, "%s is a config kind: it lives in the catalog, not the store", k.Kind)
	}
	if k.Name == "" {
		return k, Errorf(op, k, ErrInvalid, "missing name")
	}
	return k, nil
}
