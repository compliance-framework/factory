package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
)

type sseFrame struct {
	event, id, data, comment string
}

// stream is an open SSE response and its parsed frames.
type stream struct {
	t      *testing.T
	res    *http.Response
	frames chan sseFrame
	cancel context.CancelFunc
}

func (e *env) subscribe(topics []string, lastEventID string, as apiclient.RequestEditorFn) *stream {
	e.t.Helper()
	ctx, cancel := context.WithCancel(e.ctx)
	p := &apiclient.StreamEventsParams{Topics: topics}
	if lastEventID != "" {
		p.LastEventID = &lastEventID
	}
	res, err := e.c.StreamEvents(ctx, p, as) //nolint:bodyclose // closed by the test's cleanup: the stream outlives this function
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		cancel()
		e.t.Fatalf("subscribe %v: %d %s", topics, res.StatusCode, b)
	}
	s := &stream{t: e.t, res: res, frames: make(chan sseFrame, 64), cancel: cancel}
	go func() {
		defer close(s.frames)
		r := bufio.NewReader(res.Body)
		var f sseFrame
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSuffix(line, "\n")
			switch {
			case line == "":
				s.frames <- f
				f = sseFrame{}
			case strings.HasPrefix(line, ":"):
				f.comment = strings.TrimSpace(strings.TrimPrefix(line, ":"))
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	e.t.Cleanup(func() {
		cancel()
		_ = res.Body.Close()
	})
	if f := s.next(); f.comment != "connected" {
		e.t.Fatalf("first frame %+v", f)
	}
	return s
}

func (s *stream) next() sseFrame {
	s.t.Helper()
	select {
	case f, ok := <-s.frames:
		if !ok {
			s.t.Fatal("the stream ended")
		}
		return f
	case <-time.After(waitTimeout):
		s.t.Fatal("timed out waiting for a frame")
		return sseFrame{}
	}
}

// until returns the next frame of event, skipping the others.
func (s *stream) until(event string) sseFrame {
	s.t.Helper()
	for {
		if f := s.next(); f.event == event {
			return f
		}
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

func TestStreamDelivery(t *testing.T) {
	e := newEnv(t)
	gus := e.login("gus")
	agent, tok := e.register("gus", "mac1")
	e.task("tsk_1", nil)
	l := e.lease(agent, tok)
	run := *l.Lease.Run
	s := e.subscribe([]string{"tasks", "agents", "workitems/" + workItemID, "runs/" + run}, "", session(gus))

	hb, err := e.c.HeartbeatLeaseWithResponse(e.ctx, l.Lease.Id, apiclient.HeartbeatRequest{Progress: []v1alpha1.ProgressEvent{
		{At: t0, Kind: "step", Message: "clone"}, {At: t0, Kind: "step", Message: "edit"},
	}}, bearer(tok))
	if err != nil || hb.JSON200 == nil {
		t.Fatalf("heartbeat: %v %s", err, hb.Status())
	}
	var got []string
	for len(got) < 2 {
		f := s.until(SSEProgress)
		var p StreamProgress
		if err := json.Unmarshal([]byte(f.data), &p); err != nil || p.Run != run || p.Task != "tsk_1" || f.id == "" {
			t.Fatalf("progress frame %+v (%v)", f, err)
		}
		got = append(got, p.Event.Message)
	}
	if strings.Join(got, ",") != "clone,edit" {
		t.Errorf("progress %v", got)
	}
	// A second heartbeat sends only what it adds.
	if _, err := e.c.HeartbeatLeaseWithResponse(e.ctx, l.Lease.Id, apiclient.HeartbeatRequest{Progress: []v1alpha1.ProgressEvent{
		{At: t0, Kind: "step", Message: "test"},
	}}, bearer(tok)); err != nil {
		t.Fatal(err)
	}
	f := s.until(SSEProgress)
	if !strings.Contains(f.data, `"message":"test"`) {
		t.Errorf("second progress %s", f.data)
	}

	// Task and agent updates.
	if _, err := e.c.UpdateAgentWithResponse(e.ctx, agent, apiclient.AgentUpdate{Phase: ptr(v1alpha1.AgentDraining)}, bearer(tok)); err != nil {
		t.Fatal(err)
	}
	f = s.until(SSEAgentUpdated)
	var a v1alpha1.Agent
	if err := json.Unmarshal([]byte(f.data), &a); err != nil || a.Metadata.Name != agent {
		t.Errorf("agent frame %s", f.data)
	}
	// Work items: only the one subscribed to.
	{
		st := e.st
		if err := st.Create(e.ctx, &v1alpha1.WorkItem{TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindWorkItem}, Metadata: v1alpha1.ObjectMeta{Name: "wi_other"}}); err != nil {
			t.Fatal(err)
		}
		var wi v1alpha1.WorkItem
		if err := st.Get(e.ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: workItemID}, &wi); err != nil {
			t.Fatal(err)
		}
		wi.Status.State = map[string]any{"plan": "done"}
		if err := st.Update(e.ctx, &wi); err != nil {
			t.Fatal(err)
		}
	}
	f = s.until(SSEWorkItemUpdated)
	if !strings.Contains(f.data, `"name":"`+workItemID+`"`) || !strings.Contains(f.data, `"plan":"done"`) {
		t.Errorf("work item frame %s", f.data)
	}
	if _, err := e.c.CompleteLeaseWithResponse(e.ctx, l.Lease.Id, apiclient.CompleteRequest{Result: v1alpha1.Result{Task: "tsk_1", Status: v1alpha1.ResultDone, Outcome: "pr-opened"}}, bearer(tok)); err != nil {
		t.Fatal(err)
	}
	for {
		f := s.until(SSETaskUpdated)
		if strings.Contains(f.data, `"phase":"Succeeded"`) {
			break
		}
	}
}

func TestStreamKeepAliveAndDisconnect(t *testing.T) {
	e := newEnv(t)
	gus := e.login("gus")
	s := e.subscribe([]string{"tasks"}, "", session(gus))
	e.clk.BlockUntil(1) // the keep-alive timer
	e.clk.Advance(DefaultKeepAlive)
	if f := s.next(); f.comment != "keep-alive" {
		t.Fatalf("frame %+v, want a keep-alive", f)
	}
	if n := e.srv.subscribers.Load(); n != 1 {
		t.Fatalf("%d subscribers", n)
	}
	s.cancel()
	eventually(t, "the stream to end after the client left", func() bool { return e.srv.subscribers.Load() == 0 })
}

func TestStreamShutdown(t *testing.T) {
	e := newEnv(t)
	gus := e.login("gus")
	s := e.subscribe([]string{"tasks"}, "", session(gus))
	if err := e.srv.Shutdown(e.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-s.frames:
		if ok {
			t.Fatal("a frame after shutdown")
		}
	case <-time.After(waitTimeout):
		t.Fatal("the stream didn't end on shutdown")
	}
	res, err := e.c.StreamEventsWithResponse(e.ctx, &apiclient.StreamEventsParams{Topics: []string{"tasks"}}, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "subscribe after shutdown", res.HTTPResponse, res.Body, http.StatusServiceUnavailable, CodeUnavailable)
}

func TestStreamResume(t *testing.T) {
	e := newEnv(t)
	gus := e.login("gus")
	s := e.subscribe([]string{"tasks"}, "", session(gus))
	e.task("tsk_a", nil)
	f := s.until(SSETaskUpdated)
	if !strings.Contains(f.data, "tsk_a") {
		t.Fatalf("frame %s", f.data)
	}
	s.cancel()
	eventually(t, "the first stream to end", func() bool { return e.srv.subscribers.Load() == 0 })
	e.task("tsk_b", nil) // missed while disconnected
	s2 := e.subscribe([]string{"tasks"}, f.id, session(gus))
	f2 := s2.until(SSETaskUpdated)
	if !strings.Contains(f2.data, "tsk_b") || rv(f2.id) <= rv(f.id) {
		t.Fatalf("resumed frame %+v after %s", f2, f.id)
	}

	// A run's progress is backfilled from its ring buffer on resume.
	agent, tok := e.register("gus", "mac1")
	l := e.lease(agent, tok)
	if _, err := e.c.HeartbeatLeaseWithResponse(e.ctx, l.Lease.Id, apiclient.HeartbeatRequest{Progress: []v1alpha1.ProgressEvent{{At: t0, Kind: "step", Message: "early"}}}, bearer(tok)); err != nil {
		t.Fatal(err)
	}
	s3 := e.subscribe([]string{"runs/" + *l.Lease.Run}, f.id, session(gus))
	if p := s3.until(SSEProgress); !strings.Contains(p.data, `"message":"early"`) {
		t.Fatalf("backfill %s", p.data)
	}
}

func TestStreamLimits(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.MaxSubscribers = 1 })
	gus := e.login("gus")
	e.subscribe([]string{"tasks"}, "", session(gus))
	res, err := e.c.StreamEventsWithResponse(e.ctx, &apiclient.StreamEventsParams{Topics: []string{"agents"}}, session(gus))
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "a second subscriber", res.HTTPResponse, res.Body, http.StatusServiceUnavailable, CodeTooManySubscribers)
	for _, topics := range [][]string{{"nope"}, {"runs/"}, {"workitems/a/b"}} {
		res, err := e.c.StreamEventsWithResponse(e.ctx, &apiclient.StreamEventsParams{Topics: topics}, session(gus))
		if err != nil {
			t.Fatal(err)
		}
		wantProblem(t, "topics "+strings.Join(topics, ","), res.HTTPResponse, res.Body, http.StatusBadRequest, CodeInvalid)
	}
}
