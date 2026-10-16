package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/fake"
	"github.com/compliance-framework/factory/internal/rules"
	"github.com/compliance-framework/factory/internal/store"
)

// The PoC catalog has no triage TaskSpec; its intake chain is the docs-epic pack's:
// issue.opened (maintainer) → plan-maintainer-epics dispatches plan-epic → the plan-epic run
// completes "decomposed" → build-stack-start sets the plan, comments it (epic-plan) and
// dispatches implement for the first sub-issue.

var planData = map[string]any{"plan": []any{
	map[string]any{"issue": docsRepo + "#43", "title": "Translate the intro"},
	map[string]any{"issue": docsRepo + "#44", "title": "Translate the guides"},
}}

// startPlan opens docs#42 as a maintainer and returns its work item and plan-epic task.
func (e *env) startPlan() (*v1alpha1.WorkItem, *v1alpha1.Task) {
	e.t.Helper()
	e.seed(provider.KindIssue, docsRepo+"#42", nil)
	e.deliver(issueOpened(docsRepo+"#42", maintainer))
	e.processEvents()
	wi := e.workItemOf(docsRepo + "#42")
	ts := e.tasksOf(wi.Metadata.Name, "plan-epic")
	if len(ts) != 1 {
		e.t.Fatalf("plan-epic tasks = %d, want 1", len(ts))
	}
	return wi, ts[0]
}

// completePlan runs the plan-epic task to "decomposed" and processes the resulting events.
func (e *env) completePlan(task string) {
	e.t.Helper()
	e.lease(task)
	phase, err := e.run.Complete(e.ctx, task, doneResult("decomposed", planData))
	if err != nil || phase != v1alpha1.TaskSucceeded {
		e.t.Fatalf("Complete = %s, %v; want Succeeded", phase, err)
	}
	e.processEvents()
}

func TestCascadeRunningControllers(t *testing.T) {
	e := newEnv(t)
	e.seed(provider.KindIssue, docsRepo+"#42", nil)
	for _, run := range []func(context.Context) error{e.rule.Run, e.dbn.Run, e.run.Run, e.wic.Run} {
		runInBackground(t, run)
	}

	e.deliver(issueOpened(docsRepo+"#42", maintainer))
	var wi *v1alpha1.WorkItem
	var plan *v1alpha1.Task
	eventually(t, "the plan-epic task", func() bool {
		wi = findWorkItem(e, docsRepo+"#42")
		if wi == nil {
			return false
		}
		ts := e.tasksOf(wi.Metadata.Name, "plan-epic")
		if len(ts) == 1 {
			plan = ts[0]
		}
		return plan != nil
	})
	if got := plan.Status.Phase; got != v1alpha1.TaskPending {
		t.Fatalf("plan-epic phase = %s, want Pending", got)
	}

	e.lease(plan.Metadata.ID)
	eventually(t, "the run", func() bool { return len(e.runs(plan.Metadata.ID)) == 1 })
	if _, err := e.run.Complete(e.ctx, plan.Metadata.ID, doneResult("decomposed", planData)); err != nil {
		t.Fatal(err)
	}

	// factory.run.completed → build-stack-start: state, the plan comment and the next task.
	var impl *v1alpha1.Task
	eventually(t, "the implement task", func() bool {
		ts := e.tasksOf(wi.Metadata.Name, "implement")
		if len(ts) == 1 {
			impl = ts[0]
		}
		return impl != nil
	})
	if got := impl.Spec.Inputs; got["issue"] != docsRepo+"#43" || got["base"] != "main" || got["mode"] != "new" || got["target"] != "pr" {
		t.Errorf("implement inputs = %v", got)
	}
	if got := impl.Metadata.Provenance; got.Rule != "build-stack-start" || got.RuleSet != "poc-docs-epic" || !strings.HasPrefix(got.Event, FactoryEventPrefix+"run/") {
		t.Errorf("implement provenance = %+v", got)
	}
	eventually(t, "the work item's state", func() bool {
		st := e.getWorkItem(wi.Metadata.Name).Status
		return st.State["stackTotal"] == float64(2) && st.Counters["plan-epic"] == 1
	})
	eventually(t, "the epic-plan comment", func() bool {
		for _, a := range e.applies(v1alpha1.OpComment) {
			if body, _ := a.Request.Args["body"].(string); strings.Contains(body, docsRepo+"#43") && a.Request.Target.Ref == docsRepo+"#42" {
				return true
			}
		}
		return false
	})
	eventually(t, "the projection on docs#42", func() bool {
		o, _ := e.gh.Object(docsRepo + "#42")
		return len(o.Projections) == 1 && o.Projections[0].WorkItemID == wi.Metadata.Name &&
			o.Projections[0].State["stackTotal"] == float64(2) && o.Projections[0].Counters["plan-epic"] == 1
	})
}

