package memory

import (
	"context"
	"sync"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
)

const runKind = v1alpha1.KindRun

// change is one write, as kept in the history and fanned out to watchers.
type change struct {
	typ store.WatchEventType
	// rec is the new state, or the last state for Deleted.
	rec *record
	// rv is the change's version when it isn't rec's (Deleted).
	rv uint64
	// prevLabels are the labels before a Modified change.
	prevLabels map[string]string
}

func (c *change) version() uint64 {
	if c.rv != 0 {
		return c.rv
	}
	return c.rec.rv
}

// ring is the bounded change history for SinceVersion.
type ring struct {
	buf       []*change
	head, n   int
	trimmedRV uint64 // the newest version dropped from the history; 0 when none was
}

func newRing(size int) ring { return ring{buf: make([]*change, size)} }

func (r *ring) add(c *change) {
	if r.n < len(r.buf) {
		r.buf[(r.head+r.n)%len(r.buf)] = c
		r.n++
		return
	}
	r.trimmedRV = r.buf[r.head].version()
	r.buf[r.head] = c
	r.head = (r.head + 1) % len(r.buf)
}

func (r *ring) each(f func(*change)) {
	for i := range r.n {
		f(r.buf[(r.head+i)%len(r.buf)])
	}
}

// watcher is one Watch. Its channel is only sent on and closed under Store.mu, so a send never
// races a close.
type watcher struct {
	s    *Store
	kind string
	sel  *v1alpha1.LabelSelector
	ch   chan store.WatchEvent
	done chan struct{}

	closed bool // guarded by Store.mu

	errMu sync.Mutex
	err   error
}

var _ store.Watcher = (*watcher)(nil)

func (w *watcher) Events() <-chan store.WatchEvent { return w.ch }

func (w *watcher) Err() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.err
}

func (w *watcher) Stop() {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	w.s.endWatch(w, nil)
}

// event returns what w sees of c, Kubernetes-style for selectors: a change that makes an
// object match is Added, one that makes it stop matching is Deleted. ok is false when w
// doesn't see c at all.
func (w *watcher) event(c *change) (ev store.WatchEvent, ok bool) {
	typ := c.typ
	match := w.sel.Matches(c.rec.labels)
	if c.typ == store.Modified {
		was := w.sel.Matches(c.prevLabels)
		switch {
		case was && !match:
			typ, match = store.Deleted, true
		case !was && match:
			typ = store.Added
		}
	}
	if !match {
		return store.WatchEvent{}, false
	}
	v, err := c.rec.decode()
	if err != nil {
		// Stored data was encoded by this store; decoding it cannot fail short of a bug.
		w.s.log.Error("store: decode for watch", "key", c.rec.key.String(), "err", err)
		return store.WatchEvent{}, false
	}
	obj, _ := v.Interface().(store.Object)
	ver := store.FormatVersion(c.version())
	store.SetResourceVersion(obj, ver)
	return store.WatchEvent{Type: typ, Object: obj, Version: ver}, true
}

// Watch implements store.Store.
func (s *Store) Watch(ctx context.Context, kind string, opts store.WatchOptions) (store.Watcher, error) {
	k := store.Key{Kind: kind}
	if err := ctx.Err(); err != nil {
		return nil, store.Errorf("watch", k, err, "")
	}
	if kind == "" {
		return nil, store.Errorf("watch", k, store.ErrInvalid, "missing kind")
	}
	if err := opts.LabelSelector.Validate(); err != nil {
		return nil, store.Errorf("watch", k, store.ErrInvalid, "%v", err)
	}
	buffer := opts.Buffer
	if buffer <= 0 {
		buffer = s.opts.WatchBuffer
	}
	w := &watcher{s: s, kind: kind, sel: opts.LabelSelector, done: make(chan struct{})}

	s.mu.Lock()
	var replay []store.WatchEvent
	if opts.SinceVersion != "" {
		since, err := store.ParseVersion("watch", k, opts.SinceVersion)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if since > s.rv {
			s.mu.Unlock()
			return nil, store.Errorf("watch", k, store.ErrInvalid, "sinceVersion %d is ahead of the store (%d)", since, s.rv)
		}
		if since < s.history.trimmedRV {
			s.mu.Unlock()
			return nil, store.Errorf("watch", k, store.ErrWatchExpired, "sinceVersion %d is older than the history (from %d)", since, s.history.trimmedRV)
		}
		s.history.each(func(c *change) {
			if c.version() > since && c.rec.key.Kind == kind {
				if ev, ok := w.event(c); ok {
					replay = append(replay, ev)
				}
			}
		})
	}
	w.ch = make(chan store.WatchEvent, len(replay)+buffer)
	for _, ev := range replay {
		w.ch <- ev
	}
	ws := s.watchers[kind]
	if ws == nil {
		ws = map[*watcher]struct{}{}
		s.watchers[kind] = ws
	}
	ws[w] = struct{}{}
	s.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.endWatch(w, store.Errorf("watch", k, ctx.Err(), ""))
			s.mu.Unlock()
		case <-w.done:
		}
	}()
	return w, nil
}

// publish records c in the history and sends it to the kind's watchers without blocking: a
// watcher whose buffer is full is expired. The caller holds s.mu.
func (s *Store) publish(c *change) {
	s.history.add(c)
	for w := range s.watchers[c.rec.key.Kind] {
		ev, ok := w.event(c)
		if !ok {
			continue
		}
		select {
		case w.ch <- ev:
		default:
			s.log.Debug("store: watcher expired (buffer full)", "kind", w.kind, "buffer", cap(w.ch))
			s.endWatch(w, store.Errorf("watch", store.Key{Kind: w.kind}, store.ErrWatchExpired, "buffer of %d full", cap(w.ch)))
		}
	}
}

// endWatch closes w with err (once): nil for Stop, else a *store.Error. The caller holds s.mu.
func (s *Store) endWatch(w *watcher, err error) {
	if w.closed {
		return
	}
	w.closed = true
	w.errMu.Lock()
	w.err = err
	w.errMu.Unlock()
	delete(s.watchers[w.kind], w)
	close(w.ch)
	close(w.done)
}
