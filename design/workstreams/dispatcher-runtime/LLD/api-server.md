# LLD — HTTP API server and `factory serve` (RT)

> Workstream: [RT](../../definitions.md#rt-dispatcher-runtime) · Tasks: RT-S2-T03, RT-S2-T04 · Packages: `internal/server`, `internal/auth`, `cmd/factory`
> Contract: [contracts.md §HTTP API](../../foundation/LLD/contracts.md#http-api)

## Server

- **Echo v4** (`github.com/labstack/echo/v4`, the same line as `compliance-framework/api`); handlers implement the oapi-codegen **Echo strict** server interface (`internal/apiserver/gen`), registered with its generated `RegisterHandlers`.
- Middleware chain, as Echo middleware: request ID → structured access log via `slog` (no bodies) → recover → auth (per route group: `/api/v1` agent routes, UI routes, CLI) → body limit (1 MB; transcripts 20 MB) → handler.
- One `HTTPErrorHandler` renders every error as `application/problem+json`.
- Plain `http.Handler`s are mounted with `echo.WrapHandler`: the embedded UI (`internal/webui.Handler()`) as the catch-all `GET /*` and `HEAD /*`, and each provider's `Conn.Handler(sink)` at its webhook path. They stay framework-agnostic so they're testable without Echo.
- SSE (`/stream`) and lease long-polls write to the underlying `http.ResponseWriter` (`c.Response()`), flushing per event; request contexts carry cancellation.
- The ops listener (`--ops-addr`) is a second, minimal Echo instance (recover + access log only), so both listeners share one middleware story.
- Routes outside the contract: `/hooks/{provider}` (provider `Handler()`), `/` (embedded UI via `internal/webui.Handler()`), and, **on a separate ops listener** (`--ops-addr`, default `127.0.0.1:9090`, never the public `--addr`): `/healthz`, `/readyz` (catalog loaded + rebuild done), `/metrics` (Prometheus text). The ops routes have no auth, so they must not be reachable through the public tunnel; binding them to localhost guarantees it.

## Auth

| Caller | Mechanism | Checks |
|---|---|---|
| Agent | `Authorization: Bearer <agentToken>` | token hash lookup → Agent; lease/task routes require the lease to belong to the agent |
| UI | GitHub OAuth web flow → server-side session cookie (`HttpOnly`, `Secure`, `SameSite=Lax`, 12 h) | `GET /user`; **commands** require membership of the catalog Group `maintainers` (resolved via `Conn.ResolveGroup`, cached 10 min — the CCF org admins in the PoC); any org member may read |
| CLI | `Authorization: Bearer <cliToken>` (static token from env, PoC) | full admin; for `factory dispatch` |
| Webhooks | provider HMAC (in `Conn.Handler()`) | — |

OAuth app config: `FACTORY_OAUTH_CLIENT_ID`, `FACTORY_OAUTH_CLIENT_SECRET` (a GitHub OAuth App, or the author GitHub App's user-to-server flow). CSRF: `state` param bound to a short-lived cookie.

## Effector API (`POST /tasks/{id}/effects`)

```text
1. auth: agent owns the active lease of task {id}
2. op ∈ task.spec.effects.allow                         else 403 effect-not-allowed
3. label ops: every label matches task.spec.effects.labels globs   else 403
4. identity := TaskSpec.identity; target must be the task target repo (or its linked objects) else 403
5. req.idempotencyKey must be present and non-empty (≤ 200 chars)  else 400 idempotency-key-required
   idem := hash(task.id, req.idempotencyKey); if Effect(idem) exists → return it (200, replay)
6. rate limits: ≤ 20 comments/run, ≤ 60 effects/run, and ≤ 30 comments per work item per rolling hour (all runs)   else 429
7. result := Conn.Apply(EffectRequest{op, target, args, identity, idem})
8. store Effect; append to Run.progress; broadcast SSE; return {effectId, url}
```

Git-side ops (`push-branch`, `stack.*`) are **recorded** (step 5–8 without `Conn.Apply`).

## Commands (`POST /commands`)

Body: `{type, target, args}`.

| `type` | `target` / `args` | Effect |
|---|---|---|
| `pause` / `resume` | `target: {scope: global\|provider\|queue\|workitem, name}` | sets kill-switch flags in the store (rails read them); emits `factory.command.pause/resume` |
| `cancel` | `target: {task}` | Task → `Cancelling` |
| `redispatch` | `target: {task}`, `args: {harness?}` | clones the Task with an optional harness override; new Task `Pending` |
| `dispatch` | `args: {event}` | validates + stores an injected Event (source must be a configured provider or `factory`), used by `factory dispatch` and the UI dry-run "run for real" |
| `resync-catalog` | — | triggers catalog-sync |

## SSE (`GET /stream?topics=…`)

- Hub with per-topic subscribers; topics: `runs/<id>`, `workitems`, `workitems/<id>`, `tasks`, `agents`.
- Events: `progress`, `workitem.updated`, `task.updated`, `agent.updated` (JSON); `id:` = store version for resume via `Last-Event-ID` (backfill from the run's progress ring buffer).
- Keep-alive comment every 15 s; max 100 subscribers (PoC).

## `factory serve`

```text
factory serve [--addr :8080] [--ops-addr 127.0.0.1:9090] [--catalog-repo URL --catalog-ref main | --catalog-dir ./catalog]
              [--retention 2160h] [--public-url https://…] [--log-format json|text]
```

Environment (names only; values from the secret store — see [rollout](../../poc-rollout/LLD/rollout.md#secrets)):

| Variable | Use |
|---|---|
| `FACTORY_SECRET_<NAME>_<KEY>` | resolves a catalog `secretRef{name, key}` (e.g. `FACTORY_SECRET_CCF_FACTORY_APP_PRIVATE_KEY`) |
| `FACTORY_OAUTH_CLIENT_ID`, `FACTORY_OAUTH_CLIENT_SECRET` | UI login |
| `FACTORY_PUBLIC_URL` | the public base URL (the tunnel), used for `--public-url`, the webhook URL and the OAuth callback; **not a secret** |
| `FACTORY_SESSION_KEY` | cookie signing (32 bytes) |
| `FACTORY_CLI_TOKEN` | CLI admin token |
| `FACTORY_POOL_JOIN_TOKEN_<POOL>` | via `secretRef` of the Pool |

Wiring order is in [controllers.md §Ordering](controllers.md#ordering-and-startup). Graceful shutdown: stop accepting leases, wait ≤ 30 s for in-flight requests, cancel controllers.

`factory dispatch --event file.json [--url --token]` posts `{type: dispatch, event}` to `/commands`.

## Tests

- Contract tests: the generated client against `httptest` server for every operation (happy + 401/403/404/409).
- Effector: allow-list, label globs, foreign target, idempotent replay, rate limit.
- OAuth: state mismatch, non-member, member read-only, admin commands.
- SSE: resume with `Last-Event-ID`.
- Integration (RT-S2-T04): `serve` + fake GitHub (`httptest`) + fake agent completes a task end-to-end.

## Decisions taken in RT-S2-T03

- **Packages.** `internal/server` implements every operation of `gen.StrictServerInterface` on Echo v4 over `server.Deps{Store, Transcripts, Catalog, CatalogStatus, Resync, Conns, Sink, Scheduler, Factory, KillSwitches, Evaluator, Auth}`; the scheduler, the dry-run evaluator, catalog-sync and the kill switches are small interfaces (`*scheduler.Scheduler`, `*controllers.RuleController`, `*controllers.CatalogSync` and `controllers.StoreKillSwitches` satisfy them). `Handler()` is the public API (`/api/v1` and `/hooks/{provider}`); `Echo()` lets `factory serve` mount the UI catch-all; `OpsHandler()` is the ops Echo; `Shutdown(ctx)` ends the SSE streams. `internal/auth` holds the credentials, the OAuth web flow, the sessions, the Group checks and `GitHubOwners`, the scheduler's real `OwnerVerifier`.
- **Middleware.** Request ID (the caller's `X-Request-Id` when it is a sane token, else random) → access log (method, route template, path, status, size, duration, request id; never bodies or query strings, since the callback carries the code) → recover → per-operation auth → body limit. Auth and the body limit are registered per operation through the generated `RegisterHandlersOptions.OperationMiddlewares`, from one table (`operations`: operationId → access) that a test checks against the spec's `security` requirements, so the policy can't drift from the contract. The body limit is our own (`http.MaxBytesReader`, plus a `Content-Length` precheck): Echo's `middleware` package would pull in `golang.org/x/time`, which isn't on the allow-list. Strict handlers only get a `context.Context`, so the auth middleware puts the principal and the `*http.Request` (cookies, headers) in it.
- **Errors.** Handlers return Go errors; one `HTTPErrorHandler` maps them (typed `apiError`, Echo's `HTTPError`, `http.MaxBytesError`, and the scheduler, store, controllers, provider and auth sentinels) to `application/problem+json` with a stable `code` (`not-lease-holder`, `lease-gone`, `effect-not-allowed`, `label-not-allowed`, `target-not-allowed`, `idempotency-key-required`, `rate-limited`, `provider-retryable`, `oauth-state-mismatch`, `bad-return-to`, `not-a-member`, `not-admin`, `cross-site-request`, …), `instance` = the request id and `Retry-After` when there is one. 5xx details are logged, not returned.
- **Auth.**
  - Agent routes: the bearer token through `scheduler.Authenticate`; an agent acts only on itself (`403 not-your-agent` on `/agents/{id}` and the long poll for another id), and on lease, task, context, effect and transcript routes only while it holds the task's active lease (`403 not-lease-holder`; the scheduler's `ErrNotLeaseHolder` for lease routes).
  - The CLI token is compared in constant time over SHA-256 hashes and is a full admin: accepted on every UI-session route (reads, dry-run, stream, commands) except `getMe`, never on agent routes. The spec lists `cliToken` as an alternative security requirement on each of those operations.
  - UI sessions are store records (internal kind `Session`, as store.md lists) named by the SHA-256 of a random 32-byte id; the cookie is `<id>.<HMAC-SHA256(FACTORY_SESSION_KEY, id)>`, `HttpOnly`, `Secure`, `SameSite=Lax`, 12 h absolute. No GitHub token is kept: the callback exchanges the code, calls `GET /user` once and drops the token. An expired session is deleted when it is next presented (no sweeper: sessions are few in the PoC).
  - **Readers.** The LLD says "any org member may read" but names no Group for it. Reading needs the catalog Group `readers` (configurable) **or** `maintainers`; a Group missing from the catalog has no members, so until the catalog defines `readers` only maintainers can sign in (fail closed). **The catalog must define a `readers` Group**, e.g. `members: [{provider: github-public, orgRole: {org: compliance-framework, role: member}}]`; the PoC test catalog (`internal/catalog/testdata/poc/groups/readers.yaml`) defines one. The callback refuses a non-member with `403 not-a-member` and sets no session. Membership is re-checked per request through `ResolveGroup`, cached 10 min per member source (as the scheduler's owners check), so a removal takes effect within 10 minutes.
  - **CSRF.** The login `state` is 32 random bytes, bound to a `factory_oauth_state` cookie (path `/api/v1/auth`, 10 min) and kept server-side (bounded at 10 000 pending logins); the callback needs the cookie, the parameter and the pending login to match, once. Cookie-authenticated writes (commands, dry-run, logout) are also refused when their `Origin` isn't the public origin (`Options.PublicURL`, else the request's host) or `Sec-Fetch-Site` is `cross-site`, on top of `SameSite=Lax`.
  - `returnTo` must be a same-origin absolute path: no scheme, host, `//`, backslash or control characters (`400 bad-return-to`); the callback redirects to that path.
  - `GitHubOwners{Provider, APIBase, HTTPClient}` calls `GET <APIBase>/user`; a token GitHub refuses wraps `scheduler.ErrUnauthorized`, any other failure `auth.ErrOAuth` (502), and errors never carry the token.
  - `GET /auth/device-config` returns the OAuth client id and `DeviceScopes` (default `read:user`: owner verification needs `GET /user` only).
- **Leases.** `wait` must be whole seconds (`^[0-9]{1,2}s$`, default 30 s, capped at 60 s); no task is `204`. The git credential is copied into `gen.GitCredential` field by field (its own JSON is redacted); a task with no git credential request gets an empty credential, since the contract makes the field required. `CompleteLease` goes through `Scheduler.Complete`; its `transcriptRef` is set on the open run first (the upload already set it).
- **Effector.** The record name and the provider's idempotency key are both `eff_<sha256(task, key)[:20]>`, so replay is a `Get` and a concurrent duplicate loses the `Create` and returns the winner. Only applied effects are recorded: a failed `Apply` stores nothing, so the agent retries with the same key. Provider errors map to `429 provider-retryable` with `Retry-After` (contracts.md's refinements now say the same), `400` for invalid or unsupported, `404` for a missing target, `502` otherwise. Targets: the given ref, else the task's primary object (its repo for `create-issue`, `pr.open` and the git-side ops); allowed when it is the task's target repo, an object in that repo, or a link of the work item (which then gives the provider and kind). Rate limits count the Effector's own records (labelled `factory.ccf.dev/task`, `/run`, `/workitem`): ≤ 60 effects and ≤ 20 comments per run, ≤ 30 comments per work item in a rolling hour; comments are `comment`, `pr.comment`, `review.comment` and `thread.reply`; a replay is never limited. Each effect is appended to the run's progress (`kind: effect`), which the SSE stream carries.
- **Context items.** `provider.Conn` has no way to read an object's untrusted text (Observe deliberately carries none), so `getTaskContext` returns content only when the connection also implements `server.ContentReader`; otherwise the item is reference-only (`content: ""`). Untrusted content is fenced with a backtick fence longer than any run inside it and labelled `untrusted source="<provider>:<ref>" author="…"`. The GitHub adapter (GH-S1) must implement `ContentReader` (recorded in contracts.md's refinements).
- **Commands.**
  - `pause`/`resume` set and clear store-backed kill switches (internal kind `KillSwitch`, `global` or `<scope>:<name>`; resume deletes it). `controllers.StoreKillSwitches` is now the rule controller's default `KillSwitches`, so the rails (and the debounce controller, which shares the options) read them with no extra wiring. Factory events always carry a work item, so only the `workitem` scope emits `factory.command.pause/resume`; the other scopes emit none (`event` absent in the 202). The target must exist (catalog provider or queue, stored work item).
  - `cancel` CASes an active task (Pending, Leased, Running) to `Cancelling` (reason `cancelled`) and emits `factory.command.cancel`; the run controller cancels an agentless one, an agent learns it from its heartbeat. A finished task is `409 task-not-active`.
  - `redispatch` refuses an active task (`409 task-active`: cancel it first), then emits `factory.command.redispatch` and creates `tsk_<hash(task, request id)>`: the task cloned with its provenance pointing at that event, the harness request overridden when given, `resolved` cleared, a fresh Pending status, and its `TaskPrompt` copied.
  - `dispatch` stores the event as is (no factory provider involved): its source must be a catalog Provider whose type emits the event type, or `factory` with a `factory.*` type and a work item; an id-less event becomes `inject/<request id>`, `factory/…` ids are reserved for the factory; a taken id is `409`.
  - `resync-catalog` calls `CatalogSync.Resync()` (`503` when the server has no resyncer).
  - Command events are keyed by the request id, so a retried request is a new command.
- **Dry-run.** `RuleController.DryRun(ctx, event, workItem)` (added to `controllers`): the rule controller's view (store state, runs, observations through the shared cache) and its real rails, kill switches included, with nothing applied or written. An absent work item is the item the event would create. Rail verdicts map to the API's `Allowed`/`Suppressed`/`Deferred` (an unsupported action is `Suppressed`), with the rail name prefixed to the reason.
- **Lists.** Each list reads the matching objects (store label selector, or the catalog filtered with `LabelSelector.Matches`), sorts them by a key that ends with the name, and pages by keyset: the cursor is `store.EncodeCursor(kind, <last sort key>)`, so it stays valid as objects come and go and a cursor of another list is `400`. Orders: work items most recently written first (the resourceVersion is the store's global write counter), tasks newest first, everything else by name. Selector strings are parsed as Kubernetes' (`a=b`, `a==b`, `a!=b`, `a`, `!a`, `a in (x,y)`, `a notin (x,y)`). The work item timeline's events are those `EventStatus` records attribute to it plus its decisions' events (a full scan of `EventStatus`, fine at PoC scale).
- **SSE.** Each stream (at most 100; `503 too-many-subscribers`) opens its own store watches, one per kind it needs, instead of a shared hub: `Last-Event-ID` then simply becomes the watch's `SinceVersion` (when the store's history no longer has it, the stream starts from now), and a watch that expires because the client is slow is reopened from the last version it delivered. Run progress is diffed against the last ring buffer the stream saw (from the run read after the watch opened, so nothing falls in between); a resume backfills the whole ring. Frames are `event:`/`id: <store version>`/`data: <JSON>`; deletions send nothing; a `: connected` comment flushes the headers and `: keep-alive` follows every 15 s (injected clock). The stream writes through an `io.Pipe` into the generated `text/event-stream` response, which flushes per chunk, and ends cleanly when the request context ends or `Shutdown` is called.
- **Ops.** `/healthz` is `ok`; `/readyz` is `503` until catalog-sync reports ready and `Options.Ready` (the startup rebuild) is closed; `/metrics` is the Prometheus text format written by hand (no client library on the allow-list): `factory_tasks{phase}`, `factory_agents{phase}`, `factory_sse_subscribers`, `factory_ready`, `factory_http_requests_total{code}`. The ops Echo has the same request ID, access log and recover, no auth, and none of the API routes; the public Echo has none of the ops routes (tested both ways).
- **Webhooks.** `POST /hooks/{provider}` looks the connection up by the Provider's name and hands the request to `Conn.Handler(sink)` with the 1 MB body limit; signature checks, the 202/200/503 answers and the sink are the adapter's. An unknown provider is a `404` problem.
