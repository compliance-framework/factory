package controllers

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/fake"
	"github.com/compliance-framework/factory/internal/store"
	"github.com/compliance-framework/factory/internal/store/memory"
)

// Names in the PoC catalog the tests use.
const (
	ghProvider = "github-public"
	docsRepo   = "compliance-framework/docs"
	maintainer = "gus"
	agentID    = "agt_gus-mbp"
)

// staticCatalog is a CatalogProvider over a fixed snapshot.
type staticCatalog struct{ c *catalog.Catalog }

func (s staticCatalog) Current() *catalog.Catalog { return s.c }

func loadPoC(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, errs := catalog.LoadDir(os.DirFS(pocCatalog), catalog.Options{})
	if c == nil || catalog.HasErrors(errs) {
		t.Fatalf("load PoC catalog: %v", errs)
	}
	return c
}

// env is a dispatcher in a test: the memory store, the PoC catalog, the fake GitHub provider
// (its Conn outlives a restart, as GitHub does) and the three controllers, on a fake clock.
type env struct {
	t       *testing.T
	ctx     context.Context
	clk     *clock.Fake
	st      *memory.Store
	cat     *catalog.Catalog
	gh      *fake.Conn
	conns   StaticConns
	factory *FactoryProvider
	cache   *ObservationCache
	rule    *RuleController
	dbn     *DebounceController
	run     *RunController
	wic     *WorkItemController
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	cat := loadPoC(t)
	groups := map[string]v1alpha1.GroupSpec{}
	for n, g := range cat.Groups {
		groups[n] = g.Spec
	}
	gh := fake.NewConn(*cat.Providers[ghProvider], fake.Options{Clock: clk, Groups: provider.StaticGroups(groups)})
	gh.SetOrgRole("compliance-framework", "admin", maintainer)
	e := &env{t: t, ctx: context.Background(), clk: clk, cat: cat, gh: gh}
	e.start()
	return e
}

// start (re)creates the store and the controllers: a restart keeps only the provider.
func (e *env) start() {
	e.t.Helper()
	e.st = memory.New(store.Options{Clock: e.clk, Logger: discardLogger()})
	e.conns = StaticConns{ghProvider: e.gh}
	e.factory = NewFactoryProvider(e.st, e.clk)
	e.cache = NewObservationCache(e.conns, e.clk, 0)
	cp := staticCatalog{e.cat}
	var err error
	e.rule, err = NewRuleController(e.st, cp, e.conns, RuleOptions{Clock: e.clk, Logger: discardLogger(), Cache: e.cache, UIBaseURL: "https://factory.test"})
	if err != nil {
		e.t.Fatal(err)
	}
	if e.dbn, err = NewDebounceController(e.rule); err != nil {
		e.t.Fatal(err)
	}
	if e.run, err = NewRunController(e.st, cp, e.factory, RunOptions{Clock: e.clk, Logger: discardLogger()}); err != nil {
		e.t.Fatal(err)
	}
	if e.wic, err = NewWorkItemController(e.st, cp, e.conns, e.factory, WorkItemOptions{Clock: e.clk, Logger: discardLogger(), Cache: e.cache}); err != nil {
		e.t.Fatal(err)
	}
}

// seedIssue adds an open issue (or PR) to the fake GitHub.
func (e *env) seed(kind, ref string, mut func(*provider.Observation)) {
	e.t.Helper()
	o := provider.Observation{Ref: v1alpha1.ObjectRef{Provider: ghProvider, Kind: kind, Ref: ref}}
	if kind == provider.KindPR {
		o.PR = &provider.PRObservation{Head: "abc"}
	}
	if mut != nil {
		mut(&o)
	}
	if err := e.gh.Seed(o); err != nil {
		e.t.Fatal(err)
	}
}

// deliver normalises a webhook with the fake and stores its Events, as the provider
// controller does.
func (e *env) deliver(w fake.Webhook) []v1alpha1.Event {
	e.t.Helper()
	raw, err := e.gh.RawEvent(w)
	if err != nil {
		e.t.Fatal(err)
	}
	evs, err := e.gh.Normalise(e.ctx, raw)
	if err != nil {
		e.t.Fatal(err)
	}
	for i := range evs {
		if err := e.st.Create(e.ctx, &evs[i]); err != nil {
			e.t.Fatal(err)
		}
	}
	return evs
}

func issueOpened(ref, login string) fake.Webhook {
	return fake.Webhook{Type: v1alpha1.EventIssueOpened, Sender: fake.Sender{Login: login},
		Object: &v1alpha1.ObjectRef{Kind: provider.KindIssue, Ref: ref}}
}

