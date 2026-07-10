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

// RunPresenceReaper deletes ws_presence rows older than ttl on a ticker
// until ctx is cancelled — a pure storage-hygiene pass, distinct from
// PresentFor's read-time live-window filter (see the migration comment
// on ws_presence and DefaultPresenceRowTTL's doc comment for why the two
// don't share a TTL). Every reconnect mints a brand-new conn_id
// (websocket.Hub.connSeq is a per-boot monotonic counter, never reused),
// so — unlike ws_outbox, where rows are naturally short-lived — nothing
// ever overwrites a dead connection's row; without this reaper the table
// grows without bound under ordinary reconnect churn (tab refreshes,
// network blips, RevalidateSessions kicks) over weeks of uptime.
// Deliberately NOT leader-gated, same rationale as RunOutboxReaper: any
// pod's `DELETE WHERE last_seen < cutoff` is concurrency-safe regardless
// of how many pods race it.
func (b *Backplane) RunPresenceReaper(ctx context.Context, interval, ttl time.Duration) {
	if interval <= 0 {
		interval = DefaultPresenceReapInterval
	}
	if ttl <= 0 {
		ttl = DefaultPresenceRowTTL
	}
	b.reapPresenceOnce(ctx, ttl)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.reapPresenceOnce(ctx, ttl)
		}
	}
}

func (b *Backplane) reapPresenceOnce(ctx context.Context, ttl time.Duration) {
	reapCtx, cancel := context.WithTimeout(ctx, presenceWriteTimeout)
	defer cancel()
	cutoff := time.Now().Add(-ttl)
	res, err := b.db.ExecContext(reapCtx, `DELETE FROM ws_presence WHERE last_seen < $1`, cutoff)
	if err != nil {
		backplaneLog.Warn("ws_presence: reap failed", "error", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		backplaneLog.Debug("ws_presence: reaped stale rows", "rows", n)
	}
}
