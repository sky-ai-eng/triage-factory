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
//	"new"/"ack"           → the spawner's conversation-signal doorbells (TFAC-585)
//	"trigger"/"pollsoon"  → the brain trigger/PollSoon relay (TFAC-583),
//	                        holder-gated inside handleCtlMessage
//	"kick"                → the WS backplane's cross-pod session kick
//	                        (TFAC-584)
//	"cred_request"        → the brain's sealed-credential-bundle
//	                        provisioner (TFAC-614), holder-gated exactly
//	                        like "trigger"/"pollsoon"
//	"sources_changed"     → the brain's event-source policy cache + poll
//	                        re-due after an admin paused or resumed a
//	                        source, holder-gated the same way
//
// Uses wsbackplane.DirectDSN (TF_DATABASE_DIRECT_URL falling back to
// TF_DATABASE_URL) — LISTEN needs a session-scoped connection that
// bypasses any future transaction-mode pooler, same as every other
// LISTEN in the fabric. A missing DSN degrades to backstop-poll-only
// latency (every tf_ctl doorbell has one), never a boot failure.
//
// Local mode never calls this: no conversation_signals writes exist there
// (SetConversationSignals is never wired), no backplane is constructed, and
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
	case "trigger", "pollsoon", "cred_request", "sources_changed":
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
	default:
		appLog.Warn("tf_ctl: unknown message kind", "kind", probe.Kind)
	}
}
