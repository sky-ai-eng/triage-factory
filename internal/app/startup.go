package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/ratelimits"
	"github.com/sky-ai-eng/triage-factory/internal/repoevent"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"github.com/sky-ai-eng/triage-factory/internal/wsbackplane"
)

// runStartupTasks performs the one-time boot side effects, in order:
// register the clone-result callback (before any clone can fire), sweep
// orphaned worktrees, move the workspace snapshot blobs onto the task key,
// and (local only) import skill files. All of this runs before the background
// workers and the first poll.
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
	}
	a.cleanupWorktrees(ctx)
	if a.local() {
		// Every engagement that wrote an allowed-signers file ended with the
		// process before this one, and a run that continues writes its own
		// under its new claim, so none of them is read again.
		if err := os.RemoveAll(paths.AllowedSignersDir()); err != nil {
			serverLog.Warn("removing the allowed-signers files of earlier runs failed", "error", err)
		}
		// The workspace key widened from the blueprint run to the task, and a
		// migration re-keyed the lifecycle rows. The blobs those rows describe
		// live in the object store, where no migration reaches, so they are
		// moved here — once, idempotently, and only in local mode, since multi
		// is unreleased and has no deployed blobs. Runs after the worktree
		// sweep for no ordering reason beyond reading in the order boot does.
		if a.spawner != nil {
			a.spawner.RekeyWorkspaceBlobsToTask(ctx, runmode.LocalDefaultOrgID)
		}
		// A local install may hold its Jira site in the keychain beside the
		// credential, where nothing reads it. It moves into org_settings here,
		// before the first poll resolves a Jira client. A failure leaves the key
		// for the next boot and the org on whatever site its settings name.
		if err := integrations.AdoptSavedJiraSite(ctx, a.stores.Secrets, a.stores.Orgs, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID); err != nil {
			serverLog.Warn("could not move the saved Jira site into org settings", "error", err)
		}
		// The same for GitHub: an org on GitHub with no GitHub URL set gets
		// the one it was using, before anything resolves a GitHub client.
		if err := integrations.AdoptSavedGitHubHost(ctx, a.stores, runmode.LocalDefaultOrgID); err != nil {
			serverLog.Warn("could not set the GitHub URL the org was using", "error", err)
		}
		a.importLocalSkills(ctx)
	}
}

