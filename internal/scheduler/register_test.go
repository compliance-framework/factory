package scheduler

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/store"
)

func TestRegister(t *testing.T) {
	e := newEnv(t)
	lending := &v1alpha1.Lending{Queues: []string{pocQueue}, MaxConcurrent: 1}
	id, token := e.register("gus", "mbp", lending)
	if id != "agt_gus-mbp" {
		t.Fatalf("agent id = %q", id)
	}
	a := e.getAgent(id)
	if a.Spec.Owner != "github-public:gus" || a.Spec.Pool != pocPool || a.Status.Phase != v1alpha1.AgentReady {
		t.Fatalf("agent = %+v / %+v", a.Spec, a.Status)
	}
	if got, err := e.s.Authenticate(e.ctx, token); err != nil || got != id {
		t.Fatalf("Authenticate = %q, %v", got, err)
	}
	// Registering again replaces the agent (new lending) and the token.
	_, token2 := e.register("gus", "mbp", &v1alpha1.Lending{Unlimited: true})
	if !e.getAgent(id).Spec.Lending.Unlimited {
		t.Fatal("lending not replaced")
	}
	if _, err := e.s.Authenticate(e.ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old token: %v, want ErrUnauthorized", err)
	}
	if _, err := e.s.Authenticate(e.ctx, token2); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterRejects(t *testing.T) {
	tests := []struct {
		name string
		mut  func(e *env, r *RegisterRequest)
		want error
	}{
		{"wrong join token", func(_ *env, r *RegisterRequest) { r.JoinToken = "nope" }, ErrUnauthorized},
		{"no join token", func(_ *env, r *RegisterRequest) { r.JoinToken = "" }, ErrUnauthorized},
		{"unknown owner token", func(_ *env, r *RegisterRequest) { r.OwnerToken = "tok-unknown" }, ErrUnauthorized},
		{"no owner token", func(_ *env, r *RegisterRequest) { r.OwnerToken = "" }, ErrUnauthorized},
		{"owner not in the group", func(_ *env, r *RegisterRequest) { r.OwnerToken = "tok-eve" }, ErrForbidden},
		{"missing required harness", func(e *env, r *RegisterRequest) {
			e.cat.Pools[pocPool].Spec.Require = &v1alpha1.PoolRequirements{Harnesses: []string{"claude-code"}}
			r.Capabilities.Harnesses = []v1alpha1.HarnessOffer{codexOffer}
		}, ErrForbidden},
		{"unknown pool", func(_ *env, r *RegisterRequest) { r.Pool = "nowhere" }, ErrInvalid},
		{"bad host", func(_ *env, r *RegisterRequest) { r.Host = "My.Host" }, ErrInvalid},
		{"no harness", func(_ *env, r *RegisterRequest) { r.Capabilities.Harnesses = nil }, ErrInvalid},
		{"bad hours", func(_ *env, r *RegisterRequest) { r.Lending = &v1alpha1.Lending{Hours: "Mon-Fri 9-5"} }, ErrInvalid},
		{"bad zone", func(_ *env, r *RegisterRequest) {
			r.Lending = &v1alpha1.Lending{Hours: "09:00-17:00 Mars/Olympus"}
		}, ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			req := RegisterRequest{
				JoinToken: joinSecret, OwnerToken: "tok-gus", Pool: pocPool, Host: "mbp", AgentVersion: "0.1.0",
				Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{codexOffer}},
			}
			tt.mut(e, &req)
			_, err := e.s.Register(e.ctx, req)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Register: %v, want %v", err, tt.want)
			}
			var agents []*v1alpha1.Agent
			if _, err := e.st.List(e.ctx, v1alpha1.KindAgent, store.ListOptions{}, &agents); err != nil || len(agents) != 0 {
				t.Fatalf("agents stored after a rejected registration: %d (%v)", len(agents), err)
			}
		})
	}
}

