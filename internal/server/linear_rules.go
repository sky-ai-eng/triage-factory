package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

// lookupLinearRuleForTask resolves the per-team Linear rule that governs the
// given task's issue: the task's team's linear_team_rules row, in workspaceID,
// for the Linear team the entity's snapshot names. The Linear sibling of
// lookupJiraRuleForTask. The Linear team comes from the snapshot rather than
// from the identifier's prefix, because a team key can be renamed and the
// team id cannot. Returns nil when the task is not Linear-backed, has no
// TeamID stamped, its entity has no snapshot naming a team, or the team has
// no rule for that Linear team in workspaceID.
//
// Runs inside the caller's transaction so the rule read goes through the
// app-pool ListForTeam, where the linear_rules_* RLS policies gate it by team
// membership.
func lookupLinearRuleForTask(ctx context.Context, tx db.TxStores, orgID, workspaceID string, task *domain.Task) *domain.LinearTeamRules {
	if task == nil || task.EntitySource != "linear" || task.TeamID == nil || *task.TeamID == "" || workspaceID == "" {
		return nil
	}
	entity, err := tx.Entities.Get(ctx, orgID, task.EntityID)
	if err != nil {
		linearRuleLog.Warn("read entity for rule lookup failed", "entity", task.EntityID, "error", err)
		return nil
	}
	if entity == nil || entity.SnapshotJSON == "" {
		return nil
	}
	var snap domain.LinearSnapshot
	if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
		linearRuleLog.Warn("parse entity snapshot for rule lookup failed", "entity", task.EntityID, "error", err)
		return nil
	}
	if snap.TeamID == "" {
		return nil
	}
	rules, err := tx.LinearTeamRules.ListForTeam(ctx, *task.TeamID, workspaceID)
	if err != nil {
		linearRuleLog.Warn("list rules for team failed", "team", *task.TeamID, "error", err)
		return nil
	}
	for i := range rules {
		if rules[i].LinearTeamID == snap.TeamID {
			return &rules[i]
		}
	}
	return nil
}

// linearSyncTarget is what moving a Linear issue to match a TF claim needs,
// read under the acting user's claims: the issue, the rule of the Linear team
// it sits in (nil when there is none), and the acting user's Linear user id in
// the org's workspace ("" when no identity is stored).
type linearSyncTarget struct {
	identifier   string
	rule         *domain.LinearTeamRules
	linearUserID string
}

// readLinearSyncTarget reads a Linear task's sync target inside the caller's
// transaction. The workspace is the org's current one, which is the workspace
// the acting user's credential and identity belong to; a task whose issue is
// in a workspace the org no longer has bound finds no rule there, since a
// Linear team id belongs to one workspace only.
func readLinearSyncTarget(ctx context.Context, tx db.TxStores, orgID, userID string, task *domain.Task) (linearSyncTarget, error) {
	orgSet, err := tx.Orgs.GetSettings(ctx, orgID)
	if err != nil {
		return linearSyncTarget{}, fmt.Errorf("load org settings: %w", err)
	}
	workspaceID := domain.EntityScope("linear", orgSet)
	linearUserID, _, err := tx.Users.GetLinearIdentity(ctx, userID, workspaceID)
	if err != nil {
		return linearSyncTarget{}, fmt.Errorf("load linear identity: %w", err)
	}
	return linearSyncTarget{
		identifier:   task.EntitySourceID,
		rule:         lookupLinearRuleForTask(ctx, tx, orgID, workspaceID, task),
		linearUserID: linearUserID,
	}, nil
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
// the time this runs.
func applyLinearClaim(ctx context.Context, c *linear.Client, identifier, linearUserID string, rule domain.LinearTeamRules) {
	issue, err := c.GetIssue(ctx, identifier)
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
		}
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
func applyLinearUnclaim(ctx context.Context, c *linear.Client, identifier, linearUserID string, prior domain.SourceStatus, inProgress []domain.LinearStateRef) {
	issue, err := c.GetIssue(ctx, identifier)
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
