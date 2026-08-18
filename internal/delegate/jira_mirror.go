// TFAC-300: board → Jira lifecycle mirror (the system/bot lane).
//
// When a delegated agent works a Jira-backed task the TF board moves the card
// through in_progress → in_review → done, but none of that reaches the real
// Jira ticket — a watcher on Jira (not TF) sees the ticket keep its original
// assignee and status the whole time. This file mirrors the board moves back
// onto the ticket under the org's system/bot credential (jira.Resolver.ForSystem,
// TFAC-34), so the bot-side lifecycle is visible in Jira exactly as the
// user-claim path already mirrors it for human-claimed tasks (the claim guard
// in server.handleSwipe).
//
// Two chokepoints drive it, and both move the ticket into the InProgress bucket
// — no board/task hook writes Done anymore (runJiraMirror still has a done mode,
// but it is reserved for the forthcoming merge-driven Done mirror, not these):
//   - recomputeTaskBoardColumn → mirrorJiraInProgress (board in_progress /
//     in_review, which both collapse to InProgress — there is no in-review
//     canonical, and a bot awaiting input is still "in progress" to a watcher).
//   - terminateBlueprint's completed branch → mirrorJiraInProgressForTask: a
//     finished run means the agent opened its PR and the work is awaiting human
//     review + merge, which is still "in progress" to a watcher, NOT done. A
//     ticket only reaches Done when its PR merges — a separate, entity-driven
//     mirror (forthcoming), never a board/task-completion move. PR-opened ≠
//     ticket-done: that conflated the task lifecycle (TF's board "done" column =
//     work submitted) with the entity lifecycle (the change shipped).
//
// Both points only ever see bot-owned tasks: recomputeTaskBoardColumn
// early-returns unless claimed_by_agent_id is set, and the completion path
// re-checks it. So every write here is bot-attributed by construction — there is
// no "bot or human?" branch at the write site. A user takeover flips the claim
// and the bot simply stops mirroring; the terminal write then belongs to the
// user path.
//
// The mirror is idempotent and membership-based: it reads the ticket's current
// assignee + status (GetClaimState) and skips any step the ticket already
// satisfies. That is what lets it coexist safely with the agent's own
// cmd/exec/jira verbs — whoever writes first wins, the other no-ops.
//
// runJiraMirror still carries a done mode (transition into the Done bucket); it
// is the reusable mechanism the forthcoming merge-driven Done mirror will call,
// not anything triggered from the board. So two safeguards still matter: a Done
// write can come from a human (or that merge mirror), and a slow in-progress
// mirror must never drag such a ticket back. Per-issue serialization
// (jiraMirrorLocks) keeps in-process mirrors for one ticket from interleaving,
// and a forward-only in-progress rule (never transition a ticket already in the
// Done bucket) holds even against an out-of-band human move. See runJiraMirror.

package delegate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
)

// jiraMirrorTimeout bounds one mirror operation (resolve + GetClaimState +
// assign + transition) end to end. The Jira client already caps each HTTP call
// at 15s; this caps the whole multi-call sequence so a slow ticket can't pin
// the per-issue mirror lock for long.
const jiraMirrorTimeout = 30 * time.Second

// lookupJiraRuleForTaskSystem resolves the per-team Jira status rule governing
// the task's source project, under the admin pool (ListForTeamSystem) — the
// spawner runs in system context, not RLS. The system-door twin of
// server.lookupJiraRuleForTask. Returns nil when the task is not Jira-backed,
// carries no TeamID, or the team has no rule for the ticket's project.
func lookupJiraRuleForTaskSystem(ctx context.Context, jiraRules db.JiraStatusRulesStore, task *domain.Task) *domain.JiraProjectStatusRules {
	if task == nil || task.EntitySource != "jira" || task.TeamID == nil || *task.TeamID == "" {
		return nil
	}
	rules, err := jiraRules.ListForTeamSystem(ctx, *task.TeamID)
	if err != nil {
		jiraLog.Warn("mirror: list status rules for team failed", "team", *task.TeamID, "error", err)
		return nil
	}
	return domain.RuleForProject(rules, projectFromJiraKey(task.EntitySourceID))
}

