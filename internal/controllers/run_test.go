package controllers

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

func TestCompleteInvalidResult(t *testing.T) {
	tests := []struct {
		name string
		res  v1alpha1.Result
		want string // in the progress message
	}{
		{"unknown outcome", doneResult("bogus", nil), `outcome "bogus" is not one of`},
		{"done without outcome", doneResult("", nil), "a done result needs an outcome"},
		{"schema", doneResult("decomposed", map[string]any{"plan": []any{map[string]any{"issue": "x"}}}), "schema schemas/results/plan-epic.json"},
		{"another task", func() v1alpha1.Result { r := doneResult("decomposed", nil); r.Task = "tsk_other"; return r }(), "result is for task"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			wi, plan := e.startPlan()
			id := plan.Metadata.ID
			e.lease(id)
			phase, err := e.run.Complete(e.ctx, id, tt.res)
			if err != nil || phase != v1alpha1.TaskFailed {
				t.Fatalf("Complete = %s, %v; want Failed", phase, err)
			}
			if got := e.task(id).Status; got.Phase != v1alpha1.TaskFailed || got.Reason != ReasonInvalidResult {
				t.Errorf("task status = %+v", got)
			}
			runs := e.runs(id)
			if len(runs) != 1 || runs[0].Status.End != v1alpha1.RunFailed || runs[0].Status.Result != nil {
				t.Fatalf("run = %+v", runs)
			}
			p := runs[0].Status.Progress
			if len(p) == 0 || p[len(p)-1].Kind != ReasonInvalidResult || !strings.Contains(p[len(p)-1].Message, tt.want) {
				t.Errorf("progress = %+v, want %q", p, tt.want)
			}
			failed := e.events(v1alpha1.EventFactoryRunFailed)
			if len(failed) != 1 || runFact(failed[0], "reason") != ReasonInvalidResult || runFact(failed[0], "spec") != "plan-epic" ||
				runFact(failed[0], "outcome") != "" || failed[0].WorkItem != wi.Metadata.Name {
				t.Fatalf("run.failed = %+v", failed)
			}
			if n := e.getWorkItem(wi.Metadata.Name).Status.Counters["plan-epic"]; n != 1 {
				t.Errorf("counter = %d, want 1 (a failed run is a run)", n)
			}
			// Completing again is a replay.
			if phase, err := e.run.Complete(e.ctx, id, doneResult("decomposed", planData)); err != nil || phase != v1alpha1.TaskFailed {
				t.Errorf("replay = %s, %v", phase, err)
			}
			if n := len(e.events(v1alpha1.EventFactoryRunFailed)) + len(e.events(v1alpha1.EventFactoryRunCompleted)); n != 1 {
				t.Errorf("run events = %d, want 1", n)
			}
		})
	}
}

func TestCompleteEmitsRunFacts(t *testing.T) {
	e := newEnv(t)
	_, plan := e.startPlan()
	e.lease(plan.Metadata.ID)
	if _, err := e.run.Complete(e.ctx, plan.Metadata.ID, doneResult("decomposed", planData)); err != nil {
		t.Fatal(err)
	}
	evs := e.events(v1alpha1.EventFactoryRunCompleted)
	if len(evs) != 1 {
		t.Fatalf("run.completed = %d", len(evs))
	}
	ev := evs[0]
	if ev.Source != v1alpha1.FactoryProvider || ev.Data.Provider != v1alpha1.FactoryProvider {
		t.Errorf("source = %s/%s", ev.Source, ev.Data.Provider)
	}
	for k, want := range map[string]any{"spec": "plan-epic", "outcome": "decomposed", "status": "done", "agent": agentID} {
		if got := runFact(ev, k); got != want {
			t.Errorf("run.%s = %v, want %v", k, got, want)
		}
	}
	result, _ := runFact(ev, "result").(map[string]any)
	if plan, _ := result["plan"].([]any); len(plan) != 2 { // data.plan mirrored as run.result.plan
		t.Errorf("run.result = %v", result)
	}
	if h, _ := runFact(ev, "harness").(map[string]any); h["name"] != "codex" || h["model"] != "gpt-5-codex" {
		t.Errorf("run.harness = %v", h)
	}
	if _, ok := runFact(ev, "inputs").(map[string]any); !ok {
		t.Errorf("run.inputs = %v", runFact(ev, "inputs"))
	}
}

func TestCompleteFailedStatus(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	id := plan.Metadata.ID
	e.lease(id)
	res := doneResult("", nil)
	res.Status = v1alpha1.ResultFailed
	if phase, err := e.run.Complete(e.ctx, id, res); err != nil || phase != v1alpha1.TaskFailed {
		t.Fatalf("Complete = %s, %v; want Failed", phase, err)
	}
	// The agent reported: the run completed (with its Result), the task failed.
	runs := e.runs(id)
	if len(runs) != 1 || runs[0].Status.End != v1alpha1.RunCompleted || runs[0].Status.Result == nil || runs[0].Status.EndedAt.IsZero() {
		t.Fatalf("run = %+v", runs[0].Status)
	}
	failed := e.events(v1alpha1.EventFactoryRunFailed)
	if len(failed) != 1 || runFact(failed[0], "status") != "failed" || runFact(failed[0], "reason") != "failed" {
		t.Fatalf("run.failed = %+v", failed)
	}
	if n := e.getWorkItem(wi.Metadata.Name).Status.Counters["plan-epic"]; n != 1 {
		t.Errorf("counter = %d, want 1", n)
	}
}

