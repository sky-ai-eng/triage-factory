// TFAC-300: board → Jira lifecycle mirror (the system/bot lane).
//
// When a delegated agent works a Jira-backed task the TF board moves the card
// through in_progress → in_review → done, but none of that reaches the real
// Jira ticket — a watcher on Jira (not TF) sees the ticket keep its original
// assignee and status the whole time. This file mirrors the board moves back
// onto the ticket under the org's system/bot credential (jira.Resolver.ForSystem,
// TFAC-34), so the bot-side lifecycle is visible in Jira exactly as the
// user-claim path already mirrors it for human-claimed tasks (the claim guard
// in server.handleTaskClaim).
//
// The buckets are ordered To Do < InProgress < InReview < Done, and the mirror
// only ever moves a ticket forward along that order. Two chokepoints drive it,
// and both aim at the review end of the range the board owns — no board/task
// hook writes Done (runJiraMirror has a done mode, but it is reserved for the
// forthcoming merge-driven Done mirror, not these):
//   - recomputeTaskBoardColumn → the column that just landed: board in_progress
//     aims the InProgress bucket, board in_review the InReview one.
//   - terminateBlueprint's completed branch → mirrorJiraInReviewForTask: a
//     finished run means the agent opened its PR and the work is awaiting human
//     review + merge. That is precisely the in-review moment, and it is NOT
//     done. A ticket only reaches Done when its PR merges — a separate,
//     entity-driven mirror (forthcoming), never a board/task-completion move.
//
// The second one does NOT track the board column, and that divergence is the
// point. A clean completion either closes the task (board done) or, when the
// run left an unresolved artifact, parks it in the board's in_review column —
// and the Jira ticket is aimed at the review bucket in both cases, closed card
// included. TF's done column means the work was submitted; the Jira ticket's
// Done means the change shipped, and nothing here knows yet whether it did.
// Collapsing the two is what would mark a ticket Done the moment a PR opened.
//
// The InReview rule is OPTIONAL, and a project that leaves it unmapped is the
// simple case: both chokepoints aim InProgress, and a watcher sees exactly what
// a two-target project has always shown them. Where it IS mapped, the ordering
// is what keeps the two targets from fighting — an agent resuming work after
// review comments does not drag the ticket back to In Progress, which matches
// what a human would do while the PR is still open.
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
// runJiraMirror carries a done mode (transition into the Done bucket); it is
// the reusable mechanism the forthcoming merge-driven Done mirror will call,
// not anything triggered from the board. So two safeguards matter: a Done write
// can come from a human (or that merge mirror), and a slow non-terminal mirror
// must never drag such a ticket back. Per-issue serialization (jiraMirrorLocks)
// keeps in-process mirrors for one ticket from interleaving, and the
// forward-only rule holds even against an out-of-band human move. See
// runJiraMirror.

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

// mirrorTarget names the bucket one mirror pass aims a ticket at. The three are
// ordered — InProgress < InReview < Done — and a non-terminal pass refuses to
// move a ticket already past its own target, which is what makes a late,
// repeated, or out-of-order mirror safe to run.
type mirrorTarget int

const (
	targetInProgress mirrorTarget = iota
	targetInReview
	targetDone
)

// String is the phase name the mirror's log lines carry.
func (t mirrorTarget) String() string {
	switch t {
	case targetInReview:
		return "in-review"
	case targetDone:
		return "done"
	default:
		return "in-progress"
	}
}

// canonical is the status this target transitions a ticket into.
func (t mirrorTarget) canonical(rule domain.JiraProjectStatusRules) domain.JiraStatusRef {
	switch t {
	case targetInReview:
		return rule.InReviewCanonical
	case targetDone:
		return rule.DoneCanonical
	default:
		return rule.InProgressCanonical
	}
}

// contains reports whether status already sits in this target's bucket — the
// idempotency skip, membership-based like every other decision here.
func (t mirrorTarget) contains(rule domain.JiraProjectStatusRules, status domain.JiraStatusRef) bool {
	switch t {
	case targetInReview:
		return rule.InReviewContains(status)
	case targetDone:
		return rule.DoneContains(status)
	default:
		return rule.InProgressContains(status)
	}
}

// mirrorJiraInProgress mirrors a bot-owned task's move into the board's
// in_progress column onto its Jira ticket: assign the service account and
// transition into the InProgress bucket, under the org's system/bot credential.
func (s *Spawner) mirrorJiraInProgress(orgID string, task *domain.Task) {
	s.mirrorJiraBoardMove(orgID, task, targetInProgress)
}

