// The board → Linear lifecycle mirror (the system/bot lane), the Linear sibling
// of the in-progress half of jira_mirror.go.
//
// When a delegated agent picks up a Linear-backed task, the board moves the
// card into In Progress. This file moves the issue into its Linear team's
// in-progress canonical state under the org's service credential
// (linear.Resolver.ForSystem), as server.applyLinearClaim does when a person
// claims the task. It runs from the same two chokepoints as the Jira mirror,
// through mirrorInProgress: announceTaskPlacement at mint, and
// terminateBlueprint's completed branch, which re-asserts the bucket. Both
// only ever see bot-owned tasks, so every write here is bot-attributed by
// construction.
//
// It never touches the issue's assignee. TF assigns an issue only when a
// person claims it with their own credential; the org credential assigns
// nobody. When that credential is an app install, its user could not be
// assigned an issue anyway: Linear lets an app be delegated an issue, never
// assigned one, and delegation needs a scope the install does not request.
//
// The guarantees are the Jira mirror's. It reads the issue's current state and
// skips a move the issue already satisfies, so it coexists with the agent's
// own tfac exec linear verbs. It is forward-only: an issue already in one of
// the rule's done states is never moved back, whoever moved it there. And it
// is serialized per issue (linearMirrorLocks), so the mint-time pass and the
// completion pass for one issue never interleave.

package delegate

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

// linearMirrorTimeout bounds one mirror operation (resolve + GetIssue +
// transition) end to end. The Linear client caps each request at 15s; this
// caps the whole sequence so a slow issue can't pin the per-issue lock for
// long.
const linearMirrorTimeout = 30 * time.Second

// lookupLinearRuleForTaskSystem resolves the per-team Linear rule governing
// the task's issue in workspaceID, under the admin pool (ListForTeamSystem):
// the spawner runs in system context, not RLS. The Linear team is the one the
// task's entity sits in (entities.linear_team_id), which a team key rename
// does not move. The system-door twin of server.lookupLinearRuleForTask.
// Returns nil when the task is not Linear-backed, carries no TeamID, its
// entity names no Linear team, or the team has no rule for that Linear team in
// workspaceID.
func lookupLinearRuleForTaskSystem(ctx context.Context, entities db.EntityStore, linearRules db.LinearTeamRulesStore, orgID, workspaceID string, task *domain.Task) *domain.LinearTeamRules {
	if task == nil || task.EntitySource != "linear" || task.TeamID == nil || *task.TeamID == "" || workspaceID == "" {
		return nil
	}
	entity, err := entities.GetSystem(ctx, orgID, task.EntityID)
	if err != nil {
		linearLog.Warn("mirror: read the task's entity failed", "entity", task.EntityID, "error", err)
		return nil
	}
	if entity == nil || entity.LinearTeamID == "" {
		return nil
	}
	rules, err := linearRules.ListForTeamSystem(ctx, *task.TeamID, workspaceID)
	if err != nil {
		linearLog.Warn("mirror: list rules for team failed", "team", *task.TeamID, "error", err)
		return nil
	}
	for i := range rules {
		if rules[i].LinearTeamID == entity.LinearTeamID {
			return &rules[i]
		}
	}
	return nil
}

// linearMirrorRule resolves the rule for the task's Linear team in the org's
// current workspace, returning nil when the mirror is disabled (no resolver,
// rules, orgs or entity store wired) or the task isn't a rule-backed Linear
// task.
func (s *Spawner) linearMirrorRule(orgID string, task *domain.Task) *domain.LinearTeamRules {
	if s.linearRules == nil || s.orgs == nil || s.entities == nil || s.getLinearResolver() == nil {
		return nil
	}
	ctx := context.Background()
	settings, err := s.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		linearLog.Warn("mirror: read org settings for the linear workspace failed", "org", orgID, "error", err)
		return nil
	}
	return lookupLinearRuleForTaskSystem(ctx, s.entities, s.linearRules, orgID, domain.EntityScope("linear", settings), task)
}

// mirrorLinearInProgress moves a bot-owned task's Linear issue into its
// team's in-progress canonical state under the org's service credential.
// Detached so Linear latency never blocks the board WS update; a no-op for a
// task no rule resolves for.
func (s *Spawner) mirrorLinearInProgress(orgID string, task *domain.Task) {
	rule := s.linearMirrorRule(orgID, task)
	if rule == nil {
		return
	}
	if rule.InProgressCanonical.IsZero() {
		// Never guess a state. A watched Linear team may be stored unmapped, so
		// an unset canonical is the ordinary "not armed yet" state.
		linearLog.Warn("mirror: no in_progress rule for linear team, skipping", "issue", task.EntitySourceID, "linear_team", rule.LinearTeamID)
		return
	}
	teamID := ""
	if task.TeamID != nil {
		teamID = *task.TeamID
	}
	go s.runLinearMirror(orgID, task.EntityID, task.EntitySourceID, teamID, *rule)
}

