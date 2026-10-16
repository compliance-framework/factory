package catalog

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/cel"
	"github.com/compliance-framework/factory/internal/patterns"
)

func loadPoC(t *testing.T) (*Catalog, []Error) {
	t.Helper()
	return LoadDir(os.DirFS("testdata/poc"), Options{})
}

// TestPoC is the task's done criterion: the design/10-poc.md catalog validates. Its warnings
// (the RuleSets that select the same rules for overlapping scope) are in poc.golden.
func TestPoC(t *testing.T) {
	c, errs := loadPoC(t)
	if HasErrors(errs) || c == nil {
		t.Fatalf("the PoC catalog has errors:\n%s", format(errs))
	}
	checkGolden(t, "poc", format(errs))

	for kind, got := range map[string]int{
		"providers": len(c.Providers), "groups": len(c.Groups), "pools": len(c.Pools), "queues": len(c.Queues),
		"taskspecs": len(c.TaskSpecs), "templates": len(c.Templates), "rules": len(c.Rules), "rulesets": len(c.RuleSets),
		"prompts": len(c.Assets.Prompts), "schemas": len(c.Assets.Schemas),
	} {
		want := map[string]int{
			"providers": 1, "groups": 2, "pools": 1, "queues": 1, "taskspecs": 5, "templates": 4,
			"rules": 16, "rulesets": 5, "prompts": 5, "schemas": 5,
		}[kind]
		if got != want {
			t.Errorf("%d %s, want %d", got, kind, want)
		}
	}
	if !strings.HasPrefix(c.SHA, "dir:") || len(c.SHA) != len("dir:")+64 {
		t.Errorf("SHA = %q, want dir:<sha256>", c.SHA)
	}
}

func effectiveNames(rs *CompiledRuleSet) []string {
	var out []string
	for _, r := range rs.Rules {
		out = append(out, r.Name)
	}
	return out
}

func effective(t *testing.T, c *Catalog, ruleSet, rule string) *EffectiveRule {
	t.Helper()
	rs := c.RuleSets[ruleSet]
	if rs == nil {
		t.Fatalf("no RuleSet %s", ruleSet)
	}
	for _, r := range rs.Rules {
		if r.Name == rule {
			return r
		}
	}
	t.Fatalf("RuleSet %s has no rule %s (has %v)", ruleSet, rule, effectiveNames(rs))
	return nil
}

func TestPoCCompiledView(t *testing.T) {
	c, errs := loadPoC(t)
	if c == nil {
		t.Fatalf("errors:\n%s", format(errs))
	}

	// Selection: selector ∪ shared pack, minus exclude; priority desc then name.
	if got, want := effectiveNames(c.RuleSets["poc-docs-epic"]), []string{
		"record-merge-outcome", "record-review-outcome", // priority 100
		"review-whole-stack", // priority 50: reads stackNext before build-stack-next increments it
		"announce-ptbr-live", "build-stack-next", "build-stack-start", "escalate-needs-human",
		"fix-stack-review", "plan-maintainer-epics",
	}; !slices.Equal(got, want) {
		t.Errorf("poc-docs-epic rules = %v, want %v", got, want)
	}
	if got := effectiveNames(c.RuleSets["poc-release-docs"]); slices.Contains(got, "merge-release-docs") {
		t.Errorf("merge-release-docs is excluded, got %v", got)
	}
	if got := c.RuleSets["poc-release-docs"].Providers; !slices.Equal(got, []string{"github-public"}) {
		t.Errorf("providers = %v", got)
	}

	// Params: the rule's own in prod, the overrides in dev, substituted into the actions.
	prod := effective(t, c, "poc-release-docs", "docs-on-release-train")
	dev := effective(t, c, "dev-release-docs", "docs-on-release-train")
	if got := prod.Rule.Spec.Actions[0].Dispatch.Target.Repo; got != "compliance-framework/docs" {
		t.Errorf("prod target = %q", got)
	}
	if got := dev.Rule.Spec.Actions[0].Dispatch.Target.Repo; got != "compliance-framework/mock-docs" {
		t.Errorf("dev target = %q", got)
	}
	if dev.Params["allowDryRun"] != true || dev.Params["trainManifest"] != "repos.mock.yaml" || prod.Params["allowDryRun"] != false {
		t.Errorf("params: prod %v, dev %v", prod.Params, dev.Params)
	}
	if c.Rules["docs-on-release-train"].Spec.Actions[0].Dispatch.Target.Repo != "{{ params.docsRepo }}" {
		t.Error("Catalog.Rules must hold the rules as written")
	}
}

