package fake_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/fake"
)

const prov = "github-public"

var t0 = time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)

func ref(kind, r string) v1alpha1.ObjectRef {
	return v1alpha1.ObjectRef{Provider: prov, Kind: kind, Ref: r}
}

func refp(kind, r string) *v1alpha1.ObjectRef {
	x := ref(kind, r)
	return &x
}

// testProvider is a github Provider with the PoC's trust rules and identities.
func testProvider() v1alpha1.Provider {
	return v1alpha1.Provider{
		TypeMeta: v1alpha1.TypeMeta{Kind: "Provider"},
		Metadata: v1alpha1.ObjectMeta{Name: prov},
		Spec: v1alpha1.ProviderSpec{
			Type: v1alpha1.ProviderTypeGitHub,
			GitHub: &v1alpha1.GitHubProviderSpec{
				Webhook:    v1alpha1.Webhook{Path: "/hooks/github-public"},
				Identities: map[string]v1alpha1.AppIdentity{"author": {AppID: 1}, "reviewer": {AppID: 2}},
			},
			Trust: []v1alpha1.TrustRule{
				{Tier: "maintainer", Group: "maintainers"},
				{Tier: "contributor", AuthorAssociation: []string{"CONTRIBUTOR", "COLLABORATOR"}},
				{Tier: "bot", Bots: []string{"dependabot[bot]"}},
				{Tier: "anyone"},
			},
		},
	}
}

// newConn returns a connected fake with a fake clock at t0 and alice as an org admin
// (maintainers = admins of compliance-framework).
func newConn(t *testing.T) (*fake.Conn, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(t0)
	a := fake.NewAdapter(fake.Options{
		Clock: clk,
		Groups: provider.StaticGroups(map[string]v1alpha1.GroupSpec{
			"maintainers": {Members: []v1alpha1.GroupSource{
				{Provider: prov, OrgRole: &v1alpha1.OrgRole{Org: "compliance-framework", Role: "admin"}},
				{Provider: "slack", UserGroup: "ignored"},
			}},
		}),
	})
	c := a.Conn(prov)
	c.SetOrgRole("compliance-framework", "admin", "alice")
	if _, err := a.Connect(context.Background(), testProvider(), nil); err != nil {
		t.Fatal(err)
	}
	return c, clk
}

// sink is a store-like IngressSink: it dedupes deliveries by ID.
type sink struct {
	mu   sync.Mutex
	seen map[string]provider.RawEvent
	ids  []string
	err  error
}

func newSink() *sink { return &sink{seen: map[string]provider.RawEvent{}} }

func (s *sink) Accept(_ context.Context, ev provider.RawEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if _, ok := s.seen[ev.ID]; ok {
		return fmt.Errorf("create delivery %s: %w", ev.ID, provider.ErrDuplicateDelivery)
	}
	s.seen[ev.ID] = ev
	s.ids = append(s.ids, ev.ID)
	return nil
}

func (s *sink) accepted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

