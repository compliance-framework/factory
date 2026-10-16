package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/store"
)

// DefaultWallClockGrace is added to a TaskSpec's limits.maxWallClock before a run times out.
const DefaultWallClockGrace = 2 * time.Minute

// Abandon reasons (the API's AbandonRequest.reason).
const (
	AbandonThrottled  = "throttled"
	AbandonAgentError = "agent-error"
	AbandonShutdown   = "shutdown"
)

// Errors of the run API, for the API server to map to HTTP statuses.
var (
	// ErrUnknownTask is a task id the store doesn't have (404).
	ErrUnknownTask = errors.New("unknown task")
	// ErrNotLeased is a task that has no lease to complete or abandon (409).
	ErrNotLeased = errors.New("task is not leased")
)

// RunAPI is how the scheduler (RT-S2-T02) and the API server (RT-S2-T03) drive runs. The
// run controller implements it.
type RunAPI interface {
	// LeaseIssued creates the Run of a task the scheduler just leased (the run controller
	// also creates it from the Task watch, so calling this is an optimisation).
	LeaseIssued(ctx context.Context, taskID string) error
	// Complete ingests a Result. It returns the task's phase after it (Succeeded, or Failed
	// for an invalid result or a failed status); completing a finished task again returns its
	// phase and changes nothing.
	Complete(ctx context.Context, taskID string, res v1alpha1.Result) (v1alpha1.TaskPhase, error)
	// Abandon gives a leased task back: throttled returns it to Pending without using an
	// attempt; other reasons retry it per the queue's retry policy, or fail it.
	Abandon(ctx context.Context, taskID string, a Abandonment) error
	// CancelRequested is the heartbeat's cancel flag: true while the task is Cancelling.
	CancelRequested(ctx context.Context, taskID string) (bool, error)
	// AckCancel is the agent's acknowledgement: a Cancelling task becomes Cancelled.
	AckCancel(ctx context.Context, taskID string) error
}

// Abandonment is an agent giving a task back.
type Abandonment struct {
	Reason  string
	Until   time.Time
	Message string
}

// RunOptions configure the run controller. Zero fields take the defaults.
type RunOptions struct {
	Workers int
	// Clock times runs and timeouts. Default the wall clock.
	Clock clock.Clock
	// Logger. Default slog.Default().
	Logger *slog.Logger
	// Grace is added to maxWallClock. Default DefaultWallClockGrace.
	Grace time.Duration
	// LockTTL is the work item lock's lease. Default DefaultLockTTL.
	LockTTL time.Duration
}

// RunController tracks runs (controllers.md §run): it creates the Run when a task is leased,
// ingests Results (validated against the TaskSpec's output), times runs out, handles cancel
// and abandon, updates the work item's counters and emits factory.run.completed / .failed.
// Its key is the task id.
type RunController struct {
	s       store.Store
	cat     CatalogProvider
	factory *FactoryProvider
	opts    RunOptions
	log     *slog.Logger
	ctrl    *Controller
}

var _ RunAPI = (*RunController)(nil)

// NewRunController returns the run controller, watching Tasks and Runs.
func NewRunController(s store.Store, cat CatalogProvider, factory *FactoryProvider, opts RunOptions) (*RunController, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Grace <= 0 {
		opts.Grace = DefaultWallClockGrace
	}
	if opts.LockTTL <= 0 {
		opts.LockTTL = DefaultLockTTL
	}
	r := &RunController{s: s, cat: cat, factory: factory, opts: opts, log: opts.Logger.With("controller", "run")}
	r.ctrl = NewController("run", r, Options{Workers: opts.Workers, Clock: opts.Clock, Logger: opts.Logger})
	if err := r.ctrl.Watch(s, v1alpha1.KindTask, nil, InformerOptions{}); err != nil {
		return nil, err
	}
	runTask := func(obj store.Object) []string {
		if run, ok := obj.(*v1alpha1.Run); ok {
			return []string{run.Spec.Task}
		}
		return nil
	}
	if err := r.ctrl.Watch(s, v1alpha1.KindRun, runTask, InformerOptions{}); err != nil {
		return nil, err
	}
	return r, nil
}

