# LLD — Resource store (RT)

> Workstream: [RT](../../definitions.md#rt-dispatcher-runtime) · Task: RT-S1-T01 · Package: `internal/store` (+ `store/memory`, `store/storetest`)

## Interface

```go
type Object = v1alpha1.Object // GetTypeMeta().Kind, GetName(); metadata through accessors (below)
type Key struct { Kind, Name string }

type Store interface {
    Get(ctx context.Context, k Key, out Object) error                                  // ErrNotFound
    List(ctx context.Context, kind string, opts ListOptions, out any) (ListMeta, error) // out: *[]T, *[]*T or *[]Object
    Create(ctx context.Context, obj Object) error                                       // ErrAlreadyExists; sets ResourceVersion, CreatedAt
    Update(ctx context.Context, obj Object) error                                       // CAS on ResourceVersion → ErrConflict
    Delete(ctx context.Context, k Key) error                                            // ErrNotFound
    Watch(ctx context.Context, kind string, opts WatchOptions) (Watcher, error)
    Lock(ctx context.Context, name string, ttl time.Duration) (Unlock, error)           // advisory, lease-based; waits until free or ctx ends
}

type ListOptions struct { LabelSelector *v1alpha1.LabelSelector; Field map[string]string; Cursor string; Limit int }
type ListMeta struct { NextCursor string; ResourceVersion string } // NextCursor empty on the last page
type WatchOptions struct { LabelSelector *v1alpha1.LabelSelector; SinceVersion string; Buffer int }
type Watcher interface {
    Events() <-chan WatchEvent   // closed when the watch ends (ctx done, Stop, or expiry)
    Err() error                  // after Events() is closed: nil, context error, or ErrWatchExpired
    Stop()
}
type WatchEvent struct { Type WatchEventType /* added|modified|deleted */; Object Object; Version string }
type Unlock interface {
    Unlock()                                            // idempotent; never releases a lock someone else reclaimed
    Renew(ctx context.Context, ttl time.Duration) error // ErrLockLost once expired or released
}

type Sweeper interface { Sweep(ctx context.Context) (SweepResult, error) }
type TranscriptStore interface {
    PutTranscript(ctx context.Context, run string, blob []byte) error // ErrTooLarge over 20 MB
    GetTranscript(ctx context.Context, run string) ([]byte, error)   // ErrNotFound
}
```

Errors (`errors.Is`; most come as `*store.Error{Op, Key, Detail, Err}`): `ErrNotFound`, `ErrAlreadyExists`, `ErrConflict`, `ErrInvalid`, `ErrWatchExpired`, `ErrLockLost`, `ErrTooLarge`. Accessors: `ResourceVersion`, `SetResourceVersion`, `CreatedAt`, `SetCreatedAt`, `HasResourceVersion`, `Labels`; internal kinds outside v1alpha1 implement `MetaObject{GetObjectMeta() *v1alpha1.ObjectMeta}`. Label selectors use `v1alpha1.LabelSelector.Matches`/`Validate`, shared with the catalog.

Kinds stored: `Delivery` (raw provider webhook, pre-normalisation), `Event`, `WorkItem`, `Task`, `Run`, `Agent`, `Effect`, `BudgetCounter`, `Lease` (internal), `Debounce` (internal), `Session` (UI, internal). Config kinds live in the `Catalog` snapshot, **not** in the store.

## In-memory implementation

- One `map[kind]map[name]stored` guarded by a single `sync.RWMutex` (PoC scale: thousands of objects).
- `ResourceVersion` = global monotonic counter (`uint64`, decimal string).
- Objects are **deep-copied** on write and read (JSON round-trip helper; generated `DeepCopy` later if needed).
- **Field indexes** (maintained on write): `Task` by `status.phase`, `spec.queue`, `metadata.workItem`; `Run` by `task`; `WorkItem` by every `links[].provider+ref`; `Effect` by `idempotencyKey`.
- **Watch:** fan-out to subscriber channels with a bounded buffer (default 256). Sends never block the writer: if a subscriber's buffer is full, the store sets its `Err()` to `ErrWatchExpired` and **closes its channel**. Closing always succeeds, so expiry is always observable. Consumers loop until the channel closes, then check `Err()`; on `ErrWatchExpired` the controller runtime re-lists (resync) and starts a new watch. `SinceVersion` replays from a ring buffer of the last 10 000 events; if it's too old, `Watch` returns `ErrWatchExpired` immediately.
- **Locks:** `map[name]{owner, expires}`; `Unlock` is idempotent; expired locks are reclaimable.

## Retention

A sweeper goroutine (every 10 min) deletes:

| Kind | Retention | Rule |
|---|---|---|
| `Run`, transcripts (blob map), `Event`, `Effect` | **90 days** (Q13, configurable `--retention`) | by `CreatedAt` |
| `Task` | 90 days after terminal phase | `Succeeded/Failed/Cancelled` only |
| `WorkItem` | never deleted while any link is open; 90 days after last activity otherwise | |

Transcripts are stored as compressed blobs in a separate `map[runID][]byte`, capped at 20 MB each and 2 GB total (oldest evicted first, logged).

## Postgres later

`store/storetest` holds a **conformance suite** (`func Run(t *testing.T, newStore func(store.Options) storetest.Subject)`) covering CAS conflicts, watch ordering, resync, selectors, locks and retention. A future Postgres store must pass the same suite.

## Tests

- Conformance suite on `memory` under `-race`.
- Concurrency: 50 goroutines updating the same object → exactly one wins per version.
- Watch slow-consumer → channel closed, `Err() == ErrWatchExpired`, then successful resync; writer never blocks.

## Decisions taken in RT-S1-T01

- **`store.Object = v1alpha1.Object`**, not a store-specific interface: v1alpha1 kinds carry no `GetResourceVersion`/`SetResourceVersion`, so the store reaches metadata through accessors (a type switch over the runtime kinds, plus `MetaObject` for internal kinds).
- **Event** has no metadata: the store keeps its version and creation time beside the object, and `Update` on an Event is unconditional (no CAS). Every other kind's `Update` requires the stored `resourceVersion`; an empty one is `ErrConflict`.
- **`List` returns `ListMeta{NextCursor, ResourceVersion}`**: the cursor for pagination (keyset, name order, opaque), and the store version at list time, so a consumer can list then `Watch(SinceVersion: meta.ResourceVersion)` with no gap (resync).
- **`Unlock` is an interface with `Renew`.** A lease that has expired is lost: `Renew` returns `ErrLockLost` even if nobody reclaimed it, and the old holder's `Unlock` is a no-op.
- **Retention and transcripts are separate interfaces** (`Sweeper`, `TranscriptStore`), not part of `Store`; `store.RunSweeper(ctx, s, interval, log)` is the ticker helper (default 10 min).
- **Suite signature:** `storetest.Run(t, func(store.Options) storetest.Subject)`, with `Subject = Store + Sweeper + TranscriptStore`. The suite injects a fake clock and small caps, buffers and history sizes through `Options`.
- **Retention specifics:** Event by the store's creation time; Task by its last write in a terminal phase (`Succeeded/Failed/Cancelled`; there is no terminal timestamp); `BudgetCounter` swept at 90 days with Runs, Events and Effects; Agent, Approval and the internal kinds kept; a Run's transcript is deleted with the Run (and transcripts also age out on their own). WorkItems are never swept by the store: the work-item controller owns their retention (it knows whether links are open).
- **`--retention`** is wired by `factory serve` into `store.Options.Retention`; the store only takes the option.
- **Clock:** the store takes an `internal/clock.Clock` (`Real`, `Fake`). `rules.Clock` has the same shape and should converge on it.
