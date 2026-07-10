package app

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/pgnotify"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// runStartupTasks performs the one-time boot side effects, in order:
// register the clone-result callback (before any clone can fire), sweep
// orphaned worktrees, and (local only) import skill files. All of this runs
// before the background workers and the first poll.
func (a *App) runStartupTasks(ctx context.Context) {
	// Materialize the TF-controlled git hooks dir (F2, TFAC-456) before any
	// run can spawn, in both modes, so core.hooksPath always resolves to a
	// real directory. Best-effort: a failure here just means a run's git
	// hooks no-op (core.hooksPath at a missing dir is harmless), so log and
	// continue rather than block boot.
	if err := githooks.Ensure(); err != nil {
		serverLog.Warn("ensure git hooks dir failed; agent git hooks will no-op this boot", "error", err)
	}
	if a.local() {
		a.wireCloneStatusCallback()
		// Headless env-driven provisioning (TFAC-411): when TF_HEADLESS is set,
		// provision the local tenant and seed repos / Jira / identity from env so
		// a keychain-less, browser-less install reaches setup_complete. Runs
		// before startPolling → bootstrapBareClones so the seeded repos clone on
		// the first cycle. Local-mode only; if the seed vars are set without the
		// trigger, warn rather than silently ignore them.
		if server.HeadlessEnabled() {
			if err := a.srv.RunHeadlessBootstrap(ctx); err != nil {
				bootstrapLog.Error("headless bootstrap failed", "error", err)
			}
		} else {
			server.WarnIfHeadlessSeedVarsOrphaned()
		}
	}
	a.cleanupWorktrees(ctx)
	if a.local() {
		a.importLocalSkills(ctx)
	}
}

// startWorkers starts the knowledge-base file watcher and the long-lived
// background workers. The workers take the app context so they shut down
// cleanly on SIGINT/SIGTERM — previously these used a never-cancelled
// background context ("the binary has no top-level cancel today").
//
// The classifier is no longer Start()-ed here: it is now a per-org Manager
// that lazy-starts a runner on first Trigger, matching the
// scorer/profiler which are likewise never explicitly started.
func (a *App) startWorkers(ctx context.Context) {
	// Brain workers (control/all): the knowledge-base watcher (curator KB)
	// and the event router's drain workers.
	if a.plan.brain {
		a.startKnowledgeWatcher()
		// Drain sweeper: safety net for queues stuck on transient fire errors.
		go a.router.RunDrainSweeper(ctx, 30*time.Second)
		// Durable event-queue drain worker: claims github:/jira: events the
		// ingestor enqueued, routes them, and marks them done.
		go a.router.RunEventQueue(ctx, a.eventWake, routing.DefaultEventScanInterval, routing.DefaultEventPruneInterval, routing.DefaultEventPruneAge)
	}

	// Dispatcher workers (executor/all): the run-queue dispatcher (claims +
	// executes queued runs, reconciling crash-stranded runs on boot) and the
	// workspace-snapshot retention reaper (bounds durable parked/aborted-run
	// snapshot blobs by TTL; a no-op when no blob store is wired). A control
	// pod builds the spawner but never claims delegated runs, so neither
	// runs there.
	if a.plan.dispatcher {
		go a.spawner.RunDispatcher(ctx, delegate.DefaultRunScanInterval)
		go a.spawner.RunSnapshotReaper(ctx, delegate.DefaultSnapshotReapInterval)
	}

	// Cross-pod run control (TFAC-585). Multi mode only: buildExecution
	// never calls SetRunSignals in local mode, so
	// RunSignalApplyLoop/-PurgeReaper are already no-ops there — this mode
	// check just avoids opening a needless direct Postgres LISTEN
	// connection. One shared tf_ctl Listener per process dispatches both
	// "new signal for me" (wakes the apply loop) and "ack for my wait"
	// (wakes an in-flight sendSignalAndAwaitAck) doorbells — see
	// Spawner.HandleCtlNotification.
	if runmode.Current() == runmode.ModeMulti {
		listener := pgnotify.NewListener(os.Getenv("TF_DATABASE_URL"), "tf_ctl", a.spawner.HandleCtlNotification)
		go listener.Run(ctx)
		// The apply loop only makes sense on dispatcher-capable roles —
		// only they can ever own a run's live process.
		if a.plan.dispatcher {
			go a.spawner.RunSignalApplyLoop(ctx, delegate.DefaultSignalApplyScanInterval)
		}
		// The purge reaper is system housekeeping, not tied to executor
		// identity — runs on brain roles like the other reapers/sweepers
		// (never on a plain executor, avoiding N-executor duplicate sweeps).
		if a.plan.brain {
			purgeAge, perr := delegate.ParseRunSignalPurgeAge(os.Getenv("TF_RUN_SIGNAL_PURGE_AFTER"))
			if perr != nil {
				appLog.Warn("run signal purge age", "error", perr)
			}
			go a.spawner.RunSignalPurgeReaper(ctx, delegate.DefaultRunSignalPurgeInterval, purgeAge)
		}
	}

	// Instance-registry heartbeat: every role renews its fleet registry row
	// on a timer (liveness + version/role visibility; capacity fields only
	// on executor-capable roles — see SetReportCapacity).
	go a.spawner.RunInstanceHeartbeat(ctx, delegate.DefaultInstanceHeartbeatInterval)

	// Bounded bare+worktree cache reaper (TFAC-60). Every role keeps a
	// per-pod worktree cache under its own TF_STATE_ROOT — run worktrees on
	// an executor, curator worktrees on control — so the reaper runs
	// everywhere. One mechanism, policy per mode: started in both modes so
	// the eviction path is exercised in local dev daily, but DefaultPolicy
	// hands local an unbounded budget so every sweep is a cheap no-op (no
	// silent eviction of the user's own repos). Multi gets a real per-pod
	// disk budget + cold-bare TTL, bounding at-rest storage across tenants.
	worktree.StartReaper(ctx, worktree.DefaultPolicy(), 0)
}

