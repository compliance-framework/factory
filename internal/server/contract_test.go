package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/compliance-framework/factory/api"
	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
)

var (
	specOnce sync.Once
	specOps  map[string]api.Operation
	specErr  error
)

func spec(t *testing.T) map[string]api.Operation {
	t.Helper()
	specOnce.Do(func() {
		doc, err := api.Load(context.Background())
		if err != nil {
			specErr = err
			return
		}
		ops, err := api.Operations(doc)
		if err != nil {
			specErr = err
			return
		}
		specOps = map[string]api.Operation{}
		for _, o := range ops {
			specOps[o.ID] = o
		}
	})
	if specErr != nil {
		t.Fatal(specErr)
	}
	return specOps
}

// shapes checks responses against the spec and records which operations were checked.
type shapes struct {
	t    *testing.T
	ops  map[string]api.Operation
	seen map[string]bool
}

// check asserts the response has status and, for the operation's success status, the spec's
// media type and (for JSON) a body that validates against the response schema.
func (s *shapes) check(opID string, res *http.Response, body []byte, status int) {
	s.t.Helper()
	o, ok := s.ops[opID]
	if !ok {
		s.t.Fatalf("%s: not in the spec", opID)
	}
	if res.StatusCode != status {
		s.t.Fatalf("%s: status %d, want %d (%s)", opID, res.StatusCode, status, body)
	}
	s.seen[opID] = true
	if status != o.Status || !o.HasBody() {
		return
	}
	ct := res.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, o.ContentType) {
		s.t.Fatalf("%s: content type %q, want %s", opID, ct, o.ContentType)
	}
	if !strings.Contains(o.ContentType, "json") {
		return
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		s.t.Fatalf("%s: body is not JSON: %v (%s)", opID, err, body)
	}
	if err := o.Media.Schema.Value.VisitJSON(v, openapi3.MultiErrors(), openapi3.EnableFormatValidation()); err != nil {
		s.t.Fatalf("%s: body does not match the schema: %v\n%s", opID, err, body)
	}
}

