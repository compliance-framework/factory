package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/clock"
)

// Catalog-sync defaults.
const (
	// CatalogSyncKey is catalog-sync's only work-queue key.
	CatalogSyncKey = "catalog"
	// DefaultCatalogPollInterval is how often catalog-sync polls its source.
	DefaultCatalogPollInterval = 60 * time.Second
	// DefaultCatalogMaxErrors bounds the diagnostics kept in CatalogStatus.
	DefaultCatalogMaxErrors = 50
	// DefaultCatalogRetryBase is the first retry delay after the source couldn't be read;
	// retries back off exponentially up to the poll interval.
	DefaultCatalogRetryBase = time.Second
)

// ErrCatalogInvalid is wrapped by Sync when the source's catalog fails validation (the live
// catalog is kept).
var ErrCatalogInvalid = errors.New("catalog is invalid")

// CatalogProvider gives readers the live catalog snapshot. Snapshots are immutable and
// swapped whole, so a reader that calls Current once per unit of work sees one consistent
// catalog throughout. Current returns nil until the first good catalog has loaded: the
// dispatcher's startup waits for CatalogSync.Ready before starting the controllers that read
// it, so after startup it is never nil.
type CatalogProvider interface {
	Current() *catalog.Catalog
}

// CatalogStatus is catalog-sync's report (the LLD's {lastGood, lastAttempt, errors}), for
// the API server and the UI. Times come from the injected clock.
type CatalogStatus struct {
	// Source describes where the catalog comes from (no credentials).
	Source string `json:"source"`
	// Ready is true once a good catalog is live (and stays true: a later failure keeps it).
	Ready bool `json:"ready"`
	// LastGoodSHA is the live catalog's SHA; LoadedAt is when it was swapped in.
	LastGoodSHA string    `json:"lastGoodSHA,omitempty"`
	LoadedAt    time.Time `json:"loadedAt,omitzero"`
	// LastAttemptSHA is the last revision catalog-sync tried to load (equal to LastGoodSHA
	// when it is live).
	LastAttemptSHA string `json:"lastAttemptSHA,omitempty"`
	// LastSuccess is the last sync that ended with the source's revision live; LastFailure
	// the last that didn't (invalid catalog or unreadable source).
	LastSuccess time.Time `json:"lastSuccess,omitzero"`
	LastFailure time.Time `json:"lastFailure,omitzero"`
	// Errors are the error diagnostics of the last load that failed validation (the revision
	// in LastAttemptSHA, unless a source failure came after), at most MaxErrors of them;
	// ErrorsOmitted counts the rest. Cleared when a catalog is swapped in.
	Errors        []catalog.Error `json:"errors,omitempty"`
	ErrorsOmitted int             `json:"errorsOmitted,omitempty"`
	// Warnings are the live catalog's warnings, bounded like Errors.
	Warnings        []catalog.Error `json:"warnings,omitempty"`
	WarningsOmitted int             `json:"warningsOmitted,omitempty"`
	// SourceError is the last failure to read the source (git or the directory), credentials
	// redacted; empty once the source reads again.
	SourceError string `json:"sourceError,omitempty"`
}

// CatalogSyncOptions configure CatalogSync. Zero fields take the defaults.
type CatalogSyncOptions struct {
	// Interval is the poll interval. Default 60 s.
	Interval time.Duration
	// MaxErrors bounds CatalogStatus.Errors and Warnings. Default 50.
	MaxErrors int
	// Clock times polls and retries and stamps the status. Default the wall clock.
	Clock clock.Clock
	// Logger. Default slog.Default().
	Logger *slog.Logger
}

