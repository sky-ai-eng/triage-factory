// Top-level entry points for kicking off a delegated agent run, plus
// the source-specific worktree setup (GitHub PR vs Jira lazy) the
// generic runAgent loop consumes via runConfig.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/prskeleton"
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
//     (initial cwd; holds _tfac/ but no codebase), owner/repo empty.
//     Per-repo worktrees materialize as subdirs under runRoot via the
//     `triagefactory exec workspace add` CLI; the conversation_worktrees DB table
//     is the source of truth for cleanup, which iterates the table at
//     runAgent terminal.
type runConfig struct {
	orgID    string // tenant scope for every store call inside this conversation's goroutine — set once in Delegate from opts.OrgID, then read everywhere via cfg.orgID instead of being threaded positionally
	claimID  string // the engagement driving this conversation — the claims row ClaimNextConversation minted, threaded through so teardown can stamp the claim's measured sandbox cost by id (an active-claim lookup would race the release). Empty on paths with no claimed conversation in scope, which record no actuals.
	teamID   string // the conversation's owning team (conversations.team_id, NOT NULL), stamped alongside orgID from the claimed conversation row; read at construction to populate agenthost.ConversationInfo.TeamID so the capture writers can stamp artifacts.team_id (TFAC-458). Also stamped on the conversation-bearing terminal paths (dispatchClaimedConversation / handlePreAgentFailure); empty only on the CancelBlueprintRun / paused-cleanup paths that have a task but no claimed conversation in scope.
	scope    string // what the agent is scoped to (repo, PR, issue)
	toolsRef string // tool documentation to inject
	wtPath   string // initial cwd: GitHub PR worktree, or Jira run-root
	hasWT    bool   // GitHub PR has a real worktree to clean up via RemoveAt; Jira's worktrees are tracked in conversation_worktrees and cleaned by iterating that table
	runRoot  string // run-root path: GitHub PR runs == wtPath; Jira lazy runs == the throwaway parent of materialized worktrees. Always set so $TRIAGE_FACTORY_CONVERSATION_ROOT resolves uniformly for the memory-gate retry.
	owner    string // resolved GitHub owner (empty for Jira lazy runs)
	repo     string // resolved GitHub repo (empty for Jira lazy runs)
	prNumber int    // PR number (0 for non-PR runs); set so the runAgent defer can call worktree.CleanupPRConfig and reclaim the per-run branch + push remote the bare repo would otherwise accumulate

	// prSkeleton is the rendered PR history block folded into the run's
	// static task context. Empty for a non-PR run, and empty (never fatal)
	// when the fetch fails — the run proceeds with the point-in-time event
	// context it has always had.
	prSkeleton string

	// workspace is how wtPath came to be — warm from the previous engagement,
	// rehydrated from the durable snapshot, or built fresh. Resolved by the
	// setup that produced the tree (buildStepConfig), because nothing
	// downstream can tell a warm tree from a reconstruction of one. Empty on
	// paths that build no workspace.
	workspace domain.WorkspaceProvenance

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

	// skillsSourcePath is the orchestrator-owned staging dir holding this step's
	// SKILL.md, bind-mounted read-only into the jail (agentproc's
	// SkillsSourcePath). Set only for a sandboxed blueprint step; empty in local
	// mode, where the skill is written into the worktree instead.
	skillsSourcePath string

	// memorySourcePath is the orchestrator-owned staging dir holding the
	// entity-memory tree materialized for this launch, bind-mounted read-only
	// into the jail (agentproc's MemorySourcePath). Set for every sandboxed
	// launch; empty in local mode, where the same tree is rendered inside the
	// worktree instead.
	memorySourcePath string

	// sidecar, when non-nil (TF_ROLE=executor), is the run network +
	// credential sidecar + proxy coordinates the dispatcher stood up before
	// workspace setup. runAgent threads it into agentproc.RunOptions
	// (PrebuiltNetwork/PrebuiltProxyEnv) and the agenthost (ProxyCredentials);
	// the dispatcher owns its teardown (after runAgent returns).
	sidecar *runSidecar

	// localGit is the local-mode per-engagement Git credential proxy. It starts
	// before workspace setup and is owned by the dispatcher, which keeps it live
	// through the agent invocation and closes it afterward.
	localGit *localGitChannel
}

// ErrTaskBusy is returned by Delegate on the event path when the fenced
// insert loses to the one-active-auto-run-per-task index: a different
// (event, trigger) pair went active on the same task between the
// router's gate read and this insert. Unlike ErrAlreadyFired (permanently
// satisfied — the run for this exact event exists), task-busy means the
// caller's intent is still valid and must be deferred onto
// pending_firings (or released back there), never dropped.
var ErrTaskBusy = errors.New("delegate: another auto run is active on this task")

