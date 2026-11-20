package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// PollReadinessReadyFactory hands the readiness suite a wired store, the orgID
// to scope every call, and a seeder for the stamps the store writes with its
// own clock.
type PollReadinessReadyFactory func(t *testing.T) (store db.PollReadinessStore, orgID string, seed PollReadinessSeeder)

// PollReadinessSeeder reads and writes a pair's stamps directly. A restart and
// a completion stamped with the same instant are what a coarse clock produces,
// and neither dialect's clock can be made to produce them on demand.
type PollReadinessSeeder struct {
	// RestartedAt returns the pair's stored restart stamp. The pair must have
	// been restarted.
	RestartedAt func(t *testing.T, orgID, source string) time.Time
	// SetLastPollAt overwrites the pair's stored completion stamp without
	// passing MarkPollComplete's gate. The row must exist.
	SetLastPollAt func(t *testing.T, orgID, source string, at time.Time)
}

// RunPollReadinessReadyConformance covers the readiness gate every backend
// impl must hold: not ready until a poll completes; a restart clears readiness
// until a poll started at or after it completes; a completion whose poll
// started before the restart is ignored; a completion with no start time is
// accepted; a poll started at the restart's own instant is ready; any stored
// completion is ready, whatever its stamp; and sources are independent.
func RunPollReadinessReadyConformance(t *testing.T, mk PollReadinessReadyFactory) {
	t.Helper()
	ctx := context.Background()

	ready := func(t *testing.T, store db.PollReadinessStore, orgID, source string) bool {
		t.Helper()
		ok, err := store.Ready(ctx, orgID, source)
		if err != nil {
			t.Fatalf("Ready(%s): %v", source, err)
		}
		return ok
	}
	complete := func(t *testing.T, store db.PollReadinessStore, orgID, source string, startedAt time.Time) {
		t.Helper()
		if err := store.MarkPollComplete(ctx, orgID, source, startedAt); err != nil {
			t.Fatalf("MarkPollComplete(%s, %v): %v", source, startedAt, err)
		}
	}
	restart := func(t *testing.T, store db.PollReadinessStore, orgID, source string) {
		t.Helper()
		if err := store.MarkRestarted(ctx, orgID, source); err != nil {
			t.Fatalf("MarkRestarted(%s): %v", source, err)
		}
	}

	t.Run("lifecycle", func(t *testing.T) {
		store, orgID, seed := mk(t)
		if ready(t, store, orgID, "jira") {
			t.Fatal("Ready before any poll = true, want false")
		}
		complete(t, store, orgID, "jira", time.Time{})
		if !ready(t, store, orgID, "jira") {
			t.Fatal("Ready after the first completion = false, want true")
		}

		restart(t, store, orgID, "jira")
		if ready(t, store, orgID, "jira") {
			t.Fatal("Ready right after a restart = true, want false")
		}
		restartedAt := seed.RestartedAt(t, orgID, "jira")

		complete(t, store, orgID, "jira", restartedAt.Add(-time.Minute))
		if ready(t, store, orgID, "jira") {
			t.Fatal("Ready after a completion whose poll started before the restart = true, want false")
		}
		complete(t, store, orgID, "jira", restartedAt.Add(time.Minute))
		if !ready(t, store, orgID, "jira") {
			t.Fatal("Ready after a completion whose poll started after the restart = false, want true")
		}

		if ready(t, store, orgID, "github") {
			t.Fatal("Ready for a source that never polled = true, want false")
		}
	})

	t.Run("a_poll_started_at_the_restart_instant_is_ready", func(t *testing.T) {
		store, orgID, seed := mk(t)
		restart(t, store, orgID, "jira")
		complete(t, store, orgID, "jira", seed.RestartedAt(t, orgID, "jira"))
		if !ready(t, store, orgID, "jira") {
			t.Fatal("Ready after a poll that started at the restart's instant = false, want true")
		}
	})

	t.Run("a_stored_completion_is_ready_whatever_its_stamp", func(t *testing.T) {
		// The read compares no stamps. A coarse clock stamps a completion with
		// the restart's own instant, and a completion whose stamp was taken
		// just before the restart can still be stored after it.
		store, orgID, seed := mk(t)
		restart(t, store, orgID, "jira")
		restartedAt := seed.RestartedAt(t, orgID, "jira")
		for _, at := range []time.Time{restartedAt, restartedAt.Add(-time.Millisecond)} {
			seed.SetLastPollAt(t, orgID, "jira", at)
			if !ready(t, store, orgID, "jira") {
				t.Fatalf("Ready with a completion stamped %v against a restart at %v = false, want true", at, restartedAt)
			}
		}
	})

	t.Run("a_completion_with_no_start_time_is_ready_after_a_restart", func(t *testing.T) {
		// A completion with no start time cannot be told from a fresh poll, and
		// refusing it would hold readiness shut until the next restart.
		store, orgID, _ := mk(t)
		complete(t, store, orgID, "jira", time.Time{})
		restart(t, store, orgID, "jira")
		complete(t, store, orgID, "jira", time.Time{})
		if !ready(t, store, orgID, "jira") {
			t.Fatal("Ready after a restart and a completion with no start time = false, want true")
		}
	})
}
