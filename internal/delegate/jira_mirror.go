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
// Two chokepoints drive it:
//   - recomputeTaskBoardColumn → mirrorJiraInProgress (in_progress / in_review,
//     which both collapse to the InProgress bucket — there is no in-review
//     canonical, and a bot awaiting input is still "in progress" to a watcher).
//   - terminateBlueprint's completed branch → mirrorJiraDone (done).
//
// Both points only ever see bot-owned tasks: recomputeTaskBoardColumn
// early-returns unless claimed_by_agent_id is set, and the done path re-checks
// it. So every write here is bot-attributed by construction — there is no
// "bot or human?" branch at the write site. A user takeover flips the claim
// and the bot simply stops mirroring; the terminal write then belongs to the
// user path.
//
// The mirror is idempotent and membership-based: it reads the ticket's current
// assignee + status (GetClaimState) and skips any step the ticket already
// satisfies. That is what lets it coexist safely with the agent's own
// cmd/exec/jira verbs — whoever writes first wins, the other no-ops.

package delegate

import (
	"context"
	"log"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

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
		log.Printf("[jira] mirror: list rules for team %s: %v", *task.TeamID, err)
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
		log.Printf("[jira] mirror: no in_progress rule for project of %s, skipping", task.EntitySourceID)
		return
	}
	go s.runJiraMirror(orgID, task.EntitySourceID, rule.InProgressCanonical, "in-progress", rule.InProgressMembers, true)
}

// mirrorJiraDone mirrors a bot-owned task's clean completion onto its Jira
// ticket: transition into the Done bucket under the system/bot credential. No
// assignee change — a completed ticket stays assigned to the bot. Detached; a
// no-op for non-Jira tasks or when no rule resolves.
func (s *Spawner) mirrorJiraDone(orgID string, task *domain.Task) {
	rule := s.jiraMirrorRule(task)
	if rule == nil {
		return
	}
	if rule.DoneCanonical == "" {
		log.Printf("[jira] mirror: no done rule for project of %s, skipping", task.EntitySourceID)
		return
	}
	go s.runJiraMirror(orgID, task.EntitySourceID, rule.DoneCanonical, "done", rule.DoneMembers, false)
}

// mirrorJiraDoneForTask loads the task and mirrors a clean completion onto its
// Jira ticket — but only while the bot still owns it. A user takeover mid-run
// flips claimed_by_agent_id to the user, after which the terminal Jira write
// belongs to the user's advance/swipe path, not this mirror, so a
// no-longer-bot-owned task is skipped. Called from terminateBlueprint's
// completed branch — the only path that should move a ticket to Done; a
// failed/aborted/cancelled run never reaches it.
func (s *Spawner) mirrorJiraDoneForTask(ctx context.Context, orgID, taskID string) {
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
	s.mirrorJiraDone(orgID, task)
}

// runJiraMirror is the detached worker both hooks share. It resolves the org's
// system/bot Jira client fresh (creds hot-swap on config change, so a client is
// never cached) and then — skipping any step the ticket already satisfies —
// assigns the service account (when assign is set) and transitions the ticket
// into the target bucket. members is the bucket's member set for the
// idempotency check; bucket is a human-readable label for logs.
func (s *Spawner) runJiraMirror(orgID, issueKey, canonical, bucket string, members []string, assign bool) {
	resolver := s.getJiraResolver()
	if resolver == nil {
		return
	}
	ctx := context.Background()
	client, err := resolver.ForSystem(ctx, orgID)
	if err != nil {
		// ErrNoJiraSystemCredential is the expected "org has no service PAT"
		// case; a backend error is logged either way so an outage isn't silent.
		log.Printf("[jira] mirror: resolve system client for org %s: %v", orgID, err)
		return
	}

	state := client.GetClaimState(ctx, issueKey)
	needAssign := assign && (state == nil || !state.AssignedToSelf)
	needTransition := state == nil || !slices.Contains(members, state.StatusName)
	if !needAssign && !needTransition {
		// A nil state forces needTransition true, so state is non-nil here.
		log.Printf("[jira] mirror: %s already in %s bucket (%q), skipping", issueKey, bucket, state.StatusName)
		return
	}
	if needAssign {
		if err := client.AssignToSelf(ctx, issueKey); err != nil {
			log.Printf("[jira] mirror: assign %s to service account: %v", issueKey, err)
			return
		}
	}
	if needTransition {
		if err := client.TransitionTo(ctx, issueKey, canonical); err != nil {
			log.Printf("[jira] mirror: transition %s to %q (%s): %v", issueKey, canonical, bucket, err)
		}
	}
}
