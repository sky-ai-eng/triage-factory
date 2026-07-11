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

// heartbeatOnce upserts every locally-connected socket in one round trip
// via unnest()-based array binding (the same idiom
// internal/db/postgres/team_github_repos.go uses), rather than one
// ExecContext per connection inside a held-open transaction — a pod with
// thousands of live sockets would otherwise turn every heartbeat tick
// into thousands of sequential round trips serialized behind one
// long-lived transaction. A single multi-row INSERT is already atomic on
// its own, so no explicit transaction is needed here at all.
func (b *Backplane) heartbeatOnce(ctx context.Context) {
	snap := b.hub.Snapshot()
	if len(snap) == 0 {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, presenceWriteTimeout)
	defer cancel()

	userIDs := make([]string, len(snap))
	connIDs := make([]string, len(snap))
	orgIDs := make([]string, len(snap))
	viewings := make([]string, len(snap))
	visibles := make([]bool, len(snap))
	for i, c := range snap {
		userIDs[i] = c.UserID
		// connID is only unique within this process — qualify with our
		// instance id so two pods' connections can never collide on the
		// (user_id, conn_id) primary key even if their local sequence
		// counters happen to match.
		connIDs[i] = b.originID + ":" + c.ConnID
		orgIDs[i] = c.OrgID
		viewings[i] = c.Viewing
		visibles[i] = c.Visible
	}

	if _, err := b.db.ExecContext(writeCtx, `
		INSERT INTO ws_presence (user_id, conn_id, org_id, instance_id, viewing, visible, last_seen)
		SELECT t.user_id, t.conn_id, t.org_id, $1, t.viewing, t.visible, now()
		FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::bool[])
			AS t(user_id, conn_id, org_id, viewing, visible)
		ON CONFLICT (user_id, conn_id) DO UPDATE SET
			org_id = EXCLUDED.org_id,
			instance_id = EXCLUDED.instance_id,
			viewing = EXCLUDED.viewing,
			visible = EXCLUDED.visible,
			last_seen = now()
	`, b.originID, userIDs, connIDs, orgIDs, viewings, visibles); err != nil {
		backplaneLog.Warn("ws_presence: batch upsert failed", "connections", len(snap), "error", err)
		return
	}
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
	// The live window is measured against the DB clock (now()), not this
	// pod's: last_seen is stamped with the server's now() on every heartbeat,
	// so a pod whose clock runs ahead would otherwise judge genuinely-present
	// reviewers absent and fire the unattended fast-deny with someone watching.
	var exists bool
	err := b.db.QueryRowContext(readCtx, `
		SELECT EXISTS (
			SELECT 1 FROM ws_presence
			WHERE org_id = $1
			  AND visible
			  AND (viewing = 'board' OR viewing = $2)
			  AND last_seen > now() - make_interval(secs => $3)
		)
	`, orgID, runView, PresenceLiveWindowFromEnv().Seconds()).Scan(&exists)
	if err != nil {
		backplaneLog.Warn("ws_presence: read failed; treating as absent for this check", "org", orgID, "run", runID, "error", err)
		return false
	}
	return exists
}
