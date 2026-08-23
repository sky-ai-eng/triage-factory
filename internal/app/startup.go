package app

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/repoevent"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"github.com/sky-ai-eng/triage-factory/internal/wsbackplane"
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
		// before startBrain → bootstrapBareClones so the seeded repos clone on
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

// startWorkers starts the long-lived background workers. They take the app
// context so they shut down cleanly on SIGINT/SIGTERM — previously these
// used a never-cancelled background context ("the binary has no top-level
// cancel today").
func (a *App) startWorkers(ctx context.Context) {
	// Dispatcher workers (executor/all): the conversation-queue dispatcher
	// (claims + executes queued conversations, reconciling crash-stranded
	// conversations on boot), the workspace-snapshot retention reaper (bounds
	// durable parked/aborted-conversation snapshot blobs by TTL; a no-op when
	// no blob store is wired), and the workspace evictor (bounds the WARM
	// copy — the parked trees on this pod's own disk — which otherwise
	// accumulates until the process restarts). A control pod builds the
	// spawner but never claims delegated conversations, so none of them run
	// there.
	if a.plan.dispatcher {
		go a.spawner.RunDispatcher(ctx, delegate.DefaultDispatchScanInterval)
		go a.spawner.RunSnapshotReaper(ctx, delegate.DefaultSnapshotReapInterval)
		evictAfter, eerr := delegate.ParseWorkspaceEvictAfter(os.Getenv("TF_WORKSPACE_EVICT_AFTER_SEC"), a.local())
		if eerr != nil {
			appLog.Warn("workspace eviction window", "error", eerr)
		}
		go a.spawner.RunWorkspaceEvictor(ctx, delegate.DefaultWorkspaceEvictInterval, evictAfter)
	}

	// The shared tf_ctl control-plane listener + the cross-pod run-control
	// workers (TFAC-585). Multi mode only: local mode never wires
	// SetConversationSignals (so the apply loop / purge reaper below are no-ops
	// there anyway), never builds a backplane, and role=all always
	// self-holds the brain — nothing ever relays into a local process, so
	// there is no reason to open a Postgres LISTEN connection at all.
	// startCtlListener (ctl.go) is the ONE tf_ctl LISTEN per pod, every
	// role: it routes conversation-signal doorbells to the spawner, session
	// kicks to the WS backplane, and trigger/PollSoon relays to
	// handleCtlMessage (which holder-gates them).
	if runmode.Current() == runmode.ModeMulti {
		a.startCtlListener(ctx)
		// The signal apply loop only makes sense on dispatcher-capable
		// roles — only they can ever own a run's live process. The tf_wake
		// LISTEN (TFAC-586) is the same gate: only a dispatcher-capable
		// role has a claim loop worth nudging.
		if a.plan.dispatcher {
			go a.spawner.ConversationSignalApplyLoop(ctx, delegate.DefaultSignalApplyScanInterval)
			go a.startWakeListener(ctx)
		}
		// The purge reaper is system housekeeping, not tied to executor
		// identity — runs on brain roles like the other reapers/sweepers
		// (never on a plain executor, avoiding N-executor duplicate sweeps).
		if a.plan.brain {
			purgeAge, perr := delegate.ParseConversationSignalPurgeAge(os.Getenv("TF_CONVERSATION_SIGNAL_PURGE_AFTER"))
			if perr != nil {
				appLog.Warn("conversation signal purge age", "error", perr)
			}
			go a.spawner.ConversationSignalPurgeReaper(ctx, delegate.DefaultConversationSignalPurgeInterval, purgeAge)
		}
	}

	// Instance-registry heartbeat: every role renews its fleet registry row
	// on a timer (liveness + version/role visibility; capacity fields only
	// on executor-capable roles — see SetReportCapacity).
	go a.spawner.RunInstanceHeartbeat(ctx, delegate.DefaultInstanceHeartbeatInterval)

	// Fleet telemetry sampler (TFAC-589): every role writes one instance_stats
	// row a minute (cpu/load/mem/oom deployment-wide; claim-scoped fields only
	// on executor-capable roles) and reaps the ~30d tail. The dashboard reads
	// it.
	go a.spawner.RunInstanceStatSampler(ctx, delegate.DefaultInstanceStatSampleInterval, delegate.DefaultInstanceStatRetention)

	// WS backplane workers (TFAC-584) — nil in local mode / before
	// buildWSBackplane wires it, so every branch below is a no-op there.
	if a.wsBackplane != nil {
		// Public listener (tf_ws fan-out): every pod that holds user
		// websocket connections. An executor holds none (its Hub is a
		// standalone, client-less sink — buildExecutorRuntime), so it has
		// nothing to fan into and never LISTENs here; it still PUBLISHES
		// to tf_ws via the spawner's broadcasts on its client-less hub,
		// which needs no LISTEN connection at all (NOTIFY rides the pooled
		// admin connection). The tf_ctl session kick no longer rides this
		// connection — it arrives via the shared tf_ctl listener started
		// above, routed to the backplane's HandleCtlKick.
		if a.plan.serveHTTP {
			go a.wsBackplane.RunPublicListener(ctx)
			go a.wsBackplane.RunPresenceHeartbeat(ctx, wsbackplane.PresenceHeartbeatIntervalFromEnv())
			go a.srv.RunSessionRevalidation(ctx, wsbackplane.RevalidateIntervalFromEnv())
		}
		// The brain-bound sentinel relay LISTEN (tf_bus) is NOT started
		// here: it holds with the lease (startBrain/stopBrain, brain.go),
		// per spec §5.3's "only the brain LISTENs on tf_bus" — a standby
		// must not consume the fleet's sentinel stream just to fan it to a
		// bus with no subscribers.
		// ws_outbox TTL reaper: deliberately NOT gated on brain — spec
		// §11 decision 5 calls this too trivial to route through the
		// lease, and `DELETE WHERE created_at < cutoff` is safe under any
		// number of pods racing it. Every wsBackplane-wired pod runs it,
		// executors included (they publish to tf_ws too).
		go a.wsBackplane.RunOutboxReaper(ctx, wsbackplane.DefaultOutboxReapInterval, wsbackplane.OutboxTTLFromEnv())
		// ws_presence row TTL reaper: same not-leader-gated rationale as
		// the outbox reaper above, and needed for the same reason — every
		// reconnect mints a brand-new conn_id nothing will ever overwrite,
		// so without this the table grows unbounded under ordinary
		// reconnect churn (see the migration comment on ws_presence).
		go a.wsBackplane.RunPresenceReaper(ctx, wsbackplane.DefaultPresenceReapInterval, wsbackplane.PresenceRowTTLFromEnv())
	}

	// Bounded bare+worktree cache reaper (TFAC-60). Every role keeps a
	// per-pod worktree cache under its own TF_STATE_ROOT — run worktrees on
	// an executor — so the reaper runs everywhere. One mechanism, policy
	// per mode: started in both modes so
	// the eviction path is exercised in local dev daily, but DefaultPolicy
	// hands local an unbounded budget so every sweep is a cheap no-op (no
	// silent eviction of the user's own repos). Multi gets a real per-pod
	// disk budget + cold-bare TTL, bounding at-rest storage across tenants.
	worktree.StartReaper(ctx, worktree.DefaultPolicy(), 0)
}

