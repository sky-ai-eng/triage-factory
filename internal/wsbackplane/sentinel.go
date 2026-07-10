package wsbackplane

import (
	"context"
	"encoding/json"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PublishSentinel relays a TFAC-592 run sentinel (system:run:status /
// system:run:activity) onto tf_bus for the brain to pick up. Executor
// processes wire this as an eventbus subscriber for the "system:run:"
// prefix (internal/app's registerSentinelRelay) — a brain process never
// needs it, since its sentinels are already on its own local bus (see
// that wiring's doc comment).
//
// Always inline — run sentinels are small metadata, never worth a
// ws_outbox row — but a sentinel that somehow exceeds Postgres's NOTIFY
// ceiling is dropped with a warning rather than erroring the publish,
// matching tf_bus's lossy-by-contract rule (an observability seam,
// never load-bearing state — see the package doc and spec §5.3).
func (b *Backplane) PublishSentinel(evt domain.Event) {
	env := busEnvelope{OriginInstanceID: b.originID, Event: evt}
	payload, err := json.Marshal(env)
	if err != nil {
		backplaneLog.Warn("tf_bus: marshal sentinel failed; dropping", "type", evt.EventType, "error", err)
		return
	}
	if len(payload) > notifyHardCapBytes {
		backplaneLog.Warn("tf_bus: sentinel exceeds NOTIFY payload ceiling; dropping (lossy by contract)",
			"type", evt.EventType, "size", len(payload), "ceiling", notifyHardCapBytes)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	if !b.notify(ctx, b.db, ChannelBus, env) {
		backplaneLog.Warn("tf_bus: notify failed; sentinel dropped (lossy by contract)", "type", evt.EventType)
	}
}

// handleBusNotification applies a received tf_bus envelope: skips
// self-origin (at TF_ROLE=all the sentinel is already on the only bus
// there is — see the package doc), otherwise hands the decoded event to
// localPublish, which the brain-side caller wires to re-publish onto its
// own in-process eventbus (internal/app passes a.bus.Publish as
// RunBusListener's argument). Malformed envelopes are dropped with a
// warning — a sentinel is observability-only, never worth failing
// anything over.
func (b *Backplane) handleBusNotification(payload string, localPublish func(domain.Event)) {
	var env busEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		backplaneLog.Warn("tf_bus: malformed envelope; dropping", "error", err)
		return
	}
	if env.OriginInstanceID == b.originID {
		return
	}
	localPublish(env.Event)
}
