// Top-level entry points for kicking off a delegated agent run, plus
// the source-specific worktree setup (GitHub PR vs Jira lazy) the
// generic runAgent loop consumes via runConfig.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// runConfig holds everything the generic agent runner needs.
//
// Two delegation shapes share this struct:
//
//   - GitHub PR (eager): hasWT=true, wtPath is the worktree, runRoot=wtPath
//     (the worktree IS the run-root), owner/repo populated from the PR.
//     Cleanup uses RemoveAt(wtPath) + CleanupPRConfig.
//
//   - Jira (lazy): hasWT=false, wtPath=runRoot is the throwaway run-root
//     (initial cwd; holds _scratch/entity-memory/ but no codebase), owner/repo empty.
//     Per-repo worktrees materialize as subdirs under runRoot via the
//     `triagefactory exec workspace add` CLI; the run_worktrees DB table
//     is the source of truth for cleanup, which iterates the table at
//     runAgent terminal.
type runConfig struct {
	orgID     string  // tenant scope for every store call inside this run's goroutine — set once in Delegate from opts.OrgID, then read everywhere via cfg.orgID instead of being threaded positionally
	scope     string  // what the agent is scoped to (repo, PR, issue)
	toolsRef  string  // tool documentation to inject
	wtPath    string  // initial cwd: GitHub PR worktree, or Jira run-root
	hasWT     bool    // GitHub PR has a real worktree to clean up via RemoveAt; Jira's worktrees are tracked in run_worktrees and cleaned by iterating that table
	runRoot   string  // run-root path: GitHub PR runs == wtPath; Jira lazy runs == the throwaway parent of materialized worktrees. Always set so $TRIAGE_FACTORY_RUN_ROOT resolves uniformly for the memory-gate retry.
	owner     string  // resolved GitHub owner (empty for Jira lazy runs)
	repo      string  // resolved GitHub repo (empty for Jira lazy runs)
	prNumber  int     // PR number (0 for non-PR runs); set so the runAgent defer can call worktree.CleanupPRConfig and reclaim the per-PR remote + branch tracking config the bare repo would otherwise accumulate
	headRef   string  // PR head ref (empty for non-PR runs); passed to CleanupPRConfig so own-repo branch tracking (branch.<headRef>.*) gets reclaimed alongside fork-only artifacts
	projectID *string // entity's project assignment (nil for un-assigned); SKY-219 uses this to copy the project's knowledge-base into ./_scratch/project-knowledge/

	extraAllowedTools string // comma-separated extra tools from prompt.AllowedTools + agent scans; merged into --allowedTools at spawn time

	// Chain-mode toggles. When isBlueprintStep is true the chain
	// orchestrator owns the worktree lifecycle: runAgent's cleanup
	// defers (RemoveAt, RemoveRunRoot, RemoveClaudeProjectDir) all
	// short-circuit so the worktree survives across steps. The
	// orchestrator runs the equivalent cleanup once after the chain
	// terminates. appendSysPrompt is forwarded to agentproc as
	// --append-system-prompt so the chain protocol reaches the model
	// without modifying the step's prompt body.
	blueprintRunID  string
	blueprintStep   int
	isBlueprintStep bool
	appendSysPrompt string
}

// ErrAlreadyFired is returned by Delegate on the event path when the run
// insert hit the (triggering_event_id, trigger_id) fence — a run for this
// (event, trigger) already committed on a prior delivery of the same event
// (SKY-424). The router treats it as a clean skip: the at-least-once event
// queue replayed an event whose first auto-delegation already happened, so
// there is nothing to re-fire and the original run's claim still stands.
var ErrAlreadyFired = errors.New("delegate: run already fired for this (event, trigger)")

