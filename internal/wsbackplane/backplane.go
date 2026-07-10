// Package wsbackplane fans websocket.Hub events, session kicks, and
// brain-bound run sentinels across a multi-pod deployment over Postgres
// LISTEN/NOTIFY (docs/specs/horizontal-scaling/README.md §5, TFAC-584).
//
// Three channels, one Backplane:
//
//   - tf_ws — every control pod LISTENs; Broadcast publishes here and
//     every OTHER pod mirrors the event into its own local sockets
//     (Publish / RunPublicListener / DeliverRemote).
//   - tf_ctl — the cross-pod session kick; every control pod LISTENs and
//     closes matching local sockets (PublishKick / RunPublicListener).
//   - tf_bus — only the brain LISTENs; executors relay TFAC-592 run
//     sentinels here so the brain's local eventbus (and its EE
//     subscribers) sees them (PublishSentinel / RunBusListener).
//
// Local mode, and single-process TF_ROLE=all, never construct a
// Backplane — every consumer (websocket.Hub, delegate.Spawner,
// internal/server) treats a nil/unwired backplane as "behave exactly as
// before," so this package is purely additive.
package wsbackplane

import (
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Backplane is the multi-mode cross-pod fan-out for the websocket Hub,
// session kicks, and brain-bound run sentinels. See the package doc for
// the three channels it owns.
type Backplane struct {
	db        *sql.DB // admin (BYPASSRLS) pool — ws_outbox/ws_presence I/O + NOTIFY
	directDSN string  // session-mode DSN for the dedicated LISTEN connections
	originID  string  // this process's instance registry id (TFAC-577)
	hub       *websocket.Hub

	inlineMaxBytes int
}

// New wires a Backplane. db must be the admin/BYPASSRLS pool (ws_outbox
// and ws_presence carry no RLS policy — see the migration comments);
// directDSN is a session-mode DSN the LISTEN connections open OUTSIDE
// that pool (see DirectDSN); originID is this process's persistent
// instance id, stamped on every envelope this pod publishes so its own
// LISTEN loop can skip its own traffic (no echo loops, by construction).
//
// New never fails and never blocks — the actual LISTEN connections open
// lazily inside RunPublicListener/RunBusListener, which reconnect with
// backoff on their own; a Backplane that can never reach Postgres simply
// degrades every pod to local-only fan-out rather than failing boot (see
// those methods' doc comments).
func New(db *sql.DB, directDSN, originID string, hub *websocket.Hub) *Backplane {
	return &Backplane{
		db:             db,
		directDSN:      directDSN,
		originID:       originID,
		hub:            hub,
		inlineMaxBytes: InlineMaxBytesFromEnv(),
	}
}

var _ websocket.Backplane = (*Backplane)(nil)
