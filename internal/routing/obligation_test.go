package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// outage stands in for a dependency that is down for a while and then
// recovers: the first `remaining` calls fail, everything after succeeds.
// Single-worker drain, so no locking.
type outage struct{ remaining int }

func (o *outage) down() bool {
	if o.remaining > 0 {
		o.remaining--
		return true
	}
	return false
}

var errOutage = errors.New("boom: dependency is down")

// The four store wrappers below each fail exactly one of HandleEvent's
// routing-obligation stages, so a test can name the stage it is failing
// rather than infer it from a generic error.

type outageEntityStore struct {
	dbpkg.EntityStore
	o *outage
}

func (s outageEntityStore) GetSystem(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	if s.o.down() {
		return nil, errOutage
	}
	return s.EntityStore.GetSystem(ctx, orgID, id)
}

type outageHandlerStore struct {
	dbpkg.EventHandlerStore
	o *outage
}

func (s outageHandlerStore) GetEnabledForEventSystem(ctx context.Context, orgID, eventType string) ([]domain.EventHandler, error) {
	if s.o.down() {
		return nil, errOutage
	}
	return s.EventHandlerStore.GetEnabledForEventSystem(ctx, orgID, eventType)
}

type outageTaskStore struct {
	dbpkg.TaskStore
	o *outage
}

func (s outageTaskStore) FindOrCreateAtSystem(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (*domain.Task, bool, error) {
	if s.o.down() {
		return nil, false, errOutage
	}
	return s.TaskStore.FindOrCreateAtSystem(ctx, orgID, teamID, entityID, eventType, dedupKey, primaryEventID, defaultPriority, createdAt)
}

// outageDelegator fails the fire itself, standing in for the delegation tail
// behind fireMatchedTriggers. Delegating for real is fenceStubDelegator's job
// — it mints the fenced blueprint_run + run rows the replay assertions read.
type outageDelegator struct {
	*fenceStubDelegator
	o *outage
}

func (s outageDelegator) Delegate(task domain.Task, opts delegate.DelegateOpts) (string, error) {
	if s.o.down() {
		return "", errOutage
	}
	return s.fenceStubDelegator.Delegate(task, opts)
}

// mustAutoDelegate calls tryAutoDelegate and fails the test if it reports a
// dependency failure, returning the fired flag. Every call site that isn't
// itself exercising the error path goes through it: tryAutoDelegate's error
// return is what tells the queue worker an event is still owed, so a test
// that discards it would keep passing while a regression quietly turned one
// of these paths into a replayed — or eventually parked — event.
func mustAutoDelegate(t *testing.T, r *Router, task *domain.Task, trigger domain.EventHandler, entityID, triggeringEventID, actingTeamID string) bool {
	t.Helper()
	fired, err := r.tryAutoDelegate(context.Background(), runmode.LocalDefaultOrgID, task, trigger, entityID, triggeringEventID, actingTeamID)
	if err != nil {
		t.Fatalf("tryAutoDelegate: %v", err)
	}
	return fired
}

// queueRow reads the single event_queue row's terminal state.
func queueRow(t *testing.T, database *sql.DB) (status string, attempts int, lastErr string) {
	t.Helper()
	if err := database.QueryRow(`SELECT status, attempts, COALESCE(last_error, '') FROM event_queue`).
		Scan(&status, &attempts, &lastErr); err != nil {
		t.Fatalf("read the queue row: %v", err)
	}
	return status, attempts, lastErr
}

func activeTaskCount(t *testing.T, database *sql.DB, entityID string) int {
	t.Helper()
	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	return len(active)
}

// TestProcessQueuedEvent_ObligationFailure_RequeuesThenRoutesOnRecovery is
// the headline contract: a store failure at any routing-obligation stage
// leaves the event on the queue, and the event routes once the store
// recovers. Before this, every one of these bails reached MarkDone — the
// event was consumed with no task, no retry, and nothing to inspect.
//
// The recovery half is what makes the requeue worth having, and it also
// pins the replay's negative space: the second pass runs the FULL routing
// pass over the same event and must still leave exactly one task.
func TestProcessQueuedEvent_ObligationFailure_RequeuesThenRoutesOnRecovery(t *testing.T) {
	stages := []struct {
		stage   string
		disrupt func(r *Router, database *sql.DB, o *outage)
	}{
		{"entity resolution", func(r *Router, database *sql.DB, o *outage) {
			r.entities = outageEntityStore{EntityStore: sqlitestore.New(database).Entities, o: o}
		}},
		{"handler matching", func(r *Router, database *sql.DB, o *outage) {
			r.handlers = outageHandlerStore{EventHandlerStore: testEventHandlerStore(database), o: o}
		}},
		{"task upsert", func(r *Router, database *sql.DB, o *outage) {
			r.tasks = outageTaskStore{TaskStore: testTaskStore(database), o: o}
		}},
	}

	for _, tc := range stages {
		t.Run(tc.stage, func(t *testing.T) {
			database := newTestDB(t)
			r := newQueueWorkerRouter(t, database)
			o := &outage{remaining: 1}
			tc.disrupt(r, database, o)

			entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
				"github", "owner/repo#obligation", "pr", "PR", "https://example.com")
			if err != nil {
				t.Fatalf("create entity: %v", err)
			}
			enqueueCIFailed(t, database, entity.ID)

			// The stage fails: the event is still owed, so the row is
			// pending again with its attempt spent and the reason on it.
			if err := r.drainEventQueue(context.Background()); err != nil {
				t.Fatalf("drainEventQueue: %v", err)
			}
			status, attempts, lastErr := queueRow(t, database)
			if status != domain.QueuedEventStatusPending {
				t.Errorf("row after a failed %s = %q, want pending — a store failure must not consume the event", tc.stage, status)
			}
			if attempts != 1 {
				t.Errorf("attempts = %d, want 1", attempts)
			}
			if lastErr == "" {
				t.Error("last_error is empty; a requeued row must record why it did not route")
			}
			if n := activeTaskCount(t, database, entity.ID); n != 0 {
				t.Errorf("tasks after the failed pass = %d, want 0", n)
			}

			// The store recovers and the next pass routes the same event.
			if err := r.drainEventQueue(context.Background()); err != nil {
				t.Fatalf("drainEventQueue after recovery: %v", err)
			}
			status, attempts, _ = queueRow(t, database)
			if status != domain.QueuedEventStatusDone {
				t.Errorf("row after recovery = %q, want done", status)
			}
			if attempts != 2 {
				t.Errorf("attempts = %d, want 2 (the failed pass plus the one that routed)", attempts)
			}
			if n := activeTaskCount(t, database, entity.ID); n != 1 {
				t.Errorf("tasks after recovery = %d, want exactly 1 (the dedup index makes the replay a no-op)", n)
			}
		})
	}
}

