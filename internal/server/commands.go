package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/apiserver/gen"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/store"
)

// Command event types.
const (
	evPause      v1alpha1.EventType = "factory.command.pause"
	evResume     v1alpha1.EventType = "factory.command.resume"
	evCancel     v1alpha1.EventType = "factory.command.cancel"
	evRedispatch v1alpha1.EventType = "factory.command.redispatch"
)

// InjectedEventPrefix starts the id of a dispatched event that came without one.
const InjectedEventPrefix = "inject/"

// PostCommand implements gen.StrictServerInterface (api-server.md §Commands). Commands about a
// work item become factory.command.* events on it; dispatch stores the injected event.
func (s *Server) PostCommand(ctx context.Context, req gen.PostCommandRequestObject) (gen.PostCommandResponseObject, error) {
	cmd := req.Body
	key := requestIDFrom(ctx)
	actor := &v1alpha1.Actor{ID: principal(ctx).ActorID()}
	var (
		event string
		err   error
	)
	switch cmd.Type {
	case gen.CommandTypePause, gen.CommandTypeResume:
		event, err = s.pause(ctx, cmd, key, actor)
	case gen.CommandTypeCancel:
		event, err = s.cancel(ctx, cmd, key, actor)
	case gen.CommandTypeRedispatch:
		event, err = s.redispatch(ctx, cmd, key, actor)
	case gen.CommandTypeDispatch:
		event, err = s.dispatch(ctx, cmd, key)
	case gen.CommandTypeResyncCatalog:
		if s.d.Resync == nil {
			return nil, problemf(http.StatusServiceUnavailable, CodeUnavailable, "catalog resync is not available")
		}
		s.d.Resync.Resync()
	default:
		return nil, &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: fmt.Sprintf("unknown command %q", cmd.Type),
			fields: []gen.FieldError{{Field: "type", Message: "pause, resume, cancel, redispatch, dispatch or resync-catalog"}}}
	}
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(ctx, "command accepted", "type", cmd.Type, "actor", actor.ID, "event", event)
	out := gen.PostCommand202JSONResponse{Type: string(cmd.Type)}
	if event != "" {
		out.Event = &event
	}
	return out, nil
}

func fieldError(field, msg string) *apiError {
	return &apiError{status: http.StatusBadRequest, code: CodeInvalid, detail: field + ": " + msg,
		fields: []gen.FieldError{{Field: field, Message: msg}}}
}

// pause and resume set or clear a kill switch; on a work item they also emit
// factory.command.pause/resume (factory events always belong to a work item, so the other
// scopes emit none).
func (s *Server) pause(ctx context.Context, cmd *gen.Command, key string, actor *v1alpha1.Actor) (string, error) {
	t := cmd.Target
	if t == nil || t.Scope == nil {
		return "", fieldError("target.scope", "required: global, provider, queue or workitem")
	}
	scope := string(*t.Scope)
	if !t.Scope.Valid() {
		return "", fieldError("target.scope", "global, provider, queue or workitem")
	}
	name := ""
	if t.Name != nil {
		name = *t.Name
	}
	if scope == controllers.ScopeGlobal {
		name = ""
	} else if name == "" {
		return "", fieldError("target.name", "required for scope "+scope)
	}
	if err := s.checkScopeTarget(ctx, scope, name); err != nil {
		return "", err
	}
	var err error
	typ := evPause
	if cmd.Type == gen.CommandTypePause {
		err = s.d.KillSwitches.Pause(ctx, scope, name, actor.ID, s.clk.Now())
	} else {
		typ = evResume
		err = s.d.KillSwitches.Resume(ctx, scope, name)
	}
	if err != nil {
		return "", err
	}
	if scope != controllers.ScopeWorkItem {
		return "", nil
	}
	ev, err := s.d.Factory.EmitCommand(ctx, typ, name, key, map[string]any{"type": string(cmd.Type), "scope": scope, "name": name}, actor)
	if err != nil {
		return "", err
	}
	return ev.ID, nil
}

