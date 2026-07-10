// Package ctlbus is the cross-process trigger/PollSoon relay (TFAC-583,
// spec §5.3): non-leader callers of a background-brain manager's
// Trigger(orgID) or the poller's PollSoon(source, orgID) publish a
// message on Postgres' tf_ctl NOTIFY channel; the lease holder LISTENs
// and dispatches it to its in-process manager.
//
// This is deliberately lossy — the spec is explicit: "a dropped tf_ctl
// trigger costs one deferred scoring pass (the next system:poll:*
// sentinel re-kicks it). No outbox, no retry, no ordering guarantee. Do
// not build reliability here." NOTIFY payloads are also not durable
// across a LISTEN reconnect, which is the accepted cost of the simple
// design.
package ctlbus

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

var ctlbusLog = logging.Component("ctlbus")

// Channel is the Postgres NOTIFY/LISTEN channel name every relay message
// rides.
const Channel = "tf_ctl"

// Message is the relay payload. Kind selects which field group applies:
// "trigger" uses Manager/OrgID/Force (a scorer/classifier/profiler/
// reconciler Manager.Trigger call); "pollsoon" uses Source/OrgID (a
// poller.Manager.PollSoon call).
type Message struct {
	Kind    string `json:"kind"`
	Manager string `json:"manager,omitempty"`
	OrgID   string `json:"org_id"`
	Source  string `json:"source,omitempty"`
	Force   bool   `json:"force,omitempty"`
}

// execer is the minimal *sql.DB surface Publish needs — a pooled
// connection is fine, pg_notify() doesn't require a dedicated session
// (unlike LISTEN).
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Publish sends msg on Channel via a single pg_notify() call over db (the
// admin pool — no dedicated connection needed to publish). Safe to call
// from any role, holder or not; callers gate on isBrainHolder() themselves
// so a holder never relays to itself.
func Publish(ctx context.Context, db execer, msg Message) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("ctlbus: marshal message: %w", err)
	}
	if _, err := db.ExecContext(ctx, `SELECT pg_notify($1, $2)`, Channel, string(payload)); err != nil {
		return fmt.Errorf("ctlbus: publish: %w", err)
	}
	return nil
}

// Listen holds a dedicated session-mode connection LISTENing on Channel
// and invokes handle for every message received, until ctx is cancelled.
// Reconnects with exponential backoff on connection loss — per the
// package doc, a reconnect gap is an accepted, self-healing loss (the
// next system:poll:* sentinel re-kicks whatever a dropped trigger would
// have), not a correctness bug. Blocks; callers run it in its own
// goroutine.
func Listen(ctx context.Context, dsn string, handle func(Message)) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for ctx.Err() == nil {
		err := listenOnce(ctx, dsn, handle)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			ctlbusLog.Warn("tf_ctl listen connection lost; reconnecting", "error", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = time.Second
	}
}

// listenOnce opens one dedicated connection, LISTENs, and processes
// notifications until the connection errors or ctx is cancelled.
func listenOnce(ctx context.Context, dsn string, handle func(Message)) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	ctlbusLog.Info("tf_ctl listener ready")

	for {
		notif, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err // ctx cancellation surfaces here too; the caller checks ctx.Err()
		}
		var msg Message
		if err := json.Unmarshal([]byte(notif.Payload), &msg); err != nil {
			ctlbusLog.Warn("tf_ctl: malformed message, dropping", "error", err, "payload", notif.Payload)
			continue
		}
		handle(msg)
	}
}
