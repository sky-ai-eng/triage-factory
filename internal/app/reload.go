package app

import (
	"context"
	"database/sql"
	"log"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/repoprofile"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// reloader owns the credential-change reactions and the initial poll kick.
// The server's settings handlers call onGitHubChanged / onJiraChanged when
// integration creds rotate; initialPoll runs once at boot. All three share
// the same local-mode profile→restart→score sequence (reprofileRestartAndScore),
// which is why they live together.
//
// Everything here is gated to local mode: the callbacks wire process-global
// state shaped for a single-tenant binary, and the integrations.Load reads
// would fail under Postgres RLS from a claims-free goroutine. Multi mode
// does the equivalent work per tenant inside the request handlers and per
// cycle inside the poller; these methods early-return for it.
type reloader struct {
	stores     db.Stores
	database   *sql.DB
	pollerMgr  *poller.Manager
	srv        *server.Server
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
		srv:        a.srv,
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
func (r *reloader) onGitHubChanged(orgID string) {
	log.Println("[server] GitHub config changed, full restart...")
	r.announce.setPending("github")

	if runmode.Current() != runmode.ModeLocal {
		log.Printf("[server] GitHub changed for org %s: multi-mode re-dues that org only (no fleet restart)", orgID)
		r.pollerMgr.PollSoon("github", orgID)
		return
	}

	// Local mode also restarts the Jira poller below, so announce its next
	// completion too. Multi mode took the early return above without touching
	// Jira, so it must not arm a spurious "first Jira poll" toast.
	r.announce.setPending("jira")

	// Local mode: N=1, so there's no fleet to stampede. The spawner +
	// curator + profiler resolve per-(org, owner) through the run-credential
	// seam, so a config change is picked up on the next run without a hot-swap.
	r.pollerMgr.StopAll()

	ctx := context.Background()
	creds, _ := integrations.Load(ctx, r.stores.Secrets, orgID)

	if creds.GitHubPAT != "" && creds.GitHubURL != "" {
		// Server request-handler path only (reviews / dashboard /
		// pending-PRs). Separate from the run-credential seam by design —
		// those handlers run with JWT claims.
		r.srv.SetGitHubClient(ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT))
		// invalidate=true (creds changed); pollSoon=true (apply now rather
		// than wait out the interval).
		r.reprofileRestartAndScore(orgID, true, true)
	} else {
		r.srv.SetGitHubClient(nil)
		r.pollerMgr.RestartAll()
		r.pollerMgr.PollSoon("github", orgID)
		r.pollerMgr.PollSoon("jira", orgID)
	}
}

// onJiraChanged restarts only the Jira poller and refreshes the server's
// Jira client. Local-only: multi-mode Jira polling needs per-org system
// creds and the process-global Jira client is itself a local-mode construct.
func (r *reloader) onJiraChanged(orgID string) {
	log.Println("[server] Jira config changed, restarting Jira poller...")
	r.announce.setPending("jira")

	if runmode.Current() != runmode.ModeLocal {
		log.Println("[server] Jira changed: multi-mode skips process-global refresh")
		return
	}

	// SKY-463: the server no longer holds a process-global Jira write client —
	// user writes resolve per-user via jira.Resolver, system reads via the
	// poller's ForSystem. Restarting the poller is all this callback needs to do.
	r.pollerMgr.RestartJira()
	r.pollerMgr.PollSoon("jira", orgID) // apply now, don't wait out the interval
}

// initialPoll starts polling at boot. Local mode additionally wires the
// process-global GitHub identity (server request-handler client) and kicks
// the first profile+score; multi mode just starts the process-global loop,
// which fans out over every active tenant and self-gates Jira off.
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
	repoCount, _ := r.stores.Repos.CountConfiguredSystem(ctx, orgID)

	if creds.GitHubPAT != "" && creds.GitHubURL != "" && repoCount > 0 {
		r.srv.SetGitHubClient(ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT))
		log.Printf("[delegate] spawner ready (%d repos configured)", repoCount)
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
			log.Printf("[repoprofile] profiling failed: %v", err)
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
