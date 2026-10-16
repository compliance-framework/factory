package controllers

import (
	"container/heap"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/clock"
)

// Backoff defaults (the LLD's 5 ms → 5 min exponential backoff with jitter).
const (
	DefaultBaseDelay = 5 * time.Millisecond
	DefaultMaxDelay  = 5 * time.Minute
	DefaultJitter    = 0.1
)

// Backoff is an exponential per-key retry delay: Base·2^(failures-1), plus up to Jitter of
// it at random, capped at Max.
type Backoff struct {
	Base, Max time.Duration
	// Jitter is the fraction of the delay added at random (0.1 adds up to 10%); 0 disables it.
	Jitter float64
	// Rand returns a float in [0, 1); nil is math/rand/v2.Float64.
	Rand func() float64
}

func (b Backoff) withDefaults() Backoff {
	if b.Base <= 0 {
		b.Base = DefaultBaseDelay
	}
	if b.Max <= 0 {
		b.Max = DefaultMaxDelay
	}
	if b.Max < b.Base {
		b.Max = b.Base
	}
	if b.Jitter < 0 {
		b.Jitter = 0
	}
	if b.Rand == nil {
		b.Rand = rand.Float64
	}
	return b
}

// Delay returns the delay before retry number failures (1 for the first retry).
func (b Backoff) Delay(failures int) time.Duration {
	b = b.withDefaults()
	if failures < 1 {
		failures = 1
	}
	d := float64(b.Base) * math.Pow(2, float64(failures-1))
	if b.Jitter > 0 {
		d += d * b.Jitter * b.Rand()
	}
	if d >= float64(b.Max) {
		return b.Max
	}
	return time.Duration(d)
}

// Queue is a keyed work queue:
//
//   - a key is queued at most once however often it is added (dedupe);
//   - a key is handed to at most one worker at a time: a key added while it is being processed
//     is queued again only when the worker calls Done;
//   - keys can be added after a delay (AddAfter) or after a per-key exponential backoff
//     (AddRateLimited, reset by Forget), timed on the injected Clock.
//
// The zero Queue is not usable; use NewQueue. ShutDown stops it.
type Queue struct {
	clk     clock.Clock
	backoff Backoff

	mu         sync.Mutex
	cond       *sync.Cond
	queue      []string
	dirty      map[string]struct{} // queued, or added again while processing
	processing map[string]struct{}
	failures   map[string]int
	waiting    delayHeap
	waitingIdx map[string]*delayed
	shutdown   bool

	wake     chan struct{} // nudges the delay loop when an earlier delayed key arrives
	stop     chan struct{}
	loopDone chan struct{}
}

// NewQueue returns a running queue (its delay loop runs until ShutDown). A nil clk is the
// wall clock.
func NewQueue(clk clock.Clock, backoff Backoff) *Queue {
	if clk == nil {
		clk = clock.Real{}
	}
	q := &Queue{
		clk:        clk,
		backoff:    backoff.withDefaults(),
		dirty:      map[string]struct{}{},
		processing: map[string]struct{}{},
		failures:   map[string]int{},
		waitingIdx: map[string]*delayed{},
		wake:       make(chan struct{}, 1),
		stop:       make(chan struct{}),
		loopDone:   make(chan struct{}),
	}
	q.cond = sync.NewCond(&q.mu)
	go q.delayLoop()
	return q
}

// Add queues key unless it is already queued. Adds after ShutDown are ignored.
func (q *Queue) Add(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.addLocked(key)
}

func (q *Queue) addLocked(key string) {
	if q.shutdown {
		return
	}
	if _, ok := q.dirty[key]; ok {
		return
	}
	q.dirty[key] = struct{}{}
	if _, ok := q.processing[key]; ok {
		return // Done queues it
	}
	q.queue = append(q.queue, key)
	q.cond.Signal()
}