// DelegateOpts carries the per-call inputs to Delegate that grew past
// the comfortable positional-arg threshold. The struct exists so
// adding a field (CreatorUserID, future multi-mode session context)
// doesn't force every handler call site to rewrite its positional
// list.
type DelegateOpts struct {
	// OrgID is the tenant scope for every store call this Delegate
	// call (and the goroutine it spawns) will make. Handler call
	// sites extract it from r.Context() via the requireOrg /
	// OrgIDFrom accessors; router-triggered calls pass the local-
	// mode sentinel until the per-org router lands. The goroutine
	// caches this on cfg.orgID and every downstream store call
	// (run row, side-tables, memory, chain steps) routes under it.
	// Required — callers must always set this; the spawner trusts
	// the input and does not re-validate.
	OrgID string

	// ExplicitBlueprintID forces a specific blueprint to fire instead of
	// letting the caller's default path pick one. Manual delegation supplies
	// the user's chosen blueprint; auto-delegation supplies the trigger row's
	// blueprint_id. Required — the picker / trigger always names a blueprint.
	ExplicitBlueprintID string

	// TriggerType is "manual" (user clicked Delegate) or "event"
	// (router auto-fired from a matched event_handler). Drives the
	// routing decision inside the goroutine: synthetic-claims for
	// manual (so prompts_select RLS filters by user, runs_insert RLS
	// sees creator_user_id), admin pool for event (router has no
	// user identity to project). Defaults to "manual" if empty.
	//
	// **Server-side provenance, never caller-derived.** API handlers
	// hardcode "manual" (factory_delegate.go, tasks.go); the event
	// router hardcodes "event" (routing/router.go). There is no path
	// where this field is unmarshaled from a request body. The
	// invariant is load-bearing: if a caller could set
	// TriggerType="event", resolvePrompt's app-pool branch would be
	// bypassed and the caller could read prompts they can't see.
	// internal/server/delegate_trigger_invariant_test.go pins this.
	TriggerType string

	// TriggerID is the event_handler ID for event-triggered runs,
	// empty for manual. Recorded on the runs row for audit.
	TriggerID string

	// TriggeringEventID is the event instance that fired this run, for
	// event-triggered delegations; empty for manual (SKY-424). Server-side
	// provenance like TriggerID — the router threads it from the event
	// being processed (immediate path) or the pending firing row (drain
	// path). Paired with TriggerID it drives the runs_event_trigger_fence:
	// the event-path insert is conflict-aware, so a replayed event whose
	// first run already committed returns ErrAlreadyFired instead of
	// spawning a duplicate. Required on the event path — CreateIfNotFiredSystem
	// rejects an empty value (it would bind NULL and silently skip the fence)
	// with ErrFenceRequiresEventAndTrigger. Manual delegation never sets this
	// field; it uses the unfenced Create, which doesn't write the column.
	TriggeringEventID string

	// CreatorUserID is the user who initiated this Delegate call.
	// Required when TriggerType == "manual"; ignored otherwise (the
	// schema CHECK refuses non-NULL creator_user_id for event-
	// triggered runs). In local mode this is
	// runmode.LocalDefaultUserID; in multi-mode the handler extracts
	// it from JWT claims.
	CreatorUserID string
}

