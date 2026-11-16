package delegate

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestDelegate_OwnerTeamID_IsTheRunsTeam pins both halves of what
// DelegateOpts.OwnerTeamID is for, through the real Delegate rather than a
// stub: the firing consolidates the task's card onto the acting team, and
// everything Delegate resolves per team resolves against that team rather than
// the owner the task still carries when it is read.
//
// The second half is the one with no other guard. The task row is not
// rewritten until the firing transaction runs, near the end of Delegate — so a
// naive read of task.TeamID at the top gives the team that is being replaced,
// and the run would be priced, capped and modelled against the wrong one. The
// recorded resolver call below is what would catch that coming back.
func TestDelegate_OwnerTeamID_IsTheRunsTeam(t *testing.T) {
	database := newDelegateTestDB(t)
	org := runmode.LocalDefaultOrgID
	task, bpID := delegatableFixture(t, database, "ownerteam")

	// The team the trigger is firing for — not the one the task was created
	// under. In multi mode this is the ordinary "the owner has auto-delegation
	// disabled and a lower-priority team is firing" shape.
	const actingTeamID = "00000000-0000-0000-0000-0000000000b2"
	if _, err := database.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'acting', 'Acting')`, actingTeamID, org,
	); err != nil {
		t.Fatalf("seed acting team: %v", err)
	}
	if got := taskOwnerTeam(t, database, task.ID); got == actingTeamID {
		t.Fatalf("fixture task already owned by the acting team; the test would prove nothing")
	}
	// A run executes its own team's blueprint, so the blueprint is the acting
	// team's, as a trigger's is. The blueprint, its steps and their prompts
	// reference each other by team, so they move in one transaction with the
	// foreign keys checked at its commit.
	tx, err := database.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, q := range []string{
		`PRAGMA defer_foreign_keys = ON`,
		`UPDATE prompts SET team_id = ?1 WHERE id IN (SELECT step_prompt_id FROM blueprint_steps WHERE blueprint_id = ?2)`,
		`UPDATE blueprints SET team_id = ?1 WHERE id = ?2`,
		`UPDATE blueprint_steps SET team_id = ?1 WHERE blueprint_id = ?2`,
	} {
		if _, err := tx.Exec(q, actingTeamID, bpID); err != nil {
			t.Fatalf("move the blueprint to the acting team: %v\n%s", err, q)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("move the blueprint to the acting team: %v", err)
	}

	var resolvedFor []string
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	s.SetRunCredentialResolvers(nil, nil, func(_ context.Context, _, teamID string) (domain.TeamModels, error) {
		resolvedFor = append(resolvedFor, teamID)
		return domain.NewTeamModels("claude-sonnet-4-6", domain.ModelSet{}), nil
	})

	brID, err := s.Delegate(task, DelegateOpts{
		OrgID: org, ExplicitBlueprintID: bpID,
		TriggerType: "manual", CreatorUserID: runmode.LocalDefaultUserID,
		OwnerTeamID: actingTeamID,
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}

	if got := taskOwnerTeam(t, database, task.ID); got != actingTeamID {
		t.Errorf("task owner team = %q, want %q — the firing did not consolidate the card", got, actingTeamID)
	}
	if len(resolvedFor) != 1 || resolvedFor[0] != actingTeamID {
		t.Errorf("per-team model resolution ran for %v, want [%s] — the run resolved against the owner it was replacing", resolvedFor, actingTeamID)
	}
	var steps int
	if err := database.QueryRow(
		`SELECT count(*) FROM conversations WHERE blueprint_run_id = ?`, brID,
	).Scan(&steps); err != nil {
		t.Fatalf("count step conversations: %v", err)
	}
	if steps != 1 {
		t.Errorf("step conversations on the run = %d, want 1", steps)
	}
}

// TestDelegate_NoOwnerTeamID_LeavesTheCardAlone is the other side: a firing
// that asks for no consolidation resolves against, and leaves in place, the
// team the task already carries. The manual paths and the pending-firings
// drain both take this arm.
func TestDelegate_NoOwnerTeamID_LeavesTheCardAlone(t *testing.T) {
	database := newDelegateTestDB(t)
	org := runmode.LocalDefaultOrgID
	task, bpID := delegatableFixture(t, database, "ownerteam-none")
	ownerBefore := taskOwnerTeam(t, database, task.ID)

	var resolvedFor []string
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	s.SetRunCredentialResolvers(nil, nil, func(_ context.Context, _, teamID string) (domain.TeamModels, error) {
		resolvedFor = append(resolvedFor, teamID)
		return domain.NewTeamModels("claude-sonnet-4-6", domain.ModelSet{}), nil
	})

	if _, err := s.Delegate(task, DelegateOpts{
		OrgID: org, ExplicitBlueprintID: bpID,
		TriggerType: "manual", CreatorUserID: runmode.LocalDefaultUserID,
	}); err != nil {
		t.Fatalf("Delegate: %v", err)
	}

	if got := taskOwnerTeam(t, database, task.ID); got != ownerBefore {
		t.Errorf("task owner team = %q, want %q unchanged", got, ownerBefore)
	}
	if len(resolvedFor) != 1 || resolvedFor[0] != ownerBefore {
		t.Errorf("per-team model resolution ran for %v, want [%s] (the task's own owner)", resolvedFor, ownerBefore)
	}
}

func taskOwnerTeam(t *testing.T, database *sql.DB, taskID string) string {
	t.Helper()
	var team sql.NullString
	if err := database.QueryRow(`SELECT team_id FROM tasks WHERE id = ?`, taskID).Scan(&team); err != nil {
		t.Fatalf("read task team_id: %v", err)
	}
	return team.String
}