func TestPoCExpressions(t *testing.T) {
	c, errs := loadPoC(t)
	if c == nil {
		t.Fatalf("errors:\n%s", format(errs))
	}
	// Every "=" leaf is in the index, so the engine can EvalTree the actions.
	for _, src := range []string{
		"event.object.ref", "{'train': item.state.train}", "run.result.plan", "size(run.result.plan)",
		"item.state.plan[0].issue", "item.state.stackNext + 1", "item.state.plan[item.state.stackNext].issue",
		"run.result.branch", "run.outcome", "task.target.repo + '@' + inputs.base",
	} {
		if c.Programs[src] == nil {
			t.Errorf("%q is not in Catalog.Programs", src)
		}
	}
	r := effective(t, c, "poc-docs-epic", "build-stack-next")
	act := cel.Activation{
		Run:  map[string]any{"spec": "implement", "inputs": map[string]any{"mode": "new"}, "outcome": "pr-opened", "result": map[string]any{"branch": "factory/stack/1"}},
		Item: map[string]any{"state": map[string]any{"stackNext": 0, "stackTotal": 3, "plan": []any{map[string]any{"issue": "#1"}, map[string]any{"issue": "#2"}}}},
	}
	ok, err := r.Where.EvalBool(context.Background(), act)
	if err != nil || !ok {
		t.Fatalf("where = %v, %v", ok, err)
	}
	if r.Actions[1].Type != "dispatch" {
		t.Fatalf("action 1 is %s", r.Actions[1].Type)
	}
	got, err := cel.EvalTree(context.Background(), r.Actions[1].Tree, act, c.Programs)
	if err != nil {
		t.Fatal(err)
	}
	inputs := got.(map[string]any)["inputs"].(map[string]any)
	if inputs["issue"] != "#1" || inputs["base"] != "factory/stack/1" {
		t.Errorf("inputs = %v", inputs)
	}

	impl := c.TaskSpecs["implement"]
	if impl.Expressions["spec.workspace.repos[0].ref"] == nil {
		t.Errorf("implement expressions = %v", impl.Expressions)
	}
	if impl.Prompt == nil || impl.OutputSchema == nil || !strings.HasPrefix(impl.Digest, "sha256:") || impl.Default == nil {
		t.Errorf("implement compiled: prompt %v, schema %v, digest %q, default %v", impl.Prompt != nil, impl.OutputSchema != nil, impl.Digest, impl.Default)
	}
	if len(c.TaskSpecs["release-docs"].Constraints) != 2 {
		t.Errorf("release-docs constraints = %v", c.TaskSpecs["release-docs"].Constraints)
	}
	// The input "schema": defaults fill in, so only train is required for release-docs.
	if errs := c.TaskSpecs["release-docs"].ValidateInputs(map[string]any{"train": "compliance-framework/workflows#1"}); len(errs) > 0 {
		t.Errorf("ValidateInputs: %v", errs)
	}
	if errs := c.TaskSpecs["implement"].ValidateInputs(map[string]any{"mode": "=x"}); len(errs) != 1 {
		t.Errorf("ValidateInputs with a literal \"=x\": %v (runtime inputs are evaluated, so \"=x\" is just a string)", errs)
	}

	// The PoC's result schemas extend the v1alpha1 Result.
	s := c.Assets.Schemas["schemas/results/plan-epic.json"]
	if err := s.Validate(map[string]any{"task": "t", "status": "done", "question": nil, "outcome": "decomposed"}); err != nil {
		t.Errorf("valid result rejected: %v", err)
	}
	if err := s.Validate(map[string]any{"task": "t", "status": "done", "question": nil, "outcome": "merged"}); err == nil {
		t.Error("result with a foreign outcome accepted")
	}
}

