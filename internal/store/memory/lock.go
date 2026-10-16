package memory

import (
	"context"
	"time"

	"github.com/compliance-framework/factory/internal/store"
)

// lockPoll bounds how long a waiting Lock sleeps before re-checking expiry: expiry follows the
// injected clock, which wakes nobody when it moves.
const lockPoll = 20 * time.Millisecond

type lockState struct {
	token   uint64
	expires time.Time
}

// lease is a held lock.
type lease struct {
	s     *Store
	name  string
	token uint64
}

var _ store.Unlock = (*lease)(nil)

// Lock implements store.Store.
func (s *Store) Lock(ctx context.Context, name string, ttl time.Duration) (store.Unlock, error) {
	k := store.Key{Name: name}
	if name == "" {
		return nil, store.Errorf("lock", k, store.ErrInvalid, "missing name")
	}
	if ttl <= 0 {
		return nil, store.Errorf("lock", k, store.ErrInvalid, "ttl must be positive, got %s", ttl)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, store.Errorf("lock", k, err, "")
		}
		s.lockMu.Lock()
		now := s.clk.Now()
		st := s.locks[name]
		if st == nil || !now.Before(st.expires) {
			s.lockSeq++
			s.locks[name] = &lockState{token: s.lockSeq, expires: now.Add(ttl)}
			l := &lease{s: s, name: name, token: s.lockSeq}
			s.lockMu.Unlock()
			return l, nil
		}
		wake := s.lockWake
		wait := min(max(st.expires.Sub(now), time.Millisecond), lockPoll)
		s.lockMu.Unlock()

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// Unlock implements store.Unlock.
func (l *lease) Unlock() {
	s := l.s
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	if st := s.locks[l.name]; st != nil && st.token == l.token {
		s.releaseLocked(l.name)
	}
}

// Renew implements store.Unlock.
func (l *lease) Renew(ctx context.Context, ttl time.Duration) error {
	k := store.Key{Name: l.name}
	if err := ctx.Err(); err != nil {
		return store.Errorf("renew", k, err, "")
	}
	if ttl <= 0 {
		return store.Errorf("renew", k, store.ErrInvalid, "ttl must be positive, got %s", ttl)
	}
	s := l.s
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	st := s.locks[l.name]
	if st == nil || st.token != l.token {
		return store.Errorf("renew", k, store.ErrLockLost, "released or taken over")
	}
	now := s.clk.Now()
	if !now.Before(st.expires) {
		s.releaseLocked(l.name)
		return store.Errorf("renew", k, store.ErrLockLost, "expired at %s", st.expires.Format(time.RFC3339Nano))
	}
	st.expires = now.Add(ttl)
	return nil
}

// releaseLocked drops lock name and wakes the waiters. The caller holds s.lockMu.
func (s *Store) releaseLocked(name string) {
	delete(s.locks, name)
	close(s.lockWake)
	s.lockWake = make(chan struct{})
}

// sweepLocks drops expired locks nobody reclaimed, so the map doesn't grow with dead names.
func (s *Store) sweepLocks(now time.Time) {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	for name, st := range s.locks {
		if !now.Before(st.expires) {
			s.releaseLocked(name)
		}
	}
}
