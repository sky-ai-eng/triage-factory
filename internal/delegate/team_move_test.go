package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestReactor_CancelsARunWhoseTaskLeftItsTeam: the task moved to another team
// after the run's step had concluded, so the door that moved it had no live
// step to stop. The reactor cancels the run instead of advancing it onto the
// new team or completing it, which would close the task under its new holder.
// The step ends as reassigned and its memory is rung for.
func TestReactor_CancelsARunWhoseTaskLeftItsTeam(t *testing.T) {
	for _, tc := range []struct {
		name   string
		nSteps int
	}{
		{"instead_of_advancing", 2},
		{"instead_of_completing", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			org := runmode.LocalDefaultOrgID
			s, database, brID, taskID, step0 := reactorFixture(t, "team-move-"+tc.name, tc.nSteps, dbtest.SeedConcluded, "continue")
			d := &doorbell{}
			s.SetOnMemoryOwed(d.ring)
			taskStatusBefore := taskStatus(t, database, taskID)
			// tasks.team_id carries no foreign key in SQLite, so any other id
			// is another team.
			if _, err := database.Exec(`UPDATE tasks SET team_id = '00000000-0000-0000-0000-0000000000b2' WHERE id = ?`, taskID); err != nil {
				t.Fatalf("move the task to another team: %v", err)
			}

			stepConversation := loadConversation(t, s, step0)
			stepConversation.TriggerType = "manual"
			stepConversation.CreatorUserID = runmode.LocalDefaultUserID
			s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

			run := mustGetRun(t, s, org, brID)
			if run.Status != domain.BlueprintRunStatusCancelled {
				t.Errorf("run status = %q, want cancelled", run.Status)
			}
			if run.CurrentStepIndex != 0 {
				t.Errorf("current_step_index = %d, want 0", run.CurrentStepIndex)
			}
			if got := queuedStepConversations(t, database, brID); len(got) != 0 {
				t.Errorf("queued steps = %v, want none", got)
			}
			if got := taskStatus(t, database, taskID); got != taskStatusBefore {
				t.Errorf("task status = %q, want %q — the run's end must leave the new team's task alone", got, taskStatusBefore)
			}
			assertEnded(t, database, step0, domain.EndedReassigned)
			d.assertRangOnceFor(t, org, step0)
		})
	}
}
