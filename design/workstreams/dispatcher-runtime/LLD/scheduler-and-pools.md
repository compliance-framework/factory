# LLD — Scheduler, registered pools and leases (RT)

> Workstream: [RT](../../definitions.md#rt-dispatcher-runtime) · Task: RT-S2-T02 · Package: `internal/scheduler`
> Protocol overview: [10 §Agent lifecycle](../../../10-poc.md#agent-lifecycle-and-lease-protocol)

## Task phases

```text
Pending ──lease──▶ Leased ──first heartbeat──▶ Running ──complete──▶ Succeeded | Failed
   ▲   └─(deferred: budget / no-capable-agent / agent-throttled; reason on status)
   └──────── lease expired / abandon / retryable failure (attempts < queue.retry.max+1)
Running ──cancel──▶ Cancelling ──ack or lease expiry──▶ Cancelled
```

## Registration

`POST /api/v1/agents` (`registerAgent`): credentials in the headers `X-Factory-Join-Token` (the pool's join token) and `X-Factory-Owner-Token` (the owner's GitHub user token); body `{pool, host, agentVersion, capabilities, lending}`; response `201 {agentId, agentToken, expiresAt}` ([contracts.md §HTTP API](../../foundation/LLD/contracts.md#http-api)).

1. `X-Factory-Join-Token` matches `Pool.admission.tokenRef` secret (constant-time compare).
2. `X-Factory-Owner-Token` (a short-lived user token from the GitHub **device flow** that `factory-agent register` runs against the dispatcher's OAuth client) → `GET /user` → login; **discarded immediately** (never stored or logged). The worker's OS user needs no `gh` login.
3. Owner ∈ `Pool.admission.owners` Group (via `Conn.ResolveGroup`, cached 10 min).
4. Capabilities ⊇ `Pool.require` (e.g. at least one required harness).
5. Issue `agentToken` (random 32 bytes, stored hashed, TTL 24 h, renewed by heartbeats) and create/replace the `Agent` object (`agt_<owner>-<host>`, `host` from the body).

## Lease matching

`GET /agents/{id}/lease?wait=30s` parks the request; the scheduler tries to match on every Task/Agent change and on a 1 s tick.

Per candidate `(task, agent)` the matcher checks, in order (first failing reason is recorded for the UI):

| # | Check | Reason if failed |
|---|---|---|
| 1 | Task's queue → pool == agent's pool | — (not a candidate) |
| 2 | queue concurrency: running tasks of the queue < `concurrency` | `queue-full` |
| 3 | budgets (queue, RuleSet, global): runs, tokens, USD (USD only counts `apiKey` runs) | `budget` |
| 4 | agent phase `Ready`, not `Throttled` (until passed), running < `lending.maxConcurrent` (and pool `maxTasksPerAgent`) | `agent-busy` / `agent-throttled` |
| 5 | **lending**: `unlimited` short-circuits; else queue ∈ `lending.queues`, task RuleSet ∈ `lending.ruleSets` (if set), harness ∈ `lending.harnesses` (if set), within `lending.hours`, `today.runs < maxRunsPerDay`, `today.tokens < maxTokensPerDay` | `lending` |
| 6 | `requires.labels` ⊆ agent labels | `no-capable-agent` |
| 7 | harness resolution `patterns.Resolve(TaskSpec.allowed, task.request, agent.offer)` succeeds | `no-capable-agent` |
| 8 | **anti-affinity**: if TaskSpec identity is `reviewer` and the PR's last author Run's agent == this agent, skip unless it's the only Ready agent in the pool for > 10 min (soft in PoC) | `anti-affinity` |
| 9 | `has-local-stack` tasks: agent has no other `has-local-stack` task running | `agent-busy` |

Selection among matching pairs:

- Tasks ordered by queue `priority` desc, then `CreatedAt` asc.
- Agents ordered by **fairness**: least-recently-leased first; agents with `lending.unlimited` are tried **last** when another agent can take the task (`Pool.fairness: prefer-limited`, default) or first (`prefer-unlimited`).

On match:

1. `task.spec.harness.resolved = …`; phase `Leased`; `lease{id, agent, expiresAt: now+ttl}`.
2. Mint the git credential through `Conn.MintGitCredential` (rw repo with TaskSpec permissions, read repos read-only, TTL = min(1h, maxWallClock+10m)).
3. Respond `200 {lease, task, gitCredential}` (credential only in this response; never stored).

## Heartbeats and expiry

- `POST /leases/{id}/heartbeat` every `pool.lease.heartbeat` (30 s); body carries progress events (appended to Run, broadcast on SSE) and token usage deltas (`today.tokens`).
- Response: `{expiresAt, cancel: bool, refreshedGitCredential?}` (credential refreshed when < 10 min left).
- **Usage accounting.** Heartbeats carry **deltas** since the previous heartbeat (`inputTokens`, `outputTokens`, `costUSD`); the scheduler adds them to the `BudgetCounter`s and the agent's `today` as they arrive. `Result.harness.usage` is the **cumulative total** for the run. On `complete`, the scheduler adds `max(0, total − Σ deltas)` per field, so nothing is counted twice, and logs a warning when `Σ deltas > total`. On lease expiry or abandon, only the deltas already received count.
- No heartbeat for `ttl` (2 min) → lease expired → Task back to `Pending` (`attempts++`, reason `agent-lost`) if retry policy allows, else `Failed{agent-lost}`.

## Throttling

`PATCH /agents/{id} {phase: Throttled, until}` or `abandon{reason: throttled, until}` → agent `Throttled`; the Task returns to `Pending` **without** consuming an attempt.

## Budgets

Counters live in a dedicated runtime kind, **`BudgetCounter`**, one object per (scope, name, UTC day): name `budget.<queue|ruleset|global>.<name>.<YYYY-MM-DD>`, fields `runs`, `tokens`, `usd`. Queue and RuleSet stay pure config. Updates use store CAS (`Update` with `resourceVersion`, retry on `ErrConflict`): `runs` is incremented when a lease is granted; `tokens`/`usd` from heartbeat deltas and the final Result. The matcher reads today's counters for the task's queue, RuleSet and global scope. Counters are swept after 90 days like other runtime kinds. A Task deferred for budget is retried at the next UTC day boundary (a new counter object) or when the catalog budget changes.

## Tests

- Matching table: one test per row above (pass and fail).
- Fairness: two agents (one unlimited) → limited agent preferred.
- Lending: `maxRunsPerDay` reached → `lending`; hours outside window → `lending`.
- Lease expiry and retry attempts; throttled abandon doesn't consume attempts.
- Anti-affinity soft fallback after 10 min with fake clock.
- Credential is minted per lease and never persisted (store inspected).

## Decisions taken in RT-S2-T02

- **Go API, no HTTP.** `scheduler.New(Deps{Store, Catalog, Conns, Runs, Secrets, Owners}, Options)` with `Run(ctx)` (the lease controller plus the 1 s tick). The API server (RT-S2-T03) calls:
  - `Register`, `Authenticate`, `UpdateAgent` (PATCH: Ready, Throttled{until}, Draining, Offline, lending) and `Deregister`;
  - `Lease(ctx, agent, wait)`, which returns a `*Grant{Lease, Task, GitCredential}` or nil when `wait` (capped at 60 s) runs out;
  - `Heartbeat`, `Complete` and `Abandon`, keyed by `(agent, lease)`. Someone else's lease is `ErrNotLeaseHolder`, and an ended one is `ErrLeaseGone`.

  Errors wrap `ErrUnauthorized`, `ErrForbidden`, `ErrInvalid` (`*ValidationError`), `ErrNotFound` and `ErrLeaseGone`, for 401, 403, 400, 404 and 409. `Explain(task, agent)` returns the first failing reason, for the UI and tests. **The API server must call `Scheduler.Complete`, not `RunAPI.Complete`**, because the usage top-up needs the lease's Σ deltas.
- **Owner verification** goes through an injected `OwnerVerifier.VerifyOwner(ctx, token) → Owner{Provider, Login}`. `FakeOwners` is the test fake; the real verifier (GET /user) belongs to RT-S2-T03. Two details:
  - the owner must be in `admission.owners` through that Group's sources on the owner's provider (`ResolveGroup`, cached 10 min per source, logins compared case-insensitively);
  - a source the provider reports as not found has no members.
- **Agent tokens.** The token is `<agentId>.<base64url(32 random bytes)>`. Only `sha256:<hex>` of the secret part is stored, in an internal `AgentToken` record named by the agent id. The join token is compared in constant time over SHA-256 hashes. Registering again replaces the token.

  The 24 h TTL is renewed by **any authenticated call** (at most every 10 min), not only heartbeats, so an idle polling agent stays registered. The tick takes an agent whose token expired Offline.
- **Leases** are an internal `Lease` record (`lse_<hex>`, labels `factory.ccf.dev/task` and `/agent`). It holds the TTL, the resolved auth, the credential's expiry (never the credential) and Σ heartbeat deltas.

  The authoritative expiry is `Task.status.leaseExpiresAt`. Commit order:
  1. mint the credential;
  2. CAS the task to Leased (`spec.harness.resolved`, agent, expiry);
  3. create the Lease;
  4. charge `runs`;
  5. `RunAPI.LeaseIssued`.

  If the lease record fails, the task returns to Pending without an attempt. If a credential can't be minted, the task stays Pending with reason `git-credential` and the next task is tried. Ended leases are kept for 1 h, so a retried complete or abandon is answered, then deleted.
- **The lease controller** runs on the controller runtime, keyed by task id. It watches Tasks and Leases, and Agents only to wake polls. It reads the leases before the task, so a lease just created is never revoked. It does three things:
  - **Revocation (the hook RT-S2-T01 left):** any open lease whose task is no longer Leased/Running/Cancelling by that agent is ended `revoked`. That covers a run timeout, a cancel without an agent and a re-lease. Heartbeat, Complete and Abandon also check the task and answer `ErrLeaseGone`.
  - **Expiry:** after `leaseExpiresAt`, the task returns to Pending with `attempts + 1` and reason `agent-lost` when `Queue.retry` allows (same rule as the run controller's), else it fails with `agent-lost`. A Cancelling task goes through `RunAPI.AckCancel` to Cancelled. The run controller then closes the run (`lost` or `failed`).
  - The first heartbeat moves Leased to Running.
- **Matching** runs under one scheduler mutex, so in-process checks and charges are atomic (budgets, queue concurrency, capacity). The task CAS still guards against other writers.
  - Row 4: capacity is the smaller of `lending.maxConcurrent` and `pool.maxTasksPerAgent` that are set, or 1 when neither is. `Busy` is derived (Ready ↔ Busy by running tasks). A Throttled agent whose `until` passed counts as Ready and is settled back to Ready. Draining and Offline give `agent-busy`.
  - Row 5: no `lending` lends nothing. Non-unlimited lending needs the queue listed. The harness tested is the request's (or the TaskSpec default's) name.
  - `hours` is `[days] HH:MM-HH:MM [IANA zone]`, UTC by default. An end before the start crosses midnight and belongs to the day it starts on. `time/tzdata` is embedded.
  - Row 7: with no request and no default, the agent's offers are tried in order.
- **Daily caps.** There is no day field on `Agent.status.today`, so the agent's daily usage is a fourth `BudgetCounter` scope, `budget.agent.<agentId>.<UTC day>`, checked against `maxRunsPerDay`/`maxTokensPerDay` and mirrored to `status.today`. Days are UTC, like the other budgets, not the lending zone.
- **Budgets.** `runs` is charged at lease time; `tokens` and `usd` from heartbeat deltas and the complete top-up; all to the queue, RuleSet (from provenance) and agent counters, with CAS create-or-update. USD counts only runs whose resolved auth is `apiKey`. **The catalog has no global budget**, so the global scope isn't checked. Over budget, the task stays Pending (`budget`) until the next UTC day's counter.
- **Fairness.** Each pass walks the pending tasks in order (queue priority desc, `createdAt`, id). It gives each task to the fairest *parked* poll not yet given one (prefer-limited by default, then least-recently-leased, then name). A poll takes the first task it is given. The skipped tasks are left to the other parked agents, which the same change has woken. Last-lease times and parked polls are in memory.
- **Anti-affinity.** The PR's last author run is the newest Run of the work item's tasks whose TaskSpec identity is `author`. The "only Ready agent" test asks whether another serving agent in the pool could take the task at all (lending lists, labels, harness; not hours or daily caps). If none can, the author's agent takes the task once it has waited 10 min (in memory, from the first time it was held back).
- **Reasons.** The tick (`Refresh`) writes on each pending task the reason of the agent that got furthest down the table (`no-capable-agent` when the pool has no agent), only when it changed. Heartbeat progress is appended to the open Run (CAS, ring of 200).
- **Not done here.** There's an SSE broadcast of progress (RT-S2-T03), and metrics (RT-S2-T04). `controllers/run.go` still has `TODO(RT-S2-T02)` in `checkTimeout`; revocation is done here, so that comment can go in a controllers PR.
