package v1alpha1

import (
	"encoding/json"
	"time"

	"github.com/invopop/jsonschema"
)

// ---- Event ---------------------------------------------------------------------------------

// Event is a normalised provider event, shaped as a CloudEvent: its identity is
// (Source = provider instance or "factory", Type).
type Event struct {
	TypeMeta
	ID      string    `json:"id"`
	Source  string    `json:"source"`
	Type    EventType `json:"type"`
	Subject string    `json:"subject,omitzero"`
	Time    time.Time `json:"time"`
	Data    EventData `json:"data"`
	// WorkItem is set on factory.* events, which always carry their work item.
	WorkItem string `json:"workItem,omitzero"`
}

// GetName returns the event id.
func (e *Event) GetName() string { return e.ID }

// EventData is the event's payload.
type EventData struct {
	Provider   string         `json:"provider"`
	Actor      *Actor         `json:"actor,omitzero"`
	Object     *ObjectRef     `json:"object,omitzero"`
	Repo       string         `json:"repo,omitzero"`
	ContentRef string         `json:"contentRef,omitzero"`
	Facts      map[string]any `json:"facts,omitzero"`
	// Raw is the provider payload, kept for audit; never shown to rules.
	Raw json.RawMessage `json:"raw,omitzero"`
}

// Actor is who caused an event.
type Actor struct {
	// ID is provider-instance:login, e.g. github-public:octocat.
	ID        string `json:"id"`
	Tier      string `json:"tier,omitzero"`
	IsBot     bool   `json:"isBot,omitzero"`
	IsFactory bool   `json:"isFactory,omitzero"`
	// Identity is the factory identity (author, reviewer) when IsFactory.
	Identity string `json:"identity,omitzero"`
}

// ---- WorkItem ------------------------------------------------------------------------------

// WorkItem groups the provider objects that are about one piece of work.
type WorkItem struct {
	TypeMeta
	Metadata ObjectMeta      `json:"metadata"`
	Spec     WorkItemSpec    `json:"spec"`
	Status   *WorkItemStatus `json:"status,omitzero"`
}

// GetName returns metadata.name.
func (w *WorkItem) GetName() string { return w.Metadata.Name }

// WorkItemSpec is the spec of a WorkItem.
type WorkItemSpec struct {
	Links []Link `json:"links,omitzero"`
}

// Link is a provider object that belongs to the work item.
type Link struct {
	Provider string `json:"provider"`
	Ref      string `json:"ref"`
	Kind     string `json:"kind,omitzero"`
	Primary  bool   `json:"primary,omitzero"`
}

// WorkItemStatus is the work item's state, set only by rules and controllers.
type WorkItemStatus struct {
	State map[string]any `json:"state,omitzero"`
	// Counters are runs per task spec (derived).
	Counters  map[string]int `json:"counters,omitzero"`
	Approvals []ApprovalRef  `json:"approvals,omitzero"`
	// Decisions are bounded to the newest MaxDecisions.
	Decisions []Decision `json:"decisions,omitzero"`
	Reporter  *Actor     `json:"reporter,omitzero"`
}

// MaxDecisions is how many decisions a work item keeps.
const MaxDecisions = 200

// ApprovalRef is an approval's name and phase, as shown on the work item.
type ApprovalRef struct {
	Name  string        `json:"name"`
	Phase ApprovalPhase `json:"phase"`
}

// Decision records why a rule did (or didn't) act: the UI's "why did this happen?".
type Decision struct {
	At      time.Time `json:"at,omitzero"`
	Event   string    `json:"event,omitzero"`
	RuleSet string    `json:"ruleSet,omitzero"`
	Rule    string    `json:"rule,omitzero"`
	Fired   bool      `json:"fired,omitzero"`
	// Inputs are the values `where` saw.
	Inputs  map[string]any `json:"inputs,omitzero"`
	Rails   []RailVerdict  `json:"rails,omitzero"`
	Actions []string       `json:"actions,omitzero"`
	Reason  string         `json:"reason,omitzero"`
}

