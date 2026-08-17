package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SnoozeVisibilityFactory wires the two stores that jointly own the snooze
// gesture — SwipeStore writes the wake time, TaskStore is what filters on it —
// against one org, plus the seeder that produces a queued task to snooze.
//
// It is its own factory rather than an arm of SwipeStoreFactory or
// TaskStoreFactory because the invariant below lives in the seam between them:
// each store's own suite can pass while the pair is broken.
type SnoozeVisibilityFactory func(t *testing.T) (
	swipes db.SwipeStore,
	tasks db.TaskStore,
	orgID string,
	seedTask TaskSeederForSwipes,
)

// RunSnoozeVisibilityConformance pins the one thing a snooze is for: a task
// snoozed into the future is gone from the queue until its wake time.
//
// There is no sweeper — the list filter IS the wake path — so a filter that
// misreads the stored wake time is a snooze that does nothing at all, with no
// error anywhere and the task still sitting in the queue. That is what shipped:
// SQLite's DSN stores a bound time.Time with the writer's UTC offset attached
// while the filter compares against SQLite's own clock, which is UTC, so west
// of UTC every fresh snooze read as already expired.
//
// The cases below therefore assert on visibility, not on the stored bytes, and
// hand the store the same instant wearing different zones. Whether a task is
// hidden is a fact about the instant; if it also depends on the
// time.Time's Location, the offset is leaking into a comparison somewhere.
func RunSnoozeVisibilityConformance(t *testing.T, mk SnoozeVisibilityFactory) {
	t.Helper()
	ctx := context.Background()

	// UTC plus a fixed zone either side of it, applied to the wake time
	// regardless of what zone the test process runs in — so these are pinned
	// even on a UTC-only CI runner, and a sign error fails on one side while
	// passing on the other.
	zones := []struct {
		name string
		loc  *time.Location
	}{
		{"UTC", time.UTC},
		{"west of UTC (-04:00)", time.FixedZone("TEST-0400", -4*60*60)},
		{"east of UTC (+05:30)", time.FixedZone("TEST+0530", 5*60*60+30*60)},
	}

	// listed answers both questions a visibility assertion needs: whether this
	// particular task came back, and how many rows the filter matched overall
	// (the count query runs the same WHERE separately, so a filter fixed in one
	// and not the other shows up here).
	listed := func(t *testing.T, tasks db.TaskStore, orgID, taskID string, f db.TaskListFilter) (found bool, total int) {
		t.Helper()
		items, total, err := tasks.List(ctx, orgID, f, db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, task := range items {
			if task.ID == taskID {
				found = true
			}
		}
		return found, total
	}
	countListed := func(t *testing.T, tasks db.TaskStore, orgID string, f db.TaskListFilter) int {
		t.Helper()
		_, total := listed(t, tasks, orgID, "", f)
		return total
	}

	for _, z := range zones {
		t.Run("FutureSnoozeHidesTheTask/"+z.name, func(t *testing.T) {
			swipes, tasks, orgID, seedTask := mk(t)
			taskID := seedTask(t)

			// An hour out — the shortest preset the UI offers, and the one a
			// user notices immediately when it does nothing.
			until := time.Now().Add(time.Hour).In(z.loc)
			ok, err := swipes.SnoozeTask(ctx, orgID, taskID, until, 0)
			if err != nil {
				t.Fatalf("SnoozeTask: %v", err)
			}
			if !ok {
				t.Fatalf("SnoozeTask refused an unclaimed queued task")
			}

			if found, total := listed(t, tasks, orgID, taskID, db.TaskListFilter{}); found || total != 0 {
				t.Errorf("a task snoozed for an hour (wake time %s) is still in the default queue (total %d) — "+
					"the snooze is a no-op", until.Format(time.RFC3339Nano), total)
			}
			// The mirror: it is hidden, not gone. A filter that dropped the row
			// for some unrelated reason would pass the assertion above.
			if found, total := listed(t, tasks, orgID, taskID, db.TaskListFilter{IncludeSnoozed: true}); !found || total != 1 {
				t.Errorf("IncludeSnoozed does not surface the snoozed task (found=%v, total=%d)", found, total)
			}
		})

		t.Run("ElapsedSnoozeWakesTheTask/"+z.name, func(t *testing.T) {
			swipes, tasks, orgID, seedTask := mk(t)
			taskID := seedTask(t)

			// A wake time already past: the row is snoozed, but its hour is up.
			// This is the half a broken comparison passes by accident when the
			// offset happens to push every value into the past.
			until := time.Now().Add(-time.Hour).In(z.loc)
			if _, err := swipes.SnoozeTask(ctx, orgID, taskID, until, 0); err != nil {
				t.Fatalf("SnoozeTask: %v", err)
			}

			if found, total := listed(t, tasks, orgID, taskID, db.TaskListFilter{}); !found || total != 1 {
				t.Errorf("a task whose wake time (%s) has passed is still hidden (found=%v, total=%d) — "+
					"it would never come back", until.Format(time.RFC3339Nano), found, total)
			}
		})
	}

	t.Run("SnoozeIsNotAffectedByTheCallersZone", func(t *testing.T) {
		// The same instant, handed over three ways. Visibility must not move.
		swipes, tasks, orgID, seedTask := mk(t)
		base := time.Now().Add(90 * time.Minute)
		for _, z := range zones {
			taskID := seedTask(t)
			if _, err := swipes.SnoozeTask(ctx, orgID, taskID, base.In(z.loc), 0); err != nil {
				t.Fatalf("SnoozeTask (%s): %v", z.name, err)
			}
		}
		if visible := countListed(t, tasks, orgID, db.TaskListFilter{}); visible != 0 {
			t.Errorf("%d of 3 tasks snoozed to the same instant are visible — visibility depends on the "+
				"caller's Location, so an offset is reaching the comparison", visible)
		}
		if all := countListed(t, tasks, orgID, db.TaskListFilter{IncludeSnoozed: true}); all != 3 {
			t.Errorf("IncludeSnoozed total = %d, want 3", all)
		}
	})

	t.Run("SnoozeUntilRoundTripsAsAnInstant", func(t *testing.T) {
		// The stored value read back is the same instant that went in. Read
		// through Get rather than the raw column so this holds for both
		// dialects' scan paths; the second-level tolerance is SQLite's
		// millisecond storage precision plus the driver's own truncation.
		swipes, tasks, orgID, seedTask := mk(t)
		taskID := seedTask(t)
		until := time.Now().Add(3 * time.Hour).In(time.FixedZone("TEST-0400", -4*60*60))
		if _, err := swipes.SnoozeTask(ctx, orgID, taskID, until, 0); err != nil {
			t.Fatalf("SnoozeTask: %v", err)
		}
		task, err := tasks.Get(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if task == nil {
			t.Fatal("Get returned nil for the snoozed task")
		}
		got := snoozeUntilOf(t, task)
		if delta := got.Sub(until); delta > time.Second || delta < -time.Second {
			t.Errorf("snooze_until round-tripped to %s, want %s (delta %s) — the stored value is a "+
				"different instant, which is the offset being read as wall clock",
				got.UTC().Format(time.RFC3339Nano), until.UTC().Format(time.RFC3339Nano), delta)
		}
	})
}

// snoozeUntilOf reads the wake time off a task, failing if it is unset — a
// snoozed row without one never wakes, which the stores refuse to write.
func snoozeUntilOf(t *testing.T, task *domain.Task) time.Time {
	t.Helper()
	if task.SnoozeUntil == nil {
		t.Fatal("snoozed task has no snooze_until")
	}
	return *task.SnoozeUntil
}
