package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/apiserver/gen"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/rules"
	"github.com/compliance-framework/factory/internal/store"
)

// catalogStatus is catalog-sync's status, or one derived from the live catalog.
func (s *Server) catalogStatus() controllers.CatalogStatus {
	if s.d.CatalogStatus != nil {
		return s.d.CatalogStatus.Status()
	}
	var st controllers.CatalogStatus
	if c := s.d.Catalog.Current(); c != nil {
		st.Ready, st.LastGoodSHA = true, c.SHA
	}
	return st
}

// GetCatalog implements gen.StrictServerInterface: the live catalog's commit, the sync status
// and errors, and the objects it holds.
func (s *Server) GetCatalog(_ context.Context, _ gen.GetCatalogRequestObject) (gen.GetCatalogResponseObject, error) {
	st := s.catalogStatus()
	cat := s.d.Catalog.Current()
	out := gen.CatalogSummary{Ref: s.opts.CatalogRef, Sha: st.LastGoodSHA, Objects: []gen.CatalogObjectRef{}}
	if out.Sha == "" && cat != nil {
		out.Sha = cat.SHA
	}
	if s.opts.CatalogRepo != "" {
		r := s.opts.CatalogRepo
		out.Repo = &r
	}
	if !st.LoadedAt.IsZero() {
		t := st.LoadedAt
		out.LoadedAt = &t
	}
	switch {
	case !st.Ready || cat == nil:
		out.Status = gen.CatalogSummaryStatusLoading
	case (len(st.Errors) > 0 || st.SourceError != "") && st.LastFailure.After(st.LastSuccess):
		out.Status = gen.CatalogSummaryStatusError
	default:
		out.Status = gen.CatalogSummaryStatusLoaded
	}
	for _, e := range st.Errors {
		ce := gen.CatalogError{Message: e.Message}
		if e.Code != "" {
			c := e.Code
			ce.Code = &c
		}
		if e.File != "" {
			f := e.File
			ce.File = &f
		}
		if e.Line > 0 {
			l := e.Line
			ce.Line = &l
		}
		out.Errors = append(out.Errors, ce)
	}
	if st.SourceError != "" {
		c := "source"
		out.Errors = append(out.Errors, gen.CatalogError{Code: &c, Message: st.SourceError})
	}
	if cat != nil {
		out.Objects = catalogRefs(cat)
	}
	return gen.GetCatalog200JSONResponse(out), nil
}

