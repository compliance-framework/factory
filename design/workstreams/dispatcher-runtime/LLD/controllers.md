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

Implemented in RT-S2-T04 as `ProviderController` (the built-in `factory` provider is `FactoryProvider`, RT-S2-T01): see [api-server.md §Decisions taken in RT-S2-T04](api-server.md#decisions-taken-in-rt-s2-t04).

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

## Decisions taken in RT-S1-T02

- **Constructor, not struct literal.** `NewController(name, rec, Options{Workers, Clock, Backoff, Logger})` creates the `Queue`, so keys can be enqueued before `Run`. `Watch(s, kind, keyFn, InformerOptions)` takes no context: it registers an informer that `Run` starts and stops with the workers, so one context owns the whole controller. `Watch` after `Run` is an error.
- **Queue semantics** follow client-go: a dirty set dedupes, a key added while it is being processed is queued again on `Done`, and `AddAfter` keeps the earlier deadline. `ShutDown` drops the keys still queued, and `Run` returns once the in-flight reconciles and the informers have returned. A reconcile that fails because its context was cancelled is not retried.
- **Backoff:** 5 ms → 5 min, with up to 10% jitter by default. A `Result.RequeueAfter` resets it.
- **Panics:** recovered, logged with their stack, retried with backoff, and counted on `Controller.Panics()`. That counter is `controller_panics_total` once metrics are wired, in RT-S2-T04.
- **Clock:** `internal/clock.Clock` gained timers: `NewTimer(d)` and `NewTimerAt(deadline)` return a `clock.Timer{C(), Stop()}`. `clock.Fake` fires its timers on `Advance` and `Set`, and has `Timers()` and `BlockUntil(n)` for tests. The controllers, the queue and the informer all take a `clock.Clock`. Deadlines are absolute (`NewTimerAt`), so a fake advance between reading `Now` and arming a timer can't push the timer later. The store still only calls `Now`.
- **Informer:** lists in pages and watches from the **first page's** `ListMeta.ResourceVersion`. Changes made while the later pages are read are then replayed, so a handler may see an object twice. On `ErrWatchExpired` it re-lists at once and counts it (`Resyncs()`). Other list or watch errors back off from 100 ms to 30 s. `Synced()` closes after the first list.
- **catalog-sync runs on the runtime:** one key (`catalog`) and one worker. Polling is `Result{RequeueAfter: interval}` (default 60 s). `Resync()` is the `resync-catalog` command: it reloads the head even when it is unchanged or was rejected. A source that can't be read returns an error and is retried with backoff from 1 s, capped at the interval. A rejected revision is not loaded again until the head moves.
- **`CatalogSource`** is `Head(ctx)` + `Load(ctx, revision)` + `String()`. There are three implementations:
  - `GitSource{Repo, Ref, Path}` (defaults: the factory repo, `main`, `catalog`) calls `gitsource.Head` and `gitsource.Load(repo, <sha>, path)`. All git calls are in `internal/catalog/gitsource`:
    - `gitsource.Head(ctx, repoURL, ref)` runs `git ls-remote -- <repo> <ref> <ref>^{}`, with the same locked-down environment as `Load` (`GIT_ALLOW_PROTOCOL`, no prompts, no system config). It prefers a branch, then a peeled tag, then a tag; a full commit SHA is returned as is.
    - Every gitsource error has the URL's user info redacted and still wraps `ErrGit` (or the context's error). `gitsource.RedactURL` is used for logs and status.
  - `DirSource` (`--catalog-dir`) polls a hash of the tree, the same `dir:<sha256>` that `LoadDir` computes, instead of using fsnotify, which isn't on the allow-list.
  - `FakeCatalogSource`, for tests.
- **Readers:** `CatalogProvider{Current()}`. `CatalogSync.Current()` is an `atomic.Pointer[catalog.Catalog]` load. **Before the first good catalog it returns nil** and `Ready()` stays open; startup waits on `Ready()`, so controllers started after it never see nil. A later failure never clears the catalog.
- **Notification:** `Snapshot()` returns the live catalog and a channel that is closed at the next swap (a broadcast; no subscriber registry, nothing blocks the swap). The LLD names no `factory.catalog.*` event, so none is emitted.
- **Status:** `CatalogStatus` is kept in memory; it isn't a store record, because config never goes in the store. `Status()` returns a copy for the API server. Its fields:
  - `Ready`, `LastGoodSHA`, `LoadedAt`, `LastAttemptSHA`, `LastSuccess`, `LastFailure`;
  - `Errors`: the error-severity `catalog.Error` diagnostics of the last rejected load, bounded (default 50) with `ErrorsOmitted`;
  - `Warnings`: the live catalog's warnings, bounded the same way;
  - `SourceError`: the last git or filesystem failure, redacted.

  Validation errors are logged at warn level with a count and the first three diagnostics.

