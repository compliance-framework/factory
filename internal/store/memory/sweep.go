package memory

import (
	"context"

	"github.com/compliance-framework/factory/internal/store"
)

// Sweep implements store.Sweeper: it deletes the objects and transcripts past retention
// (store.RetentionOf), telling watchers, and drops expired locks. Run it with store.RunSweeper.
func (s *Store) Sweep(ctx context.Context) (store.SweepResult, error) {
	res := store.SweepResult{Objects: map[string]int{}}
	if err := ctx.Err(); err != nil {
		return res, store.Errorf("sweep", store.Key{}, err, "")
	}
	now := s.clk.Now()

	s.mu.Lock()
	var expired []*record
	for _, byName := range s.objects {
		for _, rec := range byName {
			if store.Expired(rec.retention, rec.created, rec.updated, now, s.opts.Retention) {
				expired = append(expired, rec)
			}
		}
	}
	for _, rec := range expired {
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return res, store.Errorf("sweep", store.Key{}, err, "")
		}
		s.deleteLocked(rec)
		res.Objects[rec.key.Kind]++
	}
	s.mu.Unlock()

	res.Transcripts = s.sweepTranscripts(now)
	s.sweepLocks(now)
	if len(expired) > 0 || res.Transcripts > 0 {
		s.log.InfoContext(ctx, "store: swept", "objects", res.Objects, "transcripts", res.Transcripts,
			"retention", s.opts.Retention.String())
	}
	return res, nil
}
