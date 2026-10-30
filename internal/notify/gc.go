package notify

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// RetentionGCInterval is how often RunRetentionGC deletes retired rows.
const RetentionGCInterval = time.Hour

// RunRetentionGC deletes every org's rows resolved past their retention and
// notices past their expiry when it starts, then once every interval until
// ctx is cancelled. The first pass is immediate because the brain starts this
// on every promotion. Brain-gated by its caller: one process sweeps at a time.
// A nil store makes this a logged no-op.
func RunRetentionGC(ctx context.Context, store db.NotificationStore, interval time.Duration) {
	if store == nil {
		notifyLog.Warn("notification retention GC not started: no NotificationStore wired")
		return
	}
	if interval <= 0 {
		interval = RetentionGCInterval
	}
	sweep := func() {
		if _, err := store.DeleteRetiredSystem(ctx, time.Now()); err != nil {
			notifyLog.Warn("notification retention GC failed; retrying next tick", "error", err)
		}
	}
	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