// Delegate kicks off an async agent run for any task type.
// Routes to the appropriate worktree setup based on task source.
func (s *Spawner) Delegate(task domain.Task, opts DelegateOpts) (string, error) {
	orgID := opts.OrgID

	// Resolve this run's GitHub client + default model per (org, owner, team).
	// Both modes go through the same seam (SKY-389): the resolver mints an
	// App-installation token in multi or borrows the keychain PAT in local;
	// modelFor reads the task's team default (a multi-team org honors each
	// team's model choice). owner is parsed from the task (empty for Jira
	// runs); task.TeamID is the run's owning team. context.Background —
	// Delegate has no request ctx and the resolve is a bounded token-mint /
	// DB read; the run's own cancellable ctx is created further down for the
	// agent subprocess.
	ghClient, defaultModel := s.resolveRunCredentials(context.Background(), orgID, ownerForTask(task), task.TeamID)

	// Compute trigger type + creator user up front so resolvePrompt
	// can route by them (manual delegations must honor prompts_select
	// RLS; event-triggered ones stay on the admin pool).
	triggerType := opts.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	triggerID := opts.TriggerID

	// Pair creator_user_id with trigger_type per the audit-honesty
	// invariant. Manual runs (swipe-delegate / drag-to-Agent / factory
	// drop) carry the initiating user's id as the creator. Event-
	// triggered runs carry NULL — there's no human delegator. The
	// schema CHECK enforces this pairing so the seeder can't drift.
	creatorUserID := ""
	if triggerType == "manual" {
		creatorUserID = opts.CreatorUserID
		if creatorUserID == "" {
			// Local-mode fallback. Multi-mode handlers extract this
			// from JWT claims and pass it explicitly; an empty value
			// here in multi-mode would FK-fail at Create time.
			creatorUserID = runmode.LocalDefaultUserID
		}
	}

	// Resolve the blueprint to fire + its ordered steps under the right pool
	// for the trigger type. Execution parity branches on step count, not on a
	// kind discriminator: a 1-step blueprint runs the single-prompt path; a
	// multi-step blueprint runs the orchestrator.
	blueprint, err := s.resolveBlueprint(orgID, opts.ExplicitBlueprintID, triggerType, creatorUserID)
	if err != nil {
		return "", err
	}
	steps, err := s.resolveBlueprintSteps(orgID, blueprint.ID, triggerType, creatorUserID)
	if err != nil {
		return "", fmt.Errorf("load blueprint steps: %w", err)
	}
	if len(steps) == 0 {
		return "", fmt.Errorf("blueprint %q has no steps", blueprint.Name)
	}

	model := defaultModel

	// Bump blueprint usage_count, routed per trigger type. Manual delegations
	// write under the user's synthetic claims; event-triggered ones fire
	// through the admin pool.
	bgCtx := context.Background()
	if triggerType == "manual" {
		if e := s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			return ts.Blueprints.IncrementUsage(bgCtx, orgID, blueprint.ID)
		}); e != nil {
			log.Printf("[delegate] warning: failed to increment usage for blueprint %s: %v", blueprint.ID, e)
		}
	} else if e := s.blueprints.IncrementUsageSystem(bgCtx, orgID, blueprint.ID); e != nil {
		log.Printf("[delegate] warning: failed to increment usage for blueprint %s: %v", blueprint.ID, e)
	}

	// Multi-step blueprint → the (renamed) orchestrator: one blueprint_run
	// row, the chain:verdict path between steps, byte-for-byte today's chain.
	if len(steps) > 1 {
		return s.delegateBlueprint(orgID, task, blueprint, steps, triggerType, triggerID, creatorUserID, ghClient, model)
	}

	// 1-step blueprint → today's single-prompt path on that step's prompt:
	// no blueprint_run row, completion envelope closes the task.
	resolved, err := s.resolvePrompt(orgID, task, steps[0].StepPromptID, triggerType, creatorUserID)
	if err != nil {
		return "", err
	}
	promptID := resolved.ID
	mission := resolved.Body
	if resolved.Model != "" {
		model = resolved.Model
	}
	extraTools := s.collectExtraTools(resolved.AllowedTools)

	var incErr error
	if triggerType == "manual" {
		incErr = s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			return ts.Prompts.IncrementUsage(bgCtx, orgID, promptID)
		})
	} else {
		incErr = s.prompts.IncrementUsageSystem(bgCtx, orgID, promptID)
	}
	if incErr != nil {
		log.Printf("[delegate] warning: failed to increment usage for prompt %s: %v", promptID, incErr)
	}

	// Stamp actor_agent_id at run start. Prefer the task's stamped
	// claim (set by router on trigger match, or by the user-delegate
	// handler when the user drags a queued task to the bot). Falls
	// back to GetForOrg for unclaimed tasks and manual runs. Stays
	// empty (NULL on the row) only in the seam between db init and
	// agent bootstrap, which is transient.
	actorAgentID := task.ClaimedByAgentID
	if actorAgentID == "" && s.agents != nil {
		if a, err := s.agents.GetForOrgSystem(context.Background(), orgID); err == nil && a != nil {
			actorAgentID = a.ID
		}
	}

	runID := uuid.New().String()
	runRow := domain.AgentRun{
		ID:                runID,
		TaskID:            task.ID,
		PromptID:          promptID,
		Status:            "initializing",
		Model:             model,
		TriggerType:       triggerType,
		TriggerID:         triggerID,
		TriggeringEventID: opts.TriggeringEventID,
		ActorAgentID:      actorAgentID,
		CreatorUserID:     creatorUserID,
	}
	// Route by trigger type, mirroring the IncrementUsage split above.
	// Manual inserts wrap in SyntheticClaimsWithTx so runs_insert's RLS
	// check (creator_user_id = tf.current_user_id()) sees the correct
	// identity; event-triggered inserts stay on the admin pool because
	// the router has no user identity to project.
	//
	// Event-triggered runs go through CreateIfNotFiredSystem, the
	// (triggering_event_id, trigger_id) fenced insert (SKY-424). On a
	// replayed event whose first run already committed, no row inserts and
	// we return ErrAlreadyFired so the router skips cleanly instead of
	// spawning a duplicate agent. The run insert is the crash-consistent
	// commit point: anything before it (blueprint/prompt resolution, usage
	// bumps) is cheap and idempotent enough to re-run on a fenced replay.
	var createErr error
	if triggerType == "manual" {
		createErr = s.tx.SyntheticClaimsWithTx(context.Background(), orgID, creatorUserID, func(ts db.TxStores) error {
			return ts.AgentRuns.Create(context.Background(), orgID, runRow)
		})
	} else {
		inserted, err := s.agentRuns.CreateIfNotFiredSystem(context.Background(), orgID, runRow)
		if err != nil {
			createErr = err
		} else if !inserted {
			return "", ErrAlreadyFired
		}
	}
	if createErr != nil {
		return "", fmt.Errorf("create agent run: %w", createErr)
	}
	s.broadcastRunUpdate(orgID, runID, "initializing")

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[runID] = cancel
	s.mu.Unlock()

	go func() {
		startTime := time.Now()
		defer func() {
			s.mu.Lock()
			delete(s.cancels, runID)
			// takenOver is intentionally NOT deleted here — Takeover
			// is still running its copy/finalize work in another
			// goroutine and reads wasTakenOver() up to the very last
			// step. Leaving the entry set forever costs a few bytes
			// per takeover and avoids subtle TOCTOU races.
			s.mu.Unlock()
			cancel()
			// Drain the per-entity firing queue. Fires after every
			// terminal status (completed, failed, cancelled,
			// task_unsolvable, pending_approval, taken_over) since
			// this defer runs unconditionally on goroutine exit.
			// notifyDrainer itself filters out manual runs — manual
			// is decoupled from the queue per SKY-189.
			s.notifyDrainer(orgID, triggerType, task.EntityID)
		}()

		// Phase 1: set up worktree + build config based on task source
		var cfg runConfig
		var setupErr error

		switch task.EntitySource {
		case "github":
			cfg, setupErr = s.setupGitHub(ctx, orgID, runID, task, ghClient)
		case "jira":
			cfg, setupErr = s.setupJira(ctx, orgID, runID, task, ghClient)
		default:
			setupErr = fmt.Errorf("unsupported task source: %s", task.EntitySource)
		}
		cfg.orgID = orgID

		if setupErr != nil {
			if ctx.Err() != nil {
				s.handleCancelled(orgID, runID, startTime, cfg.wtPath, triggerType, creatorUserID)
				return
			}
			s.failRun(orgID, runID, task.ID, triggerType, creatorUserID, setupErr.Error())
			return
		}

		cfg.extraAllowedTools = extraTools

		// Phase 2: run the agent
		// Announce the run start once setup is done — the agent is about to
		// actually execute work. Distinguish auto-fired (event trigger) from
		// user-initiated (manual) so the user can tell at a glance whether
		// they kicked this off or automation did.
		verb := "Run started"
		if triggerType == "event" {
			verb = "Auto-fired"
		}
		toast.Info(s.wsHub, orgID, fmt.Sprintf("%s: %s (%s)", verb, truncateToastMsg(task.Title, 80), shortRunID(runID)))
		s.runAgent(ctx, runID, task, mission, cfg, startTime, model, triggerType, creatorUserID)
	}()

	return runID, nil
}