// CatalogSync is the catalog-sync controller. It polls its CatalogSource; when the head
// revision changes it loads and validates the catalog there and, when it has no errors,
// swaps it in atomically. A catalog with errors is rejected whole: the live catalog stays,
// and Status reports the attempt and its diagnostics. A rejected revision is not loaded
// again until the head moves or Resync forces it.
//
// Before the first good catalog Current returns nil and Ready is open.
type CatalogSync struct {
	src       CatalogSource
	interval  time.Duration
	maxErrors int
	clk       clock.Clock
	log       *slog.Logger
	ctrl      *Controller

	current atomic.Pointer[catalog.Catalog]
	force   atomic.Bool

	syncMu    sync.Mutex // serialises Sync
	failedSHA string     // the last revision that failed validation; guarded by syncMu

	mu        sync.Mutex // guards status and changed
	status    CatalogStatus
	changed   chan struct{}
	ready     chan struct{}
	readyOnce sync.Once
}

var _ CatalogProvider = (*CatalogSync)(nil)

// NewCatalogSync returns a catalog-sync controller over src.
func NewCatalogSync(src CatalogSource, opts CatalogSyncOptions) *CatalogSync {
	if opts.Interval <= 0 {
		opts.Interval = DefaultCatalogPollInterval
	}
	if opts.MaxErrors <= 0 {
		opts.MaxErrors = DefaultCatalogMaxErrors
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &CatalogSync{
		src:       src,
		interval:  opts.Interval,
		maxErrors: opts.MaxErrors,
		clk:       opts.Clock,
		log:       opts.Logger.With("controller", "catalog-sync"),
		status:    CatalogStatus{Source: src.String()},
		changed:   make(chan struct{}),
		ready:     make(chan struct{}),
	}
	s.ctrl = NewController("catalog-sync", s, Options{
		Workers: 1,
		Clock:   opts.Clock,
		Backoff: Backoff{Base: min(DefaultCatalogRetryBase, opts.Interval), Max: opts.Interval, Jitter: DefaultJitter},
		Logger:  opts.Logger,
	})
	return s
}

// Current returns the live catalog, nil before the first good one.
func (s *CatalogSync) Current() *catalog.Catalog { return s.current.Load() }

// Snapshot returns the live catalog (nil before the first good one) and a channel closed at
// the next swap. Controllers that derive state from the catalog (provider connections,
// budgets) wait on it, then call Snapshot again.
func (s *CatalogSync) Snapshot() (*catalog.Catalog, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current.Load(), s.changed
}

// Ready is closed once the first good catalog is live (startup waits for it).
func (s *CatalogSync) Ready() <-chan struct{} { return s.ready }

// Status returns a copy of the current status.
func (s *CatalogSync) Status() CatalogStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	st.Errors = slices.Clone(st.Errors)
	st.Warnings = slices.Clone(st.Warnings)
	return st
}

// Resync makes the controller sync now, reloading the head even if it was already tried
// (the `resync-catalog` command).
func (s *CatalogSync) Resync() {
	s.force.Store(true)
	s.ctrl.Enqueue(CatalogSyncKey)
}

// Run syncs at once, then every Interval (and on Resync), until ctx is done.
func (s *CatalogSync) Run(ctx context.Context) error {
	s.ctrl.Enqueue(CatalogSyncKey)
	return s.ctrl.Run(ctx)
}

// Reconcile implements Reconciler: one Sync, then the next poll. An unreadable source is
// returned as an error, so it is retried with backoff (capped at the interval); an invalid
// catalog waits for the next poll.
func (s *CatalogSync) Reconcile(ctx context.Context, _ string) (Result, error) {
	err := s.Sync(ctx, s.force.Swap(false))
	switch {
	case err == nil, errors.Is(err, ErrCatalogInvalid):
		return Result{RequeueAfter: s.interval}, nil
	default:
		return Result{}, err
	}
}

