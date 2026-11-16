package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestBlueprintStore_Postgres_TeamGuardWaitsForAMoveInFlight: an advance and
// a completion read the task's team in their guard while a move to another
// team is still uncommitted. Each share-locks the task first, so it waits for
// the move and then refuses. Without the lock it would read the team the move
// has not yet replaced, and go ahead for the old team on the new team's task.
func TestBlueprintStore_Postgres_TeamGuardWaitsForAMoveInFlight(t *testing.T) {
	h := pgtest.Shared(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		write func(store db.BlueprintStore, sc dbtest.BlueprintSequenceScaffold, br domain.BlueprintRun, step0ID string) (bool, error)
	}{
		{"advance", func(store db.BlueprintStore, sc dbtest.BlueprintSequenceScaffold, br domain.BlueprintRun, step0ID string) (bool, error) {
			advanced, _, _, err := store.AdvanceRunToStepSystem(ctx, sc.OrgID, 0, step0ID, sc.Step(br, 1))
			return advanced, err
		}},
		{"completion", func(store db.BlueprintStore, sc dbtest.BlueprintSequenceScaffold, br domain.BlueprintRun, _ string) (bool, error) {
			return store.MarkRunStatusSystem(ctx, sc.OrgID, br.ID, domain.BlueprintRunStatusCompleted, "", nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, sc := pgSequenceScaffold(t, h)
			taskID := sc.NewTask(t)
			br := sc.Firing(t, taskID)
			_, _, step0, _, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br, db.AgentClaimStamp{}, "", sc.Step(br, 0))
			if err != nil || step0 == nil {
				t.Fatalf("seed a committed firing: (%v, %v)", step0, err)
			}

			// The move, held open. Each task is seeded on a team of its own,
			// so the acting team is another one.
			move, err := h.AdminDB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin the move: %v", err)
			}
			defer func() { _ = move.Rollback() }()
			if _, err := move.ExecContext(ctx, `UPDATE tasks SET team_id = $2 WHERE id = $1`, taskID, sc.ConsolidateTeamID); err != nil {
				t.Fatalf("move the task: %v", err)
			}

			type result struct {
				ok  bool
				err error
			}
			done := make(chan result, 1)
			go func() {
				ok, err := tc.write(store, sc, br, step0.ID)
				done <- result{ok, err}
			}()

			// Until the write waits on the move's lock. It is the only other
			// session on this database, so any lock wait is its.
			deadline := time.Now().Add(10 * time.Second)
			for {
				select {
				case r := <-done:
					t.Fatalf("the write finished while the move was in flight: (ok=%v, err=%v), want it to wait for the move", r.ok, r.err)
				default:
				}
				var waiting int
				if err := h.AdminDB.QueryRowContext(ctx, `
					SELECT count(*) FROM pg_stat_activity
					WHERE wait_event_type = 'Lock' AND datname = current_database()
				`).Scan(&waiting); err != nil {
					t.Fatalf("read pg_stat_activity: %v", err)
				}
				if waiting > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the write neither finished nor waited on a lock")
				}
				time.Sleep(20 * time.Millisecond)
			}

			if err := move.Commit(); err != nil {
				t.Fatalf("commit the move: %v", err)
			}
			select {
			case r := <-done:
				if r.err != nil || r.ok {
					t.Fatalf("the write after the move committed = (ok=%v, err=%v), want a refusal", r.ok, r.err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the write never returned after the move committed")
			}
		})
	}
}