// jiraMirrorRule resolves the status rule for the task's project, returning nil
// when the mirror is disabled (no resolver or rules store wired — tests, or
// local without Jira) or the task isn't a rule-backed Jira task. Callers branch
// on the single nil check.
func (s *Spawner) jiraMirrorRule(task *domain.Task) *domain.JiraProjectStatusRules {
	if s.jiraRules == nil || s.getJiraResolver() == nil {
		return nil
	}
	return lookupJiraRuleForTaskSystem(context.Background(), s.jiraRules, task)
}

// mirrorJiraInProgress mirrors a bot-owned task's board move onto its Jira
// ticket: assign the service account and transition into the InProgress bucket,
// under the org's system/bot credential. Both board in_progress and in_review
// land here. Detached so Jira latency never blocks the board WS update; a no-op
// for non-Jira tasks or when no rule resolves (skip + log, like the user path's
// "no in_progress rule configured" branch).
func (s *Spawner) mirrorJiraInProgress(orgID string, task *domain.Task) {
	rule := s.jiraMirrorRule(task)
	if rule == nil {
		return
	}
	if rule.InProgressCanonical == "" {
		// Never guess a status. The table's CHECK keeps this non-empty for a
		// persisted row, so this is defensive parity with the user path.
		jiraLog.Warn("mirror: no in_progress rule for project, skipping", "issue", task.EntitySourceID)
		return
	}
	// teamID scopes the audit row to the bot-owned task's team (TFAC-483). Empty
	// for a team-less task → the action still lands in the org governance feed.
	teamID := ""
	if task.TeamID != nil {
		teamID = *task.TeamID
	}
	go s.runJiraMirror(orgID, task.EntitySourceID, teamID, *rule, false)
}

// mirrorJiraInProgressForTask loads the task and re-asserts the InProgress
// bucket on its Jira ticket when a delegated run finishes cleanly — but only
// while the bot still owns it. A finished run means the agent opened its PR and
// the work is awaiting human review + merge: "in progress" to a Jira watcher,
// NOT done. The ticket only reaches Done when its PR merges (a separate,
// entity-driven mirror — forthcoming), never on run completion. A user takeover
// mid-run flips claimed_by_agent_id to the user, after which the terminal Jira
// write belongs to the user's advance/swipe path, so a no-longer-bot-owned task
// is skipped. Called from terminateBlueprint's completed branch; a
// failed/aborted/cancelled run never reaches it. The in-progress mirror is
// idempotent, so in the common case (the dispatch-time mirror already moved the
// ticket) this is a single GetClaimState read and no write — and it self-heals a
// ticket left in To Do by a transient dispatch-time mirror failure.
func (s *Spawner) mirrorJiraInProgressForTask(ctx context.Context, orgID, taskID string) {
	if s.tasks == nil {
		return
	}
	task, err := s.tasks.GetSystem(ctx, orgID, taskID)
	if err != nil || task == nil {
		return
	}
	if task.ClaimedByAgentID == "" {
		// User takeover (or never bot-owned) — the bot does not mirror.
		return
	}
	s.mirrorJiraInProgress(orgID, task)
}