// ErrAlreadyFired is returned by Delegate on the event path when the run
// insert hit the (triggering_event_id, trigger_id) fence — a run for this
// (event, trigger) already committed on a prior delivery of the same event.
// The router treats it as a clean skip: the at-least-once event
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
	// (conversation row, side-tables, memory, chain steps) routes under it.
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
	// manual (so prompts_select RLS filters by user, conversations_insert RLS
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
	// empty for manual. Recorded on the conversations row for audit.
	TriggerID string

	// TriggeringEventID is the event instance that fired this run, for
	// event-triggered delegations; empty for manual. Server-side
	// provenance like TriggerID — the router threads it from the event
	// being processed (immediate path) or the pending firing row (drain
	// path). Paired with TriggerID it drives the conversations_event_trigger_fence:
	// the event-path insert is conflict-aware, so a replayed event whose
	// first run already committed returns ErrAlreadyFired instead of
	// spawning a duplicate. Required on the event path —
	// BlueprintStore.CreateRunIfNotFiredSystem rejects an empty value (it would
	// bind NULL and silently skip the fence) with
	// ErrBlueprintRunFenceRequiresEventAndTrigger. Manual delegation never sets
	// this field; it uses the unfenced Create, which doesn't write the column.
	TriggeringEventID string

	// CreatorUserID is the user who initiated this Delegate call.
	// Required when TriggerType == "manual"; ignored otherwise (the
	// schema CHECK refuses non-NULL creator_user_id for event-
	// triggered runs). In local mode this is
	// runmode.LocalDefaultUserID; in multi-mode the handler extracts
	// it from JWT claims.
	CreatorUserID string

	// ActorAgentID is the agents.id of the bot that will execute this
	// delegation — server-side provenance like TriggerID, resolved by the
	// caller (the router resolves the org/team agent before firing; the manual
	// handlers hold the agent they claimed the task with; the drain path passes
	// the task's already-stamped claim). Delegate freezes it onto
	// blueprint_runs.actor_agent_id at mint, and every step run inherits it onto
	// conversations.actor_agent_id. Empty is tolerated (→ NULL on the row, the honest
	// "no actor" answer) but every production caller supplies it, so the run's
	// execution attribution never depends on re-reading a task claim that may
	// not be written yet at step 0.
	ActorAgentID string

	// TaskClaim rides the task's agent claim into the same transaction as the
	// fenced run insert, which is this delegation's commitment point: the
	// board can then never show a task free under a run that is already live,
	// because the two facts are one durable write. Event path only — manual
	// delegations claim the task in their handler before they get here, and
	// the fenced insert is the only insert the claim can ride.
	//
	// Distinct from ActorAgentID even though the immediate auto-fire path
	// sets both to the same agent: the actor is who executes this run, the
	// claim is who owns the task. The drain path proves they diverge — it
	// passes the actor but leaves this zero, because the enqueue that queued
	// the firing already stamped the claim, and a user who requeued the task
	// in between must not have it silently re-imposed. Zero value (empty
	// AgentID) means "commit the run alone".
	TaskClaim db.AgentClaimStamp
}

