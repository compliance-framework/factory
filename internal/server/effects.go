package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/apiserver/gen"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/patterns"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/store"
)

// Effector limits (api-server.md §Effector API).
const (
	MaxIdempotencyKey          = 200
	MaxEffectsPerRun           = 60
	MaxCommentsPerRun          = 20
	MaxCommentsPerWorkItemHour = 30
	commentWindow              = time.Hour
)

// Labels on the Effects the Effector records.
const (
	LabelEffectTask     = "factory.ccf.dev/task"
	LabelEffectRun      = "factory.ccf.dev/run"
	LabelEffectWorkItem = "factory.ccf.dev/workitem"
)

// commentOps count towards the comment rate limits.
var commentOps = []v1alpha1.EffectOp{v1alpha1.OpComment, v1alpha1.OpPRComment, v1alpha1.OpReviewComment, v1alpha1.OpThreadReply}

// effectID is the Effect record name, and the provider idempotency key, of an agent's key on a
// task: hash(task, key), so two tasks never share a key.
func effectID(task, key string) string {
	sum := sha256.Sum256([]byte(task + "\x00" + key))
	return "eff_" + hex.EncodeToString(sum[:20])
}

// CreateEffect implements gen.StrictServerInterface: the Effector API.
//
//  1. the agent holds the task's active lease (403 not-lease-holder);
//  2. the op is in the task's effects.allow (403 effect-not-allowed);
//  3. label ops: every label matches effects.labels (403 label-not-allowed);
//  4. the target is the task's repo, an object in it, or a work item link (403
//     target-not-allowed);
//  5. the idempotency key is present (≤ 200 chars; 400 idempotency-key-required); a known key
//     returns the stored Effect;
//  6. rate limits (429 rate-limited);
//  7. API ops: Conn.Apply as the task's identity (a retryable provider error is 429 with
//     Retry-After); git-side ops are only recorded;
//  8. the Effect is stored and appended to the run's progress (which the SSE stream sends).
func (s *Server) CreateEffect(ctx context.Context, req gen.CreateEffectRequestObject) (gen.CreateEffectResponseObject, error) {
	t, err := s.leasedTask(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	b := req.Body
	side, known := b.Op.Side()
	if !known {
		return nil, &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: fmt.Sprintf("unknown op %q", b.Op),
			fields: []gen.FieldError{{Field: "op", Message: "not an effect op"}}}
	}
	pol := t.Spec.Effects
	if pol == nil || !slices.Contains(pol.Allow, b.Op) {
		return nil, problemf(http.StatusForbidden, CodeEffectNotAllowed, "op %s is not in the task's effects.allow", b.Op)
	}
	if b.Op == v1alpha1.OpLabelAdd || b.Op == v1alpha1.OpLabelRemove {
		if err := checkLabels(b.Args, pol.Labels); err != nil {
			return nil, err
		}
	}
	target, err := s.effectTarget(ctx, t, b.Op, b.Target)
	if err != nil {
		return nil, err
	}
	if b.IdempotencyKey == "" || len(b.IdempotencyKey) > MaxIdempotencyKey {
		return nil, &apiError{status: http.StatusBadRequest, code: CodeIdempotencyKeyRequired,
			detail: fmt.Sprintf("idempotencyKey is required (1 to %d characters)", MaxIdempotencyKey),
			fields: []gen.FieldError{{Field: "idempotencyKey", Message: "required"}}}
	}
	id := effectID(t.Metadata.ID, b.IdempotencyKey)
	if e, err := s.getEffect(ctx, id); err != nil || e != nil {
		if err != nil {
			return nil, err
		}
		return effectResponse(e), nil // replay
	}
	run, err := s.openRun(ctx, t.Metadata.ID)
	if err != nil {
		return nil, err
	}
	if err := s.checkRates(ctx, t, run, b.Op); err != nil {
		return nil, err
	}

	identity := taskIdentity(t, s.d.Catalog)
	var res provider.EffectResult
	if side == v1alpha1.SideAPI {
		conn, ok := s.d.Conns.Conn(target.Provider)
		if !ok {
			return nil, problemf(http.StatusServiceUnavailable, CodeUnavailable, "provider %q is not connected", target.Provider)
		}
		res, err = conn.Apply(ctx, provider.EffectRequest{
			Op: b.Op, Target: target, Args: b.Args, Identity: identity, IdempotencyKey: id,
		})
		if err != nil {
			return nil, applyError(err)
		}
	}

	now := s.clk.Now()
	labels := map[string]string{LabelEffectTask: t.Metadata.ID, LabelEffectWorkItem: t.Metadata.WorkItem}
	if run != nil {
		labels[LabelEffectRun] = run.Metadata.Name
	}
	rec := &v1alpha1.Effect{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindEffect},
		Metadata: v1alpha1.ObjectMeta{Name: id, Labels: labels},
		Spec: v1alpha1.EffectSpec{
			Task: t.Metadata.ID, Op: b.Op, Target: target, IdempotencyKey: id, Identity: identity, Request: b.Args,
		},
		Status: &v1alpha1.EffectStatus{At: now, Result: &v1alpha1.EffectResult{URL: res.URL}},
	}
	if err := s.d.Store.Create(ctx, rec); err != nil {
		if !errors.Is(err, store.ErrAlreadyExists) {
			return nil, fmt.Errorf("record effect %s: %w", id, err)
		}
		e, gerr := s.getEffect(ctx, id) // a concurrent request with the same key won
		if gerr != nil || e == nil {
			return nil, fmt.Errorf("record effect %s: %w", id, err)
		}
		return effectResponse(e), nil
	}
	if run != nil {
		ev := v1alpha1.ProgressEvent{At: now, Kind: "effect", Message: string(b.Op) + " " + target.Ref,
			Data: map[string]any{"effect": id}}
		if res.URL != "" {
			ev.Data["url"] = res.URL
		}
		if err := s.appendProgress(ctx, run.Metadata.Name, ev); err != nil {
			s.log.WarnContext(ctx, "append effect to run progress", "run", run.Metadata.Name, "err", err)
		}
	}
	s.log.InfoContext(ctx, "effect recorded", "task", t.Metadata.ID, "op", b.Op, "effect", id, "side", side)
	return effectResponse(rec), nil
}