func TestCompleteNeedsALease(t *testing.T) {
	e := newEnv(t)
	_, plan := e.startPlan()
	if _, err := e.run.Complete(e.ctx, plan.Metadata.ID, doneResult("decomposed", planData)); !errors.Is(err, ErrNotLeased) {
		t.Errorf("Complete of a Pending task = %v, want ErrNotLeased", err)
	}
	if _, err := e.run.Complete(e.ctx, "tsk_nope", doneResult("decomposed", planData)); !errors.Is(err, ErrUnknownTask) {
		t.Errorf("Complete of an unknown task = %v, want ErrUnknownTask", err)
	}
}

func TestWallClockTimeout(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	id := plan.Metadata.ID
	e.lease(id)
	// plan-epic: maxWallClock 20m, plus the 2 min grace.
	res, err := e.run.Reconcile(e.ctx, id)
	if err != nil || res.RequeueAfter != 22*time.Minute {
		t.Fatalf("Reconcile = %+v, %v; want a requeue at the deadline", res, err)
	}
	e.clk.Advance(21 * time.Minute)
	if res, _ := e.run.Reconcile(e.ctx, id); res.RequeueAfter != time.Minute || e.task(id).Status.Phase != v1alpha1.TaskLeased {
		t.Fatalf("before the deadline: %+v, %s", res, e.task(id).Status.Phase)
	}
	e.clk.Advance(time.Minute)
	if _, err := e.run.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := e.task(id).Status; got.Phase != v1alpha1.TaskFailed || got.Reason != ReasonTimeout {
		t.Errorf("task = %+v, want Failed{timeout}", got)
	}
	failed := e.events(v1alpha1.EventFactoryRunFailed)
	if len(failed) != 1 || runFact(failed[0], "reason") != ReasonTimeout || runFact(failed[0], "status") != "failed" {
		t.Fatalf("run.failed = %+v", failed)
	}
	if n := e.getWorkItem(wi.Metadata.Name).Status.Counters["plan-epic"]; n != 1 {
		t.Errorf("counter = %d, want 1", n)
	}
}

func TestAbandon(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	id := plan.Metadata.ID

	// Throttled: back to Pending without using an attempt; the run doesn't count.
	e.lease(id)
	if err := e.run.Abandon(e.ctx, id, Abandonment{Reason: AbandonThrottled, Until: e.clk.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	st := e.task(id).Status
	if st.Phase != v1alpha1.TaskPending || st.Attempts != 0 || st.Agent != nil || st.Reason != ReasonThrottled {
		t.Fatalf("after throttled abandon: %+v", st)
	}
	if runs := e.runs(id); len(runs) != 1 || runs[0].Status.End != v1alpha1.RunAbandoned {
		t.Fatalf("runs = %+v", runs)
	}
	if n := e.getWorkItem(wi.Metadata.Name).Status.Counters["plan-epic"]; n != 0 {
		t.Errorf("counter = %d, want 0", n)
	}

	// agent-error: the poc queue retries infra failures once (attempts 1), then fails.
	e.lease(id)
	if runs := e.runs(id); len(runs) != 2 || runs[1].Spec.Attempt != 1 {
		t.Fatalf("second lease runs = %+v", runs)
	}
	if err := e.run.Abandon(e.ctx, id, Abandonment{Reason: AbandonAgentError}); err != nil {
		t.Fatal(err)
	}
	if st := e.task(id).Status; st.Phase != v1alpha1.TaskPending || st.Attempts != 1 || st.Reason != string(v1alpha1.RetryInfraFailure) {
		t.Fatalf("after a retried abandon: %+v", st)
	}
	e.lease(id)
	if err := e.run.Abandon(e.ctx, id, Abandonment{Reason: AbandonAgentError}); err != nil {
		t.Fatal(err)
	}
	if st := e.task(id).Status; st.Phase != v1alpha1.TaskFailed || st.Reason != string(v1alpha1.RetryInfraFailure) {
		t.Fatalf("after the last abandon: %+v", st)
	}
	if n := len(e.events(v1alpha1.EventFactoryRunFailed)); n != 1 {
		t.Errorf("run.failed events = %d, want 1 (only the final failure)", n)
	}
	if err := e.run.Abandon(e.ctx, id, Abandonment{Reason: AbandonAgentError}); !errors.Is(err, ErrNotLeased) {
		t.Errorf("abandon of a failed task = %v, want ErrNotLeased", err)
	}
}

func TestRunClosedWhenLeaseLost(t *testing.T) {
	e := newEnv(t)
	_, plan := e.startPlan()
	id := plan.Metadata.ID
	e.lease(id)
	// The scheduler expires the lease: the task is Pending again.
	tk := e.task(id)
	tk.Status.Phase, tk.Status.Agent, tk.Status.Attempts = v1alpha1.TaskPending, nil, 1
	if err := e.st.Update(e.ctx, tk); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if runs := e.runs(id); len(runs) != 1 || runs[0].Status.End != v1alpha1.RunLost {
		t.Fatalf("runs = %+v", runs)
	}
	if n := len(e.events(v1alpha1.EventFactoryRunFailed)); n != 0 {
		t.Errorf("run.failed events = %d, want 0", n)
	}
}
