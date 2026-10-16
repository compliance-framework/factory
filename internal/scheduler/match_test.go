package scheduler

import (
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// TestMatchingTable covers every row of the LLD's matching table, passing and failing. A
// passing case must also lease the task; a failing one must lease nothing and Explain the
// row's reason.
func TestMatchingTable(t *testing.T) {
	unlimitedLending := &v1alpha1.Lending{Unlimited: true, MaxConcurrent: 1}
	gus := func(e *env, l *v1alpha1.Lending, offers ...v1alpha1.HarnessOffer) string {
		id, _ := e.register("gus", "mbp", l, offers...)
		return id
	}
	// authored makes gus the agent of the work item's last author (implement) run.
	authored := func(e *env, agent string) {
		e.task("tsk_impl", "implement", func(t *v1alpha1.Task) {
			t.Status = &v1alpha1.TaskStatus{Phase: v1alpha1.TaskSucceeded, Agent: &agent}
		})
		if err := e.st.Create(e.ctx, &v1alpha1.Run{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindRun},
			Metadata: v1alpha1.ObjectMeta{Name: "run_impl-1"},
			Spec:     v1alpha1.RunSpec{Task: "tsk_impl", Attempt: 1, Agent: agent},
			Status:   &v1alpha1.RunStatus{StartedAt: t0.Add(-time.Hour), End: v1alpha1.RunCompleted},
		}); err != nil {
			e.t.Fatal(err)
		}
	}
	tests := []struct {
		name  string
		setup func(e *env) (task, agent string)
		want  string // "" leases the task
	}{
		// 1. The task's queue feeds the agent's pool.
		{"1 pool/same", func(e *env) (string, string) {
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, unlimitedLending)
		}, ""},
		{"1 pool/other", func(e *env) (string, string) {
			e.cat.Pools["remote"] = &v1alpha1.Pool{Metadata: v1alpha1.ObjectMeta{Name: "remote"}, Spec: v1alpha1.PoolSpec{Mode: v1alpha1.PoolRegistered}}
			e.cat.Queues["elsewhere"] = &v1alpha1.Queue{Metadata: v1alpha1.ObjectMeta{Name: "elsewhere"}, Spec: v1alpha1.QueueSpec{Pool: "remote"}}
			return e.task("tsk_a", "implement", func(t *v1alpha1.Task) { t.Spec.Queue = "elsewhere" }).Metadata.ID, gus(e, unlimitedLending)
		}, ReasonNotInPool},
		// 2. Queue concurrency (poc: 2).
		{"2 queue/room", func(e *env) (string, string) {
			e.task("tsk_r1", "implement", running("agt_bob-x"))
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, unlimitedLending)
		}, ""},
		{"2 queue/full", func(e *env) (string, string) {
			e.task("tsk_r1", "implement", running("agt_bob-x"))
			e.task("tsk_r2", "implement", running("agt_bob-y"))
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, unlimitedLending)
		}, ReasonQueueFull},
		// 3. Budgets: queue (poc: 40 runs a day) and RuleSet; runs, tokens, USD.
		{"3 budget/under", func(e *env) (string, string) {
			e.setCounter(ScopeQueue, pocQueue, v1alpha1.BudgetCounterStatus{Runs: 39})
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, unlimitedLending)
		}, ""},
		{"3 budget/queue runs", func(e *env) (string, string) {
			e.setCounter(ScopeQueue, pocQueue, v1alpha1.BudgetCounterStatus{Runs: 40})
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, unlimitedLending)
		}, ReasonBudget},
		{"3 budget/queue usd", func(e *env) (string, string) {
			e.cat.Queues[pocQueue].Spec.Budget = &v1alpha1.Budget{Daily: &v1alpha1.BudgetLimit{USD: 5}}
			e.setCounter(ScopeQueue, pocQueue, v1alpha1.BudgetCounterStatus{USD: 5})
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, unlimitedLending)
		}, ReasonBudget},
		{"3 budget/ruleset tokens", func(e *env) (string, string) {
			e.cat.RuleSets[pocRuleSet].Spec.Spec.Budget = &v1alpha1.Budget{Daily: &v1alpha1.BudgetLimit{Tokens: 1000}}
			e.setCounter(ScopeRuleSet, pocRuleSet, v1alpha1.BudgetCounterStatus{Tokens: 1000})
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, unlimitedLending)
		}, ReasonBudget},
		// 4. The agent: phase, throttling, capacity.
		{"4 agent/throttled", func(e *env) (string, string) {
			a := gus(e, unlimitedLending)
			if _, err := e.s.UpdateAgent(e.ctx, a, AgentUpdate{Phase: v1alpha1.AgentThrottled, Until: t0.Add(time.Hour)}); err != nil {
				e.t.Fatal(err)
			}
			return e.task("tsk_a", "implement", nil).Metadata.ID, a
		}, ReasonAgentThrottled},
		{"4 agent/throttle passed", func(e *env) (string, string) {
			a := gus(e, unlimitedLending)
			if _, err := e.s.UpdateAgent(e.ctx, a, AgentUpdate{Phase: v1alpha1.AgentThrottled, Until: t0.Add(time.Minute)}); err != nil {
				e.t.Fatal(err)
			}
			e.clk.Advance(2 * time.Minute)
			return e.task("tsk_a", "implement", nil).Metadata.ID, a
		}, ""},
		{"4 agent/at capacity", func(e *env) (string, string) {
			a := gus(e, unlimitedLending)
			e.task("tsk_r1", "implement", running(a))
			return e.task("tsk_a", "implement", nil).Metadata.ID, a
		}, ReasonAgentBusy},
		{"4 agent/draining", func(e *env) (string, string) {
			a := gus(e, unlimitedLending)
			if _, err := e.s.UpdateAgent(e.ctx, a, AgentUpdate{Phase: v1alpha1.AgentDraining}); err != nil {
				e.t.Fatal(err)
			}
			return e.task("tsk_a", "implement", nil).Metadata.ID, a
		}, ReasonAgentBusy},
		// 5. Lending.
		{"5 lending/unlimited", func(e *env) (string, string) {
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, &v1alpha1.Lending{Unlimited: true})
		}, ""},
		{"5 lending/none", func(e *env) (string, string) {
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, nil)
		}, ReasonLending},
		{"5 lending/queue lent", func(e *env) (string, string) {
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, lendQueues(pocQueue))
		}, ""},
		{"5 lending/queue not lent", func(e *env) (string, string) {
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, lendQueues("validation"))
		}, ReasonLending},
		{"5 lending/ruleset lent", func(e *env) (string, string) {
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, &v1alpha1.Lending{Queues: []string{pocQueue}, RuleSets: []string{pocRuleSet}})
		}, ""},
		{"5 lending/ruleset not lent", func(e *env) (string, string) {
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, &v1alpha1.Lending{Queues: []string{pocQueue}, RuleSets: []string{"docs"}})
		}, ReasonLending},
		{"5 lending/harness not lent", func(e *env) (string, string) {
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, &v1alpha1.Lending{Queues: []string{pocQueue}, Harnesses: []string{"claude-code"}})
		}, ReasonLending},
		{"5 lending/inside hours", func(e *env) (string, string) {
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, &v1alpha1.Lending{Queues: []string{pocQueue}, Hours: "Mon-Fri 09:00-18:00 Europe/London"})
		}, ""},
		{"5 lending/outside hours", func(e *env) (string, string) {
			return e.task("tsk_a", "implement", nil).Metadata.ID, gus(e, &v1alpha1.Lending{Queues: []string{pocQueue}, Hours: "Mon-Fri 18:00-20:00 Europe/Lisbon"})
		}, ReasonLending},
		{"5 lending/runs per day left", func(e *env) (string, string) {
			a := gus(e, &v1alpha1.Lending{Queues: []string{pocQueue}, MaxRunsPerDay: 10})
			e.setCounter(ScopeAgent, a, v1alpha1.BudgetCounterStatus{Runs: 9})
			return e.task("tsk_a", "implement", nil).Metadata.ID, a
		}, ""},
		{"5 lending/runs per day spent", func(e *env) (string, string) {
			a := gus(e, &v1alpha1.Lending{Queues: []string{pocQueue}, MaxRunsPerDay: 10})
			e.setCounter(ScopeAgent, a, v1alpha1.BudgetCounterStatus{Runs: 10})
			return e.task("tsk_a", "implement", nil).Metadata.ID, a
		}, ReasonLending},
		{"5 lending/tokens per day spent", func(e *env) (string, string) {
			a := gus(e, &v1alpha1.Lending{Queues: []string{pocQueue}, MaxTokensPerDay: 2_000_000})
			e.setCounter(ScopeAgent, a, v1alpha1.BudgetCounterStatus{Tokens: 2_000_000})
			return e.task("tsk_a", "implement", nil).Metadata.ID, a
		}, ReasonLending},
		// 6. requires.labels ⊆ agent labels.
		{"6 labels/present", func(e *env) (string, string) {
			return e.task("tsk_a", "implement", func(t *v1alpha1.Task) {
				t.Spec.Requires = &v1alpha1.Requires{Labels: []string{"macos"}}
			}).Metadata.ID, gus(e, unlimitedLending)
		}, ""},
		{"6 labels/missing", func(e *env) (string, string) {
			return e.task("tsk_a", "implement", func(t *v1alpha1.Task) {
				t.Spec.Requires = &v1alpha1.Requires{Labels: []string{"macos", "gif-tooling"}}
			}).Metadata.ID, gus(e, unlimitedLending)
		}, ReasonNoCapableAgent},
		// 7. Harness resolution through patterns.
		{"7 harness/resolves", func(e *env) (string, string) {
			return e.task("tsk_a", "docs-review", nil).Metadata.ID, gus(e, unlimitedLending)
		}, ""},
		{"7 harness/not offered", func(e *env) (string, string) {
			return e.task("tsk_a", "docs-review", nil).Metadata.ID, gus(e, unlimitedLending, codexOffer)
		}, ReasonNoCapableAgent},
		{"7 harness/request outside the offer", func(e *env) (string, string) {
			return e.task("tsk_a", "implement", func(t *v1alpha1.Task) {
				t.Spec.Harness = &v1alpha1.TaskHarness{Request: &v1alpha1.HarnessRequest{
					Name: "codex", Config: &v1alpha1.HarnessConfig{Model: v1alpha1.Scalar("gpt-6*")},
				}}
			}).Metadata.ID, gus(e, unlimitedLending)
		}, ReasonNoCapableAgent},
		// 8. Anti-affinity: a reviewer task skips its author's agent while another can take it.
		{"8 anti-affinity/author's agent", func(e *env) (string, string) {
			a := gus(e, unlimitedLending)
			e.register("ana", "box", unlimitedLending)
			authored(e, a)
			return e.task("tsk_a", "docs-review", nil).Metadata.ID, a
		}, ReasonAntiAffinity},
		{"8 anti-affinity/another agent", func(e *env) (string, string) {
			a := gus(e, unlimitedLending)
			b, _ := e.register("ana", "box", unlimitedLending)
			authored(e, a)
			return e.task("tsk_a", "docs-review", nil).Metadata.ID, b
		}, ""},
		// 9. One has-local-stack task per agent.
		{"9 local stack/none running", func(e *env) (string, string) {
			e.cat.Pools[pocPool].Spec.Lease.MaxTasksPerAgent = 2
			a, _ := e.registerWith("gus", "mbp", func(r *RegisterRequest) {
				r.Lending = &v1alpha1.Lending{Unlimited: true, MaxConcurrent: 2}
				r.Capabilities.Labels = []string{LabelLocalStack}
			})
			e.task("tsk_r1", "implement", running(a))
			return e.task("tsk_a", "implement", func(t *v1alpha1.Task) {
				t.Spec.Requires = &v1alpha1.Requires{Labels: []string{LabelLocalStack}}
			}).Metadata.ID, a
		}, ""},
		{"9 local stack/another running", func(e *env) (string, string) {
			e.cat.Pools[pocPool].Spec.Lease.MaxTasksPerAgent = 2
			a, _ := e.registerWith("gus", "mbp", func(r *RegisterRequest) {
				r.Lending = &v1alpha1.Lending{Unlimited: true, MaxConcurrent: 2}
				r.Capabilities.Labels = []string{LabelLocalStack}
			})
			local := func(t *v1alpha1.Task) { t.Spec.Requires = &v1alpha1.Requires{Labels: []string{LabelLocalStack}} }
			e.task("tsk_r1", "implement", func(t *v1alpha1.Task) { running(a)(t); local(t) })
			return e.task("tsk_a", "implement", local).Metadata.ID, a
		}, ReasonAgentBusy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			task, agent := tt.setup(e)
			if got := e.explain(task, agent); got != tt.want {
				t.Fatalf("Explain = %q, want %q", got, tt.want)
			}
			g := e.lease(agent)
			if tt.want != "" {
				if g != nil {
					t.Fatalf("leased %s, want nothing (%s)", grantTask(g), tt.want)
				}
				return
			}
			if grantTask(g) != task {
				t.Fatalf("leased %s, want %s", grantTask(g), task)
			}
			got := e.getTask(task)
			if got.Status.Phase != v1alpha1.TaskLeased || got.Status.Agent == nil || *got.Status.Agent != agent {
				t.Fatalf("task status = %+v, want Leased by %s", got.Status, agent)
			}
			if got.Spec.Harness == nil || got.Spec.Harness.Resolved == nil {
				t.Fatal("spec.harness.resolved not written")
			}
		})
	}
}