// Delegate kicks off an async agent run for any task type.
// Routes to the appropriate worktree setup based on task source.
func (s *Spawner) Delegate(task domain.Task, opts DelegateOpts) (string, error) {
	orgID := opts.OrgID

	// TFAC-477 runaway-spend fuse: if the org has hit its daily LLM spend cap,
	// refuse the spawn before any blueprint resolution or DB write — so no
	// blueprint_run is minted and the choke point holds for manual + autonomous
	// alike. Fails open on a read error (see checkDailyCostCap). context.Background
	// — Delegate is claims-less, and the cap reads through the admin pool.
	if err := s.checkDailyCostCap(context.Background(), orgID); err != nil {
		return "", err
	}

	// Resolve this run's GitHub client + default model per (org, owner, team).
	// Both modes go through the same seam: the resolver mints an
	// App-installation token in multi or borrows the keychain PAT in local;
	// modelFor reads the task's team default (a multi-team org honors each
	// team's model choice). owner is parsed from the task (empty for Jira
	// runs); task.TeamID is the run's owning team — nullable now, but a task
	// reaching Delegate always has a resolved owner (an unowned task gates
	// auto-fire off, and a manual claim consolidates the owner first), so the
	// nil deref to "" is purely defensive. context.Background — Delegate has
	// no request ctx and the resolve is a bounded token-mint / DB read; the
	// run's own cancellable ctx is created further down for the subprocess.
	teamID := ""
	if task.TeamID != nil {
		teamID = *task.TeamID
	}

	// TFAC-482 per-team fuse: after the org-wide cap above, also refuse the spawn
	// when THIS team has hit its own daily LLM spend cap (EE/governance-gated;
	// dormant when unlicensed, with the org cap as the safety net). Skipped for an
	// unowned task (no team to cap). Same claims-less admin-pool read + fail-open
	// posture as the org cap, and likewise before any blueprint resolution or DB
	// write so a tripped team cap mints no blueprint_run.
	if teamID != "" {
		if err := s.checkTeamDailyCostCap(context.Background(), orgID, teamID); err != nil {
			return "", err
		}
	}

	// The team's model configuration is captured here — its default is stamped
	// onto every enqueued step so the whole blueprint runs on one model, and its
	// enable-set is what each step's pin is held to. The GitHub client is
	// resolved per-claim by the dispatcher (the queue path defers all workspace
	// setup off this call).
	owner, repo := ownerRepoForTask(task)
	_, teamModels, err := s.resolveRunCredentials(context.Background(), orgID, owner, repo, teamID)
	if err != nil {
		return "", err
	}
	// A team whose default its own set no longer includes refuses right here,
	// before any write, naming the model. Asked at the delegation because this
	// is where the default is chosen for the whole firing.
	defaultModel, err := teamModels.RequireDefault()
	if err != nil {
		return "", err
	}

	// Compute trigger type + creator user up front so resolvePrompt
	// can route by them (manual delegations must honor prompts_select
	// RLS; event-triggered ones stay on the admin pool).
	triggerType := opts.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	triggerID := opts.TriggerID

	// Pair creator_user_id with trigger_type per the audit-honesty
	// invariant. Manual runs (the delegate route / drag-to-Agent / factory
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
	// for the trigger type. There is one execution path now: every blueprint —
	// 1-step or multi-step — mints a blueprint_run and runs through the
	// orchestrator (it just loops once for a 1-step). No step-count branch.
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
			delegateLog.Warn("increment usage for blueprint failed", "blueprint", blueprint.ID, "error", e)
		}
	} else if e := s.blueprints.IncrementUsageSystem(bgCtx, orgID, blueprint.ID); e != nil {
		delegateLog.Warn("increment usage for blueprint failed", "blueprint", blueprint.ID, "error", e)
	}

	// One dispatch path: mint the blueprint_run, then run the orchestrator
	// (which loops once for a 1-step blueprint — N=1 is no longer a special
	// path). The blueprint_run is the firing unit and the crash-consistent
	// commit point of a delegation, so it's created synchronously here, before
	// the orchestrator goroutine does any worktree setup — that's where the
	// relocated replay fence lives. worktree_path is filled in after setup.
	blueprintRunID := uuid.New().String()
	// triggering_event_id is event-path-only: it's half the replay-fence key, so
	// a manual run that carried one would participate in the
	// (triggering_event_id, trigger_id) unique index and could either trip a
	// spurious UNIQUE violation or stamp misleading provenance. The DelegateOpts
	// contract says manual never sets it, but pin that here rather than trust the
	// caller — only the event path threads it onto the row.
	triggeringEventID := ""
	if triggerType == "event" {
		triggeringEventID = opts.TriggeringEventID
	}
	// Freeze the resolved step plan onto the blueprint_run at mint: snapshot
	// each step's full prompt content (not just ids) so the
	// dispatcher/reactor/resume sequence off this plan and an in-flight run is
	// insulated from later edits to the blueprint's steps or its step prompts'
	// bodies. Edits are forward-only — they govern the next firing, never this
	// one. resolvePrompt honors the per-trigger RLS routing exactly as
	// resolveBlueprint/resolveBlueprintSteps did, so a manual delegation still
	// can't snapshot a prompt it can't see.
	stepPlan := make([]domain.BlueprintPlanStep, len(steps))
	for i, st := range steps {
		p, err := s.resolvePrompt(orgID, task, st.StepPromptID, triggerType, creatorUserID)
		if err != nil {
			return "", fmt.Errorf("resolve step %d prompt for blueprint %q: %w", st.StepIndex, blueprint.Name, err)
		}
		stepPlan[i] = domain.BlueprintPlanStep{
			StepIndex:    st.StepIndex,
			PromptID:     p.ID,
			PromptName:   p.Name,
			PromptBody:   p.Body,
			Source:       p.Source,
			AllowedTools: p.AllowedTools,
			Model:        p.Model,
			Brief:        st.Brief,
		}
	}
	// Refuse a firing whose models this org cannot authenticate — a provider it
	// never connected. Here, because this is the last point before the commit:
	// the plan's pins are resolved and nothing durable has been written, so the
	// refusal leaves no blueprint_run to reap and nothing to retry.
	if err := s.checkModelProviders(bgCtx, orgID, blueprintModels(model, stepPlan)); err != nil {
		return "", err
	}
	// Step 0's model, decided before the commit for the same reason: a pin the
	// team's enable-set excludes fails the firing outright, and a firing that
	// never happened should leave no blueprint_run behind. Later steps' pins are
	// held to the set at their own advance instead, where the set is read fresh
	// — narrowing one mid-flight is the case no check here can see.
	stepModel, err := stepModelOrInherit(stepPlan[0].Model, model, teamModels.Enabled())
	if err != nil {
		return "", err
	}

	brRow := domain.BlueprintRun{
		ID:                blueprintRunID,
		BlueprintID:       blueprint.ID,
		TaskID:            task.ID,
		TriggerType:       domain.BlueprintTriggerType(triggerType),
		TriggerID:         triggerID,
		TriggeringEventID: triggeringEventID,
		// Freeze the executing bot at mint. Every step run inherits this onto
		// conversations.actor_agent_id, so execution attribution is resolved once here
		// rather than re-derived from the task claim per step (which is empty at
		// step 0 on the event path and cleared by a mid-blueprint takeover).
		ActorAgentID: opts.ActorAgentID,
		Status:       domain.BlueprintRunStatusRunning,
		StepPlan:     stepPlan,
	}
	// Event-triggered firings go through the fenced insert: a replayed
	// (triggering_event_id, trigger_id) under the at-least-once router queue
	// returns ErrAlreadyFired, so the router skips cleanly instead of minting a
	// duplicate blueprint_run — closing the latent gap that multi-step chains
	// were never replay-fenced. Manual delegations write under the user's
	// synthetic claims so blueprint_runs_insert RLS sees the creator; they carry
	// no triggering_event_id and never fence (multiple manual runs of one task
	// stay allowed). Everything before this insert (blueprint/step resolution,
	// usage bumps) is cheap and idempotent enough to re-run on a fenced replay.
	if triggerType == "manual" {
		if err := s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			_, e := ts.Blueprints.CreateRun(bgCtx, orgID, brRow)
			return e
		}); err != nil {
			return "", fmt.Errorf("create blueprint run: %w", err)
		}
	} else {
		// opts.TaskClaim rides this insert's transaction: the fenced insert is
		// the commitment point, so the task's claim commits with the run or
		// not at all. A claim *refusal* (a user won the race, the bot already
		// owns it, the task went terminal) leaves the run committed — the
		// claim race has a winner either way and the run must still stand.
		inserted, _, err := s.blueprints.CreateRunIfNotFiredSystem(bgCtx, orgID, brRow, opts.TaskClaim)
		if err != nil {
			if errors.Is(err, db.ErrTaskBusyActiveAutoRun) {
				return "", ErrTaskBusy
			}
			return "", fmt.Errorf("create blueprint run: %w", err)
		}
		if !inserted {
			return "", ErrAlreadyFired
		}
	}

	// Enqueue the first step. The blueprint advances entirely through the run
	// queue from here: the dispatcher claims this step, builds the shared
	// workspace, runs the agent, and the reactor enqueues each next step (or
	// finalizes). No in-process for-loop holds the sequencing — blueprint_runs
	// does (current_step_index), so a crash mid-flight is recoverable. The
	// blueprint_run was just committed (the replay fence point); if the enqueue
	// fails, mark it failed so it doesn't strand non-terminal. A hard death in
	// the same window can't run that write, and the parent it leaves behind has
	// no child for any conversation-joining recovery arm to find it by — so the
	// childless-parent shape is owned outside this path, by the boot reconcile
	// and the leader reaper (domain.BlueprintAbortOrphanedAtMint).
	if err := s.enqueueBlueprintStep(bgCtx, orgID, blueprintRunID, task, steps[0], stepModel, triggerType, triggerID, creatorUserID, brRow.ActorAgentID); err != nil {
		if _, mErr := s.blueprints.MarkRunStatusSystem(bgCtx, orgID, blueprintRunID, domain.BlueprintRunStatusFailed, "enqueue first step: "+err.Error(), nil); mErr != nil {
			delegateLog.Warn("mark blueprint_run failed after enqueue error", "blueprint_run", blueprintRunID, "error", mErr)
		}
		return "", fmt.Errorf("enqueue first step: %w", err)
	}

	verb := "Blueprint started"
	if triggerType == "event" {
		verb = "Auto-fired blueprint"
	}
	toast.Info(s.wsHub, orgID, fmt.Sprintf("%s: %s (%s)",
		verb, truncateToastMsg(blueprint.Name, 60), shortConversationID(blueprintRunID)))

	// The blueprint_run is live → place the task in_progress immediately.
	s.recomputeTaskBoardColumn(orgID, task.ID)
	s.wakeDispatcher()
	return blueprintRunID, nil
}

