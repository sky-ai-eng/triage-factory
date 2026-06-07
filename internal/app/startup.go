package app

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// runStartupTasks performs the one-time boot side effects, in order:
// register the clone-result callback (before any clone can fire), sweep
// orphaned worktrees, and (local only) repopulate the user's identity +
// import skill files. All of this runs before the background workers and
// the first poll.
func (a *App) runStartupTasks(ctx context.Context) {
	if a.local() {
		a.wireCloneStatusCallback()
	}
	a.cleanupWorktrees(ctx)
	if a.local() {
		a.localIdentityAndSkillFixup(ctx)
	}
}

// startWorkers starts the project classifier, the knowledge-base file
// watcher, and the long-lived background workers. The workers take the app
// context so they shut down cleanly on SIGINT/SIGTERM — previously these
// used a never-cancelled background context ("the binary has no top-level
// cancel today").
func (a *App) startWorkers(ctx context.Context) {
	a.classifier.Start()
	log.Println("[classify] project classifier started (model: haiku)")

	a.startKnowledgeWatcher()

	// Drain sweeper: safety net for queues stuck on transient fire errors.
	go a.router.RunDrainSweeper(ctx, 30*time.Second)
	// Durable event-queue drain worker: claims github:/jira: events the
	// ingestor enqueued, routes them, and marks them done.
	go a.router.RunEventQueue(ctx, a.eventWake, routing.DefaultEventScanInterval, routing.DefaultEventPruneInterval, routing.DefaultEventPruneAge)
	// Run-queue dispatcher: drains the run queue the spawner enqueues
	// blueprint steps onto, reconciling crash-stranded runs on boot.
	go a.spawner.RunDispatcher(ctx, delegate.DefaultRunScanInterval)
}

// startPolling kicks the first poll cycle (and, in local mode, wires the
// process-global GitHub identity). The logic lives on the reloader since it
// shares the local-mode profile→restart→score sequence with onGitHubChanged.
func (a *App) startPolling(ctx context.Context) {
	a.reloader.initialPoll(ctx)
}

// cleanupWorktrees removes orphaned worktrees from crashed runs. taken_over
// and parked runs are preserved (their ~/.claude/projects session JSONL is
// the warm resume cache). On a query error we still sweep worktree dirs +
// bare repos — those leaks compound fast — but skip all ~/.claude/projects
// deletions, since without the preserve set we can't distinguish a
// taken-over run's JSONL from an orphan.
//
// Non-local modes get the worktree-dir + bare-repo sweep but skip
// ~/.claude/projects entirely: the preserve set is keyed by the synthetic
// sentinel org, which has no real-tenant rows in multi mode.
func (a *App) cleanupWorktrees(ctx context.Context) {
	if !a.local() {
		worktree.CleanupWithOptions(worktree.CleanupOptions{SkipClaudeProjectCleanup: true})
		return
	}

	preserveIDs, err := a.stores.AgentRuns.ListTakenOverIDsSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		log.Printf("[server] WARNING: failed to load taken_over run ids — sweeping worktree dirs but skipping ~/.claude/projects cleanup to avoid clobbering active takeover sessions: %v", err)
		worktree.CleanupWithOptions(worktree.CleanupOptions{SkipClaudeProjectCleanup: true})
		return
	}

	preserveSet := make(map[string]bool, len(preserveIDs))
	for _, id := range preserveIDs {
		preserveSet[id] = true
	}
	// Parked runs (awaiting_input / pending_approval) keep their whole
	// worktree as the warm resume cache. A load failure just forgoes the
	// optimization — those runs still resume by rehydrating from snapshot.
	preserveWorktrees := map[string]bool{}
	if parkedPaths, perr := a.stores.AgentRuns.ListParkedWorktreePathsSystem(ctx, runmode.LocalDefaultOrgID); perr != nil {
		log.Printf("[server] WARNING: failed to load parked worktree paths; parked workspaces will rehydrate from snapshot rather than reuse the warm cache: %v", perr)
	} else {
		for _, p := range parkedPaths {
			preserveWorktrees[filepath.Base(p)] = true
		}
	}
	worktree.CleanupWithOptions(worktree.CleanupOptions{
		PreserveClaudeProjectFor: preserveSet,
		PreserveWorktreeFor:      preserveWorktrees,
	})
}

