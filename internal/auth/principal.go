package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/compliance-framework/factory/internal/scheduler"
)

// Kind is what kind of caller a Principal is.
type Kind string

// Principal kinds.
const (
	KindAgent Kind = "agent"
	KindUser  Kind = "user"
	KindCLI   Kind = "cli"
)

// CLIActor is the actor id of commands sent with the CLI token.
const CLIActor = "factory:cli"

// Principal is an authenticated caller.
type Principal struct {
	Kind Kind
	// AgentID is set for KindAgent.
	AgentID string
	// User is set for KindUser.
	User *User
}

// ActorID is who the principal is, as an Actor id: "<provider>:<login>" for a user, the
// agent id, or CLIActor.
func (p Principal) ActorID() string {
	switch p.Kind {
	case KindUser:
		return p.User.Provider + ":" + p.User.Login
	case KindAgent:
		return p.AgentID
	}
	return CLIActor
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal ctx carries.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// BearerToken returns the request's bearer token ("" when there is none).
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// IsCLIToken reports whether tok is the CLI token, in constant time (over hashes, so not even
// the length leaks). It is false when no CLI token is configured.
func (a *Authenticator) IsCLIToken(tok string) bool {
	if a.cfg.CLIToken == "" || tok == "" {
		return false
	}
	want, got := sha256.Sum256([]byte(a.cfg.CLIToken)), sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// Agent authenticates the request's agent token. A missing or wrong token (or the CLI token)
// is ErrUnauthenticated.
func (a *Authenticator) Agent(ctx context.Context, r *http.Request) (Principal, error) {
	tok := BearerToken(r)
	if tok == "" {
		return Principal{}, fmt.Errorf("no bearer token: %w", ErrUnauthenticated)
	}
	if !strings.HasPrefix(tok, "agt_") {
		return Principal{}, fmt.Errorf("not an agent token: %w", ErrUnauthenticated)
	}
	id, err := a.d.Agents.Authenticate(ctx, tok)
	switch {
	case errors.Is(err, scheduler.ErrUnauthorized):
		return Principal{}, fmt.Errorf("agent token: %w: %w", ErrUnauthenticated, err)
	case err != nil:
		return Principal{}, fmt.Errorf("agent token: %w", err)
	}
	return Principal{Kind: KindAgent, AgentID: id}, nil
}

// CLI authenticates the request's CLI token.
func (a *Authenticator) CLI(r *http.Request) (Principal, bool) {
	if a.IsCLIToken(BearerToken(r)) {
		return Principal{Kind: KindCLI}, true
	}
	return Principal{}, false
}
