package app

import (
	"context"
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/repoprofile"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// reloader owns the credential-change reactions and the initial poll kick.
// The server's settings handlers call onGitHubChanged / onJiraChanged when
// integration creds rotate; initialPoll runs once at boot. All three share
// the same local-mode profile→restart→score sequence (reprofileRestartAndScore),
// which is why they live together.
//
// The heavy profile→restart→score sequence is gated to local mode: it wires
// process-global state shaped for a single-tenant binary, and its
// integrations.Load reads would fail under Postgres RLS from a claims-free
// goroutine. In multi the cred-change callbacks do only a lightweight PollSoon
// to re-due the changed org — the poller does the equivalent profiling/scoring
// per cycle — and initialPoll just starts the process-global loop.
type reloader struct {
	stores     db.Stores
	database   *sql.DB
	pollerMgr  *poller.Manager
	scorer     *ai.Manager
	ghResolver ghclient.Resolver
	runSecrets agentproc.SecretsReader
	wsHub      *websocket.Hub
	announce   *announcer
}

func newReloader(a *App) *reloader {
	return &reloader{
		stores:     a.stores,
		database:   a.database,
		pollerMgr:  a.pollerMgr,
		scorer:     a.scorer,
		ghResolver: a.ghResolver,
		runSecrets: a.runSecrets,
		wsHub:      a.wsHub,
		announce:   a.announce,
	}
}

// onGitHubChanged reacts to a GitHub credential/repo change: invalidate
// profiles → stop pollers → re-profile → restart. Multi mode can't
// selectively restart a process-global loop without re-polling every tenant
// against shared API budgets, so it just re-dues the changed org.
//
// The announce-pending flags are armed only on the local path: the announcer
// is keyed by source (not org), so arming "github" before the multi early
// return would let the next GitHub completion for ANY org consume it and ship
// the "first poll complete" toast to the wrong tenant (poll-tracker routes by
// evt.OrgID). The one-shot toast stays a local-mode (N=1) affordance.
func (r *reloader) onGitHubChanged(orgID string) {
	if runmode.Current() != runmode.ModeLocal {
		serverLog.Info("github config changed; multi-mode re-dues that org only (no fleet restart)", "org", orgID)
		r.pollerMgr.PollSoon("github", orgID)
		return
	}

	serverLog.Info("github config changed, full restart")
	// Local mode restarts both pollers below (N=1, no fleet to stampede), so arm
	// the one-shot "config took effect" toast for each; the first post-restart
	// completion of each source clears its flag.
	r.announce.setPending("github")
	r.announce.setPending("jira")

	// Local mode: N=1, so there's no fleet to stampede. The spawner +
	// curator + profiler resolve per-(org, owner) through the run-credential
	// seam, so a config change is picked up on the next run without a hot-swap.
	r.pollerMgr.StopAll()

	ctx := context.Background()
	creds, _ := integrations.Load(ctx, r.stores.Secrets, orgID)
	app, appErr := r.stores.GitHubApps.GetForOrgSystem(ctx, orgID)
	if appErr != nil {
		// Best-effort, same discipline as github_ready in the status handler:
		// a read failure leaves the App signal off and the PAT branch still
		// stands. Worth a log — an App-only org silently failing here would
		// reintroduce the TFAC-331 "never profiles" symptom this fix closes.
		serverLog.Warn("read github app registration failed; treating as no app", "org", orgID, "error", appErr)
	}

	if githubConfigured(creds, app) {
		// invalidate=true (creds changed); pollSoon=true (apply now rather
		// than wait out the interval).
		r.reprofileRestartAndScore(orgID, true, true)
	} else {
		r.pollerMgr.RestartAll()
		r.pollerMgr.PollSoon("github", orgID)
		r.pollerMgr.PollSoon("jira", orgID)
	}
}

// onJiraChanged reacts to a Jira credential/config change. Local mode restarts
// the in-process Jira poller, arms the one-shot "config took effect" toast, and
// re-dues the changed org so the new config applies now rather than after the
// interval. Multi mode can't selectively restart a process-global loop without
// re-polling every tenant against shared API budgets, so — mirroring
// onGitHubChanged — it just re-dues the changed org; the poller (running in
// multi via the claims-free system-creds reads) picks the change up on that
// org's next cycle.
//
// The announce-pending flag is left unset in multi on purpose: the announcer is
// keyed by source only (not org), so arming it would let the next Jira poll
// completion for ANY org consume it and ship the toast to the wrong tenant
// (poll-tracker routes by evt.OrgID). The one-shot toast stays a local-mode
// (N=1) affordance.
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

// initialPoll starts polling at boot. Local mode additionally kicks the first
// profile+score when GitHub is configured; multi mode just starts the
// process-global loop, which fans out over every active tenant — GitHub and
// Jira alike, since Jira now reads service creds through the claims-free system
// door and so polls in multi too. Request handlers resolve GitHub clients
// per-request through the credential resolver, so there's no process-global
// client to wire here.
func (r *reloader) initialPoll(ctx context.Context) {
	if runmode.Current() != runmode.ModeLocal {
		// runGitHubCycle fans out over ListActiveSystem each wake; orgs and
		// repos added via the admin UI are picked up without a restart. The
		// poll-complete sentinels drive scorer.Trigger per org (the "scorer"
		// subscriber), so no explicit scoring kick is needed.
		r.pollerMgr.RestartAll()
		return
	}

	orgID := runmode.LocalDefaultOrgID
	creds, _ := integrations.Load(ctx, r.stores.Secrets, orgID)
	app, appErr := r.stores.GitHubApps.GetForOrgSystem(ctx, orgID)
	if appErr != nil {
		delegateLog.Warn("read github app registration failed; treating as no app", "org", orgID, "error", appErr)
	}
	repoCount, _ := r.stores.Repos.CountConfiguredSystem(ctx, orgID)

	if githubConfigured(creds, app) && repoCount > 0 {
		delegateLog.Info("spawner ready", "repos", repoCount)
		// invalidate=false (fresh boot, profiles may still be warm);
		// pollSoon=false (the restarted loop polls on its own schedule).
		r.reprofileRestartAndScore(orgID, false, false)
	} else {
		// Not fully configured — start pollers immediately (may be empty).
		r.pollerMgr.RestartAll()
	}
	// SKY-463: no process-global Jira write client to wire here — the server
	// resolves Jira clients on demand (per-user writes, ForSystem reads).
}

// reprofileRestartAndScore runs the local-mode profile→restart→score
// sequence in the background: re-profile repos (invalidating cached
// profiles when invalidate is set), restart all pollers, optionally re-due
// the changed org immediately, trigger scoring, and refresh bare clones.
// Shared by onGitHubChanged and initialPoll.
func (r *reloader) reprofileRestartAndScore(orgID string, invalidate, pollSoon bool) {
	go func() {
		profiler := repoprofile.NewProfiler(r.ghResolver, r.runSecrets, r.database, r.stores.Repos, r.stores.Orgs, r.wsHub)
		if err := profiler.Run(context.Background(), invalidate); err != nil {
			repoprofileLog.Error("profiling failed", "error", err)
		}
		r.pollerMgr.RestartAll()
		if pollSoon {
			// Apply the change now: the restarted loop would otherwise defer
			// the re-poll up to a full interval. N=1 in local, so re-duing
			// "everyone" is just the one org.
			r.pollerMgr.PollSoon("github", orgID)
			r.pollerMgr.PollSoon("jira", orgID)
		}
		r.scorer.Trigger(orgID)
		bootstrapBareClones(r.stores.Repos)
	}()
}

// githubConfigured reports whether the org has a live GitHub access path the
// profiler can fetch repo docs through: a PAT (with a base URL) OR an active
// App registration. It is the profiler-trigger gate shared by onGitHubChanged
// and initialPoll.
//
// The App branch matches the server's github_ready signal
// (internal/server/credentials.go) exactly: an App counts only when it is
// active and carries a client_id. A *staged* App (active=false, mid PAT→App
// switch per TFAC-328) is registered but not yet minting tokens, so it must
// NOT count — the live credential is still the PAT, already covered by the
// first clause. The PAT branch is deliberately stricter than github_ready
// (which accepts a bare PAT): the profiler needs a host to call, so it also
// requires a base URL. The App branch needs no base-URL check — App orgs
// resolve their host from org_settings inside the resolver and may carry no
// github_url secret at all.
//
// TFAC-331: the old condition gated profiling on the PAT alone, so App-only
// orgs never profiled — every tracked repo sat as an unprofiled skeleton
// ("profile cannot be generated").
func githubConfigured(creds auth.Credentials, app *domain.OrgGitHubApp) bool {
	if creds.GitHubPAT != "" && creds.GitHubURL != "" {
		return true
	}
	return app != nil && app.Active && app.ClientID != ""
}
