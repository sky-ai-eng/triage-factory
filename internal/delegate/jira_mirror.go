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
//
// In-progress and done mirrors run in detached goroutines, so two safeguards
// keep a slow in-progress mirror from fighting the done mirror for one ticket:
// per-issue serialization (jiraMirrorLocks) and a forward-only in-progress rule
// (never transition a ticket already in the Done bucket). See runJiraMirror.

package delegate

import (
	"context"
	"errors"
	"log"
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
	go s.runJiraMirror(orgID, task.EntitySourceID, *rule, false)
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
	go s.runJiraMirror(orgID, task.EntitySourceID, *rule, true)
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
// never cached) and then assigns the service account (in-progress only) and
// transitions the ticket into the target bucket, skipping any step the ticket
// already satisfies.
//
// Two safeguards keep a slow in-progress mirror from clobbering the done mirror
// for the same ticket:
//   - Per-issue serialization (jiraMirrorLocks): the in-progress and done
//     mirrors for one ticket can't interleave or reorder their writes.
//   - Forward-only in-progress: under that lock it re-reads state and, if a done
//     mirror has already advanced the ticket into the Done bucket, makes no
//     in-progress move — so a terminal Done is never dragged back to In
//     Progress, whichever goroutine won the lock.
//
// The whole sequence is bounded by jiraMirrorTimeout so a slow ticket releases
// the lock rather than pinning it.
func (s *Spawner) runJiraMirror(orgID, issueKey string, rule domain.JiraProjectStatusRules, done bool) {
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
			log.Printf("[jira] mirror: resolve system client for org %s: %v", orgID, err)
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
			log.Printf("[jira] mirror: %s already in done bucket (%q), skipping", issueKey, state.StatusName)
			return
		}
		if err := client.TransitionTo(ctx, issueKey, rule.DoneCanonical); err != nil {
			log.Printf("[jira] mirror: transition %s to %q (done): %v", issueKey, rule.DoneCanonical, err)
		}
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
		log.Printf("[jira] mirror: could not read claim state for %s; skipping in-progress mirror", issueKey)
		return
	}
	// Forward-only: a concurrent done mirror may have already advanced the ticket
	// into the Done bucket — never drag a terminal ticket back to In Progress.
	if rule.DoneContains(state.StatusName) {
		log.Printf("[jira] mirror: %s already advanced to done (%q), skipping in-progress mirror", issueKey, state.StatusName)
		return
	}
	if !state.AssignedToSelf {
		if err := client.AssignToSelf(ctx, issueKey); err != nil {
			// Skip the transition too: assign + transition move together (same as
			// the user-path claim guard), so a failed assign leaves the ticket
			// untouched — To Do + unassigned — rather than "In Progress but
			// unassigned". Self-heals on the next board transition's mirror pass.
			log.Printf("[jira] mirror: assign %s to service account: %v", issueKey, err)
			return
		}
	}
	if !rule.InProgressContains(state.StatusName) {
		if err := client.TransitionTo(ctx, issueKey, rule.InProgressCanonical); err != nil {
			log.Printf("[jira] mirror: transition %s to %q (in-progress): %v", issueKey, rule.InProgressCanonical, err)
		}
	}
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
