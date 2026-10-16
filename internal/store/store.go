// Package store is the dispatcher's resource store: the runtime kinds (Event, WorkItem, Task,
// Run, Agent, Effect, BudgetCounter, Approval, and the internal Lease, Debounce, Session and
// Delivery records) with get, list, watch, compare-and-swap updates, advisory leased locks,
// retention and the transcript blobs.
//
// Config kinds live in the catalog snapshot, never here. The in-memory implementation is in
// store/memory; store/storetest is the conformance suite every implementation must pass.
//
// Design: design/workstreams/dispatcher-runtime/LLD/store.md.
package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// Object is any stored kind. The store reads its kind from TypeMeta and its name from GetName
// (metadata.name, or the id for Task and Event); ResourceVersion, CreatedAt and Labels reach
// its metadata (see meta.go).
type Object = v1alpha1.Object

// Key names one stored object.
type Key struct {
	Kind, Name string
}

func (k Key) String() string { return k.Kind + "/" + k.Name }

// KeyOf returns obj's key.
func KeyOf(obj Object) Key { return Key{Kind: obj.GetTypeMeta().Kind, Name: obj.GetName()} }

// Store is the resource store.
type Store interface {
	// Get decodes the object at k into out, which must be a pointer to the kind's Go type.
	// ErrNotFound when there is none.
	Get(ctx context.Context, k Key, out Object) error
	// List decodes the objects of kind that match opts into out, a *[]T where T is the kind's
	// Go type, a pointer to it, or an interface it implements (Object). Objects come in name
	// order; ListMeta.NextCursor continues the list when opts.Limit cut it short.
	List(ctx context.Context, kind string, opts ListOptions, out any) (ListMeta, error)
	// Create stores a new object. It sets obj's ResourceVersion and CreatedAt (from the
	// store's clock) on the stored copy and on obj. ErrAlreadyExists when the key is taken.
	Create(ctx context.Context, obj Object) error
	// Update replaces an object if obj's ResourceVersion is the stored one (compare-and-swap),
	// else ErrConflict; ErrNotFound when there is none. It sets the new ResourceVersion on obj
	// and keeps the stored CreatedAt. Kinds without a resourceVersion field (Event) are
	// replaced unconditionally.
	Update(ctx context.Context, obj Object) error
	// Delete removes an object. ErrNotFound when there is none.
	Delete(ctx context.Context, k Key) error
	// Watch streams the changes to kind's objects that match opts, in version order.
	Watch(ctx context.Context, kind string, opts WatchOptions) (Watcher, error)
	// Lock takes the advisory lock name for ttl, waiting while another holder has it and it
	// hasn't expired. It returns ctx's error (wrapped) if ctx ends first. An expired lock is
	// reclaimable by anyone.
	Lock(ctx context.Context, name string, ttl time.Duration) (Unlock, error)
}

// ListOptions select and page a List.
type ListOptions struct {
	// LabelSelector keeps the objects whose labels match; nil or empty matches all.
	LabelSelector *v1alpha1.LabelSelector
	// Field keeps the objects whose indexed field has the value (all entries must match). The
	// fields are per kind: see IndexedFields. An unknown field is ErrInvalid.
	Field map[string]string
	// Cursor is a previous page's ListMeta.NextCursor; empty starts at the beginning.
	Cursor string
	// Limit caps the page size; 0 means no limit.
	Limit int
}

// ListMeta describes a List result.
type ListMeta struct {
	// NextCursor continues the list; empty on the last page.
	NextCursor string
	// ResourceVersion is the store's version when the list was read: pass it as
	// WatchOptions.SinceVersion to watch from exactly where the list left off (resync).
	ResourceVersion string
}

// WatchOptions select a Watch.
type WatchOptions struct {
	// LabelSelector keeps the objects whose labels match. An object that stops matching is
	// sent as Deleted, one that starts matching as Added.
	LabelSelector *v1alpha1.LabelSelector
	// SinceVersion replays the changes after this version before the live ones. Empty starts
	// with the next change. A version older than the store's history is ErrWatchExpired.
	SinceVersion string
	// Buffer is the channel buffer; 0 is the store's default. A watcher whose buffer is full
	// when a change is sent is expired (its channel closed, Err() == ErrWatchExpired): writers
	// never wait for readers.
	Buffer int
}

