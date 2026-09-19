package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestBlueprintStore_SQLite_Sequence runs the shared firing conformance against
// the SQLite impl. Local mode is N=1 — one org, one team — so the owner
// consolidation writes the team it already holds and conversations take the
// team sentinel rather than deriving one; the indivisibility the suite is
// actually about is the same on both dialects, which is why it is shared.
func TestBlueprintStore_SQLite_Sequence(t *testing.T) {
	dbtest.RunBlueprintSequenceConformance(t, func(t *testing.T) (db.BlueprintStore, dbtest.BlueprintSequenceScaffold) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		ctx := context.Background()
		org := runmode.LocalDefaultOrgID

		agentID, err := stores.Agents.Create(ctx, org, domain.Agent{DisplayName: "Firing Bot"})
		if err != nil {
			t.Fatalf("Agents.Create: %v", err)
		}
		promptID := "firing-p0"
		insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: promptID, Name: "Step 0", Body: "b", Source: "user"})

		n := 0
		// Each firing brings its own blueprint and trigger:
		// event_handlers_one_trigger_per_blueprint means a second trigger needs
		// a second blueprint, and the suite stages several firings per test.
		newFiring := func(t *testing.T, taskID string, manual bool) domain.BlueprintRun {
			t.Helper()
			n++
			suffix := fmt.Sprintf("firing-%d", n)
			bpID := "bp-" + suffix
			// No blueprint_steps row: blueprint_runs carries its plan as JSON
			// and takes no FK on the step table, and one prompt may be the step
			// of only one blueprint — so seeding steps here would cost a prompt
			// per firing to prove nothing this suite is about.
			insertBlueprintForTest(t, conn, bpID, "Firing BP "+suffix)
			br := domain.BlueprintRun{
				ID: uuid.New().String(), BlueprintID: bpID, TaskID: taskID,
				Status: domain.BlueprintRunStatusRunning, WorktreePath: "/tmp/wt-" + suffix,
			}
			if manual {
				br.TriggerType = domain.BlueprintTriggerManual
				return br
			}
			triggerID := "trig-" + suffix
			if _, err := conn.Exec(`
				INSERT INTO event_handlers (id, kind, event_type, blueprint_id, breaker_threshold, min_autonomy_suitability, enabled, source, creator_user_id, team_id)
				VALUES (?, 'trigger', ?, ?, 4, 0, 1, 'user', ?, ?)
			`, triggerID, domain.EventGitHubPRCICheckFailed, bpID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
				t.Fatalf("seed trigger: %v", err)
			}
			var eventID string
			if err := conn.QueryRow(`SELECT primary_event_id FROM tasks WHERE id = ?`, taskID).Scan(&eventID); err != nil {
				t.Fatalf("read task event id: %v", err)
			}
			br.TriggerType = domain.BlueprintTriggerEvent
			br.TriggerID = triggerID
			br.TriggeringEventID = eventID
			return br
		}
		countOn := func(t *testing.T, query, taskID string) int {
			t.Helper()
			var got int
			if err := conn.QueryRow(query, taskID).Scan(&got); err != nil {
				t.Fatalf("count: %v", err)
			}
			return got
		}

		return stores.Blueprints, dbtest.BlueprintSequenceScaffold{
			OrgID:   org,
			AgentID: agentID,
			// One team in local mode, so this is the team the task already
			// owns: the write happens, the card does not move.
			ConsolidateTeamID: runmode.LocalDefaultTeamID,
			// tasks.team_id carries no foreign key here, so no value is
			// refusable and the abort arm has nothing to fail on.
			BadTeamID: "",
			NewTask: func(t *testing.T) string {
				t.Helper()
				n++
				return seedEntityEventTask(t, conn, fmt.Sprintf("firing-task-%d", n)).ID
			},
			Firing:       func(t *testing.T, taskID string) domain.BlueprintRun { return newFiring(t, taskID, false) },
			ManualFiring: func(t *testing.T, taskID string) domain.BlueprintRun { return newFiring(t, taskID, true) },
			Step: func(br domain.BlueprintRun, stepIndex int) domain.Conversation {
				return domain.Conversation{
					ID: uuid.New().String(), TaskID: br.TaskID, PromptID: promptID, Model: "m",
					TriggerType: string(br.TriggerType), TriggerID: br.TriggerID,
					BlueprintRunID: br.ID, BlueprintStepIndex: &stepIndex,
				}
			},
			ClaimTaskForUser: func(t *testing.T, taskID string) {
				t.Helper()
				if _, err := stores.Tasks.SetClaimedByUser(ctx, org, taskID, runmode.LocalDefaultUserID); err != nil {
					t.Fatalf("SetClaimedByUser: %v", err)
				}
			},
			RunCount: func(t *testing.T, taskID string) int {
				return countOn(t, `SELECT count(*) FROM blueprint_runs WHERE task_id = ?`, taskID)
			},
			ConversationCount: func(t *testing.T, taskID string) int {
				return countOn(t, `SELECT count(*) FROM conversations WHERE task_id = ?`, taskID)
			},
			TaskOwnerTeam: func(t *testing.T, taskID string) string {
				t.Helper()
				var team sql.NullString
				if err := conn.QueryRow(`SELECT team_id FROM tasks WHERE id = ?`, taskID).Scan(&team); err != nil {
					t.Fatalf("read task team_id: %v", err)
				}
				return team.String
			},
			TaskAgentClaim: func(t *testing.T, taskID string) string {
				t.Helper()
				var agent sql.NullString
				if err := conn.QueryRow(`SELECT claimed_by_agent_id FROM tasks WHERE id = ?`, taskID).Scan(&agent); err != nil {
					t.Fatalf("read task claim: %v", err)
				}
				return agent.String
			},
			RunCurrentStep: func(t *testing.T, blueprintRunID string) int {
				t.Helper()
				var idx int
				if err := conn.QueryRow(`SELECT current_step_index FROM blueprint_runs WHERE id = ?`, blueprintRunID).Scan(&idx); err != nil {
					t.Fatalf("read current_step_index: %v", err)
				}
				return idx
			},
			ConversationEnded: func(t *testing.T, convID string) bool {
				t.Helper()
				var endedAt sql.NullString
				if err := conn.QueryRow(`SELECT ended_at FROM conversations WHERE id = ?`, convID).Scan(&endedAt); err != nil {
					t.Fatalf("read ended_at: %v", err)
				}
				return endedAt.Valid
			},
			// Local mode stamps every conversation with the team sentinel
			// rather than deriving one from the task, so the assertion would
			// prove nothing here. The Postgres arm carries it.
			ConversationTeam: nil,
		}
	})
}
