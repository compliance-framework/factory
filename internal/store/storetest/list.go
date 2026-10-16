package storetest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
)

var listTests = []test{
	{name: "LabelSelectors", run: testListLabelSelectors},
	{name: "InvalidSelectors", run: testListInvalidSelectors},
	{name: "FieldSelectors", run: testListFieldSelectors},
	{name: "Pagination", run: testListPagination},
	{name: "OutTypes", run: testListOutTypes},
	{name: "ResourceVersion", run: testListResourceVersion},
}

func testListLabelSelectors(t *testing.T, h *harness) {
	h.mustCreate(t,
		newWorkItem("a", map[string]string{"team": "core", "tier": "1"}),
		newWorkItem("b", map[string]string{"team": "core"}),
		newWorkItem("c", map[string]string{"team": "web", "tier": "2"}),
		newWorkItem("d", nil),
		newRun("other-kind", "t"),
	)
	sel := func(ml map[string]string, exprs ...v1alpha1.LabelSelectorRequirement) *v1alpha1.LabelSelector {
		return &v1alpha1.LabelSelector{MatchLabels: ml, MatchExpressions: exprs}
	}
	req := func(key, op string, vals ...string) v1alpha1.LabelSelectorRequirement {
		return v1alpha1.LabelSelectorRequirement{Key: key, Operator: op, Values: vals}
	}
	for _, tc := range []struct {
		name string
		sel  *v1alpha1.LabelSelector
		want []string
	}{
		{"nil", nil, []string{"a", "b", "c", "d"}},
		{"empty", sel(nil), []string{"a", "b", "c", "d"}},
		{"matchLabels", sel(map[string]string{"team": "core"}), []string{"a", "b"}},
		{"matchLabels two", sel(map[string]string{"team": "core", "tier": "1"}), []string{"a"}},
		{"In", sel(nil, req("tier", v1alpha1.SelectorOpIn, "1", "2")), []string{"a", "c"}},
		{"NotIn", sel(nil, req("team", v1alpha1.SelectorOpNotIn, "core")), []string{"c", "d"}},
		{"Exists", sel(nil, req("tier", v1alpha1.SelectorOpExists)), []string{"a", "c"}},
		{"DoesNotExist", sel(nil, req("tier", v1alpha1.SelectorOpDoesNotExist)), []string{"b", "d"}},
		{"both", sel(map[string]string{"team": "core"}, req("tier", v1alpha1.SelectorOpDoesNotExist)), []string{"b"}},
		{"none", sel(map[string]string{"team": "nobody"}), []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := listNames(t, h, v1alpha1.KindWorkItem, store.ListOptions{LabelSelector: tc.sel})
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func testListInvalidSelectors(t *testing.T, h *harness) {
	for _, r := range []v1alpha1.LabelSelectorRequirement{
		{Key: "a", Operator: "Equals", Values: []string{"x"}},
		{Key: "a", Operator: v1alpha1.SelectorOpIn},
		{Key: "a", Operator: v1alpha1.SelectorOpExists, Values: []string{"x"}},
		{Operator: v1alpha1.SelectorOpExists},
	} {
		var out []v1alpha1.WorkItem
		_, err := h.s.List(h.ctx, v1alpha1.KindWorkItem, store.ListOptions{
			LabelSelector: &v1alpha1.LabelSelector{MatchExpressions: []v1alpha1.LabelSelectorRequirement{r}},
		}, &out)
		wantErr(t, err, store.ErrInvalid)
		_, err = h.s.Watch(h.ctx, v1alpha1.KindWorkItem, store.WatchOptions{
			LabelSelector: &v1alpha1.LabelSelector{MatchExpressions: []v1alpha1.LabelSelectorRequirement{r}},
		})
		wantErr(t, err, store.ErrInvalid)
	}
}

func testListFieldSelectors(t *testing.T, h *harness) {
	h.mustCreate(t,
		newTask("t1", "wi-1", "triage", v1alpha1.TaskPending, nil),
		newTask("t2", "wi-1", "implement", v1alpha1.TaskRunning, nil),
		newTask("t3", "wi-2", "triage", v1alpha1.TaskPending, nil),
		newRun("r1", "t1"), newRun("r2", "t2"), newRun("r3", "t1"),
		newWorkItem("wi-1", nil, v1alpha1.Link{Provider: "gh", Ref: "o/r#1"}, v1alpha1.Link{Provider: "gh", Ref: "o/r#2"}),
		newWorkItem("wi-2", nil, v1alpha1.Link{Provider: "gh", Ref: "o/r#3"}),
		newEffect("e1", "t1", "k1"), newEffect("e2", "t1", "k2"), newEffect("e3", "t2", "k1"),
	)
	for _, tc := range []struct {
		kind   string
		fields map[string]string
		want   []string
	}{
		{v1alpha1.KindTask, map[string]string{store.FieldTaskPhase: "Pending"}, []string{"t1", "t3"}},
		{v1alpha1.KindTask, map[string]string{store.FieldTaskQueue: "triage"}, []string{"t1", "t3"}},
		{v1alpha1.KindTask, map[string]string{store.FieldTaskWorkItem: "wi-1"}, []string{"t1", "t2"}},
		{v1alpha1.KindTask, map[string]string{store.FieldTaskWorkItem: "wi-1", store.FieldTaskPhase: "Pending"}, []string{"t1"}},
		{v1alpha1.KindTask, map[string]string{store.FieldTaskPhase: "Succeeded"}, []string{}},
		{v1alpha1.KindRun, map[string]string{store.FieldRunTask: "t1"}, []string{"r1", "r3"}},
		{v1alpha1.KindWorkItem, map[string]string{store.FieldWorkItemLink: store.LinkFieldValue("gh", "o/r#2")}, []string{"wi-1"}},
		{v1alpha1.KindWorkItem, map[string]string{store.FieldWorkItemLink: store.LinkFieldValue("gh", "o/r#3")}, []string{"wi-2"}},
		{v1alpha1.KindEffect, map[string]string{store.FieldEffectIdemKey: "k1"}, []string{"e1", "e3"}},
		{v1alpha1.KindEffect, map[string]string{store.FieldEffectTask: "t1"}, []string{"e1", "e2"}},
	} {
		t.Run(fmt.Sprint(tc.kind, tc.fields), func(t *testing.T) {
			got := listNames(t, h, tc.kind, store.ListOptions{Field: tc.fields})
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	// Indexes follow updates and deletes.
	var t1 v1alpha1.Task
	if err := h.s.Get(h.ctx, store.Key{Kind: v1alpha1.KindTask, Name: "t1"}, &t1); err != nil {
		t.Fatal(err)
	}
	t1.Status.Phase = v1alpha1.TaskLeased
	h.mustUpdate(t, &t1)
	if got := listNames(t, h, v1alpha1.KindTask, store.ListOptions{Field: map[string]string{store.FieldTaskPhase: "Pending"}}); !slices.Equal(got, []string{"t3"}) {
		t.Fatalf("Pending after update = %v", got)
	}
	if got := listNames(t, h, v1alpha1.KindTask, store.ListOptions{Field: map[string]string{store.FieldTaskPhase: "Leased"}}); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("Leased after update = %v", got)
	}
	if err := h.s.Delete(h.ctx, store.Key{Kind: v1alpha1.KindRun, Name: "r1"}); err != nil {
		t.Fatal(err)
	}
	if got := listNames(t, h, v1alpha1.KindRun, store.ListOptions{Field: map[string]string{store.FieldRunTask: "t1"}}); !slices.Equal(got, []string{"r3"}) {
		t.Fatalf("runs of t1 after delete = %v", got)
	}

	var out []v1alpha1.Task
	_, err := h.s.List(h.ctx, v1alpha1.KindTask, store.ListOptions{Field: map[string]string{"spec.pool": "x"}}, &out)
	wantErr(t, err, store.ErrInvalid)
	_, err = h.s.List(h.ctx, v1alpha1.KindAgent, store.ListOptions{Field: map[string]string{store.FieldTaskPhase: "x"}}, &out)
	wantErr(t, err, store.ErrInvalid)
}

func testListPagination(t *testing.T, h *harness) {
	for _, n := range []string{"e", "a", "d", "b", "c"} {
		h.mustCreate(t, newRun(n, "t"))
	}
	page := func(cursor string, limit int) ([]string, string) {
		t.Helper()
		var out []*v1alpha1.Run
		meta, err := h.s.List(h.ctx, v1alpha1.KindRun, store.ListOptions{Cursor: cursor, Limit: limit}, &out)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		names := make([]string, len(out))
		for i, r := range out {
			names[i] = r.Metadata.Name
		}
		return names, meta.NextCursor
	}

	got, next := page("", 2)
	if !slices.Equal(got, []string{"a", "b"}) || next == "" {
		t.Fatalf("page 1 = %v next %q", got, next)
	}
	// An object added before the cursor doesn't shift the next page (stable, keyset order).
	h.mustCreate(t, newRun("aa", "t"))
	got, next = page(next, 2)
	if !slices.Equal(got, []string{"c", "d"}) || next == "" {
		t.Fatalf("page 2 = %v next %q", got, next)
	}
	got, next = page(next, 2)
	if !slices.Equal(got, []string{"e"}) || next != "" {
		t.Fatalf("page 3 = %v next %q", got, next)
	}
	// An exact fit ends without a cursor.
	got, next = page("", 6)
	if len(got) != 6 || next != "" {
		t.Fatalf("exact page = %v next %q", got, next)
	}
	got, _ = page("", 0)
	if !slices.Equal(got, []string{"a", "aa", "b", "c", "d", "e"}) {
		t.Fatalf("no limit = %v", got)
	}

	var out []v1alpha1.Run
	_, err := h.s.List(h.ctx, v1alpha1.KindRun, store.ListOptions{Cursor: "%%%"}, &out)
	wantErr(t, err, store.ErrInvalid)
	_, cur := page("", 1)
	_, err = h.s.List(h.ctx, v1alpha1.KindTask, store.ListOptions{Cursor: cur}, &out)
	wantErr(t, err, store.ErrInvalid)
	_, err = h.s.List(h.ctx, v1alpha1.KindRun, store.ListOptions{Limit: -1}, &out)
	wantErr(t, err, store.ErrInvalid)
}

func testListOutTypes(t *testing.T, h *harness) {
	h.mustCreate(t, newRun("r1", "t1"), newRun("r2", "t2"))
	var values []v1alpha1.Run
	if _, err := h.s.List(h.ctx, v1alpha1.KindRun, store.ListOptions{}, &values); err != nil || len(values) != 2 || values[1].Spec.Task != "t2" {
		t.Fatalf("[]Run: %v %+v", err, values)
	}
	var ptrs []*v1alpha1.Run
	if _, err := h.s.List(h.ctx, v1alpha1.KindRun, store.ListOptions{}, &ptrs); err != nil || len(ptrs) != 2 || ptrs[0].Spec.Task != "t1" {
		t.Fatalf("[]*Run: %v %+v", err, ptrs)
	}
	var objs []store.Object
	if _, err := h.s.List(h.ctx, v1alpha1.KindRun, store.ListOptions{}, &objs); err != nil || len(objs) != 2 {
		t.Fatalf("[]Object: %v %+v", err, objs)
	}
	if _, ok := objs[0].(*v1alpha1.Run); !ok {
		t.Fatalf("[]Object holds %T", objs[0])
	}
	// The slice is replaced, not appended to.
	if _, err := h.s.List(h.ctx, v1alpha1.KindRun, store.ListOptions{Limit: 1}, &ptrs); err != nil || len(ptrs) != 1 {
		t.Fatalf("reused slice: %v len %d", err, len(ptrs))
	}
	// Nothing to list is an empty slice.
	var none []v1alpha1.Agent
	if _, err := h.s.List(h.ctx, v1alpha1.KindAgent, store.ListOptions{}, &none); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("empty list: %v %#v", err, none)
	}

	var wrong []v1alpha1.Task
	_, err := h.s.List(h.ctx, v1alpha1.KindRun, store.ListOptions{}, &wrong)
	wantErr(t, err, store.ErrInvalid)
	_, err = h.s.List(h.ctx, v1alpha1.KindRun, store.ListOptions{}, values)
	wantErr(t, err, store.ErrInvalid)
	_, err = h.s.List(h.ctx, "", store.ListOptions{}, &values)
	wantErr(t, err, store.ErrInvalid)
}

func testListResourceVersion(t *testing.T, h *harness) {
	r := newRun("r1", "t1")
	h.mustCreate(t, r)
	var out []v1alpha1.Run
	meta, err := h.s.List(h.ctx, v1alpha1.KindAgent, store.ListOptions{}, &out)
	if err != nil {
		t.Fatal(err)
	}
	// The list version is the store's, whatever the kind.
	if meta.ResourceVersion != r.Metadata.ResourceVersion {
		t.Fatalf("list rv %q, want the latest write's %q", meta.ResourceVersion, r.Metadata.ResourceVersion)
	}
}
