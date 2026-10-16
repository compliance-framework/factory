package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/provider/fake"
	"github.com/compliance-framework/factory/internal/scheduler"
	"github.com/compliance-framework/factory/internal/store"
	"github.com/compliance-framework/factory/internal/store/memory"
)

func TestSafeReturnTo(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"", "/", true},
		{"/", "/", true},
		{"/board?view=all#top", "/board?view=all#top", true},
		{"/workitems/wi_1", "/workitems/wi_1", true},
		{"//evil.example", "", false},
		{"//evil.example/path", "", false},
		{"/\\evil.example", "", false},
		{"\\\\evil.example", "", false},
		{"https://evil.example", "", false},
		{"javascript:alert(1)", "", false},
		{"evil.example", "", false},
		{"/ok\r\nLocation: https://evil.example", "", false},
		{"/tab\there", "", false},
		{"/\x00", "", false},
		{"http:/evil.example", "", false},
	}
	for _, tt := range tests {
		got, err := SafeReturnTo(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("SafeReturnTo(%q) = %q, %v; want %q, ok %v", tt.in, got, err, tt.want, tt.ok)
		}
		if err != nil && !errors.Is(err, ErrBadReturnTo) {
			t.Errorf("SafeReturnTo(%q): %v is not ErrBadReturnTo", tt.in, err)
		}
	}
}

