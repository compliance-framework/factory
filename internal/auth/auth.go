// Package auth authenticates the dispatcher's API callers (design/workstreams/dispatcher-runtime/
// LLD/api-server.md §Auth):
//
//   - agents: `Authorization: Bearer <agentToken>`, checked by the scheduler;
//   - the CLI: `Authorization: Bearer <cliToken>`, a static token compared in constant time;
//   - the web UI: the GitHub OAuth web flow, then a server-side session cookie (`HttpOnly`,
//     `Secure`, `SameSite=Lax`, 12 h). Reading needs the readers Group (or the admin Group);
//     commands need the admin Group (`maintainers`), resolved through Conn.ResolveGroup and
//     cached;
//   - agent owners: GitHubOwners, the scheduler's OwnerVerifier (a GitHub user token → login).
//
// No token, code or cookie value is ever logged or stored in clear: sessions are stored by the
// hash of their id, and GitHub user tokens are dropped as soon as the login is known.
package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/store"
)

// Defaults for Config.
const (
	DefaultSessionTTL    = 12 * time.Hour
	DefaultStateTTL      = 10 * time.Minute
	DefaultGroupCacheTTL = 10 * time.Minute
	DefaultAdminGroup    = "maintainers"
	DefaultReadersGroup  = "readers"
	DefaultAuthorizeURL  = "https://github.com/login/oauth/authorize"
	DefaultTokenURL      = "https://github.com/login/oauth/access_token"
	DefaultAPIBase       = "https://api.github.com"
	// MaxPendingLogins bounds the logins started and not finished (one state each).
	MaxPendingLogins = 10_000
)

// Cookie names.
const (
	// SessionCookie is the UI session (the spec's uiSession security scheme).
	SessionCookie = "factory_session"
	// StateCookie binds an OAuth login's state to the browser that started it.
	StateCookie = "factory_oauth_state"
)

// DefaultScopes are the OAuth scopes of the UI login and of the agent owners' device flow:
// GET /user is all either needs (Group membership is resolved with the App's credentials).
var DefaultScopes = []string{"read:user"}

// Errors. The API server maps them to statuses with errors.Is.
var (
	// ErrUnauthenticated: no credential, or a wrong, expired or malformed one (401).
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden: authenticated, but not allowed (403): not a member of the Group the route
	// needs, or a cross-site request.
	ErrForbidden = errors.New("forbidden")
	// ErrStateMismatch: an OAuth callback whose state isn't the one bound to this browser, or
	// is unknown or expired (400).
	ErrStateMismatch = errors.New("oauth state mismatch")
	// ErrBadReturnTo: a returnTo that isn't a same-origin path (400).
	ErrBadReturnTo = errors.New("returnTo must be a same-origin path")
	// ErrNotMember: a GitHub user who is in neither the readers nor the admin Group (403).
	ErrNotMember = fmt.Errorf("not a member of the factory's Groups: %w", ErrForbidden)
	// ErrOAuth: GitHub refused the code exchange or the user lookup (502 unless the token was
	// refused, which is ErrUnauthenticated).
	ErrOAuth = errors.New("github oauth failed")
)

// OAuthConfig is the GitHub OAuth App (or the author App's user-to-server client) the UI login
// and the agent owners' device flow use.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	// AuthorizeURL, TokenURL and APIBase default to github.com's; tests point them at an
	// httptest server.
	AuthorizeURL string
	TokenURL     string
	APIBase      string
	// RedirectURL is the callback: <public URL>/api/v1/auth/callback.
	RedirectURL string
	// Scopes default to DefaultScopes.
	Scopes []string
	// DeviceScopes are the agent owners' device-flow scopes (getDeviceConfig). Default Scopes.
	DeviceScopes []string
}

// Config configures an Authenticator. Zero fields take the defaults.
type Config struct {
	// CLIToken is FACTORY_CLI_TOKEN; empty disables the CLI token.
	CLIToken string
	OAuth    OAuthConfig
	// Provider is the catalog Provider the GitHub logins belong to (e.g. github-public): the
	// Groups' member sources on it are the ones checked, and owners are "<Provider>:<login>".
	Provider string
	// AdminGroup may send commands (default "maintainers"); ReadersGroup may read (default
	// "readers"). A Group missing from the catalog has no members, so without a readers Group
	// only admins can sign in (fail closed).
	AdminGroup   string
	ReadersGroup string
	// SessionKey (FACTORY_SESSION_KEY, 32 bytes) signs the session cookie; when empty a random
	// key is used, so sessions don't survive a restart (nor does the memory store).
	SessionKey []byte
	SessionTTL time.Duration
	StateTTL   time.Duration
	// GroupCacheTTL caches Conn.ResolveGroup per member source (default 10 min, the LLD's).
	GroupCacheTTL time.Duration
	// HTTPClient calls GitHub (default http.DefaultClient).
	HTTPClient *http.Client
	Clock      clock.Clock
	Logger     *slog.Logger
	// Rand is the source of session ids and states (default crypto/rand).
	Rand io.Reader
}