func TestHarnessResolvedWritten(t *testing.T) {
	e := newEnv(t)
	a, _ := e.register("gus", "mbp", &v1alpha1.Lending{Unlimited: true})
	e.task("tsk_review", "docs-review", nil)
	g := e.mustLease(a, "tsk_review")
	want := v1alpha1.HarnessResolved{Name: "claude-code", Auth: "apiKey", Model: "claude-opus-5-5", ReasoningEffort: "high"}
	if got := *e.getTask("tsk_review").Spec.Harness.Resolved; got != want {
		t.Fatalf("stored resolved = %+v, want %+v", got, want)
	}
	if got := *g.Task.Spec.Harness.Resolved; got != want {
		t.Fatalf("granted resolved = %+v, want %+v", got, want)
	}
}

func TestTaskOrder(t *testing.T) {
	e := newEnv(t)
	e.cat.Queues["urgent"] = &v1alpha1.Queue{Metadata: v1alpha1.ObjectMeta{Name: "urgent"}, Spec: v1alpha1.QueueSpec{Pool: pocPool, Priority: 90}}
	e.cat.Pools[pocPool].Spec.Lease.MaxTasksPerAgent = 3
	a, _ := e.register("gus", "mbp", &v1alpha1.Lending{Unlimited: true, MaxConcurrent: 3})
	e.task("tsk_b", "implement", nil)
	e.clk.Advance(time.Second)
	e.task("tsk_a", "implement", nil) // created later, same queue
	e.task("tsk_c", "implement", func(t *v1alpha1.Task) { t.Spec.Queue = "urgent" })
	for _, want := range []string{"tsk_c", "tsk_b", "tsk_a"} {
		e.mustLease(a, want)
	}
}

