package app

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// reloader owns the credential-change reactions and the initial poll kick.
// The server's settings handlers call onGitHubChanged / onJiraChanged when
// integration creds rotate; initialPoll runs once at boot (or on every
// background-brain acquisition — see brain.go's startBrain).
//
// These callbacks are deliberately thin. Repo profiling is NOT wired here:
// it is an independent system:poll: subscriber (the "profiler" Manager,
// registered in registerSubscribers), so a credential or repo-set change
// only needs to re-due the affected org's poll — the next github poll
// cycle's completion drives the TTL-gated profiling pass that picks up new /
// stale / newly-reachable repos. Scoring is likewise its own poll
// subscriber, so it isn't kicked from here either. This is what lets the
// callbacks read the same in both run modes.
type reloader struct {
	pollerMgr *poller.Manager
	// pollSoon is a.pollSoon — the brain-lease-aware relay wrapper
	// (relay.go). A config-save handler may run on a standby control pod,
	// where calling pollerMgr.PollSoon directly would silently update a
	// scheduler map nobody reads (the pod actually running the poller is
	// a different process) — pollSoon routes to the tf_ctl relay instead
	// when this pod isn't the holder.
	pollSoon func(source, orgID string)
	// pollReadiness backs the org-scoped "config took effect" one-shot
	// toast flag (TFAC-583) — see setAnnouncePending.
	pollReadiness db.PollReadinessStore
}

func newReloader(a *App) *reloader {
	return &reloader{
		pollerMgr:     a.pollerMgr,
		pollSoon:      a.pollSoon,
		pollReadiness: a.stores.PollReadiness,
	}
}

// setAnnouncePending arms the one-shot "config took effect" toast for
// (orgID, source). Org-scoped (TFAC-583): unlike the old process-local,
// source-only-keyed announcer, this is safe to arm in every mode — a
// completion for a DIFFERENT org can no longer consume another tenant's
// pending flag. Best-effort: a write failure just means the user misses
// one confirmation toast, not a functional regression.
func (r *reloader) setAnnouncePending(orgID, source string) {
	if err := r.pollReadiness.SetAnnouncePending(context.Background(), orgID, source); err != nil {
		serverLog.Warn("arm announce-pending flag failed", "org", orgID, "source", source, "error", err)
	}
}

// onGitHubChanged reacts to a GitHub credential/repo change. The server
// wrapper (SetOnGitHubChanged) has already evicted the reachable-repo
// enumeration cache for the org by the time this runs; all this callback
// does is re-due that org's github poll so the new credential / repo set is
// applied on the next wake (≤ base tick) rather than after a full interval.
// New or newly-reachable repos are profiled by the "profiler" subscriber on
// that poll's completion — no explicit profiler call belongs here.
func (r *reloader) onGitHubChanged(orgID string) {
	serverLog.Info("github config changed; re-duing github poll for org", "org", orgID)
	r.setAnnouncePending(orgID, "github")
	r.pollSoon("github", orgID)
}

// onJiraChanged reacts to a Jira credential/config change. Local mode
// restarts the in-process Jira poller and re-dues the changed org so the new
// config applies now rather than after the interval. Multi mode can't
// selectively restart a process-global loop without re-polling every tenant
// against shared API budgets, so it just re-dues the changed org; the poller
// (running in multi via the claims-free system-creds reads) picks the
// change up on that org's next cycle. Both branches arm the one-shot
// "config took effect" toast — org-scoped now, so a fast-moving fleet of
// tenants can't cross-wire whose toast fires (TFAC-583; previously this was
// a local-only affordance for exactly that reason).
func (r *reloader) onJiraChanged(orgID string) {
	r.setAnnouncePending(orgID, "jira")

	if runmode.Current() != runmode.ModeLocal {
		serverLog.Info("jira config changed; multi-mode re-dues that org only (no fleet restart)", "org", orgID)
		r.pollSoon("jira", orgID)
		return
	}

	serverLog.Info("jira config changed, restarting jira poller")
	// The server no longer holds a process-global Jira write client —
	// user writes resolve per-user via jira.Resolver, system reads via the
	// poller's ForSystem. Restarting the poller is all this callback needs
	// to do. Local-only (role=all always) — no relay needed for a direct
	// pollerMgr call here.
	r.pollerMgr.RestartJira()
	r.pollSoon("jira", orgID) // apply now, don't wait out the interval
}

// initialPoll starts polling — RestartAll in both modes. The poll loops fan
// out over ListActiveSystem each wake, so orgs and repos added via the UI /
// admin API are picked up without a restart, and the poll-complete
// sentinels drive the scorer + profiler subscribers per org.
// First-boot profiling therefore needs no explicit kick here: the first
// github poll cycle's completion triggers it. Request handlers resolve
// GitHub clients per-request through the credential resolver, so there is no
// process-global client to wire at boot.
//
// Called from brain.go's startBrain on every background-brain acquisition,
// not just once at process boot — a fresh acquisition (first boot, or a
// takeover after a prior holder's demotion) always starts the poll schedule
// cold, which is bounded/benign (spec §3: mostly free 304s, budgeted/
// resumable GitHub cycles).
func (r *reloader) initialPoll() {
	r.pollerMgr.RestartAll()
}