// Watcher is a running watch.
type Watcher interface {
	// Events is closed when the watch ends (ctx done, Stop, or expiry).
	Events() <-chan WatchEvent
	// Err is, once Events is closed: nil after Stop, the context's error, or ErrWatchExpired
	// (the consumer re-lists and starts a new watch).
	Err() error
	// Stop ends the watch. It is idempotent.
	Stop()
}

// WatchEventType is what happened to an object.
type WatchEventType string

// Watch event types.
const (
	Added    WatchEventType = "added"
	Modified WatchEventType = "modified"
	Deleted  WatchEventType = "deleted"
)

// WatchEvent is one change. Object is the watcher's own copy: the new state, or the last state
// for Deleted. Version is the change's resourceVersion (also set on Object when its kind has
// one).
type WatchEvent struct {
	Type    WatchEventType
	Object  Object
	Version string
}

// Unlock is a held lock (the LLD's Unlock, with renewal).
type Unlock interface {
	// Unlock releases the lock if this holder still has it. It is idempotent, and never
	// releases a lock someone else reclaimed after expiry.
	Unlock()
	// Renew extends the lease to now+ttl. ErrLockLost when the lease expired or was released.
	Renew(ctx context.Context, ttl time.Duration) error
}

// Sweeper deletes what is past retention (see Retention). Deletions are sent to watchers.
type Sweeper interface {
	Sweep(ctx context.Context) (SweepResult, error)
}

// SweepResult counts what a sweep deleted.
type SweepResult struct {
	// Objects is per kind.
	Objects     map[string]int
	Transcripts int
}

// TranscriptStore holds run transcripts as the compressed blobs the agent uploads (gzip JSON
// Lines), keyed by run name. Blobs are capped per transcript and in total; when the total cap
// is reached the oldest are evicted (logged). They are deleted with their Run and swept by
// age like Runs.
type TranscriptStore interface {
	// PutTranscript stores (or replaces) run's transcript. ErrTooLarge over the per-transcript
	// cap.
	PutTranscript(ctx context.Context, run string, blob []byte) error
	// GetTranscript returns a copy of run's transcript. ErrNotFound when there is none.
	GetTranscript(ctx context.Context, run string) ([]byte, error)
}

// Defaults.
const (
	DefaultRetention            = 90 * 24 * time.Hour
	DefaultSweepInterval        = 10 * time.Minute
	DefaultWatchBuffer          = 256
	DefaultHistory              = 10_000
	DefaultTranscriptMaxBytes   = 20 << 20
	DefaultTranscriptTotalBytes = 2 << 30
)

// Options configure a store implementation. Zero fields take the defaults.
type Options struct {
	// Clock sets CreatedAt and drives lock expiry and retention. Default clock.Real.
	Clock clock.Clock
	// Retention is how long Runs, transcripts, Events, Effects and BudgetCounters are kept
	// (and terminal Tasks after their last change). Default 90 days (`--retention`).
	Retention time.Duration
	// WatchBuffer is the default per-watcher buffer. Default 256.
	WatchBuffer int
	// History is how many changes are kept for WatchOptions.SinceVersion. Default 10 000.
	History int
	// TranscriptMaxBytes caps one transcript. Default 20 MB.
	TranscriptMaxBytes int64
	// TranscriptTotalBytes caps all transcripts. Default 2 GB.
	TranscriptTotalBytes int64
	// Logger logs evictions and sweeps. Default slog.Default().
	Logger *slog.Logger
}

// WithDefaults returns o with its zero fields set to the defaults.
func (o Options) WithDefaults() Options {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Retention <= 0 {
		o.Retention = DefaultRetention
	}
	if o.WatchBuffer <= 0 {
		o.WatchBuffer = DefaultWatchBuffer
	}
	if o.History <= 0 {
		o.History = DefaultHistory
	}
	if o.TranscriptMaxBytes <= 0 {
		o.TranscriptMaxBytes = DefaultTranscriptMaxBytes
	}
	if o.TranscriptTotalBytes <= 0 {
		o.TranscriptTotalBytes = DefaultTranscriptTotalBytes
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}
