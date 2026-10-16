package controllers

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/store"
)

// Informer defaults.
const (
	// DefaultListPageSize is the page size of an informer's (re)list.
	DefaultListPageSize = 500
	// DefaultInformerRetryBase and DefaultInformerRetryMax bound the backoff after a failed
	// list or watch.
	DefaultInformerRetryBase = 100 * time.Millisecond
	DefaultInformerRetryMax  = 30 * time.Second
)

// InformerOptions configure an Informer. Zero fields take the defaults.
type InformerOptions struct {
	// LabelSelector narrows the list and the watch.
	LabelSelector *v1alpha1.LabelSelector
	// Buffer is the watch buffer (0: the store's default). A watcher that falls further
	// behind expires, and the informer resyncs.
	Buffer int
	// PageSize is the list page size. Default 500.
	PageSize int
	// Retry is the backoff after a failed list or watch (not after an expiry, which resyncs
	// at once). Default 100 ms → 30 s.
	Retry Backoff
	// Clock times the retries. Default the wall clock.
	Clock clock.Clock
	// Logger. Default slog.Default().
	Logger *slog.Logger
}

// Informer feeds a handler from one kind of the store with list-then-watch: it lists every
// object (handing each to the handler), then watches from the list's ResourceVersion, so no
// change falls between the two. When the watch expires (store.ErrWatchExpired: the consumer
// fell behind, or the version left the history) it resyncs: lists again and watches from the
// new version. Handlers see each object at least once per change, possibly more (after a
// resync), so reconcilers must be idempotent.
type Informer struct {
	s       store.Store
	kind    string
	handler func(store.Object)
	opts    InformerOptions
	log     *slog.Logger

	resyncs atomic.Uint64
	synced  chan struct{}
	once    sync.Once
}

// NewInformer returns an informer over kind in s that calls handler for every listed object
// and every changed one (the last state of a deleted one). handler runs on the informer's
// goroutine; it must not block for long, or the watch expires.
func NewInformer(s store.Store, kind string, handler func(store.Object), opts InformerOptions) *Informer {
	if opts.PageSize <= 0 {
		opts.PageSize = DefaultListPageSize
	}
	if opts.Retry.Base <= 0 {
		opts.Retry.Base = DefaultInformerRetryBase
	}
	if opts.Retry.Max <= 0 {
		opts.Retry.Max = DefaultInformerRetryMax
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Informer{
		s: s, kind: kind, handler: handler, opts: opts,
		log:    opts.Logger.With("informer", kind),
		synced: make(chan struct{}),
	}
}

// Synced is closed once the first list has been handed to the handler.
func (i *Informer) Synced() <-chan struct{} { return i.synced }

// Resyncs returns how many times the informer re-listed after its watch expired.
func (i *Informer) Resyncs() uint64 { return i.resyncs.Load() }

// Run lists and watches until ctx is done.
func (i *Informer) Run(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		err := i.listAndWatch(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, store.ErrWatchExpired):
			i.resyncs.Add(1)
			failures = 0
			i.log.Info("watch expired; resyncing")
		case err != nil:
			failures++
			d := i.opts.Retry.Delay(failures)
			i.log.Warn("list/watch failed; retrying", "in", d, "err", err)
			if !sleep(ctx, i.opts.Clock, d) {
				return
			}
		default:
			failures = 0 // the watch ended cleanly (stopped): start over
		}
	}
}

// listAndWatch lists kind, then watches it from the list's version until the watch ends,
// returning why it ended.
func (i *Informer) listAndWatch(ctx context.Context) error {
	version := ""
	cursor := ""
	for {
		var page []store.Object
		meta, err := i.s.List(ctx, i.kind, store.ListOptions{
			LabelSelector: i.opts.LabelSelector, Cursor: cursor, Limit: i.opts.PageSize,
		}, &page)
		if err != nil {
			return err
		}
		if version == "" {
			// The first page's version: changes made while later pages are read are replayed
			// by the watch (the handler may then see an object twice; that's fine).
			version = meta.ResourceVersion
		}
		for _, obj := range page {
			i.handler(obj)
		}
		if meta.NextCursor == "" {
			break
		}
		cursor = meta.NextCursor
	}
	i.once.Do(func() { close(i.synced) })

	w, err := i.s.Watch(ctx, i.kind, store.WatchOptions{
		LabelSelector: i.opts.LabelSelector, SinceVersion: version, Buffer: i.opts.Buffer,
	})
	if err != nil {
		return err
	}
	defer w.Stop()
	for ev := range w.Events() {
		i.handler(ev.Object)
	}
	return w.Err()
}

// sleep waits d on clk; false when ctx ended first.
func sleep(ctx context.Context, clk clock.Clock, d time.Duration) bool {
	t := clk.NewTimerAt(clk.Now().Add(d))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C():
		return true
	}
}
