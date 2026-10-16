package catalog

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/patterns"
)

// checkSelector reports invalid matchExpressions (CAT303): In/NotIn need values,
// Exists/DoesNotExist take none.
func (v *validator) checkSelector(d *document, p fieldPath, s v1alpha1.LabelSelector) {
	for i, r := range s.MatchExpressions {
		ep := p.at("matchExpressions", i)
		switch r.Operator {
		case "In", "NotIn":
			if len(r.Values) == 0 {
				v.errorf(d, ep.at("values"), CodeBadSelector, "operator %s needs at least one value", r.Operator)
			}
		case "Exists", "DoesNotExist":
			if len(r.Values) > 0 {
				v.errorf(d, ep.at("values"), CodeBadSelector, "operator %s takes no values", r.Operator)
			}
		default:
			v.errorf(d, ep.at("operator"), CodeBadSelector, "unknown operator %q (In, NotIn, Exists, DoesNotExist)", r.Operator)
		}
	}
}

// providerLabels are a provider's labels, plus `type: <spec.type>` unless it sets one: rule
// selectors name provider types or families.
func providerLabels(p *v1alpha1.Provider) map[string]string {
	out := make(map[string]string, len(p.Metadata.Labels)+1)
	for k, val := range p.Metadata.Labels {
		out[k] = val
	}
	if _, ok := out["type"]; !ok {
		out["type"] = string(p.Spec.Type)
	}
	return out
}

func (v *validator) provider(name string) *v1alpha1.Provider {
	if d := v.providers[name]; d != nil {
		return d.obj.(*v1alpha1.Provider)
	}
	return nil
}

// factoryOnly reports whether every type in on is a factory.* type.
func factoryOnly(on []v1alpha1.EventType) bool {
	for _, t := range on {
		if !t.IsFactory() {
			return false
		}
	}
	return len(on) > 0
}

func hasFactory(on []v1alpha1.EventType) bool {
	return slices.ContainsFunc(on, v1alpha1.EventType.IsFactory)
}

// resolveSelection is phase 3: each RuleSet's providers, scope and rules.
func (v *validator) resolveSelection() {
	for _, name := range v.ruleNames {
		r := v.rules[name]
		switch ps := r.rule.Spec.ProviderSelector; {
		case ps != nil:
			v.checkSelector(r.doc, r.path("spec", "providerSelector"), *ps)
		case len(r.rule.Spec.On) > 0 && !factoryOnly(r.rule.Spec.On):
			v.errorf(r.doc, r.path("spec", "on"), CodeProviderMismatch, "provider event types need a providerSelector (only factory.*-only rules may omit it)")
		}
	}
	for _, name := range v.ruleSetNames {
		rse := v.ruleSets[name]
		spec := rse.rs.Spec
		v.checkSelector(rse.doc, fieldPath{"spec", "providerSelector"}, spec.ProviderSelector)
		for _, pn := range sortedKeys(v.providers) {
			if spec.ProviderSelector.Matches(providerLabels(v.provider(pn))) {
				rse.providers = append(rse.providers, pn)
			}
		}
		if len(rse.providers) == 0 {
			v.errorf(rse.doc, fieldPath{"spec", "providerSelector"}, CodeNoProviders, "matches no Provider (providers: %s)", names(v.providers))
		}
		if sc := spec.Scope; sc != nil {
			globs := func(field string, gs []string) []patterns.Pattern {
				return compilePatterns(gs, func(i int, err error) {
					v.errorf(rse.doc, fieldPath{"spec", "scope", field}.at(i), CodeBadSelector, "%v", err)
				})
			}
			rse.scope = CompiledScope{
				Repos:    globs("repos", sc.Repos),
				Exclude:  globs("exclude", sc.Exclude),
				Channels: globs("channels", sc.Channels),
				Branches: globs("branches", sc.Branches),
			}
		}
		v.selectRules(name, rse)
	}
}