func findWorkItem(e *env, ref string) *v1alpha1.WorkItem {
	var wis []*v1alpha1.WorkItem
	if _, err := e.st.List(e.ctx, v1alpha1.KindWorkItem, store.ListOptions{
		Field: map[string]string{store.FieldWorkItemLink: store.LinkFieldValue(ghProvider, ref)},
	}, &wis); err != nil || len(wis) != 1 {
		return nil
	}
	return wis[0]
}

func TestCompiledTask(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	if wi.Status.Reporter == nil || wi.Status.Reporter.Tier != "maintainer" {
		t.Errorf("reporter = %+v, want the maintainer", wi.Status.Reporter)
	}
	if got := wi.Status.State["plan"]; got != "planning" {
		t.Errorf("state.plan = %v, want planning", got)
	}
	s := plan.Spec
	if s.Queue != "poc" || s.Pool != "local" || s.Provider != ghProvider || s.Target.Repo != docsRepo {
		t.Errorf("queue/pool/provider/target = %s/%s/%s/%+v", s.Queue, s.Pool, s.Provider, s.Target)
	}
	if s.Harness == nil || s.Harness.Request == nil || s.Harness.Request.Name != "codex" || s.Harness.Resolved != nil {
		t.Errorf("harness = %+v", s.Harness)
	}
	cred := s.Credentials.Git
	if cred.Repo != docsRepo || cred.Identity != "author" || cred.Permissions["issues"] != "write" || cred.TTL != "30m0s" || s.Credentials.LLM != LLMAgentLocal {
		t.Errorf("credentials = %+v (llm %s)", cred, s.Credentials.LLM)
	}
	wantCtx := []v1alpha1.ContextItem{
		{Kind: provider.KindIssue, Provider: ghProvider, Ref: docsRepo + "#42", Trust: TrustUntrusted},
		{Kind: "repo", Provider: ghProvider, Ref: docsRepo + "@main", Mount: "/work/docs"},
	}
	if !slices.Equal(s.Context, wantCtx) {
		t.Errorf("context = %+v, want %+v", s.Context, wantCtx)
	}
	if s.Prompt == nil || s.Prompt.Ref != "api://tasks/"+plan.Metadata.ID+"/prompt" || !strings.HasPrefix(s.Prompt.Digest, "sha256:") {
		t.Errorf("prompt = %+v", s.Prompt)
	}
	var p TaskPrompt
	if err := e.st.Get(e.ctx, store.Key{Kind: KindTaskPrompt, Name: plan.Metadata.ID}, &p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text, "# Task: plan-epic for "+docsRepo) || p.Digest != s.Prompt.Digest {
		t.Errorf("prompt text = %q", p.Text)
	}
	if len(s.Skills) != 1 || s.Skills[0].Ref != "v0.8.0" || s.Output == nil || s.Limits.MaxWallClock != "20m" {
		t.Errorf("skills/output/limits = %+v / %+v / %+v", s.Skills, s.Output, s.Limits)
	}
	if plan.Metadata.Provenance.CatalogSHA != e.cat.SHA || plan.Metadata.Labels[LabelTaskSpec] != "plan-epic" {
		t.Errorf("metadata = %+v", plan.Metadata)
	}
	if s.Requires != nil {
		t.Errorf("requires = %+v, want none (plan-epic requires no labels)", s.Requires)
	}

	// Requires: the TaskSpec's labels merged with the dispatch's, sorted and deduplicated.
	task, _, err := compileTask(e.ctx, compileInput{
		cat: e.cat, wi: wi, ev: v1alpha1.Event{ID: "ev"}, taskID: "tsk_x", now: e.clk.Now(),
		d: &rules.Dispatch{TaskSpec: "docs-review", Queue: "poc", Requires: &v1alpha1.Requires{Labels: []string{"has-local-stack", "macos", "has-local-stack"}}},
	}, KeepSkills{})
	if err != nil {
		t.Fatal(err)
	}
	if got := task.Spec.Requires; got == nil || !slices.Equal(got.Labels, []string{"has-local-stack", "macos"}) {
		t.Errorf("requires = %+v", got)
	}
	if _, ok := task.Metadata.Labels["factory.ccf.dev/requires"]; ok {
		t.Error("requires is still a label")
	}
}

