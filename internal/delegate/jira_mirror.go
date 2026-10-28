// The board → Jira lifecycle mirror (the system/bot lane).
//
// When a delegated agent works a Jira-backed task the TF board moves the card
// into In Progress and, on a clean finish, to Done, but none of that reaches
// the real Jira ticket — a watcher on Jira (not TF) sees the ticket keep its
// original assignee and status the whole time. This file mirrors the board
// moves back onto the ticket under the org's system/bot credential
// (jira.Resolver.ForSystem), so the bot-side lifecycle is visible in Jira
// exactly as the user-claim path already mirrors it for human-claimed tasks
// (the claim guard in server.handleTaskClaim).
//
// One chokepoint drives it: announceTaskPlacement → mirrorJiraInProgress, which
// moves the ticket into the InProgress bucket. The delegation was minted, and
// stamping the bot claim put the card in In Progress, so the bot has the
// ticket. One delegation makes at most one mirror pass, because the
// announcement runs once at mint rather than on a recomputed column. A
// Linear-backed task takes the same point into linear_mirror.go instead.
//
// Nothing on the board writes Done, and a clean run completion leaves the
// ticket alone. A finished run means the agent opened its PR and the work is
// awaiting human review + merge, which is still "in progress" to a watcher. A
// ticket only reaches Done when its PR merges — a separate, entity-driven
// mirror (forthcoming), for which runJiraMirror's done mode is reserved.
// PR-opened ≠ ticket-done: the task lifecycle (TF's board "done" column =
// work submitted) is not the entity lifecycle (the change shipped).
//
// The chokepoint only ever sees bot-owned tasks: announceTaskPlacement
// early-returns unless claimed_by_agent_id is set. So every write here is
// bot-attributed by construction — there is no "bot or human?" branch at the
// write site. After a user takeover the ticket's lifecycle belongs to the user
// path.
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
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

// jiraMirrorTimeout bounds one mirror operation (resolve + GetClaimState +
// assign + transition) end to end. The Jira client already caps each HTTP call
// at 15s; this caps the whole multi-call sequence so a slow ticket can't pin
// the per-issue mirror lock for long.
const jiraMirrorTimeout = 30 * time.Second

// lookupJiraRuleForTaskSystem resolves the per-team Jira status rule governing
// the task's source project on site, under the admin pool (ListForTeamSystem)
// — the spawner runs in system context, not RLS. The project is the registry
// row the task's entity references (entities.jira_project_id), which a project
// key rename does not move. The system-door twin of
// server.lookupJiraRuleForTask. Returns nil when the task is not Jira-backed,
// carries no TeamID, or the team has no rule for the ticket's project.
func lookupJiraRuleForTaskSystem(ctx context.Context, entities db.EntityStore, jiraRules db.JiraStatusRulesStore, orgID, site string, task *domain.Task) *domain.JiraProjectStatusRules {
	if task == nil || task.EntitySource != "jira" || task.TeamID == nil || *task.TeamID == "" {
		return nil
	}
	entity, err := entities.GetSystem(ctx, orgID, task.EntityID)
	if err != nil {
		jiraLog.Warn("mirror: read the task's entity failed", "entity", task.EntityID, "error", err)
		return nil
	}
	if entity == nil || entity.JiraProjectID == "" {
		return nil
	}
	rules, err := jiraRules.ListForTeamSystem(ctx, *task.TeamID, site)
	if err != nil {
		jiraLog.Warn("mirror: list status rules for team failed", "team", *task.TeamID, "error", err)
		return nil
	}
	return domain.RuleForJiraProject(rules, entity.JiraProjectID)
}

// jiraMirrorRule resolves the status rule for the task's project on the org's
// Jira site, returning nil when the mirror is disabled (no resolver, rules,
// orgs or entity store wired — tests, or local without Jira) or the task isn't a
// rule-backed Jira task. Callers branch on the single nil check.
func (s *Spawner) jiraMirrorRule(orgID string, task *domain.Task) *domain.JiraProjectStatusRules {
	if s.jiraRules == nil || s.orgs == nil || s.entities == nil || s.getJiraResolver() == nil {
		return nil
	}
	ctx := context.Background()
	settings, err := s.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		jiraLog.Warn("mirror: read org settings for the jira site failed", "org", orgID, "error", err)
		return nil
	}
	return lookupJiraRuleForTaskSystem(ctx, s.entities, s.jiraRules, orgID, domain.EntityScope("jira", settings), task)
}

// mirrorJiraInProgress mirrors a bot-owned task's board move onto its Jira
// ticket: assign the service account if the ticket is unassigned, and
// transition into the InProgress bucket, under the org's system/bot credential.
// Detached so Jira latency never blocks the board WS update; a no-op for
// non-Jira tasks or when no rule resolves (skip + log, like the user path's
// "no in_progress rule configured" branch).
func (s *Spawner) mirrorJiraInProgress(orgID string, task *domain.Task) {
	rule := s.jiraMirrorRule(orgID, task)
	if rule == nil {
		return
	}
	if rule.InProgressCanonical.IsZero() {
		// Never guess a status. A watched project may be stored unmapped, so an
		// unset canonical here is the ordinary "not armed yet" state rather than
		// a broken row — either way there is nothing to transition into.
		jiraLog.Warn("mirror: no in_progress rule for project, skipping", "issue", task.EntitySourceID)
		return
	}
	// teamID scopes the audit row to the bot-owned task's team (TFAC-483). Empty
	// for a team-less task → the action still lands in the org governance feed.
	teamID := ""
	if task.TeamID != nil {
		teamID = *task.TeamID
	}
	go s.runJiraMirror(orgID, task.EntityID, task.EntitySourceID, teamID, *rule, false)
}

