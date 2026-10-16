package memory

import (
	"context"
	"slices"
	"time"

	"github.com/compliance-framework/factory/internal/store"
)

type blob struct {
	data    []byte
	created time.Time
	seq     uint64 // put order: the lowest is the oldest
}

// PutTranscript implements store.TranscriptStore.
func (s *Store) PutTranscript(ctx context.Context, run string, data []byte) error {
	k := store.Key{Kind: runKind, Name: run}
	if err := ctx.Err(); err != nil {
		return store.Errorf("transcript", k, err, "")
	}
	if run == "" {
		return store.Errorf("transcript", k, store.ErrInvalid, "missing run")
	}
	size := int64(len(data))
	if limit := min(s.opts.TranscriptMaxBytes, s.opts.TranscriptTotalBytes); size > limit {
		return store.Errorf("transcript", k, store.ErrTooLarge, "%d bytes, cap %d", size, limit)
	}
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	s.deleteTranscriptLocked(run)
	for s.blobTotal+size > s.opts.TranscriptTotalBytes && len(s.blobs) > 0 {
		oldest, ob := "", (*blob)(nil)
		for name, b := range s.blobs {
			if ob == nil || b.seq < ob.seq {
				oldest, ob = name, b
			}
		}
		s.deleteTranscriptLocked(oldest)
		s.log.Info("store: transcript evicted", "run", oldest, "bytes", len(ob.data),
			"reason", "total cap", "totalCap", s.opts.TranscriptTotalBytes)
	}
	s.blobSeq++
	s.blobs[run] = &blob{data: slices.Clone(data), created: s.clk.Now(), seq: s.blobSeq}
	s.blobTotal += size
	return nil
}

// GetTranscript implements store.TranscriptStore.
func (s *Store) GetTranscript(ctx context.Context, run string) ([]byte, error) {
	k := store.Key{Kind: runKind, Name: run}
	if err := ctx.Err(); err != nil {
		return nil, store.Errorf("transcript", k, err, "")
	}
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	b := s.blobs[run]
	if b == nil {
		return nil, store.Errorf("transcript", k, store.ErrNotFound, "")
	}
	return slices.Clone(b.data), nil
}

func (s *Store) deleteTranscript(run string) {
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	s.deleteTranscriptLocked(run)
}

// deleteTranscriptLocked drops run's transcript, if any. The caller holds s.blobMu.
func (s *Store) deleteTranscriptLocked(run string) {
	if b := s.blobs[run]; b != nil {
		s.blobTotal -= int64(len(b.data))
		delete(s.blobs, run)
	}
}

// sweepTranscripts drops the transcripts past retention and returns how many.
func (s *Store) sweepTranscripts(now time.Time) int {
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	n := 0
	for run, b := range s.blobs {
		if store.Expired(store.SinceCreated, b.created, b.created, now, s.opts.Retention) {
			s.deleteTranscriptLocked(run)
			n++
		}
	}
	return n
}
