package app

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
)

// credRequestProvisionTimeout bounds a single cred_request handler's
// ProvisionForConversation call (TFAC-614) — well inside the executor's own
// 2-minute awaiting-credentials deadline, so a stalled DB/GitHub
// round-trip degrades to one deferred backstop-sweep retry instead of
// leaking a goroutine per dropped notification.
const credRequestProvisionTimeout = 30 * time.Second

// pollSoon relays poller.Manager.PollSoon(source, orgID): in-process when
// this pod runs the brain (role=all, or a control pod currently holding
// the lease); a tf_ctl NOTIFY otherwise, for the holder's LISTEN loop to
// apply. Callers: the reloader's onGitHubChanged/onJiraChanged — config-
// save handlers that may run on ANY control pod, not necessarily the
// current holder.
func (a *App) pollSoon(source, orgID string) {
	if a.isBrainHolder() {
		if a.pollerMgr != nil {
			a.pollerMgr.PollSoon(source, orgID)
		}
		return
	}
	a.publishCtl(ctlbus.Message{Kind: "pollsoon", Source: source, OrgID: orgID})
}

// triggerScorer/triggerProfiler/triggerReach relay a background Manager's
// Trigger(orgID) the same way pollSoon relays PollSoon. Wired as the
// server's SetScorerTrigger/SetProfilerTrigger/SetReachTrigger callbacks —
// every one of those callers may run on a standby control pod. The reach one
// is driven by a REQUEST — a picker read finding the mirror stale, the
// refresh control, a credential save — which is exactly the shape that
// lands wherever the load balancer put it.
func (a *App) triggerScorer(orgID string)               { a.triggerManager("scorer", orgID, false) }
func (a *App) triggerProfiler(orgID string, force bool) { a.triggerManager("profiler", orgID, force) }
func (a *App) triggerReach(orgID string, force bool)    { a.triggerManager("reach", orgID, force) }

// sourcesChanged relays an org admin's event-source pause/resume the way
// pollSoon relays PollSoon. Wired as the server's SetOnSourcesChanged callback,
// and its caller is a request handler that lands wherever the load balancer put
// it — so the pod that served the PATCH is very often not the one whose router
// holds the stale answer.
func (a *App) sourcesChanged(orgID, kind string) {
	if a.isBrainHolder() {
		a.applySourcesChanged(orgID, kind)
		return
	}
	a.publishCtl(ctlbus.Message{Kind: "sources_changed", OrgID: orgID, Source: kind})
}

// applySourcesChanged is the brain-side effect of a pause/resume: drop the
// router's cached policy for the org, and re-due the source's poll so a resumed
// source restarts on the next wake instead of after a full interval. PollSoon
// on a source nothing polls just clears a key nobody set.
func (a *App) applySourcesChanged(orgID, kind string) {
	if a.router != nil {
		a.router.InvalidateSourceGate(orgID)
	}
	if a.pollerMgr != nil {
		a.pollerMgr.PollSoon(kind, orgID)
	}
}

// triggerManager is the shared relay body: dispatch in-process when this
// pod holds the brain, else publish the tf_ctl relay message for the
// holder to pick up.
func (a *App) triggerManager(manager, orgID string, force bool) {
	if orgID == "" {
		return
	}
	if a.isBrainHolder() {
		a.dispatchManagerTrigger(manager, orgID, force)
		return
	}
	a.publishCtl(ctlbus.Message{Kind: "trigger", Manager: manager, OrgID: orgID, Force: force})
}

