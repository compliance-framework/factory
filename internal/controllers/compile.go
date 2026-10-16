package controllers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/catalog/gitsource"
	"github.com/compliance-framework/factory/internal/cel"
	"github.com/compliance-framework/factory/internal/rules"
)

// Task compilation defaults.
const (
	// MaxCredentialTTL caps a task's git credential (design/02 §Safety rails: ≤ 1h).
	MaxCredentialTTL = time.Hour
	// credentialSlack is added to maxWallClock for the credential TTL.
	credentialSlack = 10 * time.Minute
	// LabelTaskSpec is the Task label holding its TaskSpec name.
	LabelTaskSpec = "factory.ccf.dev/taskspec"
	// LLMAgentLocal is the only LLM credential mode: the agent's own harness login.
	LLMAgentLocal = "agent-local"
	// TrustUntrusted labels context the agent must treat as untrusted input.
	TrustUntrusted = "untrusted"
)

// SkillResolver resolves a TaskSpec skill (source, ref tag) to a repo URL and a commit.
type SkillResolver interface {
	Resolve(ctx context.Context, source, ref string) (url, commit string, err error)
}

// KeepSkills is the SkillResolver that resolves nothing: the source stays as written and the
// commit empty. It is the default; `factory serve` (RT-S2-T04) wires GitSkills.
type KeepSkills struct{}

// Resolve implements SkillResolver.
func (KeepSkills) Resolve(_ context.Context, source, _ string) (string, string, error) {
	return source, "", nil
}

// GitSkills resolves a skill ref with `git ls-remote` (gitsource.Head), caching each
// (url, ref). A source that isn't a URL is a repo under Base ("ccf-skills" →
// "https://github.com/compliance-framework/ccf-skills"). It is safe for concurrent use.
type GitSkills struct {
	Base string

	mu    sync.Mutex
	cache map[string]string
}

// Resolve implements SkillResolver.
func (g *GitSkills) Resolve(ctx context.Context, source, ref string) (string, string, error) {
	url := source
	if !strings.Contains(source, "://") {
		url = strings.TrimSuffix(g.Base, "/") + "/" + source
	}
	if ref == "" {
		return url, "", nil
	}
	k := url + "@" + ref
	g.mu.Lock()
	commit, ok := g.cache[k]
	g.mu.Unlock()
	if ok {
		return url, commit, nil
	}
	commit, err := gitsource.Head(ctx, url, ref)
	if err != nil {
		return "", "", fmt.Errorf("resolve skills %s@%s: %w", gitsource.RedactURL(url), ref, err)
	}
	g.mu.Lock()
	if g.cache == nil {
		g.cache = map[string]string{}
	}
	g.cache[k] = commit
	g.mu.Unlock()
	return url, commit, nil
}

// objectID is a deterministic id for an idempotency key: "<prefix>_<20 hex>".
func objectID(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	return prefix + "_" + hex.EncodeToString(sum[:10])
}

// errInvalidDispatch is a dispatch that can't become a Task (recorded on the decision, not
// retried).
var errInvalidDispatch = errors.New("invalid dispatch")

// compileInput is what one Dispatch compiles from.
type compileInput struct {
	cat      *catalog.Catalog
	d        *rules.Dispatch
	wi       *v1alpha1.WorkItem
	ev       v1alpha1.Event
	ruleSet  string
	rule     string
	taskID   string
	deferred string // the budget reason, when the rails deferred the dispatch
	now      time.Time
}

