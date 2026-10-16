package controllers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/clock"
)

// testCatalog is a snapshot whose content repeats its SHA, so a reader can tell a whole
// snapshot from a mix of two.
func testCatalog(sha string) *catalog.Catalog {
	return &catalog.Catalog{
		SHA:       sha,
		Providers: map[string]*v1alpha1.Provider{"marker": {Metadata: v1alpha1.ObjectMeta{Name: sha}}},
		Queues:    map[string]*v1alpha1.Queue{"marker": {Metadata: v1alpha1.ObjectMeta{Name: sha}}},
	}
}

func diag(sev catalog.Severity, code, file string, line int) catalog.Error {
	return catalog.Error{Code: code, Message: "m", File: file, Line: line, Severity: sev}
}

func newTestSync(t *testing.T, src CatalogSource, opts CatalogSyncOptions) (*CatalogSync, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(t0)
	if opts.Clock == nil {
		opts.Clock = clk
	}
	opts.Logger = discardLogger()
	return NewCatalogSync(src, opts), clk
}

func isReady(s *CatalogSync) bool {
	select {
	case <-s.Ready():
		return true
	default:
		return false
	}
}

func TestCatalogSyncFirstLoadFails(t *testing.T) {
	ctx := context.Background()
	src := NewFakeCatalogSource()
	src.SetHead("bad", nil)
	src.Set("bad", FakeRevision{Diagnostics: []catalog.Error{diag(catalog.SeverityError, catalog.CodeSchema, "rules/a.yaml", 3)}})
	s, _ := newTestSync(t, src, CatalogSyncOptions{})

	err := s.Sync(ctx, false)
	if !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("Sync = %v, want ErrCatalogInvalid", err)
	}
	// No catalog yet: readers see nil and Ready stays open.
	if c := s.Current(); c != nil {
		t.Fatalf("Current = %q, want nil before any good catalog", c.SHA)
	}
	if c, _ := s.Snapshot(); c != nil {
		t.Fatal("Snapshot returned a catalog before any good one")
	}
	if isReady(s) {
		t.Fatal("Ready closed without a good catalog")
	}
	st := s.Status()
	if st.Ready || st.LastGoodSHA != "" || st.LastAttemptSHA != "bad" || len(st.Errors) != 1 || !st.LastFailure.Equal(t0) || !st.LastSuccess.IsZero() {
		t.Fatalf("status = %+v", st)
	}
}

func TestCatalogSyncBadCatalogKeepsOld(t *testing.T) {
	ctx := context.Background()
	src := NewFakeCatalogSource()
	src.Set("good1", FakeRevision{Catalog: testCatalog("good1")})
	src.Set("good2", FakeRevision{Catalog: testCatalog("good2")})
	src.Set("bad", FakeRevision{Diagnostics: []catalog.Error{
		diag(catalog.SeverityError, catalog.CodeSyntax, "rules/broken.yaml", 1),
		diag(catalog.SeverityWarning, catalog.CodeUnusedDefault, "rulesets/x.yaml", 4),
		diag(catalog.SeverityError, catalog.CodeUnknownQueue, "rules/y.yaml", 9),
	}})
	s, clk := newTestSync(t, src, CatalogSyncOptions{})

	src.SetHead("good1", nil)
	if err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if !isReady(s) || s.Current().SHA != "good1" {
		t.Fatal("first good catalog not live")
	}

	t1 := clk.Advance(time.Minute)
	src.SetHead("bad", nil)
	if err := s.Sync(ctx, false); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("Sync(bad) = %v, want ErrCatalogInvalid", err)
	}
	if c := s.Current(); c == nil || c.SHA != "good1" {
		t.Fatalf("live catalog after a bad one: %v, want good1 kept", c)
	}
	st := s.Status()
	want := CatalogStatus{
		Source:         "fake",
		Ready:          true,
		LastGoodSHA:    "good1",
		LoadedAt:       t0,
		LastAttemptSHA: "bad",
		LastSuccess:    t0,
		LastFailure:    t1,
		Errors: []catalog.Error{ // errors only, in the loader's order
			diag(catalog.SeverityError, catalog.CodeSyntax, "rules/broken.yaml", 1),
			diag(catalog.SeverityError, catalog.CodeUnknownQueue, "rules/y.yaml", 9),
		},
	}
	if fmt.Sprintf("%+v", st) != fmt.Sprintf("%+v", want) {
		t.Fatalf("status\n got %+v\nwant %+v", st, want)
	}

	// The same bad head isn't loaded again on the next poll...
	loads := len(src.Loads())
	if err := s.Sync(ctx, false); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("Sync(bad again) = %v", err)
	}
	if n := len(src.Loads()); n != loads {
		t.Fatalf("an unchanged rejected head was loaded again (%d loads, want %d)", n, loads)
	}
	// ...unless forced (resync-catalog).
	if err := s.Sync(ctx, true); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("Sync(bad, force) = %v", err)
	}
	if n := len(src.Loads()); n != loads+1 {
		t.Fatalf("forced sync didn't reload (%d loads)", n)
	}

	// A revert to the live revision is healthy again without a load.
	t2 := clk.Advance(time.Minute)
	src.SetHead("good1", nil)
	if err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	st = s.Status()
	if st.LastAttemptSHA != "good1" || st.Errors != nil || !st.LastSuccess.Equal(t2) || !st.LoadedAt.Equal(t0) {
		t.Fatalf("status after revert = %+v", st)
	}

	// A fixed catalog is swapped in and clears the errors.
	src.SetHead("bad", nil)
	_ = s.Sync(ctx, false)
	t3 := clk.Advance(time.Minute)
	src.SetHead("good2", nil)
	if err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	st = s.Status()
	if s.Current().SHA != "good2" || st.LastGoodSHA != "good2" || st.LastAttemptSHA != "good2" || st.Errors != nil || !st.LoadedAt.Equal(t3) {
		t.Fatalf("status after fix = %+v", st)
	}
}