func effectResponse(e *v1alpha1.Effect) gen.CreateEffect200JSONResponse {
	out := gen.CreateEffect200JSONResponse{EffectId: e.Metadata.Name}
	if e.Status != nil && e.Status.Result != nil && e.Status.Result.URL != "" {
		u := e.Status.Result.URL
		out.Url = &u
	}
	return out
}

func (s *Server) getEffect(ctx context.Context, id string) (*v1alpha1.Effect, error) {
	var e v1alpha1.Effect
	err := s.d.Store.Get(ctx, store.Key{Kind: v1alpha1.KindEffect, Name: id}, &e)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("get effect %s: %w", id, err)
	}
	return &e, nil
}

// applyError maps a provider's Apply error: retryable is 429 with the provider's Retry-After,
// an invalid or unsupported request 400, a missing target 404, anything else 502.
func applyError(err error) error {
	var re *provider.RetryableError
	switch {
	case errors.As(err, &re):
		return &apiError{status: http.StatusTooManyRequests, code: CodeProviderRetryable,
			detail: "the provider is rate limiting or unavailable; retry with the same idempotency key", retryAfter: max(re.RetryAfter, time.Second), err: err}
	case errors.Is(err, provider.ErrRetryable):
		return &apiError{status: http.StatusTooManyRequests, code: CodeProviderRetryable,
			detail: "the provider is rate limiting or unavailable; retry with the same idempotency key", retryAfter: time.Second, err: err}
	case errors.Is(err, provider.ErrInvalid), errors.Is(err, provider.ErrUnsupported):
		return &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: "the provider refused the effect", err: err}
	case errors.Is(err, provider.ErrNotFound):
		return &apiError{status: http.StatusNotFound, code: CodeNotFound, detail: "the effect's target doesn't exist", err: err}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return &apiError{status: http.StatusBadGateway, code: CodeProviderError, err: err}
}