// compileTask compiles a Dispatch into a Pending Task and its prompt (controllers.md §Task
// compilation): inputs merged with the TaskSpec defaults and validated, workspace "=" fields
// evaluated, the prompt rendered from context refs (never inline bodies), skills resolved and
// the credentials request (minted at lease time) filled in.
func compileTask(ctx context.Context, in compileInput, skills SkillResolver) (*v1alpha1.Task, *TaskPrompt, error) {
	ts := in.cat.TaskSpecs[in.d.TaskSpec]
	if ts == nil {
		return nil, nil, fmt.Errorf("%w: TaskSpec %q does not exist", errInvalidDispatch, in.d.TaskSpec)
	}
	q := in.cat.Queues[in.d.Queue]
	if q == nil {
		return nil, nil, fmt.Errorf("%w: queue %q does not exist", errInvalidDispatch, in.d.Queue)
	}
	spec := ts.Spec.Spec

	// 1. Inputs: TaskSpec defaults ← dispatch inputs, validated.
	inputs := map[string]any{}
	for k, decl := range spec.Inputs {
		if decl.Default != nil {
			inputs[k] = decl.Default
		}
	}
	maps.Copy(inputs, in.d.Inputs)
	if errs := ts.ValidateInputs(inputs); len(errs) > 0 {
		return nil, nil, fmt.Errorf("%w: inputs: %w", errInvalidDispatch, errors.Join(errs...))
	}

	target := taskTarget(in.d.Target, in.wi)

	// 2. Workspace: evaluate the "=" refs with inputs and task.
	act := cel.Activation{
		Inputs: inputs,
		Task:   map[string]any{"id": in.taskID, "target": map[string]any{"repo": target.Repo}},
		Now:    in.now,
	}
	type mount struct{ repo, branch, mode string }
	var mounts []mount
	if ws := spec.Workspace; ws != nil {
		for i, m := range ws.Repos {
			ref := m.Ref
			if _, ok := cel.CELSource(ref); ok {
				p := ts.Expressions[fmt.Sprintf("spec.workspace.repos[%d].ref", i)]
				if p == nil {
					return nil, nil, fmt.Errorf("%w: workspace.repos[%d].ref is not compiled", errInvalidDispatch, i)
				}
				v, err := p.EvalValue(ctx, act)
				if err != nil {
					return nil, nil, fmt.Errorf("%w: workspace.repos[%d].ref: %w", errInvalidDispatch, i, err)
				}
				s, ok := v.(string)
				if !ok {
					return nil, nil, fmt.Errorf("%w: workspace.repos[%d].ref is %T, want a string", errInvalidDispatch, i, v)
				}
				ref = s
			} else {
				ref = cel.Unescape(ref)
			}
			repo, branch, _ := strings.Cut(ref, "@")
			mode := m.Mode
			if mode == "" {
				mode = "ro"
			}
			mounts = append(mounts, mount{repo, branch, mode})
		}
	}

	// 3. Context refs: the agent fetches them; bodies are never inlined.
	var ctxItems []v1alpha1.ContextItem
	seen := map[string]bool{}
	add := func(c v1alpha1.ContextItem) {
		k := c.Kind + "|" + c.Provider + "|" + c.Ref
		if c.Ref == "" || seen[k] {
			return
		}
		seen[k] = true
		ctxItems = append(ctxItems, c)
	}
	for _, c := range in.d.Context {
		add(contextItem(c, in.ev))
	}
	if l, ok := primaryLink(in.wi.Spec.Links); ok {
		add(v1alpha1.ContextItem{Kind: l.Kind, Provider: l.Provider, Ref: l.Ref, Trust: TrustUntrusted})
	}
	for _, m := range mounts {
		ref := m.repo
		if m.branch != "" {
			ref += "@" + m.branch
		}
		add(v1alpha1.ContextItem{Kind: "repo", Provider: target.Provider, Ref: ref, Mount: "/work/" + path.Base(m.repo)})
	}

	// 4. Prompt.
	prompt := &TaskPrompt{TypeMeta: v1alpha1.TypeMeta{Kind: KindTaskPrompt}, Metadata: v1alpha1.ObjectMeta{Name: in.taskID}}
	if ts.Prompt != nil && ts.Prompt.Template != nil {
		var b bytes.Buffer
		data := map[string]any{
			"task": map[string]any{
				"id": in.taskID, "spec": in.d.TaskSpec,
				"target": map[string]any{"provider": target.Provider, "repo": target.Repo, "ref": target.Ref},
			},
			"inputs":  inputs,
			"item":    itemSummary(in.wi),
			"context": jsonValue(ctxItems),
		}
		if err := ts.Prompt.Template.Execute(&b, data); err != nil {
			return nil, nil, fmt.Errorf("%w: render prompt %s: %w", errInvalidDispatch, ts.Prompt.Path, err)
		}
		prompt.Text = b.String()
	}
	sum := sha256.Sum256([]byte(prompt.Text))
	prompt.Digest = "sha256:" + hex.EncodeToString(sum[:])

	// 5. Skills: ref tag → commit.
	var skillRefs []v1alpha1.SkillRef
	for _, s := range spec.Skills {
		url, commit, err := skills.Resolve(ctx, s.Source, s.Ref)
		if err != nil {
			return nil, nil, err // transient (git): retried
		}
		if s.Commit != "" {
			commit = s.Commit
		}
		skillRefs = append(skillRefs, v1alpha1.SkillRef{Source: url, Ref: s.Ref, Commit: commit, Use: slices.Clone(s.Use)})
	}

	// 6. Credentials request: write on at most one repo, read on the others.
	cred := &v1alpha1.GitCredentialSpec{Provider: target.Provider, Identity: spec.Identity, Permissions: maps.Clone(spec.Permissions)}
	for _, m := range mounts {
		if m.mode == "rw" && cred.Repo == "" {
			cred.Repo = m.repo
		}
	}
	if cred.Repo == "" {
		cred.Repo = target.Repo
	}
	for _, m := range mounts {
		if m.repo != cred.Repo && !slices.Contains(cred.ReadRepos, m.repo) {
			cred.ReadRepos = append(cred.ReadRepos, m.repo)
		}
	}
	if cred.Repo == "" && len(cred.ReadRepos) > 0 {
		cred.Repo, cred.ReadRepos = cred.ReadRepos[0], cred.ReadRepos[1:]
	}
	ttl := MaxCredentialTTL
	if spec.Limits != nil && spec.Limits.MaxWallClock != "" {
		if d, err := spec.Limits.MaxWallClock.Parse(); err == nil && d > 0 {
			ttl = min(ttl, d+credentialSlack)
		}
	}
	cred.TTL = v1alpha1.Duration(ttl.String())

	// Harness and requires: the dispatch's request, else the TaskSpec default.
	var harness *v1alpha1.TaskHarness
	switch {
	case in.d.Harness != nil:
		h := *in.d.Harness
		harness = &v1alpha1.TaskHarness{Request: &h}
	case spec.Harness != nil && spec.Harness.Default != nil:
		h := *spec.Harness.Default
		harness = &v1alpha1.TaskHarness{Request: &h}
	}
	var requires []string
	if spec.Requires != nil {
		requires = append(requires, spec.Requires.Labels...)
	}
	if in.d.Requires != nil {
		requires = append(requires, in.d.Requires.Labels...)
	}
	slices.Sort(requires)
	requires = slices.Compact(requires)

	task := &v1alpha1.Task{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindTask},
		Metadata: v1alpha1.TaskMeta{
			ID:         in.taskID,
			WorkItem:   in.wi.Metadata.Name,
			Provenance: &v1alpha1.Provenance{Event: in.ev.ID, RuleSet: in.ruleSet, Rule: in.rule, CatalogSHA: in.cat.SHA},
			Labels:     map[string]string{LabelTaskSpec: in.d.TaskSpec},
		},
		Spec: v1alpha1.TaskRunSpec{
			TaskSpec:    v1alpha1.TaskSpecRef{Name: in.d.TaskSpec, Digest: ts.Digest},
			Queue:       in.d.Queue,
			Pool:        q.Spec.Pool,
			Provider:    target.Provider,
			Target:      &target,
			Inputs:      inputs,
			Harness:     harness,
			Skills:      skillRefs,
			Prompt:      &v1alpha1.PromptRef{Ref: "api://tasks/" + in.taskID + "/prompt", Digest: prompt.Digest},
			Context:     ctxItems,
			Credentials: &v1alpha1.TaskCredentials{Git: cred, LLM: LLMAgentLocal},
			Effects:     cloneEffects(spec.Effects),
			Limits:      cloneLimits(spec.Limits),
			Output:      cloneOutput(spec.Output),
		},
		Status: &v1alpha1.TaskStatus{Phase: v1alpha1.TaskPending, Reason: in.deferred},
	}
	if len(requires) > 0 {
		task.Spec.Requires = &v1alpha1.Requires{Labels: requires}
	}
	return task, prompt, nil
}

