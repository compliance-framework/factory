package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/fake"
	"github.com/compliance-framework/factory/internal/store"
)

// leased is an env with one agent holding the lease of task tsk_1 (after mut).
func leased(t *testing.T, mut func(*v1alpha1.Task)) (*env, string, *apiclient.Lease) {
	t.Helper()
	e := newEnv(t)
	agent, tok := e.register("gus", "mac1")
	e.task("tsk_1", mut)
	return e, tok, e.lease(agent, tok)
}

func (e *env) effect(tok string, req apiclient.EffectRequest) *apiclient.CreateEffectResponse {
	e.t.Helper()
	r, err := e.c.CreateEffectWithResponse(e.ctx, "tsk_1", req, bearer(tok))
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func TestEffectAllowList(t *testing.T) {
	e, tok, _ := leased(t, nil)
	tests := []struct {
		name   string
		req    apiclient.EffectRequest
		status int
		code   string
	}{
		{"allowed comment", apiclient.EffectRequest{Op: v1alpha1.OpComment, Args: map[string]any{"body": "hi"}, IdempotencyKey: "a"}, 200, ""},
		{"op not allowed", apiclient.EffectRequest{Op: v1alpha1.OpPRMerge, IdempotencyKey: "b"}, 403, CodeEffectNotAllowed},
		{"unknown op", apiclient.EffectRequest{Op: "rm-rf", IdempotencyKey: "c"}, 400, CodeInvalid},
		{"label in the globs", apiclient.EffectRequest{Op: v1alpha1.OpLabelAdd, Args: map[string]any{"labels": []any{"triage:bug", "docs"}}, IdempotencyKey: "d"}, 200, ""},
		{"label outside the globs", apiclient.EffectRequest{Op: v1alpha1.OpLabelAdd, Args: map[string]any{"labels": []any{"triage:bug", "wontfix"}}, IdempotencyKey: "e"}, 403, CodeLabelNotAllowed},
		{"one label outside the globs", apiclient.EffectRequest{Op: v1alpha1.OpLabelAdd, Args: map[string]any{"label": "factory:stop"}, IdempotencyKey: "f"}, 403, CodeLabelNotAllowed},
		{"label op without labels", apiclient.EffectRequest{Op: v1alpha1.OpLabelAdd, IdempotencyKey: "g"}, 400, CodeInvalid},
		{"another repo", apiclient.EffectRequest{Op: v1alpha1.OpComment, Target: ptr("compliance-framework/api#1"), IdempotencyKey: "h"}, 403, CodeTargetNotAllowed},
		{"another org's lookalike", apiclient.EffectRequest{Op: v1alpha1.OpComment, Target: ptr("compliance-framework/docs-fork#42"), IdempotencyKey: "i"}, 403, CodeTargetNotAllowed},
		{"no idempotency key", apiclient.EffectRequest{Op: v1alpha1.OpComment}, 400, CodeIdempotencyKeyRequired},
		{"a key over 200 characters", apiclient.EffectRequest{Op: v1alpha1.OpComment, IdempotencyKey: strings.Repeat("k", 201)}, 400, CodeIdempotencyKeyRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := e.effect(tok, tt.req)
			if tt.status == 200 {
				if r.JSON200 == nil {
					t.Fatalf("status %s: %s", r.Status(), r.Body)
				}
				return
			}
			wantProblem(t, tt.name, r.HTTPResponse, r.Body, tt.status, tt.code)
		})
	}
	// Only the two allowed effects reached the provider.
	if n := len(e.gh.Applies()); n != 2 {
		t.Errorf("%d applies, want 2", n)
	}
	if obj, _ := e.gh.Object(docsIssue); !strings.Contains(strings.Join(obj.Labels, ","), "triage:bug") {
		t.Errorf("labels %v", obj.Labels)
	}
}

