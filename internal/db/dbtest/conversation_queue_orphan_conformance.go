package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// OrphanFactory is what a per-backend test file hands to
// RunOrphanedConversationsConformance. Returns the wired ConversationQueueStore
// impl and a seeder staging the fixture shapes. Each call must hand back a
// clean database: the suite asserts exact counts, so a leftover orphan from a
// sibling subtest would show up as this one's.
type OrphanFactory func(t *testing.T) (store db.ConversationQueueStore, seed OrphanSeeder)

// OrphanSeeder stages the states ParkOrphanedConversationsSystem and
// CheckOrphanedStepsSystem read. Most of them are shapes a crash or a broken
// writer produces and no store method can, so the callbacks write them
// directly against the backend's own schema.
type OrphanSeeder struct {
	// BlueprintRun stages one 'running' blueprint_run with no child
	// conversations, its started_at backdated by age (0 = now, using the
	// backend's own clock so the suite never depends on Go/DB clock skew).
	// Returns its id.
	BlueprintRun func(t *testing.T, age time.Duration) string

	// StageChild stages one mid-flight (no stored status) child conversation
	// under brID, at the run's next unused step index (0 for the first), and
	// returns its id. The run's current_step_index is not moved.
	StageChild func(t *testing.T, brID string) string

	// ForceBlueprintStatus writes a blueprint_run's status and abort_reason
	// directly, bypassing the guarded flip — the suite needs a terminal parent
	// without going through the store method that would park children as a
	// side effect.
	ForceBlueprintStatus func(t *testing.T, brID, status, abortReason string)

	// RequestCancel sets a blueprint_run's cancel_requested, whatever its
	// status.
	RequestCancel func(t *testing.T, brID string)

	// SetCurrentStep writes a blueprint_run's current_step_index directly,
	// which is the only way to stage a pointer that moved without the step it
	// names: the advance commits the two together.
	SetCurrentStep func(t *testing.T, brID string, stepIndex int)

	// HoldClaim mints a live claim on convID, its lease well in the future,
	// and returns the claim's id.
	HoldClaim func(t *testing.T, convID string) string

	// ClaimReleased reports whether claimID has been released.
	ClaimReleased func(t *testing.T, claimID string) bool

	// RequestStop stamps a stop request on convID, as a person's stop does.
	RequestStop func(t *testing.T, convID string)

	// BlueprintRunState reads back what the sweep did (or didn't) write:
	// status, abort_reason ("" for NULL), and whether completed_at is stamped.
	BlueprintRunState func(t *testing.T, brID string) (status, abortReason string, completedAtSet bool)

	// ConversationStatus reads a conversation's STORED status, SQL NULL (the
	// mid-flight state) as "".
	ConversationStatus func(t *testing.T, convID string) string

	// ConversationPark reads a conversation's park_reason ("" for NULL) and
	// whether parked_at is stamped.
	ConversationPark func(t *testing.T, convID string) (reason string, parkedAtSet bool)
}

