package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EntityTerminalCloseFactory is what a per-backend test file hands to
// RunEntityTerminalCloseConformance: the wired entity and task stores, the
// orgID every method takes, and a seeder staging the graph the terminating
// close spans.
//
// Its own factory rather than a ride on EntityStoreFactory because the
// method under test writes across entities, tasks, task_events and
// blueprint_runs, and reads conversations: the fixtures are the ones
// TaskCloseCancelIntentSeeder already knows how to stage, plus the entity
// each task hangs off.
type EntityTerminalCloseFactory func(t *testing.T) (entities db.EntityStore, tasks db.TaskStore, orgID string, seed EntityTerminalCloseSeeder)

// EntityTerminalCloseSeeder stages what the terminating close spans and
// reads back what it wrote. The embedded cancel-intent seeder stages tasks
// (each on a fresh entity), events, blueprints and conversations; the extra
// callbacks reach the entity side.
type EntityTerminalCloseSeeder struct {
	TaskCloseCancelIntentSeeder

	// EntityForTask returns the entity a seeded task hangs off.
	EntityForTask func(t *testing.T, taskID string) (entityID string)

	// TaskOnEntity stages a second queued task of eventType on an entity a
	// prior Task call created, so a close can be watched across several
	// tasks of different types on one entity.
	TaskOnEntity func(t *testing.T, entityID, eventType string) (taskID string)

	// EntityRow reads an entity's state and poll_seq.
	EntityRow func(t *testing.T, entityID string) (state string, pollSeq int64)

	// BumpPollSeq advances an entity's poll_seq by one, standing in for a
	// poll that re-judged the entity after an event was enqueued — the
	// reopen race the version guard exists for.
	BumpPollSeq func(t *testing.T, entityID string)

	// CloseEntityRaw flips an entity to closed with a bare UPDATE, leaving
	// its tasks alone — the state no store write produces and the checker's
	// Count B exists to find.
	CloseEntityRaw func(t *testing.T, entityID string)
}

