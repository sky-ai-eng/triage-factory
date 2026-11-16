package delegate

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestTerminateBlueprint_Postgres_EndsARunItsDelegatorCanNoLongerWrite: a
// person delegates, and before the run finishes they leave its team or become
// a viewer on it. The run's end is the orchestrator's decision, not theirs, so
// it still lands: the run is completed and its task closes. Ended under the
// delegator's claims, the write would match no row under the team's write
// policy, and the run would stay running, holding the task's one active run.
func TestTerminateBlueprint_Postgres_EndsARunItsDelegatorCanNoLongerWrite(t *testing.T) {
	for _, tc := range []struct {
		name, demote string
	}{
		{"left the team", `DELETE FROM memberships WHERE user_id = $1 AND team_id = $2`},
		{"became a viewer", `UPDATE memberships SET role = 'viewer' WHERE user_id = $1 AND team_id = $2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := pgtest.Shared(t)
			h.Reset(t)
			// The app half is the RLS-enforced pool, so a write made under
			// anyone's claims meets the row policies it would in production.
			stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
			s := NewSpawner(h.AdminDB, stores, nil, nil, "")

			orgID, delegator, teamID := pgtest.SeedOrgWithUser(t, h, "delegator")
			// A team keeps at least one admin, so the delegator can only step
			// down beside another.
			pgtest.AddOrgMember(t, h, pgtest.SeedUser(t, h, "admin"), orgID, teamID, "member", "admin")
			entity, _, err := stores.Entities.FindOrCreateSystem(t.Context(), orgID, "github", "https://github.com", "octo/repo#1471", "", "pr", "PR", "")
			if err != nil {
				t.Fatalf("seed entity: %v", err)
			}
			eventID, taskID, promptID := uuid.NewString(), uuid.NewString(), uuid.NewString()
			blueprintID, runID := uuid.NewString(), uuid.NewString()
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES ($1, $2, $3, 'github:pr:opened', '', '{}'::jsonb, now())
			`, eventID, orgID, entity.ID)
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key,
				                   primary_event_id, status)
				VALUES ($1, $2, $3, $4, 'team', $5, 'github:pr:opened', '', $6, 'in_progress')
			`, taskID, orgID, delegator, teamID, entity.ID, eventID)
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools)
				VALUES ($1, $2, $3, $4, 'P', 'body', 'user', '')
			`, promptID, orgID, delegator, teamID)
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO blueprints (id, org_id, team_id, creator_user_id, name, source)
				VALUES ($1, $2, $3, $4, 'BP', 'user')
			`, blueprintID, orgID, teamID, delegator)
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO blueprint_runs (id, org_id, team_id, creator_user_id, blueprint_id, task_id, trigger_type,
				                            status, worktree_path, step_plan)
				VALUES ($1, $2, $3, $4, $5, $6, 'manual', 'running', '/tmp/wt', '[]')
			`, runID, orgID, teamID, delegator, blueprintID, taskID)
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO conversations (id, org_id, team_id, visibility, creator_user_id, task_id, prompt_id, trigger_type,
				                           status, completed_at, blueprint_run_id, blueprint_step_index)
				VALUES ($1, $2, $3, 'team', $4, $5, $6, 'manual', 'open', now(), $7, 0)
			`, uuid.NewString(), orgID, teamID, delegator, taskID, promptID, runID)

			pgtest.MustExec(t, h.AdminDB, tc.demote, delegator, teamID)

			s.terminateBlueprint(orgID, runID, taskID, time.Now(),
				runConfig{orgID: orgID}, domain.BlueprintRunStatusCompleted, "", nil, true)

			var runStatus, taskStatus, closeReason string
			if err := h.AdminDB.QueryRow(`SELECT status FROM blueprint_runs WHERE id = $1`, runID).Scan(&runStatus); err != nil {
				t.Fatalf("read the run: %v", err)
			}
			if err := h.AdminDB.QueryRow(`SELECT status, COALESCE(close_reason, '') FROM tasks WHERE id = $1`, taskID).Scan(&taskStatus, &closeReason); err != nil {
				t.Fatalf("read the task: %v", err)
			}
			if runStatus != string(domain.BlueprintRunStatusCompleted) {
				t.Errorf("run status = %q, want completed", runStatus)
			}
			if taskStatus != "done" || closeReason != db.RunCompletedCloseReason {
				t.Errorf("task = (%s, %q), want (done, %q)", taskStatus, closeReason, db.RunCompletedCloseReason)
			}
		})
	}
}