// runJiraMirror is the detached worker the in-progress hooks share, and also the
// done worker the forthcoming merge-driven Done mirror calls. It resolves the
// org's system/bot Jira client fresh (creds hot-swap on config change, so a
// client is never cached) and then assigns the service account (in-progress
// only) and transitions the ticket into the target bucket, skipping any step the
// ticket already satisfies.
//
// Two safeguards keep a slow in-progress mirror from clobbering a Done write for
// the same ticket (Done now comes from a human or the merge mirror, not the
// board):
//   - Per-issue serialization (jiraMirrorLocks): in-process mirrors for one
//     ticket can't interleave or reorder their writes.
//   - Forward-only in-progress: under that lock it re-reads state and, if the
//     ticket is already in the Done bucket, makes no in-progress move — so a
//     terminal Done is never dragged back to In Progress, whichever goroutine
//     won the lock and even if the Done move happened out of band.
//
// The whole sequence is bounded by jiraMirrorTimeout so a slow ticket releases
// the lock rather than pinning it.
func (s *Spawner) runJiraMirror(orgID, issueKey, teamID string, rule domain.JiraProjectStatusRules, done bool) {
	resolver := s.getJiraResolver()
	if resolver == nil {
		return
	}

	// Serialize before any Jira call so the read→write decision for this ticket
	// is made under mutual exclusion with the other phase's mirror.
	unlock := s.jiraMirrorLocks.lock(orgID + "\x00" + issueKey)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), jiraMirrorTimeout)
	defer cancel()

	client, err := resolver.ForSystem(ctx, orgID)
	if err != nil {
		if shouldLogForSystemErr(err) {
			jiraLog.Warn("mirror: resolve system client failed", "org", orgID, "error", err)
		}
		return
	}

	state := client.GetClaimState(ctx, issueKey)

	if done {
		// Idempotency skip when the ticket is already in the Done bucket. Unlike
		// the in-progress path below, a nil state (transient read failure) falls
		// through to the transition rather than skipping: reaching the done mirror
		// means the work is complete, so moving to Done is correct either way — and
		// a redundant Done→Done attempt just errors harmlessly. Moving to Done is
		// forward, never the backward regression the in-progress skip guards.
		if state != nil && rule.DoneContains(state.StatusName) {
			jiraLog.Debug("mirror: already in done bucket, skipping", "issue", issueKey, "status", state.StatusName)
			return
		}
		if err := client.TransitionTo(ctx, issueKey, rule.DoneCanonical); err != nil {
			jiraLog.Warn("mirror: transition to done failed", "issue", issueKey, "target", rule.DoneCanonical, "error", err)
			return
		}
		// from is the status read before the move (nil state → unknown/"").
		from := ""
		if state != nil {
			from = state.StatusName
		}
		s.recordMirrorAction(ctx, orgID, issueKey, teamID, domain.ActionIssueTransitioned, from, rule.DoneCanonical)
		return
	}

	// In-progress phase. We must read the ticket's current status to honor the
	// forward-only invariant; GetClaimState returns nil on ANY error, so a nil
	// here is "unknown" — skip rather than risk transitioning a ticket a
	// concurrent done mirror already moved to Done back to In Progress. Blindly
	// proceeding (state == nil → assign + transition to In Progress) is exactly
	// the backward move the per-issue lock exists to prevent. Self-heals: every
	// board column transition re-fires the mirror, and the failed read logs.
	if state == nil {
		jiraLog.Warn("mirror: could not read claim state; skipping in-progress mirror", "issue", issueKey)
		return
	}
	// Forward-only: a concurrent done mirror may have already advanced the ticket
	// into the Done bucket — never drag a terminal ticket back to In Progress.
	if rule.DoneContains(state.StatusName) {
		jiraLog.Debug("mirror: already advanced to done, skipping in-progress mirror", "issue", issueKey, "status", state.StatusName)
		return
	}
	if !state.AssignedToSelf {
		if err := client.AssignToSelf(ctx, issueKey); err != nil {
			// Skip the transition too: assign + transition move together (same as
			// the user-path claim guard), so a failed assign leaves the ticket
			// untouched — To Do + unassigned — rather than "In Progress but
			// unassigned". Self-heals on the next board transition's mirror pass.
			jiraLog.Warn("mirror: assign to service account failed", "issue", issueKey, "error", err)
			return
		}
		s.recordMirrorAction(ctx, orgID, issueKey, teamID, domain.ActionIssueAssigned, "", "")
	}
	if !rule.InProgressContains(state.StatusName) {
		if err := client.TransitionTo(ctx, issueKey, rule.InProgressCanonical); err != nil {
			jiraLog.Warn("mirror: transition to in-progress failed", "issue", issueKey, "target", rule.InProgressCanonical, "error", err)
			return
		}
		s.recordMirrorAction(ctx, orgID, issueKey, teamID, domain.ActionIssueTransitioned, state.StatusName, rule.InProgressCanonical)
	}
}

