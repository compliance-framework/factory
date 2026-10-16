package controllers

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/provider"
)

// Destination kinds an EffectRequest.Target can name besides issues and PRs.
const (
	KindChannel = "channel"
	KindGroup   = "group"
	KindThread  = "thread"
)

// errNotify is a notification that can't be sent as configured (recorded, not retried).
var errNotify = errors.New("notify")

// notifyRequest is one rendered message to one destination.
type notifyRequest struct {
	provider string
	req      provider.EffectRequest
}

// templateData is what a Template sees: event, actor, item, run, approval (as rules see them)
// and links.ui (the work item in the Factory UI).
func templateData(ev v1alpha1.Event, wi *v1alpha1.WorkItem, uiBase string) map[string]any {
	event := map[string]any{}
	for k, v := range ev.Data.Facts {
		event[k] = v
	}
	event["id"], event["type"], event["source"], event["subject"], event["time"] = ev.ID, string(ev.Type), ev.Source, ev.Subject, ev.Time
	if o := ev.Data.Object; o != nil {
		event["object"] = map[string]any{"provider": o.Provider, "kind": o.Kind, "ref": o.Ref}
	}
	actor := map[string]any{}
	if a := ev.Data.Actor; a != nil {
		actor = map[string]any{"id": a.ID, "tier": a.Tier, "isBot": a.IsBot, "isFactory": a.IsFactory, "identity": a.Identity}
	}
	item := itemSummary(wi)
	if wi.Status != nil {
		item["counters"] = jsonValue(wi.Status.Counters)
	}
	run, _ := ev.Data.Facts["run"].(map[string]any)
	approval, _ := ev.Data.Facts["approval"].(map[string]any)
	return map[string]any{
		"event": event, "actor": actor, "item": item,
		"run": orEmpty(run), "approval": orEmpty(approval),
		"links": map[string]any{"ui": strings.TrimSuffix(uiBase, "/") + "/workitems/" + wi.Metadata.Name},
	}
}

// renderBody renders the Template's body for a provider type.
func renderBody(t *v1alpha1.Template, ptype v1alpha1.ProviderType, data map[string]any) (string, error) {
	body, ok := t.Spec.Bodies[ptype]
	if !ok {
		return "", fmt.Errorf("%w: template %s has no %s body", errNotify, t.Metadata.Name, ptype)
	}
	if body.Blocks != "" {
		// Block Kit bodies are files the catalog doesn't load yet (Slack is after the PoC).
		return "", fmt.Errorf("%w: template %s: %s blocks bodies are not supported", errNotify, t.Metadata.Name, ptype)
	}
	tmpl, err := template.New(t.Metadata.Name).Parse(body.Text)
	if err != nil {
		return "", fmt.Errorf("%w: template %s: %w", errNotify, t.Metadata.Name, err)
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("%w: template %s: %w", errNotify, t.Metadata.Name, err)
	}
	return b.String(), nil
}

// notifyRequests renders the Template once per destination object: comments on the work
// item's linked objects (item: primary | threads | all), a message to a channel, or a mention
// of a group through the `via` provider.
func notifyRequests(cat *catalog.Catalog, tmplName string, to []v1alpha1.Destination, wi *v1alpha1.WorkItem, data map[string]any, identity string) ([]notifyRequest, error) {
	t := cat.Templates[tmplName]
	if t == nil {
		return nil, fmt.Errorf("%w: template %q does not exist", errNotify, tmplName)
	}
	ptype := func(name string) (v1alpha1.ProviderType, error) {
		p := cat.Providers[name]
		if p == nil {
			return "", fmt.Errorf("%w: provider %q is not in the catalog", errNotify, name)
		}
		return p.Spec.Type, nil
	}
	var out []notifyRequest
	for _, d := range to {
		switch {
		case d.Item != "":
			for _, l := range itemLinks(wi, d.Item) {
				pt, err := ptype(l.Provider)
				if err != nil {
					return nil, err
				}
				body, err := renderBody(t, pt, data)
				if err != nil {
					return nil, err
				}
				op := v1alpha1.OpComment
				if l.Kind == provider.KindPR {
					op = v1alpha1.OpPRComment
				}
				out = append(out, notifyRequest{provider: l.Provider, req: provider.EffectRequest{
					Op: op, Target: v1alpha1.ObjectRef{Provider: l.Provider, Kind: l.Kind, Ref: l.Ref},
					Args: map[string]any{"body": body}, Identity: identity,
				}})
			}
		case d.Channel != nil:
			pt, err := ptype(d.Channel.Provider)
			if err != nil {
				return nil, err
			}
			body, err := renderBody(t, pt, data)
			if err != nil {
				return nil, err
			}
			out = append(out, notifyRequest{provider: d.Channel.Provider, req: provider.EffectRequest{
				Op: v1alpha1.OpComment, Target: v1alpha1.ObjectRef{Provider: d.Channel.Provider, Kind: KindChannel, Ref: d.Channel.Name},
				Args: map[string]any{"body": body}, Identity: identity,
			}})
		case d.Group != "":
			if d.Via == "" {
				return nil, fmt.Errorf("%w: group %s has no via provider", errNotify, d.Group)
			}
			pt, err := ptype(d.Via)
			if err != nil {
				return nil, err
			}
			body, err := renderBody(t, pt, data)
			if err != nil {
				return nil, err
			}
			as := d.As
			if as == "" {
				as = "mention"
			}
			out = append(out, notifyRequest{provider: d.Via, req: provider.EffectRequest{
				Op: v1alpha1.OpComment, Target: v1alpha1.ObjectRef{Provider: d.Via, Kind: KindGroup, Ref: d.Group},
				Args: map[string]any{"body": body, "as": as}, Identity: identity,
			}})
		default:
			return nil, fmt.Errorf("%w: destination names no group, channel or item", errNotify)
		}
	}
	return out, nil
}

// itemLinks are the links an item destination names.
func itemLinks(wi *v1alpha1.WorkItem, which string) []v1alpha1.Link {
	switch which {
	case "primary":
		if l, ok := primaryLink(wi.Spec.Links); ok {
			return []v1alpha1.Link{l}
		}
	case "threads":
		var out []v1alpha1.Link
		for _, l := range wi.Spec.Links {
			if l.Kind == KindThread {
				out = append(out, l)
			}
		}
		return out
	case "all":
		return append([]v1alpha1.Link(nil), wi.Spec.Links...)
	}
	return nil
}