// splitGitHubEntitySourceID splits a GitHub entity's source id ("owner/repo#42"
// for a PR/issue) into its parts, cutting the "#N" suffix before the owner/repo
// split. That ordering is the reason this is one helper rather than a split
// spelled out per call site: splitting the raw id leaves the repository named
// "repo#42", which no repository row and no remote resolves, and the resulting
// failure surfaces far downstream of the parse that caused it. owner and repo
// are empty when what precedes the suffix carries no "/" (a Jira key, say);
// prNumber is 0 when the suffix is absent or unparseable.
func splitGitHubEntitySourceID(sourceID string) (owner, repo string, prNumber int) {
	repoStr := sourceID
	if idx := strings.LastIndex(sourceID, "#"); idx >= 0 {
		repoStr = sourceID[:idx]
		fmt.Sscanf(sourceID[idx+1:], "%d", &prNumber)
	}
	owner, repo = parseOwnerRepo(repoStr)
	return owner, repo, prNumber
}

// ownerRepoForTask parses "owner/repo" out of a GitHub task's EntitySourceID
// ("owner/repo#N" for a PR/issue task); ("", "") for Jira (and any
// non-github source), which has no single owner/repo — the live resolver
// then resolves the org's sole App installation, falling through to PAT
// when ambiguous. The source gate is load-bearing beyond Jira: a Slack
// entity's source id ("<channel>/<ts>") splits on "/" perfectly well and
// would otherwise mint a nonsense repo pair. Every call site needs both
// halves (the repo disambiguates a bundle-backed credential lookup, since
// unlike the live resolver's account-wide App installation token, a bundle's
// RepoTokens are scoped per-repo), so this returns the pair rather than
// splitting into separate owner-only/repo-only helpers nobody calls
// independently.
func ownerRepoForTask(task domain.Task) (owner, repo string) {
	if task.EntitySource != "github" {
		return "", ""
	}
	owner, repo, _ = splitGitHubEntitySourceID(task.EntitySourceID)
	return owner, repo
}

