# LLD — Contracts: Go types, schemas, provider interface, HTTP API (FND)

> Workstream: [FND](../../definitions.md#fnd-foundation) · Tasks: FND-S1-T01, FND-S1-T02, FND-S1-T03
> These contracts are the **only** coupling between workstreams. Changing them = a dedicated PR merged before dependents.

## Go types

Package `internal/apis/v1alpha1` (`APIVersion = "factory.ccf.dev/v1alpha1"`). Every kind embeds `TypeMeta`; all kinds except Task and Event have `ObjectMeta`:

```go
type TypeMeta struct { APIVersion string `json:"apiVersion,omitzero"`; Kind string `json:"kind"` } // apiVersion optional, never added on output
type ObjectMeta struct {
    Name            string            `json:"name"`
    Labels          map[string]string `json:"labels,omitzero"`
    ResourceVersion string            `json:"resourceVersion,omitzero"` // runtime kinds only
    CreatedAt       time.Time         `json:"createdAt,omitzero"`
}
```

Optional fields use `omitzero`, so an explicitly empty list or map (`labels: []`, `inputs: {}`) survives a round trip. Pointers mark optional structs and the few fields where `false` or `null` must be kept (`publicReads`, `headless`, and the nullable runtime fields below).

**Decoding** (`Decode`, `DecodeOne`, `DecodeResult`, `Encode`) uses YAML 1.2 (`go.yaml.in/yaml/v3`), so `on`, `yes` and `no` are strings and only `true`/`false` are booleans, in typed fields and free-form maps alike. A stream is read document by document (`---`, `...`, comments and directives handled by the parser; empty documents skipped). Each document becomes a generic value (keys must be strings; duplicate keys, merge keys and unknown tags are errors; timestamps stay as written), then JSON, which is decoded with `encoding/json` and `DisallowUnknownFields`, so the `json` tags are the only field names. The document's `kind` picks the type. Unknown fields, an unknown kind and an `apiVersion` other than `APIVersion` are errors: `*DocumentError{Index, Line, Kind}` wrapping `ErrUnknownKind`, `ErrMissingKind`, `ErrAPIVersion` or a `*ValidationError{Field}` from the kind's `Validate()`. `Encode` goes through JSON too: struct field order, block style, quoting only where a plain scalar would read back as another type.

**Value types:**

| Type | Shape | Used for |
|---|---|---|
| `Duration` | `string` as written; `Parse()` = `time.ParseDuration` plus a leading whole number of days (`7d`, `1d12h`) | every duration; validated by CORE, not at decode |
| `StringOrList` | a string or a list of strings; remembers which, so it round-trips | harness patterns: `models`, `model`, `reasoningEffort` (`"*"` vs `[low, medium]`) |
| `ExprOr[T]` | an expression string or a `T`. A string must contain a `{{ … }}` template (substituted at catalog load) or start with `=` (CEL, evaluated at runtime): `ExprPattern` = `^=\|\{\{[\s\S]*\}\}`, the same pattern in Go and in the schema's `oneOf [string, T]`. Any other string (`harness: codex`) is an error asking for the object form | `dispatch.harness` (the only structured field the examples template) |
| `EffectAction` | `op` (required), `target` (optional string) and the op-specific fields **inline** (`{ op: label.add, labels: [needs-human] }`), collected into `Args map[string]any` and written back inline in sorted order; an explicit `args:` key, or an `Args` key named `op`/`target`/`args`, is an error. Schema: `op` required, other properties allowed, checked per op by the catalog loader. Inline keeps the catalog readable and matches [04](../../../04-flows.md); the Effector HTTP request keeps `{op, target, args, idempotencyKey}`, and the dispatcher maps `Args` to `args` | the `effect` action |
| `TemplateBody` | a string (inline Go `text/template`) or `{blocks: path}` (Slack Block Kit file) | `Template.spec.bodies` |
| `map[string]any` | free-form | `params`, `inputs`, `set`, `defaults`, `overrides`, `facts`, `verdict`, `data` |

### Config kinds (from git)

Each is `{TypeMeta, Metadata ObjectMeta, Spec <Kind>Spec}`.

| Kind | Spec fields (Go) | Source of truth |
|---|---|---|
| `Provider` | `Type ProviderType` (`github`\|`slack`); `GitHub *GitHubProviderSpec` (`APIURL`, `Webhook{Path, SecretRef}`, `Identities map[string]AppIdentity{AppID int64, PrivateKeyRef}`); `Slack *SlackProviderSpec{Mode, AppTokenRef, BotTokenRef}` (future); `Scope *ProviderScope{Orgs, Channels}`; `Trust []TrustRule{Tier, Group, AuthorAssociation, Bots, WorkspaceMember}`; `Resync *ResyncPolicy{Every Duration}` | [03 §Provider](../../../03-domain-model.md#provider) |
| `Group` | `Members []GroupSource{Provider, Team, OrgRole *OrgRole{Org, Role}, Users, UserGroup}` (exactly one of team \| orgRole \| users \| userGroup); `Cache *GroupCache{TTL Duration}` | [03 §Group](../../../03-domain-model.md#group) |
| `Template` | `Bodies map[ProviderType]TemplateBody` (`TemplateBody` = text \| `{Blocks}`) | [03 §Template](../../../03-domain-model.md#template) |
| `Rule` | `RuleSet string`; `ProviderSelector *LabelSelector`; `On []EventType`; `Where string`; `Params map[string]any`; `Actions []Action`; `Priority int`; `Final bool`; `Debounce *Debounce{Window, Key}`; `Schedule string` | [03 §Rule](../../../03-domain-model.md#rule) |
| `RuleSet` | `ProviderSelector LabelSelector`; `Scope *Scope{Repos, Exclude, Channels, Branches}`; `Rules RuleSelection{Refs, Selector *LabelSelector, Exclude}`; `Overrides map[string]map[string]any`; `Defaults map[string]any`; `Budget *Budget{Daily *BudgetLimit{Runs, Tokens, USD}}`; `StateKeys []string`; inline rules `InlineRules []InlineRule{Metadata, Spec RuleSpec}` (`inlineRules`) | [03 §RuleSet](../../../03-domain-model.md#ruleset-mandatory) |
| `TaskSpec` | `Description`; `Inputs map[string]InputSpec{Type, Enum []any, Default any, Required}`; `Instructions *Instructions{Template, Headless *bool}`; `Skills []SkillRef{Source, Ref, Commit, Use}`; `Requires *Requires{Labels}`; `Harness *HarnessPolicy{Allowed []HarnessConstraint{Name, Models, ReasoningEffort StringOrList}, Default *HarnessRequest{Name, Config *HarnessConfig{Model, ReasoningEffort StringOrList, Prefer []string}}}`; `Workspace *Workspace{Repos []RepoMount{Ref, Mode ro\|rw}, Checkout, PublicReads *bool}`; `Identity string`; `Permissions map[string]string`; `Effects *EffectsPolicy{Allow []EffectOp, Labels []string}`; `Limits *Limits{MaxTurns, MaxWallClock Duration, MaxCostUSD}`; `Output *OutputSpec{Schema, Outcomes}` | [03 §TaskSpec](../../../03-domain-model.md#taskspec) |
| `Queue` | `Pool string`; `Concurrency int`; `Priority int`; `Budget *Budget`; `Retry *RetryPolicy{Max, On []RetryReason}` | [03 §Queue and Pool](../../../03-domain-model.md#queue-and-pool) |
| `Pool` | `Mode PoolMode` (`registered`\|`ephemeral`); `Admission *Admission{TokenRef, Owners}`; `Lease *LeasePolicy{TTL, Heartbeat, MaxTasksPerAgent}`; `Require *PoolRequirements{Harnesses}`; `Fairness string`; ephemeral: `Backend *PoolBackend{Type, GitHubActions{ProviderRef, Repo, Workflow}}`, `Image`, `Scaling *Scaling{Min, Max}`, `LLMAuth *LLMAuth{APIKeyRef}` | same |

`Action` is a tagged union — exactly one member. The decoder calls `Rule.Validate()` / `RuleSet.Validate()`, which reject zero or several members with a `*ValidationError` naming `spec.actions[i]` (and `spec.actions[i].notify.to[j]` for a destination that doesn't set exactly one of group, channel, item); the schema says the same with `minProperties: 1, maxProperties: 1`. `Group.Validate()` likewise names `spec.members[i]`.

```go
type Action struct {
    Dispatch        *DispatchAction `json:"dispatch,omitzero"`        // TaskSpec, Queue, Inputs map[string]any, Harness *ExprOr[HarnessRequest], Requires *Requires, Context []string, Target *Target{Provider, Repo, Ref}
    Set             map[string]any  `json:"set,omitzero"`             // values: literal or "=CEL"
    Notify          *NotifyAction   `json:"notify,omitzero"`          // Template, To []Destination{Group, Via, As, Channel *ChannelRef{Provider, Name}, Item}: exactly one of group | channel | item
    RequestApproval *ApprovalSpec   `json:"requestApproval,omitzero"` // future (Q18): the Approval's spec; accepted by schema, engine returns Unsupported in PoC
    Cancel          *CancelAction   `json:"cancel,omitzero"`          // TaskSpec
    Link            *LinkAction     `json:"link,omitzero"`            // From string (CEL)
    Effect          *EffectAction   `json:"effect,omitzero"`          // Op EffectOp, Target string, op-specific fields inline (Args map[string]any); the Effector request carries them as args
}
```

### Runtime kinds (in the store)

All have the `metadata` / `spec` / `status` shape except Event (CloudEvents attributes at the top level). `status` is optional. Fields shown as `null` in [03](../../../03-domain-model.md#runtime-kinds) are pointers without `omitzero`, so they are always written (`null` until set).

| Kind | Key fields |
|---|---|
| `Event` | CloudEvents: `ID`, `Source` (provider instance or `factory`), `Type EventType`, `Subject`, `Time`; `Data EventData{Provider, Actor *Actor{ID, Tier, IsBot, IsFactory, Identity}, Object *ObjectRef{Provider, Kind, Ref}, Repo, ContentRef, Facts map[string]any, Raw json.RawMessage}`; `WorkItem string` (set for `factory.*`). No `metadata`. |
| `WorkItem` | `Spec.Links []Link{Provider, Ref, Kind, Primary}`; `Status.State map[string]any`, `Counters map[string]int`, `Approvals []ApprovalRef{Name, Phase}`, `Decisions []Decision{At, Event, RuleSet, Rule, Fired, Inputs, Rails []RailVerdict{Rail, Verdict, Reason}, Actions, Reason}` (bounded, newest `MaxDecisions` = 200), `Reporter *Actor` |
| `Task` | `Metadata TaskMeta{ID, WorkItem, Provenance{Event, RuleSet, Rule, CatalogSHA}, Labels, ResourceVersion, CreatedAt}` (named by id, no `name`); `Spec TaskRunSpec{TaskSpec TaskSpecRef{Name, Digest}, Queue, Pool, Provider, Target, Inputs, Harness{Request *HarnessRequest, Resolved *HarnessResolved (null until leased)}, Requires *Requires (merged TaskSpec + dispatch agent labels; matched by the scheduler), Skills (with commit), Prompt{Ref, Digest}, Context []ContextItem{Kind, Provider, Ref, Trust, Mount}, Credentials{Git{Provider, Identity, Repo, ReadRepos, Permissions, TTL}, LLM: "agent-local"}, Effects, Limits, Output}`; `Status{Phase (Pending\|Leased\|Running\|Succeeded\|Failed\|Cancelling\|Cancelled), Agent *string (null until leased), Reason, Attempts, LeaseExpiresAt}` |
| `Run` | `Spec{Task, Attempt, Agent, Owner}`; `Status{StartedAt, EndedAt, End RunEnd (completed\|failed\|cancelled\|abandoned\|lost; empty while running), Progress []ProgressEvent{At, Kind, Message, Data} (ring buffer), Result *Result, TranscriptRef, Usage *Usage}` |
| `Agent` | `Spec{Pool, Owner, Capabilities{Harnesses []HarnessOffer{Name, Version, Auth, Models, DefaultModel, ReasoningEffort}, Labels}, Lending *Lending{Unlimited, Queues, RuleSets, Harnesses, MaxConcurrent, MaxRunsPerDay, MaxTokensPerDay, Hours}}`; `Status{Phase (Ready\|Busy\|Throttled\|Draining\|Offline), ThrottledUntil *time.Time (null unless throttled), LastHeartbeat, Running []TaskID, Today{Runs, Tokens}}` |
| `Effect` | `Spec{Task, Op, Target ObjectRef, IdempotencyKey, Identity, Request map[string]any}`; `Status{Result{URL, Error}, At}` |
| `BudgetCounter` | name `budget.<scope>.<name>.<YYYY-MM-DD>`; `Status{Runs, Tokens, USD}` (CAS-updated by the scheduler) |
| `Approval` | type only in PoC (Q18): `Metadata{Name, Labels, …, WorkItem}`; `Spec ApprovalSpec{Name, Approvers, Quorum, Via, Template, Expires Duration}`; `Status{Phase (Pending\|Granted\|Denied\|Expired), Decisions []{By, Decision, At, Via}}` |

### Result

Exactly the JSON in [03 §Result](../../../03-domain-model.md#result): `Task`, `Status` (`done|failed|needs_input|noop`), `Outcome`, `Summary`, `Verdict map[string]any`, `Artifacts []Artifact{Kind, URL, Ref}`, `Suggest []Suggestion{TaskSpec, Reason, Inputs}`, `Question *string` (always written; `null` unless `needs_input`), `Harness *ResultHarness{Name, Version, Auth, EffectiveConfig{Model, ReasoningEffort}, Usage{InputTokens, OutputTokens, Turns, WallClock Duration, CostUSD *float64}}` (`costUSD` always written; `null` for subscription runs), plus spec-specific fields under `Data map[string]any` validated by the TaskSpec's output schema.

### Capability descriptors

```go
type AdapterCapabilities struct {          // published by each harness adapter (static per adapter version)
    Harness          string
    EffortMapping    map[string]string      // normalised effort → native value
    Features         []string               // "structured-output", "mcp", "plugins", "skills-dir"
}
type HarnessOffer struct { Name, Version, Auth string; Models []string; DefaultModel string; ReasoningEffort []string } // concrete values, not patterns; DefaultModel: the harness's own default, preferred by resolution
```

`factory validate --capabilities caps.json` validates patterns against them; the agent's `Probe` produces per-machine offers (`HarnessOffer`).

### JSON Schemas

`make schemas` runs `internal/apis/v1alpha1/schemagen`, which writes `schemas/<kind>.schema.json` for each config kind (`provider`, `group`, `template`, `rule`, `ruleset`, `taskspec`, `queue`, `pool`) and `schemas/result.schema.json` (draft 2020-12, `$id` `https://factory.ccf.dev/schemas/v1alpha1/<file>`). They are reflected from the types (invopop/jsonschema), so they match strict decoding: objects have `additionalProperties: false`, `kind` and `apiVersion` are `const`, `EventType` and `EffectOp` are enums of the vocabulary below, `Action` allows exactly one property, and `GroupSource` / `Destination` require exactly one of their sources / targets (`oneOf` of `required`). Output is deterministic; CI (`make check-generated`) fails when the committed files are stale.

## Event vocabulary

Constants in `v1alpha1/events.go`, with the emitting provider types (validation uses this map: `EventType.ProviderTypes()`, `EmittedBy(p)`, `Known()`, and `IsFactory()` for the `factory.` prefix):

| Type | Provider types |
|---|---|
| `issue.opened` `issue.commented` `issue.labeled` `issue.closed` | github |
| `pr.opened` `pr.updated` `pr.ready_for_review` `pr.labeled` `pr.review.submitted` `pr.checks.completed` `pr.merged` `pr.closed` | github |
| `release.published` | github |
| `deploy.succeeded` `deploy.failed` | github |
| `message.posted` `message.mention` | slack (future) |
| `factory.run.completed` `factory.run.failed` `factory.approval.granted` `factory.approval.denied` `factory.approval.expired` `factory.schedule.tick` `factory.workitem.resynced` `factory.command.pause` `factory.command.resume` `factory.command.redispatch` `factory.command.cancel` | **factory only** |

## Effect ops

Canonical names (as used in the PoC catalog), constants in `v1alpha1/effects.go`. `EffectOp.Side()` says who executes it: `SideAPI` (`api`) or `SideGit` (`git`).

| Op | Side | Notes |
|---|---|---|
| `comment`, `pr.comment` | API | issue / PR comment |
| `label.add`, `label.remove` | API | checked against `effects.labels` globs (matched with `internal/patterns`) |
| `create-issue`, `sub-issue.add`, `close`, `link-duplicate` | API | |
| `pr.open`, `pr.update`, `pr.retarget`, `pr.merge` | API | `pr.merge` honours repo merge method; falls back to label if review requirement blocks (Q5) |
| `review.comment`, `review.request-changes`, `review.approve` | API | reviewer identity only (TaskSpec `identity: reviewer`) |
| `thread.reply`, `thread.resolve` | API | both identities: the author addresses review feedback, the reviewer follows up on its threads |
| `push-branch`, `stack.push`, `stack.rebase` | **git (agent)** | executed in the workspace with the scoped git token; the agent still records them via `POST /effects` (op + ref) for audit |

## Provider interface

Package `internal/provider` (implemented by GH, faked by `provider/fake`):

```go
type Adapter interface {
    Type() string
    Connect(ctx context.Context, p v1alpha1.Provider, secrets SecretResolver) (Conn, error)
}

type Conn interface {
    // Ingress
    Handler(sink IngressSink) http.Handler                       // mounted at Provider.webhook.path; verifies, then sink.Accept (persists) BEFORE acking
    Recover(ctx context.Context, since time.Time, sink IngressSink) error // startup: re-ingest deliveries the store hasn't seen
    Normalise(ctx context.Context, ev RawEvent) ([]v1alpha1.Event, error)
    // Observation
    Observe(ctx context.Context, ref v1alpha1.ObjectRef) (Observation, error)
    List(ctx context.Context, scope v1alpha1.Scope) ([]v1alpha1.ObjectRef, error)
    ResolveGroup(ctx context.Context, g v1alpha1.GroupSource) ([]string, error)
    // Projection + effects
    Project(ctx context.Context, ref v1alpha1.ObjectRef, p Projection) error
    Apply(ctx context.Context, e EffectRequest) (EffectResult, error)
    // Credentials for agents
    MintGitCredential(ctx context.Context, req GitCredentialRequest) (Credential, error)
    Health() HealthStatus
}

type IngressSink interface { Accept(ctx context.Context, ev RawEvent) error } // stores a Delivery record; error → handler returns 503

type Projection struct {
    WorkItemID string
    State      map[string]any
    Counters   map[string]int
    Links      []v1alpha1.Link
    Labels     []string            // factory-owned labels to ensure (others untouched)
}
```

`provider/fake` keeps objects in memory, records effects, and can emit scripted events — used by CORE/RT tests. (`factory-mock` serves canned examples and needs no provider.)

### Refinements (FND-S1-T03)

The interface above is implemented as written; the types it references, and the gaps the GH and RT LLDs left, are settled as follows.

**Supporting types** (`internal/provider`; v1alpha1 types wherever they fit):

| Type | Shape | Why |
|---|---|---|
| `RawEvent` | `ID` (the Delivery record name, unique across providers: `gh-<X-GitHub-Delivery>`), `Provider` (instance name), `Type` (native event name, `X-GitHub-Event`), `Headers http.Header`, `Body []byte`, `ReceivedAt` | what [GH §Ingress](../../provider-github/LLD/github-provider.md#ingress-and-normalisation) stores before acking. `Body` is bytes, not `json.RawMessage`, because a provider may post form-encoded bodies |
| `Observation` | `Ref ObjectRef`, `State` (`open`\|`closed`\|`merged`), `Labels`, `Author *v1alpha1.Actor` (tier unresolved; `IsFactory`/`Identity` say whether a factory App opened it), `UpdatedAt`, exactly one of `Issue{SubIssues, LinkedPRs, Comments}` / `PR{Draft, Head, Base, Checks{State, Settled}, ApprovedBy []{Identity, Commit}, UnresolvedThreads, MergedAt, MergeCommit}`, `Deploy *{State, SHA, URL, ContainsMerge}`, `Projections []Projection` | the fields [GH §Observe](../../provider-github/LLD/github-provider.md#observe-list-and-projection) lists, shaped like the rules' `item.pr` / `item.deploy` (`rules.PRView`, `DeployView`) without importing `rules`. No untrusted text. JSON-tagged so the work-item controller can digest it for resync |
| `EffectRequest` | `Op EffectOp`, `Target ObjectRef` (for `create-issue` / `pr.open`, `Ref` is the repo `owner/name`), `Args map[string]any`, `Identity`, `IdempotencyKey` (required) | the Effect record's spec without `task`; [api-server.md §Effector](../../dispatcher-runtime/LLD/api-server.md#effector-api-post-tasksideffects) step 7 and the rule controller's `evt/<event>/<rule>/<i>` keys |
| `EffectResult` | `URL`, `Object *ObjectRef` (created or acted on), `Outcome` (e.g. `labelled-for-merge`; empty = applied as asked), `Replayed` | `v1alpha1.EffectResult{URL, Error}` is the stored status; the controllers also need the created object (to link it), pr.merge's fallback and whether a key was replayed |
| `GitCredentialRequest` | `Identity`, `Repo` (required: no unscoped token), `ReadRepos`, `Permissions`, `TTL time.Duration` | the Task's `spec.credentials.git` minus `provider` (the Conn is the provider), with the TTL the scheduler computes at lease time |
| `Credential` | `Provider, Host, Username, Token, ExpiresAt, Repo, ReadRepos, Permissions` | exactly the API's `GitCredential`. `String`, `Format` (every fmt verb), `LogValue` and `MarshalJSON` all redact `Token` (`[REDACTED]`), so it can't leak through a log line or a store record; the lease response copies the fields into the generated type explicitly |
| `SecretResolver` | `Resolve(ctx, v1alpha1.SecretRef) ([]byte, error)` (+ `SecretResolverFunc`) | bytes, for PEM keys |
| `HealthStatus` | `Status` (`ok`\|`degraded`\|`down`\|`unknown`), `Message`, `CheckedAt` | the API's `ProviderHealth` |
| `Projection` | as above, JSON-tagged | Observe reads it back |

**Behaviour the LLDs implied, now pinned:**

- **Duplicate deliveries.** A sink returns `provider.ErrDuplicateDelivery` (wrapped or not) when the Delivery record exists. `Handler` answers `202` on success, `200` on a duplicate and `503` on any other error. `Recover` skips duplicates, which is how it re-ingests "only what the store hasn't seen".
- **Event IDs.** `provider.EventID(delivery, n)` = `<delivery>/<n>`, with Events numbered in object-ref order; `Source` and `Data.Provider` are the Provider's name and actor IDs are `<provider>:<login>` (as in the rules fixtures).
- **Projection round-trip.** `Observation.Projections` holds the projections found on the object (the GitHub markers, one per work item, sorted by work item ID), so the work-item rebuild is `List` → `Observe` → `Projections`, and marker parsing stays inside the adapter.
- **Trust Groups.** `Connect` carries no catalog, but trust rules name catalog Groups. `provider.GroupLookup` (`func(name) (v1alpha1.GroupSpec, bool)`) is given to the adapter's **constructor**, reading the live catalog, so a Conn sees Group changes without reconnecting. Clock, logger and HTTP client go to the constructor too; `Connect`'s signature is unchanged. `provider.StaticGroups` builds one for tests.
- **`ResolveGroup`** returns provider-native user IDs (GitHub logins, not `<provider>:<login>` Actor IDs), sorted and deduplicated.
- **Errors** (`errors.Is`): `ErrNotFound`, `ErrInvalid` (missing key, unscoped credential, malformed delivery, wrong provider), `ErrUnsupported` (git-side ops; ops a provider type lacks), `ErrRetryable` via `*RetryableError{RetryAfter, Err}` (the Effector API answers `429 provider-retryable` with `Retry-After`, RT-S2-T03), `ErrDuplicateDelivery`, and the registry's `ErrUnknownType` / `ErrAlreadyRegistered`.
- **Context content.** `Conn` has no read of an object's untrusted text (Observe carries none). A Conn may also implement `server.ContentReader` (`ReadContent(ctx, ObjectRef) (Content{MediaType, Text, Author}, error)`) to serve context content on `GET /tasks/{id}/context/{n}`, fenced and labelled by the API server; the GitHub adapter (GH-S1) must implement it. Otherwise context items are reference-only (`content: ""`).
- **Registry.** The provider controller connects one Conn per catalog Provider by type, so `provider.Registry` (`NewRegistry`, `Register`, `Lookup`, `Types`, `Connect`) maps `Type()` to adapters. It is a value, not a global: `factory serve` registers the GitHub adapter, tests register the fake under `github`.

**Not in the contract.** `Conn` has no `Close`: the GitHub Conn holds no goroutines, and the controller simply drops a replaced Conn. The API's single `gitCredential` has no slot for the separate read-only token [GH §Auth](../../provider-github/LLD/github-provider.md#auth-and-tokens) mints for private read repos; `Credential.ReadRepos` lists only what `Token` can read, and a second token needs an additive API change (GH-S1-T01 to decide).

**`provider/fake`.** `fake.NewAdapter(Options{Type (default github), Clock, Groups, Identities (default ccf-factory[bot] → author, ccf-factory-reviewer[bot] → reviewer), Host, BaseURL})`; `Adapter.Conn(name)` returns the per-instance `*fake.Conn` (it survives reconnects) so tests can seed it before connecting, and `fake.NewConn` builds one without an adapter. Everything is mutex-guarded and getters return copies.

- **Objects:** `Seed(...Observation)` (defaults: provider, kind `issue`, state `open`), `Object`, `Update` (out-of-band changes, e.g. a human merge). `Observe` returns them; `List` returns those in scope (`internal/patterns` globs) that are open or carry a projection.
- **Ingress:** tests build `fake.Webhook{Delivery, Type, Sender{Login, Bot, Association, Tier}, Object, FanOut, Repo, Subject, ContentRef, Facts, Time}`. `Emit(ctx, sink, ...)` delivers them, `Undelivered(...)` records deliveries only `Recover` will send, `Handler` accepts a POSTed Webhook (no signature; 202/200/503 as above, 405, 400). `Normalise` resolves the actor's tier from `Sender.Tier` or the Provider's trust rules (`group` through `Groups` + `ResolveGroup`, `authorAssociation`, `bots`; `workspaceMember` is not modelled), else `anyone`.
- **Records:** `Projections()`, `Applies()` (every call, replays and failures included), `Effects()` (applied ones), `Mints()`, `Deliveries()`. `Apply` changes the objects for the ops a cascade observes (comments, labels, close, link-duplicate, create-issue, pr.open, sub-issue.add, pr.retarget, pr.merge, review.approve, thread.resolve); a replayed key returns the first result with `Replayed`; a failed Apply stores nothing, so the retry runs.
- **Groups and tokens:** `SetTeam`, `SetOrgRole`, `SetUserGroup` feed `ResolveGroup`; `MintGitCredential` returns `fake-<identity>-<n>` scoped exactly as requested, TTL capped at 1 h. `SetHealth`; `FailNext(method, err)` injects one failure.

## HTTP API

`api/openapi.yaml` (**OpenAPI 3.0.3**, see below). Base path `/api/v1`. All bodies JSON unless an operation says otherwise; times RFC 3339; errors `application/problem+json` (RFC 9457 `Problem{type, title, status, detail, instance, code, errors[]}`, `code` machine-readable: `idempotency-key-required`, `effect-not-allowed`, …). Every operation has an `operationId`, one tag (the group below), explicit `security` and a `default` problem response.

| Group | Method + path (`operationId`) | Auth | Purpose |
|---|---|---|---|
| **agents** | `POST /agents` (`registerAgent`) | join token + owner GitHub token | register; body `{pool, host, agentVersion, capabilities, lending}` → `201 {agentId, agentToken, expiresAt}` |
| | `PATCH /agents/{id}` (`updateAgent`) | agent | `{phase: Ready\|Throttled\|Draining\|Offline, until, lending}` → the Agent |
| | `DELETE /agents/{id}` (`deregisterAgent`) | agent | deregister → `204` |
| **leases** | `GET /agents/{id}/lease?wait=30s` (`leaseTask`) | agent | long-poll; `200 {lease{id, agent, task, run, expiresAt, heartbeatEvery}, task, gitCredential{provider, host, username, token, expiresAt, repo, readRepos, permissions}}` or `204` |
| | `POST /leases/{id}/heartbeat` (`heartbeatLease`) | agent | body `{progress[], usage{inputTokens, outputTokens, costUSD}}` (deltas) → `{expiresAt, cancel, refreshedGitCredential?}`; `404`/`409` = lease gone |
| | `POST /leases/{id}/complete` (`completeLease`) | agent | body `{result: Result, transcriptRef}` → `204` |
| | `POST /leases/{id}/abandon` (`abandonLease`) | agent | body `{reason: throttled\|agent-error\|shutdown, until, message}` → `204` |
| **tasks** | `GET /tasks/{id}` (`getTask`) | agent (own lease) / UI | full Task |
| | `GET /tasks/{id}/prompt` (`getTaskPrompt`) | agent (own lease) | rendered prompt (`text/plain`, `Digest` header) |
| | `GET /tasks/{id}/context/{n}` (`getTaskContext`) | agent (own lease) | `{index, kind, provider, ref, trust, mediaType, content}` (fenced, labelled trust) |
| | `PUT /runs/{id}/transcript` (`uploadTranscript`) | agent | `application/gzip` body (gzip-compressed JSON Lines), ≤ 20 MB (`413` over) → `{transcriptRef, bytes}` |
| | `GET /runs/{id}/transcript` (`getTranscript`) | UI session / agent (own lease) | the stored transcript as uploaded: `application/gzip` (gzip-compressed JSON Lines), loaded lazily by the run view; `404` problem when none was uploaded |
| **effects** | `POST /tasks/{id}/effects` (`createEffect`) | agent (own lease) | `{op, target, args, idempotencyKey}` (`idempotencyKey` **required**, `minLength: 1`, `maxLength: 200`) → `200 {effectId, url}` (also for a replayed key); 400 if the key is missing, 403 if not allowed, 429 on rate limits |
| **resources** | `GET /workitems` (`listWorkItems`), `GET /workitems/{id}` (`getWorkItem`), `GET /tasks` (`listTasks`), `GET /runs/{id}` (`getRun`), `GET /queues` (`listQueues`), `GET /pools` (`listPools`), `GET /agents` (`listAgents`), `GET /catalog` (`getCatalog`), `GET /catalog/{kind}/{name}` (`getCatalogObject`), `GET /providers` (`listProviders`) | UI session | read models: list endpoints take `?cursor=&limit=&labelSelector=` and return `{items, nextCursor}` (absent on the last page). `getWorkItem` → `{workItem, events, tasks, runs, effects}` (the timeline); `getRun` → `{run, effects}`; queues → `{queue, depth, running, pending[{task, reason, since}]}`; pools → `{pool, agents[]}`; providers → `{provider, health{status, message, checkedAt}}`; `getCatalog` → `{repo, ref, sha, loadedAt, status, errors[], objects[{kind, name, labels}]}`; `{kind}` is a config Kind name (`TaskSpec`) and the object is that kind (`oneOf`, discriminator `kind`) |
| **dry-run** | `POST /catalog/dry-run` (`dryRunCatalog`) | UI session | `{event, workItem?}` → `Evaluation{event, workItem, matches[{ruleSet, rule, result}], decisions[{ruleSet, rule, action{type, …}, rail{verdict, reason}, inputs}], stateAfter}` (no side effects) |
| **commands** | `POST /commands` (`postCommand`) | UI session (org admin) / CLI token | `{type, target, args}` with `type ∈ pause\|resume\|cancel\|redispatch\|dispatch\|resync-catalog` (shapes in [api-server.md §Commands](../../dispatcher-runtime/LLD/api-server.md#commands-post-commands)) → `202 {type, event}`; becomes a `factory.command.*` or injected event |
| **events** | `GET /stream?topics=runs/{id},workitems,agents` (`streamEvents`) | UI session | SSE (`Last-Event-ID` header to resume): `progress` (`{run, task, event: ProgressEvent}`), `workitem.updated` (WorkItem), `task.updated` (Task), `agent.updated` (Agent); the data schemas are named in the response's `x-sse-events` extension |
| **auth** | `GET /auth/login` (`login`, 302), `GET /auth/callback` (`authCallback`, 302), `POST /auth/logout` (`logout`, 204), `GET /auth/me` (`getMe`, UI session → `{login, name, avatarUrl, isAdmin}`) | — | GitHub OAuth web flow for the UI |
| | `GET /auth/device-config` (`getDeviceConfig`) | — | `{clientId, scopes}` for the agent's device-flow owner login (`factory-agent register`) |
| **webhooks** | `POST /hooks/{provider}` (`receiveWebhook`) | provider HMAC | not under `/api/v1` (path-level `servers: [/]`); delegated to `Conn.Handler()` with an opaque body → `202` |
| **ops** | `GET /healthz` (`getHealthz`), `GET /readyz` (`getReadyz`), `GET /metrics` (`getMetrics`) | none — served only on the localhost ops listener (`--ops-addr`), never on the public address | documented with path-level `servers: [http://127.0.0.1:9090]` |

**Security schemes.** `agentToken` (HTTP bearer), `joinToken` (header `X-Factory-Join-Token`) **and** `ownerToken` (header `X-Factory-Owner-Token`, the device-flow user token; discarded after the login lookup) together on `registerAgent`, `uiSession` (cookie `factory_session`), `cliToken` (HTTP bearer, `FACTORY_CLI_TOKEN`; a full admin, listed as an alternative to `uiSession` on every operation except `getMe`, RT-S2-T03), `providerSignature` (header `X-Hub-Signature-256`; documented only, checked by `Conn.Handler()`). The registration credentials are headers, not body fields, because OpenAPI security schemes can only name headers, cookies or query parameters ([scheduler-and-pools.md §Registration](../../dispatcher-runtime/LLD/scheduler-and-pools.md#registration)).

**Why OpenAPI 3.0.3, not 3.1.** oapi-codegen supports 3.1 only from v2.8.0, whose generated code needs `github.com/oapi-codegen/runtime` ≥ v1.4, which requires Echo ≥ v4.15.1. The allow-list pins Echo to v4.13.x (the `api` line), so the spec is 3.0.3 and the generator is oapi-codegen **v2.7.2** with runtime **v1.3.1**. Moving to 3.1 is a version bump plus mechanical spec edits (`nullable` → `type: [T, "null"]`, single-value `enum` → `const`), once Echo moves to v4.15.

**Codegen.** oapi-codegen is a `tool` in `go.mod` (`go tool oapi-codegen`). One config per package, run by its `//go:generate` (so `make generate` covers it): the **Echo strict server** (`echo-server` + `strict-server` + models) in `internal/apiserver/gen`, the **client** (+ models) in `internal/apiclient`. Both set `output-options.exclude-tags: [webhooks, ops]`: webhooks are mounted with `echo.WrapHandler(Conn.Handler())` and the ops routes live on the ops listener, so neither gets a generated handler on the public server (`internal/apiserver/gen/routes_test.go` checks the registered routes against the spec). `compatibility.always-prefix-enum-values` keeps enum constants `<Type><Value>`.

**The v1alpha1 components are generated, in full.** `make generate` first runs `api/componentsgen` (also `make openapi-components`), which reflects every kind the API carries (Event, WorkItem, Task, Run, Agent, Effect and the eight config kinds) with `v1alpha1.NewReflector`, the reflector behind `schemas/*.schema.json`, and writes one component per v1alpha1 type they reach (the kinds, their specs and statuses, Result, ObjectMeta, the enums, …) between the `BEGIN GENERATED` / `END GENERATED` markers at the end of `api/openapi.yaml`. It converts JSON Schema 2020-12 to OpenAPI 3.0.3 (`$defs` → components, `const` → one-value `enum`, `oneOf [T, null]` → `nullable: true`, `propertyNames` dropped, `$ref` siblings dropped), uses the Go doc comments as descriptions, and sorts components and keys, so the output is deterministic; `check-generated` covers `api/openapi.yaml`. Every component carries `x-go-type: v1alpha1.<Type>` + `x-go-type-import`, so the generated code aliases `internal/apis/v1alpha1` instead of copying it (`type Task = v1alpha1.Task`, `type ExprOrHarnessRequest = v1alpha1.ExprOr[v1alpha1.HarnessRequest]`); hand-written components are the API-only shapes above. Generating rather than hand-writing: the reflector already matches strict decoding (`additionalProperties: false`, required = never omitted, the Action and destination one-ofs), so the spec can't drift from the Go types. `api/spec_test.go` checks it both ways anyway: every JSON field of each Go type is a property and every property a JSON field, recursively through `$ref`s, required ⇔ not `omitzero`, an always-written pointer is `nullable`, every field of a v1alpha1 type refers to that type's component, and every v1alpha1 type the kinds reach has one. The always-written pointers (`TaskStatus.agent`, `TaskHarness.resolved`, `AgentStatus.throttledUntil`) carry `jsonschema:"nullable"`, and `TaskPhase`, `AgentPhase` and `ApprovalPhase` have `JSONSchema()` enums (`TaskPhases()`, …). The enums `EventType`, `EffectOp`, `TaskPhase`, `AgentPhase`, `CatalogKind` and `Result.status` are also tested equal to the v1alpha1 vocabulary. The web app generates its TypeScript client from the spec with `openapi-typescript`, so these full shapes are what it types the board, run, agents and catalog views with.

**Examples.** `api/examples/<operationId>.json` holds one example per operation's success response (the lowest 2xx with a body); operations whose success has no body (`204`, the `302` redirects, `receiveWebhook`'s `202`) have none. For a JSON media type the file is the body; for `text/event-stream` it is an array of frames `{event, id, data}`; for `application/gzip` (`getTranscript`) a JSON string holding the uncompressed JSON Lines, gzip-compressed when served; for other media types (`text/plain`) a JSON string. Package `api` (`api/embed.go`) embeds the spec and the examples, so the tests, `factory-mock` and the generators read the same files. `api/spec_test.go` (kin-openapi) loads and validates the spec, fails if an operation lacks its example (or an example has no operation), validates each example against its response schema, and strictly decodes every `x-go-type` part with the v1alpha1 decoder. `factory-mock` and MSW serve them. `factory-mock` (`--addr`, default `127.0.0.1:8081`) is a small `net/http` server that matches each request (method + path template, under each operation's server path) to its operation and answers with that operation's example and status, `404` for unknown paths and `405` (with `Allow`) for a wrong method, both `application/problem+json`; `/stream` sends the example events and stays open until the client leaves. It doesn't use the generated server, so it has no Echo dependency, and it checks no auth.

**Lint.** `make openapi-lint` runs vacuum (`go run github.com/daveshanley/vacuum@v0.30.6`, no Node) with `api/vacuum.yaml`: the recommended rules minus `oas3-missing-example` (examples live in `api/examples/`), `camel-case-properties` (v1alpha1 keeps Go initialisms: `catalogSHA`, `costUSD`), `description-duplication` and `oas-missing-type` (3.0 writes "any value" by leaving out `type`: `EventData.raw`, `InputSpec.default`); `--ext-refs` so the `x-sse-events` refs count as uses; warnings and infos fail. It is part of `make reviewable` and of CI's `make-targets`.

**Contract versioning:** additive changes only during the PoC; a breaking change bumps the path to `/api/v2` — not expected in the PoC.
