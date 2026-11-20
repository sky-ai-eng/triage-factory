package dbtest

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// BreakerSeeder stages the graph a trigger's breaker reads: triggers and the
// blueprints they fire, entities and their tasks, and finished blueprint runs.
type BreakerSeeder struct {
	// Trigger stages a one-step blueprint and an enabled trigger on its team
	// firing it with threshold as breaker_threshold.
	Trigger func(t *testing.T, threshold int) (triggerID, blueprintID string)
	// TriggerOn stages another enabled trigger firing blueprintID.
	TriggerOn func(t *testing.T, blueprintID string, threshold int) (triggerID string)
	// Task stages a fresh active entity and a queued task on it.
	Task func(t *testing.T) (entityID, taskID string)
	// TaskOnEntity stages another task on an entity Task created.
	TaskOnEntity func(t *testing.T, entityID string) (taskID string)
	// EventRun stages a blueprint run of blueprintID on taskID that triggerID
	// fired, with one step conversation. Returns the conversation's id.
	EventRun func(t *testing.T, taskID, triggerID, blueprintID, status string, startedAt time.Time) (conversationID string)
	// ManualRun stages a blueprint run of blueprintID on taskID that a person
	// started, with one step conversation. Returns the conversation's id.
	ManualRun func(t *testing.T, taskID, blueprintID, status string, startedAt time.Time) (conversationID string)
	// SetTrigger writes the trigger's enabled flag and breaker_threshold.
	SetTrigger func(t *testing.T, triggerID string, enabled bool, threshold int)
	// DeleteTrigger soft-deletes the trigger, as the delete route does.
	DeleteTrigger func(t *testing.T, triggerID string)
	// CloseEntity sets the entity's state to closed.
	CloseEntity func(t *testing.T, entityID string)
}

// BreakerFactory hands the suite a fresh database per subtest: the list read
// is org-wide and must see only what the subtest staged.
type BreakerFactory func(t *testing.T) (tasks db.TaskStore, orgID string, seed BreakerSeeder)