// AddAfter queues key once d has passed on the queue's clock (at once when d <= 0). A key
// already waiting keeps the earlier of its two deadlines.
func (q *Queue) AddAfter(key string, d time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.addAfterLocked(key, d)
}

func (q *Queue) addAfterLocked(key string, d time.Duration) {
	if q.shutdown {
		return
	}
	if d <= 0 {
		q.addLocked(key)
		return
	}
	at := q.clk.Now().Add(d)
	if e, ok := q.waitingIdx[key]; ok {
		if !at.Before(e.at) {
			return
		}
		e.at = at
		heap.Fix(&q.waiting, e.index)
	} else {
		e := &delayed{key: key, at: at}
		heap.Push(&q.waiting, e)
		q.waitingIdx[key] = e
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// AddRateLimited counts a failure for key and queues it after its backoff delay.
func (q *Queue) AddRateLimited(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shutdown {
		return
	}
	q.failures[key]++
	q.addAfterLocked(key, q.backoff.Delay(q.failures[key]))
}

// Forget resets key's failure count (call it when key was processed successfully).
func (q *Queue) Forget(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.failures, key)
}

// NumRequeues returns key's failure count since the last Forget.
func (q *Queue) NumRequeues(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.failures[key]
}

// Len returns the number of keys ready for a worker (not the delayed ones).
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue)
}

// Get blocks until a key is ready and hands it to the caller, who must call Done(key) when
// finished. shutdown is true (and key empty) once the queue is shut down.
func (q *Queue) Get() (key string, shutdown bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.queue) == 0 && !q.shutdown {
		q.cond.Wait()
	}
	if q.shutdown {
		return "", true
	}
	key = q.queue[0]
	q.queue[0] = ""
	q.queue = q.queue[1:]
	delete(q.dirty, key)
	q.processing[key] = struct{}{}
	return key, false
}

// Done marks key as processed; if it was added again meanwhile it is queued now.
func (q *Queue) Done(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.processing, key)
	if _, ok := q.dirty[key]; ok && !q.shutdown {
		q.queue = append(q.queue, key)
		q.cond.Signal()
	}
}

// ShutDown stops the queue: Get returns shutdown to every waiting and future caller, adds
// are ignored and the delay loop exits. Keys still queued are dropped. It is idempotent.
func (q *Queue) ShutDown() {
	q.mu.Lock()
	if q.shutdown {
		q.mu.Unlock()
		<-q.loopDone
		return
	}
	q.shutdown = true
	q.cond.Broadcast()
	close(q.stop)
	q.mu.Unlock()
	<-q.loopDone
}

// ShuttingDown reports whether ShutDown was called.
func (q *Queue) ShuttingDown() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.shutdown
}

// delayLoop moves delayed keys into the queue when they are due.
func (q *Queue) delayLoop() {
	defer close(q.loopDone)
	for {
		q.mu.Lock()
		now := q.clk.Now()
		for len(q.waiting) > 0 && !q.waiting[0].at.After(now) {
			e := heap.Pop(&q.waiting).(*delayed)
			delete(q.waitingIdx, e.key)
			q.addLocked(e.key)
		}
		var timer clock.Timer
		var fire <-chan time.Time
		if len(q.waiting) > 0 {
			timer = q.clk.NewTimerAt(q.waiting[0].at)
			fire = timer.C()
		}
		q.mu.Unlock()

		select {
		case <-q.stop:
		case <-q.wake:
		case <-fire:
		}
		if timer != nil {
			timer.Stop()
		}
		select {
		case <-q.stop:
			return
		default:
		}
	}
}

// delayed is a key waiting for its deadline.
type delayed struct {
	key   string
	at    time.Time
	index int
}

// delayHeap is a min-heap of delayed keys by deadline.
type delayHeap []*delayed

func (h delayHeap) Len() int           { return len(h) }
func (h delayHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h delayHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *delayHeap) Push(x any) {
	e := x.(*delayed)
	e.index = len(*h)
	*h = append(*h, e)
}
func (h *delayHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return e
}
