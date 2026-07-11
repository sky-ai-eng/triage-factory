// Package ctlbus is the cross-process trigger/PollSoon relay (TFAC-583,
// spec §5.3): non-leader callers of a background-brain manager's
// Trigger(orgID) or the poller's PollSoon(source, orgID) publish a
// message on Postgres' tf_ctl NOTIFY channel; the pod currently running
// the brain dispatches it to its in-process manager.
//
// This package owns only the PUBLISH half (plus the channel name and the
// Message wire shape). The LISTEN half is internal/app's unified tf_ctl
// dispatcher (internal/app/ctl.go) — one dedicated connection per pod
// serving every tf_ctl consumer (relay messages, run-signal doorbells,
// session kicks), with the "only the brain acts on relay traffic" rule
// enforced by a holder gate at dispatch (internal/app/relay.go's
// handleCtlMessage) rather than by lease-scoped subscription.
//
// The relay is deliberately lossy — the spec is explicit: "a dropped
// tf_ctl trigger costs one deferred scoring pass (the next system:poll:*
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
)

// Channel is the Postgres NOTIFY/LISTEN channel name every relay message
// rides. The same channel carries TFAC-584's session kicks, TFAC-585's
// run-signal doorbells, and TFAC-614's cred_request doorbell — payloads are
// discriminated by their JSON "kind" field (see internal/app/ctl.go), so
// Message kinds must stay disjoint from theirs ("kick", "new", "ack",
// "cred_request").
const Channel = "tf_ctl"

// Message is the relay payload. Kind selects which field group applies:
// "trigger" uses Manager/OrgID/Force (a scorer/classifier/profiler/
// reconciler Manager.Trigger call); "pollsoon" uses Source/OrgID (a
// poller.Manager.PollSoon call); "cred_request" uses OrgID/RunID (TFAC-614:
// an executor parked in status='awaiting_credentials' nudging the brain's
// credential provisioner — see internal/credprovision and
// internal/app/ctl.go's dispatch case).
type Message struct {
	Kind    string `json:"kind"`
	Manager string `json:"manager,omitempty"`
	OrgID   string `json:"org_id"`
	Source  string `json:"source,omitempty"`
	Force   bool   `json:"force,omitempty"`
	RunID   string `json:"run_id,omitempty"`
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
