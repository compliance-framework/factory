package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/provider"
)

// Owner is a verified agent owner: the Provider instance whose OAuth client issued the user
// token, and the login on it.
type Owner struct {
	Provider string
	Login    string
}

// ID is the owner as Agent.spec.owner writes it: "<provider>:<login>".
func (o Owner) ID() string { return o.Provider + ":" + o.Login }

// OwnerVerifier exchanges an owner's GitHub user token (from the device flow `factory-agent
// register` runs) for the owner's login. The real one (RT-S2-T03) calls GET /user; the token
// is discarded once it returns and must never be stored or logged.
type OwnerVerifier interface {
	VerifyOwner(ctx context.Context, token string) (Owner, error)
}

// FakeOwners is an OwnerVerifier over a fixed token → owner map, for tests. An unknown token
// is ErrUnauthorized.
type FakeOwners map[string]Owner

// VerifyOwner implements OwnerVerifier.
func (f FakeOwners) VerifyOwner(ctx context.Context, token string) (Owner, error) {
	if err := ctx.Err(); err != nil {
		return Owner{}, err
	}
	o, ok := f[token]
	if !ok {
		return Owner{}, fmt.Errorf("fake owner verifier: unknown token: %w", ErrUnauthorized)
	}
	return o, nil
}

// groupCache caches Conn.ResolveGroup per member source for a TTL (the LLD's 10 minutes).
type groupCache struct {
	conns controllers.ConnSource
	clk   clock.Clock
	ttl   time.Duration

	mu      sync.Mutex
	entries map[string]groupEntry
}

type groupEntry struct {
	members []string
	at      time.Time
}

func newGroupCache(conns controllers.ConnSource, clk clock.Clock, ttl time.Duration) *groupCache {
	return &groupCache{conns: conns, clk: clk, ttl: ttl, entries: map[string]groupEntry{}}
}

// members resolves one member source, from the cache when it is fresh.
func (g *groupCache) members(ctx context.Context, src v1alpha1.GroupSource) ([]string, error) {
	b, err := json.Marshal(src)
	if err != nil {
		return nil, fmt.Errorf("group source: %w", err)
	}
	key := string(b)
	now := g.clk.Now()
	g.mu.Lock()
	e, ok := g.entries[key]
	g.mu.Unlock()
	if ok && now.Sub(e.at) < g.ttl {
		return e.members, nil
	}
	conn, ok := g.conns.Conn(src.Provider)
	if !ok {
		return nil, fmt.Errorf("resolve group: no connection for provider %q", src.Provider)
	}
	members, err := conn.ResolveGroup(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("resolve group on %s: %w", src.Provider, err)
	}
	g.mu.Lock()
	g.entries[key] = groupEntry{members: slices.Clone(members), at: now}
	g.mu.Unlock()
	return members, nil
}

// isMember reports whether owner is in group, through the group's sources on the owner's
// provider. Logins compare case-insensitively, as GitHub's do.
func (g *groupCache) isMember(ctx context.Context, group *v1alpha1.Group, owner Owner) (bool, error) {
	for _, src := range group.Spec.Members {
		if src.Provider != owner.Provider {
			continue
		}
		members, err := g.members(ctx, src)
		if errors.Is(err, provider.ErrNotFound) {
			continue // the team or role doesn't exist: no members
		}
		if err != nil {
			return false, err
		}
		if slices.ContainsFunc(members, func(m string) bool { return strings.EqualFold(m, owner.Login) }) {
			return true, nil
		}
	}
	return false, nil
}
