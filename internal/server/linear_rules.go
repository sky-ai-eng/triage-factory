package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// lookupLinearRuleForTask resolves the per-team Linear rule that governs the
// given task's issue: the task's team's linear_team_rules row, in workspaceID,
// for linearTeamID, the Linear team the issue's entity sits in
// (entities.linear_team_id). The Linear sibling of lookupJiraRuleForTask. The
// Linear team comes from that column rather than from the identifier's prefix,
// because a team key can be renamed and the team id cannot, and rather than
// from the snapshot, which turning the source off clears. Returns nil when the
// task has no TeamID stamped, linearTeamID is empty, or the team has no rule
// for that Linear team in workspaceID.
//
// Runs inside the caller's transaction so the rule read goes through the
// app-pool ListForTeam, where the linear_rules_* RLS policies gate it by team
// membership.
func lookupLinearRuleForTask(ctx context.Context, tx db.TxStores, workspaceID, linearTeamID string, task *domain.Task) *domain.LinearTeamRules {
	if task == nil || task.TeamID == nil || *task.TeamID == "" || workspaceID == "" || linearTeamID == "" {
		return nil
	}
	rules, err := tx.LinearTeamRules.ListForTeam(ctx, *task.TeamID, workspaceID)
	if err != nil {
		linearRuleLog.Warn("list rules for team failed", "team", *task.TeamID, "error", err)
		return nil
	}
	for i := range rules {
		if rules[i].LinearTeamID == linearTeamID {
			return &rules[i]
		}
	}
	return nil
}

// linearSyncTarget is what moving a Linear issue to match a TF claim needs,
// read under the acting user's claims: the issue, the rule of the Linear team
// it sits in (nil when there is none), and the acting user's Linear user id in
// the org's workspace ("" when no identity is stored). issueID is Linear's
// UUID for the issue (the entity's external_id), the address every read and
// write uses; identifier is its display key, for log lines, and can be one a
// team move or key rename has replaced since the last poll.
type linearSyncTarget struct {
	issueID      string
	identifier   string
	rule         *domain.LinearTeamRules
	linearUserID string
}

// readLinearSyncTarget reads a Linear task's sync target inside the caller's
// transaction. The workspace is the org's current one, which is the workspace
// the acting user's credential and identity belong to; a task whose issue is
// in a workspace the org no longer has bound finds no rule there, since a
// Linear team id belongs to one workspace only. A task that is not
// Linear-backed, or whose entity is gone, gets a target with no issue id and
// no rule.
func readLinearSyncTarget(ctx context.Context, tx db.TxStores, orgID, userID string, task *domain.Task) (linearSyncTarget, error) {
	target := linearSyncTarget{identifier: task.EntitySourceID}
	if task.EntitySource != "linear" {
		return target, nil
	}
	entity, err := tx.Entities.Get(ctx, orgID, task.EntityID)
	if err != nil {
		return linearSyncTarget{}, fmt.Errorf("load linear entity: %w", err)
	}
	if entity == nil {
		return target, nil
	}
	target.issueID = entity.ExternalID
	orgSet, err := tx.Orgs.GetSettings(ctx, orgID)
	if err != nil {
		return linearSyncTarget{}, fmt.Errorf("load org settings: %w", err)
	}
	workspaceID := domain.EntityScope("linear", orgSet)
	if target.linearUserID, _, err = tx.Users.GetLinearIdentity(ctx, userID, workspaceID); err != nil {
		return linearSyncTarget{}, fmt.Errorf("load linear identity: %w", err)
	}
	target.rule = lookupLinearRuleForTask(ctx, tx, workspaceID, entity.LinearTeamID, task)
	return target, nil
}

// linearState is a workflow state as a rule's members name one.
func linearState(s linear.WorkflowState) domain.LinearStateRef {
	return domain.LinearStateRef{ID: s.ID, Name: s.Name, Type: s.Type}
}