// RunBreakerConformance is the shared suite for TaskStore's breaker reads,
// BreakerStreakSystem and ListTrippedBreakersSystem.
func RunBreakerConformance(t *testing.T, mk BreakerFactory) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

	streak := func(t *testing.T, tasks db.TaskStore, orgID, entityID, triggerID string) domain.BreakerStreak {
		t.Helper()
		s, err := tasks.BreakerStreakSystem(ctx, orgID, entityID, triggerID)
		if err != nil {
			t.Fatalf("BreakerStreakSystem: %v", err)
		}
		return s
	}
	tripped := func(t *testing.T, tasks db.TaskStore, orgID string) []domain.TrippedBreaker {
		t.Helper()
		out, err := tasks.ListTrippedBreakersSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListTrippedBreakersSystem: %v", err)
		}
		return out
	}

	t.Run("Counts_the_trigger's_failed_runs", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		triggerID, blueprintID := seed.Trigger(t, 3)
		entityID, taskID := seed.Task(t)
		var last string
		for i := range 3 {
			last = seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(i))
		}
		got := streak(t, tasks, orgID, entityID, triggerID)
		if got.Failures != 3 || got.LastConversationID != last {
			t.Errorf("streak = %+v, want 3 failures, the newest run's conversation %s", got, last)
		}
	})

	t.Run("Aborted_counts_and_cancelled_or_running_does_neither", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		triggerID, blueprintID := seed.Trigger(t, 3)
		entityID, taskID := seed.Task(t)
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(0))
		aborted := seed.EventRun(t, taskID, triggerID, blueprintID, "aborted", at(1))
		seed.EventRun(t, taskID, triggerID, blueprintID, "cancelled", at(2))
		seed.EventRun(t, taskID, triggerID, blueprintID, "running", at(3))
		if got := streak(t, tasks, orgID, entityID, triggerID); got.Failures != 2 || got.LastConversationID != aborted {
			t.Errorf("streak = %+v, want the failed and the aborted run, ending at %s", got, aborted)
		}
	})

	t.Run("A_completed_run_of_the_trigger_ends_the_streak", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		triggerID, blueprintID := seed.Trigger(t, 3)
		entityID, taskID := seed.Task(t)
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(0))
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(1))
		seed.EventRun(t, taskID, triggerID, blueprintID, "completed", at(2))
		if got := streak(t, tasks, orgID, entityID, triggerID); got != (domain.BreakerStreak{}) {
			t.Errorf("streak after a completed run = %+v, want none", got)
		}
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(3))
		if got := streak(t, tasks, orgID, entityID, triggerID); got.Failures != 1 {
			t.Errorf("streak = %+v, want the one failure since the completed run", got)
		}
	})

	t.Run("A_person's_completed_run_on_the_entity_resets_it", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		triggerID, blueprintID := seed.Trigger(t, 3)
		_, otherBlueprintID := seed.Trigger(t, 3)
		entityID, taskID := seed.Task(t)
		otherTaskID := seed.TaskOnEntity(t, entityID)
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(0))
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(1))

		// Running, it resets nothing.
		seed.ManualRun(t, otherTaskID, otherBlueprintID, "running", at(2))
		if got := streak(t, tasks, orgID, entityID, triggerID); got.Failures != 2 {
			t.Errorf("streak beside a person's run still going = %+v, want 2", got)
		}
		// Completed, on another task of the entity and another blueprint, it
		// resets the streak.
		seed.ManualRun(t, otherTaskID, otherBlueprintID, "completed", at(3))
		if got := streak(t, tasks, orgID, entityID, triggerID); got != (domain.BreakerStreak{}) {
			t.Errorf("streak after a person's completed run = %+v, want none", got)
		}
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(4))
		if got := streak(t, tasks, orgID, entityID, triggerID); got.Failures != 1 {
			t.Errorf("streak = %+v, want the one failure since the person's run", got)
		}
	})

	t.Run("A_person's_run_resets_only_failures_before_it_and_its_failure_does_nothing", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		triggerID, blueprintID := seed.Trigger(t, 3)
		entityID, taskID := seed.Task(t)
		seed.ManualRun(t, taskID, blueprintID, "completed", at(0))
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(1))
		seed.ManualRun(t, taskID, blueprintID, "failed", at(2))
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(3))
		if got := streak(t, tasks, orgID, entityID, triggerID); got.Failures != 2 {
			t.Errorf("streak = %+v, want the two event failures after the person's completed run", got)
		}
	})

	t.Run("Counts_only_the_entity_and_the_trigger_named", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		triggerID, blueprintID := seed.Trigger(t, 3)
		otherTriggerID, otherBlueprintID := seed.Trigger(t, 3)
		entityID, taskID := seed.Task(t)
		otherEntityID, otherTaskID := seed.Task(t)
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(0))
		seed.EventRun(t, taskID, otherTriggerID, otherBlueprintID, "failed", at(1))
		seed.EventRun(t, otherTaskID, triggerID, blueprintID, "failed", at(2))
		seed.EventRun(t, otherTaskID, triggerID, blueprintID, "failed", at(3))
		if got := streak(t, tasks, orgID, entityID, triggerID); got.Failures != 1 {
			t.Errorf("streak on the entity = %+v, want its one failure of the trigger", got)
		}
		if got := streak(t, tasks, orgID, otherEntityID, triggerID); got.Failures != 2 {
			t.Errorf("streak on the other entity = %+v, want 2", got)
		}
	})

	// Another trigger on the same blueprint has a breaker of its own.
	t.Run("Another_trigger's_runs_of_the_blueprint_do_not_count", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		triggerID, blueprintID := seed.Trigger(t, 2)
		otherTriggerID := seed.TriggerOn(t, blueprintID, 2)
		entityID, taskID := seed.Task(t)
		seed.EventRun(t, taskID, otherTriggerID, blueprintID, "failed", at(0))
		seed.EventRun(t, taskID, otherTriggerID, blueprintID, "failed", at(1))
		if got := streak(t, tasks, orgID, entityID, triggerID); got != (domain.BreakerStreak{}) {
			t.Errorf("streak = %+v, want none: the failures are the other trigger's", got)
		}
		if got := tripped(t, tasks, orgID); !slices.Equal(got, []domain.TrippedBreaker{{TriggerID: otherTriggerID, EntityID: entityID}}) {
			t.Errorf("tripped = %+v, want only the other trigger on %s", got, entityID)
		}
	})

	t.Run("Another_trigger's_completed_run_of_the_blueprint_does_not_reset_it", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		triggerID, blueprintID := seed.Trigger(t, 3)
		otherTriggerID := seed.TriggerOn(t, blueprintID, 3)
		entityID, taskID := seed.Task(t)
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(0))
		seed.EventRun(t, taskID, otherTriggerID, blueprintID, "completed", at(1))
		if got := streak(t, tasks, orgID, entityID, triggerID); got.Failures != 1 {
			t.Errorf("streak after the other trigger's completed run = %+v, want this trigger's one failure", got)
		}
	})

	// A trigger retargeted onto another blueprint starts its streak again: its
	// runs of the blueprint it left do not count.
	t.Run("Counts_only_runs_of_the_trigger's_current_blueprint", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		triggerID, blueprintID := seed.Trigger(t, 2)
		_, formerBlueprintID := seed.Trigger(t, 2)
		entityID, taskID := seed.Task(t)
		seed.EventRun(t, taskID, triggerID, formerBlueprintID, "failed", at(0))
		seed.EventRun(t, taskID, triggerID, formerBlueprintID, "failed", at(1))
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(2))
		if got := streak(t, tasks, orgID, entityID, triggerID); got.Failures != 1 {
			t.Errorf("streak = %+v, want the one failure of the trigger's current blueprint", got)
		}
		if got := tripped(t, tasks, orgID); len(got) != 0 {
			t.Errorf("tripped = %+v, want none", got)
		}
	})

	t.Run("Only_a_run_on_the_entity_resets_it_and_an_event_run_only_of_the_trigger", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		triggerID, blueprintID := seed.Trigger(t, 3)
		otherTriggerID, otherBlueprintID := seed.Trigger(t, 3)
		entityID, taskID := seed.Task(t)
		_, otherTaskID := seed.Task(t)
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(0))
		seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(1))
		seed.ManualRun(t, otherTaskID, otherBlueprintID, "completed", at(2))
		seed.EventRun(t, otherTaskID, triggerID, blueprintID, "completed", at(3))
		seed.EventRun(t, taskID, otherTriggerID, otherBlueprintID, "completed", at(4))
		if got := streak(t, tasks, orgID, entityID, triggerID); got.Failures != 2 {
			t.Errorf("streak = %+v, want both failures: a person's run on another entity, the trigger's run on another entity, and another trigger's event run reset nothing", got)
		}
	})

	t.Run("Lists_each_trigger_and_entity_at_its_threshold", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		triggerID, blueprintID := seed.Trigger(t, 2)
		otherTriggerID, otherBlueprintID := seed.Trigger(t, 1)
		entityA, taskA := seed.Task(t)
		entityB, taskB := seed.Task(t)
		seed.EventRun(t, taskA, triggerID, blueprintID, "failed", at(0))
		seed.EventRun(t, taskA, triggerID, blueprintID, "failed", at(1))
		seed.EventRun(t, taskB, triggerID, blueprintID, "failed", at(2))
		seed.EventRun(t, taskB, otherTriggerID, otherBlueprintID, "aborted", at(3))

		// The first trigger reaches its threshold of 2 on entity A and not on
		// B; the second reaches its threshold of 1 on B.
		got := tripped(t, tasks, orgID)
		want := []domain.TrippedBreaker{
			{TriggerID: triggerID, EntityID: entityA},
			{TriggerID: otherTriggerID, EntityID: entityB},
		}
		slices.SortFunc(want, func(a, b domain.TrippedBreaker) int {
			return cmp.Or(strings.Compare(a.TriggerID, b.TriggerID), strings.Compare(a.EntityID, b.EntityID))
		})
		if !slices.Equal(got, want) {
			t.Errorf("tripped = %+v, want %+v", got, want)
		}
	})

	t.Run("A_breaker_stops_being_listed_when_it_no_longer_holds", func(t *testing.T) {
		cases := []struct {
			name   string
			change func(t *testing.T, seed BreakerSeeder, triggerID, blueprintID, entityID, taskID string)
		}{
			{"threshold_raised_past_the_count", func(t *testing.T, seed BreakerSeeder, triggerID, _, _, _ string) {
				seed.SetTrigger(t, triggerID, true, 3)
			}},
			{"trigger_disabled", func(t *testing.T, seed BreakerSeeder, triggerID, _, _, _ string) {
				seed.SetTrigger(t, triggerID, false, 2)
			}},
			{"trigger_deleted", func(t *testing.T, seed BreakerSeeder, triggerID, _, _, _ string) {
				seed.DeleteTrigger(t, triggerID)
			}},
			{"entity_closed", func(t *testing.T, seed BreakerSeeder, _, _, entityID, _ string) {
				seed.CloseEntity(t, entityID)
			}},
			{"a_person's_run_completed", func(t *testing.T, seed BreakerSeeder, _, blueprintID, _, taskID string) {
				seed.ManualRun(t, taskID, blueprintID, "completed", at(5))
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tasks, orgID, seed := mk(t)
				triggerID, blueprintID := seed.Trigger(t, 2)
				entityID, taskID := seed.Task(t)
				seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(0))
				seed.EventRun(t, taskID, triggerID, blueprintID, "failed", at(1))
				if got := tripped(t, tasks, orgID); len(got) != 1 {
					t.Fatalf("tripped before the change = %+v, want the one breaker", got)
				}
				tc.change(t, seed, triggerID, blueprintID, entityID, taskID)
				if got := tripped(t, tasks, orgID); len(got) != 0 {
					t.Errorf("tripped after the change = %+v, want none", got)
				}
			})
		}
	})
}