// Controller returns the underlying controller.
func (r *RunController) Controller() *Controller { return r.ctrl }

// Run runs the controller until ctx is done.
func (r *RunController) Run(ctx context.Context) error { return r.ctrl.Run(ctx) }

func (r *RunController) getTask(ctx context.Context, id string) (*v1alpha1.Task, error) {
	var t v1alpha1.Task
	err := r.s.Get(ctx, store.Key{Kind: v1alpha1.KindTask, Name: id}, &t)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("task %s: %w", id, ErrUnknownTask)
	}
	if err != nil {
		return nil, fmt.Errorf("get task %s: %w", id, err)
	}
	if t.Status == nil {
		t.Status = &v1alpha1.TaskStatus{Phase: v1alpha1.TaskPending}
	}
	return &t, nil
}

// Reconcile implements Reconciler for one task: it keeps the task's open Run in line with
// the task's phase and times the run out.
func (r *RunController) Reconcile(ctx context.Context, id string) (Result, error) {
	t, err := r.getTask(ctx, id)
	if errors.Is(err, ErrUnknownTask) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	switch t.Status.Phase {
	case v1alpha1.TaskLeased, v1alpha1.TaskRunning:
		run, err := r.ensureRun(ctx, t)
		if err != nil {
			return Result{}, err
		}
		return r.checkTimeout(ctx, t, run)
	case v1alpha1.TaskCancelling:
		if t.Status.Agent == nil {
			// No agent holds it: nobody to tell, it is cancelled now.
			run, err := r.openRun(ctx, t.Metadata.ID)
			if err != nil {
				return Result{}, err
			}
			return Result{}, r.finish(ctx, t, run, finish{phase: v1alpha1.TaskCancelled, reason: ReasonCancelled, end: v1alpha1.RunCancelled})
		}
		// The agent learns it from its next heartbeat (CancelRequested) and acknowledges.
		run, err := r.ensureRun(ctx, t)
		if err != nil {
			return Result{}, err
		}
		return r.checkTimeout(ctx, t, run)
	default:
		// Pending or finished: a run still open lost its lease (the scheduler moved the task).
		run, err := r.openRun(ctx, t.Metadata.ID)
		if err != nil || run == nil {
			return Result{}, err
		}
		switch t.Status.Phase {
		case v1alpha1.TaskFailed:
			return Result{}, r.finish(ctx, t, run, finish{phase: t.Status.Phase, reason: t.Status.Reason, end: v1alpha1.RunFailed, force: true})
		case v1alpha1.TaskCancelled:
			return Result{}, r.finish(ctx, t, run, finish{phase: t.Status.Phase, reason: t.Status.Reason, end: v1alpha1.RunCancelled, force: true})
		default:
			return Result{}, r.closeRun(ctx, run, v1alpha1.RunLost, "the task left the run ("+string(t.Status.Phase)+")")
		}
	}
}

// checkTimeout fails the run once it has run maxWallClock plus the grace; until then it
// requeues the task for its deadline.
func (r *RunController) checkTimeout(ctx context.Context, t *v1alpha1.Task, run *v1alpha1.Run) (Result, error) {
	if t.Spec.Limits == nil || t.Spec.Limits.MaxWallClock == "" || run.Status == nil {
		return Result{}, nil
	}
	limit, err := t.Spec.Limits.MaxWallClock.Parse()
	if err != nil || limit <= 0 {
		return Result{}, nil //nolint:nilerr // an unparsable limit was rejected by the catalog; no timeout
	}
	deadline := run.Status.StartedAt.Add(limit + r.opts.Grace)
	if wait := deadline.Sub(r.opts.Clock.Now()); wait > 0 {
		return Result{RequeueAfter: wait}, nil
	}
	f := finish{phase: v1alpha1.TaskFailed, reason: ReasonTimeout, end: v1alpha1.RunFailed}
	if t.Status.Phase == v1alpha1.TaskCancelling {
		f = finish{phase: v1alpha1.TaskCancelled, reason: ReasonCancelled, end: v1alpha1.RunCancelled}
	}
	r.log.InfoContext(ctx, "run timed out", "task", t.Metadata.ID, "run", run.Metadata.Name, "limit", limit)
	// The scheduler revokes the lease of a task that left Leased/Running (internal/scheduler).
	return Result{}, r.finish(ctx, t, run, f)
}

