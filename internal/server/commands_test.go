package server

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/store"
)

func (e *env) command(cmd apiclient.Command) *apiclient.PostCommandResponse {
	e.t.Helper()
	r, err := e.c.PostCommandWithResponse(e.ctx, cmd, bearer(cliToken))
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func scoped(typ apiclient.CommandType, scope apiclient.CommandTargetScope, name string) apiclient.Command {
	c := apiclient.Command{Type: typ, Target: &apiclient.CommandTarget{Scope: &scope}}
	if name != "" {
		c.Target.Name = &name
	}
	return c
}

func TestPauseResume(t *testing.T) {
	e := newEnv(t)
	e.workItem()
	ks := controllers.StoreKillSwitches{Store: e.st}
	paused := func(scope, name string) bool {
		p, err := ks.Paused(e.ctx, scope, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	dryRun := func() apiclient.EvaluationRail {
		r, err := e.c.DryRunCatalogWithResponse(e.ctx, apiclient.DryRunRequest{Event: v1alpha1.Event{
			ID: "github-public/gh-9/0", Source: ghProvider, Type: "issue.opened",
			Data: v1alpha1.EventData{Repo: docsRepo, Actor: &v1alpha1.Actor{ID: "github-public:gus", Tier: "maintainer"},
				Object: &v1alpha1.ObjectRef{Provider: ghProvider, Kind: "issue", Ref: docsIssue}},
		}, WorkItem: ptr(workItemID)}, bearer(cliToken))
		if err != nil || r.JSON200 == nil {
			t.Fatalf("dry run: %v %s %s", err, r.Status(), r.Body)
		}
		for _, d := range r.JSON200.Decisions {
			if d.Action.Type == apiclient.ActionIntentTypeDispatch {
				return d.Rail
			}
		}
		t.Fatalf("no dispatch decision: %s", r.Body)
		return apiclient.EvaluationRail{}
	}
	if v := dryRun(); v.Verdict != apiclient.EvaluationRailVerdictAllowed {
		t.Fatalf("before pause: %+v", v)
	}

	// A queue pause suppresses dispatches to it (the rule controller's rails read the flags).
	if r := e.command(scoped(apiclient.CommandTypePause, apiclient.CommandTargetScopeQueue, "poc")); r.JSON202 == nil || r.JSON202.Event != nil {
		t.Fatalf("pause queue: %s %s", r.Status(), r.Body)
	}
	if !paused(controllers.ScopeQueue, "poc") {
		t.Fatal("queue poc not paused")
	}
	if v := dryRun(); v.Verdict != apiclient.EvaluationRailVerdictSuppressed || v.Reason == nil || !strings.Contains(*v.Reason, "paused: queue poc") {
		t.Fatalf("after pause: %+v", v)
	}
	e.command(scoped(apiclient.CommandTypeResume, apiclient.CommandTargetScopeQueue, "poc"))
	if paused(controllers.ScopeQueue, "poc") || dryRun().Verdict != apiclient.EvaluationRailVerdictAllowed {
		t.Fatal("queue poc still paused")
	}

	// Global (no name needed) and work item (an event on it).
	e.command(scoped(apiclient.CommandTypePause, apiclient.CommandTargetScopeGlobal, ""))
	if !paused(controllers.ScopeGlobal, "") {
		t.Fatal("not paused globally")
	}
	e.command(scoped(apiclient.CommandTypeResume, apiclient.CommandTargetScopeGlobal, ""))
	r := e.command(scoped(apiclient.CommandTypePause, apiclient.CommandTargetScopeWorkitem, workItemID))
	if r.JSON202 == nil || r.JSON202.Event == nil {
		t.Fatalf("pause work item: %s", r.Body)
	}
	var ev v1alpha1.Event
	if err := e.st.Get(e.ctx, store.Key{Kind: v1alpha1.KindEvent, Name: *r.JSON202.Event}, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != "factory.command.pause" || ev.WorkItem != workItemID || ev.Data.Actor == nil || ev.Data.Actor.ID != "factory:cli" {
		t.Errorf("event %+v", ev)
	}

	for _, c := range []struct {
		cmd    apiclient.Command
		status int
	}{
		{scoped(apiclient.CommandTypePause, apiclient.CommandTargetScopeQueue, ""), http.StatusBadRequest},
		{scoped(apiclient.CommandTypePause, apiclient.CommandTargetScopeQueue, "nope"), http.StatusNotFound},
		{scoped(apiclient.CommandTypePause, apiclient.CommandTargetScopeWorkitem, "wi_nope"), http.StatusNotFound},
		{scoped(apiclient.CommandTypePause, "everything", "x"), http.StatusBadRequest},
		{apiclient.Command{Type: apiclient.CommandTypePause}, http.StatusBadRequest},
		{apiclient.Command{Type: "reboot"}, http.StatusBadRequest},
	} {
		r := e.command(c.cmd)
		wantProblem(t, fmt.Sprintf("command %+v", c.cmd), r.HTTPResponse, r.Body, c.status, "")
	}
}

func TestCancelAndRedispatch(t *testing.T) {
	e := newEnv(t)
	agent, tok := e.register("gus", "mac1")
	e.task("tsk_1", nil)
	l := e.lease(agent, tok)
	task := func(id string) apiclient.Command {
		return apiclient.Command{Type: apiclient.CommandTypeCancel, Target: &apiclient.CommandTarget{Task: &id}}
	}
	redispatch := func(id string, harness string) apiclient.Command {
		c := apiclient.Command{Type: apiclient.CommandTypeRedispatch, Target: &apiclient.CommandTarget{Task: &id}}
		if harness != "" {
			c.Args = &apiclient.CommandArgs{Harness: &v1alpha1.HarnessRequest{Name: harness}}
		}
		return c
	}

	r := e.command(redispatch("tsk_1", ""))
	wantProblem(t, "redispatch an active task", r.HTTPResponse, r.Body, http.StatusConflict, "task-active")
	r = e.command(task("tsk_1"))
	if r.JSON202 == nil || r.JSON202.Event == nil {
		t.Fatalf("cancel: %s", r.Body)
	}
	if st := e.getTask("tsk_1").Status; st.Phase != v1alpha1.TaskCancelling || st.Reason != controllers.ReasonCancelled {
		t.Fatalf("after cancel: %+v", st)
	}
	// The agent learns it on its heartbeat.
	hb, err := e.c.HeartbeatLeaseWithResponse(e.ctx, l.Lease.Id, apiclient.HeartbeatRequest{}, bearer(tok))
	if err != nil || hb.JSON200 == nil || !hb.JSON200.Cancel {
		t.Fatalf("heartbeat after cancel: %v %s", err, hb.Body)
	}
	r = e.command(task("tsk_nope"))
	wantProblem(t, "cancel an unknown task", r.HTTPResponse, r.Body, http.StatusNotFound, CodeNotFound)

	// Once it's finished, it's no longer active.
	tk := e.getTask("tsk_1")
	tk.Status.Phase = v1alpha1.TaskFailed
	if err := e.st.Update(e.ctx, tk); err != nil {
		t.Fatal(err)
	}
	r = e.command(task("tsk_1"))
	wantProblem(t, "cancel a finished task", r.HTTPResponse, r.Body, http.StatusConflict, CodeTaskNotActive)

	r = e.command(redispatch("tsk_1", "claude-code"))
	if r.JSON202 == nil || r.JSON202.Event == nil {
		t.Fatalf("redispatch: %s", r.Body)
	}
	var tasks []v1alpha1.Task
	if _, err := e.st.List(e.ctx, v1alpha1.KindTask, store.ListOptions{Field: map[string]string{store.FieldTaskPhase: string(v1alpha1.TaskPending)}}, &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("%d pending tasks", len(tasks))
	}
	nt := tasks[0]
	if nt.Metadata.ID == "tsk_1" || nt.Spec.Harness.Request.Name != "claude-code" || nt.Spec.Harness.Resolved != nil ||
		nt.Metadata.Provenance.Event != *r.JSON202.Event || nt.Spec.Prompt.Ref != "api://tasks/"+nt.Metadata.ID+"/prompt" ||
		nt.Status.Agent != nil || nt.Metadata.WorkItem != workItemID {
		t.Errorf("clone %+v", nt)
	}
	var p controllers.TaskPrompt
	if err := e.st.Get(e.ctx, store.Key{Kind: controllers.KindTaskPrompt, Name: nt.Metadata.ID}, &p); err != nil || p.Text == "" {
		t.Errorf("the prompt was not copied: %v", err)
	}
	r = e.command(redispatch("tsk_1", ""))
	if r.StatusCode() != http.StatusAccepted {
		t.Errorf("a second redispatch: %s", r.Status())
	}
}

func TestDispatch(t *testing.T) {
	e := newEnv(t)
	e.workItem()
	dispatch := func(ev v1alpha1.Event) *apiclient.PostCommandResponse {
		return e.command(apiclient.Command{Type: apiclient.CommandTypeDispatch, Args: &apiclient.CommandArgs{Event: &ev}})
	}
	r := dispatch(v1alpha1.Event{Source: ghProvider, Type: "issue.opened", Data: v1alpha1.EventData{Repo: docsRepo}})
	if r.JSON202 == nil || r.JSON202.Event == nil || !strings.HasPrefix(*r.JSON202.Event, InjectedEventPrefix) {
		t.Fatalf("dispatch: %s", r.Body)
	}
	var ev v1alpha1.Event
	if err := e.st.Get(e.ctx, store.Key{Kind: v1alpha1.KindEvent, Name: *r.JSON202.Event}, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Time.IsZero() || ev.Data.Provider != ghProvider || ev.Kind != v1alpha1.KindEvent {
		t.Errorf("stored %+v", ev)
	}
	if r := dispatch(v1alpha1.Event{ID: "e1", Source: "factory", Type: "factory.schedule.tick", WorkItem: workItemID}); r.JSON202 == nil {
		t.Fatalf("a factory event: %s", r.Body)
	}
	for _, c := range []struct {
		name   string
		ev     v1alpha1.Event
		status int
	}{
		{"an unknown source", v1alpha1.Event{Source: "gitlab", Type: "issue.opened"}, http.StatusBadRequest},
		{"a type the provider doesn't emit", v1alpha1.Event{Source: ghProvider, Type: "message.posted"}, http.StatusBadRequest},
		{"an unknown type", v1alpha1.Event{Source: ghProvider, Type: "issue.exploded"}, http.StatusBadRequest},
		{"a factory event without a work item", v1alpha1.Event{Source: "factory", Type: "factory.schedule.tick"}, http.StatusBadRequest},
		{"a provider type from the factory", v1alpha1.Event{Source: "factory", Type: "issue.opened", WorkItem: workItemID}, http.StatusBadRequest},
		{"a factory id from a provider", v1alpha1.Event{ID: "factory/x", Source: ghProvider, Type: "issue.opened"}, http.StatusBadRequest},
		{"a duplicate id", v1alpha1.Event{ID: "e1", Source: "factory", Type: "factory.schedule.tick", WorkItem: workItemID}, http.StatusConflict},
	} {
		r := dispatch(c.ev)
		wantProblem(t, c.name, r.HTTPResponse, r.Body, c.status, "")
	}
	r = e.command(apiclient.Command{Type: apiclient.CommandTypeDispatch})
	wantProblem(t, "dispatch without an event", r.HTTPResponse, r.Body, http.StatusBadRequest, CodeInvalid)
}

func TestListPagination(t *testing.T) {
	e := newEnv(t)
	for i := range 5 {
		e.task(fmt.Sprintf("tsk_%d", i), func(t *v1alpha1.Task) {
			t.Metadata.Labels = map[string]string{"pack": []string{"intake", "docs"}[i%2]}
		})
		e.clk.Advance(1)
	}
	var ids []string
	cursor := (*string)(nil)
	for {
		r, err := e.c.ListTasksWithResponse(e.ctx, &apiclient.ListTasksParams{Limit: ptr(2), Cursor: cursor}, bearer(cliToken))
		if err != nil || r.JSON200 == nil {
			t.Fatalf("list: %v %s", err, r.Body)
		}
		for _, t := range r.JSON200.Items {
			ids = append(ids, t.Metadata.ID)
		}
		if r.JSON200.NextCursor == nil {
			break
		}
		cursor = r.JSON200.NextCursor
	}
	if want := []string{"tsk_4", "tsk_3", "tsk_2", "tsk_1", "tsk_0"}; !slices.Equal(ids, want) {
		t.Errorf("newest first: %v, want %v", ids, want)
	}
	for sel, want := range map[string]int{"pack=intake": 3, "pack!=intake": 2, "pack in (docs)": 2, "pack notin (docs,intake)": 0, "pack": 5, "!pack": 0} {
		r, err := e.c.ListTasksWithResponse(e.ctx, &apiclient.ListTasksParams{LabelSelector: ptr(sel)}, bearer(cliToken))
		if err != nil || r.JSON200 == nil {
			t.Fatalf("selector %q: %v %s", sel, err, r.Body)
		}
		if len(r.JSON200.Items) != want {
			t.Errorf("selector %q: %d items, want %d", sel, len(r.JSON200.Items), want)
		}
	}
	for _, bad := range []apiclient.ListTasksParams{{LabelSelector: ptr("a=(b")}, {Limit: ptr(0)}, {Limit: ptr(501)}, {Cursor: ptr("garbage")}} {
		r, err := e.c.ListTasksWithResponse(e.ctx, &bad, bearer(cliToken))
		if err != nil {
			t.Fatal(err)
		}
		wantProblem(t, fmt.Sprintf("params %+v", bad), r.HTTPResponse, r.Body, http.StatusBadRequest, CodeInvalid)
	}
	// A cursor of another list is refused.
	r, _ := e.c.ListTasksWithResponse(e.ctx, &apiclient.ListTasksParams{Limit: ptr(1)}, bearer(cliToken))
	w, _ := e.c.ListWorkItemsWithResponse(e.ctx, &apiclient.ListWorkItemsParams{Cursor: r.JSON200.NextCursor}, bearer(cliToken))
	wantProblem(t, "a task cursor on work items", w.HTTPResponse, w.Body, http.StatusBadRequest, CodeInvalid)
}

func TestNotFound(t *testing.T) {
	e := newEnv(t)
	cli := bearer(cliToken)
	for name, do := range map[string]func() (int, []byte){
		"work item": func() (int, []byte) {
			r, _ := e.c.GetWorkItemWithResponse(e.ctx, "wi_x", cli)
			return r.StatusCode(), r.Body
		},
		"run": func() (int, []byte) {
			r, _ := e.c.GetRunWithResponse(e.ctx, "run_x", cli)
			return r.StatusCode(), r.Body
		},
		"task": func() (int, []byte) {
			r, _ := e.c.GetTaskWithResponse(e.ctx, "tsk_x", cli)
			return r.StatusCode(), r.Body
		},
		"catalog object": func() (int, []byte) {
			r, _ := e.c.GetCatalogObjectWithResponse(e.ctx, apiclient.CatalogKindQueue, "nope", cli)
			return r.StatusCode(), r.Body
		},
		"transcript": func() (int, []byte) {
			r, _ := e.c.GetTranscriptWithResponse(e.ctx, "run_x", cli)
			return r.StatusCode(), r.Body
		},
		"dry-run work item": func() (int, []byte) {
			r, _ := e.c.DryRunCatalogWithResponse(e.ctx, apiclient.DryRunRequest{Event: v1alpha1.Event{Source: ghProvider, Type: "issue.opened"}, WorkItem: ptr("wi_x")}, cli)
			return r.StatusCode(), r.Body
		},
	} {
		if code, body := do(); code != http.StatusNotFound {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	r, _ := e.c.GetCatalogObjectWithResponse(e.ctx, "Widget", "x", cli)
	if r.StatusCode() != http.StatusBadRequest {
		t.Errorf("an unknown kind: %s", r.Status())
	}
}