// TestFairness: among the agents waiting, a limited agent is preferred to an unlimited one
// (prefer-limited, the default), then least-recently-leased first; prefer-unlimited flips it.
func TestFairness(t *testing.T) {
	park := func(e *env, agent string) <-chan *Grant {
		ch := make(chan *Grant, 1)
		before := e.s.parked.Load()
		go func() {
			g, err := e.s.Lease(e.ctx, agent, 30*time.Second)
			if err != nil {
				e.t.Error(err)
			}
			ch <- g
		}()
		eventually(e.t, agent+" parked", func() bool { return e.s.parked.Load() > before })
		return ch
	}

	t.Run("prefer-limited", func(t *testing.T) {
		e := newEnv(t)
		limited, _ := e.register("gus", "mbp", lendQueues(pocQueue))
		unlim, _ := e.register("ana", "box", &v1alpha1.Lending{Unlimited: true})
		ch := park(e, limited)
		e.task("tsk_a", "implement", nil) // no watch runs: nobody is woken yet
		if g := e.lease(unlim); g != nil {
			t.Fatalf("unlimited agent leased %s while a limited one waits", grantTask(g))
		}
		e.s.wake()
		if g := recv(t, ch, "the limited agent's lease"); grantTask(g) != "tsk_a" {
			t.Fatalf("limited agent got %s", grantTask(g))
		}
		// Nobody else waits: the unlimited agent takes the next one.
		e.task("tsk_b", "implement", nil)
		e.mustLease(unlim, "tsk_b")
	})

	t.Run("prefer-unlimited", func(t *testing.T) {
		e := newEnv(t)
		e.cat.Pools[pocPool].Spec.Fairness = FairnessPreferUnlimited
		limited, _ := e.register("gus", "mbp", lendQueues(pocQueue))
		unlim, _ := e.register("ana", "box", &v1alpha1.Lending{Unlimited: true})
		ch := park(e, limited)
		e.task("tsk_a", "implement", nil)
		e.mustLease(unlim, "tsk_a")
		e.clk.Advance(31 * time.Second)
		if g := recv(t, ch, "the limited agent's poll"); g != nil {
			t.Fatalf("limited agent got %s", grantTask(g))
		}
	})

	t.Run("least recently leased", func(t *testing.T) {
		e := newEnv(t)
		e.cat.Queues[pocQueue].Spec.Concurrency = 3
		first, _ := e.register("gus", "mbp", lendQueues(pocQueue))
		second, _ := e.register("ana", "box", lendQueues(pocQueue))
		e.task("tsk_a", "implement", nil)
		g := e.mustLease(first, "tsk_a")
		if _, err := e.s.Complete(e.ctx, first, g.Lease.ID, v1alpha1.Result{Status: v1alpha1.ResultDone, Outcome: "pr-opened"}); err != nil {
			t.Fatal(err)
		}
		e.clk.Advance(time.Minute)
		ch := park(e, second) // never leased: before first
		e.task("tsk_b", "implement", nil)
		if g := e.lease(first); g != nil {
			t.Fatalf("recently leased agent got %s", grantTask(g))
		}
		e.s.wake()
		if g := recv(t, ch, "the other agent's lease"); grantTask(g) != "tsk_b" {
			t.Fatalf("other agent got %s", grantTask(g))
		}
	})
}