// taskTarget is the dispatch target, completed from the work item's primary link.
func taskTarget(t *v1alpha1.Target, wi *v1alpha1.WorkItem) v1alpha1.Target {
	var out v1alpha1.Target
	if t != nil {
		out = *t
	}
	if l, ok := primaryLink(wi.Spec.Links); ok {
		if out.Provider == "" {
			out.Provider = l.Provider
		}
		if out.Ref == "" && out.Repo == "" {
			out.Ref = l.Ref
		}
	}
	if out.Repo == "" {
		out.Repo, _, _ = strings.Cut(out.Ref, "#")
	}
	return out
}

// contextItem maps a dispatch context entry ("event.object", "event.review", a ref) to a
// context ref.
func contextItem(c string, ev v1alpha1.Event) v1alpha1.ContextItem {
	if c == "event.object" {
		if o := ev.Data.Object; o != nil {
			p := o.Provider
			if p == "" {
				p = ev.Source
			}
			return v1alpha1.ContextItem{Kind: o.Kind, Provider: p, Ref: o.Ref, Trust: TrustUntrusted}
		}
		return v1alpha1.ContextItem{}
	}
	if fact, ok := strings.CutPrefix(c, "event."); ok {
		switch v := ev.Data.Facts[fact].(type) {
		case map[string]any:
			if ref, _ := v["ref"].(string); ref != "" {
				return v1alpha1.ContextItem{Kind: fact, Provider: ev.Source, Ref: ref, Trust: TrustUntrusted}
			}
			return v1alpha1.ContextItem{Kind: fact, Provider: ev.Source, Ref: ev.ID + "#" + fact, Trust: TrustUntrusted}
		case string:
			return v1alpha1.ContextItem{Kind: fact, Provider: ev.Source, Ref: v, Trust: TrustUntrusted}
		}
		if ev.Data.ContentRef != "" {
			return v1alpha1.ContextItem{Kind: fact, Provider: ev.Source, Ref: ev.Data.ContentRef, Trust: TrustUntrusted}
		}
		return v1alpha1.ContextItem{}
	}
	return v1alpha1.ContextItem{Kind: "ref", Ref: c, Trust: TrustUntrusted}
}

// itemSummary is the work item as prompts see it: id, state and links (no bodies).
func itemSummary(wi *v1alpha1.WorkItem) map[string]any {
	m := map[string]any{"id": wi.Metadata.Name, "links": jsonValue(wi.Spec.Links), "state": map[string]any{}}
	if wi.Status != nil && wi.Status.State != nil {
		m["state"] = jsonValue(wi.Status.State)
	}
	return m
}

func cloneEffects(e *v1alpha1.EffectsPolicy) *v1alpha1.EffectsPolicy {
	if e == nil {
		return nil
	}
	return &v1alpha1.EffectsPolicy{Allow: slices.Clone(e.Allow), Labels: slices.Clone(e.Labels)}
}

func cloneLimits(l *v1alpha1.Limits) *v1alpha1.Limits {
	if l == nil {
		return nil
	}
	c := *l
	return &c
}

func cloneOutput(o *v1alpha1.OutputSpec) *v1alpha1.OutputSpec {
	if o == nil {
		return nil
	}
	return &v1alpha1.OutputSpec{Schema: o.Schema, Outcomes: slices.Clone(o.Outcomes)}
}
