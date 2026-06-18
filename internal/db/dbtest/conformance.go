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
