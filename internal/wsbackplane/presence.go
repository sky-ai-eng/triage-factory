package wsbackplane

import (
	"context"
	"time"
)

const presenceWriteTimeout = 5 * time.Second
const presenceReadTimeout = 2 * time.Second

// RunPresenceHeartbeat upserts one ws_presence row per this pod's
// currently-connected, identified socket on a ~15s timer (TFAC-584,
// TF_WS_PRESENCE_HEARTBEAT_SECONDS) until ctx is cancelled. Every control
// pod runs this — it's what makes PresentFor see a reviewer's tab
// connected to a DIFFERENT pod than the one running the permission check.
//
// Pull-based deliberately: rather than instrumenting Hub.readPump's hot
// path with a DB write on every presence frame, this polls
// Hub.Snapshot() on its own timer, trading up to one heartbeat interval
// of staleness for zero coupling between the socket hot path and
// Postgres.
func (b *Backplane) RunPresenceHeartbeat(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultPresenceHeartbeatInterval
	}
	b.heartbeatOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.heartbeatOnce(ctx)
		}
	}
}

func (b *Backplane) heartbeatOnce(ctx context.Context) {
	snap := b.hub.Snapshot()
	if len(snap) == 0 {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, presenceWriteTimeout)
	defer cancel()

	tx, err := b.db.BeginTx(writeCtx, nil)
	if err != nil {
		backplaneLog.Warn("ws_presence: begin heartbeat tx failed", "error", err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, c := range snap {
		// connID is only unique within this process — qualify with our
		// instance id so two pods' connections can never collide on the
		// (user_id, conn_id) primary key even if their local sequence
		// counters happen to match.
		connID := b.originID + ":" + c.ConnID
		if _, err := tx.ExecContext(writeCtx, `
			INSERT INTO ws_presence (user_id, conn_id, org_id, instance_id, viewing, visible, last_seen)
			VALUES ($1, $2, $3, $4, $5, $6, now())
			ON CONFLICT (user_id, conn_id) DO UPDATE SET
				org_id = EXCLUDED.org_id,
				instance_id = EXCLUDED.instance_id,
				viewing = EXCLUDED.viewing,
				visible = EXCLUDED.visible,
				last_seen = now()
		`, c.UserID, connID, c.OrgID, b.originID, c.Viewing, c.Visible); err != nil {
			backplaneLog.Warn("ws_presence: upsert failed", "user", c.UserID, "error", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		backplaneLog.Warn("ws_presence: commit heartbeat tx failed", "error", err)
		return
	}
	committed = true
}

// PresentFor reports whether anyone is present for a prompt raised by
// runID in orgID (TFAC-392's unattended-prompt fast-deny), checking THIS
// pod's local sockets first (websocket.Hub.PresentFor — cheap, no DB
// round trip) and falling back to the fleet-wide ws_presence table so a
// reviewer connected to a DIFFERENT pod than the one running the
// permission check still counts. A row qualifies under the exact same
// rule Hub.PresentFor applies locally: visible AND (viewing == "board" OR
// viewing == "run:<runID>"), with last_seen inside the live window
// (default 45s, TF_WS_PRESENCE_TTL_SECONDS).
//
// A DB read failure logs and reports absent — the safe default for a
// fast-deny gate is to fail toward requiring an explicit answer, not
// toward silently approving unattended.
func (b *Backplane) PresentFor(ctx context.Context, orgID, runID string) bool {
	if b.hub.PresentFor(orgID, runID) {
		return true
	}
	readCtx, cancel := context.WithTimeout(ctx, presenceReadTimeout)
	defer cancel()

	runView := "run:" + runID
	liveSince := time.Now().Add(-PresenceLiveWindowFromEnv())
	var exists bool
	err := b.db.QueryRowContext(readCtx, `
		SELECT EXISTS (
			SELECT 1 FROM ws_presence
			WHERE org_id = $1
			  AND visible
			  AND (viewing = 'board' OR viewing = $2)
			  AND last_seen > $3
		)
	`, orgID, runView, liveSince).Scan(&exists)
	if err != nil {
		backplaneLog.Warn("ws_presence: read failed; treating as absent for this check", "org", orgID, "run", runID, "error", err)
		return false
	}
	return exists
}