// TestGroupMembershipCached: the owners Group is resolved once per 10 minutes.
func TestGroupMembershipCached(t *testing.T) {
	e := newEnv(t)
	e.register("gus", "mbp", nil)
	e.gh.SetOrgRole("compliance-framework", "admin", "ana") // gus removed
	e.register("gus", "mbp2", nil)                          // still cached
	e.clk.Advance(DefaultGroupCacheTTL)
	_, err := e.s.Register(e.ctx, RegisterRequest{
		JoinToken: joinSecret, OwnerToken: "tok-gus", Pool: pocPool, Host: "mbp3", AgentVersion: "0.1.0",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{codexOffer}},
	})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("after the cache expired: %v, want ErrForbidden", err)
	}
}

// TestTokenHashedAtRest: the agent token is stored only as a SHA-256 hash, expires after 24 h
// without use, and is renewed by use.
func TestTokenHashedAtRest(t *testing.T) {
	e := newEnv(t)
	id, token := e.register("gus", "mbp", nil)
	_, secret, _ := strings.Cut(token, ".")
	if len(secret) < 43 { // 32 bytes, base64url
		t.Fatalf("secret %q is too short", secret)
	}
	if strings.Contains(e.dump(), secret) {
		t.Fatal("the token's secret is in the store")
	}
	var tok AgentToken
	if err := e.st.Get(e.ctx, store.Key{Kind: KindAgentToken, Name: id}, &tok); err != nil {
		t.Fatal(err)
	}
	if tok.Hash != hashSecret(secret) || !strings.HasPrefix(tok.Hash, "sha256:") {
		t.Fatalf("stored hash = %q", tok.Hash)
	}
	for _, bad := range []string{"", "nodot", id + ".wrong", "agt_ana-box." + secret, id + "." + secret + "x"} {
		if _, err := e.s.Authenticate(e.ctx, bad); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("Authenticate(%q) = %v, want ErrUnauthorized", bad, err)
		}
	}
	// Used at 23 h: renewed for another 24 h.
	e.clk.Advance(23 * time.Hour)
	if _, err := e.s.Authenticate(e.ctx, token); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(23 * time.Hour)
	if _, err := e.s.Authenticate(e.ctx, token); err != nil {
		t.Fatalf("renewed token at 46 h: %v", err)
	}
	// Unused for 24 h: expired, and the tick takes the agent Offline.
	e.clk.Advance(24 * time.Hour)
	if _, err := e.s.Authenticate(e.ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired token: %v, want ErrUnauthorized", err)
	}
	if err := e.s.Refresh(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.getAgent(id).Status.Phase; got != v1alpha1.AgentOffline {
		t.Fatalf("agent with an expired token: %s, want Offline", got)
	}
}