// runJiraMirror is the detached worker the in-progress hook starts, and also the
// done worker the forthcoming merge-driven Done mirror calls. It resolves the
// org's system/bot Jira client fresh (creds hot-swap on config change, so a
// client is never cached) and then assigns the service account (in-progress
// only, and only on an unassigned ticket — a ticket assigned to a person keeps
// its assignee) and transitions the ticket into the target bucket, skipping any
// step the ticket already satisfies.
//
// Two safeguards keep a slow in-progress mirror from clobbering a Done write for
// the same ticket (Done now comes from a human or the merge mirror, not the
// board):
//   - Per-issue serialization (jiraMirrorLocks): in-process mirrors for one
//     ticket can't interleave or reorder their writes. The lock is keyed on
//     the ticket's entity, not its key: a ticket that moved answers to both
//     its old key and its new one, and a mirror holding each would not
//     serialize. issueKey is the caller's read of the key; under the lock
//     the mirror takes the entity's current key and issue id instead.
//   - Forward-only in-progress: under that lock it re-reads state and, if the
//     ticket is already in the Done bucket, makes no in-progress move — so a
//     terminal Done is never dragged back to In Progress, whichever goroutine
//     won the lock and even if the Done move happened out of band.
//
// The whole sequence is bounded by jiraMirrorTimeout so a slow ticket releases
// the lock rather than pinning it.
//
// TODO(TFAC-878): every failure below is a log line and a return. A canonical
// status that is no longer a reachable transition — retired from the project's
// workflow — makes the mirror a no-op on every board move, so Jira silently
// stops tracking the board and nothing tells the team. Needs the durable
// notification channel.
func (s *Spawner) runJiraMirror(orgID, entityID, issueKey, teamID string, rule domain.JiraProjectStatusRules, done bool) {
	resolver := s.getJiraResolver()
	if resolver == nil {
		return
	}

	// Serialize before any Jira call so the read→write decision for this ticket
	// is made under mutual exclusion with the other phase's mirror.
	unlock := s.jiraMirrorLocks.lock(orgID + "\x00" + entityID)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), jiraMirrorTimeout)
	defer cancel()

	// The entity is read again under the lock: the issue may have moved since
	// the caller read its task. The Jira calls name the issue by its id, which
	// a move leaves alone; the logs and the audit row name it by the key it
	// answers to now. A failed read keeps the caller's key, which Jira still
	// resolves. The entity's scope — its Jira site — is the audit row's.
	issue, issueID, scope := issueKey, "", ""
	if s.entities != nil {
		if e, err := s.entities.GetSystem(ctx, orgID, entityID); err == nil && e != nil && e.SourceID != "" {
			issueKey, issueID, scope = e.SourceID, e.ExternalID, e.Scope
			issue = e.JiraIssueRef()
		}
	}

	// The client talks to the org's Jira site now. An issue tracked on
	// another site is not reachable through it — the same key there is a
	// different issue — so it is left alone.
	if scope != "" && s.orgs != nil {
		set, err := s.orgs.GetSettingsSystem(ctx, orgID)
		if err != nil {
			jiraLog.Warn("mirror: read org jira site failed", "org", orgID, "error", err)
			return
		}
		if current := domain.EntityScope("jira", set); current != scope {
			jiraLog.Info("mirror: issue is on another jira site; skipping", "issue", issueKey, "issue_site", scope, "org_site", current)
			return
		}
	}

	client, err := resolver.ForSystem(ctx, orgID)
	if err != nil {
		if shouldLogForSystemErr(err) {
			jiraLog.Warn("mirror: resolve system client failed", "org", orgID, "error", err)
		}
		return
	}

	state := client.GetClaimState(ctx, issue)

	if done {
		// Idempotency skip when the ticket is already in the Done bucket. Unlike
		// the in-progress path below, a nil state (transient read failure) falls
		// through to the transition rather than skipping: reaching the done mirror
		// means the work is complete, so moving to Done is correct either way — and
		// a redundant Done→Done attempt just errors harmlessly. Moving to Done is
		// forward, never the backward regression the in-progress skip guards.
		if state != nil && rule.DoneContains(claimStatusRef(state)) {
			jiraLog.Debug("mirror: already in done bucket, skipping", "issue", issueKey, "status", state.StatusName)
			return
		}
		if err := client.TransitionTo(ctx, issue, jira.Status{ID: rule.DoneCanonical.ID, Name: rule.DoneCanonical.Name}); err != nil {
			jiraLog.Warn("mirror: transition to done failed", "issue", issueKey, "target", rule.DoneCanonical.Name, "error", err)
			return
		}
		// from is the status read before the move (nil state → unknown/"").
		from := ""
		if state != nil {
			from = state.StatusName
		}
		s.recordMirrorAction(ctx, orgID, entityID, scope, issueKey, issueID, teamID, domain.ActionIssueTransitioned, from, rule.DoneCanonical.Name)
		return
	}

	// In-progress phase. We must read the ticket's current status to honor the
	// forward-only invariant; GetClaimState returns nil on ANY error, so a nil
	// here is "unknown" — skip rather than risk transitioning a ticket a
	// concurrent done mirror already moved to Done back to In Progress. Blindly
	// proceeding (state == nil → assign + transition to In Progress) is exactly
	// the backward move the per-issue lock exists to prevent. The failed read
	// logs, and the ticket stays where it was.
	if state == nil {
		jiraLog.Warn("mirror: could not read claim state; skipping in-progress mirror", "issue", issueKey)
		return
	}
	// Forward-only: a concurrent done mirror may have already advanced the ticket
	// into the Done bucket — never drag a terminal ticket back to In Progress.
	if rule.DoneContains(claimStatusRef(state)) {
		jiraLog.Debug("mirror: already advanced to done, skipping in-progress mirror", "issue", issueKey, "status", state.StatusName)
		return
	}
	if state.Unassigned {
		if err := client.AssignToSelf(ctx, issue); err != nil {
			// Skip the transition too: assign + transition move together (same as
			// the user-path claim guard), so a failed assign leaves the ticket
			// untouched — To Do + unassigned — rather than "In Progress but
			// unassigned".
			jiraLog.Warn("mirror: assign to service account failed", "issue", issueKey, "error", err)
			return
		}
		s.recordMirrorAction(ctx, orgID, entityID, scope, issueKey, issueID, teamID, domain.ActionIssueAssigned, "", "")
	}
	if !rule.InProgressContains(claimStatusRef(state)) {
		if err := client.TransitionTo(ctx, issue, jira.Status{ID: rule.InProgressCanonical.ID, Name: rule.InProgressCanonical.Name}); err != nil {
			jiraLog.Warn("mirror: transition to in-progress failed", "issue", issueKey, "target", rule.InProgressCanonical.Name, "error", err)
			return
		}
		s.recordMirrorAction(ctx, orgID, entityID, scope, issueKey, issueID, teamID, domain.ActionIssueTransitioned, state.StatusName, rule.InProgressCanonical.Name)
	}
}