// runLinearMirror is the detached worker both in-progress hooks share. It
// resolves the org's Linear client fresh (credentials hot-swap on config
// change, so a client is never cached), reads the issue, and moves it into
// the rule's in-progress canonical unless it already sits in an in-progress
// state or has reached a done one. identifier is the caller's read of the
// issue's key, which only labels a log line when the entity can't be read.
//
// TODO(TFAC-878): every failure below is a log line and a return. An
// in-progress canonical deleted from the Linear team's workflow makes the
// mirror a no-op on every delegation, so Linear silently stops tracking the
// board and nothing tells the team. Needs the durable notification channel.
func (s *Spawner) runLinearMirror(orgID, entityID, identifier, teamID string, rule domain.LinearTeamRules) {
	resolver := s.getLinearResolver()
	if resolver == nil || s.entities == nil || s.orgs == nil {
		return
	}

	// Serialize before any Linear call so the read→write decision for this
	// issue is made under mutual exclusion with the other pass's mirror.
	unlock := s.linearMirrorLocks.lock(orgID + "\x00" + entityID)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), linearMirrorTimeout)
	defer cancel()

	// The entity is read again under the lock. Linear is addressed by the
	// issue's UUID, which a team move or key rename leaves alone, and the logs
	// and audit row name it by the identifier it answers to now. Unlike the
	// Jira mirror there is no key to fall back on: the workspace check below
	// needs the entity's scope.
	e, err := s.entities.GetSystem(ctx, orgID, entityID)
	if err != nil || e == nil {
		linearLog.Warn("mirror: read the issue's entity failed; skipping", "issue", identifier, "error", err)
		return
	}
	identifier = e.SourceID

	// The client acts in the org's current Linear workspace. An issue tracked
	// in another workspace is not reachable through it, so it is left alone.
	set, err := s.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		linearLog.Warn("mirror: read org linear workspace failed", "org", orgID, "error", err)
		return
	}
	if current := domain.EntityScope("linear", set); current != e.Scope {
		linearLog.Info("mirror: issue is in another linear workspace; skipping", "issue", identifier, "issue_workspace", e.Scope, "org_workspace", current)
		return
	}

	client, err := resolver.ForSystem(ctx, orgID)
	if err != nil {
		if shouldLogForSystemErr(err) {
			linearLog.Warn("mirror: resolve system client failed", "org", orgID, "error", err)
		}
		return
	}

	// A failed read leaves the state unknown. Skip rather than write: the
	// issue may already be done, and a blind move would drag it back. A clean
	// completion re-asserts the bucket later.
	issue, err := client.GetIssue(ctx, e.ExternalID)
	if err != nil {
		linearLog.Warn("mirror: read issue failed; skipping in-progress mirror", "issue", identifier, "error", err)
		return
	}
	state := domain.LinearStateRef{ID: issue.State.ID, Name: issue.State.Name, Type: issue.State.Type}
	if domain.ContainsState(rule.DoneMembers, state) {
		linearLog.Debug("mirror: already done, skipping in-progress mirror", "issue", identifier, "state", issue.State.Name)
		return
	}
	if domain.ContainsState(rule.InProgressMembers, state) {
		linearLog.Debug("mirror: already in progress, skipping", "issue", identifier, "state", issue.State.Name)
		return
	}
	if err := client.TransitionIssue(ctx, issue.ID, rule.InProgressCanonical.ID); err != nil {
		linearLog.Warn("mirror: transition to in-progress failed", "issue", identifier, "target", rule.InProgressCanonical.Name, "error", err)
		return
	}
	s.recordLinearMirrorTransition(ctx, orgID, teamID, e, issue, rule.InProgressCanonical.Name)
}

// recordLinearMirrorTransition appends the external_actions row for one
// board→Linear move: a system action under the org's Linear credential with no
// human actor, scoped to the bot-owned task's team, about the task's entity.
// The detached mirror holds no conversation handle, so conversation_id is left
// NULL. The identifier is the target and the UUID the external id, as on an
// agent's Linear write. Best-effort: a recording failure is logged and
// swallowed so it never unwinds the move it observed.
func (s *Spawner) recordLinearMirrorTransition(ctx context.Context, orgID, teamID string, e *domain.Entity, issue *linear.Issue, to string) {
	if s.externalActions == nil {
		return
	}
	err := s.externalActions.RecordSystem(ctx, orgID, domain.ExternalAction{
		TeamID:     teamID,
		Provider:   domain.ArtifactProviderLinear,
		Scope:      e.Scope,
		Action:     domain.ActionIssueTransitioned,
		Target:     e.SourceID,
		EntityID:   e.ID,
		ExternalID: e.ExternalID,
		URL:        issue.URL,
		FromState:  issue.State.Name,
		ToState:    to,
		Credential: domain.CredentialLinearOrg,
	})
	if err != nil {
		linearLog.Warn("mirror: external-action recording failed (Linear move already applied)",
			"issue", e.SourceID, "error", err)
	}
}