// checkScopeTarget checks that a pause target exists (a catalog provider or queue, a stored
// work item).
func (s *Server) checkScopeTarget(ctx context.Context, scope, name string) error {
	switch scope {
	case controllers.ScopeProvider, controllers.ScopeQueue:
		cat, err := s.catalog()
		if err != nil {
			return err
		}
		ok := cat.Providers[name] != nil
		if scope == controllers.ScopeQueue {
			ok = cat.Queues[name] != nil
		}
		if !ok {
			return problemf(http.StatusNotFound, CodeNotFound, "no %s %q in the catalog", scope, name)
		}
	case controllers.ScopeWorkItem:
		var wi v1alpha1.WorkItem
		if err := s.d.Store.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: name}, &wi); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return problemf(http.StatusNotFound, CodeNotFound, "no work item %q", name)
			}
			return err
		}
	}
	return nil
}

func commandTask(cmd *gen.Command) (string, error) {
	if cmd.Target == nil || cmd.Target.Task == nil || *cmd.Target.Task == "" {
		return "", fieldError("target.task", "required")
	}
	return *cmd.Target.Task, nil
}

// cancel moves an active task to Cancelling (the run controller cancels one without an agent at
// once; an agent learns it from its next heartbeat) and emits factory.command.cancel.
func (s *Server) cancel(ctx context.Context, cmd *gen.Command, key string, actor *v1alpha1.Actor) (string, error) {
	id, err := commandTask(cmd)
	if err != nil {
		return "", err
	}
	var t *v1alpha1.Task
	for range maxCAS {
		if t, err = s.getTask(ctx, id); err != nil {
			return "", err
		}
		phase := v1alpha1.TaskPending
		if t.Status != nil {
			phase = t.Status.Phase
		}
		switch phase {
		case v1alpha1.TaskCancelling:
		case v1alpha1.TaskPending, v1alpha1.TaskLeased, v1alpha1.TaskRunning:
			if t.Status == nil {
				t.Status = &v1alpha1.TaskStatus{}
			}
			t.Status.Phase, t.Status.Reason = v1alpha1.TaskCancelling, controllers.ReasonCancelled
			err = s.d.Store.Update(ctx, t)
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			if err != nil {
				return "", fmt.Errorf("cancel task %s: %w", id, err)
			}
		default:
			return "", problemf(http.StatusConflict, CodeTaskNotActive, "task %s is %s", id, phase)
		}
		break
	}
	if err != nil {
		return "", fmt.Errorf("cancel task %s: %w", id, err)
	}
	ev, err := s.d.Factory.EmitCommand(ctx, evCancel, t.Metadata.WorkItem, key, map[string]any{"type": "cancel", "task": id}, actor)
	if err != nil {
		return "", err
	}
	return ev.ID, nil
}