// cleanupWorktrees removes orphaned worktrees from crashed conversations.
// Parked `open` conversations are preserved whole — their worktree dir
// and ~/.claude/projects session JSONL are the warm resume cache. A load
// failure just forgoes that optimization; those conversations still resume
// by rehydrating from snapshot.
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
	if parkedPaths, perr := a.stores.Conversations.ListParkedWorktreePathsSystem(ctx, runmode.LocalDefaultOrgID); perr != nil {
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
// which stamps repositories with the clone outcome and broadcasts a
// websocket event so the Repos page updates live. A clone failure gets an
// SSH preflight to classify whether SSH is the cause (driving the per-row
// CTA). Local-only: the body hardcodes the sentinel org for the row-stamp +
// broadcast.
func (a *App) wireCloneStatusCallback() {
	// Org-scoped notifier (no RecipientsFunc): this hook is local-only,
	// and at N=1 the org-wide broadcast IS the REST-parity scope.
	notify := repoevent.NewNotifier(a.wsHub, nil)
	// The event is keyed on the registry row id, and the clone hook is handed
	// a name — but the stamp itself resolves the row, so this publishes the row
	// that write returned rather than looking the same repository up a second
	// time. A nil row is the write's documented no-op: nothing answers to the
	// name, so there is nothing to update and nothing for a client to merge
	// into.
	//
	// All three clone columns go on the wire because the write sets all three,
	// and off the row rather than restated from the arguments — so a success
	// after a failure clears the stored error instead of leaving a client
	// rendering it under a green status.
	publish := func(row *domain.Repository) {
		if row == nil {
			return
		}
		notify.Publish(context.Background(), runmode.LocalDefaultOrgID, repoevent.Update{
			ID:             row.ID,
			Slug:           row.Slug(),
			CloneStatus:    repoevent.Ptr(row.CloneStatus),
			CloneError:     repoevent.Ptr(row.CloneError),
			CloneErrorKind: repoevent.Ptr(row.CloneErrorKind),
		})
	}
	worktree.SetOnCloneResult(func(owner, repo string, cloneErr error) {
		ref := domain.RepoRef{Owner: owner, Repo: repo}
		if cloneErr == nil {
			row, err := a.stores.Repos.UpdateCloneStatusByRefSystem(context.Background(), runmode.LocalDefaultOrgID, ref, "ok", "", "")
			if err != nil {
				cloneStatusLog.Error("update ok status failed", "owner", owner, "repo", repo, "error", err)
				return
			}
			publish(row)
			return
		}

		cloneStatusLog.Error("clone failed", "owner", owner, "repo", repo, "error", cloneErr)

		kind := "other"
		orgSet, oErr := a.stores.Orgs.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
		if oErr != nil {
			cloneStatusLog.Warn("load org settings to classify failed; defaulting to kind=other", "owner", owner, "repo", repo, "error", oErr)
		} else if domain.EffectiveCloneProtocol(orgSet.GitHubCloneProtocol, runmode.Current() == runmode.ModeMulti) == "ssh" {
			// Through the resolver, never the stored column: probing an SSH
			// endpoint to explain a clone that never spoke SSH would name a
			// cause the operator cannot act on.
			//
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

		row, err := a.stores.Repos.UpdateCloneStatusByRefSystem(context.Background(), runmode.LocalDefaultOrgID, ref, "failed", cloneErr.Error(), kind)
		if err != nil {
			cloneStatusLog.Error("update failed status failed", "owner", owner, "repo", repo, "error", err)
			return
		}
		publish(row)
	})
}

// bootstrapBareClones reads the configured repos and asks the worktree
// package to ensure each is materialized on disk as a bare clone with the
// right origin URL. Called after profiling completes (profiling populates
// repositories.clone_url; targets without a CloneURL are skipped). DB read
// errors are logged and skipped — the lazy clone inside CreateForPR /
// CreateForBranch recovers affected delegations on the next run.
func bootstrapBareClones(repos db.RepositoryStore, secrets db.SecretStore) {
	profiles, err := repos.ListSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		worktreeLog.Warn("bootstrap: load repositories failed", "error", err)
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
