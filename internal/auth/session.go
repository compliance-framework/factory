package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
)

// KindSession is a UI session (internal store kind, no API, no schema). It is named by the
// SHA-256 of the session id, so the store never holds a usable cookie value.
const KindSession = "Session"

// sessionIDBytes is the random part of a session id.
const sessionIDBytes = 32

// User is a signed-in GitHub user.
type User struct {
	// Provider is the catalog Provider of the login (Config.Provider).
	Provider  string `json:"provider"`
	Login     string `json:"login"`
	Name      string `json:"name,omitzero"`
	AvatarURL string `json:"avatarUrl,omitzero"`
}

// Session is a stored UI session. It holds no GitHub token.
type Session struct {
	v1alpha1.TypeMeta
	Metadata  v1alpha1.ObjectMeta `json:"metadata"`
	User      User                `json:"user"`
	ExpiresAt time.Time           `json:"expiresAt"`
}

var _ store.MetaObject = (*Session)(nil)

// GetName returns metadata.name (the hash of the session id).
func (s *Session) GetName() string { return s.Metadata.Name }

// GetObjectMeta implements store.MetaObject.
func (s *Session) GetObjectMeta() *v1alpha1.ObjectMeta { return &s.Metadata }

func sessionKey(id string) store.Key {
	sum := sha256.Sum256([]byte(id))
	return store.Key{Kind: KindSession, Name: "ses_" + hex.EncodeToString(sum[:])}
}

// sign returns the cookie value of a session id: "<id>.<HMAC-SHA256(key, id)>".
func (a *Authenticator) sign(id string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(id))
	return id + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// verify returns the session id of a cookie value, if its signature is right.
func (a *Authenticator) verify(v string) (string, bool) {
	id, sig, ok := strings.Cut(v, ".")
	if !ok || id == "" {
		return "", false
	}
	want := a.sign(id)
	return id, hmac.Equal([]byte(want), []byte(id+"."+sig))
}

// newSession stores a session for u and returns its cookie.
func (a *Authenticator) newSession(ctx context.Context, u User) (*http.Cookie, error) {
	b, err := randomBytes(a.cfg.Rand, sessionIDBytes)
	if err != nil {
		return nil, err
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	exp := a.clk.Now().Add(a.cfg.SessionTTL)
	s := &Session{
		TypeMeta:  v1alpha1.TypeMeta{Kind: KindSession},
		Metadata:  v1alpha1.ObjectMeta{Name: sessionKey(id).Name},
		User:      u,
		ExpiresAt: exp,
	}
	if err := a.d.Store.Create(ctx, s); err != nil {
		return nil, fmt.Errorf("store session: %w", err)
	}
	return &http.Cookie{
		Name: SessionCookie, Value: a.sign(id), Path: "/",
		MaxAge: int(a.cfg.SessionTTL / time.Second), Expires: exp,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}, nil
}

// Session returns the request's signed-in user. No cookie, a forged one, or an unknown or
// expired session is ErrUnauthenticated.
func (a *Authenticator) Session(ctx context.Context, r *http.Request) (*User, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return nil, fmt.Errorf("no session cookie: %w", ErrUnauthenticated)
	}
	id, ok := a.verify(c.Value)
	if !ok {
		return nil, fmt.Errorf("bad session cookie: %w", ErrUnauthenticated)
	}
	var s Session
	k := sessionKey(id)
	switch err := a.d.Store.Get(ctx, k, &s); {
	case errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("unknown session: %w", ErrUnauthenticated)
	case err != nil:
		return nil, fmt.Errorf("get session: %w", err)
	}
	if !a.clk.Now().Before(s.ExpiresAt) {
		if err := a.d.Store.Delete(ctx, k); err != nil && !errors.Is(err, store.ErrNotFound) {
			a.log.WarnContext(ctx, "delete expired session", "err", err)
		}
		return nil, fmt.Errorf("session expired: %w", ErrUnauthenticated)
	}
	u := s.User
	return &u, nil
}

// Logout deletes the request's session, if any, and returns the cookie that clears it.
func (a *Authenticator) Logout(ctx context.Context, r *http.Request) (*http.Cookie, error) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		if id, ok := a.verify(c.Value); ok {
			if err := a.d.Store.Delete(ctx, sessionKey(id)); err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("delete session: %w", err)
			}
		}
	}
	return expired(SessionCookie, "/"), nil
}

// expired is a cookie that clears name.
func expired(name, path string) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: "", Path: path, MaxAge: -1, Expires: time.Unix(0, 0),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}
}
