// Package dbtest holds the conformance harness: shared test bodies
// that exercise each store interface against any conforming
// implementation. The same RunXxxConformance function is invoked by
// internal/db/sqlite/<resource>_test.go and
// internal/db/postgres/<resource>_test.go, so SQLite and Postgres
// run identical assertions and any drift between them fails one of
// the two test files immediately.
//
// Per-backend test files own setup (opening the connection, picking
// the right orgID); the conformance function owns the assertions.
package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ScoreStoreFactory is what a per-backend test file hands to
// RunScoreStoreConformance. It is called once per subtest and returns a
// fresh ScoreFixture: the wired stores, the orgID to pass to every method,
// and the schema-aware probes the harness itself cannot write.
type ScoreStoreFactory func(t *testing.T) ScoreFixture

// ScoreSeeder lets the conformance harness ask the backend test to
// create N queued/pending tasks and return their IDs. Backend tests
// implement this against whatever raw-SQL path matches their schema
// (the conformance harness is intentionally schema-blind).
type ScoreSeeder func(t *testing.T, n int) []string

// ScoreFixture is one subtest's world for the score store suite.
type ScoreFixture struct {
	Store db.ScoreStore
	OrgID string
	Seed  ScoreSeeder
	// ReDerive is the re-evaluation queue the score write admits into, so
	// the suite can drive a row through leased, parked and done and watch
	// what the next score write does to it.
	ReDerive db.TaskReDeriveStore
	// ScoreRevision reads tasks.score_revision for one task.
	ScoreRevision func(t *testing.T, taskID string) int64
	// QueueRows reads the task's task_rederive_queue rows, oldest first.
	QueueRows func(t *testing.T, taskID string) []ReDeriveQueueRow
}

// ReDeriveQueueRow is one task_rederive_queue row as the suite reads it.
type ReDeriveQueueRow struct {
	ID                int64
	Status            string
	Attempt           int
	RequestedRevision int64
	UniqueKey         string
}

// scoreOwner is the owner every claim in this suite stamps a row with.
var scoreOwner = workitem.Owner{ID: "conformance-score-suite", Epoch: 1}