// TestProcessQueuedEvent_FireFailure_RequeuesThenFiresOnce covers the
// obligation stage the other three can't reach: the task is already minted
// when the trigger's fire fails, so the retry runs against existing state.
// It must still end at exactly one task AND exactly one conversation — the
// task
// dedup index and the (event, trigger) replay fence each doing their half.
func TestProcessQueuedEvent_FireFailure_RequeuesThenFiresOnce(t *testing.T) {
	database := newTestDB(t)
	entityID := setupFenceScenario(t, database)
	o := &outage{remaining: 1}
	stub := outageDelegator{fenceStubDelegator: &fenceStubDelegator{db: database}, o: o}

	st := sqlitestore.New(database)
	r := fenceRouter(database, stub)
	r.SetEventQueue(st.EventQueue)

	// Same event shape as recordFenceEvent — the author resolves the owning
	// team, whose trigger is the one that fires — but enqueued durably, so the
	// drain worker (the thing that decides done vs. requeued) is what routes
	// it. enqueueCIFailed's authorless metadata would resolve no owner and
	// never reach a trigger.
	meta := events.GitHubPRCICheckFailedMetadata{Author: "aidan", CheckName: "build", Repo: "owner/repo", HeadSHA: "abc123"}
	metaJSON, _ := json.Marshal(meta)
	if _, err := st.EventQueue.Enqueue(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		OrgID:        runmode.LocalDefaultOrgID,
		EntityID:     &entityID,
		EventType:    domain.EventGitHubPRCICheckFailed,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
	}, ""); err != nil {
		t.Fatalf("enqueue ci_check_failed: %v", err)
	}

	// The fire fails. The task upsert ran before it, so a task exists —
	// but the trigger's obligation is unmet, so the row is not consumed.
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	status, attempts, lastErr := queueRow(t, database)
	if status != domain.QueuedEventStatusPending || attempts != 1 {
		t.Errorf("row after a failed fire = (%s, attempts %d), want (pending, 1)", status, attempts)
	}
	if lastErr == "" {
		t.Error("last_error is empty; a requeued row must record why it did not route")
	}
	if n := fenceConversationCount(t, database, entityID); n != 0 {
		t.Fatalf("conversations after the failed fire = %d, want 0", n)
	}

	// The spawner recovers: the replay re-runs every stage against the
	// task the first pass created, and fires the conversation that was owed.
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after recovery: %v", err)
	}
	if status, _, _ = queueRow(t, database); status != domain.QueuedEventStatusDone {
		t.Errorf("row after recovery = %q, want done", status)
	}
	if n := activeTaskCount(t, database, entityID); n != 1 {
		t.Errorf("tasks = %d, want exactly 1 — the replay must not mint a second", n)
	}
	if n := fenceConversationCount(t, database, entityID); n != 1 {
		t.Errorf("conversations = %d, want exactly 1", n)
	}

	// Now the negative space, with the conversation terminal so the task's
	// auto-run
	// gate is OPEN — otherwise a busy gate, not the fence, would be what
	// stopped the second conversation and this would prove nothing. Re-arm the
	// row and
	// route the same event a third time.
	fenceCompleteConversations(t, database, entityID)
	if _, err := database.Exec(`UPDATE event_queue SET status = 'pending', attempts = 0`); err != nil {
		t.Fatalf("re-arm the row: %v", err)
	}
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue on replay: %v", err)
	}
	if n := activeTaskCount(t, database, entityID); n != 1 {
		t.Errorf("tasks after replay = %d, want 1", n)
	}
	if n := fenceConversationCount(t, database, entityID); n != 1 {
		t.Errorf("conversations after replay = %d, want 1 (the (event, trigger) fence collapses it)", n)
	}
}