// applyLinearClaim is the write a claim makes in Linear, as the client's
// user: assign the issue to linearUserID unless it already is, and move it to
// the rule's in-progress canonical unless it already sits in an in-progress
// state. A failed assign stops before the transition, as the Jira claim does.
// Every failure is logged rather than returned: the claim has landed in TF by
// the time this runs. The move raises or resolves kinds.LinearMirrorFailing
// in orgID as the bot mirror's does. The issue is read by issueID, its UUID;
// identifier only labels the log lines.
func applyLinearClaim(ctx context.Context, c *linear.Client, orgID, issueID, identifier, linearUserID string, rule domain.LinearTeamRules) {
	issue, err := c.GetIssue(ctx, issueID)
	if err != nil {
		linearLog.Error("claim guard: read issue failed", "issue", identifier, "error", err)
		return
	}
	needAssign := issue.Assignee == nil || issue.Assignee.ID != linearUserID
	needTransition := !domain.ContainsState(rule.InProgressMembers, linearState(issue.State))
	if !needAssign && !needTransition {
		linearLog.Info("claim guard: already assigned to self and in progress, skipping", "issue", identifier, "state", issue.State.Name)
		return
	}
	if needAssign {
		if err := c.AssignIssue(ctx, issue.ID, linearUserID); err != nil {
			linearLog.Error("failed to assign", "issue", identifier, "error", err)
			return
		}
	}
	if needTransition {
		if err := c.TransitionIssue(ctx, issue.ID, rule.InProgressCanonical.ID); err != nil {
			linearLog.Error("failed to transition", "issue", identifier, "state", rule.InProgressCanonical.Name, "error", err)
			if errors.Is(err, linear.ErrStateNotFound) {
				kinds.LinearMirrorFailing.Set(ctx, orgID, kinds.LinearMirrorFor(rule, identifier), true)
			}
			return
		}
		kinds.LinearMirrorFailing.Set(ctx, orgID, kinds.LinearMirrorFor(rule, identifier), false)
	}
}

// applyLinearUnclaim is the write a requeue makes in Linear, as the client's
// user. It reverses only what the claim changed, read against prior, the
// state the claim recorded, by the rules applyJiraUnclaim follows:
//
//   - the issue is unassigned unless prior shows it was already the user's
//     (linearUserID) — a claim of an issue they already held assigned
//     nothing;
//   - it moves back to prior's state unless that state is itself an
//     in-progress member — a claim of an issue already in progress moved
//     nothing.
//
// It acts only while the issue still shows the claim: assigned to
// linearUserID and in an in-progress state. An issue assigned to anyone else
// is someone else's work, and one that has left in progress was moved on by a
// person, which a requeue does not reach back and undo. A failed unassign
// stops before the transition, the same ordering as the claim.
//
// A record with no state id is skipped before anything is read or written:
// Linear transitions only by id, so the issue could be unassigned but never
// moved back. The claim always records the snapshot's state id, so only a
// malformed row reaches this.
//
// The issue is read by issueID, its UUID; identifier only labels the log
// lines.
func applyLinearUnclaim(ctx context.Context, c *linear.Client, issueID, identifier, linearUserID string, prior domain.SourceStatus, inProgress []domain.LinearStateRef) {
	if prior.StatusID == "" {
		linearLog.Warn("requeue guard: recorded state has no id, skipping", "issue", identifier, "state", prior.StatusName)
		return
	}
	issue, err := c.GetIssue(ctx, issueID)
	if err != nil {
		linearLog.Error("requeue guard: read issue failed", "issue", identifier, "error", err)
		return
	}
	if issue.Assignee == nil || issue.Assignee.ID != linearUserID {
		linearLog.Info("requeue guard: issue is not assigned to the acting user, skipping", "issue", identifier)
		return
	}
	if !domain.ContainsState(inProgress, linearState(issue.State)) {
		linearLog.Info("requeue guard: state not in in-progress members, skipping",
			"issue", identifier, "state", issue.State.Name,
			"in_progress_members", domain.LinearStateNames(inProgress))
		return
	}
	alreadyMine := prior.AssigneeID != "" && prior.AssigneeID == linearUserID
	if !alreadyMine {
		if err := c.UnassignIssue(ctx, issue.ID); err != nil {
			linearLog.Error("failed to unassign on requeue", "issue", identifier, "error", err)
			return
		}
	}
	if domain.ContainsState(inProgress, domain.LinearStateRef{ID: prior.StatusID, Name: prior.StatusName}) {
		linearLog.Info("requeue guard: the claim found the issue already in progress, leaving its state", "issue", identifier, "state", prior.StatusName)
		return
	}
	if err := c.TransitionIssue(ctx, issue.ID, prior.StatusID); err != nil {
		linearLog.Error("failed to transition back on requeue", "issue", identifier, "state", prior.StatusName, "error", err)
	}
}
