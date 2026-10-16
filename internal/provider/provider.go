// Package provider is the contract between the dispatcher and a provider adapter (GitHub, and
// later Slack): ingress, observation, projection, effects and git credentials for agents
// (design/workstreams/foundation/LLD/contracts.md §Provider interface).
//
// The package holds only interfaces and plain types. Adapters live in sub-packages
// (provider/github); provider/fake is the in-memory implementation for tests.
package provider

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Adapter connects Provider objects of one type.
type Adapter interface {
	// Type is the Provider spec.type this adapter connects ("github").
	Type() string
	// Connect returns a connection for one Provider instance. Adapter-wide dependencies (HTTP
	// client, clock, logger, the catalog's Groups) are given to the adapter's constructor, not
	// here. The provider controller connects again when the Provider object changes.
	Connect(ctx context.Context, p v1alpha1.Provider, secrets SecretResolver) (Conn, error)
}

// Conn is a connection to one provider instance. It is safe for concurrent use.
type Conn interface {
	// Handler is mounted at the Provider's webhook path. It verifies the request, then calls
	// sink.Accept, and acknowledges only once Accept has returned: 202 when it succeeded, 200
	// when it returned ErrDuplicateDelivery, 503 for any other error (so the provider retries).
	Handler(sink IngressSink) http.Handler
	// Recover re-ingests, through sink.Accept, the deliveries the provider made since since
	// (startup, after the restart gap). Deliveries the sink already has come back as
	// ErrDuplicateDelivery and are skipped.
	Recover(ctx context.Context, since time.Time, sink IngressSink) error
	// Normalise maps one delivery to neutral Events, with IDs EventID(ev.ID, n), Source and
	// Data.Provider set to the Provider's name and the actor's tier resolved. A delivery that
	// maps to no event type returns no Events and no error.
	Normalise(ctx context.Context, ev RawEvent) ([]v1alpha1.Event, error)

	// Observe reads the current state of one object; ErrNotFound when it doesn't exist.
	Observe(ctx context.Context, ref v1alpha1.ObjectRef) (Observation, error)
	// List returns the objects in scope that the work-item rebuild must see: the open ones,
	// and those carrying a projection marker.
	List(ctx context.Context, scope v1alpha1.Scope) ([]v1alpha1.ObjectRef, error)
	// ResolveGroup returns the provider-native user IDs (GitHub logins, not Actor IDs) in one
	// Group member source, sorted and without duplicates.
	ResolveGroup(ctx context.Context, g v1alpha1.GroupSource) ([]string, error)

	// Project writes a work item's projection onto one object. It is idempotent: projecting
	// the same Projection again changes nothing.
	Project(ctx context.Context, ref v1alpha1.ObjectRef, p Projection) error
	// Apply executes one API-side effect. Replaying an IdempotencyKey that was already
	// applied creates nothing and returns the first result, with Replayed set.
	Apply(ctx context.Context, e EffectRequest) (EffectResult, error)

	// MintGitCredential mints a git token scoped to the requested repos and permissions.
	MintGitCredential(ctx context.Context, req GitCredentialRequest) (Credential, error)
	// Health is the connection's last known health. It doesn't block.
	Health() HealthStatus
}

// IngressSink accepts raw deliveries. The provider controller's sink stores a Delivery record;
// it returns ErrDuplicateDelivery (wrapped or not) when the record already exists, and any
// other error makes the handler answer 503.
type IngressSink interface {
	Accept(ctx context.Context, ev RawEvent) error
}

// IngressSinkFunc adapts a function to an IngressSink.
type IngressSinkFunc func(ctx context.Context, ev RawEvent) error

// Accept calls f.
func (f IngressSinkFunc) Accept(ctx context.Context, ev RawEvent) error { return f(ctx, ev) }

// SecretResolver resolves a catalog secretRef (`factory serve` reads
// FACTORY_SECRET_<NAME>_<KEY>). Values are never logged.
type SecretResolver interface {
	Resolve(ctx context.Context, ref v1alpha1.SecretRef) ([]byte, error)
}

// SecretResolverFunc adapts a function to a SecretResolver.
type SecretResolverFunc func(ctx context.Context, ref v1alpha1.SecretRef) ([]byte, error)

// Resolve calls f.
func (f SecretResolverFunc) Resolve(ctx context.Context, ref v1alpha1.SecretRef) ([]byte, error) {
	return f(ctx, ref)
}

// GroupLookup returns the catalog Group named name, for trust rules that name a Group. An
// adapter takes one in its constructor; `factory serve` passes one that reads the live
// catalog, so a Conn sees Group changes without reconnecting.
type GroupLookup func(name string) (v1alpha1.GroupSpec, bool)

// StaticGroups is a GroupLookup over a fixed map, for tests.
func StaticGroups(groups map[string]v1alpha1.GroupSpec) GroupLookup {
	return func(name string) (v1alpha1.GroupSpec, bool) {
		g, ok := groups[name]
		return g, ok
	}
}

// Projection is what a work item writes onto each linked object: the GitHub marker comment
// and the factory-owned labels. Observe reads the markers back (Observation.Projections), which
// is how the work-item rebuild recreates work items after a restart.
type Projection struct {
	WorkItemID string          `json:"workItemID"`
	State      map[string]any  `json:"state,omitzero"`
	Counters   map[string]int  `json:"counters,omitzero"`
	Links      []v1alpha1.Link `json:"links,omitzero"`
	// Labels are the factory-owned labels to ensure; other labels are left untouched.
	Labels []string `json:"labels,omitzero"`
}

// EventID is the ID of the n-th Event normalised from delivery deliveryID: "<deliveryID>/<n>".
// Adapters number a delivery's Events in a stable order (sorted by object ref), so the store's
// dedupe keeps each of them once.
func EventID(deliveryID string, n int) string { return fmt.Sprintf("%s/%d", deliveryID, n) }
