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
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ScoreStoreFactory is what a per-backend test file hands to
// RunScoreStoreConformance. The factory returns:
//   - the wired ScoreStore impl
//   - the orgID to pass to every method (sqlite returns
//     runmode.LocalDefaultOrgID, postgres returns a fresh org UUID)
//   - a seed function that creates the underlying task rows the
//     conformance suite needs. The harness doesn't know how to
//     create tasks directly (TaskStore lands in a later wave); the
//     backend test owns that wiring against its own connection.
type ScoreStoreFactory func(t *testing.T) (store db.ScoreStore, orgID string, seed ScoreSeeder)

// ScoreSeeder lets the conformance harness ask the backend test to
// create N queued/pending tasks and return their IDs. Backend tests
// implement this against whatever raw-SQL path matches their schema
// (the conformance harness is intentionally schema-blind).
type ScoreSeeder func(t *testing.T, n int) []string

// RunScoreStoreConformance is the shared assertion suite for any
// db.ScoreStore implementation. Backend tests invoke it with their
// factory; both backends run the same subtests.
func RunScoreStoreConformance(t *testing.T, mk ScoreStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("UnscoredTasks_returns_only_pending_queued", func(t *testing.T) {
		s, orgID, seed := mk(t)
		ids := seed(t, 3)
		tasks, err := s.UnscoredTasks(ctx, orgID)
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
		s, orgID, seed := mk(t)
		ids := seed(t, 2)
		if err := s.MarkScoring(ctx, orgID, ids); err != nil {
			t.Fatalf("MarkScoring: %v", err)
		}
		// UnscoredTasks only picks up scoring_status='pending', so a
		// re-read after MarkScoring should now exclude these rows.
		tasks, err := s.UnscoredTasks(ctx, orgID)
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
		s, orgID, seed := mk(t)
		ids := seed(t, 2)
		if err := s.MarkScoring(ctx, orgID, ids); err != nil {
			t.Fatalf("MarkScoring: %v", err)
		}
		if err := s.ResetScoringToPending(ctx, orgID, ids); err != nil {
			t.Fatalf("ResetScoringToPending: %v", err)
		}
		tasks, err := s.UnscoredTasks(ctx, orgID)
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
		s, orgID, seed := mk(t)
		residue := seed(t, 2)   // marked in_progress, then "the process died"
		untouched := seed(t, 1) // still pending — never picked
		done := seed(t, 1)      // scored — a completed cycle's output

		if err := s.MarkScoring(ctx, orgID, residue); err != nil {
			t.Fatalf("MarkScoring: %v", err)
		}
		if err := s.UpdateTaskScores(ctx, orgID, []domain.TaskScoreUpdate{{
			ID:                  done[0],
			PriorityScore:       0.7,
			AutonomySuitability: 0.7,
			Summary:             "already scored",
			PriorityReasoning:   "already scored",
		}}); err != nil {
			t.Fatalf("UpdateTaskScores: %v", err)
		}

		n, err := s.ResetStaleScoring(ctx, orgID)
		if err != nil {
			t.Fatalf("ResetStaleScoring: %v", err)
		}
		if n != len(residue) {
			t.Errorf("ResetStaleScoring returned %d, want %d (only the in_progress rows)", n, len(residue))
		}

		unscored := map[string]bool{}
		tasks, err := s.UnscoredTasks(ctx, orgID)
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
		again, err := s.ResetStaleScoring(ctx, orgID)
		if err != nil {
			t.Fatalf("ResetStaleScoring (second call): %v", err)
		}
		if again != 0 {
			t.Errorf("second ResetStaleScoring returned %d, want 0", again)
		}
	})

	t.Run("UpdateTaskScores_applies_scores_and_marks_scored", func(t *testing.T) {
		s, orgID, seed := mk(t)
		ids := seed(t, 2)
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
		if err := s.UpdateTaskScores(ctx, orgID, updates); err != nil {
			t.Fatalf("UpdateTaskScores: %v", err)
		}
		// After UpdateTaskScores, rows should drop out of UnscoredTasks.
		tasks, err := s.UnscoredTasks(ctx, orgID)
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

	t.Run("UpdateTaskScores_owes_a_rederive", func(t *testing.T) {
		s, orgID, seed := mk(t)
		ids := seed(t, 2)

		owed, err := s.TasksOwedReDerive(ctx, orgID)
		if err != nil {
			t.Fatalf("TasksOwedReDerive (before scoring): %v", err)
		}
		if len(owed) != 0 {
			t.Fatalf("TasksOwedReDerive before any scores = %v, want none — an unscored task owes nothing", owed)
		}

		updates := make([]domain.TaskScoreUpdate, len(ids))
		for i, id := range ids {
			updates[i] = domain.TaskScoreUpdate{ID: id, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "s", PriorityReasoning: "r"}
		}
		if err := s.UpdateTaskScores(ctx, orgID, updates); err != nil {
			t.Fatalf("UpdateTaskScores: %v", err)
		}

		owed, err = s.TasksOwedReDerive(ctx, orgID)
		if err != nil {
			t.Fatalf("TasksOwedReDerive: %v", err)
		}
		gotOwed := map[string]bool{}
		for _, id := range owed {
			gotOwed[id] = true
		}
		for _, id := range ids {
			if !gotOwed[id] {
				t.Errorf("task %s scored but owes no re-derive; the mark must ride the same write as the scores", id)
			}
		}
	})

	t.Run("ClearReDeriveOwed_discharges_only_the_named_tasks", func(t *testing.T) {
		s, orgID, seed := mk(t)
		ids := seed(t, 3)
		updates := make([]domain.TaskScoreUpdate, len(ids))
		for i, id := range ids {
			updates[i] = domain.TaskScoreUpdate{ID: id, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "s", PriorityReasoning: "r"}
		}
		if err := s.UpdateTaskScores(ctx, orgID, updates); err != nil {
			t.Fatalf("UpdateTaskScores: %v", err)
		}

		// The re-derive pass evaluated the first task and bailed on the rest.
		if err := s.ClearReDeriveOwed(ctx, orgID, ids[:1]); err != nil {
			t.Fatalf("ClearReDeriveOwed: %v", err)
		}
		owed, err := s.TasksOwedReDerive(ctx, orgID)
		if err != nil {
			t.Fatalf("TasksOwedReDerive: %v", err)
		}
		if len(owed) != 2 {
			t.Fatalf("TasksOwedReDerive after clearing 1 of 3 = %v, want the other 2", owed)
		}
		for _, id := range owed {
			if id == ids[0] {
				t.Errorf("task %s still owed after its clear", id)
			}
		}

		// Idempotent: the pass runs again on the same set (a drain racing the
		// completion callback) and finds nothing left to discharge.
		if err := s.ClearReDeriveOwed(ctx, orgID, ids); err != nil {
			t.Fatalf("ClearReDeriveOwed (second call): %v", err)
		}
		owed, err = s.TasksOwedReDerive(ctx, orgID)
		if err != nil {
			t.Fatalf("TasksOwedReDerive (after full clear): %v", err)
		}
		if len(owed) != 0 {
			t.Errorf("TasksOwedReDerive after clearing every task = %v, want none", owed)
		}
	})

	t.Run("ReDeriveOwed_survives_the_scoring_status_writes", func(t *testing.T) {
		// The mark has exactly two writers: the scores write raises it, the
		// re-derive pass clears it. A task may legitimately be back to
		// 'pending' for a re-score while still owing a re-derive from the
		// last one, so the scoring-status writes must leave it alone — and
		// owing one must not hide the task from the cycle that re-scores it.
		s, orgID, seed := mk(t)
		ids := seed(t, 1)
		if err := s.UpdateTaskScores(ctx, orgID, []domain.TaskScoreUpdate{{
			ID: ids[0], PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "s", PriorityReasoning: "r",
		}}); err != nil {
			t.Fatalf("UpdateTaskScores: %v", err)
		}

		for _, step := range []struct {
			name string
			run  func() error
		}{
			{"MarkScoring", func() error { return s.MarkScoring(ctx, orgID, ids) }},
			{"ResetStaleScoring", func() error { _, err := s.ResetStaleScoring(ctx, orgID); return err }},
			{"MarkScoring (again)", func() error { return s.MarkScoring(ctx, orgID, ids) }},
			{"ResetScoringToPending", func() error { return s.ResetScoringToPending(ctx, orgID, ids) }},
		} {
			if err := step.run(); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
			owed, err := s.TasksOwedReDerive(ctx, orgID)
			if err != nil {
				t.Fatalf("TasksOwedReDerive after %s: %v", step.name, err)
			}
			if len(owed) != 1 || owed[0] != ids[0] {
				t.Fatalf("TasksOwedReDerive after %s = %v, want [%s] — only the re-derive pass may discharge the mark", step.name, owed, ids[0])
			}
		}

		// ...and the mark never gates scoring itself.
		tasks, err := s.UnscoredTasks(ctx, orgID)
		if err != nil {
			t.Fatalf("UnscoredTasks: %v", err)
		}
		if len(tasks) != 1 || tasks[0].ID != ids[0] {
			t.Errorf("UnscoredTasks = %v, want the owed-but-pending task %s", tasks, ids[0])
		}
	})

	t.Run("ClearReDeriveOwed_empty_slice_is_noop", func(t *testing.T) {
		s, orgID, _ := mk(t)
		if err := s.ClearReDeriveOwed(ctx, orgID, nil); err != nil {
			t.Errorf("ClearReDeriveOwed(nil): %v", err)
		}
	})

	t.Run("MarkScoring_empty_slice_is_noop", func(t *testing.T) {
		s, orgID, _ := mk(t)
		if err := s.MarkScoring(ctx, orgID, nil); err != nil {
			t.Errorf("MarkScoring(nil): %v", err)
		}
	})

	// Quick guard: every method accepts a ctx with a deadline and
	// honors it for at least the round-trip — both backends call
	// ExecContext / QueryContext under the hood, so a ctx that's
	// already cancelled should fail fast.
	t.Run("CtxCancellation_fails_fast", func(t *testing.T) {
		s, orgID, _ := mk(t)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		// Give the cancellation a moment to propagate through the driver.
		time.Sleep(time.Millisecond)
		if _, err := s.UnscoredTasks(cancelled, orgID); err == nil {
			t.Errorf("UnscoredTasks with cancelled ctx: want error, got nil")
		}
	})
}
