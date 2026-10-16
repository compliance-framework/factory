package scheduler

import (
	"errors"
	"fmt"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/store"
)

// Reasons the scheduler writes on a Pending task's status (the LLD's matching table), plus
// the expiry and credential reasons.
const (
	ReasonQueueFull      = "queue-full"
	ReasonBudget         = "budget"
	ReasonAgentBusy      = "agent-busy"
	ReasonAgentThrottled = "agent-throttled"
	ReasonLending        = "lending"
	ReasonNoCapableAgent = "no-capable-agent"
	ReasonAntiAffinity   = "anti-affinity"
	// ReasonAgentLost is a lease that expired without a heartbeat (retry reason agent-lost).
	ReasonAgentLost = string(v1alpha1.RetryAgentLost)
	// ReasonGitCredential is a task whose git credential could not be minted at lease time.
	ReasonGitCredential = "git-credential"
)

// Errors of the scheduler API. Every error it returns for a bad request or a missing object
// wraps one of these, so the API server maps them to HTTP statuses with errors.Is.
var (
	// ErrUnauthorized: a wrong join token, owner token or agent token (401).
	ErrUnauthorized = errors.New("unauthorized")
	// ErrForbidden: authenticated but not allowed: the owner is not in the pool's owners
	// Group, the capabilities miss Pool.require, or the lease is someone else's (403).
	ErrForbidden = errors.New("forbidden")
	// ErrInvalid: a malformed request (400). Validation failures are *ValidationError.
	ErrInvalid = errors.New("invalid request")
	// ErrNotFound: no such agent or lease (404).
	ErrNotFound = errors.New("not found")
	// ErrLeaseGone: the lease ended (expired, revoked after a timeout or cancel, completed or
	// abandoned): the agent must stop the run (409).
	ErrLeaseGone = errors.New("lease gone")
	// ErrNotLeaseHolder: the lease belongs to another agent (403 not-lease-holder).
	ErrNotLeaseHolder = fmt.Errorf("not the lease holder: %w", ErrForbidden)
)

// ValidationError is a request field that fails validation. It matches ErrInvalid.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Msg }

// Is makes errors.Is(err, ErrInvalid) true.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalid }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// Internal kinds the scheduler keeps in the store (store.MetaObject; no API, no schema).
const (
	// KindLease is one lease: which agent holds which task until when, and the usage deltas
	// its heartbeats reported. It never holds a credential.
	KindLease = "Lease"
	// KindAgentToken is an agent's bearer token, hashed (SHA-256), named by the agent id.
	KindAgentToken = "AgentToken"
)

// Labels on Lease records.
const (
	LabelTask  = "factory.ccf.dev/task"
	LabelAgent = "factory.ccf.dev/agent"
)

// Lease ends (LeaseStatus.End).
const (
	LeaseCompleted = "completed"
	LeaseAbandoned = "abandoned"
	LeaseExpired   = "expired"
	// LeaseRevoked is a lease whose task left it: a run timeout, a cancel without an agent,
	// a deregistration, or a task finished by someone else.
	LeaseRevoked = "revoked"
)

// Lease is the record of one lease. Ended leases are kept for LeaseRetention so a retried
// complete is answered, then deleted.
type Lease struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
	Spec     LeaseSpec           `json:"spec"`
	Status   LeaseStatus         `json:"status"`
}

var _ store.MetaObject = (*Lease)(nil)

// GetName returns metadata.name (the lease id).
func (l *Lease) GetName() string { return l.Metadata.Name }

// GetObjectMeta implements store.MetaObject.
func (l *Lease) GetObjectMeta() *v1alpha1.ObjectMeta { return &l.Metadata }

// LeaseSpec is what was leased to whom.
type LeaseSpec struct {
	Task    string `json:"task"`
	Agent   string `json:"agent"`
	Pool    string `json:"pool"`
	Queue   string `json:"queue"`
	RuleSet string `json:"ruleSet,omitzero"`
	// Auth is the resolved harness's auth (USD budgets count apiKey runs only).
	Auth     string        `json:"auth,omitzero"`
	IssuedAt time.Time     `json:"issuedAt"`
	TTL      time.Duration `json:"ttl"`
	// Heartbeat is the pool's heartbeat interval, returned to the agent.
	Heartbeat time.Duration `json:"heartbeat"`
}