// checkLabels checks that the label op names labels and that every one matches a glob of the
// task's effects.labels (none matches when there are none).
func checkLabels(args map[string]any, globs []string) error {
	labels := labelArgs(args)
	if len(labels) == 0 {
		return &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: "label ops need args.labels (or args.label)",
			fields: []gen.FieldError{{Field: "args.labels", Message: "required"}}}
	}
	var p patterns.Pattern
	if len(globs) > 0 {
		var err error
		if p, err = patterns.Compile(globs); err != nil {
			return problemf(http.StatusForbidden, CodeLabelNotAllowed, "the task's effects.labels don't compile")
		}
	}
	for _, l := range labels {
		if p == nil || !p.Match(l) {
			return problemf(http.StatusForbidden, CodeLabelNotAllowed, "label %q is not in the task's effects.labels", l)
		}
	}
	return nil
}

// labelArgs returns args.labels (a list of strings) and args.label (one).
func labelArgs(args map[string]any) []string {
	var out []string
	switch v := args["labels"].(type) {
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, v...)
	case string:
		out = append(out, v)
	}
	if s, ok := args["label"].(string); ok && s != "" {
		out = append(out, s)
	}
	return out
}

func repoOf(ref string) string {
	repo, _, _ := strings.Cut(ref, "#")
	return repo
}

// effectTarget resolves the effect's target: the given ref, else the task's primary object
// (its repo for create-issue and pr.open, and for git-side ops). It must be the task's target
// repo, an object in it, or one of the work item's links (403 target-not-allowed).
func (s *Server) effectTarget(ctx context.Context, t *v1alpha1.Task, op v1alpha1.EffectOp, given *string) (v1alpha1.ObjectRef, error) {
	prov, repo, primary := t.Spec.Provider, "", ""
	if tg := t.Spec.Target; tg != nil {
		if tg.Provider != "" {
			prov = tg.Provider
		}
		repo, primary = tg.Repo, tg.Ref
		if repo == "" {
			repo = repoOf(tg.Ref)
		}
	}
	var links []v1alpha1.Link
	var wi v1alpha1.WorkItem
	switch err := s.d.Store.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: t.Metadata.WorkItem}, &wi); {
	case err == nil:
		links = wi.Spec.Links
	case !errors.Is(err, store.ErrNotFound):
		return v1alpha1.ObjectRef{}, fmt.Errorf("get work item %s: %w", t.Metadata.WorkItem, err)
	}
	side, _ := op.Side()
	repoOp := op == v1alpha1.OpCreateIssue || op == v1alpha1.OpPROpen || side == v1alpha1.SideGit

	ref := ""
	if given != nil && *given != "" && *given != "primary" {
		ref = *given
	} else {
		switch {
		case repoOp && repo != "":
			ref = repo
		case primary != "":
			ref = primary
		default:
			for _, l := range links {
				if l.Primary || ref == "" {
					ref = l.Ref
				}
			}
			if ref == "" {
				ref = repo
			}
		}
	}
	if ref == "" {
		return v1alpha1.ObjectRef{}, problemf(http.StatusForbidden, CodeTargetNotAllowed, "the task has no target to act on")
	}
	if repoOp {
		ref = repoOf(ref)
	}
	out := v1alpha1.ObjectRef{Provider: prov, Ref: ref}
	allowed := repo != "" && (ref == repo || repoOf(ref) == repo)
	for _, l := range links {
		if l.Ref == ref {
			out = v1alpha1.ObjectRef{Provider: l.Provider, Kind: l.Kind, Ref: l.Ref}
			allowed = true
			break
		}
	}
	if !allowed {
		return v1alpha1.ObjectRef{}, problemf(http.StatusForbidden, CodeTargetNotAllowed, "target %q is not the task's repo or one of its work item's objects", ref)
	}
	if out.Kind == "" && !strings.Contains(ref, "#") {
		out.Kind = "repo"
	}
	return out, nil
}