// localIdentityAndSkillFixup repopulates the local user's Jira identity and
// imports any new Claude Code skill files as prompts. Gated on tenant
// existence: on a tenant-less boot it no-ops; after a prior provision it
// refreshes the users row. GitHub identity (PAT_2) is deliberately NOT
// bootstrapped from the org PAT here — per-user identity is captured only
// through the setup wizard's User step / the Connect gate page.
func (a *App) localIdentityAndSkillFixup(ctx context.Context) {
	org, err := a.stores.Orgs.GetOrgSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		log.Printf("[bootstrap] check local tenant: %v (skipping identity/skill fixup)", err)
		return
	}
	if org == nil {
		return
	}
	if err := bootstrapLocalJiraIdentity(a.stores.Users, a.stores.Secrets); err != nil {
		log.Printf("[bootstrap] users.jira_identity: %v (continuing — Settings will capture on next save)", err)
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
				log.Printf("[clone-status] update %s/%s ok: %v", owner, repo, err)
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

		log.Printf("[clone-status] %s/%s clone failed: %v", owner, repo, cloneErr)

		kind := "other"
		orgSet, oErr := a.stores.Orgs.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
		if oErr != nil {
			log.Printf("[clone-status] %s/%s load org settings to classify: %v (defaulting to kind=other)", owner, repo, oErr)
		} else if orgSet.GitHubCloneProtocol == "ssh" {
			// Use the configured GitHub host so GHE installs probe the right
			// SSH endpoint, not github.com.
			creds, _ := integrations.Load(context.Background(), a.stores.Secrets, runmode.LocalDefaultOrgID)
			sshHost := worktree.SSHHostFromBaseURL(creds.GitHubURL)
			sshCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if perr := worktree.CachedPreflightSSH(sshCtx, sshHost); perr != nil {
				kind = "ssh"
				log.Printf("[clone-status] %s/%s SSH preflight against %s also failed → kind=ssh: %v", owner, repo, sshHost, perr)
			} else {
				log.Printf("[clone-status] %s/%s SSH preflight against %s passed → kind=other (clone error is on the git side)", owner, repo, sshHost)
			}
			cancel()
		}

		if err := a.stores.Repos.UpdateCloneStatusSystem(context.Background(), runmode.LocalDefaultOrgID, owner, repo, "failed", cloneErr.Error(), kind); err != nil {
			log.Printf("[clone-status] update %s/%s failed: %v", owner, repo, err)
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
			log.Printf("[kbwatcher] resolve org for project %s: %v (dropping live update)", projectID, err)
			return ""
		}
		if orgID == "" {
			log.Printf("[kbwatcher] no org for project %s — stale dir or unresolved row (dropping live update)", projectID)
			return ""
		}
		return orgID
	}
	if root, err := curator.ProjectsWatchRoot(); err != nil {
		log.Printf("[kbwatcher] resolve projects root: %v (live KB updates disabled)", err)
	} else if _, err := curator.NewKnowledgeWatcher(a.wsHub, root, resolveOrgForProject); err != nil {
		log.Printf("[kbwatcher] start: %v (live KB updates disabled)", err)
	}
}

// bootstrapBareClones reads the configured repos and asks the worktree
// package to ensure each is materialized on disk as a bare clone with the
// right origin URL. Called after profiling completes (profiling populates
// repo_profiles.clone_url; targets without a CloneURL are skipped). DB read
// errors are logged and skipped — the lazy clone inside CreateForPR /
// CreateForBranch recovers affected delegations on the next run.
func bootstrapBareClones(repos db.RepoStore) {
	profiles, err := repos.ListSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		log.Printf("[worktree] bootstrap: load profiles: %v", err)
		return
	}
	targets := make([]worktree.BootstrapTarget, 0, len(profiles))
	for _, p := range profiles {
		targets = append(targets, worktree.BootstrapTarget{
			Owner:    p.Owner,
			Repo:     p.Repo,
			CloneURL: p.CloneURL,
		})
	}
	worktree.BootstrapBareClones(context.Background(), targets)
}

// bootstrapLocalJiraIdentity populates users.jira_account_id and
// users.jira_display_name on the local synthetic user row by deriving them
// from the configured Jira PAT+URL (one /myself round-trip per boot). No-op
// when the row is already populated, credentials are absent, or ValidateJira
// fails. There is intentionally no GitHub analog — see localIdentityAndSkillFixup.
func bootstrapLocalJiraIdentity(users db.UsersStore, secrets db.SecretStore) error {
	if runmode.Current() != runmode.ModeLocal {
		return nil
	}
	ctx := context.Background()

	creds, _ := integrations.Load(ctx, secrets, runmode.LocalDefaultOrgID)
	if creds.JiraPAT == "" || creds.JiraURL == "" {
		return nil
	}
	existingID, existingName, err := users.GetJiraIdentity(ctx, runmode.LocalDefaultUserID)
	if err != nil {
		return fmt.Errorf("read users.jira_identity: %w", err)
	}
	if existingID != "" && existingName != "" {
		return nil
	}
	jiraUser, err := auth.ValidateJira(ctx, jira.DataCenterPAT(creds.JiraURL, creds.JiraPAT))
	if err != nil {
		log.Printf("[bootstrap] derive users.jira_identity from PAT: %v (continuing — Settings will capture next save)", err)
		return nil
	}
	accountID := jiraUser.StableID()
	if err := users.SetJiraIdentity(ctx, runmode.LocalDefaultUserID, accountID, jiraUser.DisplayName); err != nil {
		return fmt.Errorf("persist users.jira_identity: %w", err)
	}
	log.Printf("[bootstrap] users.jira_identity: derived account=%q name=%q from credentials", accountID, jiraUser.DisplayName)
	return nil
}