// redispatch clones a finished task, with an optional harness override, as a new Pending task
// (and its prompt), and emits factory.command.redispatch.
func (s *Server) redispatch(ctx context.Context, cmd *gen.Command, key string, actor *v1alpha1.Actor) (string, error) {
	id, err := commandTask(cmd)
	if err != nil {
		return "", err
	}
	var override *v1alpha1.HarnessRequest
	if cmd.Args != nil && cmd.Args.Harness != nil {
		if cmd.Args.Harness.Name == "" {
			return "", fieldError("args.harness.name", "required")
		}
		h := *cmd.Args.Harness
		override = &h
	}
	orig, err := s.getTask(ctx, id)
	if err != nil {
		return "", err
	}
	if orig.Status != nil {
		switch orig.Status.Phase {
		case v1alpha1.TaskPending, v1alpha1.TaskLeased, v1alpha1.TaskRunning, v1alpha1.TaskCancelling:
			return "", problemf(http.StatusConflict, "task-active", "task %s is %s: cancel it first", id, orig.Status.Phase)
		}
	}
	sum := sha256.Sum256([]byte("redispatch/" + id + "/" + key))
	newID := "tsk_" + hex.EncodeToString(sum[:10])
	ev, err := s.d.Factory.EmitCommand(ctx, evRedispatch, orig.Metadata.WorkItem, key,
		map[string]any{"type": "redispatch", "task": id, "newTask": newID}, actor)
	if err != nil {
		return "", err
	}

	var clone v1alpha1.Task
	b, err := json.Marshal(orig)
	if err == nil {
		err = json.Unmarshal(b, &clone)
	}
	if err != nil {
		return "", fmt.Errorf("clone task %s: %w", id, err)
	}
	prov := &v1alpha1.Provenance{Event: ev.ID}
	if p := orig.Metadata.Provenance; p != nil {
		prov.RuleSet, prov.Rule, prov.CatalogSHA = p.RuleSet, p.Rule, p.CatalogSHA
	}
	if cat := s.d.Catalog.Current(); cat != nil {
		prov.CatalogSHA = cat.SHA
	}
	clone.Metadata = v1alpha1.TaskMeta{ID: newID, WorkItem: orig.Metadata.WorkItem, Provenance: prov, Labels: clone.Metadata.Labels}
	if clone.Spec.Harness == nil {
		clone.Spec.Harness = &v1alpha1.TaskHarness{}
	}
	clone.Spec.Harness.Resolved = nil
	if override != nil {
		clone.Spec.Harness.Request = override
	}
	if clone.Spec.Prompt != nil {
		clone.Spec.Prompt.Ref = "api://tasks/" + newID + "/prompt"
	}
	clone.Status = &v1alpha1.TaskStatus{Phase: v1alpha1.TaskPending}

	var prompt controllers.TaskPrompt
	switch err := s.d.Store.Get(ctx, store.Key{Kind: controllers.KindTaskPrompt, Name: id}, &prompt); {
	case err == nil:
		np := &controllers.TaskPrompt{
			TypeMeta: v1alpha1.TypeMeta{Kind: controllers.KindTaskPrompt},
			Metadata: v1alpha1.ObjectMeta{Name: newID}, Text: prompt.Text, Digest: prompt.Digest,
		}
		if err := s.d.Store.Create(ctx, np); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return "", fmt.Errorf("copy prompt of %s: %w", id, err)
		}
	case !errors.Is(err, store.ErrNotFound):
		return "", fmt.Errorf("get prompt of %s: %w", id, err)
	}
	if err := s.d.Store.Create(ctx, &clone); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return "", fmt.Errorf("create task %s: %w", newID, err)
	}
	s.log.InfoContext(ctx, "task redispatched", "task", id, "newTask", newID)
	return ev.ID, nil
}

// dispatch validates and stores an injected event: its source must be a catalog provider
// (whose type emits the event type) or the factory (a factory.* type on a work item).
func (s *Server) dispatch(ctx context.Context, cmd *gen.Command, key string) (string, error) {
	if cmd.Args == nil || cmd.Args.Event == nil {
		return "", fieldError("args.event", "required")
	}
	ev := *cmd.Args.Event
	if err := checkEvent(ev, false); err != nil {
		return "", err
	}
	if ev.Source == v1alpha1.FactoryProvider {
		if !ev.Type.IsFactory() {
			return "", fieldError("event.type", "the factory emits only factory.* events")
		}
		if ev.WorkItem == "" {
			return "", fieldError("event.workItem", "required for factory events")
		}
	} else {
		cat, err := s.catalog()
		if err != nil {
			return "", err
		}
		p := cat.Providers[ev.Source]
		if p == nil {
			return "", fieldError("event.source", fmt.Sprintf("%q is not a configured provider or factory", ev.Source))
		}
		if !ev.Type.EmittedBy(p.Spec.Type) {
			return "", fieldError("event.type", fmt.Sprintf("a %s provider doesn't emit %s", p.Spec.Type, ev.Type))
		}
	}
	ev.Kind = v1alpha1.KindEvent
	if ev.ID == "" {
		ev.ID = InjectedEventPrefix + key
	}
	if strings.HasPrefix(ev.ID, controllers.FactoryEventPrefix) && ev.Source != v1alpha1.FactoryProvider {
		return "", fieldError("event.id", "factory/ ids belong to the factory provider")
	}
	if ev.Time.IsZero() {
		ev.Time = s.clk.Now()
	}
	if ev.Data.Provider == "" {
		ev.Data.Provider = ev.Source
	}
	if err := s.d.Store.Create(ctx, &ev); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return "", problemf(http.StatusConflict, CodeConflict, "event %q exists", ev.ID)
		}
		return "", fmt.Errorf("store event: %w", err)
	}
	return ev.ID, nil
}
