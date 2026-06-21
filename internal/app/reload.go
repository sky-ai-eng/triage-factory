package app

import (
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// reloader owns the credential-change reactions and the initial poll kick.
// The server's settings handlers call onGitHubChanged / onJiraChanged when
// integration creds rotate; initialPoll runs once at boot.
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
	announce  *announcer
}

func newReloader(a *App) *reloader {
	return &reloader{
		pollerMgr: a.pollerMgr,
		announce:  a.announce,
	}
}

// onGitHubChanged reacts to a GitHub credential/repo change. The server
// wrapper (SetOnGitHubChanged) has already evicted the reachable-repo
// enumeration cache for the org by the time this runs; all this callback
// does is re-due that org's github poll so the new credential / repo set is
// applied on the next wake (≤ base tick) rather than after a full interval.
// New or newly-reachable repos are profiled by the "profiler" subscriber on
// that poll's completion — no explicit profiler call belongs here.
//
// Same in both modes, with one local-only affordance: arm the one-shot
// "config took effect" toast. The announcer is keyed by source (not org), so
// arming it in multi would let the next github completion for ANY org consume
// it and ship the toast to the wrong tenant (poll-tracker routes by
// evt.OrgID). The one-shot toast stays an N=1 affordance.
func (r *reloader) onGitHubChanged(orgID string) {
	serverLog.Info("github config changed; re-duing github poll for org", "org", orgID)
	if runmode.Current() == runmode.ModeLocal {
		r.announce.setPending("github")
	}
	r.pollerMgr.PollSoon("github", orgID)
}

// onJiraChanged reacts to a Jira credential/config change. Local mode
// restarts the in-process Jira poller, arms the one-shot "config took effect"
// toast, and re-dues the changed org so the new config applies now rather
// than after the interval. Multi mode can't selectively restart a
// process-global loop without re-polling every tenant against shared API
// budgets, so it just re-dues the changed org; the poller (running in multi
// via the claims-free system-creds reads) picks the change up on that org's
// next cycle.
//
// The announce-pending flag is left unset in multi on purpose: the announcer
// is keyed by source only (not org), so arming it would let the next Jira
// poll completion for ANY org consume it and ship the toast to the wrong
// tenant. The one-shot toast stays a local-mode (N=1) affordance.
func (r *reloader) onJiraChanged(orgID string) {
	if runmode.Current() != runmode.ModeLocal {
		serverLog.Info("jira config changed; multi-mode re-dues that org only (no fleet restart)", "org", orgID)
		r.pollerMgr.PollSoon("jira", orgID)
		return
	}

	serverLog.Info("jira config changed, restarting jira poller")
	r.announce.setPending("jira")
	// SKY-463: the server no longer holds a process-global Jira write client —
	// user writes resolve per-user via jira.Resolver, system reads via the
	// poller's ForSystem. Restarting the poller is all this callback needs to do.
	r.pollerMgr.RestartJira()
	r.pollerMgr.PollSoon("jira", orgID) // apply now, don't wait out the interval
}

// initialPoll starts polling at boot — RestartAll in both modes. The poll
// loops fan out over ListActiveSystem each wake, so orgs and repos added via
// the UI / admin API are picked up without a restart, and the poll-complete
// sentinels drive the scorer + profiler + classifier subscribers per org.
// First-boot profiling therefore needs no explicit kick here: the first
// github poll cycle's completion triggers it. Request handlers resolve
// GitHub clients per-request through the credential resolver, so there is no
// process-global client to wire at boot.
func (r *reloader) initialPoll() {
	r.pollerMgr.RestartAll()
}
