package server

import (
	"fmt"
	"strings"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// parseSelector parses a Kubernetes-style label selector string into a LabelSelector:
// comma-separated requirements, each `key`, `!key`, `key=value` (or `==`), `key!=value`,
// `key in (a,b)` or `key notin (a,b)`. An empty string selects everything (nil).
func parseSelector(s string) (*v1alpha1.LabelSelector, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts, err := splitRequirements(s)
	if err != nil {
		return nil, err
	}
	sel := &v1alpha1.LabelSelector{}
	for _, p := range parts {
		r, err := parseRequirement(p)
		if err != nil {
			return nil, err
		}
		sel.MatchExpressions = append(sel.MatchExpressions, r)
	}
	if err := sel.Validate(); err != nil {
		return nil, err
	}
	return sel, nil
}

// splitRequirements splits on the commas outside parentheses.
func splitRequirements(s string) ([]string, error) {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
			if depth > 1 {
				return nil, fmt.Errorf("label selector %q: nested parentheses", s)
			}
		case ')':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("label selector %q: unbalanced parentheses", s)
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("label selector %q: unbalanced parentheses", s)
	}
	return append(out, s[start:]), nil
}

func parseRequirement(p string) (v1alpha1.LabelSelectorRequirement, error) {
	p = strings.TrimSpace(p)
	bad := func() (v1alpha1.LabelSelectorRequirement, error) {
		return v1alpha1.LabelSelectorRequirement{}, fmt.Errorf("label selector: malformed requirement %q", p)
	}
	if p == "" {
		return bad()
	}
	if key, ok := strings.CutPrefix(p, "!"); ok {
		key = strings.TrimSpace(key)
		if !validLabelKey(key) {
			return bad()
		}
		return v1alpha1.LabelSelectorRequirement{Key: key, Operator: v1alpha1.SelectorOpDoesNotExist}, nil
	}
	for _, op := range []struct{ tok, op string }{{"!=", v1alpha1.SelectorOpNotIn}, {"==", v1alpha1.SelectorOpIn}, {"=", v1alpha1.SelectorOpIn}} {
		if key, val, ok := strings.Cut(p, op.tok); ok {
			key, val = strings.TrimSpace(key), strings.TrimSpace(val)
			if !validLabelKey(key) || strings.ContainsAny(val, "=!() ") {
				return bad()
			}
			return v1alpha1.LabelSelectorRequirement{Key: key, Operator: op.op, Values: []string{val}}, nil
		}
	}
	if open := strings.IndexByte(p, '('); open >= 0 {
		if !strings.HasSuffix(p, ")") {
			return bad()
		}
		fields := strings.Fields(p[:open])
		if len(fields) != 2 || !validLabelKey(fields[0]) {
			return bad()
		}
		var op string
		switch strings.ToLower(fields[1]) {
		case "in":
			op = v1alpha1.SelectorOpIn
		case "notin":
			op = v1alpha1.SelectorOpNotIn
		default:
			return bad()
		}
		var vals []string
		for _, v := range strings.Split(p[open+1:len(p)-1], ",") {
			if v = strings.TrimSpace(v); v != "" {
				vals = append(vals, v)
			}
		}
		if len(vals) == 0 {
			return bad()
		}
		return v1alpha1.LabelSelectorRequirement{Key: fields[0], Operator: op, Values: vals}, nil
	}
	if !validLabelKey(p) {
		return bad()
	}
	return v1alpha1.LabelSelectorRequirement{Key: p, Operator: v1alpha1.SelectorOpExists}, nil
}

func validLabelKey(k string) bool {
	if k == "" || len(k) > 253 {
		return false
	}
	for _, r := range k {
		if !alnum(r) && !strings.ContainsRune("-_./", r) {
			return false
		}
	}
	return true
}
