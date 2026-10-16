package fake

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/patterns"
	"github.com/compliance-framework/factory/internal/provider"
)

// Observe implements provider.Conn: a copy of the seeded (and since changed) object.
func (c *Conn) Observe(ctx context.Context, ref v1alpha1.ObjectRef) (provider.Observation, error) {
	if err := c.begin(ctx, MethodObserve); err != nil {
		return provider.Observation{}, err
	}
	defer c.mu.Unlock()
	o, err := c.lookupLocked(ref)
	if err != nil {
		return provider.Observation{}, fmt.Errorf("fake: observe: %w", err)
	}
	return cloneObservation(*o), nil
}

// List implements provider.Conn: the objects whose repo matches scope.repos (any repo when
// empty) and no scope.exclude glob, that are open or carry a projection, sorted by ref.
// Channels and branches are not modelled, nor GitHub's 90-day window.
func (c *Conn) List(ctx context.Context, scope v1alpha1.Scope) ([]v1alpha1.ObjectRef, error) {
	include, err := compileGlobs(scope.Repos)
	if err != nil {
		return nil, fmt.Errorf("fake: list: scope.repos: %w: %w", err, provider.ErrInvalid)
	}
	exclude, err := compileGlobs(scope.Exclude)
	if err != nil {
		return nil, fmt.Errorf("fake: list: scope.exclude: %w: %w", err, provider.ErrInvalid)
	}
	if err := c.begin(ctx, MethodList); err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	var out []v1alpha1.ObjectRef
	for _, o := range c.objects {
		repo := RepoOf(o.Ref.Ref)
		if (include != nil && !include.Match(repo)) || (exclude != nil && exclude.Match(repo)) {
			continue
		}
		if o.State == provider.StateOpen || len(o.Projections) > 0 {
			out = append(out, o.Ref)
		}
	}
	slices.SortFunc(out, func(a, b v1alpha1.ObjectRef) int { return strings.Compare(a.Ref, b.Ref) })
	return out, nil
}

// compileGlobs compiles a scope's glob list; nil for an empty list.
func compileGlobs(globs []string) (patterns.Pattern, error) {
	if len(globs) == 0 {
		return nil, nil
	}
	return patterns.Compile(globs)
}

// SetTeam sets the members of a team ("org/team-slug").
func (c *Conn) SetTeam(team string, logins ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.teams[team] = sortedUnique(logins)
}

// SetOrgRole sets the members of an org with a role (e.g. the admins).
func (c *Conn) SetOrgRole(org, role string, logins ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.orgRoles[v1alpha1.OrgRole{Org: org, Role: role}] = sortedUnique(logins)
}

// SetUserGroup sets the members of a user group (Slack).
func (c *Conn) SetUserGroup(name string, logins ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userGroups[name] = sortedUnique(logins)
}

// ResolveGroup implements provider.Conn from the membership set with SetTeam, SetOrgRole and
// SetUserGroup; users are returned as listed. A team, org role or user group that was never
// set is ErrNotFound; a source for another provider, or with not exactly one source, is
// ErrInvalid.
func (c *Conn) ResolveGroup(ctx context.Context, g v1alpha1.GroupSource) ([]string, error) {
	if err := g.Validate(); err != nil {
		return nil, fmt.Errorf("fake: resolve group: %w: %w", err, provider.ErrInvalid)
	}
	if err := c.begin(ctx, MethodResolveGroup); err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	if g.Provider != c.p.Metadata.Name {
		return nil, fmt.Errorf("fake: resolve group: source is for provider %q, not %q: %w",
			g.Provider, c.p.Metadata.Name, provider.ErrInvalid)
	}
	var (
		members []string
		ok      bool
		what    string
	)
	switch {
	case g.Team != "":
		members, ok = c.teams[g.Team]
		what = "team " + g.Team
	case g.OrgRole != nil:
		members, ok = c.orgRoles[*g.OrgRole]
		what = fmt.Sprintf("org %s role %s", g.OrgRole.Org, g.OrgRole.Role)
	case g.UserGroup != "":
		members, ok = c.userGroups[g.UserGroup]
		what = "user group " + g.UserGroup
	default:
		return sortedUnique(g.Users), nil
	}
	if !ok {
		return nil, fmt.Errorf("fake: resolve group: %s: %w", what, provider.ErrNotFound)
	}
	return slices.Clone(members), nil
}