// RunScoreStoreConformance is the shared assertion suite for any
// db.ScoreStore implementation. Backend tests invoke it with their
// factory; both backends run the same subtests.
func RunScoreStoreConformance(t *testing.T, mk ScoreStoreFactory) {
	t.Helper()
	ctx := context.Background()

	score := func(t *testing.T, f ScoreFixture, ids ...string) {
		t.Helper()
		updates := make([]domain.TaskScoreUpdate, len(ids))
		for i, id := range ids {
			updates[i] = domain.TaskScoreUpdate{ID: id, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "s", PriorityReasoning: "r"}
		}
		if err := f.Store.UpdateTaskScores(ctx, f.OrgID, updates); err != nil {
			t.Fatalf("UpdateTaskScores: %v", err)
		}
	}
	oneRow := func(t *testing.T, f ScoreFixture, taskID string) ReDeriveQueueRow {
		t.Helper()
		rows := f.QueueRows(t, taskID)
		if len(rows) != 1 {
			t.Fatalf("task %s has %d task_rederive_queue rows, want 1: %+v", taskID, len(rows), rows)
		}
		return rows[0]
	}
	requireRow := func(t *testing.T, got ReDeriveQueueRow, taskID, status string, revision int64) {
		t.Helper()
		if got.Status != status || got.RequestedRevision != revision {
			t.Fatalf("queue row = %+v, want status %s at requested_revision %d", got, status, revision)
		}
		if got.UniqueKey != taskID {
			t.Errorf("queue row unique_key = %q, want the task id %q", got.UniqueKey, taskID)
		}
	}
	claimOne := func(t *testing.T, f ScoreFixture) db.ClaimedReDerive {
		t.Helper()
		batch, err := f.ReDerive.Claim(ctx, scoreOwner, 1)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(batch.Items) != 1 {
			t.Fatalf("Claim returned %d items, want 1 (cancelled=%d parked=%d)", len(batch.Items), batch.Cancelled, batch.Parked)
		}
		return batch.Items[0]
	}

	t.Run("UnscoredTasks_returns_only_pending_queued", func(t *testing.T) {
		f := mk(t)
		ids := f.Seed(t, 3)
		tasks, err := f.Store.UnscoredTasks(ctx, f.OrgID)
		if err != nil {
			t.Fatalf("UnscoredTasks: %v", err)
		}
		if len(tasks) != len(ids) {
			t.Fatalf("UnscoredTasks: got %d rows, want %d", len(tasks), len(ids))
		}
		for _, task := range tasks {
			if task.ScoringStatus != "pending" {
				t.Errorf("task %s: scoring_status=%q, want pending", task.ID, task.ScoringStatus)
			}
			if task.Status != "queued" {
				t.Errorf("task %s: status=%q, want queued", task.ID, task.Status)
			}
		}
	})

	t.Run("MarkScoring_flips_to_in_progress", func(t *testing.T) {
		f := mk(t)
		ids := f.Seed(t, 2)
		if err := f.Store.MarkScoring(ctx, f.OrgID, ids); err != nil {
			t.Fatalf("MarkScoring: %v", err)
		}
		// UnscoredTasks only picks up scoring_status='pending', so a
		// re-read after MarkScoring should now exclude these rows.
		tasks, err := f.Store.UnscoredTasks(ctx, f.OrgID)
		if err != nil {
			t.Fatalf("UnscoredTasks after MarkScoring: %v", err)
		}
		for _, task := range tasks {
			for _, id := range ids {
				if task.ID == id {
					t.Errorf("task %s still listed as unscored after MarkScoring", id)
				}
			}
		}
	})

	t.Run("ResetScoringToPending_restores_visibility", func(t *testing.T) {
		f := mk(t)
		ids := f.Seed(t, 2)
		if err := f.Store.MarkScoring(ctx, f.OrgID, ids); err != nil {
			t.Fatalf("MarkScoring: %v", err)
		}
		if err := f.Store.ResetScoringToPending(ctx, f.OrgID, ids); err != nil {
			t.Fatalf("ResetScoringToPending: %v", err)
		}
		tasks, err := f.Store.UnscoredTasks(ctx, f.OrgID)
		if err != nil {
			t.Fatalf("UnscoredTasks after Reset: %v", err)
		}
		seen := map[string]bool{}
		for _, task := range tasks {
			seen[task.ID] = true
		}
		for _, id := range ids {
			if !seen[id] {
				t.Errorf("task %s missing from UnscoredTasks after reset", id)
			}
		}
	})

	t.Run("ResetStaleScoring_recovers_crash_residue_only", func(t *testing.T) {
		f := mk(t)
		residue := f.Seed(t, 2)   // marked in_progress, then "the process died"
		untouched := f.Seed(t, 1) // still pending — never picked
		done := f.Seed(t, 1)      // scored — a completed cycle's output

		if err := f.Store.MarkScoring(ctx, f.OrgID, residue); err != nil {
			t.Fatalf("MarkScoring: %v", err)
		}
		if err := f.Store.UpdateTaskScores(ctx, f.OrgID, []domain.TaskScoreUpdate{{
			ID:                  done[0],
			PriorityScore:       0.7,
			AutonomySuitability: 0.7,
			Summary:             "already scored",
			PriorityReasoning:   "already scored",
		}}); err != nil {
			t.Fatalf("UpdateTaskScores: %v", err)
		}

		n, err := f.Store.ResetStaleScoring(ctx, f.OrgID)
		if err != nil {
			t.Fatalf("ResetStaleScoring: %v", err)
		}
		if n != len(residue) {
			t.Errorf("ResetStaleScoring returned %d, want %d (only the in_progress rows)", n, len(residue))
		}

		unscored := map[string]bool{}
		tasks, err := f.Store.UnscoredTasks(ctx, f.OrgID)
		if err != nil {
			t.Fatalf("UnscoredTasks after ResetStaleScoring: %v", err)
		}
		for _, task := range tasks {
			unscored[task.ID] = true
		}
		for _, id := range residue {
			if !unscored[id] {
				t.Errorf("task %s still invisible to UnscoredTasks after ResetStaleScoring", id)
			}
		}
		if !unscored[untouched[0]] {
			t.Errorf("pending task %s dropped out of UnscoredTasks", untouched[0])
		}
		if unscored[done[0]] {
			t.Errorf("scored task %s resurfaced in UnscoredTasks; ResetStaleScoring must not touch 'scored'", done[0])
		}

		// Idempotent: a second call has nothing left to move, which is
		// also what every crash-free cycle sees.
		again, err := f.Store.ResetStaleScoring(ctx, f.OrgID)
		if err != nil {
			t.Fatalf("ResetStaleScoring (second call): %v", err)
		}
		if again != 0 {
			t.Errorf("second ResetStaleScoring returned %d, want 0", again)
		}
	})

	t.Run("UpdateTaskScores_applies_scores_and_marks_scored", func(t *testing.T) {
		f := mk(t)
		ids := f.Seed(t, 2)
		updates := make([]domain.TaskScoreUpdate, len(ids))
		for i, id := range ids {
			updates[i] = domain.TaskScoreUpdate{
				ID:                  id,
				PriorityScore:       float64(i+1) * 0.25,
				AutonomySuitability: float64(i+1) * 0.10,
				Summary:             "ai summary " + id,
				PriorityReasoning:   "priority reason " + id,
			}
		}
		if err := f.Store.UpdateTaskScores(ctx, f.OrgID, updates); err != nil {
			t.Fatalf("UpdateTaskScores: %v", err)
		}
		// After UpdateTaskScores, rows should drop out of UnscoredTasks.
		tasks, err := f.Store.UnscoredTasks(ctx, f.OrgID)
		if err != nil {
			t.Fatalf("UnscoredTasks after UpdateTaskScores: %v", err)
		}
		for _, task := range tasks {
			for _, id := range ids {
				if task.ID == id {
					t.Errorf("task %s still listed as unscored after UpdateTaskScores", id)
				}
			}
		}
	})

	// The score write admits the re-evaluation obligation in its own
	// transaction: one ready row keyed on the task, at the revision the tasks
	// row now carries. A second save raises both in place.
	t.Run("UpdateTaskScores_admits_a_rederive_at_the_new_revision", func(t *testing.T) {
		f := mk(t)
		ids := f.Seed(t, 2)
		for _, id := range ids {
			if rows := f.QueueRows(t, id); len(rows) != 0 {
				t.Fatalf("unscored task %s already has queue rows %+v", id, rows)
			}
			if rev := f.ScoreRevision(t, id); rev != 0 {
				t.Fatalf("unscored task %s has score_revision %d, want 0", id, rev)
			}
		}

		score(t, f, ids...)
		for _, id := range ids {
			requireRow(t, oneRow(t, f, id), id, workitem.StatusReady, 1)
			if rev := f.ScoreRevision(t, id); rev != 1 {
				t.Errorf("task %s score_revision = %d after one save, want 1", id, rev)
			}
		}

		first := oneRow(t, f, ids[0])
		score(t, f, ids[0])
		second := oneRow(t, f, ids[0])
		if second.ID != first.ID {
			t.Errorf("second save minted row %d beside the ready row %d; it must raise the ready row in place", second.ID, first.ID)
		}
		requireRow(t, second, ids[0], workitem.StatusReady, 2)
		if rev := f.ScoreRevision(t, ids[0]); rev != 2 {
			t.Errorf("score_revision = %d after two saves, want 2", rev)
		}
		// The sibling was not touched by a save that did not name it.
		requireRow(t, oneRow(t, f, ids[1]), ids[1], workitem.StatusReady, 1)
	})

	// A batch that names one task twice is one save of that task: the last
	// update wins, and the revision moves once — on the task and on the
	// queue row alike — so the two cannot drift apart.
	t.Run("UpdateTaskScores_applies_a_repeated_id_once", func(t *testing.T) {
		f := mk(t)
		id := f.Seed(t, 1)[0]
		if err := f.Store.UpdateTaskScores(ctx, f.OrgID, []domain.TaskScoreUpdate{
			{ID: id, PriorityScore: 0.1, AutonomySuitability: 0.2, Summary: "first", PriorityReasoning: "r"},
			{ID: id, PriorityScore: 0.9, AutonomySuitability: 0.8, Summary: "last", PriorityReasoning: "r"},
		}); err != nil {
			t.Fatalf("UpdateTaskScores: %v", err)
		}
		requireRow(t, oneRow(t, f, id), id, workitem.StatusReady, 1)
		if rev := f.ScoreRevision(t, id); rev != 1 {
			t.Errorf("score_revision = %d after one save naming the task twice, want 1", rev)
		}
		tasks, err := f.Store.UnscoredTasks(ctx, f.OrgID)
		if err != nil {
			t.Fatalf("UnscoredTasks: %v", err)
		}
		if len(tasks) != 0 {
			t.Fatalf("task still unscored after the save: %+v", tasks)
		}
	})

	// A score landing while the row is leased raises it in place: the holder
	// completes against its frozen revision and finds the row has moved on.
	t.Run("UpdateTaskScores_raises_a_leased_row_in_place", func(t *testing.T) {
		f := mk(t)
		id := f.Seed(t, 1)[0]
		score(t, f, id)
		item := claimOne(t, f)
		if item.TaskID != id || item.RequestedRevision != 1 {
			t.Fatalf("claimed %+v, want task %s at revision 1", item, id)
		}

		score(t, f, id)
		row := oneRow(t, f, id)
		requireRow(t, row, id, workitem.StatusLeased, 2)
		if row.ID != item.Receipt.ItemID {
			t.Errorf("save while leased minted row %d beside the leased row %d", row.ID, item.Receipt.ItemID)
		}
		if row.Attempt != 1 {
			t.Errorf("raising a leased row changed its attempt to %d", row.Attempt)
		}
	})

	// A parked row still holds its key, so a score raises it in place too;
	// redriving it evaluates the latest revision.
	t.Run("UpdateTaskScores_raises_a_parked_row_in_place", func(t *testing.T) {
		f := mk(t)
		id := f.Seed(t, 1)[0]
		score(t, f, id)
		item := claimOne(t, f)
		parked, err := f.ReDerive.Requeue(ctx, item.Receipt, workitem.OutcomePermanent, errors.New("rejected"))
		if err != nil || !parked {
			t.Fatalf("Requeue permanent: parked=%v err=%v", parked, err)
		}

		score(t, f, id)
		row := oneRow(t, f, id)
		requireRow(t, row, id, workitem.StatusParked, 2)
		if row.ID != item.Receipt.ItemID {
			t.Errorf("save while parked minted row %d beside the parked row %d", row.ID, item.Receipt.ItemID)
		}

		h, ok := f.ReDerive.(db.WorkKindHandle)
		if !ok {
			t.Fatalf("%T does not implement db.WorkKindHandle", f.ReDerive)
		}
		if err := workitem.Redrive(ctx, h.Conn(), h.Kind(), f.OrgID, row.ID, "operator"); err != nil {
			t.Fatalf("Redrive: %v", err)
		}
		redriven := claimOne(t, f)
		if redriven.Receipt.ItemID != row.ID || redriven.RequestedRevision != 2 {
			t.Errorf("redriven claim = %+v, want row %d frozen at revision 2", redriven, row.ID)
		}
	})

	// After the row is done its key is free: the next score admits a fresh
	// ready row at the next revision, and the done row keeps its own.
	t.Run("UpdateTaskScores_admits_a_fresh_row_after_done", func(t *testing.T) {
		f := mk(t)
		id := f.Seed(t, 1)[0]
		score(t, f, id)
		item := claimOne(t, f)
		if err := f.ReDerive.Complete(ctx, item.Receipt, func(db.PendingFiringsStore) error { return nil }); err != nil {
			t.Fatalf("Complete: %v", err)
		}

		score(t, f, id)
		rows := f.QueueRows(t, id)
		if len(rows) != 2 {
			t.Fatalf("task has %d queue rows after a save past done, want the done row and a fresh ready one: %+v", len(rows), rows)
		}
		requireRow(t, rows[0], id, workitem.StatusDone, 1)
		requireRow(t, rows[1], id, workitem.StatusReady, 2)
		if rows[1].Attempt != 0 {
			t.Errorf("fresh row attempt = %d, want 0", rows[1].Attempt)
		}
		if rev := f.ScoreRevision(t, id); rev != 2 {
			t.Errorf("score_revision = %d, want 2", rev)
		}
	})

	// The obligation and the scores are one commit: a queue write that cannot
	// land — here an update naming a task that does not exist, which the
	// queue row's foreign key refuses — leaves every tasks row of the batch
	// untouched.
	t.Run("UpdateTaskScores_is_all_or_nothing_when_the_queue_write_fails", func(t *testing.T) {
		f := mk(t)
		id := f.Seed(t, 1)[0]
		err := f.Store.UpdateTaskScores(ctx, f.OrgID, []domain.TaskScoreUpdate{
			{ID: id, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "s", PriorityReasoning: "r"},
			{ID: uuid.New().String(), PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "s", PriorityReasoning: "r"},
		})
		if err == nil {
			t.Fatal("UpdateTaskScores naming a task that does not exist succeeded")
		}
		if rows := f.QueueRows(t, id); len(rows) != 0 {
			t.Errorf("a failed save left queue rows behind: %+v", rows)
		}
		if rev := f.ScoreRevision(t, id); rev != 0 {
			t.Errorf("a failed save moved score_revision to %d", rev)
		}
		tasks, err := f.Store.UnscoredTasks(ctx, f.OrgID)
		if err != nil {
			t.Fatalf("UnscoredTasks: %v", err)
		}
		if len(tasks) != 1 || tasks[0].ID != id || tasks[0].AutonomySuitability != nil {
			t.Errorf("UnscoredTasks after a failed save = %+v, want the task still pending and unscored", tasks)
		}
	})

	t.Run("MarkScoring_empty_slice_is_noop", func(t *testing.T) {
		f := mk(t)
		if err := f.Store.MarkScoring(ctx, f.OrgID, nil); err != nil {
			t.Errorf("MarkScoring(nil): %v", err)
		}
	})

	// Quick guard: every method accepts a ctx with a deadline and
	// honors it for at least the round-trip — both backends call
	// ExecContext / QueryContext under the hood, so a ctx that's
	// already cancelled should fail fast.
	t.Run("CtxCancellation_fails_fast", func(t *testing.T) {
		f := mk(t)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		// Give the cancellation a moment to propagate through the driver.
		time.Sleep(time.Millisecond)
		if _, err := f.Store.UnscoredTasks(cancelled, f.OrgID); err == nil {
			t.Errorf("UnscoredTasks with cancelled ctx: want error, got nil")
		}
	})
}
