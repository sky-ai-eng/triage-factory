package delegate

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestReactor_CancelsARunWhoseTaskWasTakenOffIt: the run's step had already
// concluded when its task was taken off the run, by a move to another team or
// a person's claim on the same team, so the claim or the move had no live step
// to stop. The reactor cancels the run instead of advancing it onto the task
// or completing it, which would close the task under its new holder. The step
// ends with the boundary that took the task, and its memory is rung for once,
// by whichever wrote that boundary.
func TestReactor_CancelsARunWhoseTaskWasTakenOffIt(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	// tasks.team_id carries no foreign key in SQLite, so any other id is
	// another team.
	moveToAnotherTeam := func(t *testing.T, s *Spawner, database *sql.DB, taskID, _ string) {
		if _, err := database.Exec(`UPDATE tasks SET team_id = '00000000-0000-0000-0000-0000000000b2' WHERE id = ?`, taskID); err != nil {
			t.Fatalf("move the task to another team: %v", err)
		}
	}
	claimForAPerson := func(t *testing.T, s *Spawner, database *sql.DB, taskID, _ string) {
		tasks := sqlitestore.New(database).Tasks
		if ok, err := tasks.ClaimQueuedForUser(context.Background(), org, taskID, runmode.LocalDefaultUserID, dbtest.TaskVersion(t, tasks, org, taskID)); err != nil || !ok {
			t.Fatalf("claim the task for a person: (%v, %v)", ok, err)
		}
	}
	for _, tc := range []struct {
		name   string
		nSteps int
		take   func(t *testing.T, s *Spawner, database *sql.DB, taskID, step0 string)
		want   domain.EndedReason
		// rings is the reactor's own memory rings: none when the claim had
		// already ended the step and rung for it.
		rings int
	}{
		{"moved_instead_of_advancing", 2, moveToAnotherTeam, domain.EndedReassigned, 1},
		{"moved_instead_of_completing", 1, moveToAnotherTeam, domain.EndedReassigned, 1},
		{"claimed_instead_of_advancing", 2, claimForAPerson, domain.EndedTakenOver, 1},
		{"claimed_instead_of_completing", 1, claimForAPerson, domain.EndedTakenOver, 1},
		{"taken_over_after_the_claim_ended_the_step", 2, func(t *testing.T, s *Spawner, database *sql.DB, taskID, step0 string) {
			claimForAPerson(t, s, database, taskID, step0)
			if _, err := s.conversations.EndConversationSystem(context.Background(), org, step0, domain.EndedTakenOver); err != nil {
				t.Fatalf("end the step as the takeover does: %v", err)
			}
		}, domain.EndedTakenOver, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, database, brID, taskID, step0 := reactorFixture(t, "taken-"+tc.name, tc.nSteps, dbtest.SeedConcluded, "continue")
			d := &doorbell{}
			s.SetOnMemoryOwed(d.ring)
			tc.take(t, s, database, taskID, step0)
			taskStatusBefore := taskStatus(t, database, taskID)

			stepConversation := loadConversation(t, s, step0)
			stepConversation.TriggerType = "manual"
			stepConversation.CreatorUserID = runmode.LocalDefaultUserID
			s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

			run := mustGetRun(t, s, org, brID)
			if run.Status != domain.BlueprintRunStatusCancelled {
				t.Errorf("run status = %q, want cancelled", run.Status)
			}
			if !run.CancelRequested {
				t.Error("cancel_requested = false, want true — recovery ends a called-off run if its terminal write fails")
			}
			if run.CurrentStepIndex != 0 {
				t.Errorf("current_step_index = %d, want 0", run.CurrentStepIndex)
			}
			if got := queuedStepConversations(t, database, brID); len(got) != 0 {
				t.Errorf("queued steps = %v, want none", got)
			}
			if got := taskStatus(t, database, taskID); got != taskStatusBefore {
				t.Errorf("task status = %q, want %q — the run's end must leave the new holder's task alone", got, taskStatusBefore)
			}
			assertEnded(t, database, step0, tc.want)
			if got := d.taken(); len(got) != tc.rings {
				t.Errorf("memory rings = %v, want %d", got, tc.rings)
			}
		})
	}
}