// startWorkers starts the long-lived background workers. They take the app
// context so they shut down cleanly on SIGINT/SIGTERM.
//
// Which of them shutdown JOINS, and which it does not, is a deliberate split —
// the `go` statements below look alike and are not:
//
//   - Dispatches are joined (drainDispatches, shutdown.go). A dispatch
//     goroutine's terminal write runs on a context.WithoutCancel precisely so a
//     shutdown cannot strand a blueprint mid-finalize, which makes it the one
//     worker whose work is not finished when its context is cancelled. Closing
//     the pools under it turns that deliberate detachment into a failed write.
//     RunDispatcher itself is not joined — it is a select loop that returns at
//     its next tick and writes nothing on the way out.
//   - Everything else here is deliberately NOT joined: the reapers, evictors,
//     heartbeat, stat sampler, signal loops, backplane listeners and suspend
//     watcher are all periodic and idempotent, every write they make rides
//     the cancellable app context, and a tick lost to shutdown is simply
//     taken by the next boot. Waiting on them would add shutdown latency to
//     buy nothing.
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
		go a.spawner.RunDiskPressureEvictor(ctx, delegate.DefaultDiskPressureInterval)
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
		// Every control pod reads the deployment signer's health it signs
		// with, reports it, and makes its periodic check when one is due.
		// Started first and waited on, so a pod on a key the deployment has
		// not checked yet makes that check before the listener below hands it
		// a signature request or a bundle to seal.
		if a.deploymentSignerHealth != nil {
			go a.deploymentSignerHealth.Run(ctx, commitsigner.DeploymentWatchInterval)
			if !a.deploymentSignerHealth.WaitFirstTick(ctx, commitsigner.FirstTickWait) {
				appLog.Warn("deployment commit signer's first check is still running; starting without it",
					"waited", commitsigner.FirstTickWait)
			}
		}
		a.startCtlListener(ctx)
		// Every control pod signs, holder or standby: the signer answers
		// sign_request doorbells from the listener above and sweeps for the
		// requests whose doorbell was dropped. nil on an executor.
		if a.commitSigner != nil {
			go a.commitSigner.Run(ctx)
		}
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

	// Rate-limit readings: every process that serves the API or runs the brain
	// makes requests on orgs' behalf (handlers, pollers, the scorer) and writes
	// the readings those responses carried, so any control pod's route answers
	// for all of them. A plain executor records none: its secret store is
	// disabled, so it resolves no org credential to make such a request with,
	// and a run's provider traffic goes through its sidecar's proxies.
	//
	// Like the heartbeat below, it outlives ctx by the shutdown drain: the
	// requests the drain finishes record readings too. Run stops it once the
	// drain returns and waits for its last write, which lands before main's
	// deferred Close takes the pools away.
	a.stopFlusher = func() {}
	if a.plan.serveHTTP || a.plan.brain {
		flushCtx, cancelFlush := context.WithCancel(context.WithoutCancel(ctx))
		flushDone := make(chan struct{})
		go func() {
			defer close(flushDone)
			ratelimits.RunFlusher(flushCtx, a.stores.RateLimitReadings, ratelimits.FlushInterval)
		}()
		a.stopFlusher = func() {
			cancelFlush()
			<-flushDone
		}
	}

	// Instance-registry heartbeat: every role renews its fleet registry row
	// on a timer (liveness + version/role visibility; capacity fields only
	// on executor-capable roles — see SetReportCapacity).
	//
	// It outlives ctx by the length of the shutdown drain, and Run stops it
	// once the drain returns: a process still handing its engagements back is
	// alive, and saying so is what lets a successor keep waiting on the
	// workspace snapshot it is uploading (snapshotWriterAlive). The same
	// heartbeats report the pod gated once its claim loop has stopped, so
	// placement stops reserving queued work for it.
	hbCtx, cancelHeartbeat := context.WithCancel(context.WithoutCancel(ctx))
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		a.spawner.RunInstanceHeartbeat(hbCtx, delegate.DefaultInstanceHeartbeatInterval)
	}()
	a.stopHeartbeat = func() {
		cancelHeartbeat()
		<-heartbeatDone
	}

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
	// per mode: DefaultPolicy hands local no budget and no TTL (no silent
	// eviction of the user's own repos), and local adds its tracked set so
	// a clone no tracked repository owns is reclaimed. Multi gets a real
	// per-pod disk budget + cold-bare TTL, bounding at-rest storage across
	// tenants; those reclaim its orphaned clones too, so it reads no
	// tracked set.
	reaperPolicy := worktree.DefaultPolicy()
	if a.local() {
		reaperPolicy.Tracked = trackedRepositoryIDs(a.stores.Repos, a.stores.Orgs)
	}
	worktree.StartReaper(ctx, reaperPolicy, 0)

	// Wake from a system suspend: drop the idle HTTP connections that may
	// not have survived it. Every role, since every role calls out over
	// HTTP. Rescheduling the polls on wake is the brain's (startBrain).
	go watchSuspendForConnections(ctx)
}

