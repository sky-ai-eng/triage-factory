package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestBlueprintStore_Postgres_ManualFiringNeedsACreator pins the one check the
// manual arm lost when it moved from the app pool to the admin pool: the
// blueprint_runs_insert WITH CHECK that held creator_user_id to
// tf.current_user_id(). There is no session identity to read on the admin
// pool, so the creator has to arrive on the call — and a firing that names
// none is refused rather than attributed to the org owner, which would put
// somebody's run on a stranger's reads with nothing to notice it by.
func TestBlueprintStore_Postgres_ManualFiringNeedsACreator(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	orgID, userID := seedPgOrgForBlueprints(t, h)
	taskID := seedPgTask(t, h, orgID, userID)
	bpID := "bp-nocreator"
	seedPgBlueprint(t, h, orgID, userID, bpID)
	promptID := "nocreator-p0"
	seedPgPrompt(t, h, orgID, userID, promptID)

	step0 := 0
	br := domain.BlueprintRun{
		ID: uuid.New().String(), BlueprintID: bpID, TaskID: taskID,
		TriggerType: domain.BlueprintTriggerManual,
		Status:      domain.BlueprintRunStatusRunning,
	}
	for _, tc := range []struct{ name, creator string }{
		{"absent", ""},
		{"the local sentinel, which has no users row in multi", runmode.LocalDefaultUserID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := domain.Conversation{
				ID: uuid.New().String(), TaskID: taskID, PromptID: promptID, Model: "m",
				TriggerType: "manual", CreatorUserID: tc.creator,
				BlueprintRunID: br.ID, BlueprintStepIndex: &step0,
			}
			_, _, _, err := stores.Blueprints.CreateRunWithFirstStepSystem(ctx, orgID, br, db.AgentClaimStamp{}, "", step)
			if !errors.Is(err, db.ErrManualCreatorRequired) {
				t.Fatalf("CreateRunWithFirstStepSystem = %v, want db.ErrManualCreatorRequired", err)
			}
			var runs int
			if err := h.AdminDB.QueryRow(`SELECT count(*) FROM blueprint_runs WHERE task_id = $1`, taskID).Scan(&runs); err != nil {
				t.Fatalf("count runs: %v", err)
			}
			if runs != 0 {
				t.Errorf("blueprint_runs on the task = %d, want 0", runs)
			}
		})
	}
}

