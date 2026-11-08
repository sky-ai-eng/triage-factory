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
	// Trigger stages a blueprint whose one step's prompt has an id of its
	// own, and an enabled trigger on its team firing it with threshold as
	// breaker_threshold.
	Trigger func(t *testing.T, threshold int) (triggerID, blueprintID, stepPromptID string)
	// Task stages a fresh active entity and a queued task on it.
	Task func(t *testing.T) (entityID, taskID string)
	// TaskOnEntity stages another task on an entity Task created.
	TaskOnEntity func(t *testing.T, entityID string) (taskID string)
	// Run stages a blueprint run of blueprintID on taskID with one step
	// conversation: triggerType "event" for one a trigger fired, "manual"
	// for one a person started. Returns the conversation's id.
	Run func(t *testing.T, taskID, blueprintID, triggerType, status string, startedAt time.Time) (conversationID string)
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

	streak := func(t *testing.T, tasks db.TaskStore, orgID, entityID, blueprintID string) domain.BreakerStreak {
		t.Helper()
		s, err := tasks.BreakerStreakSystem(ctx, orgID, entityID, blueprintID)
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

	t.Run("Counts_the_trigger's_runs_by_its_blueprint", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		_, blueprintID, promptID := seed.Trigger(t, 3)
		if blueprintID == promptID {
			t.Fatalf("the fixture's blueprint and step prompt share id %s; the suite needs them apart", blueprintID)
		}
		entityID, taskID := seed.Task(t)
		var last string
		for i := range 3 {
			last = seed.Run(t, taskID, blueprintID, "event", "failed", at(i))
		}
		got := streak(t, tasks, orgID, entityID, blueprintID)
		if got.Failures != 3 || got.LastConversationID != last {
			t.Errorf("streak = %+v, want 3 failures, the newest run's conversation %s", got, last)
		}
		if got := streak(t, tasks, orgID, entityID, promptID); got.Failures != 0 {
			t.Errorf("keyed by the step prompt, streak = %+v, want none: runs are the blueprint's", got)
		}
	})

	t.Run("Aborted_counts_and_cancelled_or_running_does_neither", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		_, blueprintID, _ := seed.Trigger(t, 3)
		entityID, taskID := seed.Task(t)
		seed.Run(t, taskID, blueprintID, "event", "failed", at(0))
		aborted := seed.Run(t, taskID, blueprintID, "event", "aborted", at(1))
		seed.Run(t, taskID, blueprintID, "event", "cancelled", at(2))
		seed.Run(t, taskID, blueprintID, "event", "running", at(3))
		if got := streak(t, tasks, orgID, entityID, blueprintID); got.Failures != 2 || got.LastConversationID != aborted {
			t.Errorf("streak = %+v, want the failed and the aborted run, ending at %s", got, aborted)
		}
	})

	t.Run("A_completed_run_of_the_blueprint_ends_the_streak", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		_, blueprintID, _ := seed.Trigger(t, 3)
		entityID, taskID := seed.Task(t)
		seed.Run(t, taskID, blueprintID, "event", "failed", at(0))
		seed.Run(t, taskID, blueprintID, "event", "failed", at(1))
		seed.Run(t, taskID, blueprintID, "event", "completed", at(2))
		if got := streak(t, tasks, orgID, entityID, blueprintID); got != (domain.BreakerStreak{}) {
			t.Errorf("streak after a completed run = %+v, want none", got)
		}
		seed.Run(t, taskID, blueprintID, "event", "failed", at(3))
		if got := streak(t, tasks, orgID, entityID, blueprintID); got.Failures != 1 {
			t.Errorf("streak = %+v, want the one failure since the completed run", got)
		}
	})

	t.Run("A_person's_completed_run_on_the_entity_resets_it", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		_, blueprintID, _ := seed.Trigger(t, 3)
		_, otherBlueprintID, _ := seed.Trigger(t, 3)
		entityID, taskID := seed.Task(t)
		otherTaskID := seed.TaskOnEntity(t, entityID)
		seed.Run(t, taskID, blueprintID, "event", "failed", at(0))
		seed.Run(t, taskID, blueprintID, "event", "failed", at(1))

		// Running, it resets nothing.
		seed.Run(t, otherTaskID, otherBlueprintID, "manual", "running", at(2))
		if got := streak(t, tasks, orgID, entityID, blueprintID); got.Failures != 2 {
			t.Errorf("streak beside a person's run still going = %+v, want 2", got)
		}
		// Completed, on another task of the entity and another blueprint, it
		// resets the streak.
		seed.Run(t, otherTaskID, otherBlueprintID, "manual", "completed", at(3))
		if got := streak(t, tasks, orgID, entityID, blueprintID); got != (domain.BreakerStreak{}) {
			t.Errorf("streak after a person's completed run = %+v, want none", got)
		}
		seed.Run(t, taskID, blueprintID, "event", "failed", at(4))
		if got := streak(t, tasks, orgID, entityID, blueprintID); got.Failures != 1 {
			t.Errorf("streak = %+v, want the one failure since the person's run", got)
		}
	})

	t.Run("A_person's_run_resets_only_failures_before_it_and_its_failure_does_nothing", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		_, blueprintID, _ := seed.Trigger(t, 3)
		entityID, taskID := seed.Task(t)
		seed.Run(t, taskID, blueprintID, "manual", "completed", at(0))
		seed.Run(t, taskID, blueprintID, "event", "failed", at(1))
		seed.Run(t, taskID, blueprintID, "manual", "failed", at(2))
		seed.Run(t, taskID, blueprintID, "event", "failed", at(3))
		if got := streak(t, tasks, orgID, entityID, blueprintID); got.Failures != 2 {
			t.Errorf("streak = %+v, want the two event failures after the person's completed run", got)
		}
	})

	t.Run("Counts_only_the_entity_and_the_blueprint_named", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		_, blueprintID, _ := seed.Trigger(t, 3)
		_, otherBlueprintID, _ := seed.Trigger(t, 3)
		entityID, taskID := seed.Task(t)
		otherEntityID, otherTaskID := seed.Task(t)
		seed.Run(t, taskID, blueprintID, "event", "failed", at(0))
		seed.Run(t, taskID, otherBlueprintID, "event", "failed", at(1))
		seed.Run(t, otherTaskID, blueprintID, "event", "failed", at(2))
		seed.Run(t, otherTaskID, blueprintID, "event", "failed", at(3))
		if got := streak(t, tasks, orgID, entityID, blueprintID); got.Failures != 1 {
			t.Errorf("streak on the entity = %+v, want its one failure of the blueprint", got)
		}
		if got := streak(t, tasks, orgID, otherEntityID, blueprintID); got.Failures != 2 {
			t.Errorf("streak on the other entity = %+v, want 2", got)
		}
		if got := streak(t, tasks, orgID, entityID, ""); got != (domain.BreakerStreak{}) {
			t.Errorf("streak for no blueprint = %+v, want none", got)
		}
	})

	t.Run("Only_a_run_on_the_entity_resets_it_and_an_event_run_only_of_the_blueprint", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		_, blueprintID, _ := seed.Trigger(t, 3)
		_, otherBlueprintID, _ := seed.Trigger(t, 3)
		entityID, taskID := seed.Task(t)
		_, otherTaskID := seed.Task(t)
		seed.Run(t, taskID, blueprintID, "event", "failed", at(0))
		seed.Run(t, taskID, blueprintID, "event", "failed", at(1))
		seed.Run(t, otherTaskID, otherBlueprintID, "manual", "completed", at(2))
		seed.Run(t, otherTaskID, blueprintID, "event", "completed", at(3))
		seed.Run(t, taskID, otherBlueprintID, "event", "completed", at(4))
		if got := streak(t, tasks, orgID, entityID, blueprintID); got.Failures != 2 {
			t.Errorf("streak = %+v, want both failures: a person's run on another entity, the blueprint's run on another entity, and another blueprint's event run reset nothing", got)
		}
	})

	t.Run("Lists_each_trigger_and_entity_at_its_threshold", func(t *testing.T) {
		tasks, orgID, seed := mk(t)
		triggerID, blueprintID, _ := seed.Trigger(t, 2)
		otherTriggerID, otherBlueprintID, _ := seed.Trigger(t, 1)
		entityA, taskA := seed.Task(t)
		entityB, taskB := seed.Task(t)
		seed.Run(t, taskA, blueprintID, "event", "failed", at(0))
		seed.Run(t, taskA, blueprintID, "event", "failed", at(1))
		seed.Run(t, taskB, blueprintID, "event", "failed", at(2))
		seed.Run(t, taskB, otherBlueprintID, "event", "aborted", at(3))

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
				seed.Run(t, taskID, blueprintID, "manual", "completed", at(5))
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tasks, orgID, seed := mk(t)
				triggerID, blueprintID, _ := seed.Trigger(t, 2)
				entityID, taskID := seed.Task(t)
				seed.Run(t, taskID, blueprintID, "event", "failed", at(0))
				seed.Run(t, taskID, blueprintID, "event", "failed", at(1))
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