// userAPI is GET /user answering tok-<login> for the given logins, and 500 for tok-broken.
func userAPI(t *testing.T, logins ...string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			http.NotFound(w, r)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "tok-broken" {
			http.Error(w, "oops", http.StatusInternalServerError)
			return
		}
		for _, l := range logins {
			if tok == "tok-"+l {
				_, _ = io.WriteString(w, `{"login":"`+l+`","name":"N","avatar_url":"https://a/`+l+`"}`)
				return
			}
		}
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestGitHubOwners(t *testing.T) {
	ts := userAPI(t, "gus")
	o := GitHubOwners{Provider: "github-public", APIBase: ts.URL + "/", HTTPClient: ts.Client()}
	ctx := context.Background()
	got, err := o.VerifyOwner(ctx, "tok-gus")
	if err != nil || got != (scheduler.Owner{Provider: "github-public", Login: "gus"}) {
		t.Fatalf("VerifyOwner = %+v, %v", got, err)
	}
	for _, tok := range []string{"tok-nobody", ""} {
		if _, err := o.VerifyOwner(ctx, tok); !errors.Is(err, scheduler.ErrUnauthorized) {
			t.Errorf("token %q: %v, want ErrUnauthorized", tok, err)
		}
	}
	_, err = o.VerifyOwner(ctx, "tok-broken")
	if errors.Is(err, scheduler.ErrUnauthorized) || !errors.Is(err, ErrOAuth) {
		t.Errorf("a GitHub failure: %v, want ErrOAuth (not a refusal)", err)
	}
	if err != nil && strings.Contains(err.Error(), "tok-broken") {
		t.Errorf("the token leaked into the error: %v", err)
	}
}

type agents map[string]string

func (a agents) Authenticate(_ context.Context, tok string) (string, error) {
	if id, ok := a[tok]; ok {
		return id, nil
	}
	return "", scheduler.ErrUnauthorized
}

type staticCatalog struct{ c *catalog.Catalog }

func (s staticCatalog) Current() *catalog.Catalog { return s.c }

func newAuth(t *testing.T, clk *clock.Fake) (*Authenticator, *fake.Conn) {
	t.Helper()
	p := v1alpha1.Provider{Metadata: v1alpha1.ObjectMeta{Name: "gh"}, Spec: v1alpha1.ProviderSpec{Type: v1alpha1.ProviderTypeGitHub}}
	gh := fake.NewConn(p, fake.Options{Clock: clk})
	gh.SetOrgRole("org", "admin", "Gus")
	gh.SetTeam("org/docs", "rita")
	admin := v1alpha1.OrgRole{Org: "org", Role: "admin"}
	cat := &catalog.Catalog{Groups: map[string]*v1alpha1.Group{
		"maintainers": {Spec: v1alpha1.GroupSpec{Members: []v1alpha1.GroupSource{{Provider: "gh", OrgRole: &admin}}}},
		"readers":     {Spec: v1alpha1.GroupSpec{Members: []v1alpha1.GroupSource{{Provider: "gh", Team: "org/docs"}, {Provider: "gh", Team: "org/gone"}}}},
	}}
	a, err := New(Config{CLIToken: "cli", Provider: "gh", Clock: clk, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, Deps{
		Store:   memory.New(store.Options{Clock: clk}),
		Catalog: staticCatalog{cat},
		Conns:   controllers.StaticConns{"gh": gh},
		Agents:  agents{"agt_gus-mac1.secret": "agt_gus-mac1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, gh
}

func TestGroups(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	a, gh := newAuth(t, clk)
	ctx := context.Background()
	for _, c := range []struct {
		login          string
		reader, admins bool
	}{{"gus", true, true}, {"rita", true, false}, {"eve", false, false}} {
		u := &User{Provider: "gh", Login: c.login}
		r, err := a.IsReader(ctx, u)
		if err != nil || r != c.reader {
			t.Errorf("%s reader = %v, %v", c.login, r, err)
		}
		ad, err := a.IsAdmin(ctx, u)
		if err != nil || ad != c.admins {
			t.Errorf("%s admin = %v, %v", c.login, ad, err)
		}
	}
	// Another provider's login of the same name is not a member.
	if ok, _ := a.IsAdmin(ctx, &User{Provider: "other", Login: "gus"}); ok {
		t.Error("a login on another provider is an admin")
	}
	// Resolutions are cached for 10 minutes.
	gh.SetOrgRole("org", "admin")
	if ok, _ := a.IsAdmin(ctx, &User{Provider: "gh", Login: "gus"}); !ok {
		t.Error("the cache was not used")
	}
	clk.Advance(DefaultGroupCacheTTL)
	if ok, _ := a.IsAdmin(ctx, &User{Provider: "gh", Login: "gus"}); ok {
		t.Error("the cache outlived its TTL")
	}
}

func TestCredentials(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	a, _ := newAuth(t, clk)
	ctx := context.Background()
	req := func(h, v string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if h != "" {
			r.Header.Set(h, v)
		}
		return r
	}
	if p, ok := a.CLI(req("Authorization", "Bearer cli")); !ok || p.Kind != KindCLI || p.ActorID() != CLIActor {
		t.Errorf("CLI token: %+v %v", p, ok)
	}
	for _, v := range []string{"Bearer cl", "Bearer clix", "Basic cli", "cli", ""} {
		if _, ok := a.CLI(req("Authorization", v)); ok {
			t.Errorf("%q accepted as the CLI token", v)
		}
	}
	if p, err := a.Agent(ctx, req("Authorization", "bearer agt_gus-mac1.secret")); err != nil || p.AgentID != "agt_gus-mac1" {
		t.Errorf("agent token: %+v %v", p, err)
	}
	for _, v := range []string{"Bearer agt_gus-mac1.wrong", "Bearer cli", ""} {
		if _, err := a.Agent(ctx, req("Authorization", v)); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%q: %v, want ErrUnauthenticated", v, err)
		}
	}

	// Sessions: signed cookies, stored by hash, 12 h.
	c, err := a.newSession(ctx, User{Provider: "gh", Login: "gus"})
	if err != nil {
		t.Fatal(err)
	}
	r := req("", "")
	r.AddCookie(c)
	if u, err := a.Session(ctx, r); err != nil || u.Login != "gus" {
		t.Fatalf("session: %+v %v", u, err)
	}
	var stored []Session
	if _, err := a.d.Store.List(ctx, KindSession, store.ListOptions{}, &stored); err != nil || len(stored) != 1 {
		t.Fatal(err)
	}
	id, _, _ := strings.Cut(c.Value, ".")
	if strings.Contains(stored[0].Metadata.Name, id) {
		t.Error("the session id is stored in clear")
	}
	forged := req("", "")
	forged.AddCookie(&http.Cookie{Name: SessionCookie, Value: id + ".forged"})
	if _, err := a.Session(ctx, forged); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("forged signature: %v", err)
	}
	clk.Advance(DefaultSessionTTL)
	if _, err := a.Session(ctx, r); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("expired session: %v", err)
	}
	if _, err := a.d.Store.List(ctx, KindSession, store.ListOptions{}, &stored); err != nil || len(stored) != 0 {
		t.Errorf("the expired session is still stored (%d)", len(stored))
	}
}

func TestBeginLoginBounds(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	a, _ := newAuth(t, clk)
	if _, _, err := a.BeginLogin(context.Background(), "/"); !errors.Is(err, ErrOAuth) {
		t.Errorf("no client id: %v", err)
	}
	a.cfg.OAuth.ClientID = "c"
	a.mu.Lock()
	for i := range MaxPendingLogins {
		a.pending[string(rune(i))] = pendingLogin{expires: clk.Now().Add(time.Minute)}
	}
	a.mu.Unlock()
	if _, _, err := a.BeginLogin(context.Background(), "/"); !errors.Is(err, ErrOAuth) {
		t.Errorf("too many logins: %v", err)
	}
	clk.Advance(time.Minute)
	if _, c, err := a.BeginLogin(context.Background(), "/"); err != nil || c.Path != "/api/v1/auth" || !c.HttpOnly || !c.Secure {
		t.Errorf("after the pending ones expired: %+v %v", c, err)
	}
}
