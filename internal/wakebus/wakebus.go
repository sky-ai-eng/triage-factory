// Package wakebus is the tf_wake NOTIFY doorbell (the spec's
// coordination-fabric table): control (any enqueue) → executors. A
// {kind: run|event, org} publish nudges idle executors to claim within
// milliseconds instead of the dispatcher's scan-interval backstop, which
// remains the guarantee — this is purely a latency win, never the only
// path (the one rule that governs every NOTIFY channel in the fabric, spec
// §5). Mirrors internal/ctlbus's shape exactly: one small file, an execer
// seam so callers don't need a dedicated connection to publish, a fixed
// channel name, and a JSON payload.
package wakebus

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Channel is the Postgres NOTIFY/LISTEN channel name every wake doorbell
// rides.
const Channel = "tf_wake"

// Kind values discriminate WHY a row became claimable — not which table it
// lives in (both kinds nudge the same run-queue claim loop): "run" is a
// fresh RunQueueStore.EnqueueRun (a new blueprint step); "event" is
// ConversationStore.MarkQueuedForResume (resume-by-enqueue — a
// parked/terminal-resumable run woken by an external message).
const (
	KindRun   = "run"
	KindEvent = "event"
)

// Message is the wake payload.
type Message struct {
	Kind  string `json:"kind"`
	OrgID string `json:"org_id"`
}

// execer is the minimal *sql.DB surface Publish needs — a pooled connection
// is fine, pg_notify() doesn't require a dedicated session (unlike LISTEN).
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Publish sends a wake doorbell for orgID via a single pg_notify() call
// over db (the admin pool — NOTIFY needs no session affinity, so any
// pooled connection works; database/sql gives no guarantee it's the same
// physical connection the preceding row-creating write used, and none is
// needed here). What DOES guarantee the notify follows the write durably:
// callers issue the write as a plain autocommit statement and only call
// Publish afterward, sequentially in Go — the write's ExecContext has
// already returned (implying committed) by the time this runs, regardless
// of which connection either statement rode. Best-effort by contract:
// callers ignore a returned error rather than fail the enqueue over it —
// see the package doc's "never the only path" rule; the scan-interval
// backstop always still runs.
func Publish(ctx context.Context, db execer, kind, orgID string) error {
	payload, err := json.Marshal(Message{Kind: kind, OrgID: orgID})
	if err != nil {
		return fmt.Errorf("wakebus: marshal message: %w", err)
	}
	if _, err := db.ExecContext(ctx, `SELECT pg_notify($1, $2)`, Channel, string(payload)); err != nil {
		return fmt.Errorf("wakebus: publish: %w", err)
	}
	return nil
}