## Decisions taken in RT-S2-T01

- **Wiring.** `NewRuleController(store, CatalogProvider, ConnSource, RuleOptions)`, `NewDebounceController(rule)`, `NewRunController(store, CatalogProvider, *FactoryProvider, RunOptions)` and `NewWorkItemController(store, CatalogProvider, ConnSource, *FactoryProvider, WorkItemOptions)`. Each has `Run(ctx)` and `Controller()`. `ConnSource{Conn(name)}` is what the provider controller implements; until it exists, `StaticConns` is used. The rule and work-item controllers share an `ObservationCache` (30 s TTL), which resync refreshes.
- **Built-in `factory` provider.** `FactoryProvider.Emit(FactoryEvent{Type, WorkItem, Key, Facts, Actor})` stores the Event `factory/<Key>`. It accepts only the vocabulary's `factory.*` types. Emitting a key again returns the stored event, so emitters can retry. `EmitCommand` is for the `factory.command.*` commands of RT-S2-T03.
- **Processed mark.** `v1alpha1.Event` has no status, so processing creates an internal `EventStatus` record named by the event id. It is deleted with its Event: the informer delivers the deletion and the reconcile finds no Event.
- **Work item creation.** The probe evaluation uses the would-be item's view (its object observed, no state) instead of a fully empty one, because rules read `item.pr` (`review-human-prs`) and would fail on an empty view. The id is deterministic, `wi_<hash(provider/ref)>`, so two events about one new object create one item. The creating event's actor is the reporter. An event without an object links its repo.
- **View.** Every link is observed through the cache, not only the primary one, because `item.prs` needs the stack's PRs. `item.pr` is the primary link's PR, else the last linked PR. `item.deploy` comes from the primary link first. `RunsBySpec` holds the counted runs: completed, failed and cancelled; abandoned and lost runs are left out.
- **Rails** belong to the rule controller (`storeRails`). They are built per evaluation and `Check` does no I/O.
  - Kill switch: the `factory:stop` label on any linked object, or `KillSwitches.Paused(scope, name)` for global, provider, queue or work item. The pause kill-switches come with RT-S2-T03's `pause`/`resume` command, which stores the flags; `StoreKillSwitches` (store-backed, internal kind `KillSwitch`) is the default since RT-S2-T03.
- **Dry-run.** `RuleController.DryRun(ctx, event, workItem)` (RT-S2-T03) evaluates an event with the rule controller's view and real rails, applying and writing nothing; it backs `POST /catalog/dry-run`.
  - Run cap: 20 tasks per item.
  - Single active run per TaskSpec. An allowed `cancel` frees its TaskSpec for a later `dispatch` in the same evaluation.
  - Budget: the queue's daily `BudgetCounter`. When it is spent the verdict is `Deferred{budget}`, and the Task is created Pending with that reason. RuleSet budgets aren't checked here, because `Rails.Check` gets no RuleSet: they are checked at lease time by the scheduler (RT-S2-T02).
  - Tasks the same event created don't count, so a retried reconcile reaches the same verdicts.