// cloneHostBase returns the scheme://host of an HTTPS clone URL — the insteadOf
// upstream the executor's git-proxy clone routing rewrites, matching the sandbox
// agent's own git-proxy pairs (which key off the org's git host base). An
// unparseable or schemeless URL (e.g. an SSH remote, which the executor doesn't
// use) returns as-is; the insteadOf then simply won't match and the clone fails
// loudly rather than leaking a token.
func cloneHostBase(cloneURL string) string {
	u, err := url.Parse(cloneURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return cloneURL
	}
	return u.Scheme + "://" + u.Host
}

// toolsReferenceFor composes a run's <tools> section from what the ORG can
// reach, unioned with the source the run is about (plus GitHub, which every
// run keeps — repos are acquired on demand).
//
// The union is what makes this safe to change: a run never loses the verbs for
// its own entity source, so nothing regresses, and it gains the verbs for every
// other source the org actually has. That is the fix for a real hole — a run
// triggered by a GitHub event was handed the gh verbs alone, so an agent
// working a pull request that referenced a ticket had no way to read, comment
// on, or transition it.
//
// Two resolve paths, by where the answer can actually be computed:
//
//   - The claim's stamped tools manifest (claim_credentials.include_tools) —
//     the brain's own availability answer, written beside the sealed bundle on
//     the same pass that provisioned this claim. This is the ONLY complete
//     answer an executor can get: its secret store is disabled, so it cannot
//     probe GitHub/Jira availability, and an event-triggered conversation has
//     no user identity to open a claims transaction as. The row is guaranteed
//     present here on the executor path — the sidecar bring-up that precedes
//     workspace setup blocks until the bundle lands.
//   - The claims-bound live resolve, for the single-process local mode where
//     no bundles exist (the manifest read answers ErrNotApplicableInLocal)
//     and the live secret store makes the probes answerable in-process.
//
// A failed resolve on both paths degrades to the superset every run used to
// get (its own source plus GitHub), never to nothing. The tools block is
// documentation; the gate that actually stops a verb is the credential funnel
// it resolves through, which refuses with an error naming the reason. An agent
// told about a verb that then refuses knows what happened; an agent told about
// no verb improvises.
//
// creatorUserID/conversationID are the caller's already-loaded
// conv.CreatorUserID / conv.ID, not ids to look up: every caller sits inside
// buildStepConfig, which already holds the full conversation row for other
// reasons, so a fetch here to recover fields off the same row would be a
// synchronous round trip added to the serial run-setup path for nothing it
// doesn't already have.
func (s *Spawner) toolsReferenceFor(ctx context.Context, orgID, creatorUserID, conversationID, ownSource string) string {
	base := []string{eventsource.KindGitHub, ownSource}

	if s.claimCredentials != nil {
		b, found, err := s.claimCredentials.Get(ctx, orgID, conversationID)
		switch {
		case err != nil && !errors.Is(err, db.ErrNotApplicableInLocal):
			delegateLog.Warn("read the claim's stamped tools manifest failed; falling back to the live availability resolve",
				"org", orgID, "conversation", conversationID, "error", err)
		case err == nil && found && b.IncludeTools != nil:
			// nil means the brain stamped no answer (its resolve failed, or an
			// older brain wrote the row) — fall through to the live resolve.
			// An empty non-nil manifest is a real answer and stops here: the
			// base union still documents the run's own sources.
			return agentprompt.ToolsReferenceForSources(append(b.IncludeTools, base...))
		}
	}

	stores, ok := s.getStores()
	if !ok || stores.Tx == nil {
		return agentprompt.ToolsReferenceForSources(base)
	}
	var kinds []string
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, creatorUserID, func(tx db.TxStores) error {
		var e error
		kinds, e = eventsource.AvailableKinds(ctx, tx, orgID)
		return e
	}); err != nil {
		delegateLog.Warn("resolve event-source availability for tools reference failed; using the run's own sources",
			"org", orgID, "error", err)
		return agentprompt.ToolsReferenceForSources(base)
	}
	return agentprompt.ToolsReferenceForSources(append(kinds, base...))
}