// ownerForTask extracts the GitHub account a task targets, for the
// per-(org, owner) credential resolution at Delegate entry (SKY-389).
// GitHub PR tasks carry "owner/repo#N" in EntitySourceID; Jira (and any
// non-github source) has no single owner, so the resolver receives "" and
// resolves the org's sole App installation, falling through to PAT when
// ambiguous.
func ownerForTask(task domain.Task) string {
	if task.EntitySource != "github" {
		return ""
	}
	repoStr := task.EntitySourceID
	if idx := strings.LastIndex(repoStr, "#"); idx >= 0 {
		repoStr = repoStr[:idx]
	}
	owner, _ := parseOwnerRepo(repoStr)
	return owner
}

// setupGitHub prepares a worktree for a GitHub PR task.
func (s *Spawner) setupGitHub(ctx context.Context, orgID, runID string, task domain.Task, ghClient *ghclient.Client) (runConfig, error) {
	if ghClient == nil {
		return runConfig{}, fmt.Errorf("GitHub credentials not configured")
	}

	// EntitySourceID for GitHub PRs is "owner/repo#42" — extract the repo part.
	repoStr := task.EntitySourceID
	if idx := strings.LastIndex(repoStr, "#"); idx >= 0 {
		repoStr = repoStr[:idx]
	}
	owner, repo := parseOwnerRepo(repoStr)
	if owner == "" || repo == "" {
		return runConfig{}, fmt.Errorf("cannot parse owner/repo from entity source ID: %q", task.EntitySourceID)
	}

	prNumber := 0
	if idx := strings.LastIndex(task.EntitySourceID, "#"); idx >= 0 {
		fmt.Sscanf(task.EntitySourceID[idx+1:], "%d", &prNumber)
	}
	if prNumber == 0 {
		return runConfig{}, fmt.Errorf("invalid PR number from task.EntitySourceID: %q", task.EntitySourceID)
	}

	s.updateStatus(orgID, runID, "fetching")
	pr, err := ghClient.GetPR(owner, repo, prNumber, false)
	if err != nil {
		return runConfig{}, fmt.Errorf("failed to fetch PR: %w", err)
	}

	// pr.BaseCloneURL / pr.BaseSSHURL: the upstream (the repo where
	// /pulls/<n> lives, which is always the canonical repo by
	// construction), populated from base.repo.{clone_url,ssh_url}.
	// pr.CloneURL / pr.SSHURL: the head's URL — the fork's URL when
	// the PR is from a fork, equal to the base URL for own-repo PRs.
	// CreateForPR uses the upstream URL to fetch refs/pull/<n>/head
	// and (if they differ) the head URL to configure push tracking so
	// commits land in the fork's branch instead of creating a stray
	// branch on upstream.
	//
	// Pick HTTPS or SSH form based on the user's config. We can't
	// just always pass HTTPS — repairOriginURL inside CreateForPR
	// rewrites the bare's origin to whatever URL we pass, so passing
	// HTTPS would clobber the SSH origin that bootstrap put there.
	//
	// Failure modes when SSH is selected:
	//   - pr.BaseSSHURL empty: the API didn't return base.repo.ssh_url
	//     (theoretically possible on weird GHE configs). Fail loudly
	//     rather than fall back to HTTPS — falling back would silently
	//     repoint the bare to HTTPS via repairOriginURL.
	//   - pr.SSHURL empty while pr.CloneURL non-empty: same condition
	//     for the head repo (the fork). Fork tracking would silently
	//     mix origins (SSH bare, HTTPS push remote) and break `git push`
	//     for SSH-only users. Same fail-loud treatment.
	// pr.CloneURL == "" (head.repo == null) is the deleted-fork case;
	// pr.SSHURL is also empty there, and we leave headCloneURL = ""
	// so CreateForPR's hasHeadRepo=false branch fires correctly.
	upstreamCloneURL, headCloneURL := pr.BaseCloneURL, pr.CloneURL
	if s.useSSHCloneProtocol(ctx, orgID, runID) {
		if pr.BaseSSHURL == "" {
			return runConfig{}, fmt.Errorf("PR #%d on %s/%s: SSH clone protocol selected but GitHub did not return base.repo.ssh_url; switch to HTTPS in Settings or check your GHE config", prNumber, owner, repo)
		}
		upstreamCloneURL = pr.BaseSSHURL
		if pr.CloneURL != "" {
			if pr.SSHURL == "" {
				return runConfig{}, fmt.Errorf("PR #%d on %s/%s: SSH clone protocol selected but GitHub did not return head.repo.ssh_url for the head fork; switch to HTTPS in Settings or check your GHE config", prNumber, owner, repo)
			}
			headCloneURL = pr.SSHURL
		}
	}
	if upstreamCloneURL == "" {
		return runConfig{}, fmt.Errorf("PR #%d on %s/%s: GitHub did not return a usable upstream URL; cannot create worktree", prNumber, owner, repo)
	}

	s.updateStatus(orgID, runID, "cloning")
	// Resolve the host-side clone credential for this repo's owner.
	// CloneAuthFor scopes the token to the upstream's host and no-ops on an
	// SSH URL or an empty token, so this is inert in local SSH mode and for
	// public/anonymous clones — only a multi-mode HTTPS private clone gets the
	// App installation token injected (per-invocation env, never persisted).
	cloneToken := s.resolveCloneToken(ctx, orgID, owner)
	wtPath, err := worktree.CreateForPR(ctx, owner, repo, upstreamCloneURL, headCloneURL, pr.HeadRef, prNumber, runID,
		worktree.WithCloneAuth(worktree.CloneAuthFor(upstreamCloneURL, cloneToken)))
	if err != nil {
		return runConfig{}, fmt.Errorf("failed to create worktree: %w", err)
	}

	if err := s.agentRuns.SetWorktreePathSystem(context.Background(), orgID, runID, wtPath); err != nil {
		log.Printf("[delegate] warning: failed to update worktree path for run %s: %v", runID, err)
	}

	// SKY-220: block briefly so the project classifier (post-poll
	// runner) can decide this entity before we read project_id for KB
	// injection. Nil-safe — tests and pre-classifier configurations
	// skip the wait.
	s.awaitClassification(ctx, orgID, task.EntityID)

	return runConfig{
		orgID:     orgID,
		scope:     fmt.Sprintf("Repository: %s/%s\nPR: #%d\nBranch: %s", owner, repo, prNumber, pr.HeadRef),
		toolsRef:  ai.GHToolsTemplate,
		wtPath:    wtPath,
		hasWT:     true,
		runRoot:   wtPath, // GitHub PR runs: worktree IS the run-root, so $TRIAGE_FACTORY_RUN_ROOT resolves to the worktree
		owner:     owner,
		repo:      repo,
		prNumber:  prNumber,
		headRef:   pr.HeadRef,
		projectID: lookupEntityProjectID(s.entities, orgID, task.EntityID),
	}, nil
}