// cleanupWorktrees removes orphaned worktrees from crashed conversations.
// Conversations that will be continued — parked `open`, or mid-flight when
// the process stopped — are preserved whole: their worktree dir and
// ~/.claude/projects session JSONL are the warm resume cache. A load failure
// just forgoes that optimization; those conversations still resume by
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
	if resumable, perr := a.stores.Conversations.ListResumableWorktreePathsSystem(ctx, runmode.LocalDefaultOrgID); perr != nil {
		serverLog.Warn("load resumable worktree paths failed; those workspaces will rehydrate from snapshot rather than reuse the warm cache", "error", perr)
	} else {
		for _, p := range resumable {
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
	// The clone hook is handed the bare's registry row id, and the outcome is
	// written by that id: a rename while it cloned moves the row's name and
	// nothing else. A row no longer there (ErrNoSuchRepository) has nothing to
	// record against and nothing for a client to merge into, so it is skipped
	// quietly. The stamp publishes the row the write returned.
	//
	// All three clone columns go on the wire because the write sets all three,
	// and off the row rather than restated from the arguments — so a success
	// after a failure clears the stored error instead of leaving a client
	// rendering it under a green status.
	publish := func(row domain.Repository) {
		notify.Publish(context.Background(), runmode.LocalDefaultOrgID, repoevent.Update{
			ID:             row.ID,
			Slug:           row.Slug(),
			CloneStatus:    repoevent.Ptr(row.CloneStatus),
			CloneError:     repoevent.Ptr(row.CloneError),
			CloneErrorKind: repoevent.Ptr(row.CloneErrorKind),
		})
	}
	worktree.SetOnCloneResult(func(bare worktree.Repo, cloneErr error) {
		owner, repo := bare.Owner, bare.Name
		record := func(status, errMsg, errKind string) {
			row, err := a.stores.Repos.UpdateCloneStatusSystem(context.Background(), runmode.LocalDefaultOrgID, bare.ID, status, errMsg, errKind)
			if errors.Is(err, db.ErrNoSuchRepository) {
				return
			}
			if err != nil {
				cloneStatusLog.Error("update clone status failed", "repository_id", bare.ID, "owner", owner, "repo", repo, "status", status, "error", err)
				return
			}
			publish(row)
		}
		if cloneErr == nil {
			record("ok", "", "")
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
			// The org's GitHub host, so GHE installs probe the right SSH
			// endpoint, not github.com.
			if sshHost := worktree.SSHHostFromBaseURL(orgSet.GitHubBaseURL); sshHost != "" {
				sshCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				if perr := worktree.CachedPreflightSSH(sshCtx, sshHost); perr != nil {
					kind = "ssh"
					cloneStatusLog.Warn("ssh preflight also failed; kind=ssh", "owner", owner, "repo", repo, "ssh_host", sshHost, "error", perr)
				} else {
					cloneStatusLog.Info("ssh preflight passed; kind=other (clone error is on the git side)", "owner", owner, "repo", repo, "ssh_host", sshHost)
				}
				cancel()
			}
		}

		record("failed", cloneErr.Error(), kind)
	})
}

// trackedRepositoryIDs is local's keep-set for the bare-clone reaper: the
// registry ids of the repositories a team tracks on the org's current GitHub
// host, the same set bootstrapBareClones warms. A clone of any other id is
// one nothing will read again: an untracked repository, a repository on a
// host the org has left, or a row a rename absorbed. An org with no GitHub
// host has no tracked set, and answers ok=false rather than an empty set so
// that disconnecting GitHub does not reclaim the clones a reconnect to the
// same host would use again.
func trackedRepositoryIDs(repos db.RepositoryStore, orgs db.OrgsStore) worktree.TrackedSet {
	return func(ctx context.Context) (map[string]bool, bool) {
		host, err := db.OrgGitHubHostSystem(ctx, orgs, runmode.LocalDefaultOrgID)
		if err != nil {
			worktreeLog.Warn("reaper: load github host failed; skipping the tracked-set reconcile", "error", err)
			return nil, false
		}
		if host == "" {
			return nil, false
		}
		rows, _, err := repos.ListTrackedSystem(ctx, runmode.LocalDefaultOrgID, host, db.Unwindowed)
		if err != nil {
			worktreeLog.Warn("reaper: load tracked repositories failed; skipping the tracked-set reconcile", "error", err)
			return nil, false
		}
		ids := make(map[string]bool, len(rows))
		for _, r := range rows {
			ids[r.ID] = true
		}
		return ids, true
	}
}

// bootstrapBareClones reads the repos at least one team tracks and asks the
// worktree package to ensure each is materialized on disk as a bare clone with
// the right origin URL. A registry row no team tracks is not warmed: nothing
// polls or profiles it. Called after profiling completes (profiling populates
// repositories.clone_url; targets without a CloneURL are skipped). DB read
// errors are logged and skipped — the lazy clone inside CreateForPR /
// CreateForBranch recovers affected delegations on the next run.
func bootstrapBareClones(repos db.RepositoryStore, orgs db.OrgsStore, secrets db.SecretStore) {
	orgSet, err := orgs.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		worktreeLog.Warn("bootstrap: load org settings failed", "error", err)
		return
	}
	profiles, _, err := repos.ListTrackedSystem(context.Background(), runmode.LocalDefaultOrgID, db.EffectiveGitHubHost(orgSet.GitHubBaseURL), db.Unwindowed)
	if err != nil {
		worktreeLog.Warn("bootstrap: load repositories failed", "error", err)
		return
	}
	// Load the org bot PAT once so HTTPS clones of private repos authenticate.
	// CloneAuthFor (applied per-target in BootstrapBareClones) no-ops on an SSH
	// CloneURL or an empty token, so the SSH default and public-repo paths are
	// unaffected — this only matters for an org whose clone protocol is https.
	// Best-effort: a load failure leaves the token empty and falls back to the
	// prior unauthenticated behavior rather than blocking the warm pass.
	creds, cerr := integrations.LoadSystem(context.Background(), secrets, runmode.LocalDefaultOrgID)
	if cerr != nil {
		worktreeLog.Warn("bootstrap: load credentials failed; HTTPS clones of private repos may fail to warm", "error", cerr)
	}
	token := creds.GitHubPAT
	targets := make([]worktree.BootstrapTarget, 0, len(profiles))
	for _, p := range profiles {
		targets = append(targets, worktree.BootstrapTarget{
			Repo:     worktree.Repo{ID: p.ID, Owner: p.Owner, Name: p.Repo},
			CloneURL: p.CloneURL,
			Token:    token,
		})
	}
	worktree.BootstrapBareClones(context.Background(), targets)
}