- **Work item lock.** Every holder of `Lock("wi/<id>")` (rule, debounce, run finish, retention) goes through `withItemLock`, which runs the locked section under a context whose deadline is 3/4 of the lease (`LockTTL`, default 2 m). The lease, which isn't renewed, therefore can't expire under a slow provider call. A section that hits the deadline fails with `ErrLockDeadline`, the lock is released, and the reconcile is retried.
- **Idempotency.** Each decision has a key `evt/<event>/<rule>/<i>`, where `i` counts the rule's decisions. The task id is `tsk_<hash(key)>`, and each notification destination gets `<key>/<n>`. A retried reconcile or a replayed event creates nothing again. `cancel` skips the tasks of the same event. Every rule-level effect and notification is also recorded as an `Effect`, labelled with the rule and with an empty `spec.task`.
- **Apply failures.** A dispatch that doesn't compile, a template that doesn't render, or an effect the provider rejects (anything but `ErrRetryable`) is recorded in the decision's `reason` (`apply failed: …`), and the other decisions still apply. Store errors and retryable provider errors fail the reconcile, which is then retried.
- **Task compilation.**
  - Inputs are the TaskSpec defaults overlaid with the dispatch inputs, then `ValidateInputs`.
  - Harness: the dispatch request, else the TaskSpec default.
  - Target: the dispatch target, else the primary link (repo taken from its ref).
  - Workspace `=` refs are evaluated with `inputs` and `task{id, target.repo}`.
  - Context, in order: `dispatch.context` (`event.object` becomes the object; `event.<fact>` becomes its ref or the event's content ref), the primary link, then the workspace repos mounted at `/work/<name>`. Everything but the repos is `untrusted`.
  - The prompt is rendered with `{task, inputs, item{id, state, links}, context}`. There is no blob store, so it is stored as an internal `TaskPrompt` record, and `prompt.ref` is `api://tasks/<id>/prompt`.
  - Skills resolve through `SkillResolver`. The default is `KeepSkills`; `factory serve` wires `GitSkills{Base}` (`gitsource.Head`, cached).
  - Credentials request: the `rw` mount (else the target repo), the other mounts as read repos, TTL `min(1h, maxWallClock + 10m)`.
  - Requires: the TaskSpec's `requires.labels` merged with the dispatch's, sorted and deduplicated, go in `Task.spec.requires` (unset when there are none), for the scheduler's label match.
- **Notify.** The body comes from the destination provider's type. `item` destinations post `comment` (`pr.comment` on PRs) on the links. A `channel` posts `comment` on `{kind: channel}`, and a `group` posts `comment` on `{provider: via, kind: group}` with `args.as`. Block Kit bodies aren't supported yet (recorded as a failure). Templates see `event`, `actor`, `item{id, state, links, counters}`, `run`, `approval` and `links.ui` (`<UIBaseURL>/workitems/<id>`). Rule-level notifications and effects act as the `author` identity.
- **Debounce.** `DebounceController` watches internal `Debounce` records named `dbn_<hash(key)>`; the first match of a key wins. The queue's delayed requeue at `Until` is the timer. When it fires it re-checks the rails, applies the held actions and records one decision.
- **Runs.**
  - The Run is created from the Task watch when the task is Leased or Running (`run_<task>-<n>`, attempt = `attempts + 1`), and `RunAPI.LeaseIssued` creates it at once.
  - How a run ended is `Run.status.end` (set with `endedAt`; an open run has none): `completed` when the agent returned a valid Result (whatever its status), `failed` for a timeout, an invalid result or a final infrastructure failure, `cancelled`, `abandoned` (given back) or `lost` (the task left the run without a word). Completed, failed and cancelled runs count towards `item.count`; abandoned and lost ones don't.
  - `RunAPI` is `LeaseIssued`, `Complete(taskID, Result)`, `Abandon`, `CancelRequested` and `AckCancel`, keyed by task id; the API server maps a lease to its task.
- **Results.**
  - A result is valid when its task matches, its status is known, its outcome is one of the TaskSpec's outcomes (required when the status is `done`), and it passes the output schema.
  - An invalid result fails the task with `invalid-result`. It isn't stored as the run's Result but as an `invalid-result` progress event. A valid result with status `failed` fails the task with `failed`, but its run is `completed`, with the Result stored.
  - `run.result` is the Result's JSON with the `data` fields mirrored at the top level (catalog-poc: `run.result.plan`).
  - The event is `factory/run/<run>`, with facts `run{id, spec, outcome, status, inputs, result, agent, harness{name, model}, reason?}`.
- **Counters.** A counted run increments the counter once, right after the run is marked ended. A retry that finds the run already ended skips the count, so the worst case is an under-count, never a double count.
- **Timeout.** The task is requeued for `StartedAt + maxWallClock + 2m`, then fails with `timeout` (a Cancelling task becomes Cancelled). Revoking the lease is RT-S2-T02's: the scheduler sees the task leave Leased/Running.
- **Cancel.** A Cancelling task without an agent is Cancelled at once, with no run event. With an agent, `CancelRequested` is true until `AckCancel`, which cancels the task, marks the run `cancelled` and emits `factory.run.failed{status: cancelled}`.
- **Abandon.** `throttled` returns the task to Pending (`agent-throttled`) without an attempt and marks the run abandoned. `agent-error` maps to `infra-failure` and `shutdown` to `agent-lost`: retried (Pending, attempts + 1) while the queue's retry policy allows, else Failed with that reason and `factory.run.failed`. Lease expiry stays the scheduler's; the run controller closes the open run of a task that went back to Pending (`lost`) or that something else failed (and emits `factory.run.failed`).
- **Work-item controller.** One key per item.
  - Projection: written when its digest changed (state, counters, links; no labels, since none are defined yet).
  - Resync: when due (the shortest Provider `resync.every`, default 10 m). The first observation after start is a baseline and emits nothing, because the rebuild already read that state. A changed observation (ignoring `updatedAt` and projections) emits `factory.workitem.resynced` with key `resync/<wi>/<digest>` and facts `resync.changed[]`.
  - Retention, in the same pass: an item whose links are all closed, merged or gone, with no active task and whose last activity (creation, decisions, tasks, run ends) is older than the retention, is deleted under its lock after a re-check.
- **Rebuild.** `Rebuild(ctx)` (bounded at 5 m) lists each RuleSet's scope on each bound provider, observes each object, and merges the projections per work item id: links are united (plus the object itself), counters take the maximum, and state comes from the most recently updated object. Items already in the store are left alone.
