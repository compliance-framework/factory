package provider

import (
	"net/http"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Object kinds, as in ObjectRef.Kind and Link.Kind.
const (
	KindIssue = "issue"
	KindPR    = "pr"
)

// RawEvent is one provider delivery as received, before normalisation. The provider
// controller stores it as a Delivery record named ID.
type RawEvent struct {
	// ID identifies the delivery across every provider: the adapter prefixes the provider's
	// own delivery ID ("gh-<X-GitHub-Delivery>"). Events normalised from it are EventID(ID, n).
	ID string `json:"id"`
	// Provider is the Provider instance's name ("github-public").
	Provider string `json:"provider"`
	// Type is the provider-native event name ("issues", X-GitHub-Event), if the provider has one.
	Type string `json:"type,omitzero"`
	// Headers are the request headers as received.
	Headers http.Header `json:"headers,omitzero"`
	// Body is the request body as received (untrusted).
	Body       []byte    `json:"body"`
	ReceivedAt time.Time `json:"receivedAt"`
}

// ObjectState is the lifecycle state of an observed object.
type ObjectState string

// Object states.
const (
	StateOpen   ObjectState = "open"
	StateClosed ObjectState = "closed"
	// StateMerged is a merged PR (a PR closed without merging is StateClosed).
	StateMerged ObjectState = "merged"
)

// Observation is the current state of one provider object, as Observe reads it. Exactly one
// of Issue and PR is set, following Ref.Kind. Untrusted text (titles, bodies, comments) is
// never part of it.
type Observation struct {
	Ref    v1alpha1.ObjectRef `json:"ref"`
	State  ObjectState        `json:"state"`
	Labels []string           `json:"labels,omitzero"`
	// Author is who opened the object; IsFactory and Identity say whether a factory App did.
	// Tier is not resolved.
	Author    *v1alpha1.Actor    `json:"author,omitzero"`
	UpdatedAt time.Time          `json:"updatedAt,omitzero"`
	Issue     *IssueObservation  `json:"issue,omitzero"`
	PR        *PRObservation     `json:"pr,omitzero"`
	Deploy    *DeployObservation `json:"deploy,omitzero"`
	// Projections are the projections found on the object (the GitHub markers), one per work
	// item, sorted by WorkItemID.
	Projections []Projection `json:"projections,omitzero"`
}

// IssueObservation is what Observe reads of an issue.
type IssueObservation struct {
	SubIssues []v1alpha1.ObjectRef `json:"subIssues,omitzero"`
	LinkedPRs []v1alpha1.ObjectRef `json:"linkedPRs,omitzero"`
	Comments  int                  `json:"comments,omitzero"`
}

// PRObservation is what Observe reads of a PR (the rules' item.pr).
type PRObservation struct {
	Draft bool `json:"draft"`
	// Head is the head commit SHA; Base the base branch.
	Head              string     `json:"head,omitzero"`
	Base              string     `json:"base,omitzero"`
	Checks            Checks     `json:"checks"`
	ApprovedBy        []Approval `json:"approvedBy,omitzero"`
	UnresolvedThreads int        `json:"unresolvedThreads,omitzero"`
	MergedAt          time.Time  `json:"mergedAt,omitzero"`
	MergeCommit       string     `json:"mergeCommit,omitzero"`
}

// Checks is the rollup of a PR head's checks.
type Checks struct {
	State   string `json:"state,omitzero"`
	Settled bool   `json:"settled,omitzero"`
}

// Approval is an approving review: by which factory identity (or login), at which commit.
type Approval struct {
	Identity string `json:"identity"`
	Commit   string `json:"commit"`
}

// DeployObservation is the latest deployment of the default branch (the rules' item.deploy).
type DeployObservation struct {
	State         string `json:"state,omitzero"`
	SHA           string `json:"sha,omitzero"`
	URL           string `json:"url,omitzero"`
	ContainsMerge bool   `json:"containsMerge,omitzero"`
}

// EffectRequest is one API-side effect, as the Effector API and the rule controller send it
// (an Effect record's spec, without the task).
type EffectRequest struct {
	Op v1alpha1.EffectOp `json:"op"`
	// Target is the object acted on; for create-issue and pr.open, Ref is the repo
	// ("owner/name").
	Target v1alpha1.ObjectRef `json:"target"`
	// Args are the op-specific fields (labels, body, title, base, …).
	Args map[string]any `json:"args,omitzero"`
	// Identity is the factory identity that acts ("author", "reviewer").
	Identity string `json:"identity,omitzero"`
	// IdempotencyKey is required: the Effector's hash(task, key), or the rule controller's
	// evt/<event>/<rule>/<i>.
	IdempotencyKey string `json:"idempotencyKey"`
}

// EffectResult is the outcome of an applied effect.
type EffectResult struct {
	URL string `json:"url,omitzero"`
	// Object is the object created (create-issue, pr.open) or acted on.
	Object *v1alpha1.ObjectRef `json:"object,omitzero"`
	// Outcome qualifies an op that did something other than asked, e.g. pr.merge's
	// "labelled-for-merge"; empty when the op was applied as asked.
	Outcome string `json:"outcome,omitzero"`
	// Replayed is true when the idempotency key had already been applied and nothing new was
	// created.
	Replayed bool `json:"replayed,omitzero"`
}

// GitCredentialRequest asks for an agent's git token: the Task's spec.credentials.git without
// the provider (the Conn is the provider), with the TTL the scheduler computed at lease time.
type GitCredentialRequest struct {
	Identity string
	// Repo is the task repo ("owner/name"); required, so a token is never unscoped.
	Repo        string
	ReadRepos   []string
	Permissions map[string]string
	TTL         time.Duration
}

// HealthState is a connection's health, as GET /providers reports it.
type HealthState string

// Health states.
const (
	HealthOK       HealthState = "ok"
	HealthDegraded HealthState = "degraded"
	HealthDown     HealthState = "down"
	HealthUnknown  HealthState = "unknown"
)

// HealthStatus is Conn.Health(): the API's ProviderHealth.
type HealthStatus struct {
	Status    HealthState `json:"status"`
	Message   string      `json:"message,omitzero"`
	CheckedAt time.Time   `json:"checkedAt,omitzero"`
}
