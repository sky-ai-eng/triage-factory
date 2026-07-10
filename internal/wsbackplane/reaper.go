package wsbackplane

import (
	"context"
	"time"
)

// RunOutboxReaper deletes ws_outbox rows older than ttl on a ticker until
// ctx is cancelled. Deliberately NOT leader-gated (spec §11 decision 5 /
// §5's ws_outbox lifecycle note: "any pod may run the reaper... run it on
// every pod's slow timer rather than brain-gating something this
// trivial") — `DELETE WHERE created_at < cutoff` is concurrency-safe
// under any number of pods racing it.
func (b *Backplane) RunOutboxReaper(ctx context.Context, interval, ttl time.Duration) {
	if interval <= 0 {
		interval = DefaultOutboxReapInterval
	}
	if ttl <= 0 {
		ttl = DefaultOutboxTTL
	}
	b.reapOutboxOnce(ctx, ttl)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.reapOutboxOnce(ctx, ttl)
		}
	}
}

func (b *Backplane) reapOutboxOnce(ctx context.Context, ttl time.Duration) {
	reapCtx, cancel := context.WithTimeout(ctx, presenceWriteTimeout)
	defer cancel()
	cutoff := time.Now().Add(-ttl)
	res, err := b.db.ExecContext(reapCtx, `DELETE FROM ws_outbox WHERE created_at < $1`, cutoff)
	if err != nil {
		backplaneLog.Warn("ws_outbox: reap failed", "error", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		backplaneLog.Debug("ws_outbox: reaped stale rows", "rows", n)
	}
}