// selectRules resolves refs ∪ selector ∪ inline rules, minus exclude, and checks each
// selected rule's providerSelector against the RuleSet's providers.
func (v *validator) selectRules(name string, rse *ruleSetEntry) {
	sel := rse.rs.Spec.Rules
	picked := map[string]bool{}
	for _, ref := range sel.Refs {
		if r := v.rules[ref]; r != nil && r.inlineOf == "" {
			picked[ref] = true
		}
	}
	if sel.Selector != nil {
		v.checkSelector(rse.doc, fieldPath{"spec", "rules", "selector"}, *sel.Selector)
		matched := 0
		for _, rn := range v.ruleNames {
			r := v.rules[rn]
			if r.inlineOf == "" && sel.Selector.Matches(r.rule.Metadata.Labels) {
				picked[rn] = true
				matched++
			}
		}
		if matched == 0 {
			v.errorf(rse.doc, fieldPath{"spec", "rules", "selector"}, CodeNoRules, "matches no Rule")
		}
	}
	for _, rn := range v.ruleNames {
		if v.rules[rn].inlineOf == name {
			picked[rn] = true
		}
	}
	for _, ex := range sel.Exclude {
		delete(picked, ex)
		delete(picked, name+"/"+ex)
	}
	for _, rn := range sortedKeys(picked) {
		r := v.rules[rn]
		rse.selected = append(rse.selected, r)
		if factoryOnly(r.rule.Spec.On) || len(rse.providers) == 0 || r.rule.Spec.ProviderSelector == nil {
			continue // nothing to match, CAT300, or the rule-level CAT302 above
		}
		if len(v.ruleProviders(rse, r)) == 0 {
			v.errorf(r.doc, r.path("spec", "providerSelector"), CodeProviderMismatch, "matches none of RuleSet %s's providers (%s)", name, strings.Join(rse.providers, ", "))
		}
	}
}

// ruleProviders are the RuleSet's providers that a rule's providerSelector matches (all of
// them when the rule has none).
func (v *validator) ruleProviders(rse *ruleSetEntry, r *ruleEntry) []string {
	var out []string
	for _, pn := range rse.providers {
		if ps := r.rule.Spec.ProviderSelector; ps == nil || ps.Matches(providerLabels(v.provider(pn))) {
			out = append(out, pn)
		}
	}
	return out
}

// effectiveProviders is EffectiveRule.Providers: the matched providers, plus the factory
// provider when on has factory.* types (["factory"] for a factory-only rule).
func (v *validator) effectiveProviders(rse *ruleSetEntry, r *ruleEntry) []string {
	var out []string
	if !factoryOnly(r.rule.Spec.On) {
		out = v.ruleProviders(rse, r)
	}
	if hasFactory(r.rule.Spec.On) {
		out = append(out, v1alpha1.FactoryProvider)
	}
	return out
}

// checkVocabulary is phase 4.
func (v *validator) checkVocabulary() {
	for _, name := range sortedKeys(v.providers) {
		d := v.providers[name]
		p := d.obj.(*v1alpha1.Provider)
		if name == v1alpha1.FactoryProvider {
			v.errorf(d, fieldPath{"metadata", "name"}, CodeFactoryReserved, "%q is the built-in provider, the only source of factory.* events; a catalog Provider can't use the name", name)
		}
		if p.Spec.Type == v1alpha1.ProviderTypeFactory {
			v.errorf(d, fieldPath{"spec", "type"}, CodeFactoryReserved, "type %q is reserved for the built-in provider", p.Spec.Type)
		}
	}
	for _, name := range v.ruleNames {
		v.checkSchedule(v.rules[name])
	}
	for _, name := range v.ruleSetNames {
		rse := v.ruleSets[name]
		for _, r := range rse.selected {
			provs := v.ruleProviders(rse, r)
			if len(provs) == 0 || factoryOnly(r.rule.Spec.On) {
				continue // nothing to check, or CAT300/CAT302 already
			}
			for i, t := range r.rule.Spec.On {
				if t.IsFactory() || !t.Known() {
					continue
				}
				if !slices.ContainsFunc(provs, func(pn string) bool { return t.EmittedBy(v.provider(pn).Spec.Type) }) {
					v.errorf(r.doc, r.path("spec", "on").at(i), CodeNotEmittable, "%s is not emitted by RuleSet %s's providers for this rule (%s); emitted by provider types %v", t, name, strings.Join(provs, ", "), t.ProviderTypes())
				}
			}
		}
	}
}