// LeaseStatus is the lease's state.
type LeaseStatus struct {
	ExpiresAt     time.Time `json:"expiresAt"`
	LastHeartbeat time.Time `json:"lastHeartbeat,omitzero"`
	// CredentialExpiresAt is when the last git credential minted for the lease expires
	// (the credential itself is never stored).
	CredentialExpiresAt time.Time `json:"credentialExpiresAt,omitzero"`
	// Usage is the sum of the heartbeats' usage deltas.
	Usage   LeaseUsage `json:"usage"`
	End     string     `json:"end,omitzero"`
	EndedAt time.Time  `json:"endedAt,omitzero"`
}

// LeaseUsage is usage summed over heartbeats.
type LeaseUsage struct {
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUSD"`
}

// AgentToken is an agent's token, stored hashed.
type AgentToken struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
	// Hash is "sha256:<hex>" of the token's secret part.
	Hash      string    `json:"hash"`
	ExpiresAt time.Time `json:"expiresAt"`
	RenewedAt time.Time `json:"renewedAt"`
}

var _ store.MetaObject = (*AgentToken)(nil)

// GetName returns metadata.name (the agent id).
func (a *AgentToken) GetName() string { return a.Metadata.Name }

// GetObjectMeta implements store.MetaObject.
func (a *AgentToken) GetObjectMeta() *v1alpha1.ObjectMeta { return &a.Metadata }

// ---- API types -------------------------------------------------------------------------------

// RegisterRequest is registerAgent: the two header credentials and the body.
type RegisterRequest struct {
	// JoinToken is X-Factory-Join-Token, the pool's join token.
	JoinToken string
	// OwnerToken is X-Factory-Owner-Token, the owner's GitHub user token from the device
	// flow. It is exchanged for a login and discarded: never stored or logged.
	OwnerToken   string
	Pool         string
	Host         string
	AgentVersion string
	Capabilities v1alpha1.AgentCapabilities
	Lending      *v1alpha1.Lending
}

// Registration is registerAgent's response. AgentToken is returned only here.
type Registration struct {
	AgentID    string
	AgentToken string
	ExpiresAt  time.Time
}

// AgentUpdate is updateAgent (PATCH /agents/{id}): a phase report and/or a lending update.
type AgentUpdate struct {
	// Phase is Ready, Throttled (with Until), Draining or Offline; empty keeps it.
	Phase v1alpha1.AgentPhase
	Until time.Time
	// Lending, when set, replaces the agent's lending.
	Lending *v1alpha1.Lending
}

// Grant is a lease handed to an agent: leaseTask's 200 response.
type Grant struct {
	Lease LeaseInfo
	Task  *v1alpha1.Task
	// GitCredential is minted for this lease and never stored; nil when the task has no git
	// credential request.
	GitCredential *provider.Credential
}

// LeaseInfo is the API's LeaseInfo.
type LeaseInfo struct {
	ID             string
	Agent          string
	Task           string
	Run            string
	ExpiresAt      time.Time
	HeartbeatEvery time.Duration
}

// HeartbeatRequest is the progress and usage since the previous heartbeat.
type HeartbeatRequest struct {
	Progress []v1alpha1.ProgressEvent
	Usage    *UsageDelta
}

// UsageDelta is usage since the previous heartbeat. CostUSD is nil for subscription runs.
type UsageDelta struct {
	InputTokens  int64
	OutputTokens int64
	CostUSD      *float64
}

// HeartbeatResponse is heartbeatLease's response.
type HeartbeatResponse struct {
	ExpiresAt time.Time
	// Cancel tells the agent to stop the run: the task is being cancelled.
	Cancel bool
	// RefreshedGitCredential is a new credential when the last one had under 10 minutes left.
	RefreshedGitCredential *provider.Credential
}

// AbandonRequest is abandonLease's body.
type AbandonRequest struct {
	// Reason is throttled, agent-error or shutdown.
	Reason string
	// Until is when a throttled harness's limit resets.
	Until   time.Time
	Message string
}
