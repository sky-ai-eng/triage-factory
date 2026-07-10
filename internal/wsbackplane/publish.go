package wsbackplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// publishTimeout bounds every tf_ws/tf_ctl/tf_bus write — these are
// best-effort NOTIFY publishes on the request/broadcast hot path, so a
// wedged connection must not hang the caller.
const publishTimeout = 3 * time.Second

// Publish implements websocket.Backplane: it fans evt onto tf_ws so
// every OTHER control pod's Hub mirrors it into its own local sockets.
// Called by Hub.Broadcast AFTER it has already delivered evt to this
// pod's own local sockets — Publish never touches this pod's Hub, only
// Postgres, which is what keeps the origin pod from ever double-delivering
// to its own clients (see DeliverRemote's doc comment for the other half
// of that invariant).
//
// Failures are logged and swallowed: a lost tf_ws NOTIFY is the
// documented lossy contract (a UI blip other pods' clients absorb via
// their normal refetch-on-focus/poll behavior), never a reason to fail
// the caller's request.
func (b *Backplane) Publish(evt websocket.Event) {
	we, err := newWireEvent(evt)
	if err != nil {
		backplaneLog.Warn("tf_ws: marshal event failed; not publishing cross-pod", "type", evt.Type, "error", err)
		return
	}
	raw, err := json.Marshal(we)
	if err != nil {
		backplaneLog.Warn("tf_ws: marshal wire event failed; not publishing cross-pod", "type", evt.Type, "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()

	if len(raw) <= b.inlineMaxBytes {
		env := wsEnvelope{OriginInstanceID: b.originID, Event: &we}
		if b.notify(ctx, b.db, ChannelWS, env) {
			return
		}
		// notify already logged the failure; nothing left to try inline.
		return
	}
	b.publishViaOutbox(ctx, evt.Type, raw)
}

// publishViaOutbox inserts payload into ws_outbox and NOTIFYs a row
// reference in the SAME transaction — so a reader can never observe a
// ref whose row isn't visible yet, and a rolled-back tx can never emit a
// NOTIFY for a row that doesn't exist (spec §5's "NOTIFY inside the same
// tx as the underlying write" rule).
func (b *Backplane) publishViaOutbox(ctx context.Context, evtType string, payload []byte) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		backplaneLog.Warn("tf_ws: begin outbox tx failed; event stays local-only on this pod", "type", evtType, "error", err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var outboxID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO ws_outbox (payload) VALUES ($1) RETURNING id`, payload,
	).Scan(&outboxID); err != nil {
		backplaneLog.Warn("tf_ws: insert outbox row failed; event stays local-only on this pod", "type", evtType, "error", err)
		return
	}

	env := wsEnvelope{OriginInstanceID: b.originID, OutboxID: outboxID}
	if !b.notify(ctx, tx, ChannelWS, env) {
		return
	}
	if err := tx.Commit(); err != nil {
		backplaneLog.Warn("tf_ws: commit outbox tx failed; event stays local-only on this pod", "type", evtType, "outbox_id", outboxID, "error", err)
		return
	}
	committed = true
}

// execer is the sql.DB / sql.Tx overlap notify needs — lets outbox
// publishes NOTIFY on the same transaction as their row insert while
// every other publisher just passes the pool directly.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// notify marshals env and issues `SELECT pg_notify(channel, payload)` on
// q. Returns false (and logs) on any marshal or exec failure so callers
// can bail out of whatever transaction they're mid-way through.
func (b *Backplane) notify(ctx context.Context, q execer, channel string, env any) bool {
	payload, err := json.Marshal(env)
	if err != nil {
		backplaneLog.Warn("marshal NOTIFY envelope failed", "channel", channel, "error", err)
		return false
	}
	if _, err := q.ExecContext(ctx, `SELECT pg_notify($1, $2)`, channel, string(payload)); err != nil {
		backplaneLog.Warn("NOTIFY failed", "channel", channel, "error", err)
		return false
	}
	return true
}

// handleWSNotification applies a received tf_ws envelope: resolves the
// inline event or fetches its ws_outbox row, then delivers it to this
// pod's local sockets via DeliverRemote — never Broadcast, which would
// re-publish it (no echo loops, by construction; see DeliverRemote's doc
// comment).
func (b *Backplane) handleWSNotification(payload string) {
	var env wsEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		backplaneLog.Warn("tf_ws: malformed envelope; dropping", "error", err)
		return
	}
	if env.OriginInstanceID == b.originID {
		// Already delivered locally at publish time — see Publish's doc
		// comment for why this is the origin pod's half of the no-echo
		// invariant.
		return
	}

	var evt websocket.Event
	if env.Event != nil {
		evt = env.Event.toEvent()
	} else if env.OutboxID != 0 {
		fetched, ok := b.fetchOutbox(env.OutboxID)
		if !ok {
			return
		}
		evt = fetched
	} else {
		backplaneLog.Warn("tf_ws: envelope carries neither an inline event nor an outbox ref; dropping")
		return
	}
	b.hub.DeliverRemote(evt)
}

// fetchOutbox reads a ws_outbox row by id. A missing row (TTL-reaped, or
// the publishing tx rolled back after its NOTIFY was already buffered by
// a listener) is not an error — it's the documented lossy contract, so
// this drops the event with a debug log rather than warning.
// Named returns so every early-exit path is a bare `return` — evt stays
// the zero Event and ok stays false without a websocket.Event{} literal
// at every drop site (pkg/websocket's broadcast_callsite_test.go asserts
// every SUCH literal outside pkg/websocket stamps OrgID; these are
// discarded placeholders, not events anyone delivers, so a zero value
// via naked return is both correct and keeps that scan focused on real
// call sites).
func (b *Backplane) fetchOutbox(id int64) (evt websocket.Event, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()

	var raw []byte
	err := b.db.QueryRowContext(ctx, `SELECT payload FROM ws_outbox WHERE id = $1`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		backplaneLog.Debug("tf_ws: outbox row missing (TTL-reaped or rolled-back publish); dropping event", "outbox_id", id)
		return
	}
	if err != nil {
		backplaneLog.Warn("tf_ws: fetch outbox row failed; dropping event", "outbox_id", id, "error", err)
		return
	}
	var we wireEvent
	if err := json.Unmarshal(raw, &we); err != nil {
		backplaneLog.Warn("tf_ws: malformed outbox payload; dropping event", "outbox_id", id, "error", err)
		return
	}
	return we.toEvent(), true
}