func TestEffectTargets(t *testing.T) {
	e, tok, _ := leased(t, nil)
	// A linked object in another repo is allowed (the work item's links).
	var wi v1alpha1.WorkItem
	if err := e.st.Get(e.ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: workItemID}, &wi); err != nil {
		t.Fatal(err)
	}
	wi.Spec.Links = append(wi.Spec.Links, v1alpha1.Link{Provider: ghProvider, Ref: "compliance-framework/api#7", Kind: provider.KindPR})
	if err := e.st.Update(e.ctx, &wi); err != nil {
		t.Fatal(err)
	}
	if err := e.gh.Seed(provider.Observation{Ref: v1alpha1.ObjectRef{Kind: provider.KindPR, Ref: "compliance-framework/api#7"}}); err != nil {
		t.Fatal(err)
	}
	if r := e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpComment, Target: ptr("compliance-framework/api#7"), IdempotencyKey: "linked"}); r.JSON200 == nil {
		t.Fatalf("a linked object: %s %s", r.Status(), r.Body)
	}
	// Another issue of the task's repo is allowed; pr.open targets the repo.
	if err := e.gh.Seed(provider.Observation{Ref: v1alpha1.ObjectRef{Kind: provider.KindIssue, Ref: "compliance-framework/docs#43"}}); err != nil {
		t.Fatal(err)
	}
	if r := e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpComment, Target: ptr("compliance-framework/docs#43"), IdempotencyKey: "same-repo"}); r.JSON200 == nil {
		t.Fatalf("same repo: %s %s", r.Status(), r.Body)
	}
	r := e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpPROpen, Args: map[string]any{"head": "fix", "base": "main"}, IdempotencyKey: "pr"})
	if r.JSON200 == nil || r.JSON200.Url == nil {
		t.Fatalf("pr.open: %s %s", r.Status(), r.Body)
	}
	applies := e.gh.Applies()
	last := applies[len(applies)-1].Request
	if last.Target.Ref != docsRepo || last.Identity != "author" || !strings.HasPrefix(last.IdempotencyKey, "eff_") {
		t.Errorf("pr.open request %+v", last)
	}
}

func TestEffectIdempotency(t *testing.T) {
	e, tok, l := leased(t, nil)
	req := apiclient.EffectRequest{Op: v1alpha1.OpComment, Args: map[string]any{"body": "hi"}, IdempotencyKey: "tsk_1/comment/1"}
	first := e.effect(tok, req)
	second := e.effect(tok, req)
	if first.JSON200 == nil || second.JSON200 == nil || first.JSON200.EffectId != second.JSON200.EffectId ||
		first.JSON200.Url == nil || second.JSON200.Url == nil || *first.JSON200.Url != *second.JSON200.Url {
		t.Fatalf("replay: %s / %s", first.Body, second.Body)
	}
	if n := len(e.gh.Applies()); n != 1 {
		t.Errorf("a replayed key reached the provider: %d applies", n)
	}
	var effects []v1alpha1.Effect
	if _, err := e.st.List(e.ctx, v1alpha1.KindEffect, store.ListOptions{}, &effects); err != nil || len(effects) != 1 {
		t.Fatalf("%d effects stored (%v)", len(effects), err)
	}
	if eff := effects[0]; eff.Spec.Task != "tsk_1" || eff.Metadata.Labels[LabelEffectRun] != *l.Lease.Run || eff.Status.Result.URL == "" {
		t.Errorf("effect %+v", eff)
	}
	// The same key on another task is another effect.
	if effectID("tsk_1", "k") == effectID("tsk_2", "k") {
		t.Error("keys are not scoped by task")
	}
}

func TestEffectGitSide(t *testing.T) {
	e, tok, l := leased(t, nil)
	r := e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpPushBranch, Args: map[string]any{"ref": "factory/tsk_1", "sha": "abc"}, IdempotencyKey: "push-1"})
	if r.JSON200 == nil || r.JSON200.Url != nil {
		t.Fatalf("push-branch: %s %s", r.Status(), r.Body)
	}
	if n := len(e.gh.Applies()); n != 0 {
		t.Errorf("a git-side op reached the provider: %d applies", n)
	}
	var eff v1alpha1.Effect
	if err := e.st.Get(e.ctx, store.Key{Kind: v1alpha1.KindEffect, Name: r.JSON200.EffectId}, &eff); err != nil {
		t.Fatal(err)
	}
	if eff.Spec.Op != v1alpha1.OpPushBranch || eff.Spec.Target.Ref != docsRepo || eff.Spec.Request["ref"] != "factory/tsk_1" {
		t.Errorf("recorded %+v", eff.Spec)
	}
	run, err := e.c.GetRunWithResponse(e.ctx, *l.Lease.Run, bearer(cliToken))
	if err != nil {
		t.Fatal(err)
	}
	p := run.JSON200.Run.Status.Progress
	if len(p) != 1 || p[0].Kind != "effect" || !strings.HasPrefix(p[0].Message, "push-branch ") {
		t.Errorf("run progress %+v", p)
	}
}

