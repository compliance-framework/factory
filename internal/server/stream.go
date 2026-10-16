package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/apiserver/gen"
	"github.com/compliance-framework/factory/internal/store"
)

// SSE event names.
const (
	SSEProgress        = "progress"
	SSEWorkItemUpdated = "workitem.updated"
	SSETaskUpdated     = "task.updated"
	SSEAgentUpdated    = "agent.updated"
)

var topicRE = regexp.MustCompile(`^(runs/[^/,]+|workitems(/[^/,]+)?|tasks|agents)$`)

// StreamProgress is the data of a progress event.
type StreamProgress struct {
	Run   string                 `json:"run"`
	Task  string                 `json:"task,omitempty"`
	Event v1alpha1.ProgressEvent `json:"event"`
}

// topicSet is what one subscriber asked for.
type topicSet struct {
	allWorkItems bool
	workItems    map[string]bool
	tasks        bool
	agents       bool
	runs         map[string]bool
}

func parseTopics(topics []string) (topicSet, error) {
	ts := topicSet{workItems: map[string]bool{}, runs: map[string]bool{}}
	var all []string
	for _, t := range topics {
		all = append(all, strings.Split(t, ",")...)
	}
	if len(all) == 0 {
		return ts, fieldError("topics", "at least one topic")
	}
	for _, t := range all {
		t = strings.TrimSpace(t)
		if !topicRE.MatchString(t) {
			return ts, fieldError("topics", fmt.Sprintf("%q is not runs/<id>, workitems, workitems/<id>, tasks or agents", t))
		}
		switch {
		case t == "workitems":
			ts.allWorkItems = true
		case t == "tasks":
			ts.tasks = true
		case t == "agents":
			ts.agents = true
		case strings.HasPrefix(t, "workitems/"):
			ts.workItems[strings.TrimPrefix(t, "workitems/")] = true
		case strings.HasPrefix(t, "runs/"):
			ts.runs[strings.TrimPrefix(t, "runs/")] = true
		}
	}
	return ts, nil
}

func (t topicSet) wantWorkItems() bool { return t.allWorkItems || len(t.workItems) > 0 }

// StreamEvents implements gen.StrictServerInterface: server-sent events from store watches.
// Each frame's id is the store version; with Last-Event-ID the watches replay from there (the
// store's history) and run progress is backfilled from the runs' ring buffers. A keep-alive
// comment is sent every KeepAlive; the stream ends when the client leaves or the server shuts
// down.
func (s *Server) StreamEvents(ctx context.Context, req gen.StreamEventsRequestObject) (gen.StreamEventsResponseObject, error) {
	ts, err := parseTopics(req.Params.Topics)
	if err != nil {
		return nil, err
	}
	if !s.beginStream() {
		return nil, problemf(http.StatusServiceUnavailable, CodeUnavailable, "the server is shutting down")
	}
	started := false
	defer func() {
		if !started {
			s.streams.Done()
		}
	}()
	if n := s.subscribers.Add(1); n > int64(s.opts.MaxSubscribers) {
		s.subscribers.Add(-1)
		return nil, &apiError{status: http.StatusServiceUnavailable, code: CodeTooManySubscribers,
			detail: fmt.Sprintf("at most %d streams", s.opts.MaxSubscribers), retryAfter: s.opts.KeepAlive}
	}
	since := ""
	if req.Params.LastEventID != nil {
		since = strings.TrimSpace(*req.Params.LastEventID)
		if _, err := strconv.ParseUint(since, 10, 64); since != "" && err != nil {
			s.subscribers.Add(-1)
			return nil, fieldError("Last-Event-ID", "must be a store version")
		}
	}
	sub := &subscription{s: s, topics: ts, since: since, runs: map[string]*runState{}}
	if err := sub.start(ctx); err != nil {
		sub.stop()
		s.subscribers.Add(-1)
		return nil, err
	}
	pr, pw := io.Pipe()
	started = true
	go func() {
		defer s.streams.Done()
		defer s.subscribers.Add(-1)
		defer sub.stop()
		err := sub.run(ctx, pw)
		_ = pw.CloseWithError(err) // nil: the response ends cleanly
	}()
	return gen.StreamEvents200TexteventStreamResponse{Body: pr}, nil
}