// RailVerdict is one safety rail's verdict on a decision.
type RailVerdict struct {
	Rail    string `json:"rail"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitzero"`
}

// ---- Task ----------------------------------------------------------------------------------

// TaskPhase is a Task's lifecycle phase.
type TaskPhase string

// Task phases: Pending → Leased → Running → Succeeded | Failed, and
// Running → Cancelling → Cancelled.
const (
	TaskPending   TaskPhase = "Pending"
	TaskLeased    TaskPhase = "Leased"
	TaskRunning   TaskPhase = "Running"
	TaskSucceeded TaskPhase = "Succeeded"
	TaskFailed    TaskPhase = "Failed"
	// TaskCancelling is a task being cancelled (the cancel command): its agent is told on the
	// next heartbeat and the task becomes Cancelled on the agent's ack or on lease expiry.
	TaskCancelling TaskPhase = "Cancelling"
	TaskCancelled  TaskPhase = "Cancelled"
)

// TaskPhases lists the task phases in lifecycle order.
func TaskPhases() []TaskPhase {
	return []TaskPhase{TaskPending, TaskLeased, TaskRunning, TaskSucceeded, TaskFailed, TaskCancelling, TaskCancelled}
}

// JSONSchema makes TaskPhase an enum of TaskPhases.
func (TaskPhase) JSONSchema() *jsonschema.Schema { return enumSchema(TaskPhases()) }

// Task is one instantiation of a TaskSpec for a work item.
type Task struct {
	TypeMeta
	Metadata TaskMeta    `json:"metadata"`
	Spec     TaskRunSpec `json:"spec"`
	Status   *TaskStatus `json:"status,omitzero"`
}

// GetName returns the task id.
func (t *Task) GetName() string { return t.Metadata.ID }

// TaskMeta is a Task's metadata: tasks are named by id and carry their provenance.
type TaskMeta struct {
	ID              string            `json:"id"`
	WorkItem        string            `json:"workItem"`
	Provenance      *Provenance       `json:"provenance,omitzero"`
	Labels          map[string]string `json:"labels,omitzero"`
	ResourceVersion string            `json:"resourceVersion,omitzero"`
	CreatedAt       time.Time         `json:"createdAt,omitzero"`
}

// Provenance is the event, RuleSet, rule and catalog commit that created a task.
type Provenance struct {
	Event      string `json:"event"`
	RuleSet    string `json:"ruleSet"`
	Rule       string `json:"rule"`
	CatalogSHA string `json:"catalogSHA"`
}

// TaskRunSpec is a Task's spec: the TaskSpec resolved for this work item.
type TaskRunSpec struct {
	TaskSpec TaskSpecRef    `json:"taskSpec"`
	Queue    string         `json:"queue"`
	Pool     string         `json:"pool,omitzero"`
	Provider string         `json:"provider,omitzero"`
	Target   *Target        `json:"target,omitzero"`
	Inputs   map[string]any `json:"inputs,omitzero"`
	Harness  *TaskHarness   `json:"harness,omitzero"`
	// Requires are the agent labels the task needs: the TaskSpec's requires merged with the
	// dispatch's. The scheduler matches them against agent capabilities.
	Requires    *Requires        `json:"requires,omitzero"`
	Skills      []SkillRef       `json:"skills,omitzero"`
	Prompt      *PromptRef       `json:"prompt,omitzero"`
	Context     []ContextItem    `json:"context,omitzero"`
	Credentials *TaskCredentials `json:"credentials,omitzero"`
	Effects     *EffectsPolicy   `json:"effects,omitzero"`
	Limits      *Limits          `json:"limits,omitzero"`
	Output      *OutputSpec      `json:"output,omitzero"`
}

