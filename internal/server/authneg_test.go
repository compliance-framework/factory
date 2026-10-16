package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/auth"
	"github.com/compliance-framework/factory/internal/provider/fake"
)

// reply is a raw response, its body read and closed.
type reply struct {
	StatusCode int
	Header     http.Header
}

// replyOf is a generated client's response as a reply (its body is already read and closed).
func replyOf(h *http.Response) reply {
	if h == nil {
		return reply{}
	}
	return reply{StatusCode: h.StatusCode, Header: h.Header}
}

// wantProblemReply is wantProblem for a raw reply.
func wantProblemReply(t *testing.T, what string, r reply, body []byte, status int, code string) {
	t.Helper()
	checkProblem(t, what, r.StatusCode, r.Header, body, status, code)
}

// do sends a raw request to the public server.
func (e *env) do(method, path string, body []byte, edit ...func(*http.Request)) (reply, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(e.ctx, method, e.ts.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, f := range edit {
		f(req)
	}
	res, err := e.hc.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return reply{StatusCode: res.StatusCode, Header: res.Header}, b
}

func withBearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func withSession(v string) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: v}) }
}

// concrete turns a spec path template into a request path.
func concrete(p string) string {
	r := strings.NewReplacer("{id}", "x", "{n}", "0", "{kind}", "TaskSpec", "{name}", "implement")
	return r.Replace(p)
}

// TestAuthRequired: every operation but the public ones refuses a request without
// credentials, and one with a wrong credential, with a 401 problem.
func TestAuthRequired(t *testing.T) {
	e := newEnv(t)
	for id, o := range spec(t) {
		acc, ok := operations[id]
		if !ok || acc == public {
			continue
		}
		path := concrete(o.Path)
		if id == "streamEvents" {
			path += "?topics=tasks"
		}
		var body []byte
		if o.Method != http.MethodGet && o.Method != http.MethodDelete {
			body = []byte("{}")
		}
		t.Run(id, func(t *testing.T) {
			res, b := e.do(o.Method, path, body)
			wantProblemReply(t, id+" without credentials", res, b, http.StatusUnauthorized, CodeUnauthenticated)

			var wrong []func(*http.Request)
			switch acc {
			case agentOnly:
				wrong = []func(*http.Request){withBearer("agt_nobody-x.wrong"), withBearer(cliToken), withSession("forged.cookie")}
			case reader, admin, sessionOnly:
				wrong = []func(*http.Request){withSession("forged.cookie"), withBearer("not-the-cli-token"), withBearer("agt_nobody-x.wrong")}
			case readerOrAgent:
				wrong = []func(*http.Request){withSession("forged.cookie"), withBearer("agt_nobody-x.wrong")}
			case registration:
				wrong = []func(*http.Request){func(r *http.Request) { r.Header.Set(HeaderJoinToken, joinSecret) }}
			}
			for i, w := range wrong {
				res, b := e.do(o.Method, path, body, w)
				wantProblemReply(t, id+" with a wrong credential "+string(rune('a'+i)), res, b, http.StatusUnauthorized, "")
			}
		})
	}
	// The registration headers are checked by the scheduler: a wrong join or owner token.
	for _, h := range [][2]string{{"wrong-join", "tok-gus"}, {joinSecret, "tok-unknown"}} {
		body, _ := json.Marshal(apiclient.RegisterAgentRequest{Pool: "local", Host: "mac1", AgentVersion: "1",
			Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{Name: "codex"}}}})
		res, b := e.do(http.MethodPost, BasePath+"/agents", body, func(r *http.Request) {
			r.Header.Set(HeaderJoinToken, h[0])
			r.Header.Set(HeaderOwnerToken, h[1])
		})
		wantProblemReply(t, "register with "+h[0]+"/"+h[1], res, b, http.StatusUnauthorized, CodeUnauthenticated)
	}
	// An owner who isn't in the pool's owners Group.
	body, _ := json.Marshal(apiclient.RegisterAgentRequest{Pool: "local", Host: "mac1", AgentVersion: "1",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{Name: "codex"}}}})
	res, b := e.do(http.MethodPost, BasePath+"/agents", body, func(r *http.Request) {
		r.Header.Set(HeaderJoinToken, joinSecret)
		r.Header.Set(HeaderOwnerToken, "tok-rita")
	})
	wantProblemReply(t, "register as a non-owner", res, b, http.StatusForbidden, CodeForbidden)
}