// startPolling kicks the first poll cycle. The logic lives on the reloader
// alongside the credential-change callbacks it shares the poller manager
// with. First-boot profiling and scoring are driven by the poll-complete
// subscribers, not wired here.
func (a *App) startPolling() {
	a.reloader.initialPoll()
}

// cleanupWorktrees removes orphaned worktrees from crashed runs. Parked
// `open` runs are preserved whole — their worktree dir
// and ~/.claude/projects session JSONL are the warm resume cache. A load
// failure just forgoes that optimization; those runs still resume by
// rehydrating from snapshot.
//
// Non-local modes get the worktree-dir + bare-repo sweep but skip
// ~/.claude/projects entirely: the preserve set is keyed by the synthetic
// sentinel org, which has no real-tenant rows in multi mode.
func (a *App) cleanupWorktrees(ctx context.Context) {
	if !a.local() {
		worktree.CleanupWithOptions(worktree.CleanupOptions{SkipClaudeProjectCleanup: true})
		return
	}

	preserveWorktrees := map[string]bool{}
	if parkedPaths, perr := a.stores.AgentRuns.ListParkedWorktreePathsSystem(ctx, runmode.LocalDefaultOrgID); perr != nil {
		serverLog.Warn("load parked worktree paths failed; parked workspaces will rehydrate from snapshot rather than reuse the warm cache", "error", perr)
	} else {
		for _, p := range parkedPaths {
			preserveWorktrees[filepath.Base(p)] = true
		}
	}
	worktree.CleanupWithOptions(worktree.CleanupOptions{
		PreserveWorktreeFor: preserveWorktrees,
	})
}

// importLocalSkills imports any new Claude Code skill files as prompts. Gated
// on tenant existence: on a tenant-less boot it no-ops; after a prior provision
// it imports against the provisioned org. Per-user identity — GitHub (PAT_2)
// and Jira (access + derived identity) alike — is deliberately NOT bootstrapped
// from the org PAT here: it's captured only through the setup wizard's User
// step / the Connect gate page, never derived from the org's access credential.
func (a *App) importLocalSkills(ctx context.Context) {
	org, err := a.stores.Orgs.GetOrgSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		bootstrapLog.Warn("check local tenant failed; skipping skill import", "error", err)
		return
	}
	if org == nil {
		return
	}
	// SKILL.md files live on the user's machine, not the server's, so this
	// is local-only by construction.
	skills.ImportAll(ctx, a.database, a.stores.Prompts)
}