// dispatchManagerTrigger calls the named in-process Manager directly. Two
// callers: triggerManager's local (holder) fast path above, and
// handleCtlMessage when a relayed message arrives at the process that IS
// the brain. Every Manager here may be nil on a role that never builds
// brain objects (defensive; in practice this is only ever called from
// paths already gated to brain-capable roles).
func (a *App) dispatchManagerTrigger(manager, orgID string, force bool) {
	switch manager {
	case "scorer":
		if a.scorer != nil {
			a.scorer.Trigger(orgID)
		}
	case "profiler":
		if a.profiler != nil {
			a.profiler.Trigger(orgID, force)
		}
	case "reconciler":
		if a.reconciler != nil {
			a.reconciler.Trigger(orgID)
		}
	case "reach":
		if a.reachCache != nil {
			a.reachCache.Trigger(orgID, force)
		}
	default:
		appLog.Warn("tf_ctl: unknown manager in trigger relay", "manager", manager)
	}
}

// handleCtlMessage dispatches a trigger/pollsoon relay message received
// on tf_ctl (routed here by dispatchCtl, ctl.go). Since TFAC-585's tf_ctl
// consolidation the listener is process-lifetime and role-wide — every
// multi-mode pod hears every relay message — so "only the brain acts on
// relay traffic" is enforced by the holder gate below, not by
// subscription scope. publishCtl is only ever called by a non-holder and
// NOTIFY fans out to every listener, so exactly the current holder acts
// on each message; a message published in a holderless gap (mid-failover)
// is dropped by every pod — the relay is lossy by contract (spec §3), and
// the next system:poll:* sentinel re-kicks whatever it carried.
func (a *App) handleCtlMessage(msg ctlbus.Message) {
	if !a.isBrainHolder() {
		return
	}
	switch msg.Kind {
	case "trigger":
		a.dispatchManagerTrigger(msg.Manager, msg.OrgID, msg.Force)
	case "pollsoon":
		if a.pollerMgr != nil {
			a.pollerMgr.PollSoon(msg.Source, msg.OrgID)
		}
	case "sources_changed":
		a.applySourcesChanged(msg.OrgID, msg.Source)
	case "cred_request":
		// nil at TF_ROLE=executor (buildCredProvisioner never constructs
		// a.credProvisioner there) and in local mode — an executor is
		// never the brain holder, so it can't reach this branch anyway;
		// the nil check is defense in depth. Detached but BOUNDED context:
		// this dispatches off the shared LISTEN connection's read loop and
		// must not block it, but an unbounded context.Background() call
		// could hang indefinitely on a stalled DB/GitHub round-trip and
		// leak one goroutine per dropped cred_request under repeated
		// notifications. credRequestProvisionTimeout caps it well inside
		// the executor's own 2-minute awaiting-credentials deadline, so a
		// timeout here just costs one deferred pass — the backstop sweep
		// (internal/credprovision.RunAwaitingSweep) retries it. Unlike
		// a.dispatchManagerTrigger's fire-and-forget Trigger() channel
		// send, ProvisionForConversation does real work and its error is worth
		// logging.
		if a.credProvisioner != nil {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), credRequestProvisionTimeout)
				defer cancel()
				if err := a.credProvisioner.ProvisionForConversation(ctx, msg.OrgID, msg.ConversationID); err != nil {
					appLog.Warn("tf_ctl: cred_request provision failed", "conversation", msg.ConversationID, "error", err)
				}
			}()
		}
	default:
		appLog.Warn("tf_ctl: unknown relay message kind", "kind", msg.Kind)
	}
}

// publishCtl publishes a relay message on tf_ctl. Only ever called when
// !isBrainHolder(), which — given isBrainHolder's role switch — can only
// happen in multi mode (a standby control pod, or an executor), so
// a.database is always a live Postgres admin pool here. Relay is lossy by
// design (spec §3): a publish failure just costs one deferred pass, the
// same cost as a dropped NOTIFY, so this logs and moves on rather than
// retrying or surfacing an error to the caller.
func (a *App) publishCtl(msg ctlbus.Message) {
	if err := ctlbus.Publish(context.Background(), a.database, msg); err != nil {
		appLog.Warn("tf_ctl relay publish failed", "kind", msg.Kind, "error", err)
	}
}
