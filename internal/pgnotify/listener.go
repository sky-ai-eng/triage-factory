// Package pgnotify provides the small, dedicated-connection LISTEN/NOTIFY
// primitive the coordination fabric needs (docs/specs/horizontal-scaling/
// README.md §5): "each pod holds 1-2 dedicated direct connections for
// LISTEN (session-scoped — they must bypass any transaction-mode pooler)."
//
// One rule governs every channel built on this package: NOTIFY is a
// doorbell — never the data, and never the only path. A Listener's
// reconnect loop means a dropped connection only DELAYS delivery; callers
// are responsible for pairing every Listener with an independent backstop
// poll of the backing table so a delay can never become a loss.
package pgnotify

import (
	"context"
	"database/sql"
	"time"

	"github.com/jackc/pgx/v5"
)

// initialBackoff / maxBackoff bound the reconnect loop's exponential
// backoff after a lost LISTEN connection.
const (
	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second
)

// Listener maintains one dedicated LISTEN connection to a single Postgres
// channel, dispatching every notification's payload to onNotify and
// reconnecting with backoff on any error. It never touches the
// application's pooled *sql.DB — pgx.Connect opens its own direct
// connection, so a pool reset/eviction can never silently drop the LISTEN
// registration underneath a caller that doesn't know to reissue it.
type Listener struct {
	dsn      string
	channel  string
	onNotify func(payload string)
}

// NewListener builds a Listener against dsn (a full Postgres connection
// string) for channel. onNotify is called synchronously from the
// connection's read loop for every notification received — it must not
// block for long, since a slow handler stalls delivery of the next
// notification on this same connection (callers that need to fan out work
// should hand off to a buffered channel/goroutine inside onNotify).
func NewListener(dsn, channel string, onNotify func(payload string)) *Listener {
	return &Listener{dsn: dsn, channel: channel, onNotify: onNotify}
}

// Run connects, issues LISTEN, and dispatches notifications until ctx is
// cancelled, reconnecting with exponential backoff on any connection
// error. Blocks until ctx is done — callers run it in its own goroutine.
func (l *Listener) Run(ctx context.Context) {
	backoff := initialBackoff
	for ctx.Err() == nil {
		connected, err := l.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			pgnotifyLog.Warn("listen connection lost; reconnecting", "channel", l.channel, "error", err, "backoff", backoff)
		}
		// A connection that made it to LISTEN before eventually failing
		// resets the backoff — only a run of back-to-back failures (never
		// reaching a live LISTEN) should grow the delay.
		if connected {
			backoff = initialBackoff
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if !connected {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// runOnce opens one direct connection, LISTENs, and dispatches
// notifications until the connection errors or ctx is cancelled. connected
// reports whether LISTEN was successfully issued (true even if the
// connection later dropped) — the caller's backoff only grows across
// failures that never reach a live LISTEN. A clean ctx cancellation
// returns (true-or-false, nil); anything else is a connection-loss error
// the caller's Run loop backs off and retries on.
func (l *Listener) runOnce(ctx context.Context) (connected bool, err error) {
	conn, err := pgx.Connect(ctx, l.dsn)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(context.Background()) }()

	if _, err := conn.Exec(ctx, `LISTEN `+pgx.Identifier{l.channel}.Sanitize()); err != nil {
		return false, err
	}
	pgnotifyLog.Info("listening", "channel", l.channel)
	for {
		notif, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, err
		}
		if l.onNotify != nil {
			l.onNotify(notif.Payload)
		}
	}
}

// Notify publishes a payload on channel via pg_notify(), using the
// caller's ordinary pooled connection — unlike LISTEN, NOTIFY needs no
// session affinity, so it rides whatever connection the caller already
// has (typically the same admin pool that just wrote the row the
// notification announces, so the notify is issued only after that write
// is durably committed).
func Notify(ctx context.Context, db *sql.DB, channel, payload string) error {
	_, err := db.ExecContext(ctx, `SELECT pg_notify($1, $2)`, channel, payload)
	return err
}
