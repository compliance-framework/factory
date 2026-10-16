package controllers

import (
	"reflect"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/fake"
	"github.com/compliance-framework/factory/internal/store"
)

func TestRestartRebuildsFromProjections(t *testing.T) {
	e := newEnv(t)
	wi, plan := e.startPlan()
	e.completePlan(plan.Metadata.ID)
	// The implement task opened a PR the factory linked (as the Effector will): link it.
	before := e.getWorkItem(wi.Metadata.Name)
	e.seed(provider.KindPR, docsRepo+"#57", nil)
	before.Spec.Links = append(before.Spec.Links, v1alpha1.Link{Provider: ghProvider, Ref: docsRepo + "#57", Kind: provider.KindPR})
	if err := e.st.Update(e.ctx, before); err != nil {
		t.Fatal(err)
	}
	if _, err := e.wic.Reconcile(e.ctx, wi.Metadata.Name); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{docsRepo + "#42", docsRepo + "#57"} {
		if o, _ := e.gh.Object(ref); len(o.Projections) != 1 {
			t.Fatalf("%s projections = %+v", ref, o.Projections)
		}
	}
	// The issue closes once the PR exists; the marker on it still lists the item.
	if err := e.gh.Update(docsRepo+"#42", func(o *provider.Observation) { o.State = provider.StateClosed }); err != nil {
		t.Fatal(err)
	}

	// Restart: a new, empty store; only GitHub remembers.
	e.start()
	res, err := e.wic.Rebuild(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 || res.Created[0] != wi.Metadata.Name || res.Objects != 2 {
		t.Fatalf("rebuild = %+v", res)
	}
	after := e.getWorkItem(wi.Metadata.Name)
	if !reflect.DeepEqual(after.Spec.Links, before.Spec.Links) {
		t.Errorf("links = %+v, want %+v", after.Spec.Links, before.Spec.Links)
	}
	if !reflect.DeepEqual(jsonValue(after.Status.State), jsonValue(before.Status.State)) {
		t.Errorf("state = %v, want %v", after.Status.State, before.Status.State)
	}
	if !reflect.DeepEqual(after.Status.Counters, before.Status.Counters) || after.Status.Counters["plan-epic"] != 1 {
		t.Errorf("counters = %v, want %v", after.Status.Counters, before.Status.Counters)
	}
	// A second rebuild finds the item stored.
	if res, err := e.wic.Rebuild(e.ctx); err != nil || len(res.Created) != 0 || res.Existing != 1 {
		t.Errorf("second rebuild = %+v, %v", res, err)
	}

	// Rules continue from the rebuilt state: an event on the PR finds the rebuilt item.
	e.deliver(fake.Webhook{Type: v1alpha1.EventPRLabeled, Sender: fake.Sender{Login: maintainer}, Object: &v1alpha1.ObjectRef{Kind: provider.KindPR, Ref: docsRepo + "#57"}})
	e.processEvents()
	var wis []*v1alpha1.WorkItem
	if _, err := e.st.List(e.ctx, v1alpha1.KindWorkItem, store.ListOptions{}, &wis); err != nil || len(wis) != 1 {
		t.Fatalf("work items after the event = %d (%v), want the rebuilt one", len(wis), err)
	}
}

func TestProjectionOnlyWhenChanged(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	for range 2 {
		if _, err := e.wic.Reconcile(e.ctx, wi.Metadata.Name); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.gh.Projections()); n != 1 {
		t.Fatalf("Project calls = %d, want 1 (unchanged the second time)", n)
	}
	got := e.gh.Projections()[0]
	if got.Projection.WorkItemID != wi.Metadata.Name || got.Projection.State["plan"] != "planning" || got.Ref.Ref != docsRepo+"#42" {
		t.Errorf("projection = %+v", got)
	}
}

func TestResyncEmitsResynced(t *testing.T) {
	e := newEnv(t)
	wi, _ := e.startPlan()
	id := wi.Metadata.Name
	res, err := e.wic.Reconcile(e.ctx, id) // first observation: the baseline
	if err != nil || res.RequeueAfter != DefaultResyncInterval {
		t.Fatalf("Reconcile = %+v, %v", res, err)
	}
	if n := len(e.events(v1alpha1.EventFactoryWorkItemResynced)); n != 0 {
		t.Fatalf("resynced events after the baseline = %d", n)
	}
	// A human labels the issue out of band; nothing tells the dispatcher.
	if err := e.gh.Update(docsRepo+"#42", func(o *provider.Observation) { o.Labels = append(o.Labels, "help-wanted") }); err != nil {
		t.Fatal(err)
	}
	// Not due yet: no observation.
	if res, _ := e.wic.Reconcile(e.ctx, id); res.RequeueAfter != DefaultResyncInterval {
		t.Fatalf("early reconcile = %+v", res)
	}
	e.clk.Advance(DefaultResyncInterval)
	if _, err := e.wic.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	evs := e.events(v1alpha1.EventFactoryWorkItemResynced)
	if len(evs) != 1 || evs[0].WorkItem != id || evs[0].Source != v1alpha1.FactoryProvider {
		t.Fatalf("resynced events = %+v", evs)
	}
	// The cache was refreshed for the rule controller; the next resync sees no change.
	e.clk.Advance(DefaultResyncInterval)
	if _, err := e.wic.Reconcile(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if n := len(e.events(v1alpha1.EventFactoryWorkItemResynced)); n != 1 {
		t.Errorf("resynced events = %d, want still 1", n)
	}
	if n := e.processEvents(); n != 1 {
		t.Errorf("processed %d events, want the resynced one", n)
	}
}

func TestWorkItemRetention(t *testing.T) {
	e := newEnv(t)
	mk := func(ref string, state provider.ObjectState) string {
		e.seed(provider.KindIssue, ref, func(o *provider.Observation) { o.State = state })
		wi := &v1alpha1.WorkItem{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem},
			Metadata: v1alpha1.ObjectMeta{Name: objectID("wi", ref)},
			Spec:     v1alpha1.WorkItemSpec{Links: []v1alpha1.Link{{Provider: ghProvider, Ref: ref, Kind: provider.KindIssue, Primary: true}}},
		}
		if err := e.st.Create(e.ctx, wi); err != nil {
			t.Fatal(err)
		}
		return wi.Metadata.Name
	}
	closed := mk(docsRepo+"#1", provider.StateClosed)
	open := mk(docsRepo+"#2", provider.StateOpen)
	recent := mk(docsRepo+"#3", provider.StateClosed)
	// recent had activity a day ago.
	e.clk.Advance(store.DefaultRetention - 24*time.Hour)
	r := e.getWorkItem(recent)
	appendDecisions(r, []v1alpha1.Decision{{At: e.clk.Now(), Event: "x"}})
	if err := e.st.Update(e.ctx, r); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(24 * time.Hour)

	for _, id := range []string{closed, open, recent} {
		if _, err := e.wic.Reconcile(e.ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(id string) bool {
		return !isNotFoundErr(e.st.Get(e.ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: id}, &v1alpha1.WorkItem{}))
	}
	if exists(closed) {
		t.Error("the closed, idle item was kept")
	}
	if !exists(open) {
		t.Error("the open item was deleted")
	}
	if !exists(recent) {
		t.Error("the recently active item was deleted")
	}
}
