// Package memory is the in-memory store.Store: one map per kind behind a single RWMutex (PoC
// scale: thousands of objects), JSON deep copies on the way in and out, field indexes, watch
// fan-out with bounded buffers and a replay history, leased locks, retention and the
// transcript blobs.
package memory

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/store"
)

// record is one stored object. Records are immutable: a write replaces the record, so a
// reader may decode one after releasing the lock.
type record struct {
	key       store.Key
	typ       reflect.Type // the struct type; the stored object is a *typ
	data      []byte       // JSON, with resourceVersion and createdAt set when the kind has them
	rv        uint64
	created   time.Time
	updated   time.Time
	labels    map[string]string
	fields    map[string][]string
	retention store.Retention
}

// Store is the in-memory store. Use New.
type Store struct {
	opts store.Options
	clk  clock.Clock
	log  *slog.Logger

	mu       sync.RWMutex
	rv       uint64
	objects  map[string]map[string]*record                        // kind → name
	index    map[string]map[string]map[string]map[string]struct{} // kind → field → value → names
	history  ring
	watchers map[string]map[*watcher]struct{} // kind → watchers

	lockMu   sync.Mutex
	locks    map[string]*lockState
	lockSeq  uint64
	lockWake chan struct{} // closed (and replaced) whenever a lock is released

	blobMu    sync.Mutex
	blobs     map[string]*blob
	blobTotal int64
	blobSeq   uint64
}

var (
	_ store.Store           = (*Store)(nil)
	_ store.Sweeper         = (*Store)(nil)
	_ store.TranscriptStore = (*Store)(nil)
)

// New returns an empty store.
func New(opts store.Options) *Store {
	opts = opts.WithDefaults()
	return &Store{
		opts:     opts,
		clk:      opts.Clock,
		log:      opts.Logger,
		objects:  map[string]map[string]*record{},
		index:    map[string]map[string]map[string]map[string]struct{}{},
		history:  newRing(opts.History),
		watchers: map[string]map[*watcher]struct{}{},
		locks:    map[string]*lockState{},
		lockWake: make(chan struct{}),
		blobs:    map[string]*blob{},
	}
}

// Get implements store.Store.
func (s *Store) Get(ctx context.Context, k store.Key, out store.Object) error {
	if err := ctx.Err(); err != nil {
		return store.Errorf("get", k, err, "")
	}
	if k.Kind == "" || k.Name == "" {
		return store.Errorf("get", k, store.ErrInvalid, "missing kind or name")
	}
	ov := reflect.ValueOf(out)
	if out == nil || ov.Kind() != reflect.Pointer || ov.IsNil() {
		return store.Errorf("get", k, store.ErrInvalid, "out must be a non-nil pointer, got %T", out)
	}
	s.mu.RLock()
	rec := s.objects[k.Kind][k.Name]
	s.mu.RUnlock()
	if rec == nil {
		return store.Errorf("get", k, store.ErrNotFound, "")
	}
	if ov.Elem().Type() != rec.typ {
		return store.Errorf("get", k, store.ErrInvalid, "out is %T, the object is *%s", out, rec.typ)
	}
	ov.Elem().SetZero()
	if err := json.Unmarshal(rec.data, out); err != nil {
		return store.Errorf("get", k, err, "decode")
	}
	return nil
}