// TestProcessQueuedEvent_PersistentObligationFailure_ParksAfterBudget pins
// the poison-pill half: an event whose dependency never recovers must stop
// consuming worker cycles, and the row it leaves behind must still say why.
//
// It also pins the pacing that makes the budget mean anything: a requeue
// ends the drain pass. ClaimNext takes the oldest pending row, which is the
// one just requeued, so a pass that drained on would spend all five attempts
// in microseconds and park an event over a blip that had no time to clear.
func TestProcessQueuedEvent_PersistentObligationFailure_ParksAfterBudget(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	// Far more failures than the budget allows — the dependency never
	// comes back.
	o := &outage{remaining: 100}
	r.entities = outageEntityStore{EntityStore: sqlitestore.New(database).Entities, o: o}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", "owner/repo#poison", "pr", "PR", "https://example.com")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	enqueueCIFailed(t, database, entity.ID)

	for pass := 1; pass <= maxEventAttempts; pass++ {
		if err := r.drainEventQueue(context.Background()); err != nil {
			t.Fatalf("drain pass %d: %v", pass, err)
		}
		status, attempts, _ := queueRow(t, database)
		if attempts != pass {
			t.Fatalf("after pass %d, attempts = %d, want %d — one pass must spend exactly one attempt", pass, attempts, pass)
		}
		want := domain.QueuedEventStatusPending
		if pass == maxEventAttempts {
			want = domain.QueuedEventStatusFailed
		}
		if status != want {
			t.Fatalf("after pass %d, status = %q, want %q", pass, status, want)
		}
	}

	_, _, lastErr := queueRow(t, database)
	if lastErr == "" {
		t.Error("a parked row with no last_error is exactly the un-inspectable state this ticket exists to remove")
	}

	// Parked is terminal: the worker no longer picks the row up, so the
	// queue is drained and the rest of it is unblocked.
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain after park: %v", err)
	}
	if _, attempts, _ := queueRow(t, database); attempts != maxEventAttempts {
		t.Errorf("attempts = %d after draining past a parked row, want %d — a failed row must not be re-claimed", attempts, maxEventAttempts)
	}
}

// requeueFailingQueueStore fails the requeue write itself, which leaves the
// row stranded in 'processing' — reachable by neither ClaimNext nor this
// process's boot recovery.
type requeueFailingQueueStore struct{ dbpkg.EventQueueStore }

func (requeueFailingQueueStore) Requeue(ctx context.Context, orgID string, id int64, lastErr string) error {
	return errOutage
}

