package v1alpha1

import (
	"errors"
	"fmt"
	"slices"
)

// Label selector operators.
const (
	SelectorOpIn           = "In"
	SelectorOpNotIn        = "NotIn"
	SelectorOpExists       = "Exists"
	SelectorOpDoesNotExist = "DoesNotExist"
)

// ErrInvalidSelector is wrapped by LabelSelector.Validate's errors.
var ErrInvalidSelector = errors.New("invalid label selector")

// Matches reports whether labels satisfy the selector, as in Kubernetes: every matchLabels
// entry and every matchExpressions requirement must hold. A nil or empty selector matches
// everything; a requirement with an unknown operator matches nothing.
func (s *LabelSelector) Matches(labels map[string]string) bool {
	if s == nil {
		return true
	}
	for k, want := range s.MatchLabels {
		if got, ok := labels[k]; !ok || got != want {
			return false
		}
	}
	for _, r := range s.MatchExpressions {
		got, ok := labels[r.Key]
		switch r.Operator {
		case SelectorOpIn:
			if !ok || !slices.Contains(r.Values, got) {
				return false
			}
		case SelectorOpNotIn:
			if ok && slices.Contains(r.Values, got) {
				return false
			}
		case SelectorOpExists:
			if !ok {
				return false
			}
		case SelectorOpDoesNotExist:
			if ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Validate reports the first malformed matchExpressions entry, wrapping ErrInvalidSelector:
// a missing key, an unknown operator, In or NotIn without values, or Exists or DoesNotExist
// with values. A nil selector is valid.
func (s *LabelSelector) Validate() error {
	if s == nil {
		return nil
	}
	for i, r := range s.MatchExpressions {
		switch {
		case r.Key == "":
			return fmt.Errorf("%w: matchExpressions[%d]: missing key", ErrInvalidSelector, i)
		case r.Operator == SelectorOpIn || r.Operator == SelectorOpNotIn:
			if len(r.Values) == 0 {
				return fmt.Errorf("%w: matchExpressions[%d]: %s needs at least one value", ErrInvalidSelector, i, r.Operator)
			}
		case r.Operator == SelectorOpExists || r.Operator == SelectorOpDoesNotExist:
			if len(r.Values) > 0 {
				return fmt.Errorf("%w: matchExpressions[%d]: %s takes no values", ErrInvalidSelector, i, r.Operator)
			}
		default:
			return fmt.Errorf("%w: matchExpressions[%d]: unknown operator %q (In, NotIn, Exists, DoesNotExist)", ErrInvalidSelector, i, r.Operator)
		}
	}
	return nil
}