func TestUpdateAgent(t *testing.T) {
	e := newEnv(t)
	id, _ := e.register("gus", "mbp", lendQueues(pocQueue))
	for _, bad := range []AgentUpdate{
		{},
		{Phase: v1alpha1.AgentThrottled},
		{Phase: v1alpha1.AgentBusy},
		{Lending: &v1alpha1.Lending{Hours: "whenever"}},
	} {
		if _, err := e.s.UpdateAgent(e.ctx, id, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("UpdateAgent(%+v) = %v, want ErrInvalid", bad, err)
		}
	}
	if _, err := e.s.UpdateAgent(e.ctx, "agt_nobody-x", AgentUpdate{Phase: v1alpha1.AgentReady}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown agent: %v, want ErrNotFound", err)
	}
	a, err := e.s.UpdateAgent(e.ctx, id, AgentUpdate{Lending: &v1alpha1.Lending{Queues: []string{"validation"}}})
	if err != nil || a.Spec.Lending.Queues[0] != "validation" {
		t.Fatalf("lending update: %+v, %v", a, err)
	}
	until := t0.Add(time.Hour)
	if a, err = e.s.UpdateAgent(e.ctx, id, AgentUpdate{Phase: v1alpha1.AgentThrottled, Until: until}); err != nil || a.Status.Phase != v1alpha1.AgentThrottled {
		t.Fatalf("throttle: %+v, %v", a, err)
	}
	e.clk.Advance(time.Hour)
	if err := e.s.Refresh(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.getAgent(id).Status; got.Phase != v1alpha1.AgentReady || got.ThrottledUntil != nil {
		t.Fatalf("after the throttle: %+v, want Ready", got)
	}
}

// TestDeregister: the token is revoked, the agent Offline and its lease abandoned (retried
// per the queue's policy).
func TestDeregister(t *testing.T) {
	e := newEnv(t)
	id, token := e.register("gus", "mbp", lendQueues(pocQueue))
	e.task("tsk_a", "implement", nil)
	g := e.mustLease(id, "tsk_a")
	if err := e.s.Deregister(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Authenticate(e.ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("token after deregister: %v", err)
	}
	if got := e.getAgent(id).Status.Phase; got != v1alpha1.AgentOffline {
		t.Fatalf("phase = %s, want Offline", got)
	}
	if got := e.getTask("tsk_a").Status; got.Phase != v1alpha1.TaskPending || got.Attempts != 1 || got.Reason != string(v1alpha1.RetryAgentLost) {
		t.Fatalf("task = %+v, want Pending, 1 attempt, agent-lost", got)
	}
	if l := e.getLease(g.Lease.ID); l.Status.End != LeaseAbandoned {
		t.Fatalf("lease end = %q", l.Status.End)
	}
	if err := e.s.Abandon(e.ctx, id, g.Lease.ID, AbandonRequest{Reason: controllers.AbandonShutdown}); err != nil {
		t.Fatalf("retried abandon: %v", err)
	}
	if err := e.s.Abandon(e.ctx, id, g.Lease.ID, AbandonRequest{Reason: "bored"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad reason: %v", err)
	}
}

func TestHours(t *testing.T) {
	lisbon := mustLoc(t, "Europe/Lisbon")
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, lisbon) } // 16 is a Friday
	tests := []struct {
		hours string
		at    time.Time
		want  bool
	}{
		{"Mon-Fri 08:00-20:00 Europe/Lisbon", at(16, 8, 0), true},
		{"Mon-Fri 08:00-20:00 Europe/Lisbon", at(16, 20, 0), false},
		{"Mon-Fri 08:00-20:00 Europe/Lisbon", at(17, 12, 0), false}, // Saturday
		{"08:00-20:00", time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC), true},
		{"Sat,Sun 00:00-00:00 Europe/Lisbon", at(17, 3, 0), true},
		{"Sat,Sun 00:00-00:00 Europe/Lisbon", at(16, 23, 59), false},
		{"Fri-Mon 22:00-06:00 Europe/Lisbon", at(19, 5, 59), true},  // Monday morning: Sunday's night
		{"Fri-Mon 22:00-06:00 Europe/Lisbon", at(20, 5, 59), true},  // Tuesday morning: Monday's night
		{"Fri-Mon 22:00-06:00 Europe/Lisbon", at(21, 5, 59), false}, // Wednesday morning
		{"Fri-Mon 22:00-06:00 Europe/Lisbon", at(16, 21, 59), false},
		{"mon-fri 22:00-06:00 UTC", time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC), true},
	}
	for _, tt := range tests {
		h, err := ParseHours(tt.hours)
		if err != nil {
			t.Fatalf("ParseHours(%q): %v", tt.hours, err)
		}
		if got := h.Contains(tt.at); got != tt.want {
			t.Errorf("%q contains %s = %v, want %v", tt.hours, tt.at, got, tt.want)
		}
	}
	for _, bad := range []string{"", "Mon-Fri", "9-17", "Mon-Fri 08:00-25:00", "Mon-Xyz 08:00-09:00", "08:00-09:00 Nowhere/Zone", "Mon 08:00-09:00 UTC extra"} {
		if _, err := ParseHours(bad); err == nil {
			t.Errorf("ParseHours(%q) succeeded", bad)
		}
	}
}