func TestPoCSources(t *testing.T) {
	c, _ := loadPoC(t)
	if c == nil {
		t.Fatal("no catalog")
	}
	want := map[ObjectKey]SourcePos{
		{Kind: v1alpha1.KindRule, Name: "docs-on-release-train"}: {File: "rules/release-docs/docs-on-release-train.yaml", Line: 1},
		{Kind: v1alpha1.KindTaskSpec, Name: "merge"}:             {File: "taskspecs/merge.yaml", Line: 1},
		{Kind: v1alpha1.KindProvider, Name: "github-public"}:     {File: "providers/github-public.yaml", Line: 1},
	}
	for k, w := range want {
		if got := c.Sources[k]; got != w {
			t.Errorf("Sources[%v] = %v, want %v", k, got, w)
		}
	}
}

func TestSHADeterministic(t *testing.T) {
	a, _ := LoadDir(fsWith(nil), Options{})
	b, _ := LoadDir(fsWith(nil), Options{})
	if a == nil || b == nil || a.SHA != b.SHA {
		t.Fatalf("SHA differs between loads: %v / %v", a, b)
	}
	changed, _ := LoadDir(fsWith(map[string]string{"prompts/work.md.tmpl": "Work on it.\n"}), Options{})
	if changed == nil || changed.SHA == a.SHA {
		t.Error("SHA did not change with the content")
	}
	pinned, _ := LoadDir(fsWith(nil), Options{SHA: "0123abc"})
	if pinned == nil || pinned.SHA != "0123abc" {
		t.Errorf("Options.SHA not used: %v", pinned)
	}
}

func TestDiscovery(t *testing.T) {
	c, errs := LoadDir(fsWith(map[string]string{
		"testdata/case/event.yaml":   "not: an object\n",
		"rules/testdata/x.yaml":      "neither: is this\n",
		"prompts/notes.yaml":         "prompts are assets: {{ .x }}\n",
		".github/workflows/ci.yaml":  "on: push\n",
		"README.md":                  "# catalog\n",
		"schemas/results/extra.json": `{"type": "object"}`,
		"rules/more.yml":             "# comment-only document\n---\n\n---\n" + strings.Replace(base["rules/work.yaml"], "work-on-issue", "second", 1),
	}), Options{})
	if c == nil {
		t.Fatalf("errors:\n%s", format(errs))
	}
	if c.Rules["second"] == nil {
		t.Error("the rule in the .yml multi-document file was not loaded")
	}
	if c.Assets.Prompts["prompts/notes.yaml"] == nil || c.Assets.Schemas["schemas/results/extra.json"] == nil {
		t.Error("assets not loaded")
	}
}

func TestInlineRules(t *testing.T) {
	c, errs := LoadDir(fsWith(map[string]string{
		"rulesets/rs.yaml": base["rulesets/rs.yaml"] + `  overrides:
    local: { greeting: hello }
  inlineRules:
    - metadata: { name: local }
      spec:
        on: [factory.run.completed]
        where: "run.spec == 'work'"
        params: { greeting: hi }
        actions: [{ set: { work: "{{ params.greeting }}" } }]
`,
	}), Options{})
	if c == nil {
		t.Fatalf("errors:\n%s", format(errs))
	}
	if c.Rules["rs/local"] == nil {
		t.Fatalf("rules = %v", sortedKeys(c.Rules))
	}
	r := effective(t, c, "rs", "rs/local")
	if got := r.Rule.Spec.Actions[0].Set["work"]; got != "hello" {
		t.Errorf("override not applied: %v", got)
	}
	if !slices.Equal(r.Providers, []string{v1alpha1.FactoryProvider}) {
		t.Errorf("providers = %v", r.Providers)
	}
	if got := c.Sources[ObjectKey{Kind: v1alpha1.KindRule, Name: "rs/local"}]; got.File != "rulesets/rs.yaml" || got.Line != 11 {
		t.Errorf("source = %v", got)
	}

	_, errs = LoadDir(fsWith(map[string]string{
		"rulesets/rs.yaml": base["rulesets/rs.yaml"] + `  inlineRules:
    - metadata: { name: local }
      spec: { on: [factory.run.completed], actions: [{ set: { nope: 1 } }] }
`,
	}), Options{})
	if got := format(errs); got != "rulesets/rs.yaml:10: CAT1000 spec.inlineRules[0].spec.actions[0].set.nope: state key \"nope\" is not in RuleSet rs's stateKeys (work)\n" {
		t.Errorf("inline rule error position:\n%s", got)
	}
}

