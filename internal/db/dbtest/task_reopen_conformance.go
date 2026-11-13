package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunTaskReopenConformance is the shared suite for the reopen the System mint
// doors carry (TaskStore.FindOrCreateAtSystem and
// FindOrCreateAtUnlessEntityActiveSystem): which closed task an event's
// recurrence returns to work, what the reopen writes, and every case where
// the mint creates a task instead. It runs on the held-close suite's stores
// and seeder, since a reopen and a held close are decided by the same working
// conversation.
//
// Most of the cases assert that the mint creates a task. A reopen that
// reaches a task it should have left closed puts a card back on the board
// that a person, a merge, or a reassignment resolved.
func RunTaskReopenConformance(t *testing.T, mk TaskCloseDeferralFactory) {
	t.Helper()
	ctx := context.Background()
	const ci = domain.EventGitHubPRCICheckFailed
	const passed = domain.EventGitHubPRCICheckPassed

	// closeBySibling closes the task as CI passing closes a CI-failure task.
	closeBySibling := func(t *testing.T, tasks db.TaskStore, orgID, taskID string) {
		t.Helper()
		if _, err := tasks.CloseSystem(ctx, orgID, taskID, db.SiblingEventCloseReason, passed); err != nil {
			t.Fatalf("CloseSystem(%s): %v", taskID, err)
		}
	}
	// mint is the router's mint of the CI-failure event on the task's entity.
	mint := func(t *testing.T, tasks db.TaskStore, orgID, entityID, eventID string) (*domain.Task, db.TaskMint) {
		t.Helper()
		task, outcome, err := tasks.FindOrCreateAtSystem(ctx, orgID, "", entityID, ci, "", eventID, 0.5, time.Now().UTC())
		if err != nil {
			t.Fatalf("FindOrCreateAtSystem: %v", err)
		}
		if task == nil {
			t.Fatal("FindOrCreateAtSystem returned no task")
		}
		return task, outcome
	}
	get := func(t *testing.T, tasks db.TaskStore, orgID, taskID string) domain.Task {
		t.Helper()
		task, err := tasks.GetSystem(ctx, orgID, taskID)
		if err != nil || task == nil {
			t.Fatalf("GetSystem(%s) = %v (err %v)", taskID, task, err)
		}
		return *task
	}
	// assertStillClosed fails when the task is not the closed row before was.
	assertStillClosed := func(t *testing.T, tasks db.TaskStore, orgID string, before domain.Task) {
		t.Helper()
		after := get(t, tasks, orgID, before.ID)
		if after.Status != before.Status || after.CloseReason != before.CloseReason || after.ClosedAt == nil || after.Version != before.Version {
			t.Errorf("closed task %s changed: status %q reason %q closed_at %v version %d, want %q %q set %d",
				before.ID, after.Status, after.CloseReason, after.ClosedAt, after.Version, before.Status, before.CloseReason, before.Version)
		}
	}

	t.Run("Reopens_the_task_a_sibling_event_closed_under_working_work", func(t *testing.T) {
		for _, claimed := range []bool{true, false} {
			stores, orgID, seed := mk(t)
			taskID := seed.Task(t)
			entityID := seed.EntityForTask(t, taskID)
			if claimed {
				if ok, err := stores.Tasks.ClaimQueuedForUser(ctx, orgID, taskID, stores.UserID, TaskVersion(t, stores.Tasks, orgID, taskID)); err != nil || !ok {
					t.Fatalf("ClaimQueuedForUser = %v (err %v)", ok, err)
				}
			}
			_, convID := seed.BlueprintAndConversation(t, taskID, "running", "")
			// An earlier event held its close, and the close that landed
			// left the hold on the row.
			heldEvent := seed.Event(t, taskID)
			if out, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, db.SiblingEventCloseReason, passed, heldEvent); err != nil || !out.Held {
				t.Fatalf("CloseOrHoldSystem = %+v (err %v), want held", out, err)
			}
			for _, producer := range []string{domain.StagedInjectionProducerTaskClosing, domain.StagedInjectionProducerPRCoherence} {
				if _, err := stores.StagedInjections.AppendSystem(ctx, orgID, domain.StagedInjection{
					ConversationID: convID, Subtype: domain.MessageSubtypeInjectionSystemNote,
					Producer: producer, Body: producer, Provenance: domain.NoteProvenance{EventID: heldEvent},
				}); err != nil {
					t.Fatalf("AppendSystem(%s): %v", producer, err)
				}
			}
			closeBySibling(t, stores.Tasks, orgID, taskID)
			closed := get(t, stores.Tasks, orgID, taskID)

			got, outcome := mint(t, stores.Tasks, orgID, entityID, seed.Event(t, taskID))

			if outcome != db.TaskReopened || got.ID != taskID {
				t.Fatalf("claimed=%v: mint = task %s outcome %v, want %s reopened", claimed, got.ID, outcome, taskID)
			}
			wantStatus, wantUser := "queued", ""
			if claimed {
				wantStatus, wantUser = "in_progress", stores.UserID
			}
			if got.Status != wantStatus || got.ClaimedByUserID != wantUser {
				t.Errorf("claimed=%v: reopened as status %q claimed by %q, want %q %q", claimed, got.Status, got.ClaimedByUserID, wantStatus, wantUser)
			}
			if got.ClosedAt != nil || got.CloseReason != "" || got.CloseEventType != "" {
				t.Errorf("claimed=%v: close columns survive the reopen: closed_at %v reason %q event type %q", claimed, got.ClosedAt, got.CloseReason, got.CloseEventType)
			}
			if got.Version != closed.Version+1 {
				t.Errorf("claimed=%v: version = %d, want %d: a reopen changes what a person acts on", claimed, got.Version, closed.Version+1)
			}
			AssertWriteReturnedStoredRow(t, "FindOrCreateAtSystem (reopen)", *got, func() (*domain.Task, error) {
				return stores.Tasks.GetSystem(ctx, orgID, taskID)
			})
			// The task is active again, so a hold left on it would be read.
			if d, err := stores.Tasks.CloseDeferralForTaskSystem(ctx, orgID, taskID); err != nil || d != nil {
				t.Errorf("claimed=%v: held close after the reopen = %+v (err %v), want none", claimed, d, err)
			}
			left, err := stores.StagedInjections.FlushPendingSystem(ctx, orgID, convID)
			if err != nil {
				t.Fatalf("FlushPendingSystem: %v", err)
			}
			if len(left) != 1 || left[0].Producer != domain.StagedInjectionProducerPRCoherence {
				t.Errorf("claimed=%v: still staged = %+v, want only the pull request note", claimed, left)
			}
			// The next event finds the task active.
			if again, outcome := mint(t, stores.Tasks, orgID, entityID, seed.Event(t, taskID)); outcome != db.TaskFound || again.ID != taskID {
				t.Errorf("claimed=%v: mint after the reopen = task %s outcome %v, want %s found", claimed, again.ID, outcome, taskID)
			}
		}
	})

	t.Run("No_working_conversation_creates_a_task", func(t *testing.T) {
		cases := []struct {
			name  string
			stage func(t *testing.T, seed TaskCloseDeferralSeeder, taskID string)
		}{
			{name: "no conversation", stage: func(*testing.T, TaskCloseDeferralSeeder, string) {}},
			{name: "concluded", stage: func(t *testing.T, seed TaskCloseDeferralSeeder, taskID string) {
				seed.BlueprintAndConversation(t, taskID, string(domain.BlueprintRunStatusCompleted), SeedConcluded)
			}},
			{name: "parked on a person", stage: func(t *testing.T, seed TaskCloseDeferralSeeder, taskID string) {
				seed.BlueprintAndConversation(t, taskID, "running", domain.StatusOpen)
			}},
			{name: "blueprint called off", stage: func(t *testing.T, seed TaskCloseDeferralSeeder, taskID string) {
				runID, _ := seed.BlueprintAndConversation(t, taskID, "running", "")
				seed.CallOff(t, runID)
			}},
		}
		for _, tc := range cases {
			stores, orgID, seed := mk(t)
			taskID := seed.Task(t)
			tc.stage(t, seed, taskID)
			closeBySibling(t, stores.Tasks, orgID, taskID)
			closed := get(t, stores.Tasks, orgID, taskID)

			got, outcome := mint(t, stores.Tasks, orgID, seed.EntityForTask(t, taskID), seed.Event(t, taskID))
			if outcome != db.TaskCreated || got.ID == taskID {
				t.Errorf("%s: mint = task %s outcome %v, want a new task", tc.name, got.ID, outcome)
			}
			assertStillClosed(t, stores.Tasks, orgID, closed)
		}
	})

	t.Run("Only_a_sibling_events_close_reopens", func(t *testing.T) {
		cases := []struct {
			name  string
			close func(t *testing.T, tasks db.TaskStore, orgID, taskID string)
		}{
			{name: "terminating", close: func(t *testing.T, tasks db.TaskStore, orgID, taskID string) {
				if _, err := tasks.CloseSystem(ctx, orgID, taskID, "entity_closed", domain.EventGitHubPRMerged); err != nil {
					t.Fatalf("CloseSystem: %v", err)
				}
			}},
			{name: "reconciled", close: func(t *testing.T, tasks db.TaskStore, orgID, taskID string) {
				if _, err := tasks.CloseSystem(ctx, orgID, taskID, "reconciled", ""); err != nil {
					t.Fatalf("CloseSystem: %v", err)
				}
			}},
			{name: "reassigned", close: func(t *testing.T, tasks db.TaskStore, orgID, taskID string) {
				if _, err := tasks.CloseSystem(ctx, orgID, taskID, db.ReassignedCloseReason, ci); err != nil {
					t.Fatalf("CloseSystem: %v", err)
				}
			}},
			{name: "run_completed", close: func(t *testing.T, tasks db.TaskStore, orgID, taskID string) {
				if _, err := tasks.CloseSystem(ctx, orgID, taskID, "run_completed", ""); err != nil {
					t.Fatalf("CloseSystem: %v", err)
				}
			}},
			{name: "duplicate merge", close: func(t *testing.T, tasks db.TaskStore, orgID, taskID string) {
				if _, err := tasks.CloseSystem(ctx, orgID, taskID, db.DuplicateEntityMergedCloseReason, ""); err != nil {
					t.Fatalf("CloseSystem: %v", err)
				}
			}},
			{name: "a person's complete", close: func(t *testing.T, tasks db.TaskStore, orgID, taskID string) {
				if _, err := tasks.CloseByUser(ctx, orgID, taskID, "done", TaskVersion(t, tasks, orgID, taskID)); err != nil {
					t.Fatalf("CloseByUser: %v", err)
				}
			}},
			{name: "a person's dismiss", close: func(t *testing.T, tasks db.TaskStore, orgID, taskID string) {
				if _, err := tasks.CloseByUser(ctx, orgID, taskID, "dismissed", TaskVersion(t, tasks, orgID, taskID)); err != nil {
					t.Fatalf("CloseByUser: %v", err)
				}
			}},
		}
		for _, tc := range cases {
			stores, orgID, seed := mk(t)
			taskID := seed.Task(t)
			seed.BlueprintAndConversation(t, taskID, "running", "")
			tc.close(t, stores.Tasks, orgID, taskID)
			closed := get(t, stores.Tasks, orgID, taskID)

			got, outcome := mint(t, stores.Tasks, orgID, seed.EntityForTask(t, taskID), seed.Event(t, taskID))
			if outcome != db.TaskCreated || got.ID == taskID {
				t.Errorf("%s: mint = task %s outcome %v, want a new task", tc.name, got.ID, outcome)
			}
			assertStillClosed(t, stores.Tasks, orgID, closed)
		}
	})

	t.Run("Only_the_newest_closed_task_reopens", func(t *testing.T) {
		// A working conversation on an older closed task reopens nothing
		// when a newer closed task with the key would not reopen itself.
		t.Run("older_stays_closed_behind_a_newer_one", func(t *testing.T) {
			stores, orgID, seed := mk(t)
			older := seed.Task(t)
			entityID := seed.EntityForTask(t, older)
			seed.BlueprintAndConversation(t, older, "running", "")
			closeBySibling(t, stores.Tasks, orgID, older)
			newer := seed.TaskOnEntity(t, entityID, ci)
			if _, err := stores.Tasks.CloseByUser(ctx, orgID, newer, "dismissed", TaskVersion(t, stores.Tasks, orgID, newer)); err != nil {
				t.Fatalf("CloseByUser: %v", err)
			}
			olderClosed, newerClosed := get(t, stores.Tasks, orgID, older), get(t, stores.Tasks, orgID, newer)

			got, outcome := mint(t, stores.Tasks, orgID, entityID, seed.Event(t, older))
			if outcome != db.TaskCreated || got.ID == older || got.ID == newer {
				t.Errorf("mint = task %s outcome %v, want a new task", got.ID, outcome)
			}
			assertStillClosed(t, stores.Tasks, orgID, olderClosed)
			assertStillClosed(t, stores.Tasks, orgID, newerClosed)
		})
		t.Run("newer_reopens_and_older_stays_closed", func(t *testing.T) {
			stores, orgID, seed := mk(t)
			older := seed.Task(t)
			entityID := seed.EntityForTask(t, older)
			seed.BlueprintAndConversation(t, older, "running", "")
			closeBySibling(t, stores.Tasks, orgID, older)
			newer := seed.TaskOnEntity(t, entityID, ci)
			seed.BlueprintAndConversation(t, newer, "running", "")
			closeBySibling(t, stores.Tasks, orgID, newer)
			olderClosed := get(t, stores.Tasks, orgID, older)

			got, outcome := mint(t, stores.Tasks, orgID, entityID, seed.Event(t, older))
			if outcome != db.TaskReopened || got.ID != newer {
				t.Errorf("mint = task %s outcome %v, want %s reopened", got.ID, outcome, newer)
			}
			assertStillClosed(t, stores.Tasks, orgID, olderClosed)
		})
	})

	t.Run("A_person_created_task_is_always_new", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		seed.BlueprintAndConversation(t, taskID, "running", "")
		closeBySibling(t, stores.Tasks, orgID, taskID)
		closed := get(t, stores.Tasks, orgID, taskID)

		got, created, err := stores.Tasks.FindOrCreate(ctx, orgID, "", seed.EntityForTask(t, taskID), ci, "", seed.Event(t, taskID), 0.5)
		if err != nil {
			t.Fatalf("FindOrCreate: %v", err)
		}
		if !created || got == nil || got.ID == taskID {
			t.Errorf("FindOrCreate = task %v created %v, want a new task", got, created)
		}
		assertStillClosed(t, stores.Tasks, orgID, closed)
	})

	t.Run("A_closed_entity_reopens_nothing", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		entityID := seed.EntityForTask(t, taskID)
		seed.BlueprintAndConversation(t, taskID, "running", "")
		closeBySibling(t, stores.Tasks, orgID, taskID)
		// The lifecycle task a terminating event rides, closed by a sibling
		// event with work still on it: the one type the mint admits here.
		rider := seed.TaskOnEntity(t, entityID, domain.EventGitHubPRMerged)
		seed.BlueprintAndConversation(t, rider, "running", "")
		closeBySibling(t, stores.Tasks, orgID, rider)
		seed.CloseEntityRaw(t, entityID)
		closed, riderClosed := get(t, stores.Tasks, orgID, taskID), get(t, stores.Tasks, orgID, rider)

		if task, _, err := stores.Tasks.FindOrCreateAtSystem(ctx, orgID, "", entityID, ci, "", seed.Event(t, taskID), 0.5, time.Now().UTC()); !errors.Is(err, db.ErrEntityClosed) || task != nil {
			t.Errorf("mint on the closed entity = task %v (err %v), want ErrEntityClosed", task, err)
		}
		assertStillClosed(t, stores.Tasks, orgID, closed)

		got, outcome, err := stores.Tasks.FindOrCreateAtSystem(ctx, orgID, "", entityID, domain.EventGitHubPRMerged, "", seed.Event(t, taskID), 0.5, time.Now().UTC())
		if err != nil || got == nil {
			t.Fatalf("mint the rider on the closed entity = %v (err %v)", got, err)
		}
		if outcome != db.TaskCreated || got.ID == rider {
			t.Errorf("rider mint = task %s outcome %v, want a new task", got.ID, outcome)
		}
		assertStillClosed(t, stores.Tasks, orgID, riderClosed)
	})

	t.Run("The_became_atomic_door_reopens", func(t *testing.T) {
		const atomic = domain.EventJiraIssueBecameAtomic
		stores, orgID, seed := mk(t)
		other := seed.Task(t)
		entityID := seed.EntityForTask(t, other)
		// The door suppresses on any active task, so the entity holds none.
		if _, err := stores.Tasks.CloseSystem(ctx, orgID, other, "run_completed", ""); err != nil {
			t.Fatalf("CloseSystem: %v", err)
		}
		taskID := seed.TaskOnEntity(t, entityID, atomic)
		seed.BlueprintAndConversation(t, taskID, "running", "")
		closeBySibling(t, stores.Tasks, orgID, taskID)

		got, outcome, suppressed, err := stores.Tasks.FindOrCreateAtUnlessEntityActiveSystem(ctx, orgID, "", entityID, atomic, "", seed.Event(t, taskID), 0.5, time.Now().UTC())
		if err != nil {
			t.Fatalf("FindOrCreateAtUnlessEntityActiveSystem: %v", err)
		}
		if suppressed || got == nil || got.ID != taskID || outcome != db.TaskReopened {
			t.Errorf("became_atomic mint = task %v outcome %v suppressed %v, want %s reopened", got, outcome, suppressed, taskID)
		}
	})

	// A reopen and a requeue both make a task active on the key. Whichever
	// lands second finds the other's active row or is refused by the dedup
	// index, and neither row is left half written.
	t.Run("A_requeue_and_a_reopen_leave_one_active_task", func(t *testing.T) {
		stage := func(t *testing.T) (stores TaskCloseDeferralStores, orgID, entityID, older, newer string, seed TaskCloseDeferralSeeder) {
			t.Helper()
			stores, orgID, seed = mk(t)
			older = seed.Task(t)
			entityID = seed.EntityForTask(t, older)
			if _, err := stores.Tasks.CloseByUser(ctx, orgID, older, "dismissed", TaskVersion(t, stores.Tasks, orgID, older)); err != nil {
				t.Fatalf("CloseByUser: %v", err)
			}
			newer = seed.TaskOnEntity(t, entityID, ci)
			seed.BlueprintAndConversation(t, newer, "running", "")
			closeBySibling(t, stores.Tasks, orgID, newer)
			return stores, orgID, entityID, older, newer, seed
		}
		t.Run("requeue_first", func(t *testing.T) {
			stores, orgID, entityID, older, newer, seed := stage(t)
			newerClosed := get(t, stores.Tasks, orgID, newer)
			if ok, err := stores.Tasks.RequeueTask(ctx, orgID, older, TaskVersion(t, stores.Tasks, orgID, older)); err != nil || !ok {
				t.Fatalf("RequeueTask = %v (err %v)", ok, err)
			}

			got, outcome := mint(t, stores.Tasks, orgID, entityID, seed.Event(t, older))
			if outcome != db.TaskFound || got.ID != older {
				t.Errorf("mint = task %s outcome %v, want the requeued %s found", got.ID, outcome, older)
			}
			assertStillClosed(t, stores.Tasks, orgID, newerClosed)
		})
		t.Run("reopen_first", func(t *testing.T) {
			stores, orgID, entityID, older, newer, seed := stage(t)
			olderClosed := get(t, stores.Tasks, orgID, older)
			if got, outcome := mint(t, stores.Tasks, orgID, entityID, seed.Event(t, older)); outcome != db.TaskReopened || got.ID != newer {
				t.Fatalf("mint = task %s outcome %v, want %s reopened", got.ID, outcome, newer)
			}
			reopened := get(t, stores.Tasks, orgID, newer)

			if ok, err := stores.Tasks.RequeueTask(ctx, orgID, older, olderClosed.Version); err == nil {
				t.Errorf("RequeueTask of a second task on the key = %v, want the dedup index's refusal", ok)
			}
			assertStillClosed(t, stores.Tasks, orgID, olderClosed)
			if after := get(t, stores.Tasks, orgID, newer); after.Status != reopened.Status || after.Version != reopened.Version {
				t.Errorf("reopened task changed under the refused requeue: status %q version %d, want %q %d", after.Status, after.Version, reopened.Status, reopened.Version)
			}
		})
	})
}
