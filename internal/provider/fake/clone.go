package fake

import (
	"maps"
	"slices"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
)

// cloneValue deep-copies JSON-like values (maps, slices, scalars).
func cloneValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		return cloneMap(v)
	case []any:
		if v == nil {
			return v
		}
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = cloneValue(e)
		}
		return out
	case []string:
		return slices.Clone(v)
	case map[string]string:
		return maps.Clone(v)
	case map[string]int:
		return maps.Clone(v)
	default:
		return v
	}
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneProjection(p provider.Projection) provider.Projection {
	p.State = cloneMap(p.State)
	p.Counters = maps.Clone(p.Counters)
	p.Links = slices.Clone(p.Links)
	p.Labels = slices.Clone(p.Labels)
	return p
}

func cloneObservation(o provider.Observation) provider.Observation {
	o.Labels = slices.Clone(o.Labels)
	if o.Author != nil {
		a := *o.Author
		o.Author = &a
	}
	if o.Issue != nil {
		i := *o.Issue
		i.SubIssues = slices.Clone(i.SubIssues)
		i.LinkedPRs = slices.Clone(i.LinkedPRs)
		o.Issue = &i
	}
	if o.PR != nil {
		pr := *o.PR
		pr.ApprovedBy = slices.Clone(pr.ApprovedBy)
		o.PR = &pr
	}
	if o.Deploy != nil {
		d := *o.Deploy
		o.Deploy = &d
	}
	if o.Projections != nil {
		ps := make([]provider.Projection, len(o.Projections))
		for i, p := range o.Projections {
			ps[i] = cloneProjection(p)
		}
		o.Projections = ps
	}
	return o
}

func cloneEffectRequest(e provider.EffectRequest) provider.EffectRequest {
	e.Args = cloneMap(e.Args)
	return e
}

func cloneEffectResult(r provider.EffectResult) provider.EffectResult {
	if r.Object != nil {
		o := *r.Object
		r.Object = &o
	}
	return r
}

func cloneRawEvent(ev provider.RawEvent) provider.RawEvent {
	ev.Headers = ev.Headers.Clone()
	ev.Body = slices.Clone(ev.Body)
	return ev
}

func cloneRef(r *v1alpha1.ObjectRef) *v1alpha1.ObjectRef {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}
