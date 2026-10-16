package v1alpha1

import (
	"errors"
	"testing"
)

func TestLabelSelectorMatches(t *testing.T) {
	labels := map[string]string{"team": "core", "tier": "1"}
	req := func(key, op string, vals ...string) *LabelSelector {
		return &LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: key, Operator: op, Values: vals}}}
	}
	for _, tc := range []struct {
		name   string
		sel    *LabelSelector
		labels map[string]string
		want   bool
	}{
		{"nil", nil, labels, true},
		{"empty", &LabelSelector{}, labels, true},
		{"empty, no labels", &LabelSelector{}, nil, true},
		{"matchLabels", &LabelSelector{MatchLabels: map[string]string{"team": "core"}}, labels, true},
		{"matchLabels all", &LabelSelector{MatchLabels: map[string]string{"team": "core", "tier": "1"}}, labels, true},
		{"matchLabels miss", &LabelSelector{MatchLabels: map[string]string{"team": "web"}}, labels, false},
		{"matchLabels absent", &LabelSelector{MatchLabels: map[string]string{"x": ""}}, labels, false},
		{"In", req("tier", SelectorOpIn, "1", "2"), labels, true},
		{"In miss", req("tier", SelectorOpIn, "2"), labels, false},
		{"In absent", req("x", SelectorOpIn, "1"), labels, false},
		{"NotIn", req("tier", SelectorOpNotIn, "2"), labels, true},
		{"NotIn hit", req("tier", SelectorOpNotIn, "1"), labels, false},
		{"NotIn absent", req("x", SelectorOpNotIn, "1"), labels, true},
		{"Exists", req("team", SelectorOpExists), labels, true},
		{"Exists absent", req("x", SelectorOpExists), labels, false},
		{"DoesNotExist", req("x", SelectorOpDoesNotExist), labels, true},
		{"DoesNotExist present", req("team", SelectorOpDoesNotExist), labels, false},
		{"DoesNotExist, no labels", req("team", SelectorOpDoesNotExist), nil, true},
		{"unknown operator", req("team", "Equals", "core"), labels, false},
		{"both must hold", &LabelSelector{
			MatchLabels:      map[string]string{"team": "core"},
			MatchExpressions: []LabelSelectorRequirement{{Key: "tier", Operator: SelectorOpIn, Values: []string{"2"}}},
		}, labels, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sel.Matches(tc.labels); got != tc.want {
				t.Fatalf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLabelSelectorValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    LabelSelectorRequirement
		ok   bool
	}{
		{"In", LabelSelectorRequirement{Key: "a", Operator: SelectorOpIn, Values: []string{"x"}}, true},
		{"NotIn", LabelSelectorRequirement{Key: "a", Operator: SelectorOpNotIn, Values: []string{"x"}}, true},
		{"Exists", LabelSelectorRequirement{Key: "a", Operator: SelectorOpExists}, true},
		{"DoesNotExist", LabelSelectorRequirement{Key: "a", Operator: SelectorOpDoesNotExist}, true},
		{"In without values", LabelSelectorRequirement{Key: "a", Operator: SelectorOpIn}, false},
		{"NotIn without values", LabelSelectorRequirement{Key: "a", Operator: SelectorOpNotIn}, false},
		{"Exists with values", LabelSelectorRequirement{Key: "a", Operator: SelectorOpExists, Values: []string{"x"}}, false},
		{"DoesNotExist with values", LabelSelectorRequirement{Key: "a", Operator: SelectorOpDoesNotExist, Values: []string{"x"}}, false},
		{"unknown operator", LabelSelectorRequirement{Key: "a", Operator: "Gt", Values: []string{"1"}}, false},
		{"missing key", LabelSelectorRequirement{Operator: SelectorOpExists}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (&LabelSelector{MatchExpressions: []LabelSelectorRequirement{tc.r}}).Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("Validate = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalidSelector) {
				t.Fatalf("Validate = %v, not ErrInvalidSelector", err)
			}
		})
	}
	var nilSel *LabelSelector
	if err := nilSel.Validate(); err != nil {
		t.Fatalf("nil selector: %v", err)
	}
	if err := (&LabelSelector{MatchLabels: map[string]string{"a": "b"}}).Validate(); err != nil {
		t.Fatalf("matchLabels only: %v", err)
	}
}