func TestEventWithoutMatchingRuleMakesNoWorkItem(t *testing.T) {
	e := newEnv(t)
	e.seed(provider.KindIssue, docsRepo+"#7", nil)
	e.deliver(issueOpened(docsRepo+"#7", "stranger")) // tier anyone: plan-maintainer-epics doesn't fire
	if n := e.processEvents(); n != 1 {
		t.Fatalf("processed %d events, want 1", n)
	}
	var wis []*v1alpha1.WorkItem
	if _, err := e.st.List(e.ctx, v1alpha1.KindWorkItem, store.ListOptions{}, &wis); err != nil || len(wis) != 0 {
		t.Fatalf("work items = %d (%v), want none", len(wis), err)
	}
}

func TestDedupeTwoRuleSetsNotifyAndEffect(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	e.completePlan(plan.Metadata.ID)
	impl := e.tasksOf(wi.Metadata.Name, "implement")[0]
	e.lease(impl.Metadata.ID)
	if _, err := e.run.Complete(e.ctx, impl.Metadata.ID, doneResult("blocked", nil)); err != nil {
		t.Fatal(err)
	}
	e.processEvents()

	// escalate-needs-human is selected by poc-docs-epic and poc-docs-human-review (pack
	// shared): it fires once, through the first by name.
	labels := e.applies(v1alpha1.OpLabelAdd)
	if len(labels) != 1 {
		t.Fatalf("label.add applies = %d, want 1", len(labels))
	}
	if got := labels[0].Request; got.Target.Ref != docsRepo+"#42" || !slices.Equal(stringsOf(got.Args["labels"]), []string{"needs-human"}) ||
		!strings.HasSuffix(got.IdempotencyKey, "/escalate-needs-human/0") || got.Identity != "author" {
		t.Errorf("label.add = %+v", got)
	}
	var needsHuman []fake.ApplyCall
	for _, a := range e.applies(v1alpha1.OpComment) {
		if body, _ := a.Request.Args["body"].(string); strings.Contains(body, "The factory needs a human: blocked from implement") {
			needsHuman = append(needsHuman, a)
		}
	}
	if len(needsHuman) != 1 || !strings.Contains(needsHuman[0].Request.Args["body"].(string), "https://factory.test/workitems/"+wi.Metadata.Name) {
		t.Fatalf("needs-human comments = %+v, want 1 with the UI link", needsHuman)
	}
	if o, _ := e.gh.Object(docsRepo + "#42"); !slices.Contains(o.Labels, "needs-human") {
		t.Errorf("docs#42 labels = %v", o.Labels)
	}

	var fired, dup *v1alpha1.Decision
	for _, d := range e.getWorkItem(wi.Metadata.Name).Status.Decisions {
		if d.Rule != "escalate-needs-human" || !strings.Contains(d.Event, impl.Metadata.ID[len("tsk_"):]) {
			continue
		}
		switch d.RuleSet {
		case "poc-docs-epic":
			fired = &d
		case "poc-docs-human-review":
			dup = &d
		}
	}
	if fired == nil || !fired.Fired || len(fired.Actions) != 2 || fired.Rails[0].Verdict != rules.VerdictAllowed {
		t.Errorf("fired decision = %+v", fired)
	}
	if dup == nil || dup.Fired || dup.Reason != "already fired through RuleSet poc-docs-epic" {
		t.Errorf("duplicate decision = %+v", dup)
	}
	var effects []*v1alpha1.Effect
	if _, err := e.st.List(e.ctx, v1alpha1.KindEffect, store.ListOptions{}, &effects); err != nil || len(effects) != 3 {
		t.Errorf("effect records = %d (%v), want 3 (epic-plan, label, needs-human)", len(effects), err)
	}
}

func stringsOf(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
	case []string:
		out = x
	}
	return out
}

