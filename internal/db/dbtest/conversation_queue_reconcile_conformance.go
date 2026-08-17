package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ReconcileOrphanFactory is what a per-backend test file hands to
// RunReconcileOrphanedConversationsConformance. Returns the wired ConversationQueueStore impl and
// a seeder staging the fixture shapes. Each call must hand back a clean
// database: the suite asserts exact healed counts, so a leftover orphan from a
// sibling subtest would show up as this one's.
type ReconcileOrphanFactory func(t *testing.T) (store db.ConversationQueueStore, seed ReconcileOrphanSeeder)

// ReconcileOrphanSeeder stages the states ReconcileOrphanedConversations recovers from.
// Every one of them is a shape a crash produces and no store method can, so the
// callbacks write them directly against the backend's own schema.
type ReconcileOrphanSeeder struct {
	// BlueprintRun stages one 'running' blueprint_run with no child
	// conversations, its started_at backdated by age (0 = now, using the
	// backend's own clock so the suite never depends on Go/DB clock skew).
	// Returns its id.
	BlueprintRun func(t *testing.T, age time.Duration) string

	// EnqueueChild stages one mid-flight (no stored status) child conversation
	// under brID and returns its id.
	EnqueueChild func(t *testing.T, brID string) string

	// ForceBlueprintStatus writes a blueprint_run's status and abort_reason
	// directly, bypassing the guarded flip — the suite needs a terminal parent
	// without going through the store method that would park children as a
	// side effect.
	ForceBlueprintStatus func(t *testing.T, brID, status, abortReason string)

	// BlueprintRunState reads back what the sweep did (or didn't) write:
	// status, abort_reason ("" for NULL), and whether completed_at is stamped.
	BlueprintRunState func(t *testing.T, brID string) (status, abortReason string, completedAtSet bool)

	// ConversationStatus reads a conversation's STORED status, SQL NULL (the
	// mid-flight state) as "".
	ConversationStatus func(t *testing.T, convID string) string
}

