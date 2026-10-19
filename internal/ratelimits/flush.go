// Package ratelimits moves the rate-limit readings each process records in
// memory (internal/upstream) into the shared rate_limit_readings table, and
// names the credentials they are metered against for the route that serves
// them.
package ratelimits

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

var log = logging.Component("ratelimits")

// FlushInterval is how often a process writes the readings that changed since
// its last write. A reading in the table is at most this much older than the
// process's own.
const FlushInterval = 15 * time.Second

// SweepInterval is how often the brain deletes readings that no longer
// describe their buckets. The route already leaves them out, so the sweep only
// bounds the table's size.
const SweepInterval = 10 * time.Minute

// RunFlusher writes this process's changed readings to store every interval
// until ctx is cancelled. A failed write is retried on the next tick with
// every reading changed since the last write that succeeded, so a transient
// database fault costs a delay, not a reading.
//
// Every process that makes requests on an org's behalf runs one: each holds
// only the readings of the responses it received, and the newest observation
// of a key wins in the table whichever process wrote it. A nil store makes
// this a logged no-op.
func RunFlusher(ctx context.Context, store db.RateLimitReadingStore, interval time.Duration) {
	if store == nil {
		log.Warn("rate-limit flusher not started: no RateLimitReadingStore wired")
		return
	}
	if interval <= 0 {
		interval = FlushInterval
	}
	var mark uint64
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mark = flush(ctx, store, mark)
		}
	}
}

// flush writes the readings changed since mark and returns the mark to pass
// next: the latest change's when the write succeeded, mark itself when it
// failed.
func flush(ctx context.Context, store db.RateLimitReadingStore, mark uint64) uint64 {
	readings, next := upstream.ChangedReadings(mark)
	if len(readings) == 0 {
		return next
	}
	if err := store.UpsertSystem(ctx, readings); err != nil {
		log.Warn("rate-limit readings not written; retrying next tick", "readings", len(readings), "error", err)
		return mark
	}
	return next
}

// RunExpirySweep deletes every org's expired readings once every interval
// until ctx is cancelled. Brain-gated by its caller: one process sweeps at a
// time. A nil store makes this a logged no-op.
func RunExpirySweep(ctx context.Context, store db.RateLimitReadingStore, interval time.Duration) {
	if store == nil {
		log.Warn("rate-limit expiry sweep not started: no RateLimitReadingStore wired")
		return
	}
	if interval <= 0 {
		interval = SweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := store.DeleteExpiredSystem(ctx, time.Now()); err != nil {
				log.Warn("rate-limit expiry sweep failed; retrying next tick", "error", err)
			}
		}
	}
}