// setupJira prepares the run-root for a Jira delegation. No repo is
// pre-cloned — the agent decides which repo(s) it needs after reading
// the ticket and materializes them via `triagefactory exec workspace
// add <owner/repo>`. Each materialization lands a worktree at
// {runRoot}/{owner}/{repo}/ and inserts a row into run_worktrees.
//
// The agent's initial cwd is the run-root: a throwaway dir holding
// only ./_scratch/entity-memory/ (populated by materializePriorMemories
// below). Both gh and jira tool surfaces are exposed since the agent
// will need both to implement and ship a PR.
//
// runs.worktree_path is set to the run-root. Yield/resume reads this
// field as the cwd to resume the session in (`claude --resume` keys
// session storage by cwd-encoded ~/.claude/projects/<encoded>, and we
// passed cwd=runRoot to the original agentproc.Run). Even though Jira
// runs don't have a single "the worktree" the way GitHub PR runs do,
// the run-root IS the agent's session cwd, which is the load-bearing
// invariant for resume. Takeover guards against Jira runs explicitly
// further down.
func (s *Spawner) setupJira(ctx context.Context, orgID, runID string, task domain.Task, ghClient *ghclient.Client) (runConfig, error) {
	runRoot, err := worktree.MakeRunRoot(runID)
	if err != nil {
		return runConfig{}, fmt.Errorf("create run root: %w", err)
	}
	if err := s.agentRuns.SetWorktreePathSystem(context.Background(), orgID, runID, runRoot); err != nil {
		log.Printf("[delegate] warning: failed to set worktree_path for Jira run %s: %v — yield/resume will reject this run", runID, err)
	}

	// SKY-220: block briefly so the project classifier can decide this
	// entity before we read project_id for KB injection. Nil-safe.
	s.awaitClassification(ctx, orgID, task.EntityID)

	return runConfig{
		orgID:     orgID,
		scope:     fmt.Sprintf("Jira issue: %s", task.EntitySourceID),
		toolsRef:  ai.GHToolsTemplate + "\n\n" + ai.JiraToolsTemplate,
		wtPath:    runRoot,
		hasWT:     false,
		runRoot:   runRoot,
		projectID: lookupEntityProjectID(s.entities, orgID, task.EntityID),
		// owner/repo intentionally empty: the agent picks per-ticket via `workspace add`
	}, nil
}
