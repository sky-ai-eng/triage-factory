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

	// StageChild stages one mid-flight (no stored status) child conversation
	// under brID and returns its id.
	StageChild func(t *testing.T, brID string) string

	// ForceBlueprintStatus writes a blueprint_run's status and abort_reason
	// directly, bypassing the guarded flip — the suite needs a terminal parent
	// without going through the store method that would park children as a
	// side effect.
	ForceBlueprintStatus func(t *testing.T, brID, status, abortReason string)

	// SetCurrentStep writes a blueprint_run's current_step_index directly,
	// which is the only way to stage a pointer that moved without the step it
	// names: the advance commits the two together.
	SetCurrentStep func(t *testing.T, brID string, stepIndex int)

	// BlueprintRunState reads back what the sweep did (or didn't) write:
	// status, abort_reason ("" for NULL), and whether completed_at is stamped.
	BlueprintRunState func(t *testing.T, brID string) (status, abortReason string, completedAtSet bool)

	// ConversationStatus reads a conversation's STORED status, SQL NULL (the
	// mid-flight state) as "".
	ConversationStatus func(t *testing.T, convID string) string
}

// RunReconcileOrphanedConversationsConformance is the shared suite for the boot
// self-heal: the parent↔child desync it repairs, and the one shape it only
// reports.
//
// That checker is where the pressure is, because its predicate is an absence
// and its whole job is to stay hands-off. A 'running' blueprint_run with no
// conversation at the step its current_step_index names is unreachable by
// every recovery path — they drive or heal the step the pointer names — and
// unreachable by any live writer too, now that a firing commits the run with
// its first step and an advance commits the pointer with the step it names. So
// the subtests here pin both halves, for both ways the shape arises: it is
// counted when it exists, and it is never written to, whatever its age.
func RunReconcileOrphanedConversationsConformance(t *testing.T, mk ReconcileOrphanFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Childless_running_is_counted_and_not_repaired", func(t *testing.T) {
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, time.Hour)

		n, check, err := store.ReconcileOrphanedConversations(ctx)
		if err != nil {
			t.Fatalf("ReconcileOrphanedConversations: %v", err)
		}
		if n != 0 {
			t.Errorf("healed count = %d, want 0 (the checker repairs nothing)", n)
		}
		if check.Count != 1 {
			t.Fatalf("check.Count = %d, want 1", check.Count)
		}
		if len(check.Sample) != 1 || check.Sample[0] != brID {
			t.Errorf("check.Sample = %v, want [%s]", check.Sample, brID)
		}
		if status, reason, completed := seed.BlueprintRunState(t, brID); status != string(domain.BlueprintRunStatusRunning) || reason != "" || completed {
			t.Errorf("blueprint_run = (%q, %q, completed=%v), want (running, \"\", false) — the checker must not write", status, reason, completed)
		}

		// Reporting is not consuming: the row is still there, so a second call
		// counts it again. That is the point — the invariant is still broken.
		if _, again, err := store.ReconcileOrphanedConversations(ctx); err != nil || again.Count != 1 {
			t.Errorf("second call = (%d, %v), want count 1", again.Count, err)
		}
	})

	t.Run("Childless_running_is_counted_at_any_age", func(t *testing.T) {
		// No grace, because there is no window to protect: the firing commits
		// the run and its first step together, so a fresh childless run is not
		// a mint in flight — it is the same broken invariant an old one is.
		store, seed := mk(t)
		seed.BlueprintRun(t, 0)

		_, check, err := store.ReconcileOrphanedConversations(ctx)
		if err != nil {
			t.Fatalf("ReconcileOrphanedConversations: %v", err)
		}
		if check.Count != 1 {
			t.Errorf("check.Count = %d, want 1 (age is not part of the predicate)", check.Count)
		}
	})

	t.Run("Running_with_a_child_is_not_counted", func(t *testing.T) {
		// A child is the proof the firing committed. Age says nothing after
		// that: a long-running blueprint is ordinary work.
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, 30*24*time.Hour)
		convID := seed.StageChild(t, brID)

		n, check, err := store.ReconcileOrphanedConversations(ctx)
		if err != nil {
			t.Fatalf("ReconcileOrphanedConversations: %v", err)
		}
		if n != 0 || check.Count != 0 {
			t.Errorf("(healed, counted) = (%d, %d), want (0, 0) — a parent with a child is not an orphan", n, check.Count)
		}
		if status, _, _ := seed.BlueprintRunState(t, brID); status != string(domain.BlueprintRunStatusRunning) {
			t.Errorf("blueprint_run status = %q, want running", status)
		}
		if got := seed.ConversationStatus(t, convID); got != "" {
			t.Errorf("child status = %q, want no stored status (mid-flight, untouched)", got)
		}
	})

	t.Run("Running_with_no_child_at_its_current_step_is_counted", func(t *testing.T) {
		// The mid-advance orphan, and the reason the predicate reads the
		// pointer rather than merely the presence of a child: step 0 ran, the
		// pointer moved to step 1, and the step-1 conversation never landed.
		// The claim gate drives the step the pointer names, so nothing will
		// ever pick this run up, and no arm that walks its children can tell
		// it from ordinary work.
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, time.Hour)
		convID := seed.StageChild(t, brID)
		seed.SetCurrentStep(t, brID, 1)

		n, check, err := store.ReconcileOrphanedConversations(ctx)
		if err != nil {
			t.Fatalf("ReconcileOrphanedConversations: %v", err)
		}
		if n != 0 {
			t.Errorf("healed count = %d, want 0 (the checker repairs nothing)", n)
		}
		if check.Count != 1 {
			t.Fatalf("check.Count = %d, want 1 — a pointer naming no conversation is as undrivable as no child at all", check.Count)
		}
		if len(check.Sample) != 1 || check.Sample[0] != brID {
			t.Errorf("check.Sample = %v, want [%s]", check.Sample, brID)
		}
		if status, reason, completed := seed.BlueprintRunState(t, brID); status != string(domain.BlueprintRunStatusRunning) || reason != "" || completed {
			t.Errorf("blueprint_run = (%q, %q, completed=%v), want (running, \"\", false) — the checker must not write", status, reason, completed)
		}
		if got := seed.ConversationStatus(t, convID); got != "" {
			t.Errorf("the run's step-0 child status = %q, want no stored status (untouched)", got)
		}
	})

	t.Run("Terminal_childless_blueprint_run_is_not_counted", func(t *testing.T) {
		// Every terminal is a settled account of what happened. Only a run
		// still claiming to be 'running' is an unmet obligation.
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, time.Hour)
		seed.ForceBlueprintStatus(t, brID, string(domain.BlueprintRunStatusCancelled), "user_cancelled")

		n, check, err := store.ReconcileOrphanedConversations(ctx)
		if err != nil {
			t.Fatalf("ReconcileOrphanedConversations: %v", err)
		}
		if n != 0 || check.Count != 0 {
			t.Errorf("(healed, counted) = (%d, %d), want (0, 0) — terminal blueprint runs are settled", n, check.Count)
		}
		status, reason, _ := seed.BlueprintRunState(t, brID)
		if status != string(domain.BlueprintRunStatusCancelled) || reason != "user_cancelled" {
			t.Errorf("terminal blueprint_run = (%q, %q), want (cancelled, user_cancelled)", status, reason)
		}
	})

	t.Run("Mid_flight_child_under_a_terminal_parent_is_parked", func(t *testing.T) {
		// The repair arm, and the mirror of the checker above: a live child
		// under a dead parent. Kept beside it so the two can't drift — the
		// checker must not count this parent (it has a child) and must not
		// change what the park writes.
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, time.Hour)
		convID := seed.StageChild(t, brID)
		seed.ForceBlueprintStatus(t, brID, string(domain.BlueprintRunStatusFailed), "step_failed")

		n, check, err := store.ReconcileOrphanedConversations(ctx)
		if err != nil {
			t.Fatalf("ReconcileOrphanedConversations: %v", err)
		}
		if n != 1 {
			t.Fatalf("healed count = %d, want 1 (the parked child)", n)
		}
		if check.Count != 0 {
			t.Errorf("check.Count = %d, want 0 (a terminal parent is not the checker's shape)", check.Count)
		}
		if got := seed.ConversationStatus(t, convID); got != "open" {
			t.Errorf("child status = %q, want open (parked under a terminal parent)", got)
		}
		if _, reason, _ := seed.BlueprintRunState(t, brID); reason != "step_failed" {
			t.Errorf("parent abort_reason = %q, want step_failed (the park must not restamp it)", reason)
		}
	})

	t.Run("Sample_is_bounded_and_the_count_is_not", func(t *testing.T) {
		// One log line stays a log line, and the number stays honest: the
		// sample is capped, the count is every row.
		store, seed := mk(t)
		for i := 0; i < db.OrphanedStepSampleLimit+3; i++ {
			seed.BlueprintRun(t, time.Duration(i)*time.Minute)
		}

		_, check, err := store.ReconcileOrphanedConversations(ctx)
		if err != nil {
			t.Fatalf("ReconcileOrphanedConversations: %v", err)
		}
		if check.Count != db.OrphanedStepSampleLimit+3 {
			t.Errorf("check.Count = %d, want %d (every row, not just the sampled ones)", check.Count, db.OrphanedStepSampleLimit+3)
		}
		if len(check.Sample) != db.OrphanedStepSampleLimit {
			t.Errorf("len(check.Sample) = %d, want %d", len(check.Sample), db.OrphanedStepSampleLimit)
		}
	})
}
