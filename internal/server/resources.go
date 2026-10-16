package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/apiserver/gen"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/scheduler"
	"github.com/compliance-framework/factory/internal/store"
)

// listParams are a list endpoint's query: cursor, limit and label selector.
type listParams struct {
	cursor   string
	limit    int
	selector *v1alpha1.LabelSelector
}

func parseList(cursor *string, limit *int, selector *string) (listParams, error) {
	p := listParams{limit: DefaultPageSize}
	if cursor != nil {
		p.cursor = *cursor
	}
	if limit != nil {
		if *limit < 1 || *limit > MaxPageSize {
			return p, &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: fmt.Sprintf("limit must be 1 to %d", MaxPageSize),
				fields: []gen.FieldError{{Field: "limit", Message: fmt.Sprintf("1 to %d", MaxPageSize)}}}
		}
		p.limit = *limit
	}
	if selector != nil {
		sel, err := parseSelector(*selector)
		if err != nil {
			return p, &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: err.Error(),
				fields: []gen.FieldError{{Field: "labelSelector", Message: err.Error()}}}
		}
		p.selector = sel
	}
	return p, nil
}

// page sorts items by sortKey (ascending, unique: it ends with the name) and returns the page
// after the cursor, and the next cursor when more items follow. The cursor is the last sort
// key returned (opaque, via store.EncodeCursor), so it stays valid while items come and go.
func page[T any](kind string, items []T, sortKey func(T) string, p listParams) ([]T, *string, error) {
	after, err := store.DecodeCursor(kind, p.cursor)
	if err != nil {
		return nil, nil, &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: "malformed cursor",
			fields: []gen.FieldError{{Field: "cursor", Message: "not a cursor of this list"}}}
	}
	type keyed struct {
		k string
		v T
	}
	ks := make([]keyed, 0, len(items))
	for _, it := range items {
		if k := sortKey(it); after == "" || k > after {
			ks = append(ks, keyed{k, it})
		}
	}
	slices.SortFunc(ks, func(a, b keyed) int { return cmp.Compare(a.k, b.k) })
	out := make([]T, 0, min(len(ks), p.limit))
	for i := 0; i < len(ks) && i < p.limit; i++ {
		out = append(out, ks[i].v)
	}
	if len(ks) > p.limit {
		c := store.EncodeCursor(kind, ks[p.limit-1].k)
		return out, &c, nil
	}
	return out, nil, nil
}

// descending turns n into a key that sorts newest (largest) first.
func descending(n uint64) string { return fmt.Sprintf("%020d", math.MaxUint64-n) }