var cronMacros = []string{"@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly"}

var cronFieldRE = regexp.MustCompile(`^[0-9A-Za-z*/,?#-]+$`)

// checkSchedule: schedule only with on: [factory.schedule.tick], and the tick needs a
// schedule (CAT402).
func (v *validator) checkSchedule(r *ruleEntry) {
	s := r.rule.Spec
	tick := slices.Index(s.On, v1alpha1.EventFactoryScheduleTick)
	switch {
	case s.Schedule != "" && (len(s.On) != 1 || tick != 0):
		v.errorf(r.doc, r.path("spec", "schedule"), CodeSchedule, "schedule is only allowed with on: [%s]", v1alpha1.EventFactoryScheduleTick)
	case s.Schedule == "" && tick >= 0:
		v.errorf(r.doc, r.path("spec", "on").at(tick), CodeSchedule, "%s needs a schedule", v1alpha1.EventFactoryScheduleTick)
	case s.Schedule != "":
		if err := checkCron(s.Schedule); err != nil {
			v.errorf(r.doc, r.path("spec", "schedule"), CodeSchedule, "%v", err)
		}
	}
}

// checkCron is a shape check of a cron expression: a macro, "@every <duration>", or five
// fields. The scheduler (RT) parses it for real.
func checkCron(s string) error {
	if strings.HasPrefix(s, "@every ") {
		if _, err := v1alpha1.Duration(strings.TrimPrefix(s, "@every ")).Parse(); err != nil {
			return fmt.Errorf("schedule %q: %w", s, err)
		}
		return nil
	}
	if strings.HasPrefix(s, "@") {
		if !slices.Contains(cronMacros, s) {
			return fmt.Errorf("schedule %q: unknown macro", s)
		}
		return nil
	}
	fields := strings.Fields(s)
	if len(fields) != 5 {
		return fmt.Errorf("schedule %q: want 5 cron fields (minute hour day month weekday), got %d", s, len(fields))
	}
	for _, f := range fields {
		if !cronFieldRE.MatchString(f) {
			return fmt.Errorf("schedule %q: invalid field %q", s, f)
		}
	}
	return nil
}

// checkOverlap is phase 12: a rule selected by two RuleSets with overlapping scope for the
// same providers is evaluated in each, but fires at most once per event, through the first
// RuleSet by name whose where passes (the engine dedupes firing per (event, rule)). Warning
// (CAT1200).
func (v *validator) checkOverlap() {
	for i, an := range v.ruleSetNames {
		a := v.ruleSets[an]
		for _, bn := range v.ruleSetNames[i+1:] {
			b := v.ruleSets[bn]
			if !intersects(a.providers, b.providers) || !scopesOverlap(a.scope, b.scope) {
				continue
			}
			for _, r := range b.selected {
				if r.inlineOf != "" || !slices.Contains(a.selected, r) {
					continue
				}
				v.warnf(b.doc, fieldPath{"spec", "rules"}, CodeOverlap, "rule %s is also selected by RuleSet %s, with overlapping scope and providers: it is evaluated in each RuleSet with its params, and fires at most once per event, through the first RuleSet (by name) whose where passes (%s, then %s)", r.name, an, an, bn)
			}
		}
	}
}

func intersects(a, b []string) bool {
	return slices.ContainsFunc(a, func(s string) bool { return slices.Contains(b, s) })
}

// scopesOverlap is a best-effort static check: two glob lists overlap when either is empty
// (any) or a glob of one matches a glob of the other as written. Excludes are ignored.
func scopesOverlap(a, b CompiledScope) bool {
	return globsOverlap(a.Repos, b.Repos) && globsOverlap(a.Channels, b.Channels) && globsOverlap(a.Branches, b.Branches)
}

func globsOverlap(a, b []patterns.Pattern) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, x := range a {
		for _, y := range b {
			if x.Match(y.String()) || y.Match(x.String()) {
				return true
			}
		}
	}
	return false
}