// processEvents reconciles every unprocessed event, in name order, until none is left (the
// rule controller without its goroutines). It returns how many it processed.
func (e *env) processEvents() int {
	e.t.Helper()
	n := 0
	for {
		var evs []*v1alpha1.Event
		if _, err := e.st.List(e.ctx, v1alpha1.KindEvent, store.ListOptions{}, &evs); err != nil {
			e.t.Fatal(err)
		}
		progressed := false
		for _, ev := range evs {
			if done, _ := e.rule.processed(e.ctx, ev.ID); done {
				continue
			}
			if _, err := e.rule.Reconcile(e.ctx, ev.ID); err != nil {
				e.t.Fatalf("reconcile event %s: %v", ev.ID, err)
			}
			n++
			progressed = true
		}
		if !progressed {
			return n
		}
	}
}

func (e *env) workItemOf(ref string) *v1alpha1.WorkItem {
	e.t.Helper()
	var wis []*v1alpha1.WorkItem
	if _, err := e.st.List(e.ctx, v1alpha1.KindWorkItem, store.ListOptions{
		Field: map[string]string{store.FieldWorkItemLink: store.LinkFieldValue(ghProvider, ref)},
	}, &wis); err != nil {
		e.t.Fatal(err)
	}
	if len(wis) != 1 {
		e.t.Fatalf("work items of %s: %d, want 1", ref, len(wis))
	}
	return wis[0]
}

func (e *env) getWorkItem(id string) *v1alpha1.WorkItem {
	e.t.Helper()
	var wi v1alpha1.WorkItem
	if err := e.st.Get(e.ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: id}, &wi); err != nil {
		e.t.Fatal(err)
	}
	return &wi
}

func (e *env) tasks(wi string) []*v1alpha1.Task {
	e.t.Helper()
	ts, err := listTasks(e.ctx, e.st, wi)
	if err != nil {
		e.t.Fatal(err)
	}
	return ts
}

// tasksOf returns the work item's tasks of a TaskSpec.
func (e *env) tasksOf(wi, spec string) []*v1alpha1.Task {
	var out []*v1alpha1.Task
	for _, t := range e.tasks(wi) {
		if t.Spec.TaskSpec.Name == spec {
			out = append(out, t)
		}
	}
	return out
}

func (e *env) task(id string) *v1alpha1.Task {
	e.t.Helper()
	t, err := e.run.getTask(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return t
}

// lease does what the scheduler does on a match: the task becomes Leased by agentID. Then
// the run controller hears of it.
func (e *env) lease(id string) {
	e.t.Helper()
	t := e.task(id)
	agent := agentID
	t.Status.Phase, t.Status.Agent = v1alpha1.TaskLeased, &agent
	t.Status.LeaseExpiresAt = e.clk.Now().Add(2 * time.Minute)
	if err := e.st.Update(e.ctx, t); err != nil {
		e.t.Fatal(err)
	}
	if err := e.run.LeaseIssued(e.ctx, id); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) runs(task string) []*v1alpha1.Run {
	e.t.Helper()
	rs, err := listRuns(e.ctx, e.st, task)
	if err != nil {
		e.t.Fatal(err)
	}
	return rs
}

// events returns the stored events of a type.
func (e *env) events(typ v1alpha1.EventType) []*v1alpha1.Event {
	e.t.Helper()
	var evs []*v1alpha1.Event
	if _, err := e.st.List(e.ctx, v1alpha1.KindEvent, store.ListOptions{}, &evs); err != nil {
		e.t.Fatal(err)
	}
	return slices.DeleteFunc(evs, func(ev *v1alpha1.Event) bool { return ev.Type != typ })
}

// applies returns the fake's Apply calls with an op.
func (e *env) applies(op v1alpha1.EffectOp) []fake.ApplyCall {
	return slices.DeleteFunc(e.gh.Applies(), func(a fake.ApplyCall) bool { return a.Request.Op != op })
}

func doneResult(outcome string, data map[string]any) v1alpha1.Result {
	return v1alpha1.Result{Status: v1alpha1.ResultDone, Outcome: outcome, Summary: "ok", Data: data,
		Harness: &v1alpha1.ResultHarness{Name: "codex", EffectiveConfig: v1alpha1.EffectiveConfig{Model: "gpt-5-codex"}}}
}

func isNotFoundErr(err error) bool { return errors.Is(err, store.ErrNotFound) }