// taskIdentity is the factory identity the task acts as: its git credential's, else its
// TaskSpec's, else author.
func taskIdentity(t *v1alpha1.Task, cp controllers.CatalogProvider) string {
	if c := t.Spec.Credentials; c != nil && c.Git != nil && c.Git.Identity != "" {
		return c.Git.Identity
	}
	if cat := cp.Current(); cat != nil {
		if ts := cat.TaskSpecs[t.Spec.TaskSpec.Name]; ts != nil && ts.Spec.Spec.Identity != "" {
			return ts.Spec.Spec.Identity
		}
	}
	return "author"
}

// checkRates enforces the Effector's rate limits: ≤ 60 effects and ≤ 20 comments per run, and
// ≤ 30 comments per work item in any rolling hour (all runs).
func (s *Server) checkRates(ctx context.Context, t *v1alpha1.Task, run *v1alpha1.Run, op v1alpha1.EffectOp) error {
	comment := slices.Contains(commentOps, op)
	if run != nil {
		var effects []*v1alpha1.Effect
		if _, err := s.d.Store.List(ctx, v1alpha1.KindEffect, store.ListOptions{
			LabelSelector: &v1alpha1.LabelSelector{MatchLabels: map[string]string{LabelEffectRun: run.Metadata.Name}},
		}, &effects); err != nil {
			return fmt.Errorf("list effects of run %s: %w", run.Metadata.Name, err)
		}
		comments := 0
		for _, e := range effects {
			if slices.Contains(commentOps, e.Spec.Op) {
				comments++
			}
		}
		if len(effects) >= MaxEffectsPerRun {
			return problemf(http.StatusTooManyRequests, CodeRateLimited, "the run made %d effects (limit %d)", len(effects), MaxEffectsPerRun)
		}
		if comment && comments >= MaxCommentsPerRun {
			return problemf(http.StatusTooManyRequests, CodeRateLimited, "the run made %d comments (limit %d)", comments, MaxCommentsPerRun)
		}
	}
	if !comment || t.Metadata.WorkItem == "" {
		return nil
	}
	var effects []*v1alpha1.Effect
	if _, err := s.d.Store.List(ctx, v1alpha1.KindEffect, store.ListOptions{
		LabelSelector: &v1alpha1.LabelSelector{MatchLabels: map[string]string{LabelEffectWorkItem: t.Metadata.WorkItem}},
	}, &effects); err != nil {
		return fmt.Errorf("list effects of work item %s: %w", t.Metadata.WorkItem, err)
	}
	now := s.clk.Now()
	var recent []time.Time
	for _, e := range effects {
		if e.Status != nil && slices.Contains(commentOps, e.Spec.Op) && now.Sub(e.Status.At) < commentWindow {
			recent = append(recent, e.Status.At)
		}
	}
	if len(recent) >= MaxCommentsPerWorkItemHour {
		slices.SortFunc(recent, time.Time.Compare)
		wait := recent[len(recent)-MaxCommentsPerWorkItemHour].Add(commentWindow).Sub(now)
		return &apiError{status: http.StatusTooManyRequests, code: CodeRateLimited, retryAfter: max(wait, time.Second),
			detail: fmt.Sprintf("the work item had %d comments in the last hour (limit %d)", len(recent), MaxCommentsPerWorkItemHour)}
	}
	return nil
}

// appendProgress appends ev to the run's progress ring (bounded like the scheduler's).
func (s *Server) appendProgress(ctx context.Context, runID string, ev v1alpha1.ProgressEvent) error {
	return s.updateRun(ctx, runID, func(r *v1alpha1.Run) bool {
		if r.Status == nil {
			r.Status = &v1alpha1.RunStatus{}
		}
		if r.Status.End != "" {
			return false
		}
		p := append(r.Status.Progress, ev)
		if over := len(p) - maxProgress; over > 0 {
			p = slices.Clone(p[over:])
		}
		r.Status.Progress = p
		return true
	})
}

// maxProgress is the run's progress ring size (as the scheduler's and run controller's).
const maxProgress = 200
