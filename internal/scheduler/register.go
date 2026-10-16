package scheduler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/store"
)

// tokenBytes is the agent token's random part (the LLD's 32 bytes).
const tokenBytes = 32

// Agent labels.
const (
	LabelOwner = "factory.ccf.dev/owner"
	LabelPool  = "factory.ccf.dev/pool"
)

var hostRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// AgentID is the agent named for an owner login and a host: agt_<login>-<host>, lowercase.
func AgentID(login, host string) string { return "agt_" + strings.ToLower(login) + "-" + host }

// hashSecret is how a token's secret part is stored: "sha256:<hex>".
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// equalSecret compares two secrets in constant time (over their hashes, so not even the
// length leaks). An empty secret never matches.
func equalSecret(want []byte, got string) bool {
	if len(want) == 0 || got == "" {
		return false
	}
	a, b := sha256.Sum256(want), sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func validateCapabilities(c v1alpha1.AgentCapabilities) error {
	if len(c.Harnesses) == 0 {
		return invalid("capabilities.harnesses", "at least one harness")
	}
	for i, h := range c.Harnesses {
		if h.Name == "" {
			return invalid(fmt.Sprintf("capabilities.harnesses[%d].name", i), "required")
		}
	}
	return nil
}

func validateLending(l *v1alpha1.Lending) error {
	if l == nil {
		return nil
	}
	switch {
	case l.MaxConcurrent < 0:
		return invalid("lending.maxConcurrent", "must not be negative")
	case l.MaxRunsPerDay < 0:
		return invalid("lending.maxRunsPerDay", "must not be negative")
	case l.MaxTokensPerDay < 0:
		return invalid("lending.maxTokensPerDay", "must not be negative")
	}
	if l.Hours != "" {
		if _, err := ParseHours(l.Hours); err != nil {
			return invalid("lending.hours", "%v", err)
		}
	}
	return nil
}

// Register is registerAgent (LLD §Registration):
//
//  1. the join token matches the pool's admission.tokenRef secret (constant-time);
//  2. the owner token is exchanged for a login by the OwnerVerifier, then dropped;
//  3. the owner is in the pool's admission.owners Group (ResolveGroup, cached 10 min);
//  4. the capabilities offer one of Pool.require.harnesses;
//  5. the Agent agt_<owner>-<host> is created or replaced, and a new agent token (32 random
//     bytes, stored hashed, TTL 24 h renewed by use) is returned once.
func (s *Scheduler) Register(ctx context.Context, req RegisterRequest) (*Registration, error) {
	ownerToken := req.OwnerToken
	req.OwnerToken = "" // never kept past the exchange
	switch {
	case req.Pool == "":
		return nil, invalid("pool", "required")
	case !hostRE.MatchString(req.Host):
		return nil, invalid("host", "%q must match %s", req.Host, hostRE)
	case req.AgentVersion == "":
		return nil, invalid("agentVersion", "required")
	}
	if err := validateCapabilities(req.Capabilities); err != nil {
		return nil, err
	}
	if err := validateLending(req.Lending); err != nil {
		return nil, err
	}
	cat := s.d.Catalog.Current()
	if cat == nil {
		return nil, errNoCatalog
	}
	pool := cat.Pools[req.Pool]
	if pool == nil || pool.Spec.Mode != v1alpha1.PoolRegistered || pool.Spec.Admission == nil {
		return nil, invalid("pool", "%q is not a registered pool", req.Pool)
	}
	adm := pool.Spec.Admission

	joinSecret, err := s.d.Secrets.Resolve(ctx, adm.TokenRef)
	if err != nil {
		return nil, fmt.Errorf("pool %s: resolve join token: %w", req.Pool, err)
	}
	if !equalSecret(joinSecret, req.JoinToken) {
		return nil, fmt.Errorf("pool %s: wrong join token: %w", req.Pool, ErrUnauthorized)
	}
	if ownerToken == "" {
		return nil, fmt.Errorf("no owner token: %w", ErrUnauthorized)
	}
	owner, err := s.d.Owners.VerifyOwner(ctx, ownerToken)
	if err != nil {
		return nil, fmt.Errorf("verify owner: %w", err)
	}
	if owner.Login == "" || owner.Provider == "" {
		return nil, fmt.Errorf("verify owner: no login: %w", ErrUnauthorized)
	}
	group := cat.Groups[adm.Owners]
	if group == nil {
		return nil, fmt.Errorf("pool %s: owners group %q is not in the catalog: %w", req.Pool, adm.Owners, ErrForbidden)
	}
	member, err := s.groups.isMember(ctx, group, owner)
	if err != nil {
		return nil, fmt.Errorf("pool %s: %w", req.Pool, err)
	}
	if !member {
		return nil, fmt.Errorf("pool %s: %s is not in group %s: %w", req.Pool, owner.ID(), adm.Owners, ErrForbidden)
	}
	if r := pool.Spec.Require; r != nil && len(r.Harnesses) > 0 &&
		!slices.ContainsFunc(req.Capabilities.Harnesses, func(h v1alpha1.HarnessOffer) bool { return slices.Contains(r.Harnesses, h.Name) }) {
		return nil, fmt.Errorf("pool %s requires one of the harnesses %v: %w", req.Pool, r.Harnesses, ErrForbidden)
	}

	id := AgentID(owner.Login, req.Host)
	if err := s.putAgent(ctx, id, req, owner); err != nil {
		return nil, err
	}
	secretBytes, err := s.randomBytes(tokenBytes)
	if err != nil {
		return nil, err
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	now := s.clk.Now()
	tok := &AgentToken{
		TypeMeta:  v1alpha1.TypeMeta{Kind: KindAgentToken},
		Metadata:  v1alpha1.ObjectMeta{Name: id},
		Hash:      hashSecret(secret),
		ExpiresAt: now.Add(s.opts.TokenTTL),
		RenewedAt: now,
	}
	if err := s.putToken(ctx, tok); err != nil {
		return nil, err
	}
	if _, err := s.syncAgent(ctx, id, nil); err != nil {
		return nil, err
	}
	s.wake()
	s.log.InfoContext(ctx, "agent registered", "agent", id, "owner", owner.ID(), "pool", req.Pool, "version", req.AgentVersion)
	return &Registration{AgentID: id, AgentToken: id + "." + secret, ExpiresAt: tok.ExpiresAt}, nil
}

// putAgent creates or replaces the Agent: the new spec, and a fresh Ready status that keeps
// only the derived fields (running, today).
func (s *Scheduler) putAgent(ctx context.Context, id string, req RegisterRequest, owner Owner) error {
	spec := v1alpha1.AgentSpec{Pool: req.Pool, Owner: owner.ID(), Capabilities: req.Capabilities, Lending: req.Lending}
	labels := map[string]string{LabelOwner: strings.ToLower(owner.Login), LabelPool: req.Pool}
	for range maxCAS {
		_, err := s.updateAgent(ctx, id, func(a *v1alpha1.Agent) (bool, error) {
			a.Spec, a.Metadata.Labels = spec, labels
			a.Status.Phase, a.Status.ThrottledUntil = v1alpha1.AgentReady, nil
			return true, nil
		})
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		err = s.d.Store.Create(ctx, &v1alpha1.Agent{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindAgent},
			Metadata: v1alpha1.ObjectMeta{Name: id, Labels: labels},
			Spec:     spec,
			Status:   &v1alpha1.AgentStatus{Phase: v1alpha1.AgentReady},
		})
		if !errors.Is(err, store.ErrAlreadyExists) {
			if err != nil {
				return fmt.Errorf("create agent %s: %w", id, err)
			}
			return nil
		}
	}
	return fmt.Errorf("put agent %s: %w", id, store.ErrConflict)
}

// putToken creates or replaces an agent's token record.
func (s *Scheduler) putToken(ctx context.Context, tok *AgentToken) error {
	k := store.KeyOf(tok)
	for range maxCAS {
		var cur AgentToken
		err := s.d.Store.Get(ctx, k, &cur)
		switch {
		case errors.Is(err, store.ErrNotFound):
			tok.Metadata.ResourceVersion = ""
			err = s.d.Store.Create(ctx, tok)
			if errors.Is(err, store.ErrAlreadyExists) {
				continue
			}
		case err != nil:
		default:
			tok.Metadata.ResourceVersion = cur.Metadata.ResourceVersion
			err = s.d.Store.Update(ctx, tok)
			if errors.Is(err, store.ErrConflict) {
				continue
			}
		}
		if err != nil {
			return fmt.Errorf("store agent token %s: %w", tok.Metadata.Name, err)
		}
		return nil
	}
	return fmt.Errorf("store agent token %s: %w", tok.Metadata.Name, store.ErrConflict)
}

// Authenticate checks an agent token ("<agentId>.<secret>") against its stored hash and
// returns the agent id. It renews the token (now + TokenTTL) at most every TokenRenewEvery:
// heartbeats and polls keep a live agent's token valid.
func (s *Scheduler) Authenticate(ctx context.Context, token string) (string, error) {
	id, secret, ok := strings.Cut(token, ".")
	if !ok || !strings.HasPrefix(id, "agt_") || secret == "" {
		return "", fmt.Errorf("malformed agent token: %w", ErrUnauthorized)
	}
	var tok AgentToken
	err := s.d.Store.Get(ctx, store.Key{Kind: KindAgentToken, Name: id}, &tok)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("unknown agent token: %w", ErrUnauthorized)
	}
	if err != nil {
		return "", fmt.Errorf("get agent token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(tok.Hash)) != 1 {
		return "", fmt.Errorf("wrong agent token: %w", ErrUnauthorized)
	}
	now := s.clk.Now()
	if !now.Before(tok.ExpiresAt) {
		return "", fmt.Errorf("agent token expired: %w", ErrUnauthorized)
	}
	if now.Sub(tok.RenewedAt) >= s.opts.TokenRenewEvery {
		tok.ExpiresAt, tok.RenewedAt = now.Add(s.opts.TokenTTL), now
		if err := s.d.Store.Update(ctx, &tok); err != nil && !errors.Is(err, store.ErrConflict) {
			s.log.WarnContext(ctx, "renew agent token", "agent", id, "err", err)
		}
	}
	return id, nil
}

// UpdateAgent is updateAgent (PATCH /agents/{id}): the agent reports Ready, Throttled{until},
// Draining or Offline, and/or re-sends its lending.
func (s *Scheduler) UpdateAgent(ctx context.Context, agentID string, u AgentUpdate) (*v1alpha1.Agent, error) {
	switch u.Phase {
	case "":
		if u.Lending == nil {
			return nil, invalid("phase", "give a phase or a lending")
		}
	case v1alpha1.AgentReady, v1alpha1.AgentDraining, v1alpha1.AgentOffline:
	case v1alpha1.AgentThrottled:
		if u.Until.IsZero() {
			return nil, invalid("until", "required with Throttled")
		}
	default:
		return nil, invalid("phase", "%q is not one of Ready, Throttled, Draining, Offline", u.Phase)
	}
	if err := validateLending(u.Lending); err != nil {
		return nil, err
	}
	a, err := s.syncAgent(ctx, agentID, func(a *v1alpha1.Agent) bool {
		if u.Lending != nil {
			l := *u.Lending
			a.Spec.Lending = &l
		}
		switch u.Phase {
		case v1alpha1.AgentReady:
			a.Status.Phase, a.Status.ThrottledUntil = v1alpha1.AgentReady, nil
		case v1alpha1.AgentThrottled:
			until := u.Until
			a.Status.Phase, a.Status.ThrottledUntil = v1alpha1.AgentThrottled, &until
		case v1alpha1.AgentDraining, v1alpha1.AgentOffline:
			a.Status.Phase = u.Phase
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	s.wake()
	return a, nil
}

// Deregister is deregisterAgent: it revokes the token, marks the agent Offline and abandons
// its running leases (reason shutdown: retried per the queue's policy).
func (s *Scheduler) Deregister(ctx context.Context, agentID string) error {
	if err := s.d.Store.Delete(ctx, store.Key{Kind: KindAgentToken, Name: agentID}); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("revoke agent token %s: %w", agentID, err)
	}
	if _, err := s.syncAgent(ctx, agentID, func(a *v1alpha1.Agent) bool {
		a.Status.Phase = v1alpha1.AgentOffline
		return true
	}); err != nil {
		return err
	}
	var leases []*Lease
	if _, err := s.d.Store.List(ctx, KindLease, store.ListOptions{
		LabelSelector: &v1alpha1.LabelSelector{MatchLabels: map[string]string{LabelAgent: agentID}},
	}, &leases); err != nil {
		return fmt.Errorf("list leases of %s: %w", agentID, err)
	}
	for _, l := range leases {
		if l.Status.End != "" {
			continue
		}
		if _, err := s.leasedTask(ctx, l); err != nil {
			continue // revoked
		}
		err := s.d.Runs.Abandon(ctx, l.Spec.Task, controllers.Abandonment{Reason: controllers.AbandonShutdown, Message: "agent deregistered"})
		if err != nil && !errors.Is(err, controllers.ErrNotLeased) && !errors.Is(err, controllers.ErrUnknownTask) {
			return err
		}
		s.endLease(ctx, l, LeaseAbandoned)
	}
	s.log.InfoContext(ctx, "agent deregistered", "agent", agentID)
	s.wake()
	return nil
}