// recordMirrorAction appends one external_actions row for a board→Jira mirror
// write (TFAC-483): a system/bot action under the org Jira service-account
// credential, with no human actor (actor_user_id NULL). team_id scopes it to the
// bot-owned task's team. The detached mirror holds no conversation handle, so conversation_id is
// left NULL. The row is about the task's entity (entityID), the issue the
// mirror moved, which a rename of the issue moves the row's link with. The
// issue key is the target and the issue id the external id, as on an agent's
// Jira write; an entity that has not learned its id yet records the key there
// instead. scope is the entity's Jira site; when the entity could not be read
// it is "", and the row takes the org's current site, which the mirror's
// system client wrote to. Admin pool (RecordSystem — no JWT claims).
// Best-effort: a recording failure is logged and swallowed so it never unwinds
// the Jira move it observed, and nil-safe for a partial test Stores.
func (s *Spawner) recordMirrorAction(ctx context.Context, orgID, entityID, scope, issueKey, issueID, teamID, action, from, to string) {
	if s.externalActions == nil {
		return
	}
	externalID := issueID
	if externalID == "" {
		externalID = issueKey
	}
	if scope == "" && s.orgs != nil {
		if set, err := s.orgs.GetSettingsSystem(ctx, orgID); err == nil {
			scope = domain.ExternalObjectScope(domain.ArtifactProviderJira, set)
		}
	}
	err := s.externalActions.RecordSystem(ctx, orgID, domain.ExternalAction{
		TeamID:     teamID,
		Provider:   domain.ArtifactProviderJira,
		Scope:      scope,
		Action:     action,
		Target:     issueKey,
		EntityID:   entityID,
		ExternalID: externalID,
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

// shouldLogForSystemErr reports whether a resolver.ForSystem error, from the
// Jira or the Linear resolver, is worth logging. ErrNoJiraSystemCredential and
// ErrNoLinearSystemCredential are expected, normal states (an org without that
// tracker, or creds removed mid-flight), and so is eventsource.ErrDisabled (an
// org admin turned the tracker off, so leaving its tickets alone is the
// switch working), so the mirror no-ops silently rather than logging on every
// mirrored board transition; any other error is a real failure (e.g. a vault
// outage) and is logged. errors.Is unwraps, so the wrapped sentinel ForSystem
// actually returns ("%w: org=...") is still treated as silent.
func shouldLogForSystemErr(err error) bool {
	return err != nil &&
		!errors.Is(err, jira.ErrNoJiraSystemCredential) &&
		!errors.Is(err, linear.ErrNoLinearSystemCredential) &&
		!errors.Is(err, eventsource.ErrDisabled)
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