// setupGitHub prepares a worktree for a GitHub PR task.
//
// On the executor path (sidecar non-nil) the GetPR client and the host-side
// clone both route through the run's credential sidecar — the client against
// the sidecar's GitHub-REST proxy, the clone through its git proxy — so the
// orchestrator holds no GitHub credential for either. Local reads the PR through
// the resolver-built client and routes the clone through its loopback proxy.
func (s *Spawner) setupGitHub(ctx context.Context, orgID, conversationID, claimID, rootKey, creatorUserID string, task domain.Task, ghClient *ghclient.Client, sidecar *runSidecar, localGit *localGitChannel) (runConfig, error) {
	ghClient = prReadClient(ghClient, sidecar)
	if ghClient == nil {
		return runConfig{}, fmt.Errorf("GitHub credentials not configured")
	}

	owner, repo, prNumber := splitGitHubEntitySourceID(task.EntitySourceID)
	if owner == "" || repo == "" {
		return runConfig{}, fmt.Errorf("cannot parse owner/repo from entity source ID: %q", task.EntitySourceID)
	}
	if prNumber == 0 {
		return runConfig{}, fmt.Errorf("invalid PR number from task.EntitySourceID: %q", task.EntitySourceID)
	}

	s.updatePhase(ctx, orgID, conversationID, claimID, domain.ClaimPhaseFetching)
	// Named for the phase it reports, so the trace and the run station agree
	// on what the engagement was doing. On the executor path this GET is not
	// a direct call to GitHub — it crosses the run's sidecar REST proxy — so
	// the span covers a hop the outbound-client instrumentation cannot see.
	fetchCtx, fetchSpan := tracer.Start(ctx, "engagement.fetch")
	pr, err := ghClient.GetPR(fetchCtx, owner, repo, prNumber, false)
	recordSpanError(fetchSpan, err)
	fetchSpan.End()
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
	if localGit == nil && s.useSSHCloneProtocol(ctx, orgID, conversationID) {
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

	s.updatePhase(ctx, orgID, conversationID, claimID, domain.ClaimPhaseCloning)
	// Resolve the host-side clone credential. In both modes the clone routes
	// through a git proxy (CloneAuthViaGitProxy): git is
	// rewritten from the upstream host to the proxy URL and presents only the
	// per-run placeholder, so the real credential stays in the proxy. The
	// insteadOf upstream is the clone URL's scheme+host (matching the sandbox
	// agent's own git-proxy pairs, which use the org git base). Local uses its
	// loopback proxy; multi uses the credential sidecar's proxy.
	var cloneAuth worktree.CloneAuth
	if sidecar != nil {
		cloneAuth = worktree.CloneAuthViaGitProxy(sidecar.res.GitProxyURL, cloneHostBase(upstreamCloneURL), sidecar.res.GitProxyToken)
	} else if localGit != nil {
		cloneAuth = localGit.cloneAuth(upstreamCloneURL)
	}
	// rootKey (the blueprint run id), not this step's conversation id, keys the
	// worktree dir + its per-run push config — the PR worktree IS the shared
	// run-root, and a cold rehydrate rebuilds it under the same key.
	// CleanupPRConfig reclaims via filepath.Base(wtPath), so it follows this
	// key automatically.
	cloneCtx, cloneSpan := tracer.Start(ctx, "engagement.clone")
	wtPath, err := worktree.CreateForPR(cloneCtx, owner, repo, upstreamCloneURL, headCloneURL, pr.HeadRef, prNumber, rootKey,
		worktree.WithCloneAuth(cloneAuth),
		// Refresh origin/<base> at materialization so `pr diff` frames against a
		// current base instead of a clone-time-frozen ref (TFAC-505).
		worktree.WithBaseBranch(pr.BaseRef))
	recordSpanError(cloneSpan, err)
	cloneSpan.End()
	if err != nil {
		return runConfig{}, fmt.Errorf("failed to create worktree: %w", err)
	}

	// Fence refusals are excluded here and at the two sibling setups below:
	// setWorktreePath already logged the ownership loss, and this line's
	// subject is a write that failed on a row this engagement still owns.
	if err := s.setWorktreePath(context.WithoutCancel(ctx), orgID, conversationID, claimID, wtPath); err != nil && !errors.Is(err, db.ErrClaimReleased) {
		delegateLog.Warn("update worktree path for conversation failed", "conversation", conversationID, "error", err)
	}

	// Record the eager worktree in conversation_worktrees so the least-privilege gates
	// (git proxy + exec gh) treat the task repo uniformly with workspace-add'd
	// repos: a run may touch a repo only if its team tracks it AND it appears in
	// this ledger. ref = pr-<N> is the materialization selector (the push gate
	// reads the worktree's live current branch, not this row). Durable, so a
	// resume re-derives authority with no head-ref threading, and the row is
	// what the multi-PR review anchor (add-review-comment) resolves the PR's
	// worktree HEAD through. Log-and-continue like SetWorktreePathSystem above:
	// a failure degrades to denied pushes (a clear 403), never a crash.
	if s.conversationWorktrees != nil {
		if _, _, werr := s.conversationWorktrees.InsertSystem(context.Background(), orgID, domain.ConversationWorktree{
			ConversationID: conversationID,
			RepoID:         owner + "/" + repo,
			Path:           wtPath,
			Ref:            worktree.PRRefSlug(prNumber),
		}); werr != nil {
			delegateLog.Warn("record eager worktree in conversation_worktrees failed; pushes to this repo will be denied until retried", "conversation", conversationID, "repo", owner+"/"+repo, "error", werr)
		}
	}

	return runConfig{
		orgID:      orgID,
		scope:      fmt.Sprintf("Repository: %s/%s\nPR: #%d\nBranch: %s", owner, repo, prNumber, pr.HeadRef),
		toolsRef:   s.toolsReferenceFor(ctx, orgID, creatorUserID, conversationID, eventsource.KindGitHub),
		wtPath:     wtPath,
		hasWT:      true,
		runRoot:    wtPath, // GitHub PR runs: worktree IS the run-root, so $TRIAGE_FACTORY_CONVERSATION_ROOT resolves to the worktree
		owner:      owner,
		repo:       repo,
		prNumber:   prNumber,
		prSkeleton: renderPRSkeleton(ctx, ghClient, owner, repo, prNumber),
	}, nil
}

// prReadClient resolves the client a PR-scoped read should use. On the
// executor path every GitHub read routes through the run's credential
// sidecar (the REST proxy), so the orchestrator holds no token; elsewhere
// the resolver-built client is already the right one.
func prReadClient(ghClient *ghclient.Client, sidecar *runSidecar) *ghclient.Client {
	if sidecar != nil {
		return ghclient.NewProxyClient(sidecar.res.GitHubAPIURL, sidecar.res.GitHubAPIToken)
	}
	return ghClient
}

// renderPRSkeleton fetches and renders the PR's history for the run's static
// context. Two GETs through whichever client the caller resolved, so it
// inherits the run's credential path (the sidecar's REST proxy on an
// executor, the org's App-or-PAT client elsewhere) with nothing new to
// provision.
//
// Re-fetched per step rather than carried on the blueprint run: a later
// step's history should include what its predecessors just pushed, which a
// value frozen at the first step's setup could not show.
//
// Best-effort by construction: a failure returns "" and the run proceeds
// with the context it has always had. History is an enrichment, and a
// GitHub hiccup fetching it must never be the reason a delegation dies.
func renderPRSkeleton(ctx context.Context, ghClient *ghclient.Client, owner, repo string, prNumber int) string {
	if ghClient == nil || owner == "" || repo == "" || prNumber == 0 {
		return ""
	}
	sk, err := prskeleton.FetchPR(ctx, ghClient, owner, repo, prNumber)
	if err != nil {
		delegateLog.Warn("fetch PR history skeleton failed; the conversation continues without it",
			"repo", owner+"/"+repo, "pr", prNumber, "error", err)
		return ""
	}
	return prskeleton.Render(*sk, prskeleton.Options{Now: time.Now()})
}

// setupJira prepares the run-root for a Jira delegation. No repo is
// pre-cloned — the agent decides which repo(s) it needs after reading
// the ticket and materializes them via `triagefactory exec workspace
// add <owner/repo>`. Each materialization lands a worktree at
// {runRoot}/{owner}/{repo}/ and inserts a row into conversation_worktrees.
//
// The agent's initial cwd is the run-root: a throwaway dir holding
// only ./_tfac/ (whose entity-memory is materializePriorMemories' rendering, or
// under a jail the symlink standing in for its read-only mount). Both gh and
// jira tool surfaces are exposed since the agent
// will need both to implement and ship a PR.
//
// conversations.worktree_path is set to the run-root. The resume path reads this
// field as the cwd to resume the session in (`claude --resume` keys
// session storage by cwd-encoded ~/.claude/projects/<encoded>, and we
// passed cwd=runRoot to the original agentproc.Run). Even though Jira
// runs don't have a single "the worktree" the way GitHub PR runs do,
// the run-root IS the agent's session cwd, which is the load-bearing
// invariant for resume.
func (s *Spawner) setupJira(ctx context.Context, orgID, conversationID, claimID, rootKey, creatorUserID string, task domain.Task, ghClient *ghclient.Client) (runConfig, error) {
	// The run-root is a blueprint-run resource — shared across every step and
	// rebuilt under the same key on a cold rehydrate — so it is keyed by rootKey
	// (the blueprint run id), not this step's conversation id. conversationID
	// still stamps the per-conversation worktree_path record below.
	runRoot, err := worktree.MakeRunRoot(rootKey)
	if err != nil {
		return runConfig{}, fmt.Errorf("create run root: %w", err)
	}
	if err := s.setWorktreePath(context.Background(), orgID, conversationID, claimID, runRoot); err != nil && !errors.Is(err, db.ErrClaimReleased) {
		delegateLog.Warn("set worktree_path for Jira conversation failed; resume will reject this conversation", "conversation", conversationID, "error", err)
	}

	return runConfig{
		orgID:    orgID,
		scope:    fmt.Sprintf("Jira issue: %s", task.EntitySourceID),
		toolsRef: s.toolsReferenceFor(ctx, orgID, creatorUserID, conversationID, eventsource.KindJira),
		wtPath:   runRoot,
		hasWT:    false,
		runRoot:  runRoot,
		// owner/repo intentionally empty: the agent picks per-ticket via `workspace add`
	}, nil
}

// setupSlack prepares the run-root for a Slack-thread delegation
// (TFAC-591). Mirrors setupJira: no repo is pre-cloned, the agent acquires
// repo(s) on demand via `triagefactory exec workspace add` (TFAC-498), and
// the run-root is the agent's session cwd — the same resume load-bearing
// invariant documented on setupJira above.
//
// One difference from Jira:
//   - The tools reference layers in an ee-registered Slack verb doc (if
//     any registered via agentprompt.RegisterToolsReference) on top of the GH
//     template — GH tools are included because the agent acquires repos on
//     demand and then needs the gh verbs, the same reasoning as the Jira
//     arm's GH+Jira composition. No registered reference is not an error:
//     the conversation still works with base tools.
func (s *Spawner) setupSlack(ctx context.Context, orgID, conversationID, claimID, rootKey, creatorUserID string, task domain.Task, ghClient *ghclient.Client) (runConfig, error) {
	// Keyed by rootKey (the blueprint run id), not this step's conversation id
	// — the run-root is blueprint-scoped and cold-rehydrates under the same key.
	// See setupJira.
	runRoot, err := worktree.MakeRunRoot(rootKey)
	if err != nil {
		return runConfig{}, fmt.Errorf("create run root: %w", err)
	}
	if err := s.setWorktreePath(context.Background(), orgID, conversationID, claimID, runRoot); err != nil && !errors.Is(err, db.ErrClaimReleased) {
		delegateLog.Warn("set worktree_path for Slack conversation failed; resume will reject this conversation", "conversation", conversationID, "error", err)
	}

	toolsRef := s.toolsReferenceFor(ctx, orgID, creatorUserID, conversationID, "slack")

	return runConfig{
		orgID:    orgID,
		scope:    fmt.Sprintf("Slack thread: %s", task.EntitySourceID),
		toolsRef: toolsRef,
		wtPath:   runRoot,
		hasWT:    false,
		runRoot:  runRoot,
		// owner/repo intentionally empty: the agent picks repos per-thread
		// via `workspace add`.
	}, nil
}
