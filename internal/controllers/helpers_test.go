package controllers

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"
)

// waitTimeout guards every wait in these tests; nothing sleeps.
const waitTimeout = 10 * time.Second

var t0 = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recv receives from ch or fails the test after waitTimeout.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitTimeout):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// eventually waits for cond without sleeping (yielding the processor between checks), failing
// the test after waitTimeout.
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

// getKey takes the next key from q, failing the test if none is ready in time.
func getKey(t *testing.T, q *Queue) string {
	t.Helper()
	type got struct {
		key      string
		shutdown bool
	}
	ch := make(chan got, 1)
	go func() {
		k, s := q.Get()
		ch <- got{k, s}
	}()
	g := recv(t, ch, "a key from the queue")
	if g.shutdown {
		t.Fatal("queue shut down")
	}
	return g.key
}

// runInBackground runs f(ctx) and returns a cancel that stops it and waits for it to return.
func runInBackground(t *testing.T, f func(ctx context.Context) error) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f(ctx) }()
	stopped := false
	var err error
	stop = func() error {
		if !stopped {
			stopped = true
			cancel()
			err = recv(t, done, "Run to return")
		}
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}
