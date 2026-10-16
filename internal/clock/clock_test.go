package clock

import (
	"sync"
	"testing"
	"time"
)

func TestFake(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := NewFake(start)
	if got := f.Now(); !got.Equal(start) {
		t.Fatalf("Now = %v, want %v", got, start)
	}
	if got := f.Advance(time.Hour); !got.Equal(start.Add(time.Hour)) {
		t.Fatalf("Advance = %v", got)
	}
	f.Set(start)
	if got := f.Now(); !got.Equal(start) {
		t.Fatalf("after Set: %v", got)
	}

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Advance(time.Second)
			_ = f.Now()
		}()
	}
	wg.Wait()
	if got := f.Now(); !got.Equal(start.Add(10 * time.Second)) {
		t.Fatalf("after concurrent Advance: %v", got)
	}
}

func TestReal(t *testing.T) {
	var c Clock = Real{}
	if c.Now().IsZero() {
		t.Fatal("Real.Now is zero")
	}
}
