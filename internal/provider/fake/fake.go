// Package fake is an in-memory provider for tests: it keeps objects in memory, records every
// Project and Apply for assertions, emits scripted deliveries into an IngressSink and
// normalises them into Events, resolves Groups from configured membership, mints fake git
// tokens and reports a settable health.
//
// Everything is safe for concurrent use. Getters return copies.
package fake

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/provider"
)

// Defaults for Options.
const (
	DefaultType    = string(v1alpha1.ProviderTypeGitHub)
	DefaultHost    = "github.com"
	DefaultBaseURL = "https://fake.invalid"
	// DefaultTier is the tier of an actor no trust rule matches.
	DefaultTier = "anyone"
	// DeliveryPrefix starts the ID of every delivery the fake makes ("fake-1").
	DeliveryPrefix = "fake-"
)

// DefaultIdentities are the factory App logins, as on GitHub.
var DefaultIdentities = map[string]string{
	"ccf-factory[bot]":          "author",
	"ccf-factory-reviewer[bot]": "reviewer",
}

// Options configure an Adapter or a Conn. The zero value is usable.
type Options struct {
	// Type is the adapter type (default "github"), so a catalog Provider of that type
	// connects to the fake.
	Type string
	// Clock stamps deliveries, merges, tokens and health (default clock.Real).
	Clock clock.Clock
	// Groups resolves the catalog Groups named by trust rules; nil means group rules never
	// match.
	Groups provider.GroupLookup
	// Identities maps the factory Apps' logins to identities (default DefaultIdentities).
	Identities map[string]string
	// Host is the git host of minted credentials (default "github.com").
	Host string
	// BaseURL prefixes the URLs of objects and effects (default "https://fake.invalid").
	BaseURL string
}

func (o Options) withDefaults() Options {
	if o.Type == "" {
		o.Type = DefaultType
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Identities == nil {
		o.Identities = DefaultIdentities
	}
	if o.Host == "" {
		o.Host = DefaultHost
	}
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	return o
}

// Adapter is a fake provider.Adapter. Each Provider instance (by name) gets one Conn, which
// outlives reconnects, as a real provider's objects do.
type Adapter struct {
	opts     Options
	mu       sync.Mutex
	conns    map[string]*Conn
	connects int
}

var _ provider.Adapter = (*Adapter)(nil)

// NewAdapter returns an adapter with no connections.
func NewAdapter(opts Options) *Adapter {
	return &Adapter{opts: opts.withDefaults(), conns: map[string]*Conn{}}
}

// Type implements provider.Adapter.
func (a *Adapter) Type() string { return a.opts.Type }

// Connect implements provider.Adapter: it binds p to the Conn named p.Metadata.Name, creating
// it if needed. Secrets are not used.
func (a *Adapter) Connect(ctx context.Context, p v1alpha1.Provider, _ provider.SecretResolver) (provider.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Metadata.Name == "" {
		return nil, fmt.Errorf("fake: connect: provider has no name: %w", provider.ErrInvalid)
	}
	if p.Spec.Type != "" && string(p.Spec.Type) != a.opts.Type {
		return nil, fmt.Errorf("fake: connect %q: type %q, adapter is %q: %w",
			p.Metadata.Name, p.Spec.Type, a.opts.Type, provider.ErrInvalid)
	}
	c := a.Conn(p.Metadata.Name)
	c.bind(p)
	a.mu.Lock()
	a.connects++
	a.mu.Unlock()
	return c, nil
}

// Conn returns the Conn for the Provider named name, creating it if needed, so a test can seed
// it before the code under test connects.
func (a *Adapter) Conn(name string) *Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.conns[name]
	if !ok {
		c = NewConn(v1alpha1.Provider{
			TypeMeta: v1alpha1.TypeMeta{Kind: "Provider"},
			Metadata: v1alpha1.ObjectMeta{Name: name},
			Spec:     v1alpha1.ProviderSpec{Type: v1alpha1.ProviderType(a.opts.Type)},
		}, a.opts)
		a.conns[name] = c
	}
	return c
}

// Connects returns how many times Connect succeeded.
func (a *Adapter) Connects() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connects
}

// Method names a Conn method, for FailNext.
type Method string

// Methods that can be made to fail.
const (
	MethodRecover           Method = "Recover"
	MethodNormalise         Method = "Normalise"
	MethodObserve           Method = "Observe"
	MethodList              Method = "List"
	MethodResolveGroup      Method = "ResolveGroup"
	MethodProject           Method = "Project"
	MethodApply             Method = "Apply"
	MethodMintGitCredential Method = "MintGitCredential"
)

// Conn is a fake provider.Conn for one Provider instance.
type Conn struct {
	opts Options

	mu  sync.Mutex
	p   v1alpha1.Provider
	seq int // delivery, comment, issue and token numbers

	objects map[string]*provider.Observation // by ref.Ref

	teams      map[string][]string
	orgRoles   map[v1alpha1.OrgRole][]string
	userGroups map[string][]string

	deliveries []provider.RawEvent
	projects   []ProjectCall
	applies    []ApplyCall
	applied    map[string]provider.EffectResult // by idempotency key
	mints      []MintCall
	health     *provider.HealthStatus
	faults     map[Method][]error
}

var _ provider.Conn = (*Conn)(nil)

// NewConn returns a Conn for p, without an Adapter.
func NewConn(p v1alpha1.Provider, opts Options) *Conn {
	return &Conn{
		opts:       opts.withDefaults(),
		p:          p,
		objects:    map[string]*provider.Observation{},
		teams:      map[string][]string{},
		orgRoles:   map[v1alpha1.OrgRole][]string{},
		userGroups: map[string][]string{},
		applied:    map[string]provider.EffectResult{},
		faults:     map[Method][]error{},
	}
}