// mirrorJiraInReview is the in_review twin — the agent's work is up for a human
// — and it is where the optional rule is resolved. A project that maps an
// in-review canonical gets its ticket moved there; one that does not falls back
// to the InProgress bucket, which is the whole of what "the bot is on it" can
// say for that project.
func (s *Spawner) mirrorJiraInReview(orgID string, task *domain.Task) {
	s.mirrorJiraBoardMove(orgID, task, targetInReview)
}

// mirrorJiraBoardMove is the body the two board hooks share. Detached so Jira
// latency never blocks the board WS update; a no-op for non-Jira tasks or when
// no rule resolves (skip + log, like the user path's "no in_progress rule
// configured" branch).
func (s *Spawner) mirrorJiraBoardMove(orgID string, task *domain.Task, target mirrorTarget) {
	rule := s.jiraMirrorRule(task)
	if rule == nil {
		return
	}
	if target == targetInReview && rule.InReviewCanonical.IsZero() {
		target = targetInProgress
	}
	if target.canonical(*rule).IsZero() {
		// Never guess a status. A watched project may be stored unmapped, so an
		// unset canonical here is the ordinary "not armed yet" state rather than
		// a broken row — either way there is nothing to transition into.
		jiraLog.Warn("mirror: project has no canonical for this phase, skipping", "issue", task.EntitySourceID, "phase", target)
		return
	}
	// teamID scopes the audit row to the bot-owned task's team. Empty for a
	// team-less task → the action still lands in the org governance feed.
	teamID := ""
	if task.TeamID != nil {
		teamID = *task.TeamID
	}
	go s.runJiraMirror(orgID, task.EntitySourceID, teamID, *rule, target)
}

// mirrorJiraInReviewForTask loads the task and asserts the review bucket on its
// Jira ticket when a delegated run finishes cleanly — but only while the bot
// still owns it. A finished run means the agent opened its PR and the work is
// awaiting human review + merge: the in-review bucket for a project that maps
// one, InProgress for one that does not, and NOT done either way. The ticket
// only reaches Done when its PR merges (a separate, entity-driven mirror —
// forthcoming), never on run completion. A user takeover mid-run flips
// claimed_by_agent_id to the user, after which the terminal Jira write belongs
// to the user's own task-lifecycle writes, so a no-longer-bot-owned task is
// skipped. Called from terminateBlueprint's completed branch for BOTH of its
// outcomes — the closed task and the one parked for approval — since a Jira
// watcher is owed the same thing either way; a failed/aborted/cancelled run
// never reaches it. The mirror is idempotent, so when a board in_review move
// already mirrored this ticket during the run it is a single GetClaimState read
// and no write, and it self-heals a ticket left behind by a transient
// dispatch-time mirror failure.
func (s *Spawner) mirrorJiraInReviewForTask(ctx context.Context, orgID, taskID string) {
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
	s.mirrorJiraInReview(orgID, task)
}