func TestSameEventTwiceIsIdempotent(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	e.completePlan(plan.Metadata.ID)
	done := e.events(v1alpha1.EventFactoryRunCompleted)
	if len(done) != 1 {
		t.Fatalf("run.completed events = %d", len(done))
	}

	// Processed: a second reconcile is a no-op.
	if _, err := e.rule.Reconcile(e.ctx, done[0].ID); err != nil {
		t.Fatal(err)
	}
	// Even forgetting it was processed (a crash before the mark), replaying it makes no
	// second task and no second comment.
	if err := e.st.Delete(e.ctx, store.Key{Kind: KindEventStatus, Name: done[0].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.rule.Reconcile(e.ctx, done[0].ID); err != nil {
		t.Fatal(err)
	}
	if n := len(e.tasksOf(wi.Metadata.Name, "implement")); n != 1 {
		t.Errorf("implement tasks = %d, want 1", n)
	}
	comments := 0
	for _, a := range e.gh.Effects() {
		if a.Request.Op == v1alpha1.OpComment {
			comments++
		}
	}
	if comments != 1 {
		t.Errorf("comments applied = %d, want 1", comments)
	}
	if len(e.applies(v1alpha1.OpComment)) != 2 {
		t.Errorf("the replayed comment should reach the provider as a replay: %+v", e.applies(v1alpha1.OpComment))
	}
}

func TestCancelThenDispatch(t *testing.T) {
	e := newEnv(t)
	pr := docsRepo + "#50"
	e.seed(provider.KindPR, pr, nil)
	prEvent := func(typ v1alpha1.EventType) {
		e.deliver(fake.Webhook{Type: typ, Sender: fake.Sender{Login: "alice"}, Object: &v1alpha1.ObjectRef{Kind: provider.KindPR, Ref: pr}})
		e.processEvents()
	}
	prEvent(v1alpha1.EventPROpened)
	wi := e.workItemOf(pr)
	first := e.tasksOf(wi.Metadata.Name, "docs-review")
	if len(first) != 1 {
		t.Fatalf("docs-review tasks = %d, want 1", len(first))
	}
	leased := first[0].Metadata.ID
	e.lease(leased)

	// pr.updated: review-human-prs cancels the review in flight and dispatches a new one (the
	// cancel frees the single-active-run rail).
	prEvent(v1alpha1.EventPRUpdated)
	if got := e.task(leased).Status; got.Phase != v1alpha1.TaskCancelling || got.Reason != ReasonCancelled {
		t.Fatalf("leased task = %+v, want Cancelling", got)
	}
	ts := e.tasksOf(wi.Metadata.Name, "docs-review")
	if len(ts) != 2 {
		t.Fatalf("docs-review tasks = %d, want 2", len(ts))
	}
	pending := ts[0].Metadata.ID
	if pending == leased {
		pending = ts[1].Metadata.ID
	}

	// The agent holds the cancelled task: it learns it from the heartbeat, then acknowledges.
	if _, err := e.run.Reconcile(e.ctx, leased); err != nil {
		t.Fatal(err)
	}
	if c, err := e.run.CancelRequested(e.ctx, leased); err != nil || !c {
		t.Fatalf("CancelRequested = %v, %v", c, err)
	}
	if err := e.run.AckCancel(e.ctx, leased); err != nil {
		t.Fatal(err)
	}
	if got := e.task(leased).Status.Phase; got != v1alpha1.TaskCancelled {
		t.Errorf("after ack: %s, want Cancelled", got)
	}
	failed := e.events(v1alpha1.EventFactoryRunFailed)
	if len(failed) != 1 || runFact(failed[0], "status") != "cancelled" {
		t.Fatalf("run.failed events = %+v", failed)
	}

	// A task no agent holds is cancelled at once, without a run event.
	prEvent(v1alpha1.EventPRUpdated)
	if got := e.task(pending).Status.Phase; got != v1alpha1.TaskCancelling {
		t.Fatalf("pending task = %s, want Cancelling", got)
	}
	if _, err := e.run.Reconcile(e.ctx, pending); err != nil {
		t.Fatal(err)
	}
	if got := e.task(pending).Status.Phase; got != v1alpha1.TaskCancelled {
		t.Errorf("pending task after reconcile = %s, want Cancelled", got)
	}
	if n := len(e.events(v1alpha1.EventFactoryRunFailed)); n != 1 {
		t.Errorf("run.failed events = %d, want still 1", n)
	}
}

func runFact(ev *v1alpha1.Event, key string) any {
	run, _ := ev.Data.Facts["run"].(map[string]any)
	return run[key]
}

func TestRailsBudgetAndStopLabel(t *testing.T) {
	e := newEnv(t)
	// The poc queue's 40 daily runs are spent: the dispatch is deferred, not dropped.
	day := e.clk.Now().UTC().Format(time.DateOnly)
	if err := e.st.Create(e.ctx, &v1alpha1.BudgetCounter{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindBudgetCounter},
		Metadata: v1alpha1.ObjectMeta{Name: "budget.queue.poc." + day},
		Status:   v1alpha1.BudgetCounterStatus{Runs: 40},
	}); err != nil {
		t.Fatal(err)
	}
	wi, plan := e.startPlan()
	if got := plan.Status; got.Phase != v1alpha1.TaskPending || got.Reason != ReasonBudget {
		t.Errorf("deferred task status = %+v, want Pending{budget}", got)
	}
	if d := lastFired(e.getWorkItem(wi.Metadata.Name)); d == nil || d.Rails[0].Verdict != rules.VerdictDeferred || d.Rails[0].Rail != rules.RailBudget {
		t.Errorf("decision = %+v", d)
	}

	// factory:stop on the issue suppresses every action of the item.
	e.seed(provider.KindIssue, docsRepo+"#61", func(o *provider.Observation) { o.Labels = []string{StopLabel} })
	e.deliver(issueOpened(docsRepo+"#61", maintainer))
	e.processEvents()
	stopped := e.workItemOf(docsRepo + "#61")
	if n := len(e.tasks(stopped.Metadata.Name)); n != 0 {
		t.Errorf("tasks of a stopped item = %d, want 0", n)
	}
	d := lastFired(stopped)
	if d == nil || len(d.Rails) != 2 || d.Rails[0].Verdict != rules.VerdictSuppressed || d.Rails[0].Rail != rules.RailKillSwitch {
		t.Fatalf("decision = %+v", d)
	}
	if _, ok := stopped.Status.State["plan"]; ok {
		t.Errorf("a suppressed set was applied: %v", stopped.Status.State)
	}
}

func lastFired(wi *v1alpha1.WorkItem) *v1alpha1.Decision {
	for i := len(wi.Status.Decisions) - 1; i >= 0; i-- {
		if wi.Status.Decisions[i].Fired {
			return &wi.Status.Decisions[i]
		}
	}
	return nil
}

func TestStoreRailsCheck(t *testing.T) {
	dispatch := func(spec, queue string) rules.ActionIntent {
		return rules.ActionIntent{Dispatch: &rules.Dispatch{TaskSpec: spec, Queue: queue}}
	}
	tests := []struct {
		name  string
		rails storeRails
		do    []rules.ActionIntent
		want  []string // rail:verdict per action
	}{
		{"run cap", storeRails{tasks: 20, runCap: 20}, []rules.ActionIntent{dispatch("a", "q")}, []string{"run-cap:suppressed"}},
		{"cap reached within the evaluation", storeRails{tasks: 19, runCap: 20, active: map[string]int{}},
			[]rules.ActionIntent{dispatch("a", "q"), dispatch("b", "q")}, []string{":allowed", "run-cap:suppressed"}},
		{"single active run", storeRails{runCap: 20, active: map[string]int{"a": 1}}, []rules.ActionIntent{dispatch("a", "q"), dispatch("b", "q")},
			[]string{"active-run:suppressed", ":allowed"}},
		{"cancel frees the spec", storeRails{runCap: 20, active: map[string]int{"a": 1}},
			[]rules.ActionIntent{{Cancel: &rules.Cancel{TaskSpec: "a"}}, dispatch("a", "q"), dispatch("a", "q")},
			[]string{":allowed", ":allowed", "active-run:suppressed"}},
		{"paused queue", storeRails{runCap: 20, active: map[string]int{}, pausedQueues: map[string]bool{"q": true}},
			[]rules.ActionIntent{dispatch("a", "q"), {Set: &rules.Set{Key: "k"}}}, []string{"kill-switch:suppressed", ":allowed"}},
		{"budget", storeRails{runCap: 20, active: map[string]int{}, overBudget: map[string]string{"q": "spent"}},
			[]rules.ActionIntent{dispatch("a", "q"), dispatch("a", "q")}, []string{"budget:deferred", "active-run:suppressed"}},
		{"stopped", storeRails{stopped: "label"}, []rules.ActionIntent{{Set: &rules.Set{Key: "k"}}}, []string{"kill-switch:suppressed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.rails
			if r.active == nil {
				r.active = map[string]int{}
			}
			var got []string
			for _, a := range tt.do {
				v := r.Check(a)
				got = append(got, v.Rail+":"+v.Verdict)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("verdicts = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDebounceFiresAtUntil(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	until := e.clk.Now().Add(5 * time.Minute)
	rec := &DebounceRecord{
		TypeMeta: v1alpha1.TypeMeta{Kind: KindDebounce},
		Metadata: v1alpha1.ObjectMeta{Name: "dbn_test"},
		Spec: DebounceSpec{
			Key: "r/" + wi.Metadata.Name, WorkItem: wi.Metadata.Name, Event: "none", RuleSet: "poc-docs-epic", Rule: "r", Until: until,
			Then: []rules.ActionIntent{
				{Set: &rules.Set{Key: "announced", Value: true}},
				{Notify: &rules.Notify{Template: "ptbr-live", To: []v1alpha1.Destination{{Item: "primary"}}}},
			},
		},
	}
	if err := e.st.Create(e.ctx, rec); err != nil {
		t.Fatal(err)
	}
	res, err := e.dbn.Reconcile(e.ctx, rec.Metadata.Name)
	if err != nil || res.RequeueAfter != 5*time.Minute {
		t.Fatalf("before Until: %+v, %v", res, err)
	}
	e.clk.Advance(5 * time.Minute)
	if _, err := e.dbn.Reconcile(e.ctx, rec.Metadata.Name); err != nil {
		t.Fatal(err)
	}
	got := e.getWorkItem(wi.Metadata.Name)
	if got.Status.State["announced"] != true {
		t.Errorf("state = %v", got.Status.State)
	}
	if d := got.Status.Decisions[len(got.Status.Decisions)-1]; d.Rule != "r" || len(d.Actions) != 2 || !strings.HasPrefix(d.Reason, "debounced until") {
		t.Errorf("decision = %+v", d)
	}
	if n := len(e.applies(v1alpha1.OpComment)); n != 1 {
		t.Errorf("comments = %d, want 1", n)
	}
	if err := e.st.Get(e.ctx, store.KeyOf(rec), &DebounceRecord{}); !isNotFoundErr(err) {
		t.Errorf("debounce record still there: %v", err)
	}
}

func TestDecisionsAreBounded(t *testing.T) {
	wi := &v1alpha1.WorkItem{}
	for i := range v1alpha1.MaxDecisions + 30 {
		appendDecisions(wi, []v1alpha1.Decision{{Event: fmt.Sprint(i)}})
	}
	ds := wi.Status.Decisions
	if len(ds) != v1alpha1.MaxDecisions || ds[0].Event != "30" || ds[len(ds)-1].Event != fmt.Sprint(v1alpha1.MaxDecisions+29) {
		t.Fatalf("kept %d decisions, %s…%s", len(ds), ds[0].Event, ds[len(ds)-1].Event)
	}

	// Through the controller: one event with many candidates stays bounded too.
	e := newEnv(t)
	w, _ := e.startPlan()
	stored := e.getWorkItem(w.Metadata.Name)
	stored.Status.Decisions = make([]v1alpha1.Decision, v1alpha1.MaxDecisions)
	if err := e.st.Update(e.ctx, stored); err != nil {
		t.Fatal(err)
	}
	e.deliver(fake.Webhook{Type: v1alpha1.EventIssueCommented, Sender: fake.Sender{Login: maintainer}, Object: &v1alpha1.ObjectRef{Kind: provider.KindIssue, Ref: docsRepo + "#42"}})
	e.deliver(issueOpened(docsRepo+"#42", maintainer))
	e.processEvents()
	if n := len(e.getWorkItem(w.Metadata.Name).Status.Decisions); n != v1alpha1.MaxDecisions {
		t.Errorf("decisions = %d, want %d", n, v1alpha1.MaxDecisions)
	}
}