func (c *Conn) bind(p v1alpha1.Provider) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.p = p
}

// Name is the Provider instance's name.
func (c *Conn) Name() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.p.Metadata.Name
}

// providerType is the Provider's spec.type, or the adapter type when the spec has none.
func (c *Conn) providerTypeLocked() v1alpha1.ProviderType {
	if c.p.Spec.Type != "" {
		return c.p.Spec.Type
	}
	return v1alpha1.ProviderType(c.opts.Type)
}

// FailNext makes the next call of m return err (once per call; queued in order).
func (c *Conn) FailNext(m Method, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.faults[m] = append(c.faults[m], err)
}

// faultLocked pops the next injected error for m.
func (c *Conn) faultLocked(m Method) error {
	q := c.faults[m]
	if len(q) == 0 {
		return nil
	}
	c.faults[m] = q[1:]
	return q[0]
}

// begin checks ctx, takes the lock and pops an injected fault. On a nil error the caller
// holds the lock and must unlock it.
func (c *Conn) begin(ctx context.Context, m Method) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if err := c.faultLocked(m); err != nil {
		c.mu.Unlock()
		return err
	}
	return nil
}

// ---- Objects ---------------------------------------------------------------------------------

// Seed adds or replaces objects. Ref.Provider defaults to this provider, Ref.Kind to
// "issue", State to open; an issue gets an empty Issue and a PR an empty PR when unset.
func (c *Conn) Seed(objs ...provider.Observation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, o := range objs {
		o = cloneObservation(o)
		if err := c.normaliseRefLocked(&o.Ref); err != nil {
			return fmt.Errorf("fake: seed: %w", err)
		}
		if o.Ref.Kind == "" {
			o.Ref.Kind = provider.KindIssue
		}
		if o.State == "" {
			o.State = provider.StateOpen
		}
		switch o.Ref.Kind {
		case provider.KindIssue:
			if o.Issue == nil {
				o.Issue = &provider.IssueObservation{}
			}
		case provider.KindPR:
			if o.PR == nil {
				o.PR = &provider.PRObservation{}
			}
		}
		if o.UpdatedAt.IsZero() {
			o.UpdatedAt = c.opts.Clock.Now()
		}
		c.objects[o.Ref.Ref] = &o
	}
	return nil
}

// Object returns a copy of the object at ref.
func (c *Conn) Object(ref string) (provider.Observation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.objects[ref]
	if !ok {
		return provider.Observation{}, false
	}
	return cloneObservation(*o), true
}

// Update changes an object out of band (a human merging a PR, say); ErrNotFound when there is
// none at ref.
func (c *Conn) Update(ref string, fn func(*provider.Observation)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.objects[ref]
	if !ok {
		return fmt.Errorf("fake: update %q: %w", ref, provider.ErrNotFound)
	}
	fn(o)
	o.UpdatedAt = c.opts.Clock.Now()
	return nil
}

// normaliseRefLocked fills ref.Provider and rejects a ref for another provider or without Ref.
func (c *Conn) normaliseRefLocked(ref *v1alpha1.ObjectRef) error {
	if ref.Ref == "" {
		return fmt.Errorf("object ref has no ref: %w", provider.ErrInvalid)
	}
	switch ref.Provider {
	case "":
		ref.Provider = c.p.Metadata.Name
	case c.p.Metadata.Name:
	default:
		return fmt.Errorf("object %s is on provider %q, not %q: %w",
			ref.Ref, ref.Provider, c.p.Metadata.Name, provider.ErrInvalid)
	}
	return nil
}

// lookupLocked returns the object at ref; ErrNotFound when there is none or its kind differs.
func (c *Conn) lookupLocked(ref v1alpha1.ObjectRef) (*provider.Observation, error) {
	if err := c.normaliseRefLocked(&ref); err != nil {
		return nil, err
	}
	o, ok := c.objects[ref.Ref]
	if !ok || (ref.Kind != "" && ref.Kind != o.Ref.Kind) {
		return nil, fmt.Errorf("%s %s: %w", ref.Kind, ref.Ref, provider.ErrNotFound)
	}
	return o, nil
}

// objectURL is the fake web URL of an object: <base>/<owner>/<repo>/<issues|pull>/<n>.
func (c *Conn) objectURL(ref v1alpha1.ObjectRef) string {
	repo, num, ok := strings.Cut(ref.Ref, "#")
	if !ok {
		return c.opts.BaseURL + "/" + ref.Ref
	}
	kind := "issues"
	if ref.Kind == provider.KindPR {
		kind = "pull"
	}
	return fmt.Sprintf("%s/%s/%s/%s", c.opts.BaseURL, repo, kind, num)
}

// RepoOf returns the repo of a ref ("owner/name#42" → "owner/name"), or "" for a ref without
// a number.
func RepoOf(ref string) string {
	repo, _, ok := strings.Cut(ref, "#")
	if !ok {
		return ""
	}
	return repo
}

// ---- Health ----------------------------------------------------------------------------------

// SetHealth sets what Health returns; a zero CheckedAt is stamped with the clock on read.
func (c *Conn) SetHealth(h provider.HealthStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health = &h
}

// Health implements provider.Conn: ok unless SetHealth said otherwise.
func (c *Conn) Health() provider.HealthStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := provider.HealthStatus{Status: provider.HealthOK}
	if c.health != nil {
		h = *c.health
	}
	if h.CheckedAt.IsZero() {
		h.CheckedAt = c.opts.Clock.Now()
	}
	return h
}

// sortedUnique returns a sorted copy of ss without duplicates (never nil).
func sortedUnique(ss []string) []string {
	out := append([]string{}, ss...)
	slices.Sort(out)
	return slices.Compact(out)
}