// runJiraMirror is the detached worker the board hooks share, and also the done
// worker the forthcoming merge-driven Done mirror calls. It resolves the org's
// system/bot Jira client fresh (creds hot-swap on config change, so a client is
// never cached) and then assigns the service account (non-done targets only) and
// transitions the ticket into the target bucket, skipping any step the ticket
// already satisfies.
//
// Two safeguards keep a slow mirror from clobbering a write further along the
// order for the same ticket (Done comes from a human or the merge mirror, not
// the board):
//   - Per-issue serialization (jiraMirrorLocks): in-process mirrors for one
//     ticket can't interleave or reorder their writes.
//   - Forward-only: under that lock it re-reads state and makes no move once the
//     ticket has passed the target — a terminal Done is never dragged back, and
//     a ticket in the review bucket is never dragged back to In Progress —
//     whichever goroutine won the lock and even if the move happened out of band.
//
// The whole sequence is bounded by jiraMirrorTimeout so a slow ticket releases
// the lock rather than pinning it.
//
// TODO(TFAC-878): every failure below is a log line and a return. A canonical
// status that is no longer a reachable transition — retired from the project's
// workflow — makes the mirror a no-op on every board move, so Jira silently
// stops tracking the board and nothing tells the team. Needs the durable
// notification channel.
func (s *Spawner) runJiraMirror(orgID, issueKey, teamID string, rule domain.JiraProjectStatusRules, target mirrorTarget) {
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
	canonical := target.canonical(rule)

	if target == targetDone {
		// Idempotency skip when the ticket is already in the Done bucket. Unlike
		// the non-terminal paths below, a nil state (transient read failure) falls
		// through to the transition rather than skipping: reaching the done mirror
		// means the work is complete, so moving to Done is correct either way — and
		// a redundant Done→Done attempt just errors harmlessly. Moving to Done is
		// forward, never the backward regression the other skips guard.
		if state != nil && rule.DoneContains(claimStatusRef(state)) {
			jiraLog.Debug("mirror: already in done bucket, skipping", "issue", issueKey, "status", state.StatusName)
			return
		}
		if err := client.TransitionTo(ctx, issueKey, jira.Status{ID: canonical.ID, Name: canonical.Name}); err != nil {
			jiraLog.Warn("mirror: transition to done failed", "issue", issueKey, "target", canonical.Name, "error", err)
			return
		}
		// from is the status read before the move (nil state → unknown/"").
		from := ""
		if state != nil {
			from = state.StatusName
		}
		s.recordMirrorAction(ctx, orgID, issueKey, teamID, domain.ActionIssueTransitioned, from, canonical.Name)
		return
	}

	// Non-terminal phases. We must read the ticket's current status to honor the
	// forward-only invariant; GetClaimState returns nil on ANY error, so a nil
	// here is "unknown" — skip rather than risk pulling a ticket back down the
	// order from wherever a concurrent mirror or a human already put it. Blindly
	// proceeding (state == nil → assign + transition) is exactly the backward
	// move the per-issue lock exists to prevent. Self-heals: every board column
	// transition re-fires the mirror, and the failed read logs.
	if state == nil {
		jiraLog.Warn("mirror: could not read claim state; skipping", "issue", issueKey, "phase", target)
		return
	}
	current := claimStatusRef(state)
	// Forward-only. Done is terminal, and it may have been written by a human or
	// the merge mirror rather than by us. The review bucket is the same guard one
	// step earlier: an agent picking work back up after review comments must not
	// pull its ticket out of review, because the PR is still open — which is what
	// a human would leave it at too.
	if rule.DoneContains(current) || (target == targetInProgress && rule.InReviewContains(current)) {
		jiraLog.Debug("mirror: ticket already past this phase, skipping", "issue", issueKey, "status", state.StatusName, "phase", target)
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
	if !target.contains(rule, current) {
		if err := client.TransitionTo(ctx, issueKey, jira.Status{ID: canonical.ID, Name: canonical.Name}); err != nil {
			jiraLog.Warn("mirror: transition failed", "issue", issueKey, "target", canonical.Name, "phase", target, "error", err)
			return
		}
		s.recordMirrorAction(ctx, orgID, issueKey, teamID, domain.ActionIssueTransitioned, state.StatusName, canonical.Name)
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
	rm := k.reserve(key)
	rm.mu.Lock()
	return func() { k.releaseUnlock(key, rm) }
}

// tryLock is lock for a caller with something better to do than wait: it takes
// the key's mutex if it is free and reports false without blocking if it is
// not. ok=false hands back no unlock func — there is nothing to release.
func (k *keyedMutex) tryLock(key string) (unlock func(), ok bool) {
	rm := k.reserve(key)
	if !rm.mu.TryLock() {
		k.release(key, rm)
		return nil, false
	}
	return func() { k.releaseUnlock(key, rm) }, true
}

// reserve returns the key's refMutex with this caller counted against it, so
// the entry cannot be collected out from under a caller that is about to lock
// (or is holding) it. Every reserve is paired with exactly one release.
func (k *keyedMutex) reserve(key string) *refMutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.locks == nil {
		k.locks = make(map[string]*refMutex)
	}
	rm := k.locks[key]
	if rm == nil {
		rm = &refMutex{}
		k.locks[key] = rm
	}
	rm.refs++
	return rm
}

// release drops this caller's reservation, collecting the entry once nobody
// holds one. A holder's own reservation is still counted while it holds the
// mutex, so an entry can never be collected while locked.
func (k *keyedMutex) release(key string, rm *refMutex) {
	k.mu.Lock()
	defer k.mu.Unlock()
	rm.refs--
	if rm.refs == 0 {
		delete(k.locks, key)
	}
}

func (k *keyedMutex) releaseUnlock(key string, rm *refMutex) {
	rm.mu.Unlock()
	k.release(key, rm)
}

// claimStatusRef pairs a live claim read's status name with its id, so the
// membership tests compare on the id whenever both sides carry one.
func claimStatusRef(state *jira.ClaimState) domain.JiraStatusRef {
	if state == nil {
		return domain.JiraStatusRef{}
	}
	return domain.JiraStatusRef{ID: state.StatusID, Name: state.StatusName}
}
