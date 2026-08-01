package app

import (
	"context"
	"encoding/json"

	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/pgnotify"
	"github.com/sky-ai-eng/triage-factory/internal/wsbackplane"
)

// startCtlListener starts THE tf_ctl LISTEN connection for this process:
// exactly one dedicated connection per multi-mode pod, every role,
// process-lifetime (spec §5's connection budget — tf_ctl is one shared,
// kind-discriminated channel, so it gets one shared listener, not one
// per consuming subsystem). dispatchCtl routes each payload by its JSON
// "kind" to the subsystem that owns it:
//
//	"new"/"ack"           → the spawner's run-signal doorbells (TFAC-585)
//	"trigger"/"pollsoon"  → the brain trigger/PollSoon relay (TFAC-583),
//	                        holder-gated inside handleCtlMessage
//	"kick"                → the WS backplane's cross-pod session kick
//	                        (TFAC-584)
//	"cred_request"        → the brain's sealed-credential-bundle
//	                        provisioner (TFAC-614), holder-gated exactly
//	                        like "trigger"/"pollsoon"
//
// Uses wsbackplane.DirectDSN (TF_DATABASE_DIRECT_URL falling back to
// TF_DATABASE_URL) — LISTEN needs a session-scoped connection that
// bypasses any future transaction-mode pooler, same as every other
// LISTEN in the fabric. A missing DSN degrades to backstop-poll-only
// latency (every tf_ctl doorbell has one), never a boot failure.
//
// Local mode never calls this: no conversation_signals writes exist there
// (SetRunSignals is never wired), no backplane is constructed, and
// role=all always self-holds the brain, so nothing ever needs to relay
// INTO the process.
func (a *App) startCtlListener(ctx context.Context) {
	dsn := wsbackplane.DirectDSN()
	if dsn == "" {
		appLog.Warn("multi mode with no TF_DATABASE_URL/TF_DATABASE_DIRECT_URL; tf_ctl listener disabled — cross-pod control degrades to backstop-poll latency")
		return
	}
	listener := pgnotify.NewListener(dsn, ctlbus.Channel, a.dispatchCtl)
	go listener.Run(ctx)
}

// dispatchCtl routes one tf_ctl notification payload by its "kind" field.
// Runs synchronously on the LISTEN connection's read loop, so every
// branch must stay non-blocking — and every handler here is: the
// spawner's doorbell dispatch is a non-blocking channel send, the relay
// managers' Trigger/PollSoon are merge-on-signal nudges, and the kick
// handler spawns per-connection close goroutines. Malformed or unknown
// payloads are logged and dropped: every tf_ctl consumer pairs the
// doorbell with its own backstop (apply-loop scan, ack poll, poll
// sentinel re-kick, session-revalidation timer), so a dropped
// notification only delays work, never loses it.
func (a *App) dispatchCtl(payload string) {
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		appLog.Warn("tf_ctl: malformed payload; dropping", "error", err)
		return
	}
	switch probe.Kind {
	case "new", "ack":
		if a.spawner != nil {
			a.spawner.HandleCtlNotification(payload)
		}
	case "trigger", "pollsoon", "cred_request", "curator_cred_request":
		var msg ctlbus.Message
		if err := json.Unmarshal([]byte(payload), &msg); err != nil {
			appLog.Warn("tf_ctl: malformed relay message; dropping", "error", err)
			return
		}
		a.handleCtlMessage(msg)
	case "kick":
		// nil on a pod whose backplane build found no DSN — no sockets to
		// kick there anyway (local never reaches this listener at all).
		if a.wsBackplane != nil {
			a.wsBackplane.HandleCtlKick(payload)
		}
	case "curator_new":
		// Curator homing: nudge THIS pod's claim loop to scan for freshly
		// enqueued turns. Broadcast + self-filter — the claim's own home gate
		// admits only turns homed here, so a pod that isn't the home simply
		// finds nothing. The backstop scan covers a dropped notification.
		if a.spawner != nil {
			a.spawner.WakeDispatcher()
		}
	case "curator_cancel":
		// Curator homing (spec §6.3): fire the in-process session cancel if THIS
		// pod holds the project's live session. Broadcast + self-filter —
		// CancelLocal is a no-op on every pod but the home. CancelLocal (not
		// Cancel) so a delivered cancel doesn't re-broadcast onto the bus.
		if a.curator != nil {
			var msg ctlbus.Message
			if err := json.Unmarshal([]byte(payload), &msg); err != nil {
				appLog.Warn("tf_ctl: malformed curator_cancel; dropping", "error", err)
				return
			}
			a.curator.CancelLocal(msg.ProjectID)
		}
	case "kb_changed":
		// Project knowledge base: a control-pod KB upload/delete
		// nudges the home executor to materialize the panel write into a live
		// session's dir; op="project_deleted" drops the executor's stale
		// materialized project dir. Broadcast + self-filter — the curator's own
		// live-session check makes this a no-op on every pod but the one running
		// the project's session (nil curator on a plain control pod → no-op).
		if a.curator != nil {
			var msg ctlbus.Message
			if err := json.Unmarshal([]byte(payload), &msg); err != nil {
				appLog.Warn("tf_ctl: malformed kb_changed; dropping", "error", err)
				return
			}
			// Materialize/drop do store + disk I/O, so run off the LISTEN read
			// loop (which every dispatchCtl branch must not block).
			switch msg.Op {
			case "project_deleted":
				go a.curator.DropMaterializedKBIfIdle(msg.OrgID, msg.ProjectID)
			default:
				if a.curator.HasLiveSession(msg.ProjectID) {
					go a.curator.MaterializeKB(context.Background(), msg.OrgID, msg.ProjectID)
				}
			}
		}
	default:
		appLog.Warn("tf_ctl: unknown message kind", "kind", probe.Kind)
	}
}