func TestSplitDocuments(t *testing.T) {
	data := []byte("a: 1\n---\nb: 2\n--- # c\nc: 3\nd: |\n  ---\n")
	segs := splitDocuments(data)
	if len(segs) != 3 {
		t.Fatalf("%d segments", len(segs))
	}
	for i, wantLine := range []int{1, 3, 5} {
		root, err := parseSegment(segs[i])
		if err != nil {
			t.Fatal(err)
		}
		if root.Line != wantLine {
			t.Errorf("segment %d: root at line %d, want %d", i, root.Line, wantLine)
		}
	}
}

func TestParsePath(t *testing.T) {
	for in, want := range map[string]fieldPath{
		"spec.actions[0].dispatch.queue": {"spec", "actions", "[0]", "dispatch", "queue"},
		"spec.actions":                   {"spec", "actions"},
		"a[1][2].b":                      {"a", "[1]", "[2]", "b"},
	} {
		got := parsePath(in)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parsePath(%q) = %v, want %v", in, got, want)
		}
		if got.String() != in {
			t.Errorf("String() = %q, want %q", got.String(), in)
		}
	}
}

func TestScope(t *testing.T) {
	c, _ := loadPoC(t)
	if c == nil {
		t.Fatal("no catalog")
	}
	s := c.RuleSets["dev-release-docs"].Scope
	for repo, want := range map[string]bool{
		"compliance-framework/mock-docs": true, "compliance-framework/mock-api": true, "compliance-framework/docs": false,
	} {
		if got := s.MatchRepo(repo); got != want {
			t.Errorf("MatchRepo(%s) = %v", repo, got)
		}
	}
	if !(CompiledScope{}).MatchRepo("any/repo") || !(CompiledScope{}).MatchChannel("#x") {
		t.Error("an empty scope matches everything")
	}
}

func TestCapabilitiesFiles(t *testing.T) {
	if got := DefaultCapabilities(); len(got) != 2 || got[0].Harness != "codex" || got[1].Harness != "claude-code" {
		t.Errorf("default capabilities = %v", got)
	}
	if got := DefaultKnownModels(); !slices.Equal(got["claude-code"], []string{"claude-sonnet-5-5", "claude-opus-5-5"}) {
		t.Errorf("default known models = %v", got)
	}
	if caps, err := ParseCapabilities(defaultHarnesses); err != nil || len(caps) != 2 {
		t.Errorf("the embedded file as --capabilities: %v, %v", caps, err)
	}
	for _, bad := range []string{`[{"harness": ""}]`, `[{"harness": "a"}, {"harness": "a"}]`, `[{"harness": "a", "x": 1}]`, `nope`} {
		if _, err := ParseCapabilities([]byte(bad)); err == nil {
			t.Errorf("ParseCapabilities(%s) accepted", bad)
		}
	}
	if m, err := ParseKnownModels([]byte(`{"codex": ["gpt-6-codex"]}`)); err != nil || m["codex"][0] != "gpt-6-codex" {
		t.Errorf("ParseKnownModels: %v, %v", m, err)
	}

	// Custom capabilities and known models change the verdicts.
	fsys := fsWith(nil)
	if _, errs := LoadDir(fsys, Options{KnownModels: map[string][]string{"codex": {"o4"}}}); !hasCode(errs, CodeNotSubsumed) {
		t.Errorf("a known-models list without gpt-5 models: want CAT801, got:\n%s", format(errs))
	}
	caps := []v1alpha1.AdapterCapabilities{{Harness: "codex", Features: []string{"mcp"}}}
	if _, errs := LoadDir(fsys, Options{Capabilities: caps}); !hasCode(errs, CodeCapability) {
		t.Errorf("codex without structured-output: want CAT803, got:\n%s", format(errs))
	}
}

func hasCode(errs []Error, code string) bool {
	return slices.ContainsFunc(errs, func(e Error) bool { return e.Code == code })
}