// catalogRefs lists every object of the catalog by kind (in ConfigKinds order), then name.
func catalogRefs(c *catalog.Catalog) []gen.CatalogObjectRef {
	var out []gen.CatalogObjectRef
	add := func(kind string, name string, labels map[string]string) {
		out = append(out, gen.CatalogObjectRef{Kind: gen.CatalogKind(kind), Name: name, Labels: labels})
	}
	for n, o := range c.Providers {
		add(v1alpha1.KindProvider, n, o.Metadata.Labels)
	}
	for n, o := range c.Groups {
		add(v1alpha1.KindGroup, n, o.Metadata.Labels)
	}
	for n, o := range c.Templates {
		add(v1alpha1.KindTemplate, n, o.Metadata.Labels)
	}
	for n, o := range c.Rules {
		add(v1alpha1.KindRule, n, o.Metadata.Labels)
	}
	for n, o := range c.RuleSets {
		add(v1alpha1.KindRuleSet, n, o.Spec.Metadata.Labels)
	}
	for n, o := range c.TaskSpecs {
		add(v1alpha1.KindTaskSpec, n, o.Spec.Metadata.Labels)
	}
	for n, o := range c.Queues {
		add(v1alpha1.KindQueue, n, o.Metadata.Labels)
	}
	for n, o := range c.Pools {
		add(v1alpha1.KindPool, n, o.Metadata.Labels)
	}
	order := v1alpha1.ConfigKinds()
	slices.SortFunc(out, func(a, b gen.CatalogObjectRef) int {
		return cmp.Or(cmp.Compare(slices.Index(order, string(a.Kind)), slices.Index(order, string(b.Kind))), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// GetCatalogObject implements gen.StrictServerInterface.
func (s *Server) GetCatalogObject(_ context.Context, req gen.GetCatalogObjectRequestObject) (gen.GetCatalogObjectResponseObject, error) {
	if !req.Kind.Valid() {
		return nil, &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: fmt.Sprintf("%q is not a config kind", req.Kind),
			fields: []gen.FieldError{{Field: "kind", Message: "not a config kind"}}}
	}
	cat, err := s.catalog()
	if err != nil {
		return nil, err
	}
	var (
		obj   gen.CatalogObject
		found bool
	)
	switch req.Kind {
	case gen.CatalogKindProvider:
		if o := cat.Providers[req.Name]; o != nil {
			v := *o
			v.Kind = v1alpha1.KindProvider
			found, err = true, obj.FromProvider(v)
		}
	case gen.CatalogKindGroup:
		if o := cat.Groups[req.Name]; o != nil {
			v := *o
			v.Kind = v1alpha1.KindGroup
			found, err = true, obj.FromGroup(v)
		}
	case gen.CatalogKindTemplate:
		if o := cat.Templates[req.Name]; o != nil {
			v := *o
			v.Kind = v1alpha1.KindTemplate
			found, err = true, obj.FromTemplate(v)
		}
	case gen.CatalogKindRule:
		if o := cat.Rules[req.Name]; o != nil {
			v := *o
			v.Kind = v1alpha1.KindRule
			found, err = true, obj.FromRule(v)
		}
	case gen.CatalogKindRuleSet:
		if o := cat.RuleSets[req.Name]; o != nil {
			v := *o.Spec
			v.Kind = v1alpha1.KindRuleSet
			found, err = true, obj.FromRuleSet(v)
		}
	case gen.CatalogKindTaskSpec:
		if o := cat.TaskSpecs[req.Name]; o != nil {
			v := *o.Spec
			v.Kind = v1alpha1.KindTaskSpec
			found, err = true, obj.FromTaskSpec(v)
		}
	case gen.CatalogKindQueue:
		if o := cat.Queues[req.Name]; o != nil {
			v := *o
			v.Kind = v1alpha1.KindQueue
			found, err = true, obj.FromQueue(v)
		}
	case gen.CatalogKindPool:
		if o := cat.Pools[req.Name]; o != nil {
			v := *o
			v.Kind = v1alpha1.KindPool
			found, err = true, obj.FromPool(v)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("encode %s %s: %w", req.Kind, req.Name, err)
	}
	if !found {
		return nil, problemf(http.StatusNotFound, CodeNotFound, "no %s %q in the catalog", req.Kind, req.Name)
	}
	return gen.GetCatalogObject200JSONResponse(obj), nil
}

// DryRunCatalog implements gen.StrictServerInterface: the rule controller's evaluation of the
// event, with nothing stored or applied.
func (s *Server) DryRunCatalog(ctx context.Context, req gen.DryRunCatalogRequestObject) (gen.DryRunCatalogResponseObject, error) {
	if _, err := s.catalog(); err != nil {
		return nil, err
	}
	ev := req.Body.Event
	if err := checkEvent(ev, false); err != nil {
		return nil, err
	}
	ev.Kind = v1alpha1.KindEvent
	if ev.ID == "" {
		ev.ID = "dry-run"
	}
	if ev.Time.IsZero() {
		ev.Time = s.clk.Now()
	}
	wi := ""
	if req.Body.WorkItem != nil {
		wi = *req.Body.WorkItem
	}
	eval, err := s.d.Evaluator.DryRun(ctx, ev, wi)
	if errors.Is(err, store.ErrNotFound) {
		return nil, problemf(http.StatusNotFound, CodeNotFound, "no work item %q", wi)
	}
	if err != nil {
		return nil, err
	}
	out, err := evaluation(eval)
	if err != nil {
		return nil, err
	}
	return gen.DryRunCatalog200JSONResponse(out), nil
}

// checkEvent validates an event a client sends (dry-run, dispatch): a known type and a source.
func checkEvent(ev v1alpha1.Event, needID bool) error {
	var fields []gen.FieldError
	if !ev.Type.Known() {
		fields = append(fields, gen.FieldError{Field: "event.type", Message: fmt.Sprintf("%q is not a known event type", ev.Type)})
	}
	if ev.Source == "" {
		fields = append(fields, gen.FieldError{Field: "event.source", Message: "required"})
	}
	if needID && ev.ID == "" {
		fields = append(fields, gen.FieldError{Field: "event.id", Message: "required"})
	}
	if len(fields) > 0 {
		return &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: "invalid event", fields: fields}
	}
	return nil
}

var actionIndexRE = regexp.MustCompile(`^actions\[(\d+)\]`)

// evaluation maps the engine's Evaluation to the API's.
func evaluation(e rules.Evaluation) (gen.Evaluation, error) {
	out := gen.Evaluation{Event: e.Event, Matches: []gen.RuleMatch{}, Decisions: []gen.EvaluationDecision{}, StateAfter: e.StateAfter}
	if e.WorkItem != "" {
		w := e.WorkItem
		out.WorkItem = &w
	}
	for _, m := range e.Matches {
		rm := gen.RuleMatch{RuleSet: m.RuleSet, Rule: m.Rule, Result: gen.RuleMatchResult(m.Result)}
		if m.Error != "" {
			msg := m.Error
			rm.Error = &msg
		}
		if sm := actionIndexRE.FindStringSubmatch(m.Action); sm != nil {
			if i, err := strconv.Atoi(sm[1]); err == nil {
				rm.ActionIndex = &i
			}
		}
		out.Matches = append(out.Matches, rm)
	}
	for _, d := range e.Decisions {
		a, err := actionIntent(d.Action)
		if err != nil {
			return gen.Evaluation{}, err
		}
		out.Decisions = append(out.Decisions, gen.EvaluationDecision{
			RuleSet: d.RuleSet, Rule: d.Rule, Action: a, Rail: railVerdict(d.Rail), Inputs: d.Inputs,
		})
	}
	return out, nil
}

// actionIntent is the API's materialised action: {type, <the member's fields>}.
func actionIntent(a rules.ActionIntent) (gen.ActionIntent, error) {
	var member any
	switch {
	case a.Dispatch != nil:
		member = a.Dispatch
	case a.Set != nil:
		member = a.Set
	case a.Notify != nil:
		member = a.Notify
	case a.Cancel != nil:
		member = a.Cancel
	case a.Link != nil:
		member = a.Link
	case a.Effect != nil:
		member = a.Effect
	case a.Debounce != nil:
		member = a.Debounce
	case a.Unsupported != nil:
		member = a.Unsupported
	}
	out := gen.ActionIntent{Type: gen.ActionIntentType(a.Kind()), AdditionalProperties: map[string]any{}}
	if member == nil {
		return out, nil
	}
	b, err := json.Marshal(member)
	if err != nil {
		return gen.ActionIntent{}, fmt.Errorf("encode action: %w", err)
	}
	if err := json.Unmarshal(b, &out.AdditionalProperties); err != nil {
		return gen.ActionIntent{}, fmt.Errorf("encode action: %w", err)
	}
	delete(out.AdditionalProperties, "type")
	return out, nil
}

// railVerdict maps a rail verdict to the API's (an unsupported action is suppressed).
func railVerdict(v v1alpha1.RailVerdict) gen.EvaluationRail {
	out := gen.EvaluationRail{Verdict: gen.EvaluationRailVerdictAllowed}
	reason := v.Reason
	switch v.Verdict {
	case rules.VerdictSuppressed:
		out.Verdict = gen.EvaluationRailVerdictSuppressed
	case rules.VerdictDeferred:
		out.Verdict = gen.EvaluationRailVerdictDeferred
	case rules.VerdictUnsupported:
		out.Verdict = gen.EvaluationRailVerdictSuppressed
		if reason == "" {
			reason = "unsupported"
		}
	}
	if v.Rail != "" && reason != "" {
		reason = v.Rail + ": " + reason
	}
	if reason != "" {
		out.Reason = &reason
	}
	return out
}