// runState is what a subscriber has sent of one run's progress.
type runState struct {
	task     string
	progress []v1alpha1.ProgressEvent
	version  uint64
}

// subscription is one SSE client: its watches and what it has sent.
type subscription struct {
	s      *Server
	topics topicSet
	since  string

	wi, tasks, agents, runsW *watch
	runs                     map[string]*runState
	backfill                 []frame
}

// watch is a store watch that resumes from the last version it delivered when it expires.
type watch struct {
	kind string
	w    store.Watcher
	last string
}

type frame struct {
	event, id string
	data      any
}

func (sub *subscription) open(ctx context.Context, kind, since string) (*watch, error) {
	w, err := sub.s.d.Store.Watch(ctx, kind, store.WatchOptions{SinceVersion: since})
	if errors.Is(err, store.ErrWatchExpired) && since != "" {
		// Too old to replay: start from now (the client missed some changes; the next
		// updates carry the full objects).
		w, err = sub.s.d.Store.Watch(ctx, kind, store.WatchOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("watch %s: %w", kind, err)
	}
	return &watch{kind: kind, w: w, last: since}, nil
}

// start opens the watches and, for runs, reads the progress baseline after the run watch
// started (so nothing falls between them).
func (sub *subscription) start(ctx context.Context) error {
	var err error
	if sub.topics.wantWorkItems() {
		if sub.wi, err = sub.open(ctx, v1alpha1.KindWorkItem, sub.since); err != nil {
			return err
		}
	}
	if sub.topics.tasks {
		if sub.tasks, err = sub.open(ctx, v1alpha1.KindTask, sub.since); err != nil {
			return err
		}
	}
	if sub.topics.agents {
		if sub.agents, err = sub.open(ctx, v1alpha1.KindAgent, sub.since); err != nil {
			return err
		}
	}
	if len(sub.topics.runs) == 0 {
		return nil
	}
	if sub.runsW, err = sub.open(ctx, v1alpha1.KindRun, ""); err != nil {
		return err
	}
	for id := range sub.topics.runs {
		st := &runState{}
		sub.runs[id] = st
		var r v1alpha1.Run
		switch err := sub.s.d.Store.Get(ctx, store.Key{Kind: v1alpha1.KindRun, Name: id}, &r); {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return fmt.Errorf("get run %s: %w", id, err)
		}
		st.task, st.version = r.Spec.Task, rv(r.Metadata.ResourceVersion)
		if r.Status != nil {
			st.progress = r.Status.Progress
		}
		if sub.since != "" { // a resume: backfill from the ring buffer
			for _, p := range st.progress {
				sub.backfill = append(sub.backfill, frame{SSEProgress, r.Metadata.ResourceVersion, StreamProgress{Run: id, Task: st.task, Event: p}})
			}
		}
	}
	return nil
}

func (sub *subscription) stop() {
	for _, w := range []*watch{sub.wi, sub.tasks, sub.agents, sub.runsW} {
		if w != nil {
			w.w.Stop()
		}
	}
}

func events(w *watch) <-chan store.WatchEvent {
	if w == nil {
		return nil
	}
	return w.w.Events()
}

// run writes frames to out until ctx ends (the client left), the server shuts down or a write
// fails.
func (sub *subscription) run(ctx context.Context, out io.Writer) error {
	if _, err := io.WriteString(out, ": connected\n\n"); err != nil {
		return nil
	}
	for _, f := range sub.backfill {
		if err := writeFrame(out, f); err != nil {
			return nil
		}
	}
	sub.backfill = nil
	ka := sub.s.opts.KeepAlive
	timer := sub.s.clk.NewTimer(ka)
	defer func() { timer.Stop() }()
	for {
		var (
			ev store.WatchEvent
			ok bool
			w  *watch
		)
		select {
		case <-ctx.Done():
			return nil
		case <-sub.s.done:
			return nil
		case <-timer.C():
			if _, err := io.WriteString(out, ": keep-alive\n\n"); err != nil {
				return nil
			}
			timer = sub.s.clk.NewTimer(ka)
			continue
		case ev, ok = <-events(sub.wi):
			w = sub.wi
		case ev, ok = <-events(sub.tasks):
			w = sub.tasks
		case ev, ok = <-events(sub.agents):
			w = sub.agents
		case ev, ok = <-events(sub.runsW):
			w = sub.runsW
		}
		if !ok {
			if err := sub.reopen(ctx, w); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			continue
		}
		w.last = ev.Version
		for _, f := range sub.frames(ev) {
			if err := writeFrame(out, f); err != nil {
				return nil
			}
		}
	}
}

// reopen replaces a watch that ended (expired: the client was slow) with one that replays
// from the last version it delivered.
func (sub *subscription) reopen(ctx context.Context, w *watch) error {
	if err := w.w.Err(); err != nil && !errors.Is(err, store.ErrWatchExpired) {
		return err
	}
	nw, err := sub.open(ctx, w.kind, w.last)
	if err != nil {
		return err
	}
	w.w = nw.w
	return nil
}

// frames are what one change sends to this subscriber.
func (sub *subscription) frames(ev store.WatchEvent) []frame {
	if ev.Type == store.Deleted {
		return nil
	}
	switch o := ev.Object.(type) {
	case *v1alpha1.WorkItem:
		if sub.topics.allWorkItems || sub.topics.workItems[o.Metadata.Name] {
			return []frame{{SSEWorkItemUpdated, ev.Version, o}}
		}
	case *v1alpha1.Task:
		return []frame{{SSETaskUpdated, ev.Version, o}}
	case *v1alpha1.Agent:
		return []frame{{SSEAgentUpdated, ev.Version, o}}
	case *v1alpha1.Run:
		st := sub.runs[o.Metadata.Name]
		v := rv(ev.Version)
		if st == nil || v <= st.version {
			return nil
		}
		var cur []v1alpha1.ProgressEvent
		if o.Status != nil {
			cur = o.Status.Progress
		}
		fresh := newProgress(st.progress, cur)
		st.progress, st.version, st.task = cur, v, o.Spec.Task
		out := make([]frame, 0, len(fresh))
		for _, p := range fresh {
			out = append(out, frame{SSEProgress, ev.Version, StreamProgress{Run: o.Metadata.Name, Task: o.Spec.Task, Event: p}})
		}
		return out
	}
	return nil
}

// newProgress is what cur (a ring buffer) adds to old: the events after old's last one, or all
// of cur when old's last one has rolled out.
func newProgress(old, cur []v1alpha1.ProgressEvent) []v1alpha1.ProgressEvent {
	if len(old) == 0 {
		return cur
	}
	last, err := json.Marshal(old[len(old)-1])
	if err != nil {
		return cur
	}
	for j := len(cur) - 1; j >= 0; j-- {
		b, err := json.Marshal(cur[j])
		if err == nil && bytes.Equal(b, last) {
			return cur[j+1:]
		}
	}
	return cur
}

func writeFrame(w io.Writer, f frame) error {
	data, err := json.Marshal(f.data)
	if err != nil {
		return fmt.Errorf("encode %s: %w", f.event, err)
	}
	var b bytes.Buffer
	b.WriteString("event: ")
	b.WriteString(f.event)
	b.WriteString("\nid: ")
	b.WriteString(f.id)
	b.WriteString("\ndata: ")
	b.Write(data)
	b.WriteString("\n\n")
	_, err = w.Write(b.Bytes())
	return err
}
