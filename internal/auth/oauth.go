package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// statePath is where the state cookie is sent: the auth routes only.
const statePath = "/api/v1/auth"

// pendingLogin is a login started and not yet finished.
type pendingLogin struct {
	returnTo string
	expires  time.Time
}

// SafeReturnTo validates a returnTo: it must be a same-origin absolute path ("/board?x=1"),
// not a URL, a scheme-relative "//host" or a backslash trick ("/\host"). "" is "/".
func SafeReturnTo(s string) (string, error) {
	if s == "" {
		return "/", nil
	}
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || strings.ContainsAny(s, "\\\r\n\t") {
		return "", ErrBadReturnTo
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", ErrBadReturnTo
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return "", ErrBadReturnTo
	}
	return s, nil
}

// BeginLogin starts the OAuth web flow: it returns the GitHub authorize URL to redirect to and
// the cookie that binds the login's state to this browser (CSRF). returnTo must be a
// same-origin path (ErrBadReturnTo).
func (a *Authenticator) BeginLogin(ctx context.Context, returnTo string) (string, *http.Cookie, error) {
	rt, err := SafeReturnTo(returnTo)
	if err != nil {
		return "", nil, err
	}
	if a.cfg.OAuth.ClientID == "" {
		return "", nil, fmt.Errorf("oauth: no client id configured: %w", ErrOAuth)
	}
	b, err := randomBytes(a.cfg.Rand, 32)
	if err != nil {
		return "", nil, err
	}
	state := base64.RawURLEncoding.EncodeToString(b)
	now := a.clk.Now()
	a.mu.Lock()
	for s, p := range a.pending {
		if !now.Before(p.expires) {
			delete(a.pending, s)
		}
	}
	if len(a.pending) >= MaxPendingLogins {
		a.mu.Unlock()
		return "", nil, fmt.Errorf("oauth: too many logins in progress: %w", ErrOAuth)
	}
	a.pending[state] = pendingLogin{returnTo: rt, expires: now.Add(a.cfg.StateTTL)}
	a.mu.Unlock()

	q := url.Values{
		"client_id": {a.cfg.OAuth.ClientID},
		"state":     {state},
		"scope":     {strings.Join(a.cfg.OAuth.Scopes, " ")},
	}
	if a.cfg.OAuth.RedirectURL != "" {
		q.Set("redirect_uri", a.cfg.OAuth.RedirectURL)
	}
	loc := a.cfg.OAuth.AuthorizeURL + "?" + q.Encode()
	a.log.DebugContext(ctx, "login started")
	return loc, &http.Cookie{
		Name: StateCookie, Value: state, Path: statePath,
		MaxAge: int(a.cfg.StateTTL / time.Second), Expires: now.Add(a.cfg.StateTTL),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}, nil
}

// Login is a finished login: the session cookie to set, the state cookie to clear and where
// to send the browser.
type Login struct {
	User       User
	Session    *http.Cookie
	ClearState *http.Cookie
	ReturnTo   string
}

// FinishLogin is the OAuth callback. The state must equal the state cookie of r (constant
// time) and be a login this server started and that hasn't expired (ErrStateMismatch); each
// state is used once. The code is exchanged for a user token, the token for the login (GET
// /user), and the token is dropped. A user in neither the readers nor the admin Group is
// ErrNotMember and gets no session.
func (a *Authenticator) FinishLogin(ctx context.Context, r *http.Request, code, state string) (*Login, error) {
	c, err := r.Cookie(StateCookie)
	if err != nil || c.Value == "" || state == "" ||
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		return nil, fmt.Errorf("callback state doesn't match the login's cookie: %w", ErrStateMismatch)
	}
	a.mu.Lock()
	p, ok := a.pending[state]
	delete(a.pending, state)
	a.mu.Unlock()
	if !ok || !a.clk.Now().Before(p.expires) {
		return nil, fmt.Errorf("unknown or expired login state: %w", ErrStateMismatch)
	}
	if code == "" {
		return nil, fmt.Errorf("no code: %w", ErrUnauthenticated)
	}
	token, err := a.exchange(ctx, code)
	if err != nil {
		return nil, err
	}
	gu, err := a.users.user(ctx, token) // the token goes no further: the session holds none
	if err != nil {
		return nil, err
	}
	u := User{Provider: a.cfg.Provider, Login: gu.Login, Name: gu.Name, AvatarURL: gu.AvatarURL}
	reader, err := a.IsReader(ctx, &u)
	if err != nil {
		return nil, fmt.Errorf("check membership: %w", err)
	}
	if !reader {
		a.log.InfoContext(ctx, "login refused: not a member", "login", u.Login)
		return nil, ErrNotMember
	}
	cookie, err := a.newSession(ctx, u)
	if err != nil {
		return nil, err
	}
	a.log.InfoContext(ctx, "signed in", "login", u.Login)
	return &Login{User: u, Session: cookie, ClearState: expired(StateCookie, statePath), ReturnTo: p.returnTo}, nil
}