func TestErrorFormat(t *testing.T) {
	e := Error{Code: CodeInputMissing, Message: "dispatch.inputs.train: required input missing", File: "catalog/rules/x.yaml", Line: 7, Severity: SeverityError}
	if got := e.Error(); got != "catalog/rules/x.yaml:7: CAT700 dispatch.inputs.train: required input missing" {
		t.Errorf("Error() = %q", got)
	}
	e.Severity, e.Line = SeverityWarning, 0
	if got := e.Error(); got != "catalog/rules/x.yaml: CAT700 warning: dispatch.inputs.train: required input missing" {
		t.Errorf("Error() = %q", got)
	}
}

func TestMapFSErrorsDontPanic(t *testing.T) {
	// Garbage in every shape must produce diagnostics, never a panic.
	for name, doc := range map[string]string{
		"scalar":   "just a string\n",
		"list":     "- a\n- b\n",
		"alias":    "a: &x [1]\nb: *x\nkind: Rule\n",
		"merge":    "base: &b { kind: Rule }\n<<: *b\n",
		"intkey":   "1: a\nkind: Rule\n",
		"badkind":  "kind: Frobnicator\n",
		"nullspec": "kind: Rule\nmetadata: { name: r }\nspec: null\n",
		"tab":      "kind: Rule\n\tmetadata: x\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, errs := LoadDir(fstest.MapFS{"x.yaml": {Data: []byte(doc)}}, Options{})
			if !HasErrors(errs) {
				t.Errorf("no errors for %q", doc)
			}
		})
	}
}

// TestDerivedConstraintResolves: a PoC TaskSpec without harness.allowed gets one constraint
// derived from its harness.default, so it resolves against a 10-poc agent's offer.
func TestDerivedConstraintResolves(t *testing.T) {
	c, errs := loadPoC(t)
	if c == nil {
		t.Fatalf("errors:\n%s", format(errs))
	}
	// The harnesses of agt_gus-mbp (design/10-poc.md §Provider, Group, Pool, Queue).
	codex := patterns.OfferFrom(v1alpha1.HarnessOffer{Name: "codex", Models: []string{"gpt-5-codex"}, ReasoningEffort: []string{"low", "medium", "high"}})
	claude := patterns.OfferFrom(v1alpha1.HarnessOffer{Name: "claude-code", Models: []string{"claude-sonnet-5-5", "claude-opus-5-5"}, DefaultModel: "claude-sonnet-5-5", ReasoningEffort: []string{"low", "medium", "high", "max"}})
	tests := []struct {
		taskSpec string
		offer    patterns.Offer
		want     patterns.Resolved
	}{
		{"plan-epic", codex, patterns.Resolved{Harness: "codex", Model: "gpt-5-codex", Effort: "high"}},
		{"implement", codex, patterns.Resolved{Harness: "codex", Model: "gpt-5-codex", Effort: "medium"}},
		{"merge", codex, patterns.Resolved{Harness: "codex", Model: "gpt-5-codex", Effort: "low"}},
		{"docs-review", claude, patterns.Resolved{Harness: "claude-code", Model: "claude-sonnet-5-5", Effort: "high"}},
	}
	for _, tt := range tests {
		t.Run(tt.taskSpec, func(t *testing.T) {
			ts := c.TaskSpecs[tt.taskSpec]
			if len(ts.Spec.Spec.Harness.Allowed) != 0 || len(ts.Constraints) != 1 || ts.Default == nil {
				t.Fatalf("want one derived constraint and a default, got %v / %v", ts.Constraints, ts.Default)
			}
			got, err := patterns.Resolve(ts.Constraints, *ts.Default, tt.offer)
			if err != nil || got != tt.want {
				t.Errorf("Resolve = %v, %v; want %v", got, err, tt.want)
			}
		})
	}

	// Nothing outside the default runs: another harness, or an effort the default excludes.
	ts := c.TaskSpecs["plan-epic"]
	if _, err := patterns.Resolve(ts.Constraints, patterns.Request{Harness: "claude-code"}, claude); !errors.Is(err, patterns.ErrNoMatch) {
		t.Errorf("claude-code for plan-epic: %v, want ErrNoMatch", err)
	}
	low, _ := patterns.Compile("low")
	if _, err := patterns.Resolve(ts.Constraints, patterns.Request{Harness: "codex", Effort: low}, codex); !errors.Is(err, patterns.ErrNoMatch) {
		t.Errorf("effort low for plan-epic (default high): %v, want ErrNoMatch", err)
	}
}