// openRun returns the task's open run, or nil.
func (r *RunController) openRun(ctx context.Context, taskID string) (*v1alpha1.Run, error) {
	runs, err := listRuns(ctx, r.s, taskID)
	if err != nil {
		return nil, err
	}
	for i := len(runs) - 1; i >= 0; i-- {
		if !ended(runs[i]) {
			return runs[i], nil
		}
	}
	return nil, nil
}

// ensureRun returns the task's open run, creating it for the current lease.
func (r *RunController) ensureRun(ctx context.Context, t *v1alpha1.Task) (*v1alpha1.Run, error) {
	runs, err := listRuns(ctx, r.s, t.Metadata.ID)
	if err != nil {
		return nil, err
	}
	for i := len(runs) - 1; i >= 0; i-- {
		if !ended(runs[i]) {
			return runs[i], nil
		}
	}
	agent := ""
	if t.Status.Agent != nil {
		agent = *t.Status.Agent
	}
	run := &v1alpha1.Run{
		TypeMeta: v1alpha1.TypeMeta{Kind: v1alpha1.KindRun},
		Metadata: v1alpha1.ObjectMeta{
			Name:   "run_" + strings.TrimPrefix(t.Metadata.ID, "tsk_") + "-" + strconv.Itoa(len(runs)+1),
			Labels: map[string]string{LabelTaskSpec: t.Spec.TaskSpec.Name},
		},
		Spec:   v1alpha1.RunSpec{Task: t.Metadata.ID, Attempt: t.Status.Attempts + 1, Agent: agent},
		Status: &v1alpha1.RunStatus{StartedAt: r.opts.Clock.Now()},
	}
	if err := r.s.Create(ctx, run); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			var got v1alpha1.Run
			if gerr := r.s.Get(ctx, store.KeyOf(run), &got); gerr != nil {
				return nil, fmt.Errorf("get run %s: %w", run.Metadata.Name, gerr)
			}
			return &got, nil
		}
		return nil, fmt.Errorf("create run %s: %w", run.Metadata.Name, err)
	}
	r.log.InfoContext(ctx, "run started", "task", t.Metadata.ID, "run", run.Metadata.Name, "agent", agent)
	return run, nil
}

// closeRun ends a run that doesn't count (abandoned, lost).
func (r *RunController) closeRun(ctx context.Context, run *v1alpha1.Run, end v1alpha1.RunEnd, msg string) error {
	now := r.opts.Clock.Now()
	setEnd(run, end, now)
	run.Status.Progress = appendProgress(run.Status.Progress, v1alpha1.ProgressEvent{At: now, Kind: string(end), Message: msg})
	if err := r.s.Update(ctx, run); err != nil {
		return fmt.Errorf("close run %s: %w", run.Metadata.Name, err)
	}
	return nil
}

func setEnd(run *v1alpha1.Run, end v1alpha1.RunEnd, now time.Time) {
	if run.Status == nil {
		run.Status = &v1alpha1.RunStatus{}
	}
	run.Status.End = end
	run.Status.EndedAt = now
}

// maxProgress bounds a run's progress ring buffer.
const maxProgress = 200

func appendProgress(p []v1alpha1.ProgressEvent, e v1alpha1.ProgressEvent) []v1alpha1.ProgressEvent {
	p = append(p, e)
	if over := len(p) - maxProgress; over > 0 {
		p = slices.Clone(p[over:])
	}
	return p
}