// TestDrainEventQueue_RequeueWriteFails_KeepsDraining pins that the
// stop-the-pass signal means "the row is pending again," not "something went
// wrong." A row whose requeue write failed is stuck in 'processing', so
// ClaimNext can't hand it back — stopping over it would strand every other
// pending row until the next scan tick for no benefit at all.
func TestDrainEventQueue_RequeueWriteFails_KeepsDraining(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	// Only the first event's entity load fails, so the second row is
	// perfectly routable and its fate is entirely the drain loop's choice.
	r.entities = outageEntityStore{EntityStore: sqlitestore.New(database).Entities, o: &outage{remaining: 1}}
	r.SetEventQueue(requeueFailingQueueStore{sqlitestore.New(database).EventQueue})

	first, second := seedTwoQueuedEvents(t, database)

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	var firstStatus, secondStatus string
	if err := database.QueryRow(`SELECT status FROM event_queue WHERE entity_id = ?`, first).Scan(&firstStatus); err != nil {
		t.Fatalf("read the first row: %v", err)
	}
	if err := database.QueryRow(`SELECT status FROM event_queue WHERE entity_id = ?`, second).Scan(&secondStatus); err != nil {
		t.Fatalf("read the second row: %v", err)
	}
	if firstStatus != domain.QueuedEventStatusProcessing {
		t.Errorf("first row = %q, want processing — a failed requeue write leaves it exactly where it was", firstStatus)
	}
	if secondStatus != domain.QueuedEventStatusDone {
		t.Errorf("second row = %q, want done — the pass must not stop over a row that can no longer be re-claimed", secondStatus)
	}
	if n := activeTaskCount(t, database, second); n != 1 {
		t.Errorf("tasks on the second entity = %d, want 1", n)
	}
}

// TestHandleEvent_BestEffortTailFailure_ConsumesTheEvent is the other side
// of the split: the tails hanging off a completed stage are recoverable by
// other means, so their failures must NOT replay a whole event. A visibility
// write that fails still leaves the task created and the row done.
func TestHandleEvent_BestEffortTailFailure_ConsumesTheEvent(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	r.tasks = visibilityFailingTaskStore{TaskStore: testTaskStore(database)}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", "owner/repo#tail", "pr", "PR", "https://example.com")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	enqueueCIFailed(t, database, entity.ID)

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	if status, _, _ := queueRow(t, database); status != domain.QueuedEventStatusDone {
		t.Errorf("row = %q, want done — a failed visibility write is repaired by the next event, not by replaying this one", status)
	}
	if n := activeTaskCount(t, database, entity.ID); n != 1 {
		t.Errorf("tasks = %d, want 1", n)
	}
}

// visibilityFailingTaskStore fails one of the writes that follow a
// successful task upsert — the widening of the task's visibility set, which
// a later event on the same entity re-attempts.
type visibilityFailingTaskStore struct{ dbpkg.TaskStore }

func (visibilityFailingTaskStore) SetVisibilityTeamsSystem(ctx context.Context, orgID, taskID string, teamIDs []string) error {
	return errOutage
}

// TestFireMatchedTriggers_OneTriggerFails_SiblingsStillCommit pins that a
// failing trigger doesn't cancel the others. Both teams match; team A's
// fire is broken. The replay the error asks for is fenced, so letting team
// B commit now costs nothing and losing its conversation would cost a
// conversation.
func TestFireMatchedTriggers_OneTriggerFails_SiblingsStillCommit(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	stores := sqlitestore.New(database)

	teamA := runmode.LocalDefaultTeamID
	teamB := seedTeam(t, database, "team-b-obligation")
	enableTeamAutoDelegate(t, database, teamA, teamB)
	seedImmediateTrigger(t, database, teamA, domain.EventJiraIssueAvailable, "oblig-a")
	seedImmediateTrigger(t, database, teamB, domain.EventJiraIssueAvailable, "oblig-b")

	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID,
		"jira", "SKY-oblig", "issue", "Obligation", "https://example.com/oblig")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	// Fail the first Delegate call only: the pool model orders teams
	// deterministically, so exactly one team's trigger hits the outage and
	// the other must still get its conversation.
	stub := outageDelegator{fenceStubDelegator: &fenceStubDelegator{db: database}, o: &outage{remaining: 1}}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database),
		stores.Agents, stores.TeamAgents, nil, testTaskStore(database), stores.Conversations, stores.Entities,
		stores.PendingFirings, stores.Events, stores.Orgs, stores.Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())

	err = router.HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventJiraIssueAvailable,
		EntityID:     &entity.ID,
		MetadataJSON: `{"issue_key":"SKY-oblig","project":"SKY","status":"To Do"}`,
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err == nil {
		t.Fatal("HandleEvent returned nil though a trigger's fire failed; the worker would consume the event and the conversation would never happen")
	}
	if n := activeTaskCount(t, database, entity.ID); n != 1 {
		t.Errorf("tasks = %d, want 1 (the upsert stage completed)", n)
	}
	if n := fenceConversationCount(t, database, entity.ID); n != 1 {
		t.Errorf("conversations = %d, want 1 — the sibling trigger must commit even though its neighbour failed", n)
	}
}
