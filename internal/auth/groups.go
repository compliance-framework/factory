package auth

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

// groupCache caches Conn.ResolveGroup per member source for a TTL.
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
	members = slices.Clone(members)
	g.mu.Lock()
	g.entries[key] = groupEntry{members: members, at: now}
	g.mu.Unlock()
	return members, nil
}

// isMember reports whether login on providerName is in group, through the group's sources on
// that provider. Logins compare case-insensitively, as GitHub's do. A source the provider
// doesn't know (a deleted team) has no members.
func (g *groupCache) isMember(ctx context.Context, group *v1alpha1.Group, providerName, login string) (bool, error) {
	for _, src := range group.Spec.Members {
		if src.Provider != providerName {
			continue
		}
		members, err := g.members(ctx, src)
		if errors.Is(err, provider.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if slices.ContainsFunc(members, func(m string) bool { return strings.EqualFold(m, login) }) {
			return true, nil
		}
	}
	return false, nil
}

// inGroup reports whether u is in the catalog Group name; a Group the catalog doesn't have has
// no members.
func (a *Authenticator) inGroup(ctx context.Context, name string, u *User) (bool, error) {
	cat := a.d.Catalog.Current()
	if cat == nil || u == nil {
		return false, nil
	}
	group := cat.Groups[name]
	if group == nil {
		return false, nil
	}
	return a.groups.isMember(ctx, group, u.Provider, u.Login)
}

// IsAdmin reports whether u is in the admin Group (may send commands).
func (a *Authenticator) IsAdmin(ctx context.Context, u *User) (bool, error) {
	return a.inGroup(ctx, a.cfg.AdminGroup, u)
}

// IsReader reports whether u may read: a member of the readers Group or of the admin Group.
func (a *Authenticator) IsReader(ctx context.Context, u *User) (bool, error) {
	ok, err := a.inGroup(ctx, a.cfg.ReadersGroup, u)
	if err != nil || ok {
		return ok, err
	}
	return a.IsAdmin(ctx, u)
}
