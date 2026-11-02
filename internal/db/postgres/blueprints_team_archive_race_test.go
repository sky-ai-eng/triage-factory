package postgres_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestBlueprintStore_Postgres_FiringRacesTeamArchive runs a firing and a team
// archive against each other, in both orders the share lock on the run's team
// allows. The archive stamps deleted_at and only then lists the team's
// conversations to stop, so the property under test is that the firing either
// commits before the stamp, and is listed, or reads the stamp and refuses —
// never commits a run the listing missed.
func TestBlueprintStore_Postgres_FiringRacesTeamArchive(t *testing.T) {
	h := pgtest.Shared(t)

	type fired struct {
		inserted bool
		convID   string
		err      error
	}
	setup := func(t *testing.T) (db.Stores, raceFixture, string, string) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		fx := newRaceFixture(t, h)
		taskID := fx.newTask(t)
		var teamID string
		if err := h.AdminDB.QueryRow(`SELECT team_id::text FROM tasks WHERE id = $1`, taskID).Scan(&teamID); err != nil {
			t.Fatalf("read the task's team: %v", err)
		}
		return stores, fx, taskID, teamID
	}
	// fireAsync starts the firing on its own goroutine. The firing is
	// composed here, because composing it seeds rows and fails the test on
	// error, which only the test goroutine may do.
	fireAsync := func(t *testing.T, stores db.Stores, fx raceFixture, taskID string) <-chan fired {
		t.Helper()
		br := fx.firing(t, taskID)
		br.ID = uuid.New().String()
		step := fx.firstStep(br)
		done := make(chan fired, 1)
		go func() {
			inserted, _, conv, _, err := stores.Blueprints.CreateRunWithFirstStepSystem(context.Background(), fx.orgID, br, db.AgentClaimStamp{}, "", step)
			r := fired{inserted: inserted, err: err}
			if conv != nil {
				r.convID = conv.ID
			}
			done <- r
		}()
		return done
	}
	// waitForLockWait polls until a backend running a statement that matches
	// pattern is blocked on a lock.
	waitForLockWait := func(t *testing.T, what, pattern string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting int
			if err := h.AdminDB.QueryRow(`
				SELECT count(*) FROM pg_stat_activity
				 WHERE wait_event_type = 'Lock' AND query LIKE $1
			`, pattern).Scan(&waiting); err != nil {
				t.Fatalf("read waiting backends: %v", err)
			}
			if waiting > 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never blocked", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	receive := func(t *testing.T, what string, ch <-chan fired) fired {
		t.Helper()
		select {
		case r := <-ch:
			return r
		case <-time.After(15 * time.Second):
			t.Fatalf("%s never returned", what)
			return fired{}
		}
	}

	t.Run("the_firing_locks_the_team_first", func(t *testing.T) {
		stores, fx, taskID, teamID := setup(t)
		ctx := context.Background()

		// Hold the firing between its team lock and its run insert: a SHARE
		// table lock blocks the insert's ROW EXCLUSIVE and nothing the firing
		// takes before it.
		blocker, err := h.AdminDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin the blocker: %v", err)
		}
		defer func() { _ = blocker.Rollback() }()
		if _, err := blocker.Exec(`LOCK TABLE blueprint_runs IN SHARE MODE`); err != nil {
			t.Fatalf("lock blueprint_runs: %v", err)
		}
		firing := fireAsync(t, stores, fx, taskID)
		waitForLockWait(t, "the firing's run insert", "%INSERT INTO blueprint_runs%")

		// The archive's write, behind the firing's share lock on the team.
		archived := make(chan error, 1)
		go func() {
			_, err := stores.Teams.Archive(ctx, teamID)
			archived <- err
		}()
		waitForLockWait(t, "the archive's write", "%UPDATE teams SET deleted_at = now()%")

		if err := blocker.Rollback(); err != nil {
			t.Fatalf("release the blocker: %v", err)
		}
		r := receive(t, "the firing", firing)
		if r.err != nil || !r.inserted || r.convID == "" {
			t.Fatalf("firing = (inserted %v, conv %q, %v), want committed", r.inserted, r.convID, r.err)
		}
		select {
		case err := <-archived:
			if err != nil {
				t.Fatalf("archive: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the archive never returned")
		}

		ids, err := stores.Conversations.ActiveIDsForTeamSystem(ctx, fx.orgID, teamID)
		if err != nil {
			t.Fatalf("ActiveIDsForTeamSystem: %v", err)
		}
		if !slices.Contains(ids, r.convID) {
			t.Errorf("the archive's listing = %v, want it to hold the firing's conversation %s", ids, r.convID)
		}
	})

	t.Run("the_archive_writes_the_team_first", func(t *testing.T) {
		stores, fx, taskID, teamID := setup(t)
		ctx := context.Background()

		// The archive's write, uncommitted, holding the team row.
		archive, err := h.AdminDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin the archive: %v", err)
		}
		defer func() { _ = archive.Rollback() }()
		if _, err := archive.Exec(`UPDATE teams SET deleted_at = now(), updated_at = now() WHERE id = $1 AND deleted_at IS NULL`, teamID); err != nil {
			t.Fatalf("archive the team: %v", err)
		}
		firing := fireAsync(t, stores, fx, taskID)
		waitForLockWait(t, "the firing's team lock", "%FROM teams WHERE org_id = $1 AND id = $2 FOR SHARE%")

		if err := archive.Commit(); err != nil {
			t.Fatalf("commit the archive: %v", err)
		}
		r := receive(t, "the firing", firing)
		if !errors.Is(r.err, db.ErrTeamArchived) {
			t.Fatalf("firing = (inserted %v, %v), want db.ErrTeamArchived", r.inserted, r.err)
		}

		ids, err := stores.Conversations.ActiveIDsForTeamSystem(ctx, fx.orgID, teamID)
		if err != nil {
			t.Fatalf("ActiveIDsForTeamSystem: %v", err)
		}
		if len(ids) != 0 {
			t.Errorf("the archived team holds conversations %v, want none", ids)
		}
		var runs int
		if err := h.AdminDB.QueryRow(`SELECT count(*) FROM blueprint_runs WHERE task_id = $1`, taskID).Scan(&runs); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		if runs != 0 {
			t.Errorf("blueprint_runs on the task = %d, want 0", runs)
		}
	})
}