func TestAdapterConnect(t *testing.T) {
	a := fake.NewAdapter(fake.Options{})
	if a.Type() != "github" {
		t.Fatalf("Type = %q", a.Type())
	}
	tests := []struct {
		name string
		p    v1alpha1.Provider
		want error
	}{
		{"ok", testProvider(), nil},
		{"no type uses the adapter's", v1alpha1.Provider{Metadata: v1alpha1.ObjectMeta{Name: "other"}}, nil},
		{"no name", v1alpha1.Provider{Spec: v1alpha1.ProviderSpec{Type: "github"}}, provider.ErrInvalid},
		{"wrong type", v1alpha1.Provider{Metadata: v1alpha1.ObjectMeta{Name: "s"}, Spec: v1alpha1.ProviderSpec{Type: "slack"}}, provider.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := a.Connect(context.Background(), tt.p, nil); !errors.Is(err, tt.want) {
				t.Fatalf("Connect = %v, want %v", err, tt.want)
			}
		})
	}
	if a.Connects() != 2 {
		t.Errorf("Connects = %d, want 2", a.Connects())
	}

	// Objects survive a reconnect, and the registry finds the fake under its type.
	c := a.Conn(prov)
	if err := c.Seed(provider.Observation{Ref: ref("issue", "o/r#1")}); err != nil {
		t.Fatal(err)
	}
	r, err := provider.NewRegistry(a)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := r.Connect(context.Background(), testProvider(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if conn != provider.Conn(c) {
		t.Error("reconnect returned another Conn")
	}
	if _, err := conn.Observe(context.Background(), ref("issue", "o/r#1")); err != nil {
		t.Errorf("Observe after reconnect: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Connect(ctx, testProvider(), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("Connect(cancelled) = %v", err)
	}
}

func TestEmitNormalise(t *testing.T) {
	tests := []struct {
		name string
		hook fake.Webhook
		want []v1alpha1.Event
	}{
		{
			name: "maintainer via group org role",
			hook: fake.Webhook{
				Delivery: "1", Type: v1alpha1.EventIssueOpened, Sender: fake.Sender{Login: "alice"},
				Object: refp("issue", "compliance-framework/docs#42"),
				Facts:  map[string]any{"issue": map[string]any{"title": "Docs gap", "labels": []any{"docs"}}},
			},
			want: []v1alpha1.Event{{
				TypeMeta: v1alpha1.TypeMeta{Kind: "Event"},
				ID:       "fake-1/0", Source: prov, Type: v1alpha1.EventIssueOpened,
				Subject: "compliance-framework/docs#42", Time: t0,
				Data: v1alpha1.EventData{
					Provider: prov,
					Actor:    &v1alpha1.Actor{ID: prov + ":alice", Tier: "maintainer"},
					Object:   refp("issue", "compliance-framework/docs#42"),
					Repo:     "compliance-framework/docs",
					Facts:    map[string]any{"issue": map[string]any{"title": "Docs gap", "labels": []any{"docs"}}},
				},
			}},
		},
		{
			name: "contributor by association, object provider defaulted",
			hook: fake.Webhook{
				Delivery: "2", Type: v1alpha1.EventPROpened,
				Sender: fake.Sender{Login: "bob", Association: "CONTRIBUTOR"},
				Object: &v1alpha1.ObjectRef{Kind: "pr", Ref: "o/r#5"}, Subject: "custom",
				Time: t0.Add(-time.Hour),
			},
			want: []v1alpha1.Event{{
				TypeMeta: v1alpha1.TypeMeta{Kind: "Event"},
				ID:       "fake-2/0", Source: prov, Type: v1alpha1.EventPROpened, Subject: "custom",
				Time: t0.Add(-time.Hour),
				Data: v1alpha1.EventData{
					Provider: prov, Actor: &v1alpha1.Actor{ID: prov + ":bob", Tier: "contributor"},
					Object: refp("pr", "o/r#5"), Repo: "o/r",
				},
			}},
		},
		{
			name: "allow-listed bot",
			hook: fake.Webhook{Delivery: "3", Type: v1alpha1.EventPROpened, Sender: fake.Sender{Login: "dependabot[bot]", Bot: true}, Object: refp("pr", "o/r#6")},
			want: []v1alpha1.Event{{
				TypeMeta: v1alpha1.TypeMeta{Kind: "Event"},
				ID:       "fake-3/0", Source: prov, Type: v1alpha1.EventPROpened, Subject: "o/r#6", Time: t0,
				Data: v1alpha1.EventData{
					Provider: prov, Actor: &v1alpha1.Actor{ID: prov + ":dependabot[bot]", Tier: "bot", IsBot: true},
					Object: refp("pr", "o/r#6"), Repo: "o/r",
				},
			}},
		},
		{
			name: "factory identity, tier override",
			hook: fake.Webhook{Delivery: "4", Type: v1alpha1.EventPRReviewSubmitted, Sender: fake.Sender{Login: "ccf-factory-reviewer[bot]", Tier: "bot"}, Object: refp("pr", "o/r#7")},
			want: []v1alpha1.Event{{
				TypeMeta: v1alpha1.TypeMeta{Kind: "Event"},
				ID:       "fake-4/0", Source: prov, Type: v1alpha1.EventPRReviewSubmitted, Subject: "o/r#7", Time: t0,
				Data: v1alpha1.EventData{
					Provider: prov,
					Actor:    &v1alpha1.Actor{ID: prov + ":ccf-factory-reviewer[bot]", Tier: "bot", IsBot: true, IsFactory: true, Identity: "reviewer"},
					Object:   refp("pr", "o/r#7"), Repo: "o/r",
				},
			}},
		},
		{
			name: "fan-out numbered in ref order",
			hook: fake.Webhook{
				Delivery: "5", Type: v1alpha1.EventPRChecksCompleted, Sender: fake.Sender{Login: "carol"},
				Object: refp("pr", "o/r#9"), FanOut: []v1alpha1.ObjectRef{ref("pr", "o/r#11"), ref("pr", "o/r#10")},
				Facts: map[string]any{"checks": map[string]any{"state": "success"}},
			},
			want: func() []v1alpha1.Event {
				var out []v1alpha1.Event
				for n, r := range []string{"o/r#10", "o/r#11", "o/r#9"} {
					out = append(out, v1alpha1.Event{
						TypeMeta: v1alpha1.TypeMeta{Kind: "Event"},
						ID:       fmt.Sprintf("fake-5/%d", n), Source: prov, Type: v1alpha1.EventPRChecksCompleted,
						Subject: r, Time: t0,
						Data: v1alpha1.EventData{
							Provider: prov, Actor: &v1alpha1.Actor{ID: prov + ":carol", Tier: "anyone"},
							Object: refp("pr", r), Repo: "o/r",
							Facts: map[string]any{"checks": map[string]any{"state": "success"}},
						},
					})
				}
				return out
			}(),
		},
		{
			name: "no object, no sender",
			hook: fake.Webhook{Delivery: "6", Type: v1alpha1.EventReleasePublished, Repo: "o/r"},
			want: []v1alpha1.Event{{
				TypeMeta: v1alpha1.TypeMeta{Kind: "Event"},
				ID:       "fake-6/0", Source: prov, Type: v1alpha1.EventReleasePublished, Time: t0,
				Data: v1alpha1.EventData{Provider: prov, Repo: "o/r"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newConn(t)
			s := newSink()
			raws, err := c.Emit(context.Background(), s, tt.hook)
			if err != nil {
				t.Fatal(err)
			}
			if len(raws) != 1 || s.accepted()[0] != raws[0].ID || raws[0].Provider != prov || raws[0].Type != string(tt.hook.Type) {
				t.Fatalf("emitted %+v, sink saw %v", raws, s.accepted())
			}
			got, err := c.Normalise(context.Background(), raws[0])
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Normalise =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestEmitGeneratesDeliveryIDsAndStopsOnError(t *testing.T) {
	c, _ := newConn(t)
	s := newSink()
	hooks := []fake.Webhook{{Type: v1alpha1.EventIssueOpened}, {Type: v1alpha1.EventIssueClosed}}
	raws, err := c.Emit(context.Background(), s, hooks...)
	if err != nil {
		t.Fatal(err)
	}
	if raws[0].ID == raws[1].ID || !strings.HasPrefix(raws[0].ID, fake.DeliveryPrefix) {
		t.Fatalf("IDs %q, %q", raws[0].ID, raws[1].ID)
	}
	// Re-emitting a delivery the sink has is a duplicate: Emit stops there.
	dup := fake.Webhook{Delivery: strings.TrimPrefix(raws[0].ID, fake.DeliveryPrefix), Type: v1alpha1.EventIssueOpened}
	got, err := c.Emit(context.Background(), s, dup, fake.Webhook{Type: v1alpha1.EventIssueOpened})
	if !errors.Is(err, provider.ErrDuplicateDelivery) || len(got) != 0 {
		t.Fatalf("Emit(dup) = %v, %v", got, err)
	}
	if n := len(s.accepted()); n != 2 {
		t.Errorf("sink accepted %d deliveries, want 2", n)
	}
}

func TestNormaliseErrors(t *testing.T) {
	c, _ := newConn(t)
	good, err := c.RawEvent(fake.Webhook{Type: v1alpha1.EventIssueOpened})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(f func(*provider.RawEvent)) provider.RawEvent {
		ev := good
		f(&ev)
		return ev
	}
	slackType, _ := c.RawEvent(fake.Webhook{Type: v1alpha1.EventMessagePosted})
	factoryType, _ := c.RawEvent(fake.Webhook{Type: v1alpha1.EventFactoryRunCompleted})
	unknown, _ := c.RawEvent(fake.Webhook{Type: "issue.reopened"})
	tests := []struct {
		name string
		ev   provider.RawEvent
	}{
		{"other provider", mk(func(e *provider.RawEvent) { e.Provider = "github-private" })},
		{"not json", mk(func(e *provider.RawEvent) { e.Body = []byte("nope") })},
		{"unknown field", mk(func(e *provider.RawEvent) { e.Body = []byte(`{"type":"issue.opened","x":1}`) })},
		{"trailing data", mk(func(e *provider.RawEvent) { e.Body = []byte(`{"type":"issue.opened"} {}`) })},
		{"type not emitted by github", slackType},
		{"factory type", factoryType},
		{"unknown type", unknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := c.Normalise(context.Background(), tt.ev); !errors.Is(err, provider.ErrInvalid) {
				t.Fatalf("Normalise = %v, want ErrInvalid", err)
			}
		})
	}
	c.FailNext(fake.MethodNormalise, errors.New("injected"))
	if _, err := c.Normalise(context.Background(), good); err == nil || err.Error() != "injected" {
		t.Errorf("injected fault: %v", err)
	}
	if _, err := c.Normalise(context.Background(), good); err != nil {
		t.Errorf("fault fired twice: %v", err)
	}
}

func TestHandler(t *testing.T) {
	body := `{"delivery":"77","type":"issue.opened","sender":{"login":"alice"},"object":{"kind":"issue","ref":"o/r#1"}}`
	tests := []struct {
		name     string
		method   string
		body     string
		sinkErr  error
		pre      bool // the sink already has the delivery
		want     int
		accepted int
	}{
		{name: "accepted", method: http.MethodPost, body: body, want: http.StatusAccepted, accepted: 1},
		{name: "duplicate", method: http.MethodPost, body: body, pre: true, want: http.StatusOK, accepted: 1},
		{name: "sink fails", method: http.MethodPost, body: body, sinkErr: errors.New("store down"), want: http.StatusServiceUnavailable},
		{name: "wrong method", method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{name: "bad body", method: http.MethodPost, body: `{"type":`, want: http.StatusBadRequest},
		{name: "too large", method: http.MethodPost, body: `{"facts":{"x":"` + strings.Repeat("a", fake.MaxWebhookBytes) + `"}}`, want: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newConn(t)
			s := newSink()
			if tt.pre {
				s.seen["fake-77"] = provider.RawEvent{}
				s.ids = []string{"fake-77"}
			}
			s.err = tt.sinkErr
			req := httptest.NewRequest(tt.method, "/hooks/github-public", strings.NewReader(tt.body))
			req.Header.Set("X-Fake-Event", "issues")
			rec := httptest.NewRecorder()
			c.Handler(s).ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
			if got := len(s.accepted()); got != tt.accepted {
				t.Errorf("sink has %d deliveries, want %d", got, tt.accepted)
			}
			if tt.want == http.StatusAccepted {
				ev := s.seen["fake-77"]
				if ev.Headers.Get("X-Fake-Event") != "issues" || ev.ReceivedAt != t0 {
					t.Errorf("stored delivery %+v", ev)
				}
				evs, err := c.Normalise(context.Background(), ev)
				if err != nil || len(evs) != 1 || evs[0].ID != "fake-77/0" || evs[0].Data.Actor.Tier != "maintainer" {
					t.Errorf("Normalise(posted) = %+v, %v", evs, err)
				}
			}
		})
	}
}

func TestRecover(t *testing.T) {
	c, clk := newConn(t)
	s := newSink()
	early, err := c.Undelivered(fake.Webhook{Delivery: "early", Type: v1alpha1.EventIssueOpened})
	if err != nil {
		t.Fatal(err)
	}
	since := clk.Advance(time.Minute)
	if _, err := c.Emit(context.Background(), s, fake.Webhook{Delivery: "seen", Type: v1alpha1.EventIssueOpened}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	if _, err := c.Undelivered(fake.Webhook{Delivery: "missed", Type: v1alpha1.EventIssueClosed}); err != nil {
		t.Fatal(err)
	}
	if n := len(c.Deliveries()); n != 3 {
		t.Fatalf("Deliveries = %d, want 3", n)
	}

	if err := c.Recover(context.Background(), since, s); err != nil {
		t.Fatal(err)
	}
	if got, want := s.accepted(), []string{"fake-seen", "fake-missed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after Recover the sink has %v, want %v (not %s, before since)", got, want, early[0].ID)
	}
	// Recovering again re-sends only duplicates.
	if err := c.Recover(context.Background(), since, s); err != nil {
		t.Fatal(err)
	}
	if n := len(s.accepted()); n != 2 {
		t.Errorf("second Recover: sink has %d deliveries", n)
	}
	// Recovering from earlier also picks up the early one.
	if err := c.Recover(context.Background(), t0, s); err != nil {
		t.Fatal(err)
	}
	if n := len(s.accepted()); n != 3 {
		t.Errorf("Recover(t0): sink has %d deliveries", n)
	}

	failing := newSink()
	failing.err = errors.New("store down")
	if err := c.Recover(context.Background(), t0, failing); err == nil || !strings.Contains(err.Error(), "store down") {
		t.Errorf("Recover with a failing sink = %v", err)
	}
	c.FailNext(fake.MethodRecover, errors.New("injected"))
	if err := c.Recover(context.Background(), t0, s); err == nil {
		t.Error("injected Recover fault not returned")
	}
}

func seedDocs(t *testing.T, c *fake.Conn) {
	t.Helper()
	err := c.Seed(
		provider.Observation{Ref: ref("issue", "compliance-framework/docs#42"), Labels: []string{"docs"}},
		provider.Observation{Ref: ref("pr", "compliance-framework/docs#57"), PR: &provider.PRObservation{Head: "beef", Base: "main", UnresolvedThreads: 2}},
		provider.Observation{Ref: ref("issue", "compliance-framework/docs#1"), State: provider.StateClosed},
		provider.Observation{
			Ref: ref("issue", "compliance-framework/docs#2"), State: provider.StateClosed,
			Projections: []provider.Projection{{WorkItemID: "wi_old"}},
		},
		provider.Observation{Ref: v1alpha1.ObjectRef{Ref: "compliance-framework/api#3"}},
		provider.Observation{Ref: ref("issue", "compliance-framework/workflows#123")},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestObserveAndList(t *testing.T) {
	c, _ := newConn(t)
	seedDocs(t, c)
	ctx := context.Background()

	got, err := c.Observe(ctx, ref("pr", "compliance-framework/docs#57"))
	if err != nil {
		t.Fatal(err)
	}
	if got.State != provider.StateOpen || got.PR.Head != "beef" || got.Issue != nil || got.UpdatedAt != t0 {
		t.Errorf("Observe(pr) = %+v", got)
	}
	// Defaults from Seed: provider, kind and an empty Issue.
	got, err = c.Observe(ctx, v1alpha1.ObjectRef{Ref: "compliance-framework/api#3"})
	if err != nil || got.Ref != ref("issue", "compliance-framework/api#3") || got.Issue == nil {
		t.Errorf("Observe(defaulted) = %+v, %v", got, err)
	}
	// Observations are copies.
	got.Labels = append(got.Labels, "mutated")
	if again, _ := c.Observe(ctx, ref("issue", "compliance-framework/api#3")); len(again.Labels) != 0 {
		t.Error("Observe returned shared state")
	}

	errTests := []struct {
		name string
		ref  v1alpha1.ObjectRef
		want error
	}{
		{"missing", ref("issue", "o/r#404"), provider.ErrNotFound},
		{"kind mismatch", ref("pr", "compliance-framework/docs#42"), provider.ErrNotFound},
		{"other provider", v1alpha1.ObjectRef{Provider: "slack", Ref: "compliance-framework/docs#42"}, provider.ErrInvalid},
		{"no ref", v1alpha1.ObjectRef{Provider: prov}, provider.ErrInvalid},
	}
	for _, tt := range errTests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := c.Observe(ctx, tt.ref); !errors.Is(err, tt.want) {
				t.Fatalf("Observe = %v, want %v", err, tt.want)
			}
		})
	}

	listTests := []struct {
		name  string
		scope v1alpha1.Scope
		want  []string
	}{
		{"everything open or projected", v1alpha1.Scope{}, []string{
			"compliance-framework/api#3", "compliance-framework/docs#2", "compliance-framework/docs#42",
			"compliance-framework/docs#57", "compliance-framework/workflows#123",
		}},
		{"one repo", v1alpha1.Scope{Repos: []string{"compliance-framework/docs"}}, []string{
			"compliance-framework/docs#2", "compliance-framework/docs#42", "compliance-framework/docs#57",
		}},
		{"glob minus exclude", v1alpha1.Scope{Repos: []string{"compliance-framework/*"}, Exclude: []string{"compliance-framework/{docs,workflows}"}}, []string{
			"compliance-framework/api#3",
		}},
		{"no match", v1alpha1.Scope{Repos: []string{"other/*"}}, nil},
	}
	for _, tt := range listTests {
		t.Run("list "+tt.name, func(t *testing.T) {
			refs, err := c.List(ctx, tt.scope)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range refs {
				if r.Provider != prov || r.Kind == "" {
					t.Errorf("ref %+v lacks provider or kind", r)
				}
				got = append(got, r.Ref)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("List = %v, want %v", got, tt.want)
			}
		})
	}
	if _, err := c.List(ctx, v1alpha1.Scope{Repos: []string{"re:("}}); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("List(bad glob) = %v", err)
	}

	// Out-of-band change: a human merges the PR.
	if err := c.Update("compliance-framework/docs#57", func(o *provider.Observation) { o.State = provider.StateMerged }); err != nil {
		t.Fatal(err)
	}
	if o, _ := c.Object("compliance-framework/docs#57"); o.State != provider.StateMerged {
		t.Errorf("after Update: %s", o.State)
	}
	if err := c.Update("o/r#404", func(*provider.Observation) {}); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Update(missing) = %v", err)
	}
	if err := c.Seed(provider.Observation{Ref: v1alpha1.ObjectRef{Provider: "slack", Ref: "x"}}); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("Seed(other provider) = %v", err)
	}
}

func TestResolveGroup(t *testing.T) {
	c, _ := newConn(t)
	c.SetTeam("compliance-framework/docs-team", "zoe", "bob", "bob")
	c.SetUserGroup("oncall", "dave")
	tests := []struct {
		name string
		src  v1alpha1.GroupSource
		want []string
		err  error
	}{
		{"team", v1alpha1.GroupSource{Provider: prov, Team: "compliance-framework/docs-team"}, []string{"bob", "zoe"}, nil},
		{"org role", v1alpha1.GroupSource{Provider: prov, OrgRole: &v1alpha1.OrgRole{Org: "compliance-framework", Role: "admin"}}, []string{"alice"}, nil},
		{"users", v1alpha1.GroupSource{Provider: prov, Users: []string{"y", "x", "y"}}, []string{"x", "y"}, nil},
		{"user group", v1alpha1.GroupSource{Provider: prov, UserGroup: "oncall"}, []string{"dave"}, nil},
		{"unknown team", v1alpha1.GroupSource{Provider: prov, Team: "nope"}, nil, provider.ErrNotFound},
		{"unknown role", v1alpha1.GroupSource{Provider: prov, OrgRole: &v1alpha1.OrgRole{Org: "x", Role: "admin"}}, nil, provider.ErrNotFound},
		{"two sources", v1alpha1.GroupSource{Provider: prov, Team: "t", Users: []string{"u"}}, nil, provider.ErrInvalid},
		{"other provider", v1alpha1.GroupSource{Provider: "slack", Users: []string{"u"}}, nil, provider.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.ResolveGroup(context.Background(), tt.src)
			if !errors.Is(err, tt.err) {
				t.Fatalf("ResolveGroup err = %v, want %v", err, tt.err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ResolveGroup = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProject(t *testing.T) {
	c, clk := newConn(t)
	seedDocs(t, c)
	ctx := context.Background()
	target := ref("issue", "compliance-framework/docs#42")
	p := provider.Projection{
		WorkItemID: "wi_1",
		State:      map[string]any{"release": "drafting", "plan": map[string]any{"steps": []any{"a"}}},
		Counters:   map[string]int{"release-docs": 1},
		Links:      []v1alpha1.Link{{Provider: prov, Ref: "compliance-framework/docs#42", Kind: "issue", Primary: true}},
		Labels:     []string{"factory:tracked"},
	}
	clk.Advance(time.Minute)
	if err := c.Project(ctx, target, p); err != nil {
		t.Fatal(err)
	}
	// The caller's maps are not shared with the fake.
	p.State["release"] = "mutated"
	if got := c.Projections()[0].Projection.State["release"]; got != "drafting" {
		t.Fatalf("recorded projection shares the caller's map: %v", got)
	}
	p.State["release"] = "drafting"
	if err := c.Project(ctx, target, p); err != nil {
		t.Fatal(err)
	}
	if err := c.Project(ctx, target, provider.Projection{WorkItemID: "wi_0"}); err != nil {
		t.Fatal(err)
	}

	calls := c.Projections()
	if len(calls) != 3 || !calls[0].Changed || calls[1].Changed || !calls[2].Changed {
		t.Fatalf("Projections = %+v", calls)
	}
	if !reflect.DeepEqual(calls[0].Projection, p) || calls[0].Ref != target {
		t.Errorf("recorded %+v", calls[0])
	}
	o, err := c.Observe(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o.Labels, []string{"docs", "factory:tracked"}) {
		t.Errorf("labels %v: projection labels are added, others kept", o.Labels)
	}
	if len(o.Projections) != 2 || o.Projections[0].WorkItemID != "wi_0" || !reflect.DeepEqual(o.Projections[1], p) {
		t.Errorf("Observe projections = %+v", o.Projections)
	}
	if !o.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("UpdatedAt = %v", o.UpdatedAt)
	}

	errTests := []struct {
		name string
		ref  v1alpha1.ObjectRef
		p    provider.Projection
		want error
	}{
		{"missing object", ref("issue", "o/r#404"), provider.Projection{WorkItemID: "wi_1"}, provider.ErrNotFound},
		{"no work item", target, provider.Projection{}, provider.ErrInvalid},
	}
	for _, tt := range errTests {
		t.Run(tt.name, func(t *testing.T) {
			if err := c.Project(ctx, tt.ref, tt.p); !errors.Is(err, tt.want) {
				t.Fatalf("Project = %v, want %v", err, tt.want)
			}
		})
	}
	if calls := c.Projections(); len(calls) != 5 || calls[4].Err == nil {
		t.Errorf("failed calls not recorded: %+v", calls)
	}
}

func TestApply(t *testing.T) {
	issue := ref("issue", "compliance-framework/docs#42")
	pr := ref("pr", "compliance-framework/docs#57")
	tests := []struct {
		name    string
		req     provider.EffectRequest
		wantURL string
		wantObj v1alpha1.ObjectRef
		check   func(t *testing.T, c *fake.Conn)
		wantErr error
	}{
		{
			name:    "comment",
			req:     provider.EffectRequest{Op: v1alpha1.OpComment, Target: issue, Args: map[string]any{"body": "hi"}},
			wantURL: "https://fake.invalid/compliance-framework/docs/issues/42#comment-1", wantObj: issue,
			check: func(t *testing.T, c *fake.Conn) {
				if o, _ := c.Object(issue.Ref); o.Issue.Comments != 1 {
					t.Errorf("comments = %d", o.Issue.Comments)
				}
			},
		},
		{
			name:    "label.add",
			req:     provider.EffectRequest{Op: v1alpha1.OpLabelAdd, Target: issue, Args: map[string]any{"labels": []any{"needs-human", "docs"}}},
			wantURL: "https://fake.invalid/compliance-framework/docs/issues/42", wantObj: issue,
			check: func(t *testing.T, c *fake.Conn) {
				if o, _ := c.Object(issue.Ref); !reflect.DeepEqual(o.Labels, []string{"docs", "needs-human"}) {
					t.Errorf("labels = %v", o.Labels)
				}
			},
		},
		{
			name: "label.remove", req: provider.EffectRequest{Op: v1alpha1.OpLabelRemove, Target: issue, Args: map[string]any{"label": "docs"}},
			wantURL: "https://fake.invalid/compliance-framework/docs/issues/42", wantObj: issue,
			check: func(t *testing.T, c *fake.Conn) {
				if o, _ := c.Object(issue.Ref); len(o.Labels) != 0 {
					t.Errorf("labels = %v", o.Labels)
				}
			},
		},
		{
			name: "link-duplicate", req: provider.EffectRequest{Op: v1alpha1.OpLinkDuplicate, Target: issue},
			wantURL: "https://fake.invalid/compliance-framework/docs/issues/42", wantObj: issue,
			check: func(t *testing.T, c *fake.Conn) {
				if o, _ := c.Object(issue.Ref); o.State != provider.StateClosed || !reflect.DeepEqual(o.Labels, []string{"docs", "duplicate"}) {
					t.Errorf("object = %+v", o)
				}
			},
		},
		{
			name:    "create-issue",
			req:     provider.EffectRequest{Op: v1alpha1.OpCreateIssue, Target: v1alpha1.ObjectRef{Ref: "compliance-framework/docs"}, Identity: "author", Args: map[string]any{"title": "t", "labels": []string{"epic"}}},
			wantURL: "https://fake.invalid/compliance-framework/docs/issues/58", wantObj: ref("issue", "compliance-framework/docs#58"),
			check: func(t *testing.T, c *fake.Conn) {
				o, ok := c.Object("compliance-framework/docs#58")
				if !ok || o.Issue == nil || o.State != provider.StateOpen || !o.Author.IsFactory || o.Author.Identity != "author" || o.Labels[0] != "epic" {
					t.Errorf("created %+v", o)
				}
			},
		},
		{
			name:    "pr.open",
			req:     provider.EffectRequest{Op: v1alpha1.OpPROpen, Target: v1alpha1.ObjectRef{Provider: prov, Ref: "o/new"}, Args: map[string]any{"head": "fx/x", "base": "main", "draft": true}},
			wantURL: "https://fake.invalid/o/new/pull/1", wantObj: ref("pr", "o/new#1"),
			check: func(t *testing.T, c *fake.Conn) {
				if o, _ := c.Object("o/new#1"); o.PR == nil || !o.PR.Draft || o.PR.Base != "main" {
					t.Errorf("created %+v", o)
				}
			},
		},
		{
			name: "sub-issue.add", req: provider.EffectRequest{Op: v1alpha1.OpSubIssueAdd, Target: issue, Args: map[string]any{"issue": "compliance-framework/docs#43"}},
			wantURL: "https://fake.invalid/compliance-framework/docs/issues/42", wantObj: issue,
			check: func(t *testing.T, c *fake.Conn) {
				if o, _ := c.Object(issue.Ref); len(o.Issue.SubIssues) != 1 || o.Issue.SubIssues[0].Ref != "compliance-framework/docs#43" {
					t.Errorf("sub-issues %+v", o.Issue.SubIssues)
				}
			},
		},
		{
			name: "pr.merge", req: provider.EffectRequest{Op: v1alpha1.OpPRMerge, Target: pr},
			wantURL: "https://fake.invalid/compliance-framework/docs/pull/57", wantObj: pr,
			check: func(t *testing.T, c *fake.Conn) {
				if o, _ := c.Object(pr.Ref); o.State != provider.StateMerged || o.PR.MergeCommit == "" || o.PR.MergedAt != t0 {
					t.Errorf("merged %+v %+v", o, o.PR)
				}
			},
		},
		{
			name: "review.approve", req: provider.EffectRequest{Op: v1alpha1.OpReviewApprove, Target: pr, Identity: "reviewer"},
			wantURL: "https://fake.invalid/compliance-framework/docs/pull/57", wantObj: pr,
			check: func(t *testing.T, c *fake.Conn) {
				if o, _ := c.Object(pr.Ref); !reflect.DeepEqual(o.PR.ApprovedBy, []provider.Approval{{Identity: "reviewer", Commit: "beef"}}) {
					t.Errorf("approvedBy %+v", o.PR.ApprovedBy)
				}
			},
		},
		{
			name: "pr.retarget and thread.resolve", req: provider.EffectRequest{Op: v1alpha1.OpPRRetarget, Target: pr, Args: map[string]any{"base": "fx/base"}},
			wantURL: "https://fake.invalid/compliance-framework/docs/pull/57", wantObj: pr,
			check: func(t *testing.T, c *fake.Conn) {
				if _, err := c.Apply(context.Background(), provider.EffectRequest{Op: v1alpha1.OpThreadResolve, Target: pr, IdempotencyKey: "k2"}); err != nil {
					t.Fatal(err)
				}
				if o, _ := c.Object(pr.Ref); o.PR.Base != "fx/base" || o.PR.UnresolvedThreads != 1 {
					t.Errorf("pr %+v", o.PR)
				}
			},
		},
		{name: "unknown target", req: provider.EffectRequest{Op: v1alpha1.OpClose, Target: ref("issue", "o/r#404")}, wantErr: provider.ErrNotFound},
		{name: "PR op on an issue", req: provider.EffectRequest{Op: v1alpha1.OpPRMerge, Target: issue}, wantErr: provider.ErrInvalid},
		{name: "create in a non-repo", req: provider.EffectRequest{Op: v1alpha1.OpCreateIssue, Target: issue}, wantErr: provider.ErrInvalid},
		{name: "git-side op", req: provider.EffectRequest{Op: v1alpha1.OpPushBranch, Target: pr}, wantErr: provider.ErrUnsupported},
		{name: "unknown op", req: provider.EffectRequest{Op: "explode", Target: pr}, wantErr: provider.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newConn(t)
			seedDocs(t, c)
			tt.req.IdempotencyKey = "k1"
			res, err := c.Apply(context.Background(), tt.req)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Apply = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				if calls := c.Applies(); len(calls) != 1 || calls[0].Err == nil || len(c.Effects()) != 0 {
					t.Errorf("failed call recorded as %+v", calls)
				}
				return
			}
			if res.URL != tt.wantURL || res.Object == nil || *res.Object != tt.wantObj || res.Replayed {
				t.Errorf("Apply = %+v (object %+v), want URL %s object %+v", res, res.Object, tt.wantURL, tt.wantObj)
			}
			if tt.check != nil {
				tt.check(t, c)
			}
		})
	}
}

func TestApplyIdempotency(t *testing.T) {
	c, _ := newConn(t)
	seedDocs(t, c)
	ctx := context.Background()
	issue := ref("issue", "compliance-framework/docs#42")
	req := provider.EffectRequest{Op: v1alpha1.OpComment, Target: issue, Identity: "author", IdempotencyKey: "evt/1/triage/0", Args: map[string]any{"body": "x"}}

	first, err := c.Apply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.Apply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.URL != first.URL || *again.Object != *first.Object {
		t.Errorf("replay = %+v, first = %+v", again, first)
	}
	if o, _ := c.Object(issue.Ref); o.Issue.Comments != 1 {
		t.Errorf("replay created a comment: %d", o.Issue.Comments)
	}

	// A failed Apply is not cached: the retry runs.
	c.FailNext(fake.MethodApply, &provider.RetryableError{Err: errors.New("rate limited")})
	retry := req
	retry.IdempotencyKey = "evt/1/triage/1"
	if _, err := c.Apply(ctx, retry); !errors.Is(err, provider.ErrRetryable) {
		t.Fatalf("injected fault = %v", err)
	}
	if res, err := c.Apply(ctx, retry); err != nil || res.Replayed {
		t.Fatalf("retry = %+v, %v", res, err)
	}

	missing := req
	missing.IdempotencyKey = ""
	if _, err := c.Apply(ctx, missing); !errors.Is(err, provider.ErrInvalid) {
		t.Errorf("Apply without key = %v", err)
	}

	calls := c.Applies()
	if len(calls) != 5 {
		t.Fatalf("Applies = %d, want 5", len(calls))
	}
	if eff := c.Effects(); len(eff) != 2 || eff[0].Request.IdempotencyKey != "evt/1/triage/0" || eff[1].Request.IdempotencyKey != "evt/1/triage/1" {
		t.Errorf("Effects = %+v", eff)
	}
	// Recorded requests are copies.
	calls[0].Request.Args["body"] = "mutated"
	if c.Applies()[0].Request.Args["body"] != "x" {
		t.Error("Applies returned shared state")
	}
}

func TestMintGitCredential(t *testing.T) {
	ok := provider.GitCredentialRequest{
		Identity: "author", Repo: "compliance-framework/docs", ReadRepos: []string{"compliance-framework/api"},
		Permissions: map[string]string{"contents": "write", "pull_requests": "write"}, TTL: 30 * time.Minute,
	}
	tests := []struct {
		name    string
		mod     func(*provider.GitCredentialRequest)
		wantTTL time.Duration
		wantErr error
	}{
		{name: "scoped", mod: func(*provider.GitCredentialRequest) {}, wantTTL: 30 * time.Minute},
		{name: "ttl capped", mod: func(r *provider.GitCredentialRequest) { r.TTL = 3 * time.Hour }, wantTTL: time.Hour},
		{name: "no repo", mod: func(r *provider.GitCredentialRequest) { r.Repo = "" }, wantErr: provider.ErrInvalid},
		{name: "no identity", mod: func(r *provider.GitCredentialRequest) { r.Identity = "" }, wantErr: provider.ErrInvalid},
		{name: "unknown identity", mod: func(r *provider.GitCredentialRequest) { r.Identity = "admin" }, wantErr: provider.ErrInvalid},
		{name: "no ttl", mod: func(r *provider.GitCredentialRequest) { r.TTL = 0 }, wantErr: provider.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newConn(t)
			req := ok
			req.ReadRepos = append([]string(nil), ok.ReadRepos...)
			tt.mod(&req)
			cred, err := c.MintGitCredential(context.Background(), req)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Mint = %v, want %v", err, tt.wantErr)
			}
			if len(c.Mints()) != 1 {
				t.Fatalf("Mints = %d", len(c.Mints()))
			}
			if tt.wantErr != nil {
				return
			}
			if cred.Provider != prov || cred.Host != "github.com" || cred.Username != "x-access-token" ||
				!strings.HasPrefix(cred.Token, "fake-author-") || cred.Repo != req.Repo ||
				!reflect.DeepEqual(cred.ReadRepos, req.ReadRepos) || !reflect.DeepEqual(cred.Permissions, req.Permissions) ||
				!cred.ExpiresAt.Equal(t0.Add(tt.wantTTL)) {
				t.Errorf("credential %+v (token %q)", cred, cred.Token)
			}
			if strings.Contains(fmt.Sprint(cred), cred.Token) {
				t.Error("token printed")
			}
		})
	}
	c, _ := newConn(t)
	a, _ := c.MintGitCredential(context.Background(), ok)
	b, _ := c.MintGitCredential(context.Background(), ok)
	if a.Token == b.Token {
		t.Error("two mints returned the same token")
	}
}

func TestHealth(t *testing.T) {
	c, clk := newConn(t)
	if h := c.Health(); h.Status != provider.HealthOK || !h.CheckedAt.Equal(t0) {
		t.Errorf("default health %+v", h)
	}
	c.SetHealth(provider.HealthStatus{Status: provider.HealthDegraded, Message: "rate limited"})
	clk.Advance(time.Second)
	if h := c.Health(); h.Status != provider.HealthDegraded || h.Message != "rate limited" || !h.CheckedAt.Equal(t0.Add(time.Second)) {
		t.Errorf("set health %+v", h)
	}
}

func TestCancelledContext(t *testing.T) {
	c, _ := newConn(t)
	seedDocs(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	issue := ref("issue", "compliance-framework/docs#42")
	calls := map[string]func() error{
		"Observe": func() error { _, err := c.Observe(ctx, issue); return err },
		"List":    func() error { _, err := c.List(ctx, v1alpha1.Scope{}); return err },
		"ResolveGroup": func() error {
			_, err := c.ResolveGroup(ctx, v1alpha1.GroupSource{Provider: prov, Users: []string{"a"}})
			return err
		},
		"Project": func() error { return c.Project(ctx, issue, provider.Projection{WorkItemID: "w"}) },
		"Apply": func() error {
			_, err := c.Apply(ctx, provider.EffectRequest{Op: v1alpha1.OpClose, Target: issue, IdempotencyKey: "k"})
			return err
		},
		"Mint": func() error {
			_, err := c.MintGitCredential(ctx, provider.GitCredentialRequest{Identity: "author", Repo: "o/r", TTL: time.Minute})
			return err
		},
		"Recover":   func() error { return c.Recover(ctx, t0, newSink()) },
		"Normalise": func() error { _, err := c.Normalise(ctx, provider.RawEvent{}); return err },
		"Emit":      func() error { _, err := c.Emit(ctx, newSink(), fake.Webhook{}); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s = %v, want context.Canceled", name, err)
			}
		})
	}
	if o, _ := c.Object(issue.Ref); o.State != provider.StateOpen {
		t.Error("cancelled Apply changed the object")
	}
}

// TestConcurrent exercises every entry point at once; it is meant for -race.
func TestConcurrent(t *testing.T) {
	c, _ := newConn(t)
	seedDocs(t, c)
	s := newSink()
	srv := httptest.NewServer(c.Handler(s))
	defer srv.Close()
	ctx := context.Background()
	issue := ref("issue", "compliance-framework/docs#42")

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 20 {
				key := fmt.Sprintf("k-%d", j%5) // shared keys: concurrent replays
				if _, err := c.Apply(ctx, provider.EffectRequest{Op: v1alpha1.OpComment, Target: issue, IdempotencyKey: key}); err != nil {
					t.Error(err)
				}
				if err := c.Project(ctx, issue, provider.Projection{WorkItemID: fmt.Sprintf("wi_%d", i), Counters: map[string]int{"n": j}}); err != nil {
					t.Error(err)
				}
				if _, err := c.Observe(ctx, issue); err != nil {
					t.Error(err)
				}
				raws, err := c.Emit(ctx, s, fake.Webhook{Type: v1alpha1.EventIssueCommented, Sender: fake.Sender{Login: "alice"}, Object: &issue})
				if err != nil {
					t.Error(err)
					continue
				}
				if _, err := c.Normalise(ctx, raws[0]); err != nil {
					t.Error(err)
				}
				resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"type":"issue.opened"}`))
				if err != nil {
					t.Error(err)
					continue
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusAccepted {
					t.Errorf("POST status %d", resp.StatusCode)
				}
				_, _ = c.List(ctx, v1alpha1.Scope{})
				_ = c.Health()
				_ = c.Applies()
				_ = c.Projections()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			if err := c.Recover(ctx, t0, s); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()

	if o, _ := c.Object(issue.Ref); o.Issue.Comments != 5 {
		t.Errorf("comments = %d, want 5 (one per idempotency key)", o.Issue.Comments)
	}
	if got, want := len(s.accepted()), 8*20*2; got != want {
		t.Errorf("sink has %d deliveries, want %d", got, want)
	}
}