// LeaseIssued implements RunAPI.
func (r *RunController) LeaseIssued(ctx context.Context, taskID string) error {
	t, err := r.getTask(ctx, taskID)
	if err != nil {
		return err
	}
	switch t.Status.Phase {
	case v1alpha1.TaskLeased, v1alpha1.TaskRunning:
		_, err = r.ensureRun(ctx, t)
		return err
	}
	return fmt.Errorf("task %s is %s: %w", taskID, t.Status.Phase, ErrNotLeased)
}

// CancelRequested implements RunAPI.
func (r *RunController) CancelRequested(ctx context.Context, taskID string) (bool, error) {
	t, err := r.getTask(ctx, taskID)
	if err != nil {
		return false, err
	}
	return t.Status.Phase == v1alpha1.TaskCancelling, nil
}

// AckCancel implements RunAPI.
func (r *RunController) AckCancel(ctx context.Context, taskID string) error {
	t, err := r.getTask(ctx, taskID)
	if err != nil {
		return err
	}
	switch t.Status.Phase {
	case v1alpha1.TaskCancelled:
		return nil
	case v1alpha1.TaskCancelling:
	default:
		return fmt.Errorf("task %s is %s, not Cancelling: %w", taskID, t.Status.Phase, ErrNotLeased)
	}
	run, err := r.openRun(ctx, taskID)
	if err != nil {
		return err
	}
	return r.finish(ctx, t, run, finish{phase: v1alpha1.TaskCancelled, reason: ReasonCancelled, end: v1alpha1.RunCancelled})
}

// Abandon implements RunAPI.
func (r *RunController) Abandon(ctx context.Context, taskID string, a Abandonment) error {
	t, err := r.getTask(ctx, taskID)
	if err != nil {
		return err
	}
	switch t.Status.Phase {
	case v1alpha1.TaskLeased, v1alpha1.TaskRunning:
	case v1alpha1.TaskCancelling:
		run, err := r.openRun(ctx, taskID)
		if err != nil {
			return err
		}
		return r.finish(ctx, t, run, finish{phase: v1alpha1.TaskCancelled, reason: ReasonCancelled, end: v1alpha1.RunCancelled})
	default:
		return fmt.Errorf("task %s is %s: %w", taskID, t.Status.Phase, ErrNotLeased)
	}
	run, err := r.openRun(ctx, taskID)
	if err != nil {
		return err
	}
	msg := "abandoned: " + a.Reason
	if a.Message != "" {
		msg += ": " + a.Message
	}
	if a.Reason == AbandonThrottled {
		// Throttling never consumes an attempt (the scheduler throttles the agent until Until).
		return r.requeue(ctx, t, run, ReasonThrottled, msg, false)
	}
	retry := v1alpha1.RetryInfraFailure
	if a.Reason == AbandonShutdown {
		retry = v1alpha1.RetryAgentLost
	}
	if r.mayRetry(t, retry) {
		return r.requeue(ctx, t, run, string(retry), msg, true)
	}
	return r.finish(ctx, t, run, finish{phase: v1alpha1.TaskFailed, reason: string(retry), end: v1alpha1.RunFailed})
}

// mayRetry reports whether the queue's retry policy allows another attempt for reason.
func (r *RunController) mayRetry(t *v1alpha1.Task, reason v1alpha1.RetryReason) bool {
	cat := r.cat.Current()
	if cat == nil {
		return false
	}
	q := cat.Queues[t.Spec.Queue]
	if q == nil || q.Spec.Retry == nil {
		return false
	}
	p := q.Spec.Retry
	if len(p.On) > 0 && !slices.Contains(p.On, reason) {
		return false
	}
	return t.Status.Attempts+1 <= p.Max
}