// RunReconcileOrphanedConversationsConformance is the shared suite for the boot
// self-heal, covering both directions of the parent↔child desync and the
// negative space around each.
//
// The mint-crash arm is the one under the most pressure to be wrong, because
// its predicate is an absence: a 'running' blueprint_run with NO child
// conversation is unreachable by every recovery path that joins through
// conversations, so nothing else would catch a regression here. Its three ways
// to misfire — sweeping a healthy parent whose child simply hasn't been claimed
// yet, sweeping one whose mint is still in flight, and re-stamping a run that
// already reached a terminal — each get their own subtest.
func RunReconcileOrphanedConversationsConformance(t *testing.T, mk ReconcileOrphanFactory) {
	t.Helper()
	ctx := context.Background()

	// Comfortably past the grace, so a slow CI box can't drift a fixture back
	// inside it mid-test.
	pastGrace := domain.BlueprintOrphanedAtMintGrace + time.Minute

	t.Run("Childless_running_past_grace_fails_orphaned_at_mint", func(t *testing.T) {
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, pastGrace)

		n, err := store.ReconcileOrphanedConversations(ctx)
		if err != nil {
			t.Fatalf("ReconcileOrphanedConversations: %v", err)
		}
		if n != 1 {
			t.Fatalf("healed count = %d, want 1 (the childless orphan)", n)
		}
		status, reason, completed := seed.BlueprintRunState(t, brID)
		if status != string(domain.BlueprintRunStatusFailed) {
			t.Errorf("blueprint_run status = %q, want failed", status)
		}
		if reason != domain.BlueprintAbortOrphanedAtMint {
			t.Errorf("abort_reason = %q, want %q", reason, domain.BlueprintAbortOrphanedAtMint)
		}
		if !completed {
			t.Error("completed_at is NULL on a terminal blueprint_run")
		}

		// Idempotent: the row is terminal now, so a second sweep finds nothing.
		if n2, err := store.ReconcileOrphanedConversations(ctx); err != nil || n2 != 0 {
			t.Errorf("second sweep = (%d, %v), want (0, nil)", n2, err)
		}
	})

	t.Run("Childless_running_within_grace_is_untouched", func(t *testing.T) {
		// The grace is what makes racing a live Delegate impossible: the mint
		// commits the parent and enqueues the first step milliseconds later, and
		// in between the row looks exactly like an orphan.
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, 0)

		n, err := store.ReconcileOrphanedConversations(ctx)
		if err != nil {
			t.Fatalf("ReconcileOrphanedConversations: %v", err)
		}
		if n != 0 {
			t.Errorf("healed count = %d, want 0 (a mint still inside the grace)", n)
		}
		if status, reason, completed := seed.BlueprintRunState(t, brID); status != string(domain.BlueprintRunStatusRunning) || reason != "" || completed {
			t.Errorf("fresh blueprint_run = (%q, %q, completed=%v), want (running, \"\", false)", status, reason, completed)
		}
	})

	t.Run("Running_with_a_child_is_untouched_at_any_age", func(t *testing.T) {
		// A child is the proof the mint completed. Age says nothing after that:
		// a long-running blueprint is ordinary work, and failing it would kill
		// a live agent mid-step.
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, 30*24*time.Hour)
		convID := seed.EnqueueChild(t, brID)

		n, err := store.ReconcileOrphanedConversations(ctx)
		if err != nil {
			t.Fatalf("ReconcileOrphanedConversations: %v", err)
		}
		if n != 0 {
			t.Errorf("healed count = %d, want 0 (a parent with a child is not an orphan)", n)
		}
		if status, _, _ := seed.BlueprintRunState(t, brID); status != string(domain.BlueprintRunStatusRunning) {
			t.Errorf("blueprint_run status = %q, want running", status)
		}
		if got := seed.ConversationStatus(t, convID); got != "" {
			t.Errorf("child status = %q, want no stored status (mid-flight, untouched)", got)
		}
	})

	t.Run("Terminal_childless_run_keeps_its_own_verdict", func(t *testing.T) {
		// Every terminal is a settled account of what happened, including the
		// abort_reason a cancel or an abort already wrote. The arm may only
		// claim a run that is still 'running'.
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, pastGrace)
		seed.ForceBlueprintStatus(t, brID, string(domain.BlueprintRunStatusCancelled), "user_cancelled")

		n, err := store.ReconcileOrphanedConversations(ctx)
		if err != nil {
			t.Fatalf("ReconcileOrphanedConversations: %v", err)
		}
		if n != 0 {
			t.Errorf("healed count = %d, want 0 (terminal runs are settled)", n)
		}
		status, reason, _ := seed.BlueprintRunState(t, brID)
		if status != string(domain.BlueprintRunStatusCancelled) || reason != "user_cancelled" {
			t.Errorf("terminal blueprint_run = (%q, %q), want (cancelled, user_cancelled)", status, reason)
		}
	})

	t.Run("Mid_flight_child_under_a_terminal_parent_is_parked", func(t *testing.T) {
		// The original arm, and the mirror of the one above: a live child under
		// a dead parent. Kept here so the two can't drift — the mint-crash arm
		// must not swallow this shape (its parent has a child) and must not
		// change what this one writes.
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, pastGrace)
		convID := seed.EnqueueChild(t, brID)
		seed.ForceBlueprintStatus(t, brID, string(domain.BlueprintRunStatusFailed), "step_failed")

		n, err := store.ReconcileOrphanedConversations(ctx)
		if err != nil {
			t.Fatalf("ReconcileOrphanedConversations: %v", err)
		}
		if n != 1 {
			t.Fatalf("healed count = %d, want 1 (the parked child)", n)
		}
		if got := seed.ConversationStatus(t, convID); got != "open" {
			t.Errorf("child status = %q, want open (parked under a terminal parent)", got)
		}
		if _, reason, _ := seed.BlueprintRunState(t, brID); reason != "step_failed" {
			t.Errorf("parent abort_reason = %q, want step_failed (the park must not restamp it)", reason)
		}
	})
}