func TestCatalogSyncStatusBoundsDiagnostics(t *testing.T) {
	ctx := context.Background()
	src := NewFakeCatalogSource()
	var errs, warns []catalog.Error
	for i := range 5 {
		errs = append(errs, diag(catalog.SeverityError, catalog.CodeSchema, "a.yaml", i+1))
		warns = append(warns, diag(catalog.SeverityWarning, catalog.CodeOverlap, "b.yaml", i+1))
	}
	src.Set("good", FakeRevision{Catalog: testCatalog("good"), Diagnostics: warns})
	src.Set("bad", FakeRevision{Diagnostics: errs})
	s, _ := newTestSync(t, src, CatalogSyncOptions{MaxErrors: 2})

	src.SetHead("good", nil)
	if err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); len(st.Warnings) != 2 || st.WarningsOmitted != 3 {
		t.Fatalf("warnings %d (+%d omitted), want 2 (+3)", len(st.Warnings), st.WarningsOmitted)
	}
	src.SetHead("bad", nil)
	_ = s.Sync(ctx, false)
	st := s.Status()
	if len(st.Errors) != 2 || st.ErrorsOmitted != 3 || st.Errors[0].Line != 1 {
		t.Fatalf("errors %v (+%d omitted), want the first 2 (+3)", st.Errors, st.ErrorsOmitted)
	}
	// Status is a copy.
	st.Errors[0].Code = "changed"
	if s.Status().Errors[0].Code == "changed" {
		t.Fatal("Status shares its slices")
	}
}

func TestCatalogSyncSourceErrors(t *testing.T) {
	ctx := context.Background()
	src := NewFakeCatalogSource()
	src.Set("good", FakeRevision{Catalog: testCatalog("good")})
	src.Set("unfetchable", FakeRevision{Err: errors.New("fetch failed")})
	s, clk := newTestSync(t, src, CatalogSyncOptions{})

	src.SetHead("good", nil)
	if err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}

	t1 := clk.Advance(time.Minute)
	src.SetHead("", errors.New("ls-remote: network down"))
	err := s.Sync(ctx, false)
	if err == nil || errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("Sync = %v, want the source error", err)
	}
	st := s.Status()
	if st.SourceError != "ls-remote: network down" || !st.LastFailure.Equal(t1) || st.LastAttemptSHA != "good" || s.Current().SHA != "good" {
		t.Fatalf("status = %+v", st)
	}

	src.SetHead("unfetchable", nil)
	if err := s.Sync(ctx, false); err == nil {
		t.Fatal("want the load error")
	}
	if st := s.Status(); st.SourceError != "fetch failed" || st.LastAttemptSHA != "unfetchable" {
		t.Fatalf("status = %+v", st)
	}
	// A fetch failure is retried on the next poll (unlike a rejected catalog).
	loads := len(src.Loads())
	_ = s.Sync(ctx, false)
	if len(src.Loads()) != loads+1 {
		t.Fatal("a revision that failed to fetch wasn't retried")
	}

	src.SetHead("good", nil)
	if err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); st.SourceError != "" {
		t.Fatalf("SourceError = %q after recovery", st.SourceError)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	before := s.Status()
	if err := s.Sync(cctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync(cancelled) = %v", err)
	}
	if after := s.Status(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("a cancelled sync changed the status:\n%+v\n%+v", before, after)
	}
}