// requeue returns a task to Pending, closing its run as abandoned.
func (r *RunController) requeue(ctx context.Context, t *v1alpha1.Task, run *v1alpha1.Run, reason, msg string, attempt bool) error {
	if run != nil {
		if err := r.closeRun(ctx, run, v1alpha1.RunAbandoned, msg); err != nil {
			return err
		}
	}
	t.Status.Phase = v1alpha1.TaskPending
	t.Status.Reason = reason
	t.Status.Agent = nil
	t.Status.LeaseExpiresAt = time.Time{}
	if attempt {
		t.Status.Attempts++
	}
	if err := r.s.Update(ctx, t); err != nil {
		return fmt.Errorf("requeue task %s: %w", t.Metadata.ID, err)
	}
	r.log.InfoContext(ctx, "task requeued", "task", t.Metadata.ID, "reason", reason, "attempts", t.Status.Attempts)
	return nil
}

// Complete implements RunAPI.
func (r *RunController) Complete(ctx context.Context, taskID string, res v1alpha1.Result) (v1alpha1.TaskPhase, error) {
	t, err := r.getTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	switch t.Status.Phase {
	case v1alpha1.TaskSucceeded, v1alpha1.TaskFailed, v1alpha1.TaskCancelled:
		return t.Status.Phase, nil // replay
	case v1alpha1.TaskLeased, v1alpha1.TaskRunning, v1alpha1.TaskCancelling:
	default:
		return "", fmt.Errorf("task %s is %s: %w", taskID, t.Status.Phase, ErrNotLeased)
	}
	run, err := r.ensureRun(ctx, t)
	if err != nil {
		return "", err
	}
	if res.Task == "" {
		res.Task = taskID
	}
	f := finish{phase: v1alpha1.TaskSucceeded, end: v1alpha1.RunCompleted, result: &res}
	if verr := validateResult(r.cat.Current(), t, res); verr != nil {
		r.log.WarnContext(ctx, "invalid result", "task", taskID, "err", verr)
		f = finish{phase: v1alpha1.TaskFailed, reason: ReasonInvalidResult, end: v1alpha1.RunFailed, invalid: verr.Error(), rejected: &res}
	} else if res.Status == v1alpha1.ResultFailed {
		// The agent reported: the run completed (Run.status.end), the task failed.
		f.phase, f.reason = v1alpha1.TaskFailed, string(v1alpha1.ResultFailed)
	}
	if err := r.finish(ctx, t, run, f); err != nil {
		return "", err
	}
	return f.phase, nil
}

// validateResult checks a Result against the task's TaskSpec output: its schema and outcomes.
func validateResult(cat *catalog.Catalog, t *v1alpha1.Task, res v1alpha1.Result) error {
	if res.Task != t.Metadata.ID {
		return fmt.Errorf("result is for task %q, not %q", res.Task, t.Metadata.ID)
	}
	switch res.Status {
	case v1alpha1.ResultDone, v1alpha1.ResultFailed, v1alpha1.ResultNeedsInput, v1alpha1.ResultNoop:
	default:
		return fmt.Errorf("status %q is not one of done, failed, needs_input, noop", res.Status)
	}
	out := t.Spec.Output
	if out != nil && len(out.Outcomes) > 0 {
		switch {
		case res.Outcome == "" && res.Status == v1alpha1.ResultDone:
			return fmt.Errorf("a done result needs an outcome (one of %s)", strings.Join(out.Outcomes, ", "))
		case res.Outcome != "" && !slices.Contains(out.Outcomes, res.Outcome):
			return fmt.Errorf("outcome %q is not one of %s", res.Outcome, strings.Join(out.Outcomes, ", "))
		}
	}
	if cat == nil {
		return nil
	}
	ts := cat.TaskSpecs[t.Spec.TaskSpec.Name]
	if ts == nil || ts.OutputSchema == nil {
		return nil
	}
	b, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	if err := ts.OutputSchema.Validate(inst); err != nil {
		return fmt.Errorf("schema %s: %s", out.Schema, strings.Join(strings.Fields(err.Error()), " "))
	}
	return nil
}

// finish is how a task ends.
type finish struct {
	phase  v1alpha1.TaskPhase
	reason string
	// end is the run's end (Run.status.end); empty when there is no run.
	end    v1alpha1.RunEnd
	result *v1alpha1.Result
	// invalid and rejected are an invalid result's error and the result, kept as progress.
	invalid  string
	rejected *v1alpha1.Result
	// force finishes a task that is already in phase (the scheduler failed it) without
	// writing it again.
	force bool
}