// TestContract runs the generated client against the server for every operation of the spec
// (except the webhook and ops routes, which TestWebhook and TestOps cover): each succeeds with
// a response the spec allows.
func TestContract(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx
	sh := &shapes{t: t, ops: spec(t), seen: map[string]bool{}}

	// auth
	dc, err := e.c.GetDeviceConfigWithResponse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sh.check("getDeviceConfig", dc.HTTPResponse, dc.Body, http.StatusOK)
	if dc.JSON200.ClientId != "client-1" || !slices.Equal(dc.JSON200.Scopes, []string{"read:user"}) {
		t.Errorf("device config %+v", dc.JSON200)
	}
	lg, err := e.c.LoginWithResponse(ctx, &apiclient.LoginParams{ReturnTo: ptr("/board?view=all")})
	if err != nil {
		t.Fatal(err)
	}
	sh.check("login", lg.HTTPResponse, lg.Body, http.StatusFound)
	if loc := lg.HTTPResponse.Header.Get("Location"); !strings.HasPrefix(loc, e.github.URL+"/login/oauth/authorize?") ||
		!strings.Contains(loc, "client_id=client-1") || !strings.Contains(loc, "redirect_uri=") {
		t.Errorf("login location %q", loc)
	}
	state := cookieOf(t, lg.HTTPResponse.Header, "factory_oauth_state").Value
	cb, err := e.c.AuthCallbackWithResponse(ctx, &apiclient.AuthCallbackParams{Code: "code-gus", State: state},
		func(_ context.Context, r *http.Request) error {
			r.AddCookie(&http.Cookie{Name: "factory_oauth_state", Value: state})
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	sh.check("authCallback", cb.HTTPResponse, cb.Body, http.StatusFound)
	if loc := cb.HTTPResponse.Header.Get("Location"); loc != "/board?view=all" {
		t.Errorf("callback location %q, want the returnTo", loc)
	}
	gus := cookieOf(t, cb.HTTPResponse.Header, "factory_session").Value
	if c := cookieOf(t, cb.HTTPResponse.Header, "factory_oauth_state"); c == nil || c.MaxAge >= 0 {
		t.Errorf("the state cookie is not cleared: %+v", c)
	}
	me, err := e.c.GetMeWithResponse(ctx, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("getMe", me.HTTPResponse, me.Body, http.StatusOK)
	if me.JSON200.Login != "gus" || !me.JSON200.IsAdmin || me.JSON200.Name == nil || *me.JSON200.Name != "GUS" {
		t.Errorf("me %+v", me.JSON200)
	}

	// agents
	reg, err := e.c.RegisterAgentWithResponse(ctx, apiclient.RegisterAgentRequest{
		Pool: "local", Host: "mac1", AgentVersion: "0.1.0",
		Capabilities: v1alpha1.AgentCapabilities{Harnesses: []v1alpha1.HarnessOffer{{
			Name: "codex", Auth: "subscription", Models: []string{"gpt-5-codex"}, ReasoningEffort: []string{"medium"},
		}}},
		Lending: &v1alpha1.Lending{Queues: []string{"poc"}, MaxConcurrent: 1},
	}, header(HeaderJoinToken, joinSecret), header(HeaderOwnerToken, "tok-gus"))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("registerAgent", reg.HTTPResponse, reg.Body, http.StatusCreated)
	agent, tok := reg.JSON201.AgentId, reg.JSON201.AgentToken
	if agent != "agt_gus-mac1" {
		t.Fatalf("agent id %q", agent)
	}
	ua, err := e.c.UpdateAgentWithResponse(ctx, agent, apiclient.AgentUpdate{Phase: ptr(v1alpha1.AgentReady)}, bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("updateAgent", ua.HTTPResponse, ua.Body, http.StatusOK)
	la, err := e.c.ListAgentsWithResponse(ctx, &apiclient.ListAgentsParams{}, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("listAgents", la.HTTPResponse, la.Body, http.StatusOK)
	if len(la.JSON200.Items) != 1 || la.JSON200.NextCursor != nil {
		t.Errorf("agents %+v", la.JSON200)
	}

	// leases and tasks
	wait := "0s"
	none, err := e.c.LeaseTaskWithResponse(ctx, agent, &apiclient.LeaseTaskParams{Wait: &wait}, bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	if none.StatusCode() != http.StatusNoContent {
		t.Fatalf("lease with no task: %s", none.Status())
	}
	e.task("tsk_1", nil)
	ls, err := e.c.LeaseTaskWithResponse(ctx, agent, &apiclient.LeaseTaskParams{Wait: &wait}, bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("leaseTask", ls.HTTPResponse, ls.Body, http.StatusOK)
	lease := ls.JSON200
	if gc := lease.GitCredential; !strings.HasPrefix(gc.Token, "fake-author-") || gc.Repo != docsRepo || gc.Username == "" {
		t.Errorf("git credential %+v: the token must be the real one, copied field by field", gc)
	}
	if lease.Lease.Run == nil || lease.Lease.HeartbeatEvery != "30s" || lease.Task.Metadata.ID != "tsk_1" {
		t.Errorf("lease %+v", lease.Lease)
	}
	run := *lease.Lease.Run

	gt, err := e.c.GetTaskWithResponse(ctx, "tsk_1", bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("getTask", gt.HTTPResponse, gt.Body, http.StatusOK)
	if gt, err := e.c.GetTaskWithResponse(ctx, "tsk_1", session(gus)); err != nil || gt.StatusCode() != http.StatusOK {
		t.Fatalf("get task as UI: %v %s", err, gt.Status())
	}
	gp, err := e.c.GetTaskPromptWithResponse(ctx, "tsk_1", bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("getTaskPrompt", gp.HTTPResponse, gp.Body, http.StatusOK)
	if string(gp.Body) != "Implement "+docsIssue+"." || gp.HTTPResponse.Header.Get("Digest") != "sha256:abc" {
		t.Errorf("prompt %q digest %q", gp.Body, gp.HTTPResponse.Header.Get("Digest"))
	}
	gc, err := e.c.GetTaskContextWithResponse(ctx, "tsk_1", 0, bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("getTaskContext", gc.HTTPResponse, gc.Body, http.StatusOK)
	if c := gc.JSON200; c.Trust != "untrusted" || !strings.HasPrefix(c.Content, "`````untrusted source=\"github-public:"+docsIssue+"\" author=\"octocat\"\n") ||
		!strings.HasSuffix(c.Content, "\n`````\n") {
		t.Errorf("context %q: untrusted content must be fenced past its own backticks", c.Content)
	}
	hb, err := e.c.HeartbeatLeaseWithResponse(ctx, lease.Lease.Id, apiclient.HeartbeatRequest{
		Progress: []v1alpha1.ProgressEvent{{At: t0, Kind: "step", Message: "cloning"}},
		Usage:    &apiclient.UsageDelta{InputTokens: ptr(int64(10)), OutputTokens: ptr(int64(5))},
	}, bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("heartbeatLease", hb.HTTPResponse, hb.Body, http.StatusOK)
	if hb.JSON200.Cancel {
		t.Error("heartbeat says cancel")
	}

	// effects
	ef, err := e.c.CreateEffectWithResponse(ctx, "tsk_1", apiclient.EffectRequest{
		Op: v1alpha1.OpComment, Args: map[string]any{"body": "on it"}, IdempotencyKey: "tsk_1/comment/1",
	}, bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("createEffect", ef.HTTPResponse, ef.Body, http.StatusOK)

	// transcripts
	up, err := e.c.UploadTranscriptWithBodyWithResponse(ctx, run, "application/gzip",
		bytes.NewReader(gzipped(t, `{"type":"message"}`+"\n")), bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("uploadTranscript", up.HTTPResponse, up.Body, http.StatusOK)
	for _, as := range []apiclient.RequestEditorFn{bearer(tok), session(gus)} {
		tr, err := e.c.GetTranscriptWithResponse(ctx, run, as)
		if err != nil {
			t.Fatal(err)
		}
		sh.check("getTranscript", tr.HTTPResponse, tr.Body, http.StatusOK)
		if !bytes.Equal(tr.Body, gzipped(t, `{"type":"message"}`+"\n")) {
			t.Error("the transcript is not served back as uploaded")
		}
	}

	// resources
	gr, err := e.c.GetRunWithResponse(ctx, run, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("getRun", gr.HTTPResponse, gr.Body, http.StatusOK)
	if len(gr.JSON200.Effects) != 1 || gr.JSON200.Run.Status.TranscriptRef == "" || len(gr.JSON200.Run.Status.Progress) != 2 {
		t.Errorf("run detail: %d effects, transcript %q, progress %v", len(gr.JSON200.Effects), gr.JSON200.Run.Status.TranscriptRef, gr.JSON200.Run.Status.Progress)
	}
	gw, err := e.c.GetWorkItemWithResponse(ctx, workItemID, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("getWorkItem", gw.HTTPResponse, gw.Body, http.StatusOK)
	if len(gw.JSON200.Tasks) != 1 || len(gw.JSON200.Runs) != 1 || len(gw.JSON200.Effects) != 1 {
		t.Errorf("timeline: %d tasks, %d runs, %d effects", len(gw.JSON200.Tasks), len(gw.JSON200.Runs), len(gw.JSON200.Effects))
	}
	lw, err := e.c.ListWorkItemsWithResponse(ctx, &apiclient.ListWorkItemsParams{}, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("listWorkItems", lw.HTTPResponse, lw.Body, http.StatusOK)
	lt, err := e.c.ListTasksWithResponse(ctx, &apiclient.ListTasksParams{}, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("listTasks", lt.HTTPResponse, lt.Body, http.StatusOK)
	lq, err := e.c.ListQueuesWithResponse(ctx, &apiclient.ListQueuesParams{}, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("listQueues", lq.HTTPResponse, lq.Body, http.StatusOK)
	if q := lq.JSON200.Items; len(q) != 1 || q[0].Running != 1 || q[0].Depth != 0 {
		t.Errorf("queues %+v", q)
	}
	lp, err := e.c.ListPoolsWithResponse(ctx, &apiclient.ListPoolsParams{}, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("listPools", lp.HTTPResponse, lp.Body, http.StatusOK)
	if p := lp.JSON200.Items; len(p) != 1 || !slices.Equal(p[0].Agents, []string{agent}) {
		t.Errorf("pools %+v", p)
	}
	lpv, err := e.c.ListProvidersWithResponse(ctx, &apiclient.ListProvidersParams{}, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("listProviders", lpv.HTTPResponse, lpv.Body, http.StatusOK)
	if p := lpv.JSON200.Items; len(p) != 1 || p[0].Health.Status != apiclient.ProviderHealthStatusOk {
		t.Errorf("providers %+v", p)
	}
	gcat, err := e.c.GetCatalogWithResponse(ctx, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("getCatalog", gcat.HTTPResponse, gcat.Body, http.StatusOK)
	if c := gcat.JSON200; c.Status != apiclient.CatalogSummaryStatusLoaded || c.Sha != e.cat.SHA || c.Ref != "main" || len(c.Objects) == 0 {
		t.Errorf("catalog %+v", c)
	}
	gco, err := e.c.GetCatalogObjectWithResponse(ctx, apiclient.CatalogKindTaskSpec, "implement", session(gus))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("getCatalogObject", gco.HTTPResponse, gco.Body, http.StatusOK)
	if ts, err := gco.JSON200.AsTaskSpec(); err != nil || ts.Metadata.Name != "implement" || ts.Kind != v1alpha1.KindTaskSpec {
		t.Errorf("catalog object %+v %v", ts, err)
	}
	dr, err := e.c.DryRunCatalogWithResponse(ctx, apiclient.DryRunRequest{Event: v1alpha1.Event{
		ID: "github-public/gh-1/0", Source: ghProvider, Type: "issue.opened", Time: t0,
		Data: v1alpha1.EventData{
			Provider: ghProvider, Repo: docsRepo,
			Actor:  &v1alpha1.Actor{ID: "github-public:gus", Tier: "maintainer"},
			Object: &v1alpha1.ObjectRef{Provider: ghProvider, Kind: "issue", Ref: "compliance-framework/docs#77"},
		},
	}}, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("dryRunCatalog", dr.HTTPResponse, dr.Body, http.StatusOK)
	if !slices.ContainsFunc(dr.JSON200.Decisions, func(d apiclient.EvaluationDecision) bool {
		return d.Rule == "plan-maintainer-epics" && d.Action.Type == apiclient.ActionIntentTypeDispatch && d.Rail.Verdict == apiclient.EvaluationRailVerdictAllowed
	}) {
		t.Errorf("dry run decisions %s", dr.Body)
	}
	var tasks []v1alpha1.Task
	if _, err := e.st.List(ctx, v1alpha1.KindTask, store.ListOptions{}, &tasks); err != nil || len(tasks) != 1 {
		t.Errorf("the dry run created tasks: %d %v", len(tasks), err)
	}

	// stream
	res, err := e.c.StreamEvents(ctx, &apiclient.StreamEventsParams{Topics: []string{"tasks", "runs/" + run}}, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	_ = res.Body.Close()
	if err != nil || line != ": connected\n" {
		t.Fatalf("stream: %q %v", line, err)
	}
	sh.check("streamEvents", res, nil, http.StatusOK)

	// complete, then a second task to abandon
	cl, err := e.c.CompleteLeaseWithResponse(ctx, lease.Lease.Id, apiclient.CompleteRequest{
		Result:        v1alpha1.Result{Task: "tsk_1", Status: v1alpha1.ResultDone, Outcome: "pr-opened", Summary: "done"},
		TranscriptRef: ptr(TranscriptRefPrefix + run + ".jsonl.gz"),
	}, bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("completeLease", cl.HTTPResponse, cl.Body, http.StatusNoContent)
	if p := e.getTask("tsk_1").Status.Phase; p != v1alpha1.TaskSucceeded {
		t.Errorf("task after complete: %s", p)
	}
	e.task("tsk_2", nil)
	l2 := e.lease(agent, tok)
	ab, err := e.c.AbandonLeaseWithResponse(ctx, l2.Lease.Id, apiclient.AbandonRequest{Reason: apiclient.AbandonRequestReasonAgentError, Message: ptr("boom")}, bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("abandonLease", ab.HTTPResponse, ab.Body, http.StatusNoContent)

	// commands
	for _, cmd := range []apiclient.Command{
		{Type: apiclient.CommandTypePause, Target: &apiclient.CommandTarget{Scope: ptr(apiclient.CommandTargetScopeQueue), Name: ptr("poc")}},
		{Type: apiclient.CommandTypeResume, Target: &apiclient.CommandTarget{Scope: ptr(apiclient.CommandTargetScopeQueue), Name: ptr("poc")}},
		{Type: apiclient.CommandTypePause, Target: &apiclient.CommandTarget{Scope: ptr(apiclient.CommandTargetScopeWorkitem), Name: ptr(workItemID)}},
		{Type: apiclient.CommandTypeCancel, Target: &apiclient.CommandTarget{Task: ptr("tsk_2")}},
		{Type: apiclient.CommandTypeRedispatch, Target: &apiclient.CommandTarget{Task: ptr("tsk_1")}, Args: &apiclient.CommandArgs{Harness: &v1alpha1.HarnessRequest{Name: "claude-code"}}},
		{Type: apiclient.CommandTypeDispatch, Args: &apiclient.CommandArgs{Event: &v1alpha1.Event{Source: ghProvider, Type: "issue.opened", Data: v1alpha1.EventData{Repo: docsRepo}}}},
		{Type: apiclient.CommandTypeResyncCatalog},
	} {
		pc, err := e.c.PostCommandWithResponse(ctx, cmd, session(gus))
		if err != nil {
			t.Fatal(err)
		}
		sh.check("postCommand", pc.HTTPResponse, pc.Body, http.StatusAccepted)
	}
	if e.status.resyncs != 1 {
		t.Errorf("resyncs %d", e.status.resyncs)
	}

	// sign out, deregister
	lo, err := e.c.LogoutWithResponse(ctx, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("logout", lo.HTTPResponse, lo.Body, http.StatusNoContent)
	if me, _ := e.c.GetMeWithResponse(ctx, session(gus)); me.StatusCode() != http.StatusUnauthorized {
		t.Errorf("me after logout: %s", me.Status())
	}
	dg, err := e.c.DeregisterAgentWithResponse(ctx, agent, bearer(tok))
	if err != nil {
		t.Fatal(err)
	}
	sh.check("deregisterAgent", dg.HTTPResponse, dg.Body, http.StatusNoContent)

	for id, o := range sh.ops {
		if o.Tags[0] == "webhooks" || o.Tags[0] == "ops" {
			continue
		}
		if !sh.seen[id] {
			t.Errorf("operation %s is not covered", id)
		}
	}
}

// TestOperationsCovered checks that every generated operation has an access policy.
func TestOperationsCovered(t *testing.T) {
	ops := spec(t)
	for id, o := range ops {
		_, ok := operations[id]
		generated := o.Tags[0] != "webhooks" && o.Tags[0] != "ops"
		if ok != generated {
			t.Errorf("operation %s: access policy %v, generated %v", id, ok, generated)
		}
	}
	for id := range operations {
		if _, ok := ops[id]; !ok {
			t.Errorf("access policy for %s, which the spec doesn't have", id)
		}
	}
	// The policies follow the spec's security requirements.
	want := map[access][]string{
		agentOnly:     {"agentToken"},
		reader:        {"cliToken", "uiSession"},
		readerOrAgent: {"agentToken", "cliToken", "uiSession"},
		admin:         {"cliToken", "uiSession"},
		sessionOnly:   {"uiSession"},
		registration:  {"joinToken", "ownerToken"},
		public:        nil,
	}
	for id, acc := range operations {
		var got []string
		if sec := ops[id].Operation.Security; sec != nil {
			for _, req := range *sec {
				for name := range req {
					got = append(got, name)
				}
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, want[acc]) {
			t.Errorf("operation %s: spec security %v, policy %d expects %v", id, got, acc, want[acc])
		}
	}
}