// wireCloneStatusCallback registers the worktree clone-result callback,
// which stamps repo_profiles with the clone outcome and broadcasts a
// websocket event so the Repos page updates live. A clone failure gets an
// SSH preflight to classify whether SSH is the cause (driving the per-row
// CTA). Local-only: the body hardcodes the sentinel org for the row-stamp +
// broadcast.
func (a *App) wireCloneStatusCallback() {
	worktree.SetOnCloneResult(func(owner, repo string, cloneErr error) {
		if cloneErr == nil {
			if err := a.stores.Repos.UpdateCloneStatusSystem(context.Background(), runmode.LocalDefaultOrgID, owner, repo, "ok", "", ""); err != nil {
				cloneStatusLog.Error("update ok status failed", "owner", owner, "repo", repo, "error", err)
			}
			a.wsHub.Broadcast(websocket.Event{
				Type:  "repo_profile_updated",
				OrgID: runmode.LocalDefaultOrgID,
				Data: map[string]any{
					"id":           owner + "/" + repo,
					"clone_status": "ok",
				},
			})
			return
		}

		cloneStatusLog.Error("clone failed", "owner", owner, "repo", repo, "error", cloneErr)

		kind := "other"
		orgSet, oErr := a.stores.Orgs.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
		if oErr != nil {
			cloneStatusLog.Warn("load org settings to classify failed; defaulting to kind=other", "owner", owner, "repo", repo, "error", oErr)
		} else if orgSet.GitHubCloneProtocol == "ssh" {
			// Use the configured GitHub host so GHE installs probe the right
			// SSH endpoint, not github.com.
			creds, _ := integrations.Load(context.Background(), a.stores.Secrets, runmode.LocalDefaultOrgID)
			sshHost := worktree.SSHHostFromBaseURL(creds.GitHubURL)
			sshCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if perr := worktree.CachedPreflightSSH(sshCtx, sshHost); perr != nil {
				kind = "ssh"
				cloneStatusLog.Warn("ssh preflight also failed; kind=ssh", "owner", owner, "repo", repo, "ssh_host", sshHost, "error", perr)
			} else {
				cloneStatusLog.Info("ssh preflight passed; kind=other (clone error is on the git side)", "owner", owner, "repo", repo, "ssh_host", sshHost)
			}
			cancel()
		}

		if err := a.stores.Repos.UpdateCloneStatusSystem(context.Background(), runmode.LocalDefaultOrgID, owner, repo, "failed", cloneErr.Error(), kind); err != nil {
			cloneStatusLog.Error("update failed status failed", "owner", owner, "repo", repo, "error", err)
		}
		a.wsHub.Broadcast(websocket.Event{
			Type:  "repo_profile_updated",
			OrgID: runmode.LocalDefaultOrgID,
			Data: map[string]any{
				"id":               owner + "/" + repo,
				"clone_status":     "failed",
				"clone_error":      cloneErr.Error(),
				"clone_error_kind": kind,
			},
		})
	})
}

// startKnowledgeWatcher fires `project_knowledge_updated` over the websocket
// whenever a file under <projectsRoot>/<id>/knowledge-base/ changes, so the
// frontend Knowledge panel refetches as the agent writes files mid-turn.
// Failure is non-fatal — the panel still works, just without live updates.
func (a *App) startKnowledgeWatcher() {
	// resolveOrgForProject stamps each broadcast with the project's owning
	// org so the hub's per-connection filter keeps it scoped. Returning ""
	// drops the broadcast rather than fanning out cross-tenancy.
	resolveOrgForProject := func(projectID string) string {
		orgID, err := a.stores.Projects.ResolveOrgSystem(context.Background(), projectID)
		if err != nil {
			kbwatcherLog.Warn("resolve org for project failed; dropping live update", "project", projectID, "error", err)
			return ""
		}
		if orgID == "" {
			kbwatcherLog.Warn("no org for project; stale dir or unresolved row, dropping live update", "project", projectID)
			return ""
		}
		return orgID
	}
	if root, err := curator.ProjectsWatchRoot(); err != nil {
		kbwatcherLog.Warn("resolve projects root failed; live kb updates disabled", "error", err)
	} else if _, err := curator.NewKnowledgeWatcher(a.wsHub, root, resolveOrgForProject); err != nil {
		kbwatcherLog.Warn("start failed; live kb updates disabled", "error", err)
	}
}

// bootstrapBareClones reads the configured repos and asks the worktree
// package to ensure each is materialized on disk as a bare clone with the
// right origin URL. Called after profiling completes (profiling populates
// repo_profiles.clone_url; targets without a CloneURL are skipped). DB read
// errors are logged and skipped — the lazy clone inside CreateForPR /
// CreateForBranch recovers affected delegations on the next run.
func bootstrapBareClones(repos db.RepoStore, secrets db.SecretStore) {
	profiles, err := repos.ListSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		worktreeLog.Warn("bootstrap: load profiles failed", "error", err)
		return
	}
	// Load the org bot PAT once so HTTPS clones of private repos authenticate.
	// CloneAuthFor (applied per-target in BootstrapBareClones) no-ops on an SSH
	// CloneURL or an empty token, so the SSH default and public-repo paths are
	// unaffected — this only matters for the https-pinned headless install.
	// Best-effort: a load failure leaves the token empty and falls back to the
	// prior unauthenticated behavior rather than blocking the warm pass.
	creds, cerr := integrations.LoadSystem(context.Background(), secrets, runmode.LocalDefaultOrgID)
	if cerr != nil {
		worktreeLog.Warn("bootstrap: load credentials failed; HTTPS clones of private repos may fail to warm", "error", cerr)
	}
	targets := make([]worktree.BootstrapTarget, 0, len(profiles))
	for _, p := range profiles {
		targets = append(targets, worktree.BootstrapTarget{
			Owner:    p.Owner,
			Repo:     p.Repo,
			CloneURL: p.CloneURL,
			Token:    creds.GitHubPAT,
		})
	}
	worktree.BootstrapBareClones(context.Background(), targets)
}
