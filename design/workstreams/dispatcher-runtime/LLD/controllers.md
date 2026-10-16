# LLD — Controllers (RT)

> Workstream: [RT](../../definitions.md#rt-dispatcher-runtime) · Tasks: RT-S1-T02, RT-S2-T01 · Package: `internal/controllers`
> Architecture: [02 §Controllers](../../../02-architecture.md#controllers)

## Controller runtime

```go
type Reconciler interface { Reconcile(ctx context.Context, key string) (Result, error) }
type Result struct { RequeueAfter time.Duration }

type Controller struct {
    Name   string
    Queue  workqueue                 // dedupes keys; rate-limited retries (5ms→5m exp backoff, jitter)
    Rec    Reconciler
    Workers int                      // default 2
}
func (c *Controller) Watch(ctx context.Context, s store.Store, kind string, keyFn func(store.Object) []string)
func (c *Controller) Run(ctx context.Context) error
```

- A key is processed by at most one worker at a time.
- `Reconcile` must be **idempotent**: it re-reads state, never trusts the triggering event alone (P4).
- Panics in `Reconcile` are recovered, logged, counted (`controller_panics_total`) and retried.
- A `Manager` starts all controllers, the store sweeper and the server; stops them on context cancel.

## Controllers

### catalog-sync (RT-S1-T02)

| | |
|---|---|
| Watches | timer (default 60 s) + `POST /commands {type: resync-catalog}` |
| Does | `git ls-remote <repo> <catalog-ref>` (branch or tag as configured; default `main`); if SHA changed → `catalog.LoadGit(repo, <that SHA>)` → if no errors swap `atomic.Pointer[catalog.Catalog]`; else keep old, publish status `{lastGood, lastAttempt, errors}` |
| Config | `--catalog-repo` (default `https://github.com/compliance-framework/factory`), `--catalog-path catalog`, `--catalog-ref main`; `--catalog-dir` for local dev (fsnotify reload) |

### provider

Creates one `Conn` per Provider in the catalog (re-created when the Provider object's digest changes). Mounts `Conn.Handler(sink)` at the webhook path, where `sink.Accept` stores a `Delivery` record before the handler acknowledges. Watches `Delivery` (status `received`) → `Conn.Normalise` → `store.Create(Event)` per output (IDs `<delivery>/<n>`) → marks the delivery `normalised`. On startup calls `Conn.Recover`. Also owns the built-in **`factory` provider**: an in-process emitter `EmitFactoryEvent(type, workItem, data)` used by other controllers; it's the only writer of `factory.*` Events.

### rule (RT-S2-T01)

| | |
|---|---|
| Watches | `Event` (added) |
| Does | 1) find/create WorkItem (`links` index; create when no match and ≥1 rule would match — the engine is called with an empty view first); 2) build `WorkItemView` (store + `Conn.Observe` of the primary link, cached 30 s); 3) `engine.Evaluate`; 4) **apply decisions in order** inside a store transaction-like sequence guarded by `Lock("wi/<id>")`: `Set` → WorkItem.status.state; `Dispatch` → create `Task{phase: Pending}` (with `status.reason: budget` when the rails deferred it; there is no separate deferred phase) with compiled Task spec (TaskSpec + inputs + resolved prompt ref + context refs + credentials request); `Cancel` → mark active Tasks of that spec `Cancelling`; `Link` → add link; `Notify` → render Template per destination and `Conn.Apply(comment/message)`; `Effect` → `Conn.Apply`; `Debounce` → upsert internal `Debounce` object; 5) append Decisions (bounded) to WorkItem; 6) mark Event processed |
| Idempotency | Event has `status.processed`; reprocessing a processed event is a no-op. Effects carry idempotency keys `evt/<eventID>/<rule>/<i>` |

Task compilation (part of `Dispatch` apply):

1. Merge TaskSpec defaults with dispatch `inputs`, `harness`, `requires`; validate inputs against TaskSpec input schema (runtime types).
2. Evaluate TaskSpec `=` fields (`workspace.repos[].ref`) with `inputs`/`task`.
3. Render the prompt: TaskSpec `instructions.template` (Go `text/template`) with `inputs`, `item` summary and **context refs** (never inline untrusted bodies; the agent fetches them via `/tasks/{id}/context/{n}`), store as blob `prompt/<taskID>`, digest it.
4. Resolve skills: `ref` tag → commit (`git ls-remote`, cached).
5. Credentials request (not the credential): provider, identity, rw repo, read repos, permissions, TTL — minted at **lease** time.

### work-item (RT-S2-T01)

| | |
|---|---|
| Watches | `WorkItem` (modified: state/links/counters), resync timer (Provider `resync`, default 10 m) |
| Does | **Projection:** for each link on a projecting provider, `Conn.Project(ref, Projection{...})` when the projection digest changed. **Resync:** `Conn.Observe` each open link; if observation changed (e.g. PR merged by a human), emit `factory.workitem.resynced`. **Rebuild** (on start, before the rule controller starts): for each RuleSet scope `Conn.List` → parse marker → recreate WorkItems (state, counters, links) |
| Retention | Owns WorkItem retention, since the store can't tell whether a link is open: deletes a WorkItem whose links are all closed once its last activity is older than `--retention` (default 90 days). The store's sweeper never deletes WorkItems. |
| Marker | `<!-- factory:wi v=1 id=… state=k:v,… runs=k:n,… links=prov:ref,… -->` (URL-encoded values; full spec in GH LLD) |

### run (RT-S2-T01)

| | |
|---|---|
| Watches | `Task` (phase changes), `Run` (modified), timer (1 min) |
| Does | Lease issued → create `Run`; `complete` → validate `Result` against TaskSpec output schema & outcomes (invalid → `failed{invalid-result}`), set Task `Succeeded/Failed`, update WorkItem counters, emit `factory.run.completed` (or `.failed`) with `{spec, outcome, status, inputs, result, agent, harness}`; wall-clock (`limits.maxWallClock` + 2 min grace) exceeded → mark `Failed{timeout}` and revoke lease; `Cancelling` → tell agent via heartbeat response `{cancel: true}`; `abandon{throttled}` → Task back to `Pending` (not counted as attempt) |

### debounce (part of rule controller)

Timer wheel over `Debounce` objects; at `Until` re-evaluates the held intent's rails and applies it.

## Ordering and startup

```text
start store → catalog-sync (wait first good catalog) → provider (connect) → work-item rebuild (blocking, bounded 5 min)
→ rule, work-item, run, scheduler controllers → HTTP server (agents may now lease)
```

Webhooks received during rebuild are accepted and stored as Events; the rule controller processes them after rebuild.

## Tests

- In-process cascade with `provider/fake`: `issue.opened` → triage Task → fake agent completes → `factory.run.completed` → follow-up decision.
- Rebuild: fake provider pre-loaded with markers → identical WorkItems after restart.
- Human merge out of band → resync emits `factory.workitem.resynced` → merge rule doesn't double-merge (observes merged).
- Idempotency: the same Event delivered twice → one Task, one comment.