func TestEffectProviderErrors(t *testing.T) {
	e, tok, _ := leased(t, nil)
	e.gh.FailNext(fake.MethodApply, &provider.RetryableError{RetryAfter: 30 * time.Second, Err: fmt.Errorf("secondary rate limit")})
	r := e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpComment, IdempotencyKey: "k1"})
	wantProblem(t, "a retryable provider error", r.HTTPResponse, r.Body, http.StatusTooManyRequests, CodeProviderRetryable)
	if ra := r.HTTPResponse.Header.Get("Retry-After"); ra != "30" {
		t.Errorf("Retry-After %q", ra)
	}
	// Nothing was recorded: the retry with the same key applies.
	if r := e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpComment, IdempotencyKey: "k1"}); r.JSON200 == nil {
		t.Fatalf("retry: %s %s", r.Status(), r.Body)
	}
	e.gh.FailNext(fake.MethodApply, fmt.Errorf("boom: %w", provider.ErrNotFound))
	r = e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpComment, IdempotencyKey: "k2"})
	wantProblem(t, "a missing target", r.HTTPResponse, r.Body, http.StatusNotFound, CodeNotFound)
	e.gh.FailNext(fake.MethodApply, fmt.Errorf("boom"))
	r = e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpComment, IdempotencyKey: "k3"})
	wantProblem(t, "a provider failure", r.HTTPResponse, r.Body, http.StatusBadGateway, CodeProviderError)
}

func TestEffectRateLimits(t *testing.T) {
	e, tok, _ := leased(t, nil)
	for i := range MaxCommentsPerRun {
		if r := e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpComment, IdempotencyKey: fmt.Sprintf("c%d", i)}); r.JSON200 == nil {
			t.Fatalf("comment %d: %s %s", i, r.Status(), r.Body)
		}
	}
	r := e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpComment, IdempotencyKey: "one-too-many"})
	wantProblem(t, "the 21st comment of the run", r.HTTPResponse, r.Body, http.StatusTooManyRequests, CodeRateLimited)
	// A replay of a known key is still answered.
	if r := e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpComment, IdempotencyKey: "c0"}); r.JSON200 == nil {
		t.Fatalf("replay over the limit: %s", r.Status())
	}
	// Other effects go on, up to 60 per run.
	for i := MaxCommentsPerRun; i < MaxEffectsPerRun; i++ {
		if r := e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpPushBranch, IdempotencyKey: fmt.Sprintf("p%d", i)}); r.JSON200 == nil {
			t.Fatalf("effect %d: %s %s", i, r.Status(), r.Body)
		}
	}
	r = e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpPushBranch, IdempotencyKey: "p-last"})
	wantProblem(t, "the 61st effect of the run", r.HTTPResponse, r.Body, http.StatusTooManyRequests, CodeRateLimited)
}

func TestEffectWorkItemHourlyLimit(t *testing.T) {
	e, tok, _ := leased(t, nil)
	// 30 comments on the work item in the last hour, by earlier runs.
	for i := range MaxCommentsPerWorkItemHour {
		err := e.st.Create(e.ctx, &v1alpha1.Effect{
			TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEffect},
			Metadata: v1alpha1.ObjectMeta{Name: fmt.Sprintf("eff_old%d", i), Labels: map[string]string{
				LabelEffectWorkItem: workItemID, LabelEffectRun: "run_old", LabelEffectTask: "tsk_old",
			}},
			Spec:   v1alpha1.EffectSpec{Task: "tsk_old", Op: v1alpha1.OpComment, IdempotencyKey: fmt.Sprintf("old%d", i)},
			Status: &v1alpha1.EffectStatus{At: t0.Add(-time.Duration(59-i) * time.Minute)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	r := e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpComment, IdempotencyKey: "k"})
	wantProblem(t, "the 31st comment in an hour", r.HTTPResponse, r.Body, http.StatusTooManyRequests, CodeRateLimited)
	if ra := r.HTTPResponse.Header.Get("Retry-After"); ra != "60" {
		t.Errorf("Retry-After %q, want 60 (the oldest comment leaves the window in a minute)", ra)
	}
	// Non-comment effects aren't limited by it, and a minute later the comment goes through.
	if r := e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpPushBranch, IdempotencyKey: "p"}); r.JSON200 == nil {
		t.Fatalf("push: %s", r.Status())
	}
	e.clk.Advance(time.Minute)
	if r := e.effect(tok, apiclient.EffectRequest{Op: v1alpha1.OpComment, IdempotencyKey: "k"}); r.JSON200 == nil {
		t.Fatalf("a minute later: %s %s", r.Status(), r.Body)
	}
}