// List implements store.Store.
func (s *Store) List(ctx context.Context, kind string, opts store.ListOptions, out any) (store.ListMeta, error) {
	k := store.Key{Kind: kind}
	if err := ctx.Err(); err != nil {
		return store.ListMeta{}, store.Errorf("list", k, err, "")
	}
	if kind == "" {
		return store.ListMeta{}, store.Errorf("list", k, store.ErrInvalid, "missing kind")
	}
	if err := opts.LabelSelector.Validate(); err != nil {
		return store.ListMeta{}, store.Errorf("list", k, store.ErrInvalid, "%v", err)
	}
	if err := store.ValidateFields(kind, opts.Field); err != nil {
		return store.ListMeta{}, err
	}
	if opts.Limit < 0 {
		return store.ListMeta{}, store.Errorf("list", k, store.ErrInvalid, "negative limit %d", opts.Limit)
	}
	after, err := store.DecodeCursor(kind, opts.Cursor)
	if err != nil {
		return store.ListMeta{}, err
	}
	sv := reflect.ValueOf(out)
	if out == nil || sv.Kind() != reflect.Pointer || sv.IsNil() || sv.Elem().Kind() != reflect.Slice {
		return store.ListMeta{}, store.Errorf("list", k, store.ErrInvalid, "out must be a pointer to a slice, got %T", out)
	}

	s.mu.RLock()
	var recs []*record
	keep := func(rec *record) {
		if rec.key.Name > after && opts.LabelSelector.Matches(rec.labels) {
			recs = append(recs, rec)
		}
	}
	if len(opts.Field) > 0 {
		for name := range s.fieldMatches(kind, opts.Field) {
			keep(s.objects[kind][name])
		}
	} else {
		for _, rec := range s.objects[kind] {
			keep(rec)
		}
	}
	meta := store.ListMeta{ResourceVersion: store.FormatVersion(s.rv)}
	s.mu.RUnlock()

	slices.SortFunc(recs, func(a, b *record) int { return cmp.Compare(a.key.Name, b.key.Name) })
	if opts.Limit > 0 && len(recs) > opts.Limit {
		recs = recs[:opts.Limit]
		meta.NextCursor = store.EncodeCursor(kind, recs[len(recs)-1].key.Name)
	}

	slice := reflect.MakeSlice(sv.Elem().Type(), 0, len(recs))
	elem := slice.Type().Elem()
	for _, rec := range recs {
		v, err := rec.decode()
		if err != nil {
			return store.ListMeta{}, store.Errorf("list", rec.key, err, "decode")
		}
		switch {
		case elem == v.Type():
		case elem == rec.typ:
			v = v.Elem()
		case elem.Kind() == reflect.Interface && v.Type().Implements(elem):
		default:
			return store.ListMeta{}, store.Errorf("list", k, store.ErrInvalid, "out is %T, the objects are *%s", out, rec.typ)
		}
		slice = reflect.Append(slice, v)
	}
	sv.Elem().Set(slice)
	return meta, nil
}

// fieldMatches returns the names of kind's objects whose indexed fields have the values in
// fields. The caller holds s.mu.
func (s *Store) fieldMatches(kind string, fields map[string]string) map[string]struct{} {
	var out map[string]struct{}
	for f, v := range fields {
		names := s.index[kind][f][v]
		if out == nil {
			out = maps.Clone(names)
			if out == nil {
				return nil
			}
			continue
		}
		for n := range out {
			if _, ok := names[n]; !ok {
				delete(out, n)
			}
		}
	}
	return out
}

// Create implements store.Store.
func (s *Store) Create(ctx context.Context, obj store.Object) error {
	k, err := store.CheckObject("create", obj)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return store.Errorf("create", k, err, "")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects[k.Kind][k.Name] != nil {
		return store.Errorf("create", k, store.ErrAlreadyExists, "")
	}
	now := s.clk.Now()
	rec, err := s.newRecord("create", k, obj, s.rv+1, now, now)
	if err != nil {
		return err
	}
	s.rv++
	s.put(rec)
	s.publish(&change{typ: store.Added, rec: rec})
	return nil
}

// Update implements store.Store.
func (s *Store) Update(ctx context.Context, obj store.Object) error {
	k, err := store.CheckObject("update", obj)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return store.Errorf("update", k, err, "")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.objects[k.Kind][k.Name]
	if old == nil {
		return store.Errorf("update", k, store.ErrNotFound, "")
	}
	if t := reflect.TypeOf(obj).Elem(); t != old.typ {
		return store.Errorf("update", k, store.ErrInvalid, "object is %T, the stored one is *%s", obj, old.typ)
	}
	if store.HasResourceVersion(obj) {
		if got, want := store.ResourceVersion(obj), store.FormatVersion(old.rv); got != want {
			return store.Errorf("update", k, store.ErrConflict, "resourceVersion %q, stored %q", got, want)
		}
	}
	rec, err := s.newRecord("update", k, obj, s.rv+1, old.created, s.clk.Now())
	if err != nil {
		return err
	}
	s.rv++
	s.unindex(old)
	s.put(rec)
	s.publish(&change{typ: store.Modified, rec: rec, prevLabels: old.labels})
	return nil
}

