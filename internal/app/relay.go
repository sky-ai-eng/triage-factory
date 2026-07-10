package app

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
)

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

// triggerScorer/triggerProfiler/triggerClassifier relay a background
// Manager's Trigger(orgID) the same way pollSoon relays PollSoon. Wired
// as the server's SetScorerTrigger/SetProfilerTrigger callbacks and the
// spawner's classifier-wait trigger (internal/app/subsystems.go) — every
// one of those callers may run on a standby control pod, and the
// classifier one may also run on an executor (which has no local
// classifier object at all).
func (a *App) triggerScorer(orgID string)               { a.triggerManager("scorer", orgID, false) }
func (a *App) triggerProfiler(orgID string, force bool) { a.triggerManager("profiler", orgID, force) }
func (a *App) triggerClassifier(orgID string)           { a.triggerManager("classifier", orgID, false) }

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
	case "classifier":
		if a.classifier != nil {
			a.classifier.Trigger(orgID)
		}
	case "profiler":
		if a.profiler != nil {
			a.profiler.Trigger(orgID, force)
		}
	case "reconciler":
		if a.reconciler != nil {
			a.reconciler.Trigger(orgID)
		}
	default:
		appLog.Warn("tf_ctl: unknown manager in trigger relay", "manager", manager)
	}
}

// handleCtlMessage is the tf_ctl LISTEN callback (brain.go's
// startCtlListener). This process IS the brain whenever it fires — the
// listener only runs while holding the lease — so every message
// dispatches straight to the in-process manager/poller with no further
// holder check.
func (a *App) handleCtlMessage(msg ctlbus.Message) {
	switch msg.Kind {
	case "trigger":
		a.dispatchManagerTrigger(msg.Manager, msg.OrgID, msg.Force)
	case "pollsoon":
		if a.pollerMgr != nil {
			a.pollerMgr.PollSoon(msg.Source, msg.OrgID)
		}
	case "new", "ack":
		// TFAC-585's run_signals doorbell kinds — tf_ctl is a shared channel
		// (spec §5's table), and a lease-holding control pod that also
		// serves HTTP LISTENs here AND on delegate.Spawner's own dedicated
		// tf_ctl connection (HandleCtlNotification), so this pod legitimately
		// receives every signal doorbell too. Not an error, just traffic
		// this listener has nothing to do with — silently ignore rather than
		// WARN on every cross-pod interrupt/steer/cancel/permission/inject.
	default:
		appLog.Warn("tf_ctl: unknown message kind", "kind", msg.Kind)
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