// TaskSpecRef is the TaskSpec a task was made from, by name and digest.
type TaskSpecRef struct {
	Name   string `json:"name"`
	Digest string `json:"digest,omitzero"`
}

// TaskHarness is the requested harness and, once leased, the resolved one (null until then).
type TaskHarness struct {
	Request  *HarnessRequest  `json:"request,omitzero"`
	Resolved *HarnessResolved `json:"resolved" jsonschema:"nullable"`
}

// HarnessResolved is the concrete harness config chosen when an agent leased the task.
type HarnessResolved struct {
	Name            string `json:"name"`
	Version         string `json:"version,omitzero"`
	Auth            string `json:"auth,omitzero"`
	Model           string `json:"model,omitzero"`
	ReasoningEffort string `json:"reasoningEffort,omitzero"`
}

// PromptRef is where the agent fetches the rendered prompt.
type PromptRef struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest,omitzero"`
}

// ContextItem is something the agent is given, with its trust label.
type ContextItem struct {
	Kind     string `json:"kind"`
	Provider string `json:"provider,omitzero"`
	Ref      string `json:"ref"`
	Trust    string `json:"trust,omitzero"`
	Mount    string `json:"mount,omitzero"`
}

// TaskCredentials are the credentials a task gets. LLM is always "agent-local": the factory
// never sees LLM credentials.
type TaskCredentials struct {
	Git *GitCredentialSpec `json:"git,omitzero"`
	LLM string             `json:"llm,omitzero"`
}

// GitCredentialSpec describes the scoped git token minted for a task.
type GitCredentialSpec struct {
	Provider    string            `json:"provider"`
	Identity    string            `json:"identity"`
	Repo        string            `json:"repo"`
	ReadRepos   []string          `json:"readRepos,omitzero"`
	Permissions map[string]string `json:"permissions,omitzero"`
	TTL         Duration          `json:"ttl,omitzero"`
}

// TaskStatus is a Task's status. Agent is null until the task is leased.
type TaskStatus struct {
	Phase          TaskPhase `json:"phase"`
	Agent          *string   `json:"agent" jsonschema:"nullable"`
	Reason         string    `json:"reason,omitzero"`
	Attempts       int       `json:"attempts,omitzero"`
	LeaseExpiresAt time.Time `json:"leaseExpiresAt,omitzero"`
}

// ---- Run -----------------------------------------------------------------------------------

// Run is one attempt at a Task by one agent.
type Run struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     RunSpec    `json:"spec"`
	Status   *RunStatus `json:"status,omitzero"`
}

// GetName returns metadata.name.
func (r *Run) GetName() string { return r.Metadata.Name }

// RunSpec is a Run's spec.
type RunSpec struct {
	Task    string `json:"task"`
	Attempt int    `json:"attempt"`
	Agent   string `json:"agent"`
	Owner   string `json:"owner,omitzero"`
}

// RunStatus is a Run's status.
type RunStatus struct {
	StartedAt time.Time `json:"startedAt,omitzero"`
	EndedAt   time.Time `json:"endedAt,omitzero"`
	// End is how the run ended; empty while it runs.
	End RunEnd `json:"end,omitzero" jsonschema:"enum=completed,enum=failed,enum=cancelled,enum=abandoned,enum=lost"`
	// Progress is a ring buffer of the latest progress events.
	Progress      []ProgressEvent `json:"progress,omitzero"`
	Result        *Result         `json:"result,omitzero"`
	TranscriptRef string          `json:"transcriptRef,omitzero"`
	Usage         *Usage          `json:"usage,omitzero"`
}

// RunEnd is how a run ended.
type RunEnd string

// Run ends.
const (
	RunCompleted RunEnd = "completed" // the agent returned a Result (done, failed, needs_input or noop)
	RunFailed    RunEnd = "failed"    // timeout, invalid result or infrastructure failure
	RunCancelled RunEnd = "cancelled" // cancelled by a rule or command, and acknowledged
	RunAbandoned RunEnd = "abandoned" // the agent gave the lease back (throttled, error, shutdown)
	RunLost      RunEnd = "lost"      // the lease expired without a heartbeat
)

