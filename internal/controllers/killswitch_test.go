package controllers

import (
	"errors"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/rules"
	"github.com/compliance-framework/factory/internal/store"
)

func TestStoreKillSwitches(t *testing.T) {
	e := newEnv(t)
	ks := StoreKillSwitches{Store: e.st}
	paused := func(scope, name string) bool {
		t.Helper()
		p, err := ks.Paused(e.ctx, scope, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if paused(ScopeQueue, "poc") {
		t.Fatal("paused before any pause")
	}
	for range 2 { // pausing twice is a no-op
		if err := ks.Pause(e.ctx, ScopeQueue, "poc", "github-public:gus", t0); err != nil {
			t.Fatal(err)
		}
	}
	if !paused(ScopeQueue, "poc") || paused(ScopeQueue, "other") || paused(ScopeGlobal, "") {
		t.Fatal("pause flags are wrong")
	}
	if err := ks.Pause(e.ctx, ScopeGlobal, "ignored", "x", t0); err != nil || !paused(ScopeGlobal, "") {
		t.Fatalf("global pause: %v", err)
	}
	for range 2 {
		if err := ks.Resume(e.ctx, ScopeQueue, "poc"); err != nil {
			t.Fatal(err)
		}
	}
	if paused(ScopeQueue, "poc") {
		t.Fatal("still paused after resume")
	}
	if err := ks.Pause(e.ctx, "everything", "x", "", t0); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("bad scope: %v", err)
	}
	if err := ks.Pause(e.ctx, ScopeQueue, "", "", t0); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("no name: %v", err)
	}
}

// TestRulesReadKillSwitches: the rule controller's default kill switches are the store's, so a
// paused queue suppresses its dispatches; DryRun reports the same without writing anything.
func TestRulesReadKillSwitches(t *testing.T) {
	e := newEnv(t)
	if err := (StoreKillSwitches{Store: e.st}).Pause(e.ctx, ScopeQueue, "poc", "github-public:gus", t0); err != nil {
		t.Fatal(err)
	}
	e.seed(provider.KindIssue, "compliance-framework/docs#1", nil)
	evs := e.deliver(issueOpened("compliance-framework/docs#1", maintainer))

	eval, err := e.rule.DryRun(e.ctx, evs[0], "")
	if err != nil {
		t.Fatal(err)
	}
	var verdict string
	for _, d := range eval.Decisions {
		if d.Action.Dispatch != nil {
			verdict = d.Rail.Verdict + " " + d.Rail.Reason
		}
	}
	if verdict != rules.VerdictSuppressed+" paused: queue poc" {
		t.Fatalf("dry run dispatch verdict %q", verdict)
	}
	var wis []*v1alpha1.WorkItem
	if _, err := e.st.List(e.ctx, v1alpha1.KindWorkItem, store.ListOptions{}, &wis); err != nil || len(wis) != 0 {
		t.Fatalf("the dry run wrote %d work items (%v)", len(wis), err)
	}
	if _, err := e.rule.DryRun(e.ctx, evs[0], "wi_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a missing work item: %v", err)
	}

	e.processEvents()
	wi := e.workItemOf("compliance-framework/docs#1")
	if n := len(e.tasks(wi.Metadata.Name)); n != 0 {
		t.Fatalf("%d tasks dispatched to a paused queue", n)
	}
}