// TestCatalogSyncAtomicSwap swaps catalogs while readers check, under -race, that every
// snapshot they get is whole and that they never go back in time.
func TestCatalogSyncAtomicSwap(t *testing.T) {
	ctx := context.Background()
	src := NewFakeCatalogSource()
	const swaps = 200
	for i := range swaps {
		sha := fmt.Sprintf("v%03d", i)
		src.Set(sha, FakeRevision{Catalog: testCatalog(sha)})
	}
	s, _ := newTestSync(t, src, CatalogSyncOptions{})
	src.SetHead("v000", nil)
	if err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	problems := make(chan string, 16)
	check := func(c *catalog.Catalog, last string) string {
		switch {
		case c == nil:
			problems <- "nil catalog after the first load"
		case c.Providers["marker"].Metadata.Name != c.SHA || c.Queues["marker"].Metadata.Name != c.SHA:
			problems <- fmt.Sprintf("torn snapshot %s: provider %s, queue %s", c.SHA, c.Providers["marker"].Metadata.Name, c.Queues["marker"].Metadata.Name)
		case c.SHA < last:
			problems <- fmt.Sprintf("went back from %s to %s", last, c.SHA)
		default:
			return c.SHA
		}
		return last
	}
	for range 8 {
		wg.Go(func() {
			last := ""
			for {
				select {
				case <-stop:
					return
				default:
				}
				last = check(s.Current(), last)
				c, changed := s.Snapshot()
				last = check(c, last)
				select {
				case <-changed: // a swap happened since: the next Current is newer
					if n := s.Current(); n == c {
						problems <- "changed closed but Current is the same snapshot"
					}
				default:
				}
			}
		})
	}
	for i := 1; i < swaps; i++ {
		src.SetHead(fmt.Sprintf("v%03d", i), nil)
		if err := s.Sync(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	close(problems)
	for p := range problems {
		t.Error(p)
	}
	if c := s.Current(); c.SHA != fmt.Sprintf("v%03d", swaps-1) {
		t.Fatalf("final catalog %s", c.SHA)
	}
}

func TestCatalogSyncRunPollsAndResyncs(t *testing.T) {
	src := NewFakeCatalogSource()
	for _, sha := range []string{"v1", "v2"} {
		src.Set(sha, FakeRevision{Catalog: testCatalog(sha)})
	}
	src.SetHead("v1", nil)
	s, clk := newTestSync(t, src, CatalogSyncOptions{Interval: time.Minute})
	runInBackground(t, s.Run)

	recv(t, s.Ready(), "the first catalog")
	c, changed := s.Snapshot()
	if c.SHA != "v1" {
		t.Fatalf("Current = %s, want v1", c.SHA)
	}

	src.SetHead("v2", nil)
	clk.BlockUntil(1) // the next poll
	clk.Advance(time.Minute)
	recv(t, changed, "the swap to v2 on the next poll")
	c, changed = s.Snapshot()
	if c.SHA != "v2" {
		t.Fatalf("Current = %s, want v2", c.SHA)
	}

	// resync-catalog reloads even though the head didn't move.
	s.Resync()
	recv(t, changed, "the forced reload")
	if got := src.Loads(); len(got) != 3 || got[2] != "v2" {
		t.Fatalf("loads = %v, want [v1 v2 v2]", got)
	}

	// An unreadable source is retried with backoff (first retry within a second), not only
	// at the next poll.
	_, changed = s.Snapshot()
	src.SetHead("", errors.New("down"))
	s.Resync()
	eventually(t, "the failed sync to be scheduled for retry", func() bool {
		return s.ctrl.Queue().NumRequeues(CatalogSyncKey) == 1
	})
	src.SetHead("v1", nil)
	src.Set("v1", FakeRevision{Catalog: testCatalog("v1")})
	clk.Advance(DefaultCatalogRetryBase + DefaultCatalogRetryBase/10)
	recv(t, changed, "the swap after the source recovered")
	if s.Current().SHA != "v1" || s.Status().SourceError != "" {
		t.Fatalf("after recovery: %s, %+v", s.Current().SHA, s.Status())
	}
}