// ProgressEvent is one progress line streamed by the agent.
type ProgressEvent struct {
	At      time.Time      `json:"at"`
	Kind    string         `json:"kind"`
	Message string         `json:"message,omitzero"`
	Data    map[string]any `json:"data,omitzero"`
}

// ---- Agent ---------------------------------------------------------------------------------

// AgentPhase is a registered agent's state.
type AgentPhase string

// Agent phases.
const (
	AgentReady     AgentPhase = "Ready"
	AgentBusy      AgentPhase = "Busy"
	AgentThrottled AgentPhase = "Throttled"
	AgentDraining  AgentPhase = "Draining"
	AgentOffline   AgentPhase = "Offline"
)

// AgentPhases lists the agent phases.
func AgentPhases() []AgentPhase {
	return []AgentPhase{AgentReady, AgentBusy, AgentThrottled, AgentDraining, AgentOffline}
}

// JSONSchema makes AgentPhase an enum of AgentPhases.
func (AgentPhase) JSONSchema() *jsonschema.Schema { return enumSchema(AgentPhases()) }

// Agent is a registered worker.
type Agent struct {
	TypeMeta
	Metadata ObjectMeta   `json:"metadata"`
	Spec     AgentSpec    `json:"spec"`
	Status   *AgentStatus `json:"status,omitzero"`
}

// GetName returns metadata.name.
func (a *Agent) GetName() string { return a.Metadata.Name }

// AgentSpec is what the agent offers and what its owner lends.
type AgentSpec struct {
	Pool string `json:"pool"`
	// Owner must be in the pool's owners Group.
	Owner        string            `json:"owner"`
	Capabilities AgentCapabilities `json:"capabilities"`
	Lending      *Lending          `json:"lending,omitzero"`
}

// AgentCapabilities are the agent's harness offers and labels.
type AgentCapabilities struct {
	Harnesses []HarnessOffer `json:"harnesses,omitzero"`
	Labels    []string       `json:"labels,omitzero"`
}

// Lending is the owner's consent: what the agent may take. Unlimited means any queue, any
// hour, no daily cap.
type Lending struct {
	Unlimited       bool     `json:"unlimited,omitzero"`
	Queues          []string `json:"queues,omitzero"`
	RuleSets        []string `json:"ruleSets,omitzero"`
	Harnesses       []string `json:"harnesses,omitzero"`
	MaxConcurrent   int      `json:"maxConcurrent,omitzero"`
	MaxRunsPerDay   int      `json:"maxRunsPerDay,omitzero"`
	MaxTokensPerDay int64    `json:"maxTokensPerDay,omitzero"`
	Hours           string   `json:"hours,omitzero"`
}

// AgentStatus is the agent's state. ThrottledUntil is null unless the harness reported plan limits.
type AgentStatus struct {
	Phase          AgentPhase  `json:"phase"`
	ThrottledUntil *time.Time  `json:"throttledUntil" jsonschema:"nullable"`
	LastHeartbeat  time.Time   `json:"lastHeartbeat,omitzero"`
	Running        []string    `json:"running,omitzero"`
	Today          *AgentUsage `json:"today,omitzero"`
}

// AgentUsage is the agent's usage today, checked against its lending.
type AgentUsage struct {
	Runs   int   `json:"runs"`
	Tokens int64 `json:"tokens"`
}

// ---- Effect --------------------------------------------------------------------------------

// Effect is an audit record of one provider write.
type Effect struct {
	TypeMeta
	Metadata ObjectMeta    `json:"metadata"`
	Spec     EffectSpec    `json:"spec"`
	Status   *EffectStatus `json:"status,omitzero"`
}

// GetName returns metadata.name.
func (e *Effect) GetName() string { return e.Metadata.Name }