func rv(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

// ListWorkItems implements gen.StrictServerInterface: most recently updated first (the
// resourceVersion is the store's global write counter).
func (s *Server) ListWorkItems(ctx context.Context, req gen.ListWorkItemsRequestObject) (gen.ListWorkItemsResponseObject, error) {
	p, err := parseList(req.Params.Cursor, req.Params.Limit, req.Params.LabelSelector)
	if err != nil {
		return nil, err
	}
	var items []v1alpha1.WorkItem
	if _, err := s.d.Store.List(ctx, v1alpha1.KindWorkItem, store.ListOptions{LabelSelector: p.selector}, &items); err != nil {
		return nil, err
	}
	out, next, err := page(v1alpha1.KindWorkItem, items, func(w v1alpha1.WorkItem) string {
		return descending(rv(w.Metadata.ResourceVersion)) + "/" + w.Metadata.Name
	}, p)
	if err != nil {
		return nil, err
	}
	return gen.ListWorkItems200JSONResponse{Items: out, NextCursor: next}, nil
}

// ListTasks implements gen.StrictServerInterface: newest first.
func (s *Server) ListTasks(ctx context.Context, req gen.ListTasksRequestObject) (gen.ListTasksResponseObject, error) {
	p, err := parseList(req.Params.Cursor, req.Params.Limit, req.Params.LabelSelector)
	if err != nil {
		return nil, err
	}
	var items []v1alpha1.Task
	if _, err := s.d.Store.List(ctx, v1alpha1.KindTask, store.ListOptions{LabelSelector: p.selector}, &items); err != nil {
		return nil, err
	}
	out, next, err := page(v1alpha1.KindTask, items, func(t v1alpha1.Task) string {
		return descending(uint64(max(t.Metadata.CreatedAt.UnixNano(), 0))) + "/" + t.Metadata.ID
	}, p)
	if err != nil {
		return nil, err
	}
	return gen.ListTasks200JSONResponse{Items: out, NextCursor: next}, nil
}

// ListAgents implements gen.StrictServerInterface: by name.
func (s *Server) ListAgents(ctx context.Context, req gen.ListAgentsRequestObject) (gen.ListAgentsResponseObject, error) {
	p, err := parseList(req.Params.Cursor, req.Params.Limit, req.Params.LabelSelector)
	if err != nil {
		return nil, err
	}
	var items []v1alpha1.Agent
	if _, err := s.d.Store.List(ctx, v1alpha1.KindAgent, store.ListOptions{LabelSelector: p.selector}, &items); err != nil {
		return nil, err
	}
	out, next, err := page(v1alpha1.KindAgent, items, func(a v1alpha1.Agent) string { return a.Metadata.Name }, p)
	if err != nil {
		return nil, err
	}
	return gen.ListAgents200JSONResponse{Items: out, NextCursor: next}, nil
}

// catalogObjects returns the values of a catalog map whose labels match the selector.
func catalogObjects[T any](m map[string]*T, labels func(*T) map[string]string, sel *v1alpha1.LabelSelector) []*T {
	out := make([]*T, 0, len(m))
	for _, v := range m {
		if sel.Matches(labels(v)) {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) catalog() (*catalog.Catalog, error) {
	c := s.d.Catalog.Current()
	if c == nil {
		return nil, problemf(http.StatusServiceUnavailable, CodeCatalogNotLoaded, "the catalog is not loaded yet")
	}
	return c, nil
}

// ListQueues implements gen.StrictServerInterface: each queue with its depth, running tasks
// and pending tasks with the reason each waits.
func (s *Server) ListQueues(ctx context.Context, req gen.ListQueuesRequestObject) (gen.ListQueuesResponseObject, error) {
	p, err := parseList(req.Params.Cursor, req.Params.Limit, req.Params.LabelSelector)
	if err != nil {
		return nil, err
	}
	cat, err := s.catalog()
	if err != nil {
		return nil, err
	}
	queues := catalogObjects(cat.Queues, func(q *v1alpha1.Queue) map[string]string { return q.Metadata.Labels }, p.selector)
	queues, next, err := page(v1alpha1.KindQueue, queues, func(q *v1alpha1.Queue) string { return q.Metadata.Name }, p)
	if err != nil {
		return nil, err
	}
	out := gen.ListQueues200JSONResponse{Items: make([]gen.QueueView, 0, len(queues)), NextCursor: next}
	for _, q := range queues {
		var tasks []*v1alpha1.Task
		if _, err := s.d.Store.List(ctx, v1alpha1.KindTask, store.ListOptions{
			Field: map[string]string{store.FieldTaskQueue: q.Metadata.Name},
		}, &tasks); err != nil {
			return nil, err
		}
		v := gen.QueueView{Queue: *q, Pending: []gen.PendingTask{}}
		slices.SortFunc(tasks, func(a, b *v1alpha1.Task) int {
			return cmp.Or(a.Metadata.CreatedAt.Compare(b.Metadata.CreatedAt), cmp.Compare(a.Metadata.ID, b.Metadata.ID))
		})
		for _, t := range tasks {
			phase := v1alpha1.TaskPending
			if t.Status != nil {
				phase = t.Status.Phase
			}
			switch phase {
			case v1alpha1.TaskPending:
				pt := gen.PendingTask{Task: t.Metadata.ID}
				if t.Status != nil && t.Status.Reason != "" {
					r := t.Status.Reason
					pt.Reason = &r
				}
				if !t.Metadata.CreatedAt.IsZero() {
					since := t.Metadata.CreatedAt
					pt.Since = &since
				}
				v.Pending = append(v.Pending, pt)
			case v1alpha1.TaskLeased, v1alpha1.TaskRunning, v1alpha1.TaskCancelling:
				v.Running++
			}
		}
		v.Depth = len(v.Pending)
		out.Items = append(out.Items, v)
	}
	return out, nil
}

// ListPools implements gen.StrictServerInterface: each pool with its registered agents.
func (s *Server) ListPools(ctx context.Context, req gen.ListPoolsRequestObject) (gen.ListPoolsResponseObject, error) {
	p, err := parseList(req.Params.Cursor, req.Params.Limit, req.Params.LabelSelector)
	if err != nil {
		return nil, err
	}
	cat, err := s.catalog()
	if err != nil {
		return nil, err
	}
	pools := catalogObjects(cat.Pools, func(p *v1alpha1.Pool) map[string]string { return p.Metadata.Labels }, p.selector)
	pools, next, err := page(v1alpha1.KindPool, pools, func(p *v1alpha1.Pool) string { return p.Metadata.Name }, p)
	if err != nil {
		return nil, err
	}
	out := gen.ListPools200JSONResponse{Items: make([]gen.PoolView, 0, len(pools)), NextCursor: next}
	for _, pool := range pools {
		var agents []*v1alpha1.Agent
		if _, err := s.d.Store.List(ctx, v1alpha1.KindAgent, store.ListOptions{
			LabelSelector: &v1alpha1.LabelSelector{MatchLabels: map[string]string{scheduler.LabelPool: pool.Metadata.Name}},
		}, &agents); err != nil {
			return nil, err
		}
		v := gen.PoolView{Pool: *pool, Agents: make([]string, 0, len(agents))}
		for _, a := range agents {
			v.Agents = append(v.Agents, a.Metadata.Name)
		}
		out.Items = append(out.Items, v)
	}
	return out, nil
}

// ListProviders implements gen.StrictServerInterface: each provider with its connection's
// health (unknown when not connected).
func (s *Server) ListProviders(_ context.Context, req gen.ListProvidersRequestObject) (gen.ListProvidersResponseObject, error) {
	p, err := parseList(req.Params.Cursor, req.Params.Limit, req.Params.LabelSelector)
	if err != nil {
		return nil, err
	}
	cat, err := s.catalog()
	if err != nil {
		return nil, err
	}
	provs := catalogObjects(cat.Providers, func(p *v1alpha1.Provider) map[string]string { return p.Metadata.Labels }, p.selector)
	provs, next, err := page(v1alpha1.KindProvider, provs, func(p *v1alpha1.Provider) string { return p.Metadata.Name }, p)
	if err != nil {
		return nil, err
	}
	out := gen.ListProviders200JSONResponse{Items: make([]gen.ProviderView, 0, len(provs)), NextCursor: next}
	for _, pr := range provs {
		h := provider.HealthStatus{Status: provider.HealthUnknown, Message: "not connected"}
		if conn, ok := s.d.Conns.Conn(pr.Metadata.Name); ok {
			h = conn.Health()
		}
		ph := gen.ProviderHealth{Status: gen.ProviderHealthStatus(h.Status)}
		if !ph.Status.Valid() {
			ph.Status = gen.ProviderHealthStatusUnknown
		}
		if h.Message != "" {
			m := h.Message
			ph.Message = &m
		}
		if !h.CheckedAt.IsZero() {
			t := h.CheckedAt
			ph.CheckedAt = &t
		}
		out.Items = append(out.Items, gen.ProviderView{Provider: *pr, Health: ph})
	}
	return out, nil
}

// GetWorkItem implements gen.StrictServerInterface: the work item and its timeline (events,
// tasks, runs, effects).
func (s *Server) GetWorkItem(ctx context.Context, req gen.GetWorkItemRequestObject) (gen.GetWorkItemResponseObject, error) {
	var wi v1alpha1.WorkItem
	if err := s.d.Store.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: req.Id}, &wi); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, problemf(http.StatusNotFound, CodeNotFound, "no work item %q", req.Id)
		}
		return nil, err
	}
	out := gen.WorkItemDetail{WorkItem: wi, Events: []gen.Event{}, Tasks: []gen.Task{}, Runs: []gen.Run{}, Effects: []gen.Effect{}}

	ids := map[string]bool{}
	var marks []controllers.EventStatus
	if _, err := s.d.Store.List(ctx, controllers.KindEventStatus, store.ListOptions{}, &marks); err != nil {
		return nil, err
	}
	for _, m := range marks {
		if m.WorkItem == req.Id {
			ids[m.Metadata.Name] = true
		}
	}
	if wi.Status != nil {
		for _, d := range wi.Status.Decisions {
			if d.Event != "" {
				ids[d.Event] = true
			}
		}
	}
	for id := range ids {
		var ev v1alpha1.Event
		switch err := s.d.Store.Get(ctx, store.Key{Kind: v1alpha1.KindEvent, Name: id}, &ev); {
		case errors.Is(err, store.ErrNotFound):
			continue // aged out
		case err != nil:
			return nil, err
		}
		out.Events = append(out.Events, ev)
	}
	slices.SortFunc(out.Events, func(a, b gen.Event) int { return cmp.Or(a.Time.Compare(b.Time), cmp.Compare(a.ID, b.ID)) })

	var tasks []v1alpha1.Task
	if _, err := s.d.Store.List(ctx, v1alpha1.KindTask, store.ListOptions{
		Field: map[string]string{store.FieldTaskWorkItem: req.Id},
	}, &tasks); err != nil {
		return nil, err
	}
	slices.SortFunc(tasks, func(a, b v1alpha1.Task) int {
		return cmp.Or(a.Metadata.CreatedAt.Compare(b.Metadata.CreatedAt), cmp.Compare(a.Metadata.ID, b.Metadata.ID))
	})
	out.Tasks = append(out.Tasks, tasks...)
	for _, t := range tasks {
		runs, err := s.taskRuns(ctx, t.Metadata.ID)
		if err != nil {
			return nil, err
		}
		for _, r := range runs {
			out.Runs = append(out.Runs, *r)
		}
		var effects []v1alpha1.Effect
		if _, err := s.d.Store.List(ctx, v1alpha1.KindEffect, store.ListOptions{
			Field: map[string]string{store.FieldEffectTask: t.Metadata.ID},
		}, &effects); err != nil {
			return nil, err
		}
		out.Effects = append(out.Effects, effects...)
	}
	slices.SortFunc(out.Runs, func(a, b gen.Run) int {
		return cmp.Or(a.Metadata.CreatedAt.Compare(b.Metadata.CreatedAt), cmp.Compare(a.Metadata.Name, b.Metadata.Name))
	})
	slices.SortFunc(out.Effects, func(a, b gen.Effect) int { return effectAt(a).Compare(effectAt(b)) })
	return gen.GetWorkItem200JSONResponse(out), nil
}

func effectAt(e v1alpha1.Effect) time.Time {
	if e.Status != nil && !e.Status.At.IsZero() {
		return e.Status.At
	}
	return e.Metadata.CreatedAt
}

// GetRun implements gen.StrictServerInterface: the run and the effects it made.
func (s *Server) GetRun(ctx context.Context, req gen.GetRunRequestObject) (gen.GetRunResponseObject, error) {
	run, _, err := s.runTask(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	var effects []v1alpha1.Effect
	if _, err := s.d.Store.List(ctx, v1alpha1.KindEffect, store.ListOptions{
		LabelSelector: &v1alpha1.LabelSelector{MatchLabels: map[string]string{LabelEffectRun: run.Metadata.Name}},
	}, &effects); err != nil {
		return nil, err
	}
	slices.SortFunc(effects, func(a, b v1alpha1.Effect) int { return effectAt(a).Compare(effectAt(b)) })
	if effects == nil {
		effects = []v1alpha1.Effect{}
	}
	return gen.GetRun200JSONResponse{Run: *run, Effects: effects}, nil
}