// TestBlueprintStore_Postgres_Sequence runs the shared firing conformance
// against the Postgres impl. This is the arm where the owner consolidation has
// something to prove — the org has two teams, so a firing by the one that is
// not the task's owner moves the card and its step conversation follows.
func TestBlueprintStore_Postgres_Sequence(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunBlueprintSequenceConformance(t, func(t *testing.T) (db.BlueprintStore, dbtest.BlueprintSequenceScaffold) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		ctx := context.Background()
		orgID, userID := seedPgOrgForBlueprints(t, h)
		// Every seeded task lands on a team of its own, and the firing
		// consolidates onto this one — so the suite's owner assertions are
		// about a card that actually moves.
		actingTeamID := seedPgDefaultTeam(t, h, orgID, userID)

		agentID := uuid.New().String()
		pgtest.MustExec(t, h.AdminDB,
			`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Firing Bot')`, agentID, orgID)
		promptID := "firing-p0"
		seedPgPrompt(t, h, orgID, userID, promptID)

		n := 0
		// Each firing brings its own blueprint and trigger:
		// event_handlers_one_trigger_per_blueprint means a second trigger needs
		// a second blueprint, and the suite stages several firings per test.
		newFiring := func(t *testing.T, taskID string, manual bool) domain.BlueprintRun {
			t.Helper()
			n++
			suffix := fmt.Sprintf("firing-%d", n)
			bpID := "bp-" + suffix
			seedPgBlueprint(t, h, orgID, userID, bpID)
			br := domain.BlueprintRun{
				ID: uuid.New().String(), BlueprintID: bpID, TaskID: taskID,
				Status: domain.BlueprintRunStatusRunning, WorktreePath: "/tmp/wt-" + suffix,
				StepPlan: []domain.BlueprintPlanStep{{StepIndex: 0, PromptID: promptID, PromptName: "p", PromptBody: "b"}},
			}
			if manual {
				br.TriggerType = domain.BlueprintTriggerManual
				return br
			}
			triggerID := uuid.New().String()
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO event_handlers (id, org_id, team_id, creator_user_id, kind, event_type, enabled, source, blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
				VALUES ($1, $2, $3, $4, 'trigger', $5, true, 'user', $6, 4, 0, now(), now())
			`, triggerID, orgID, ensurePgTeamForOrg(t, h, orgID, userID), userID, domain.EventGitHubPRCICheckFailed, bpID)
			eventID := uuid.New().String()
			var entityID string
			if err := h.AdminDB.QueryRow(`SELECT entity_id FROM tasks WHERE id = $1`, taskID).Scan(&entityID); err != nil {
				t.Fatalf("read task entity: %v", err)
			}
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
			`, eventID, orgID, entityID, domain.EventGitHubPRCICheckFailed)
			br.TriggerType = domain.BlueprintTriggerEvent
			br.TriggerID = triggerID
			br.TriggeringEventID = eventID
			return br
		}
		countOn := func(t *testing.T, query, taskID string) int {
			t.Helper()
			var got int
			if err := h.AdminDB.QueryRow(query, taskID).Scan(&got); err != nil {
				t.Fatalf("count: %v", err)
			}
			return got
		}

		return stores.Blueprints, dbtest.BlueprintSequenceScaffold{
			OrgID:             orgID,
			AgentID:           agentID,
			ConsolidateTeamID: actingTeamID,
			// A syntactically valid uuid naming no team: tasks.team_id carries
			// a foreign key, so the consolidation refuses and takes the firing
			// with it.
			BadTeamID: uuid.New().String(),
			NewTask: func(t *testing.T) string {
				t.Helper()
				return seedPgTask(t, h, orgID, userID)
			},
			Firing:       func(t *testing.T, taskID string) domain.BlueprintRun { return newFiring(t, taskID, false) },
			ManualFiring: func(t *testing.T, taskID string) domain.BlueprintRun { return newFiring(t, taskID, true) },
			Step: func(br domain.BlueprintRun, stepIndex int) domain.Conversation {
				return domain.Conversation{
					ID: uuid.New().String(), TaskID: br.TaskID, PromptID: promptID, Model: "m",
					TriggerType: string(br.TriggerType), TriggerID: br.TriggerID, CreatorUserID: userID,
					BlueprintRunID: br.ID, BlueprintStepIndex: &stepIndex,
				}
			},
			ClaimTaskForUser: func(t *testing.T, taskID string) {
				t.Helper()
				if _, err := stores.Tasks.SetClaimedByUser(ctx, orgID, taskID, userID); err != nil {
					t.Fatalf("SetClaimedByUser: %v", err)
				}
			},
			RunCount: func(t *testing.T, taskID string) int {
				return countOn(t, `SELECT count(*) FROM blueprint_runs WHERE task_id = $1`, taskID)
			},
			ConversationCount: func(t *testing.T, taskID string) int {
				return countOn(t, `SELECT count(*) FROM conversations WHERE task_id = $1`, taskID)
			},
			TaskOwnerTeam: func(t *testing.T, taskID string) string {
				t.Helper()
				var team sql.NullString
				if err := h.AdminDB.QueryRow(`SELECT team_id::text FROM tasks WHERE id = $1`, taskID).Scan(&team); err != nil {
					t.Fatalf("read task team_id: %v", err)
				}
				return team.String
			},
			TaskAgentClaim: func(t *testing.T, taskID string) string {
				t.Helper()
				var agent sql.NullString
				if err := h.AdminDB.QueryRow(`SELECT claimed_by_agent_id::text FROM tasks WHERE id = $1`, taskID).Scan(&agent); err != nil {
					t.Fatalf("read task claim: %v", err)
				}
				return agent.String
			},
			RunCurrentStep: func(t *testing.T, blueprintRunID string) int {
				t.Helper()
				var idx int
				if err := h.AdminDB.QueryRow(`SELECT current_step_index FROM blueprint_runs WHERE id = $1`, blueprintRunID).Scan(&idx); err != nil {
					t.Fatalf("read current_step_index: %v", err)
				}
				return idx
			},
			ConversationEnded: func(t *testing.T, convID string) bool {
				t.Helper()
				var endedAt sql.NullTime
				if err := h.AdminDB.QueryRow(`SELECT ended_at FROM conversations WHERE id = $1`, convID).Scan(&endedAt); err != nil {
					t.Fatalf("read ended_at: %v", err)
				}
				return endedAt.Valid
			},
			ConversationTeam: func(t *testing.T, convID string) string {
				t.Helper()
				var team sql.NullString
				if err := h.AdminDB.QueryRow(`SELECT team_id::text FROM conversations WHERE id = $1`, convID).Scan(&team); err != nil {
					t.Fatalf("read conversation team_id: %v", err)
				}
				return team.String
			},
		}
	})
}