// recordMirrorAction appends one external_actions row for a board→Jira mirror
// write (TFAC-483): a system/bot action under the org Jira service-account
// credential, with no human actor (actor_user_id NULL). team_id scopes it to the
// bot-owned task's team. The detached mirror holds no conversation handle, so conversation_id is
// left NULL, and it doesn't resolve the issue's browse URL (the issue key is the
// target). Admin pool (RecordSystem — no JWT claims). Best-effort: a recording
// failure is logged and swallowed so it never unwinds the Jira move it observed,
// and nil-safe for a partial test Stores.
func (s *Spawner) recordMirrorAction(ctx context.Context, orgID, issueKey, teamID, action, from, to string) {
	if s.externalActions == nil {
		return
	}
	err := s.externalActions.RecordSystem(ctx, orgID, domain.ExternalAction{
		TeamID:     teamID,
		Provider:   domain.ArtifactProviderJira,
		Action:     action,
		Target:     issueKey,
		ExternalID: issueKey,
		URL:        s.jiraBrowseURL(ctx, orgID, issueKey),
		FromState:  from,
		ToState:    to,
		Credential: domain.CredentialJiraOrg,
	})
	if err != nil {
		jiraLog.Warn("mirror: external-action recording failed (Jira move already applied)",
			"issue", issueKey, "action", action, "error", err)
	}
}

// jiraBrowseURL builds the issue's human-facing {site}/browse/<KEY> link from the
// org's configured Jira site URL, or "" when it's unset/unreadable (URL is an
// optional audit field). Best-effort, admin-pool settings read — same source the
// agent's exec-side jiraBrowseURL + the poller stamp entity URLs from — so the
// mirror's audit rows link to the ticket just like the bot's own do.
func (s *Spawner) jiraBrowseURL(ctx context.Context, orgID, issueKey string) string {
	if s.orgs == nil {
		return ""
	}
	set, err := s.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil || set.JiraBaseURL == "" {
		return ""
	}
	return strings.TrimRight(set.JiraBaseURL, "/") + "/browse/" + issueKey
}

// shouldLogForSystemErr reports whether a resolver.ForSystem error is worth
// logging. ErrNoJiraSystemCredential is an expected, normal state (a GitHub-only
// org, or creds removed mid-flight), so the mirror no-ops silently rather than
// logging on every mirrored board transition; any other error is a real failure
// (e.g. a vault outage) and is logged. errors.Is unwraps, so the wrapped
// sentinel ForSystem actually returns ("%w: org=...") is still treated as silent.
func shouldLogForSystemErr(err error) bool {
	return err != nil && !errors.Is(err, jira.ErrNoJiraSystemCredential)
}

// keyedMutex hands out a mutex per string key, so callers can serialize work on
// one key without serializing across unrelated keys. Entries are refcounted and
// dropped when the last holder releases, so the map doesn't grow unboundedly as
// Jira issue keys come and go over a long-running process. The zero value is
// ready to use.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*refMutex
}

type refMutex struct {
	mu   sync.Mutex
	refs int
}

// lock acquires the per-key mutex and returns its unlock func, which the caller
// must invoke exactly once (defer is the usual shape).
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*refMutex)
	}
	rm := k.locks[key]
	if rm == nil {
		rm = &refMutex{}
		k.locks[key] = rm
	}
	rm.refs++
	k.mu.Unlock()

	rm.mu.Lock()
	return func() {
		rm.mu.Unlock()
		k.mu.Lock()
		rm.refs--
		if rm.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