func (c Config) withDefaults() Config {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.SessionTTL, DefaultSessionTTL)
	def(&c.StateTTL, DefaultStateTTL)
	def(&c.GroupCacheTTL, DefaultGroupCacheTTL)
	if c.AdminGroup == "" {
		c.AdminGroup = DefaultAdminGroup
	}
	if c.ReadersGroup == "" {
		c.ReadersGroup = DefaultReadersGroup
	}
	if c.OAuth.AuthorizeURL == "" {
		c.OAuth.AuthorizeURL = DefaultAuthorizeURL
	}
	if c.OAuth.TokenURL == "" {
		c.OAuth.TokenURL = DefaultTokenURL
	}
	if c.OAuth.APIBase == "" {
		c.OAuth.APIBase = DefaultAPIBase
	}
	c.OAuth.APIBase = strings.TrimRight(c.OAuth.APIBase, "/")
	if len(c.OAuth.Scopes) == 0 {
		c.OAuth.Scopes = DefaultScopes
	}
	if len(c.OAuth.DeviceScopes) == 0 {
		c.OAuth.DeviceScopes = c.OAuth.Scopes
	}
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}
	if c.Clock == nil {
		c.Clock = clock.Real{}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Rand == nil {
		c.Rand = rand.Reader
	}
	return c
}

// AgentAuthenticator checks an agent token and returns the agent id (scheduler.Authenticate).
// Its errors for a bad token wrap scheduler.ErrUnauthorized.
type AgentAuthenticator interface {
	Authenticate(ctx context.Context, token string) (string, error)
}

// Deps are what the Authenticator reads.
type Deps struct {
	// Store keeps the sessions.
	Store store.Store
	// Catalog gives the Groups; Conns resolve them.
	Catalog controllers.CatalogProvider
	Conns   controllers.ConnSource
	// Agents checks agent tokens.
	Agents AgentAuthenticator
}

// Authenticator authenticates requests. It is safe for concurrent use.
type Authenticator struct {
	cfg    Config
	d      Deps
	clk    clock.Clock
	log    *slog.Logger
	key    []byte
	groups *groupCache
	users  *userClient

	mu      sync.Mutex
	pending map[string]pendingLogin // state → login in progress
}

// New returns an Authenticator.
func New(cfg Config, d Deps) (*Authenticator, error) {
	switch {
	case d.Store == nil:
		return nil, errors.New("auth: no store")
	case d.Catalog == nil:
		return nil, errors.New("auth: no catalog")
	case d.Conns == nil:
		return nil, errors.New("auth: no provider connections")
	case d.Agents == nil:
		return nil, errors.New("auth: no agent authenticator")
	}
	cfg = cfg.withDefaults()
	a := &Authenticator{
		cfg:     cfg,
		d:       d,
		clk:     cfg.Clock,
		log:     cfg.Logger.With("component", "auth"),
		key:     cfg.SessionKey,
		groups:  newGroupCache(d.Conns, cfg.Clock, cfg.GroupCacheTTL),
		users:   &userClient{http: cfg.HTTPClient, apiBase: cfg.OAuth.APIBase},
		pending: map[string]pendingLogin{},
	}
	if len(a.key) == 0 {
		k, err := randomBytes(cfg.Rand, 32)
		if err != nil {
			return nil, fmt.Errorf("auth: session key: %w", err)
		}
		a.key = k
	}
	return a, nil
}

// DeviceConfig is getDeviceConfig: the OAuth client id and scopes of the owners' device flow.
func (a *Authenticator) DeviceConfig() (clientID string, scopes []string) {
	return a.cfg.OAuth.ClientID, append([]string(nil), a.cfg.OAuth.DeviceScopes...)
}

// Provider is the catalog Provider the GitHub logins belong to.
func (a *Authenticator) Provider() string { return a.cfg.Provider }

func randomBytes(r io.Reader, n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, fmt.Errorf("random: %w", err)
	}
	return b, nil
}