// TestAntiAffinitySoftFallback: when the author's agent is the only one that can take the
// review, it gets it once the review has waited 10 minutes.
func TestAntiAffinitySoftFallback(t *testing.T) {
	e := newEnv(t)
	gus, _ := e.register("gus", "mbp", &v1alpha1.Lending{Unlimited: true})
	ana, _ := e.register("ana", "box", &v1alpha1.Lending{Unlimited: true}, codexOffer) // can't run claude-code
	e.task("tsk_impl", "implement", nil)
	g := e.mustLease(gus, "tsk_impl")
	if _, err := e.s.Complete(e.ctx, gus, g.Lease.ID, v1alpha1.Result{Status: v1alpha1.ResultDone, Outcome: "pr-opened"}); err != nil {
		t.Fatal(err)
	}
	e.task("tsk_review", "docs-review", nil)
	if got := e.explain("tsk_review", gus); got != ReasonAntiAffinity {
		t.Fatalf("Explain(gus) = %q, want anti-affinity", got)
	}
	if got := e.explain("tsk_review", ana); got != ReasonNoCapableAgent {
		t.Fatalf("Explain(ana) = %q, want no-capable-agent", got)
	}
	if err := e.s.Refresh(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.getTask("tsk_review").Status.Reason; got != ReasonAntiAffinity {
		t.Fatalf("reason = %q, want anti-affinity", got)
	}
	e.clk.Advance(9 * time.Minute)
	if g := e.lease(gus); g != nil {
		t.Fatalf("leased %s before the 10 minutes", grantTask(g))
	}
	e.clk.Advance(time.Minute)
	e.mustLease(gus, "tsk_review")
}

// TestRefreshRecordsReason: a pending task's reason is the furthest any agent got.
func TestRefreshRecordsReason(t *testing.T) {
	e := newEnv(t)
	e.task("tsk_a", "docs-review", nil)
	if err := e.s.Refresh(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.getTask("tsk_a").Status.Reason; got != ReasonNoCapableAgent {
		t.Fatalf("no agents: reason = %q, want no-capable-agent", got)
	}
	e.register("gus", "mbp", lendQueues("validation"))                       // row 5
	e.register("ana", "box", &v1alpha1.Lending{Unlimited: true}, codexOffer) // row 7
	if err := e.s.Refresh(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.getTask("tsk_a").Status.Reason; got != ReasonNoCapableAgent {
		t.Fatalf("reason = %q, want no-capable-agent (row 7 beats row 5)", got)
	}
}