// RunEntityTerminalCloseConformance is the shared suite for
// EntityStore.CloseTerminalSystem — the terminating close: entity flip and
// task closes in one transaction, guarded on state and on the version the
// event was judged at — and for the mint guard on the other side of that
// serialization, TaskStore's refusal to mint on a closed entity.
//
// The declined cases carry the weight. A close that lands against the wrong
// version cancels work on a pull request someone just reopened, and a close
// that lands half — tasks flipped, entity not, or the reverse — is exactly
// the state the invariant forbids. So every guard miss is asserted as "nothing
// at all was written", against every table the transaction touches.
func RunEntityTerminalCloseConformance(t *testing.T, mk EntityTerminalCloseFactory) {
	t.Helper()
	ctx := context.Background()
	const ci = domain.EventGitHubPRCICheckFailed
	const review = domain.EventGitHubPRReviewChangesRequested

	t.Run("Closes_entity_and_every_open_task_in_one_call", func(t *testing.T) {
		entities, _, orgID, seed := mk(t)
		taskA := seed.Task(t)
		entityID := seed.EntityForTask(t, taskA)
		taskB := seed.TaskOnEntity(t, entityID, review)
		eventID := seed.Event(t, taskA)
		brID, convID := seed.BlueprintAndConversation(t, taskA, "running", "")
		_, seq := seed.EntityRow(t, entityID)

		res, err := entities.CloseTerminalSystem(ctx, orgID, entityID, &seq, []string{ci, review}, "entity_closed", domain.EventGitHubPRMerged, eventID)
		if err != nil {
			t.Fatalf("CloseTerminalSystem: %v", err)
		}
		if !res.Closed {
			t.Fatal("Closed = false at the entity's current version, want the close to land")
		}
		if len(res.ClosedTaskIDs) != 2 {
			t.Errorf("ClosedTaskIDs = %v, want both tasks", res.ClosedTaskIDs)
		}
		if got := res.ActiveConversationIDs[taskA]; len(got) != 1 || got[0] != convID {
			t.Errorf("active conversations for %s = %v, want exactly [%s]", taskA, got, convID)
		}
		if state, _ := seed.EntityRow(t, entityID); state != "closed" {
			t.Errorf("entity state = %q, want closed", state)
		}
		for _, id := range []string{taskA, taskB} {
			if got := seed.TaskStatus(t, id); got != "done" {
				t.Errorf("task %s status = %q, want done", id, got)
			}
		}
		if !seed.CancelRequested(t, brID) {
			t.Error("cancel_requested = false; the terminating close must carry the stop intent exactly as a typed close does")
		}
		if n := seed.CloseAuditCount(t, taskA); n != 1 {
			t.Errorf("close-audit rows on %s = %d, want 1", taskA, n)
		}
		if n := seed.CloseAuditCount(t, taskB); n != 1 {
			t.Errorf("close-audit rows on %s = %d, want 1", taskB, n)
		}
	})

	t.Run("Version_guard_miss_writes_nothing", func(t *testing.T) {
		// The reopen race: the event was judged at version N, and a poll
		// has since re-judged the entity. Nothing may land — not the task,
		// not its audit row, not the cancel intent, not the entity.
		entities, _, orgID, seed := mk(t)
		taskID := seed.Task(t)
		entityID := seed.EntityForTask(t, taskID)
		eventID := seed.Event(t, taskID)
		brID, _ := seed.BlueprintAndConversation(t, taskID, "running", "")
		_, judgedAt := seed.EntityRow(t, entityID)
		seed.BumpPollSeq(t, entityID)

		res, err := entities.CloseTerminalSystem(ctx, orgID, entityID, &judgedAt, []string{ci}, "entity_closed", domain.EventGitHubPRMerged, eventID)
		if err != nil {
			t.Fatalf("CloseTerminalSystem: %v", err)
		}
		if res.Closed || len(res.ClosedTaskIDs) != 0 {
			t.Errorf("result = %+v, want a declined close", res)
		}
		if state, _ := seed.EntityRow(t, entityID); state != "active" {
			t.Errorf("entity state = %q, want active — the close was judged at a version that no longer exists", state)
		}
		if got := seed.TaskStatus(t, taskID); got != "queued" {
			t.Errorf("task status = %q, want queued", got)
		}
		if seed.CancelRequested(t, brID) {
			t.Error("cancel_requested = true after a declined close; a reopened pull request's run was cancelled")
		}
		if n := seed.CloseAuditCount(t, taskID); n != 0 {
			t.Errorf("close-audit rows = %d, want 0", n)
		}
	})

	t.Run("Closed_entity_declines", func(t *testing.T) {
		entities, _, orgID, seed := mk(t)
		taskID := seed.Task(t)
		entityID := seed.EntityForTask(t, taskID)
		eventID := seed.Event(t, taskID)
		_, seq := seed.EntityRow(t, entityID)
		if res, err := entities.CloseTerminalSystem(ctx, orgID, entityID, &seq, []string{ci}, "entity_closed", domain.EventGitHubPRMerged, eventID); err != nil || !res.Closed {
			t.Fatalf("first close: res=%+v err=%v", res, err)
		}
		// A replay, or a straggler: the entity is closed, so nothing
		// happens — with the version the replay would carry and with none.
		_, seqAfter := seed.EntityRow(t, entityID)
		for _, expected := range []*int64{&seqAfter, nil} {
			res, err := entities.CloseTerminalSystem(ctx, orgID, entityID, expected, []string{ci}, "entity_closed", domain.EventGitHubPRMerged, eventID)
			if err != nil {
				t.Fatalf("second close: %v", err)
			}
			if res.Closed {
				t.Error("Closed = true on an already-closed entity")
			}
		}
	})

	t.Run("Missing_entity_declines", func(t *testing.T) {
		entities, _, orgID, _ := mk(t)
		res, err := entities.CloseTerminalSystem(ctx, orgID, "00000000-0000-0000-0000-00000000dead", nil, []string{ci}, "entity_closed", domain.EventGitHubPRMerged, "")
		if err != nil {
			t.Fatalf("CloseTerminalSystem: %v", err)
		}
		if res.Closed {
			t.Error("Closed = true for an entity that does not exist")
		}
	})

	t.Run("Nil_expected_is_guarded_on_state_alone", func(t *testing.T) {
		// The ingest-path event carries no version: it closes an active
		// entity whatever its poll_seq is.
		entities, _, orgID, seed := mk(t)
		taskID := seed.Task(t)
		entityID := seed.EntityForTask(t, taskID)
		seed.BumpPollSeq(t, entityID)
		seed.BumpPollSeq(t, entityID)

		res, err := entities.CloseTerminalSystem(ctx, orgID, entityID, nil, []string{ci}, "entity_closed", domain.EventJiraIssueUnreachable, "")
		if err != nil {
			t.Fatalf("CloseTerminalSystem: %v", err)
		}
		if !res.Closed || len(res.ClosedTaskIDs) != 1 {
			t.Errorf("result = %+v, want the entity closed with its one task", res)
		}
		if n := seed.CloseAuditCount(t, taskID); n != 0 {
			t.Errorf("close-audit rows = %d, want 0 — no closing event was named", n)
		}
	})

	t.Run("Close_types_narrow_which_tasks_close", func(t *testing.T) {
		// The terminating event's own type is excluded by its relation so a
		// lifecycle task riding a run survives; here the review task plays
		// that part.
		entities, _, orgID, seed := mk(t)
		taskA := seed.Task(t)
		entityID := seed.EntityForTask(t, taskA)
		survivor := seed.TaskOnEntity(t, entityID, review)
		_, seq := seed.EntityRow(t, entityID)

		res, err := entities.CloseTerminalSystem(ctx, orgID, entityID, &seq, []string{ci}, "entity_closed", domain.EventGitHubPRMerged, "")
		if err != nil {
			t.Fatalf("CloseTerminalSystem: %v", err)
		}
		if !res.Closed || len(res.ClosedTaskIDs) != 1 || res.ClosedTaskIDs[0] != taskA {
			t.Errorf("result = %+v, want exactly the CI task closed", res)
		}
		if got := seed.TaskStatus(t, survivor); got != "queued" {
			t.Errorf("task outside the close set status = %q, want queued", got)
		}
		if state, _ := seed.EntityRow(t, entityID); state != "closed" {
			t.Errorf("entity state = %q, want closed regardless of which tasks the set covers", state)
		}
	})

	t.Run("Entity_with_no_open_task_still_closes", func(t *testing.T) {
		entities, tasks, orgID, seed := mk(t)
		taskID := seed.Task(t)
		entityID := seed.EntityForTask(t, taskID)
		if _, err := tasks.CloseSystem(ctx, orgID, taskID, "user_done", ""); err != nil {
			t.Fatalf("pre-close the task: %v", err)
		}
		_, seq := seed.EntityRow(t, entityID)

		res, err := entities.CloseTerminalSystem(ctx, orgID, entityID, &seq, []string{ci}, "entity_closed", domain.EventGitHubPRMerged, "")
		if err != nil {
			t.Fatalf("CloseTerminalSystem: %v", err)
		}
		if !res.Closed {
			t.Error("Closed = false; a merged pull request with no live tasks still closes its entity")
		}
		if len(res.ClosedTaskIDs) != 0 {
			t.Errorf("ClosedTaskIDs = %v, want none — the task was already terminal", res.ClosedTaskIDs)
		}
	})

	t.Run("Mint_refuses_a_closed_entity", func(t *testing.T) {
		entities, tasks, orgID, seed := mk(t)
		taskID := seed.Task(t)
		entityID := seed.EntityForTask(t, taskID)
		eventID := seed.Event(t, taskID)
		_, seq := seed.EntityRow(t, entityID)
		if res, err := entities.CloseTerminalSystem(ctx, orgID, entityID, &seq, []string{ci}, "entity_closed", domain.EventGitHubPRMerged, eventID); err != nil || !res.Closed {
			t.Fatalf("close: res=%+v err=%v", res, err)
		}

		// The mint that lost the race to the close: it refuses rather than
		// strand a task on an entity nothing will close again — for every
		// variant, since each holds the same guard.
		_, _, err := tasks.FindOrCreateAtSystem(ctx, orgID, "", entityID, review, "", eventID, 0.5, time.Now().UTC())
		if !errors.Is(err, db.ErrEntityClosed) {
			t.Errorf("FindOrCreateAtSystem on a closed entity: err = %v, want ErrEntityClosed", err)
		}
		_, _, _, err = tasks.FindOrCreateAtUnlessEntityActiveSystem(ctx, orgID, "", entityID, review, "", eventID, 0.5, time.Now().UTC())
		if !errors.Is(err, db.ErrEntityClosed) {
			t.Errorf("FindOrCreateAtUnlessEntityActiveSystem on a closed entity: err = %v, want ErrEntityClosed", err)
		}
		_, _, err = tasks.FindOrCreateAtSystem(ctx, orgID, "", "00000000-0000-0000-0000-00000000dead", review, "", eventID, 0.5, time.Now().UTC())
		if !errors.Is(err, db.ErrEntityClosed) {
			t.Errorf("FindOrCreateAtSystem on a missing entity: err = %v, want ErrEntityClosed", err)
		}
		// And the refusal really minted nothing.
		if active, err := tasks.FindActiveByEntitySystem(ctx, orgID, entityID); err != nil || len(active) != 0 {
			t.Errorf("active tasks after a refused mint = %d (err %v), want 0", len(active), err)
		}

		// The one exemption: the lifecycle task the terminating transition
		// mints on the entity it just closed. It is minted, it is the only
		// open task on the entity, and the checker's read does not count it.
		riding, created, err := tasks.FindOrCreateAtSystem(ctx, orgID, "", entityID, domain.EventGitHubPRMerged, "", eventID, 0.5, time.Now().UTC())
		if err != nil || !created || riding == nil {
			t.Fatalf("mint the riding task on the closed entity: task=%v created=%v err=%v", riding, created, err)
		}
		if open, err := tasks.ListOpenOnClosedEntitiesSystem(ctx, orgID); err != nil || len(open) != 0 {
			t.Errorf("open-on-closed = %v (err %v), want none — the riding task lives on a closed entity by design", open, err)
		}
	})

	t.Run("ListOpenOnClosedEntitiesSystem_counts_only_stranded_tasks", func(t *testing.T) {
		_, tasks, orgID, seed := mk(t)
		// A task on an active entity: not counted.
		live := seed.Task(t)
		// A task on an entity closed around the guard: counted.
		stranded := seed.Task(t)
		seed.CloseEntityRaw(t, seed.EntityForTask(t, stranded))
		// A closed task on a closed entity: not counted.
		done := seed.Task(t)
		if _, err := tasks.CloseSystem(ctx, orgID, done, "user_done", ""); err != nil {
			t.Fatalf("close task: %v", err)
		}
		seed.CloseEntityRaw(t, seed.EntityForTask(t, done))

		got, err := tasks.ListOpenOnClosedEntitiesSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListOpenOnClosedEntitiesSystem: %v", err)
		}
		if len(got) != 1 || got[0] != stranded {
			t.Errorf("open-on-closed = %v, want exactly [%s]; %s is live and %s is closed", got, stranded, live, done)
		}
	})
}