// Sync polls the source once: when its head differs from the live catalog (or force is
// set), it loads the head and swaps it in if it validates. It returns nil when the head is
// live afterwards, an error wrapping ErrCatalogInvalid when the head failed validation (now
// or on an earlier attempt), or the source's error.
func (s *CatalogSync) Sync(ctx context.Context, force bool) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	head, err := s.src.Head(ctx)
	if err != nil {
		return s.sourceFailed(ctx, "", err)
	}
	cur := s.current.Load()
	if !force {
		if cur != nil && cur.SHA == head {
			s.update(func(st *CatalogStatus) {
				if st.LastAttemptSHA != head {
					st.LastAttemptSHA = head
					st.Errors, st.ErrorsOmitted = nil, 0
				}
				st.SourceError = ""
				st.LastSuccess = s.clk.Now()
			})
			s.failedSHA = ""
			return nil
		}
		if head == s.failedSHA {
			return fmt.Errorf("%w: %s (unchanged since it was rejected)", ErrCatalogInvalid, head)
		}
	}

	s.log.Info("loading catalog", "sha", head, "source", s.src.String())
	c, diags, err := s.src.Load(ctx, head)
	if err != nil {
		return s.sourceFailed(ctx, head, err)
	}
	if catalog.HasErrors(diags) {
		return s.rejected(head, diags)
	}
	if c == nil {
		return s.sourceFailed(ctx, head, fmt.Errorf("loading %s returned no catalog and no errors", head))
	}
	s.swap(head, c, diags)
	return nil
}

// sourceFailed records a failure to read the source (head is "" when Head itself failed).
// A cancelled context is not a failure.
func (s *CatalogSync) sourceFailed(ctx context.Context, head string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.update(func(st *CatalogStatus) {
		if head != "" {
			st.LastAttemptSHA = head
		}
		st.SourceError = err.Error()
		st.LastFailure = s.clk.Now()
	})
	s.log.Warn("catalog source unreadable; keeping the live catalog", "source", s.src.String(), "sha", head, "live", s.liveSHA(), "err", err)
	return fmt.Errorf("catalog-sync: %w", err)
}

// rejected records a catalog that failed validation; the live one stays.
func (s *CatalogSync) rejected(head string, diags []catalog.Error) error {
	s.failedSHA = head
	errs := slices.DeleteFunc(slices.Clone(diags), func(e catalog.Error) bool { return e.Severity != catalog.SeverityError })
	n := len(errs)
	kept, omitted := bound(errs, s.maxErrors)
	s.update(func(st *CatalogStatus) {
		st.LastAttemptSHA = head
		st.Errors, st.ErrorsOmitted = kept, omitted
		st.SourceError = ""
		st.LastFailure = s.clk.Now()
	})
	first := make([]string, 0, 3)
	for _, e := range errs[:min(3, n)] {
		first = append(first, e.Error())
	}
	s.log.Warn("catalog rejected; keeping the live catalog", "sha", head, "live", s.liveSHA(), "errors", n, "first", first)
	return fmt.Errorf("%w: %s has %d errors", ErrCatalogInvalid, head, n)
}

// swap makes c the live catalog and wakes Snapshot waiters.
func (s *CatalogSync) swap(head string, c *catalog.Catalog, diags []catalog.Error) {
	s.failedSHA = ""
	warnings, omitted := bound(diags, s.maxErrors) // no errors here: all warnings
	now := s.clk.Now()

	s.mu.Lock()
	prev := s.current.Swap(c)
	close(s.changed)
	s.changed = make(chan struct{})
	st := &s.status
	st.Ready = true
	st.LastGoodSHA = c.SHA
	st.LoadedAt = now
	st.LastAttemptSHA = head
	st.LastSuccess = now
	st.Errors, st.ErrorsOmitted = nil, 0
	st.Warnings, st.WarningsOmitted = warnings, omitted
	st.SourceError = ""
	s.mu.Unlock()
	s.readyOnce.Do(func() { close(s.ready) })

	prevSHA := ""
	if prev != nil {
		prevSHA = prev.SHA
	}
	s.log.Info("catalog swapped in", "sha", c.SHA, "previous", prevSHA, "warnings", len(diags))
}

func (s *CatalogSync) update(f func(*CatalogStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.status)
}

func (s *CatalogSync) liveSHA() string {
	if c := s.current.Load(); c != nil {
		return c.SHA
	}
	return ""
}

// bound returns at most n of diags and how many it left out.
func bound(diags []catalog.Error, n int) ([]catalog.Error, int) {
	if len(diags) == 0 {
		return nil, 0
	}
	if len(diags) <= n {
		return slices.Clone(diags), 0
	}
	return slices.Clone(diags[:n]), len(diags) - n
}