// RunOrphanedConversationsConformance is the shared suite for the orphan park
// and the checks beside it.
//
// The park's pressure is on what it leaves alone. Its predicate is the claim
// gate's blueprint clause, negated, and a mistake in either direction is
// costly: too wide, and it parks a follow-up another executor is running on a
// finished blueprint's final step; too narrow, and an orphan goes on looking
// in flight forever. So every shape the gate drives is pinned untouched beside
// every shape it refuses for good.
//
// The orphaned-step checker's pressure is the same, because its predicate is
// an absence and its whole job is to stay hands-off. A 'running' blueprint_run with no
// conversation at the step its current_step_index names is unreachable by
// every recovery path — they drive or heal the step the pointer names — and
// unreachable by any live writer too, now that a firing commits the run with
// its first step and an advance commits the pointer with the step it names. So
// the subtests here pin both halves, for both ways the shape arises: it is
// counted when it exists, and it is never written to, whatever its age.
func RunOrphanedConversationsConformance(t *testing.T, mk OrphanFactory) {
	t.Helper()
	ctx := context.Background()

	// park runs the park and fails the test on an error, returning the parked
	// rows keyed by conversation id.
	park := func(t *testing.T, store db.ConversationQueueStore) map[string]db.OrphanedConversation {
		t.Helper()
		parked, err := store.ParkOrphanedConversationsSystem(ctx)
		if err != nil {
			t.Fatalf("ParkOrphanedConversationsSystem: %v", err)
		}
		out := make(map[string]db.OrphanedConversation, len(parked))
		for _, o := range parked {
			out[o.ConversationID] = o
		}
		return out
	}
	check := func(t *testing.T, store db.ConversationQueueStore) db.OrphanedStepCheck {
		t.Helper()
		c, err := store.CheckOrphanedStepsSystem(ctx)
		if err != nil {
			t.Fatalf("CheckOrphanedStepsSystem: %v", err)
		}
		return c
	}
	// untouched asserts convID is still mid-flight and unparked.
	untouched := func(t *testing.T, seed OrphanSeeder, convID, what string) {
		t.Helper()
		if got := seed.ConversationStatus(t, convID); got != "" {
			t.Errorf("%s: status = %q, want no stored status (mid-flight, untouched)", what, got)
		}
		if reason, parkedAt := seed.ConversationPark(t, convID); reason != "" || parkedAt {
			t.Errorf("%s: park = (%q, parked_at=%v), want none", what, reason, parkedAt)
		}
	}
	// parkedAs asserts convID was parked open with reason.
	parkedAs := func(t *testing.T, seed OrphanSeeder, convID string, reason domain.ParkReason, what string) {
		t.Helper()
		if got := seed.ConversationStatus(t, convID); got != domain.StatusOpen {
			t.Errorf("%s: status = %q, want open", what, got)
		}
		if got, parkedAt := seed.ConversationPark(t, convID); got != string(reason) || !parkedAt {
			t.Errorf("%s: park = (%q, parked_at=%v), want (%q, stamped)", what, got, parkedAt, reason)
		}
	}

	t.Run("Current_step_of_a_finished_run_is_not_an_orphan", func(t *testing.T) {
		// A follow-up on a finished blueprint's final step re-queues it, and
		// the claim gate drives it whatever the run's terminal: so it is not
		// an orphan, whether it waits for a claim or another executor already
		// holds one.
		store, seed := mk(t)
		type subject struct {
			convID, claimID, what string
		}
		var subjects []subject
		for _, status := range []domain.BlueprintRunStatus{
			domain.BlueprintRunStatusCompleted,
			domain.BlueprintRunStatusAborted,
			domain.BlueprintRunStatusFailed,
		} {
			for _, claimed := range []bool{false, true} {
				brID := seed.BlueprintRun(t, time.Hour)
				convID := seed.StageChild(t, brID)
				seed.ForceBlueprintStatus(t, brID, string(status), "")
				s := subject{convID: convID, what: string(status) + " run, waiting for a claim"}
				if claimed {
					s.claimID = seed.HoldClaim(t, convID)
					s.what = string(status) + " run, claimed"
				}
				subjects = append(subjects, s)
			}
		}

		if parked := park(t, store); len(parked) != 0 {
			t.Fatalf("parked %d conversations, want none — a finished run's current step is drivable", len(parked))
		}
		for _, s := range subjects {
			untouched(t, seed, s.convID, s.what)
			if s.claimID != "" && seed.ClaimReleased(t, s.claimID) {
				t.Errorf("%s: claim released, want it left to its holder", s.what)
			}
		}
	})

	t.Run("Called_off_runs_orphans_are_parked", func(t *testing.T) {
		// Both spellings of a run called off: cancelled, and a cancel
		// requested on a run that then stopped another way, which the
		// settlement does not take because it settles only running runs.
		store, seed := mk(t)
		cancelledRun := seed.BlueprintRun(t, time.Hour)
		underCancelled := seed.StageChild(t, cancelledRun)
		seed.ForceBlueprintStatus(t, cancelledRun, string(domain.BlueprintRunStatusCancelled), "user_cancelled")
		requestedRun := seed.BlueprintRun(t, time.Hour)
		underRequested := seed.StageChild(t, requestedRun)
		seed.RequestCancel(t, requestedRun)
		seed.ForceBlueprintStatus(t, requestedRun, string(domain.BlueprintRunStatusCompleted), "")

		parked := park(t, store)
		if len(parked) != 2 {
			t.Fatalf("parked %d conversations, want 2", len(parked))
		}
		parkedAs(t, seed, underCancelled, domain.ParkReasonBlueprintCancelled, "under a cancelled run")
		parkedAs(t, seed, underRequested, domain.ParkReasonBlueprintCancelled, "under a completed run with a cancel requested")

		got := parked[underCancelled]
		if got.BlueprintRunID != cancelledRun || got.OrgID == "" || got.StepIndex == nil || *got.StepIndex != 0 ||
			got.RunStatus != domain.BlueprintRunStatusCancelled || got.RunCancelRequested || got.RunCurrentStepIndex != 0 {
			t.Errorf("returned row = %+v, want its run %s (cancelled, no cancel request, on step 0) and step 0", got, cancelledRun)
		}
		if got := parked[underRequested]; got.RunStatus != domain.BlueprintRunStatusCompleted || !got.RunCancelRequested {
			t.Errorf("returned row = %+v, want its run completed with a cancel requested", got)
		}
		if status, reason, _ := seed.BlueprintRunState(t, cancelledRun); status != string(domain.BlueprintRunStatusCancelled) || reason != "user_cancelled" {
			t.Errorf("run = (%q, %q), want (cancelled, user_cancelled) — the park writes no run", status, reason)
		}

		if again := park(t, store); len(again) != 0 {
			t.Errorf("second pass parked %d conversations, want none", len(again))
		}
	})

	t.Run("Step_the_run_moved_past_is_parked", func(t *testing.T) {
		// The pointer only moves forward, so a step behind it is never driven
		// again, whatever the run's status. The step the pointer names is
		// untouched in both.
		store, seed := mk(t)
		type pair struct{ behind, current, what string }
		var pairs []pair
		for _, status := range []domain.BlueprintRunStatus{
			domain.BlueprintRunStatusRunning,
			domain.BlueprintRunStatusCompleted,
		} {
			brID := seed.BlueprintRun(t, time.Hour)
			behind := seed.StageChild(t, brID)
			current := seed.StageChild(t, brID)
			seed.SetCurrentStep(t, brID, 1)
			if status != domain.BlueprintRunStatusRunning {
				seed.ForceBlueprintStatus(t, brID, string(status), "")
			}
			pairs = append(pairs, pair{behind: behind, current: current, what: string(status) + " run"})
		}

		parked := park(t, store)
		if len(parked) != len(pairs) {
			t.Fatalf("parked %d conversations, want %d", len(parked), len(pairs))
		}
		for _, p := range pairs {
			parkedAs(t, seed, p.behind, domain.ParkReasonBlueprintTerminal, p.what+", step behind the pointer")
			if o := parked[p.behind]; o.StepIndex == nil || *o.StepIndex != 0 || o.RunCurrentStepIndex != 1 {
				t.Errorf("%s: returned row = %+v, want step 0 under a run on step 1", p.what, o)
			}
			untouched(t, seed, p.current, p.what+", step the pointer names")
		}
	})

	t.Run("Claimed_orphan_is_left_to_its_holder", func(t *testing.T) {
		// A live claim is an engagement the park cannot see into. Once it
		// ends or lapses and is released, a later pass takes the row.
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, time.Hour)
		convID := seed.StageChild(t, brID)
		seed.ForceBlueprintStatus(t, brID, string(domain.BlueprintRunStatusCancelled), "user_cancelled")
		claimID := seed.HoldClaim(t, convID)

		if parked := park(t, store); len(parked) != 0 {
			t.Fatalf("parked %d conversations, want none", len(parked))
		}
		untouched(t, seed, convID, "claimed orphan")
		if seed.ClaimReleased(t, claimID) {
			t.Error("claim released, want it left to its holder")
		}
	})

	t.Run("Settlement_shapes_are_left_to_it", func(t *testing.T) {
		// The dispatcher's settlement owns a stop request, and a running run
		// with a cancel requested: it parks the first with the reason the
		// request names and cancels the second along with its conversations.
		store, seed := mk(t)
		cancellingRun := seed.BlueprintRun(t, time.Hour)
		underCancelling := seed.StageChild(t, cancellingRun)
		seed.RequestCancel(t, cancellingRun)
		stoppedRun := seed.BlueprintRun(t, time.Hour)
		stopped := seed.StageChild(t, stoppedRun)
		seed.ForceBlueprintStatus(t, stoppedRun, string(domain.BlueprintRunStatusCancelled), "user_cancelled")
		seed.RequestStop(t, stopped)

		if parked := park(t, store); len(parked) != 0 {
			t.Fatalf("parked %d conversations, want none", len(parked))
		}
		untouched(t, seed, underCancelling, "under a running run with a cancel requested")
		untouched(t, seed, stopped, "with a stop requested")
		if status, _, _ := seed.BlueprintRunState(t, cancellingRun); status != string(domain.BlueprintRunStatusRunning) {
			t.Errorf("cancelling run status = %q, want running — the park writes no run", status)
		}
	})

	t.Run("Childless_running_is_counted_and_not_repaired", func(t *testing.T) {
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, time.Hour)

		got := check(t, store)
		if got.Count != 1 {
			t.Fatalf("check.Count = %d, want 1", got.Count)
		}
		if len(got.Sample) != 1 || got.Sample[0] != brID {
			t.Errorf("check.Sample = %v, want [%s]", got.Sample, brID)
		}
		if status, reason, completed := seed.BlueprintRunState(t, brID); status != string(domain.BlueprintRunStatusRunning) || reason != "" || completed {
			t.Errorf("blueprint_run = (%q, %q, completed=%v), want (running, \"\", false) — the checker must not write", status, reason, completed)
		}

		// Reporting is not consuming: the row is still there, so a second call
		// counts it again. That is the point — the invariant is still broken.
		if again := check(t, store); again.Count != 1 {
			t.Errorf("second call count = %d, want 1", again.Count)
		}
	})

	t.Run("Childless_running_is_counted_at_any_age", func(t *testing.T) {
		// No grace, because there is no window to protect: the firing commits
		// the run and its first step together, so a fresh childless run is not
		// a mint in flight — it is the same broken invariant an old one is.
		store, seed := mk(t)
		seed.BlueprintRun(t, 0)

		if got := check(t, store); got.Count != 1 {
			t.Errorf("check.Count = %d, want 1 (age is not part of the predicate)", got.Count)
		}
	})

	t.Run("Running_with_a_child_is_not_counted_or_parked", func(t *testing.T) {
		// A child is the proof the firing committed. Age says nothing after
		// that: a long-running blueprint is ordinary work.
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, 30*24*time.Hour)
		convID := seed.StageChild(t, brID)

		if got := check(t, store); got.Count != 0 {
			t.Errorf("check.Count = %d, want 0 — a parent with a child at its pointer is not an orphan", got.Count)
		}
		if parked := park(t, store); len(parked) != 0 {
			t.Errorf("parked %d conversations, want none", len(parked))
		}
		if status, _, _ := seed.BlueprintRunState(t, brID); status != string(domain.BlueprintRunStatusRunning) {
			t.Errorf("blueprint_run status = %q, want running", status)
		}
		untouched(t, seed, convID, "the run's current step")
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

		got := check(t, store)
		if got.Count != 1 {
			t.Fatalf("check.Count = %d, want 1 — a pointer naming no conversation is as undrivable as no child at all", got.Count)
		}
		if len(got.Sample) != 1 || got.Sample[0] != brID {
			t.Errorf("check.Sample = %v, want [%s]", got.Sample, brID)
		}
		if status, reason, completed := seed.BlueprintRunState(t, brID); status != string(domain.BlueprintRunStatusRunning) || reason != "" || completed {
			t.Errorf("blueprint_run = (%q, %q, completed=%v), want (running, \"\", false) — the checker must not write", status, reason, completed)
		}
		untouched(t, seed, convID, "the run's step-0 child, after the check")
	})

	t.Run("Terminal_childless_blueprint_run_is_not_counted", func(t *testing.T) {
		// Every terminal is a settled account of what happened. Only a run
		// still claiming to be 'running' is an unmet obligation.
		store, seed := mk(t)
		brID := seed.BlueprintRun(t, time.Hour)
		seed.ForceBlueprintStatus(t, brID, string(domain.BlueprintRunStatusCancelled), "user_cancelled")

		if got := check(t, store); got.Count != 0 {
			t.Errorf("check.Count = %d, want 0 — terminal blueprint runs are settled", got.Count)
		}
		status, reason, _ := seed.BlueprintRunState(t, brID)
		if status != string(domain.BlueprintRunStatusCancelled) || reason != "user_cancelled" {
			t.Errorf("terminal blueprint_run = (%q, %q), want (cancelled, user_cancelled)", status, reason)
		}
	})

	t.Run("Sample_is_bounded_and_the_count_is_not", func(t *testing.T) {
		// One log line stays a log line, and the number stays honest: the
		// sample is capped, the count is every row.
		store, seed := mk(t)
		for i := 0; i < db.OrphanedStepSampleLimit+3; i++ {
			seed.BlueprintRun(t, time.Duration(i)*time.Minute)
		}

		got := check(t, store)
		if got.Count != db.OrphanedStepSampleLimit+3 {
			t.Errorf("check.Count = %d, want %d (every row, not just the sampled ones)", got.Count, db.OrphanedStepSampleLimit+3)
		}
		if len(got.Sample) != db.OrphanedStepSampleLimit {
			t.Errorf("len(check.Sample) = %d, want %d", len(got.Sample), db.OrphanedStepSampleLimit)
		}
	})
}