// EffectSpec is what was asked.
type EffectSpec struct {
	Task           string         `json:"task"`
	Op             EffectOp       `json:"op"`
	Target         ObjectRef      `json:"target"`
	IdempotencyKey string         `json:"idempotencyKey"`
	Identity       string         `json:"identity,omitzero"`
	Request        map[string]any `json:"request,omitzero"`
}

// EffectStatus is what happened.
type EffectStatus struct {
	Result *EffectResult `json:"result,omitzero"`
	At     time.Time     `json:"at,omitzero"`
}

// EffectResult is the outcome of an effect.
type EffectResult struct {
	URL   string `json:"url,omitzero"`
	Error string `json:"error,omitzero"`
}

// ---- BudgetCounter -------------------------------------------------------------------------

// BudgetCounter counts spend in one budget window. Its name is
// budget.<scope>.<name>.<YYYY-MM-DD>; the scheduler updates it with compare-and-swap.
type BudgetCounter struct {
	TypeMeta
	Metadata ObjectMeta          `json:"metadata"`
	Status   BudgetCounterStatus `json:"status"`
}

// GetName returns metadata.name.
func (b *BudgetCounter) GetName() string { return b.Metadata.Name }

// BudgetCounterStatus is the spend so far.
type BudgetCounterStatus struct {
	Runs   int     `json:"runs"`
	Tokens int64   `json:"tokens"`
	USD    float64 `json:"usd"`
}

// ---- Approval ------------------------------------------------------------------------------

// ApprovalPhase is an Approval's state: Pending → Granted | Denied | Expired.
type ApprovalPhase string

// Approval phases.
const (
	ApprovalPending ApprovalPhase = "Pending"
	ApprovalGranted ApprovalPhase = "Granted"
	ApprovalDenied  ApprovalPhase = "Denied"
	ApprovalExpired ApprovalPhase = "Expired"
)

// ApprovalPhases lists the approval phases.
func ApprovalPhases() []ApprovalPhase {
	return []ApprovalPhase{ApprovalPending, ApprovalGranted, ApprovalDenied, ApprovalExpired}
}

// JSONSchema makes ApprovalPhase an enum of ApprovalPhases.
func (ApprovalPhase) JSONSchema() *jsonschema.Schema { return enumSchema(ApprovalPhases()) }

// Approval turns "a human said yes" into one factory event. Type only in the PoC (Q18).
type Approval struct {
	TypeMeta
	Metadata ApprovalMeta    `json:"metadata"`
	Spec     ApprovalSpec    `json:"spec"`
	Status   *ApprovalStatus `json:"status,omitzero"`
}

// GetName returns metadata.name.
func (a *Approval) GetName() string { return a.Metadata.Name }

// ApprovalMeta is ObjectMeta plus the work item the approval belongs to.
type ApprovalMeta struct {
	ObjectMeta
	WorkItem string `json:"workItem"`
}

// ApprovalSpec is what is asked, of whom. It is also the payload of the requestApproval action.
type ApprovalSpec struct {
	// Name is what rules test: approval.name == 'implement'.
	Name string `json:"name"`
	// Approvers is a Group.
	Approvers string `json:"approvers"`
	Quorum    int    `json:"quorum,omitzero"`
	// Via is where it's presented and answers are accepted.
	Via      []string `json:"via,omitzero"`
	Template string   `json:"template,omitzero"`
	Expires  Duration `json:"expires,omitzero"`
}

// ApprovalStatus is the approval's state and the decisions so far.
type ApprovalStatus struct {
	Phase     ApprovalPhase      `json:"phase"`
	Decisions []ApprovalDecision `json:"decisions,omitzero"`
}

// ApprovalDecision is one human's answer.
type ApprovalDecision struct {
	By       string    `json:"by"`
	Decision string    `json:"decision"`
	At       time.Time `json:"at"`
	Via      string    `json:"via,omitzero"`
}