// finish ends a task and its run under the work item's lock, in an order that makes a retry
// safe: the run (result, end), the work item's counters (only when this call ended the run),
// the task's phase, then the factory.run.* event (deduplicated by the run's name).
func (r *RunController) finish(ctx context.Context, t *v1alpha1.Task, run *v1alpha1.Run, f finish) error {
	return withItemLock(ctx, r.s, t.Metadata.WorkItem, r.opts.LockTTL, func(ctx context.Context) error {
		return r.finishLocked(ctx, t, run, f)
	})
}

// finishLocked is finish with the work item's lock held.
func (r *RunController) finishLocked(ctx context.Context, t *v1alpha1.Task, run *v1alpha1.Run, f finish) error {
	now := r.opts.Clock.Now()

	if run != nil && !ended(run) {
		setEnd(run, f.end, now)
		if f.result != nil {
			res := *f.result
			run.Status.Result = &res
			if res.Harness != nil {
				u := res.Harness.Usage
				run.Status.Usage = &u
			}
		}
		if f.invalid != "" {
			run.Status.Progress = appendProgress(run.Status.Progress, v1alpha1.ProgressEvent{
				At: now, Kind: ReasonInvalidResult, Message: f.invalid, Data: resultMap(f.rejected),
			})
		}
		if f.reason != "" && f.result == nil && f.invalid == "" {
			run.Status.Progress = appendProgress(run.Status.Progress, v1alpha1.ProgressEvent{At: now, Kind: string(f.end), Message: f.reason})
		}
		if err := r.s.Update(ctx, run); err != nil {
			return fmt.Errorf("end run %s: %w", run.Metadata.Name, err)
		}
		if counted(run) {
			if err := r.count(ctx, t.Metadata.WorkItem, t.Spec.TaskSpec.Name); err != nil {
				return err
			}
		}
	}
	if !f.force {
		t.Status.Phase = f.phase
		t.Status.Reason = f.reason
		t.Status.LeaseExpiresAt = time.Time{}
		if err := r.s.Update(ctx, t); err != nil {
			return fmt.Errorf("finish task %s: %w", t.Metadata.ID, err)
		}
	}
	r.log.InfoContext(ctx, "task finished", "task", t.Metadata.ID, "phase", f.phase, "reason", f.reason)
	if run == nil {
		return nil // never ran: no run event
	}
	typ := v1alpha1.EventFactoryRunCompleted
	if f.phase != v1alpha1.TaskSucceeded {
		typ = v1alpha1.EventFactoryRunFailed
	}
	sum := runSummary(t, run)
	if f.phase == v1alpha1.TaskCancelled {
		sum.Status = "cancelled"
	}
	reason := f.reason
	if f.phase == v1alpha1.TaskSucceeded {
		reason = ""
	}
	_, err := r.factory.Emit(ctx, FactoryEvent{
		Type: typ, WorkItem: t.Metadata.WorkItem, Key: "run/" + run.Metadata.Name, Facts: runFacts(sum, reason),
	})
	return err
}

// count adds one run of spec to the work item's counters (compare-and-swap, retried on a
// conflict). The caller holds the work item's lock.
func (r *RunController) count(ctx context.Context, workItem, spec string) error {
	for attempt := 0; ; attempt++ {
		var wi v1alpha1.WorkItem
		err := r.s.Get(ctx, store.Key{Kind: v1alpha1.KindWorkItem, Name: workItem}, &wi)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get work item %s: %w", workItem, err)
		}
		st := ensureStatus(&wi)
		counters := maps.Clone(st.Counters)
		if counters == nil {
			counters = map[string]int{}
		}
		counters[spec]++
		st.Counters = counters
		err = r.s.Update(ctx, &wi)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) || attempt >= 4 {
			return fmt.Errorf("count run of %s on %s: %w", spec, workItem, err)
		}
	}
}