// TestAgentIsolation: agent A can't act on agent B, its lease, its task or its run.
func TestAgentIsolation(t *testing.T) {
	e := newEnv(t)
	_, aTok := e.register("gus", "mac1")
	b, bTok := e.register("ana", "mac2")
	e.task("tsk_b", nil)
	lb := e.lease(b, bTok)
	run := *lb.Lease.Run
	ctx := e.ctx

	type call struct {
		name string
		do   func() (reply, []byte, error)
		code string
	}
	wait := "0s"
	calls := []call{
		{"update B", func() (reply, []byte, error) {
			r, err := e.c.UpdateAgentWithResponse(ctx, b, apiclient.AgentUpdate{Phase: ptr(v1alpha1.AgentOffline)}, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotYourAgent},
		{"deregister B", func() (reply, []byte, error) {
			r, err := e.c.DeregisterAgentWithResponse(ctx, b, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotYourAgent},
		{"lease for B", func() (reply, []byte, error) {
			r, err := e.c.LeaseTaskWithResponse(ctx, b, &apiclient.LeaseTaskParams{Wait: &wait}, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotYourAgent},
		{"heartbeat B's lease", func() (reply, []byte, error) {
			r, err := e.c.HeartbeatLeaseWithResponse(ctx, lb.Lease.Id, apiclient.HeartbeatRequest{}, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"complete B's lease", func() (reply, []byte, error) {
			r, err := e.c.CompleteLeaseWithResponse(ctx, lb.Lease.Id, apiclient.CompleteRequest{Result: v1alpha1.Result{Task: "tsk_b", Status: v1alpha1.ResultDone}}, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"abandon B's lease", func() (reply, []byte, error) {
			r, err := e.c.AbandonLeaseWithResponse(ctx, lb.Lease.Id, apiclient.AbandonRequest{Reason: apiclient.AbandonRequestReasonShutdown}, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"get B's task", func() (reply, []byte, error) {
			r, err := e.c.GetTaskWithResponse(ctx, "tsk_b", bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"get B's prompt", func() (reply, []byte, error) {
			r, err := e.c.GetTaskPromptWithResponse(ctx, "tsk_b", bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"get B's context", func() (reply, []byte, error) {
			r, err := e.c.GetTaskContextWithResponse(ctx, "tsk_b", 0, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"effect on B's task", func() (reply, []byte, error) {
			r, err := e.c.CreateEffectWithResponse(ctx, "tsk_b", apiclient.EffectRequest{Op: v1alpha1.OpComment, IdempotencyKey: "k"}, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"upload B's transcript", func() (reply, []byte, error) {
			r, err := e.c.UploadTranscriptWithBodyWithResponse(ctx, run, "application/gzip", bytes.NewReader(gzipped(t, "{}")), bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
		{"get B's transcript", func() (reply, []byte, error) {
			r, err := e.c.GetTranscriptWithResponse(ctx, run, bearer(aTok))
			return replyOf(r.HTTPResponse), r.Body, err
		}, CodeNotLeaseHolder},
	}
	for _, c := range calls {
		res, body, err := c.do()
		if err != nil {
			t.Fatal(err)
		}
		wantProblemReply(t, c.name, res, body, http.StatusForbidden, c.code)
	}
	// B still holds its lease and its agent is untouched.
	if p := e.getTask("tsk_b").Status; p.Phase != v1alpha1.TaskLeased || *p.Agent != b {
		t.Errorf("B's task changed: %+v", p)
	}
}

// TestUIAccess: readers read, only maintainers (or the CLI) send commands, non-members get
// no session, sessions expire, and cookie-authenticated writes must be same-origin.
func TestUIAccess(t *testing.T) {
	e := newEnv(t)
	rita := e.login("rita") // a reader, not a maintainer
	gus := e.login("gus")
	ctx := e.ctx
	pause := apiclient.Command{Type: apiclient.CommandTypePause, Target: &apiclient.CommandTarget{Scope: ptr(apiclient.CommandTargetScopeGlobal)}}

	if r, err := e.c.ListTasksWithResponse(ctx, &apiclient.ListTasksParams{}, session(rita)); err != nil || r.StatusCode() != http.StatusOK {
		t.Fatalf("a reader lists tasks: %v %s", err, r.Status())
	}
	me, _ := e.c.GetMeWithResponse(ctx, session(rita))
	if me.JSON200 == nil || me.JSON200.IsAdmin {
		t.Errorf("rita's me: %s", me.Body)
	}
	pc, err := e.c.PostCommandWithResponse(ctx, pause, session(rita))
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "a reader's command", pc.HTTPResponse, pc.Body, http.StatusForbidden, CodeNotAdmin)

	// A maintainer's command from another site is refused; from the same origin it's accepted.
	res, b := e.do(http.MethodPost, BasePath+"/commands", []byte(`{"type":"resync-catalog"}`), withSession(gus),
		func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	wantProblemReply(t, "a cross-site command", res, b, http.StatusForbidden, CodeCrossSite)
	res, b = e.do(http.MethodPost, BasePath+"/commands", []byte(`{"type":"resync-catalog"}`), withSession(gus),
		func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") })
	wantProblemReply(t, "a cross-site command without Origin", res, b, http.StatusForbidden, CodeCrossSite)
	res, b = e.do(http.MethodPost, BasePath+"/commands", []byte(`{"type":"resync-catalog"}`), withSession(gus),
		func(r *http.Request) { r.Header.Set("Origin", e.ts.URL) })
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("a same-origin command: %d %s", res.StatusCode, b)
	}

	// The CLI token is a full admin.
	if r, err := e.c.PostCommandWithResponse(ctx, pause, bearer(cliToken)); err != nil || r.StatusCode() != http.StatusAccepted {
		t.Fatalf("CLI command: %v %s", err, r.Status())
	}
	if r, err := e.c.ListWorkItemsWithResponse(ctx, &apiclient.ListWorkItemsParams{}, bearer(cliToken)); err != nil || r.StatusCode() != http.StatusOK {
		t.Fatalf("CLI read: %v %s", err, r.Status())
	}
	// An agent token is not a UI credential.
	_, agentTok := e.register("gus", "mac1")
	lw, _ := e.c.ListWorkItemsWithResponse(ctx, &apiclient.ListWorkItemsParams{}, bearer(agentTok))
	wantProblem(t, "an agent reading the UI routes", lw.HTTPResponse, lw.Body, http.StatusUnauthorized, CodeUnauthenticated)

	// eve is in no Group: no session.
	lg, _ := e.c.LoginWithResponse(ctx, &apiclient.LoginParams{})
	state := cookieOf(t, lg.HTTPResponse.Header, auth.StateCookie).Value
	cb, err := e.c.AuthCallbackWithResponse(ctx, &apiclient.AuthCallbackParams{Code: "code-eve", State: state},
		func(_ context.Context, r *http.Request) error {
			r.AddCookie(&http.Cookie{Name: auth.StateCookie, Value: state})
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "a non-member's callback", cb.HTTPResponse, cb.Body, http.StatusForbidden, CodeNotMember)
	if c := cookieOf(t, cb.HTTPResponse.Header, auth.SessionCookie); c != nil {
		t.Error("a non-member got a session cookie")
	}

	// A tampered cookie, then expiry after 12 h.
	tampered := gus[:len(gus)-2] + "xx"
	r, _ := e.c.GetMeWithResponse(ctx, session(tampered))
	wantProblem(t, "a tampered session", r.HTTPResponse, r.Body, http.StatusUnauthorized, CodeUnauthenticated)
	e.clk.Advance(12*time.Hour + time.Second)
	r, _ = e.c.GetMeWithResponse(ctx, session(gus))
	wantProblem(t, "an expired session", r.HTTPResponse, r.Body, http.StatusUnauthorized, CodeUnauthenticated)
}

// TestOAuthState: the callback's state must match the cookie of the browser that started the
// login, once, before it expires.
func TestOAuthState(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx
	begin := func() string {
		lg, err := e.c.LoginWithResponse(ctx, &apiclient.LoginParams{})
		if err != nil {
			t.Fatal(err)
		}
		return cookieOf(t, lg.HTTPResponse.Header, auth.StateCookie).Value
	}
	callback := func(state, cookie string) *apiclient.AuthCallbackResponse {
		r, err := e.c.AuthCallbackWithResponse(ctx, &apiclient.AuthCallbackParams{Code: "code-gus", State: state},
			func(_ context.Context, r *http.Request) error {
				if cookie != "" {
					r.AddCookie(&http.Cookie{Name: auth.StateCookie, Value: cookie})
				}
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	s1, s2 := begin(), begin()
	for _, c := range []struct{ name, state, cookie string }{
		{"no cookie", s1, ""},
		{"another login's cookie", s1, s2},
		{"a forged state", "forged", "forged"},
	} {
		r := callback(c.state, c.cookie)
		wantProblem(t, c.name, r.HTTPResponse, r.Body, http.StatusBadRequest, CodeStateMismatch)
	}
	if r := callback(s1, s1); r.StatusCode() != http.StatusFound {
		t.Fatalf("the matching state: %s %s", r.Status(), r.Body)
	}
	r := callback(s1, s1)
	wantProblem(t, "a state used twice", r.HTTPResponse, r.Body, http.StatusBadRequest, CodeStateMismatch)
	e.clk.Advance(11 * time.Minute)
	r = callback(s2, s2)
	wantProblem(t, "an expired state", r.HTTPResponse, r.Body, http.StatusBadRequest, CodeStateMismatch)
	// A code GitHub doesn't know.
	s3 := begin()
	r, err := e.c.AuthCallbackWithResponse(ctx, &apiclient.AuthCallbackParams{Code: "code-nobody", State: s3},
		func(_ context.Context, req *http.Request) error {
			req.AddCookie(&http.Cookie{Name: auth.StateCookie, Value: s3})
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "an unknown code", r.HTTPResponse, r.Body, http.StatusUnauthorized, CodeUnauthenticated)
}

// TestOpenRedirect: returnTo must be a same-origin path.
func TestOpenRedirect(t *testing.T) {
	e := newEnv(t)
	for _, rt := range []string{"//evil.example", "https://evil.example/", "/\\evil.example", "javascript:alert(1)", "evil", "/ok\r\nSet-Cookie: x=y", "/%0d%0a"} {
		lg, err := e.c.LoginWithResponse(e.ctx, &apiclient.LoginParams{ReturnTo: ptr(rt)})
		if err != nil {
			t.Fatal(err)
		}
		if rt == "/%0d%0a" {
			// Encoded, it's a plain path: allowed, and it stays a path.
			if lg.StatusCode() != http.StatusFound {
				t.Errorf("returnTo %q: %s", rt, lg.Status())
			}
			continue
		}
		wantProblem(t, "returnTo "+rt, lg.HTTPResponse, lg.Body, http.StatusBadRequest, CodeBadReturnTo)
	}
}

// TestWebhook: deliveries go to the provider's handler, which checks the signature before the
// sink stores them.
func TestWebhook(t *testing.T) {
	e := newEnv(t)
	body, err := json.Marshal(fake.Webhook{Delivery: "d-1", Type: "issue.opened", Repo: docsRepo, Sender: fake.Sender{Login: "octocat"}})
	if err != nil {
		t.Fatal(err)
	}
	res, b := e.do(http.MethodPost, "/hooks/"+ghProvider, body, func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", "sha256=00") })
	if res.StatusCode != http.StatusUnauthorized || e.sink.count() != 0 {
		t.Fatalf("a bad signature: %d %s, %d stored", res.StatusCode, b, e.sink.count())
	}
	res, b = e.do(http.MethodPost, "/hooks/"+ghProvider, body)
	if res.StatusCode != http.StatusUnauthorized || e.sink.count() != 0 {
		t.Fatalf("no signature: %d %s", res.StatusCode, b)
	}
	res, b = e.do(http.MethodPost, "/hooks/"+ghProvider, body, func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", sign(body)) })
	if res.StatusCode != http.StatusAccepted || e.sink.count() != 1 {
		t.Fatalf("a signed delivery: %d %s, %d stored", res.StatusCode, b, e.sink.count())
	}
	res, _ = e.do(http.MethodPost, "/hooks/"+ghProvider, body, func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", sign(body)) })
	if res.StatusCode != http.StatusOK || e.sink.count() != 1 {
		t.Fatalf("a duplicate delivery: %d, %d stored", res.StatusCode, e.sink.count())
	}
	res, b = e.do(http.MethodPost, "/hooks/nope", body)
	wantProblemReply(t, "an unknown provider", res, b, http.StatusNotFound, CodeNotFound)
}