// Delete implements store.Store.
func (s *Store) Delete(ctx context.Context, k store.Key) error {
	if err := ctx.Err(); err != nil {
		return store.Errorf("delete", k, err, "")
	}
	if k.Kind == "" || k.Name == "" {
		return store.Errorf("delete", k, store.ErrInvalid, "missing kind or name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.objects[k.Kind][k.Name]
	if rec == nil {
		return store.Errorf("delete", k, store.ErrNotFound, "")
	}
	s.deleteLocked(rec)
	return nil
}

// deleteLocked removes rec, tells the watchers and drops a Run's transcript. The caller holds
// s.mu.
func (s *Store) deleteLocked(rec *record) {
	s.rv++
	s.unindex(rec)
	delete(s.objects[rec.key.Kind], rec.key.Name)
	s.publish(&change{typ: store.Deleted, rec: rec, rv: s.rv})
	if rec.key.Kind == runKind {
		s.deleteTranscript(rec.key.Name)
	}
}

// newRecord encodes obj as version rv. It sets rv and created on obj (restoring them if
// encoding fails), as Create and Update promise.
func (s *Store) newRecord(op string, k store.Key, obj store.Object, rv uint64, created, updated time.Time) (*record, error) {
	oldRV, oldCreated := store.ResourceVersion(obj), store.CreatedAt(obj)
	store.SetResourceVersion(obj, store.FormatVersion(rv))
	store.SetCreatedAt(obj, created)
	data, err := json.Marshal(obj)
	if err != nil {
		store.SetResourceVersion(obj, oldRV)
		store.SetCreatedAt(obj, oldCreated)
		return nil, store.Errorf(op, k, store.ErrInvalid, "encode: %v", err)
	}
	return &record{
		key:       k,
		typ:       reflect.TypeOf(obj).Elem(),
		data:      data,
		rv:        rv,
		created:   created,
		updated:   updated,
		labels:    maps.Clone(store.Labels(obj)),
		fields:    store.FieldValues(obj),
		retention: store.RetentionOf(obj),
	}, nil
}

// put stores rec and indexes it. The caller holds s.mu.
func (s *Store) put(rec *record) {
	m := s.objects[rec.key.Kind]
	if m == nil {
		m = map[string]*record{}
		s.objects[rec.key.Kind] = m
	}
	m[rec.key.Name] = rec
	if len(rec.fields) == 0 {
		return
	}
	byField := s.index[rec.key.Kind]
	if byField == nil {
		byField = map[string]map[string]map[string]struct{}{}
		s.index[rec.key.Kind] = byField
	}
	for f, vals := range rec.fields {
		byValue := byField[f]
		if byValue == nil {
			byValue = map[string]map[string]struct{}{}
			byField[f] = byValue
		}
		for _, v := range vals {
			names := byValue[v]
			if names == nil {
				names = map[string]struct{}{}
				byValue[v] = names
			}
			names[rec.key.Name] = struct{}{}
		}
	}
}

// unindex removes rec from the field indexes. The caller holds s.mu.
func (s *Store) unindex(rec *record) {
	byField := s.index[rec.key.Kind]
	for f, vals := range rec.fields {
		for _, v := range vals {
			names := byField[f][v]
			delete(names, rec.key.Name)
			if len(names) == 0 {
				delete(byField[f], v)
			}
		}
	}
}

// decode returns a new *typ holding the record's object.
func (r *record) decode() (reflect.Value, error) {
	v := reflect.New(r.typ)
	if err := json.Unmarshal(r.data, v.Interface()); err != nil {
		return reflect.Value{}, err
	}
	return v, nil
}
