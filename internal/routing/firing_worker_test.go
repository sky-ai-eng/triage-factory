package routing

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// stubDelegator records every Delegate call and creates a real
// blueprint_run row each time so MarkFired's FK to blueprint_runs(id) is
// satisfied. failFor names tasks whose Delegate refuses with a transient
// error, for the requeue and park cases.
type stubDelegator struct {
	db    *sql.DB
	calls int64

	// mu guards the things concurrent units write: lastRunTeamID, the team
	// the most recent Delegate call resolved for its run (tests assert a
	// firing runs as the acting team, not the task's creation-time owner),
	// the teardown ids below, and failFor.
	mu            sync.Mutex
	lastRunTeamID string
	failFor       map[string]bool
	// stopped is every blueprint-run id handed to StopBlueprintRun, so a test
	// can assert nothing tore a run down.
	stopped []string
	// tornDown is every task id handed to TeardownTaskArtifactsSystem, so a
	// test can assert a close reached the artifact pass.
	tornDown []string
}

var errStubDelegate = errors.New("stub: delegation refused")

func (s *stubDelegator) Delegate(task domain.Task, opts delegate.DelegateOpts) (string, error) {
	atomic.AddInt64(&s.calls, 1)
	s.mu.Lock()
	if s.failFor[task.ID] {
		s.mu.Unlock()
		return "", errStubDelegate
	}
	// Resolved the way production's Delegate resolves it: the firing's own
	// consolidation wins over the task's stored owner, which the firing
	// transaction has not rewritten yet at this point.
	s.lastRunTeamID = teamIDValue(&task)
	if opts.OwnerTeamID != "" {
		s.lastRunTeamID = opts.OwnerTeamID
	}
	s.mu.Unlock()
	return stubDelegateRun(s.db, task, opts)
}

// stubDelegateRun mirrors the production Delegate path for the router
// tests: it mints a blueprint_run (fenced on (triggering_event_id,
// trigger_id) for event triggers — the relocated replay fence) and a linked
// conversation row (conversations.blueprint_run_id is NOT NULL). Returns the
// blueprint_run id, or delegate.ErrAlreadyFired when an event replay trips
// the fence. No worktree/agent is stood up — only the DB rows the router's
// ErrAlreadyFired + run-count assertions read.
func stubDelegateRun(database *sql.DB, task domain.Task, opts delegate.DelegateOpts) (string, error) {
	store := sqlitestore.New(database)
	// opts.ExplicitBlueprintID is a blueprint id; the conversation row's prompt_id FK
	// needs a real prompt, so resolve the blueprint's first step prompt.
	promptID := opts.ExplicitBlueprintID
	if promptID != "" {
		if steps, err := store.Blueprints.ListSteps(context.Background(), runmode.LocalDefaultOrgID, opts.ExplicitBlueprintID); err == nil && len(steps) > 0 {
			promptID = steps[0].StepPromptID
		}
	}
	brID := uuid.New().String()
	br := domain.BlueprintRun{
		ID:                brID,
		BlueprintID:       opts.ExplicitBlueprintID,
		TaskID:            task.ID,
		TriggerType:       domain.BlueprintTriggerType(opts.TriggerType),
		TriggerID:         opts.TriggerID,
		TriggeringEventID: opts.TriggeringEventID,
		// Mirror production: freeze the caller-resolved actor onto the
		// blueprint_run so a router test can assert it matches the task claim.
		ActorAgentID: opts.ActorAgentID,
		Status:       domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-" + brID,
	}
	// The one firing door, exactly as production calls it: the claim, the
	// owner consolidation and step 0's conversation commit inside the run
	// insert's transaction, so a stub that split them would hide the very
	// coupling the router now depends on.
	stepIdx := 0
	inserted, _, _, err := store.Blueprints.CreateRunWithFirstStepSystem(context.Background(), runmode.LocalDefaultOrgID, br, opts.TaskClaim, opts.OwnerTeamID,
		domain.Conversation{
			ID: uuid.New().String(), TaskID: task.ID, PromptID: promptID, Model: "stub",
			TriggerType: opts.TriggerType, TriggerID: opts.TriggerID, CreatorUserID: opts.CreatorUserID,
			ActorAgentID: opts.ActorAgentID, BlueprintRunID: brID, BlueprintStepIndex: &stepIdx,
		})
	if err != nil {
		// Map the store sentinel the way production's Delegate does, so a
		// router test exercising the busy-task deferral sees the error the
		// router branches on rather than a raw store error.
		if errors.Is(err, dbpkg.ErrTaskBusyActiveRun) {
			return "", delegate.ErrTaskBusy
		}
		return "", err
	}
	if !inserted {
		return "", delegate.ErrAlreadyFired
	}
	return brID, nil
}

func (s *stubDelegator) StopConversationAndCancelBlueprint(orgID, conversationID, userID string, cause delegate.StopCause) error {
	return nil
}

// StopBlueprintRun records the id the caller asked to tear down.
func (s *stubDelegator) StopBlueprintRun(orgID, blueprintRunID string, cause delegate.StopCause) error {
	s.mu.Lock()
	s.stopped = append(s.stopped, blueprintRunID)
	s.mu.Unlock()
	return nil
}

// TeardownTaskArtifactsSystem records the task whose artifacts the close asked
// to retire. Recording only: what the real teardown writes is
// artifactteardown's to prove, and what a router test can prove is that the
// close cascade reached it with the task it closed.
func (s *stubDelegator) TeardownTaskArtifactsSystem(_ context.Context, orgID, taskID string) {
	s.mu.Lock()
	s.tornDown = append(s.tornDown, taskID)
	s.mu.Unlock()
}

// tornDownCopy is the task ids TeardownTaskArtifactsSystem was called with.
func (s *stubDelegator) tornDownCopy() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.tornDown))
	copy(out, s.tornDown)
	return out
}

// stoppedCopy is the blueprint-run ids StopBlueprintRun was called with.
func (s *stubDelegator) stoppedCopy() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.stopped))
	copy(out, s.stopped)
	return out
}

func (s *stubDelegator) StageOrDeliverAdditiveEvent(ctx context.Context, orgID, conversationID, producer, body string, prov domain.NoteProvenance, firing delegate.AdditiveFiringRef) delegate.InjectOutcome {
	return delegate.InjectNotDelivered
}

// setupDrainScenario seeds entity + prompt + event + task + trigger so a
// firing can be admitted and fired against a realistic FK graph.
func setupDrainScenario(t *testing.T, database *sql.DB) (entityID, taskID, triggerID, eventID string) {
	t.Helper()
	return setupDrainScenarioN(t, database, "1")
}

// setupDrainScenarioN is setupDrainScenario with a suffix, so a test can
// stand up several tasks on their own entities.
func setupDrainScenarioN(t *testing.T, database *sql.DB, suffix string) (entityID, taskID, triggerID, eventID string) {
	t.Helper()

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#"+suffix, "pr",
		"Test PR "+suffix, "https://github.com/owner/repo/pull/"+suffix)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	entityID = entity.ID

	createTestPrompt(t, database, domain.Prompt{
		ID: "p-drain-" + suffix, Name: "P", Body: "x", Source: "user",
	})

	eventID, err = sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entityID,
		DedupKey:     "build",
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}

	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entityID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	taskID = task.ID

	// The worker checks task.ClaimedByAgentID before firing. In production
	// the admission path (tryAutoDelegate) stamps the claim when a firing
	// lands; the test setup here admits the firing directly, so stamp the
	// claim explicitly with the local agent sentinel.
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if ok, err := testTaskStore(database).StampAgentClaimIfUnclaimed(t.Context(), runmode.LocalDefaultOrgID, taskID, runmode.LocalDefaultAgentID, runmode.LocalDefaultTeamID); err != nil || !ok {
		t.Fatalf("stamp claim: ok=%v err=%v", ok, err)
	}

	trig := domain.EventHandler{
		ID:                     "t-drain-" + suffix,
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "p-drain-" + suffix,
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              domain.EventGitHubPRCICheckFailed,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(0),
		Enabled:                true,
	}
	createTriggerForTestRouting(t, database, trig)
	triggerID = trig.ID
	return
}

// drainRouter builds a router over the database with the given firings
// store and delegator, and an executor identity so the claim has an owner.
func drainRouter(database *sql.DB, firings dbpkg.PendingFiringsStore, spawner Delegator) *Router {
	st := sqlitestore.New(database)
	r := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), st.Conversations, st.Entities, firings, st.Events, st.Orgs, st.Teams, nil, nil, nil, spawner, noopScorer{}, websocket.NewHub())
	r.SetExecutorID("firing-worker-test", 1)
	return r
}

// enqueueFiring admits a firing for the chain directly, the way the router's
// busy branch does.
func enqueueFiring(t *testing.T, database *sql.DB, entityID, taskID, triggerID, eventID string) {
	t.Helper()
	if _, _, err := sqlitestore.New(database).PendingFirings.Enqueue(t.Context(), runmode.LocalDefaultOrgID, entityID, taskID, triggerID, eventID, dbpkg.AgentClaimStamp{}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
}

// drainOnce runs one full drain pass synchronously.
func drainOnce(t *testing.T, r *Router) {
	t.Helper()
	if err := r.drainFiringQueue(context.Background()); err != nil {
		t.Fatalf("drainFiringQueue: %v", err)
	}
}

// firingsFor lists the entity's firings.
func firingsFor(t *testing.T, database *sql.DB, entityID string) []domain.PendingFiring {
	t.Helper()
	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list firings: %v", err)
	}
	return rows
}

// endTaskConversations ends every conversation on the task and settles its
// running blueprint, the way a terminal does: the task's gate reopens.
func endTaskConversations(t *testing.T, database *sql.DB, taskID string) {
	t.Helper()
	if _, err := database.Exec(`
		UPDATE conversations SET status = 'completed', completed_at = CURRENT_TIMESTAMP,
		       ended_at = CURRENT_TIMESTAMP, ended_reason = 'step_advanced'
		WHERE task_id = ? AND ended_at IS NULL
	`, taskID); err != nil {
		t.Fatalf("end conversations: %v", err)
	}
	if _, err := database.Exec(`
		UPDATE blueprint_runs SET status = 'completed', completed_at = CURRENT_TIMESTAMP
		WHERE task_id = ? AND status = 'running'
	`, taskID); err != nil {
		t.Fatalf("settle blueprint runs: %v", err)
	}
}

// ripenFirings clears every ready row's retry time, so a requeued row is
// claimable again without waiting out the backoff.
func ripenFirings(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`UPDATE pending_firings SET next_attempt_at = NULL WHERE status = 'ready'`); err != nil {
		t.Fatalf("ripen: %v", err)
	}
}

// expireFiringLease rewinds a leased row's lease into the past, standing in
// for a holder that died without a terminal write or lost a takeover.
func expireFiringLease(t *testing.T, database *sql.DB, firingID int64) {
	t.Helper()
	if _, err := database.Exec(`UPDATE pending_firings SET lease_expires_at = strftime('%Y-%m-%d %H:%M:%f','now','-1 hours') WHERE id = ? AND status = 'leased'`, firingID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
}

func requireSkipped(t *testing.T, f domain.PendingFiring, reason string) {
	t.Helper()
	if f.Status != workitem.StatusDone {
		t.Errorf("firing %d status = %q, want done", f.ID, f.Status)
	}
	if f.SkipReason != reason {
		t.Errorf("firing %d skip_reason = %q, want %q", f.ID, f.SkipReason, reason)
	}
	if f.FiredBlueprintRunID != nil {
		t.Errorf("firing %d fired_run_id = %v on a skipped row", f.ID, *f.FiredBlueprintRunID)
	}
}

func requireFired(t *testing.T, f domain.PendingFiring) {
	t.Helper()
	if f.Status != workitem.StatusDone || f.FiredBlueprintRunID == nil || f.SkipReason != "" {
		t.Errorf("firing %d = %+v, want done with a run and no skip reason", f.ID, f)
	}
}

// TestFiringWorker_ClosedTask: the worker re-validates task state at firing
// time. A firing whose task closed while it waited is skipped as task_closed
// rather than fired into a dead task.
func TestFiringWorker_ClosedTask(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	if _, err := testTaskStore(database).Close(t.Context(), runmode.LocalDefaultOrgID, taskID, "test_close", ""); err != nil {
		t.Fatalf("close task: %v", err)
	}
	stub := &stubDelegator{db: database}
	drainOnce(t, drainRouter(database, sqlitestore.New(database).PendingFirings, stub))

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	requireSkipped(t, rows[0], domain.PendingFiringSkipTaskClosed)
	if stub.calls != 0 {
		t.Errorf("Delegate called %d times on a closed task", stub.calls)
	}
}

// TestFiringWorker_SnoozedTask pins the semantic that snooze is a
// lifecycle-axis "do not act" signal, reached before the claim guard: a
// snooze releases the claim, and the skip reason names the lifecycle rather
// than the missing claim.
func TestFiringWorker_SnoozedTask(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	if _, err := database.Exec(
		`UPDATE tasks
		    SET status = 'snoozed', snooze_until = '2099-01-01 00:00:00',
		        claimed_by_agent_id = NULL, claimed_by_user_id = NULL
		  WHERE id = ?`,
		taskID,
	); err != nil {
		t.Fatalf("snooze task: %v", err)
	}
	drainOnce(t, drainRouter(database, sqlitestore.New(database).PendingFirings, &stubDelegator{db: database}))

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	requireSkipped(t, rows[0], domain.PendingFiringSkipTaskClosed)
}

// TestFiringWorker_DisabledTrigger: a trigger disabled while its firing
// waited must not fire.
func TestFiringWorker_DisabledTrigger(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	setTriggerEnabledForTestRouting(t, database, triggerID, false)
	drainOnce(t, drainRouter(database, sqlitestore.New(database).PendingFirings, &stubDelegator{db: database}))

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	requireSkipped(t, rows[0], domain.PendingFiringSkipTriggerDisabled)
}

// TestFiringWorker_BreakerTripped: a breaker at its threshold declines the
// firing rather than stacking another run on the entity-prompt pair.
func TestFiringWorker_BreakerTripped(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	// A threshold of zero trips on zero failures.
	if _, err := database.Exec(`UPDATE event_handlers SET breaker_threshold = 0 WHERE id = ?`, triggerID); err != nil {
		t.Fatalf("set breaker: %v", err)
	}
	drainOnce(t, drainRouter(database, sqlitestore.New(database).PendingFirings, &stubDelegator{db: database}))

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	requireSkipped(t, rows[0], domain.PendingFiringSkipBreakerTripped)
}

// TestFiringWorker_MultipleStaleFirings: three queued firings on one closed
// task all settle in one pass — a skip does not hold the batch up.
func TestFiringWorker_MultipleStaleFirings(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, _, eventID := setupDrainScenario(t, database)
	for i := 0; i < 3; i++ {
		promptID := []string{"p-1", "p-2", "p-3"}[i]
		createTestPrompt(t, database, domain.Prompt{ID: promptID, Name: promptID, Body: "x", Source: "user"})
		trigID := []string{"tr-1", "tr-2", "tr-3"}[i]
		createTriggerForTestRouting(t, database, domain.EventHandler{
			ID: trigID, Kind: domain.EventHandlerKindTrigger,
			BlueprintID: promptID, TriggerType: domain.TriggerTypeEvent,
			EventType:        domain.EventGitHubPRCICheckFailed,
			BreakerThreshold: intPtr(4), MinAutonomySuitability: floatPtr(0),
			Enabled: true,
		})
		enqueueFiring(t, database, entityID, taskID, trigID, eventID)
	}
	if _, err := testTaskStore(database).Close(t.Context(), runmode.LocalDefaultOrgID, taskID, "test_close", ""); err != nil {
		t.Fatalf("close task: %v", err)
	}
	drainOnce(t, drainRouter(database, sqlitestore.New(database).PendingFirings, &stubDelegator{db: database}))

	rows := firingsFor(t, database, entityID)
	if len(rows) != 3 {
		t.Fatalf("expected 3 firing rows, got %d", len(rows))
	}
	for _, r := range rows {
		requireSkipped(t, r, domain.PendingFiringSkipTaskClosed)
	}
}

// TestFiringWorker_EmptyQueue: a pass over nothing is a clean no-op.
func TestFiringWorker_EmptyQueue(t *testing.T) {
	database := newTestDB(t)
	entityID, _, _, _ := setupDrainScenario(t, database)
	stub := &stubDelegator{db: database}
	drainOnce(t, drainRouter(database, sqlitestore.New(database).PendingFirings, stub))
	if rows := firingsFor(t, database, entityID); len(rows) != 0 {
		t.Errorf("expected empty queue, got %d rows", len(rows))
	}
	if stub.calls != 0 {
		t.Errorf("Delegate called %d times on an empty queue", stub.calls)
	}
}

// TestFiringWorker_SecondFiringDefersAtNoCostThenFiresAfterTheEnd is the
// worker's replacement for "one fire per drain": two firings on one task,
// the first fires, the second meets the busy task and defers with its
// attempt refunded, and it fires once the conversation ends — on the wake,
// and on the scan tick alone.
func TestFiringWorker_SecondFiringDefersAtNoCostThenFiresAfterTheEnd(t *testing.T) {
	for _, mode := range []string{"wake", "scan"} {
		t.Run(mode, func(t *testing.T) {
			database := newTestDB(t)
			entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
			createTestPrompt(t, database, domain.Prompt{ID: "p-second", Name: "P2", Body: "x", Source: "user"})
			createTriggerForTestRouting(t, database, domain.EventHandler{
				ID: "t-second", Kind: domain.EventHandlerKindTrigger,
				BlueprintID: "p-second", TriggerType: domain.TriggerTypeEvent,
				EventType:        domain.EventGitHubPRCICheckFailed,
				BreakerThreshold: intPtr(4), MinAutonomySuitability: floatPtr(0),
				Enabled: true,
			})
			enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
			enqueueFiring(t, database, entityID, taskID, "t-second", eventID)

			stub := &stubDelegator{db: database}
			router := drainRouter(database, sqlitestore.New(database).PendingFirings, stub)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			scan := time.Hour
			if mode == "scan" {
				scan = 20 * time.Millisecond
			}
			go router.RunFiringQueue(ctx, scan)

			// The initial drain fires the first row and defers the second.
			waitFor(t, func() bool {
				rows := firingsFor(t, database, entityID)
				return len(rows) == 2 && rows[0].Status == workitem.StatusDone && rows[1].LastOutcome == "deferred"
			})
			rows := firingsFor(t, database, entityID)
			requireFired(t, rows[0])
			second := rows[1]
			if second.Status != workitem.StatusReady || second.Attempt != 0 || second.LastError != "task_busy" {
				t.Fatalf("second firing = %+v, want ready, refunded, deferred as task_busy", second)
			}
			if calls := atomic.LoadInt64(&stub.calls); calls != 2 {
				t.Fatalf("Delegate calls = %d, want 2 (one fire, one busy refusal)", calls)
			}

			// The second stays put while the first's conversation is live.
			if mode == "wake" {
				router.WakeFirings()
			}
			time.Sleep(60 * time.Millisecond)
			if got := firingsFor(t, database, entityID)[1]; got.Status != workitem.StatusReady {
				t.Fatalf("second firing = %+v while the task is busy, want still ready", got)
			}

			endTaskConversations(t, database, taskID)
			if mode == "wake" {
				router.WakeFirings()
			}
			waitFor(t, func() bool {
				return firingsFor(t, database, entityID)[1].Status == workitem.StatusDone
			})
			requireFired(t, firingsFor(t, database, entityID)[1])
			if got := firingsFor(t, database, entityID)[1].Attempt; got != 1 {
				t.Errorf("second firing charged %d attempts across the deferral and the fire, want 1", got)
			}
		})
	}
}

// TestFiringWorker_RunStillRunningAfterItsConversationEndsHoldsTheRow: a run
// is marked terminal only after its last conversation is, and between those
// two writes the task has no live conversation while the one-active-run index
// still refuses a second run. A drain landing there must leave the queued
// firing alone — not claim it into the refusal and charge it an attempt.
func TestFiringWorker_RunStillRunningAfterItsConversationEndsHoldsTheRow(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	createTestPrompt(t, database, domain.Prompt{ID: "p-second", Name: "P2", Body: "x", Source: "user"})
	createTriggerForTestRouting(t, database, domain.EventHandler{
		ID: "t-second", Kind: domain.EventHandlerKindTrigger,
		BlueprintID: "p-second", TriggerType: domain.TriggerTypeEvent,
		EventType:        domain.EventGitHubPRCICheckFailed,
		BreakerThreshold: intPtr(4), MinAutonomySuitability: floatPtr(0),
		Enabled: true,
	})
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	enqueueFiring(t, database, entityID, taskID, "t-second", eventID)

	stub := &stubDelegator{db: database}
	router := drainRouter(database, sqlitestore.New(database).PendingFirings, stub)
	drainOnce(t, router) // the first fires, the second defers behind it

	// The conversation reaches its terminal status; its run has not been
	// marked terminal yet.
	if _, err := database.Exec(`UPDATE conversations SET status = 'completed', completed_at = CURRENT_TIMESTAMP WHERE task_id = ?`, taskID); err != nil {
		t.Fatalf("complete conversation: %v", err)
	}
	callsBefore := atomic.LoadInt64(&stub.calls)
	drainOnce(t, router)
	second := firingsFor(t, database, entityID)[1]
	if second.Status != workitem.StatusReady || second.Attempt != 0 || second.LastOutcome != "deferred" {
		t.Fatalf("second firing inside the window = %+v, want ready, uncharged, still deferred", second)
	}
	if calls := atomic.LoadInt64(&stub.calls); calls != callsBefore {
		t.Fatalf("Delegate calls inside the window = %d, want %d: the row must not be claimed", calls, callsBefore)
	}

	// The run is marked terminal: the gate opens and the row fires on its
	// first charged attempt.
	endTaskConversations(t, database, taskID)
	drainOnce(t, router)
	second = firingsFor(t, database, entityID)[1]
	requireFired(t, second)
	if second.Attempt != 1 {
		t.Errorf("second firing charged %d attempts, want 1", second.Attempt)
	}
}

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// TestFiringWorker_LiveConversationIsNotClaimed: a ready firing for a task
// with a live conversation is not picked at all — no lease, no attempt, no
// Delegate call.
func TestFiringWorker_LiveConversationIsNotClaimed(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	if _, err := database.Exec(`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p-live', 'L', 'x', ?, ?)`,
		runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	dbtest.SeedConversation(t, database, domain.Conversation{ID: "c-live", TaskID: taskID, PromptID: "p-live", Model: "m", TriggerType: "event"})
	stub := &stubDelegator{db: database}
	drainOnce(t, drainRouter(database, sqlitestore.New(database).PendingFirings, stub))

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 || rows[0].Status != workitem.StatusReady || rows[0].Attempt != 0 || rows[0].LeaseOwner != "" {
		t.Fatalf("firing behind a live conversation = %+v, want untouched and ready", rows)
	}
	if stub.calls != 0 {
		t.Errorf("Delegate called %d times behind a live conversation", stub.calls)
	}
}

// TestFiringWorker_DelegateFailureRequeuesWithBackoff_NextTaskStillFires: a
// refused fire returns its row to ready with a backoff, and the next firing
// in the same pass, on another task, still fires.
func TestFiringWorker_DelegateFailureRequeuesWithBackoff_NextTaskStillFires(t *testing.T) {
	database := newTestDB(t)
	entityA, taskA, triggerA, eventA := setupDrainScenarioN(t, database, "a")
	entityB, taskB, triggerB, eventB := setupDrainScenarioN(t, database, "b")
	enqueueFiring(t, database, entityA, taskA, triggerA, eventA)
	enqueueFiring(t, database, entityB, taskB, triggerB, eventB)

	stub := &stubDelegator{db: database, failFor: map[string]bool{taskA: true}}
	drainOnce(t, drainRouter(database, sqlitestore.New(database).PendingFirings, stub))

	a := firingsFor(t, database, entityA)[0]
	if a.Status != workitem.StatusReady || a.Attempt != 1 || a.NextAttemptAt == nil || a.LastOutcome != "transient" || a.LastError == "" {
		t.Errorf("failed firing = %+v, want ready with a backoff and the transient outcome", a)
	}
	requireFired(t, firingsFor(t, database, entityB)[0])
}

// TestFiringWorker_FifthFailureParks: a firing that fails on every attempt
// parks after its budget, with the cause recorded for whoever redrives it.
func TestFiringWorker_FifthFailureParks(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	stub := &stubDelegator{db: database, failFor: map[string]bool{taskID: true}}
	router := drainRouter(database, sqlitestore.New(database).PendingFirings, stub)

	for attempt := 1; attempt <= 5; attempt++ {
		ripenFirings(t, database)
		drainOnce(t, router)
		f := firingsFor(t, database, entityID)[0]
		if f.Attempt != attempt {
			t.Fatalf("after pass %d attempt = %d", attempt, f.Attempt)
		}
		if attempt < 5 && f.Status != workitem.StatusReady {
			t.Fatalf("after pass %d status = %q, want ready with budget left", attempt, f.Status)
		}
	}
	f := firingsFor(t, database, entityID)[0]
	if f.Status != workitem.StatusParked || f.DoneAt == nil || f.LastError == "" || f.LastOutcome != "transient" {
		t.Errorf("after five failures = %+v, want parked with the last cause", f)
	}
	if calls := atomic.LoadInt64(&stub.calls); calls != 5 {
		t.Errorf("Delegate calls = %d, want 5", calls)
	}
}

// hookedFirings wraps the real store so a test can act on a row between the
// claim and the renewal, or before the terminal write — the windows a
// concurrent takeover or an operator can land in.
type hookedFirings struct {
	dbpkg.PendingFiringsStore
	afterClaim      func(ids []int64)
	beforeMarkFired func(id int64)
}

func (h hookedFirings) Claim(ctx context.Context, owner workitem.Owner, n int) (dbpkg.FiringClaim, error) {
	out, err := h.PendingFiringsStore.Claim(ctx, owner, n)
	if h.afterClaim != nil && len(out.Firings) > 0 {
		ids := make([]int64, 0, len(out.Firings))
		for _, cf := range out.Firings {
			ids = append(ids, cf.Firing.ID)
		}
		h.afterClaim(ids)
	}
	return out, err
}

func (h hookedFirings) MarkFired(ctx context.Context, r workitem.Receipt, blueprintRunID string) error {
	if h.beforeMarkFired != nil {
		h.beforeMarkFired(r.ItemID)
	}
	return h.PendingFiringsStore.MarkFired(ctx, r, blueprintRunID)
}

// TestFiringWorker_LeaseLostBeforeRenewalWritesNothing: a lease that lapses
// between the claim and the renewal ends the unit before any read or fire;
// the row is left for its next holder untouched.
func TestFiringWorker_LeaseLostBeforeRenewalWritesNothing(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	stub := &stubDelegator{db: database}
	firings := hookedFirings{
		PendingFiringsStore: sqlitestore.New(database).PendingFirings,
		afterClaim: func(ids []int64) {
			for _, id := range ids {
				expireFiringLease(t, database, id)
			}
		},
	}
	drainOnce(t, drainRouter(database, firings, stub))

	f := firingsFor(t, database, entityID)[0]
	if f.Status != workitem.StatusLeased || f.SkipReason != "" || f.FiredBlueprintRunID != nil || f.LastOutcome != "" {
		t.Errorf("firing after a lost lease = %+v, want the leased row untouched", f)
	}
	if stub.calls != 0 {
		t.Errorf("Delegate called %d times after the lease was lost", stub.calls)
	}
}

// TestFiringWorker_MarkFiredLosesLease_RunStandsAndReplaySkips pins the
// crash table's hardest row: the run committed and the terminal write lost
// its lease. Nothing tears the run down; the row's next holder replays, the
// fence answers, and the row ends done with skip_reason already_fired.
func TestFiringWorker_MarkFiredLosesLease_RunStandsAndReplaySkips(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	stub := &stubDelegator{db: database}
	lost := false
	firings := hookedFirings{
		PendingFiringsStore: sqlitestore.New(database).PendingFirings,
		beforeMarkFired: func(id int64) {
			if !lost {
				lost = true
				expireFiringLease(t, database, id)
			}
		},
	}
	router := drainRouter(database, firings, stub)
	drainOnce(t, router)

	runs := func() int {
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM blueprint_runs WHERE task_id = ?`, taskID).Scan(&n); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		return n
	}
	f := firingsFor(t, database, entityID)[0]
	if f.Status != workitem.StatusLeased || f.FiredBlueprintRunID != nil {
		t.Fatalf("firing after the lost terminal = %+v, want still leased with no run recorded", f)
	}
	if runs() != 1 || len(stub.stoppedCopy()) != 0 {
		t.Fatalf("runs = %d, torn down = %v; the committed run must stand", runs(), stub.stoppedCopy())
	}

	// The replay: the reclaim takes the row whatever the live conversation
	// says, and the fence — after the busy task's own deferral, if the mint
	// reports the live run first — answers already_fired.
	drainOnce(t, router)
	if f := firingsFor(t, database, entityID)[0]; f.Status != workitem.StatusDone {
		if f.Status != workitem.StatusReady || f.LastOutcome != "deferred" {
			t.Fatalf("replay left the firing as %+v, want deferred behind the live run or done", f)
		}
		endTaskConversations(t, database, taskID)
		drainOnce(t, router)
	}
	requireSkipped(t, firingsFor(t, database, entityID)[0], domain.PendingFiringSkipAlreadyFired)
	if runs() != 1 || len(stub.stoppedCopy()) != 0 {
		t.Errorf("runs = %d, torn down = %v after the replay; the fence must not duplicate or undo the run", runs(), stub.stoppedCopy())
	}
}

// TestFiringWorker_CancellationIsSettled: a request on a ready row settles
// at the next claim, and one on a leased row at the holder's renewal.
func TestFiringWorker_CancellationIsSettled(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	stub := &stubDelegator{db: database}
	real := sqlitestore.New(database).PendingFirings
	h := real.(dbpkg.WorkKindHandle)
	cancel := func(id int64) {
		if err := workitem.RequestCancel(context.Background(), h.Conn(), h.Kind(), runmode.LocalDefaultOrgID, id, "operator", "stop"); err != nil {
			t.Fatalf("RequestCancel: %v", err)
		}
	}

	// Ready: requested before the claim, settled by it.
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	cancel(firingsFor(t, database, entityID)[0].ID)
	drainOnce(t, drainRouter(database, real, stub))
	if f := firingsFor(t, database, entityID)[0]; f.Status != workitem.StatusCancelled {
		t.Fatalf("ready row with a request = %+v, want cancelled at the claim", f)
	}

	// Leased: requested between the claim and the renewal, settled there.
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	firings := hookedFirings{PendingFiringsStore: real, afterClaim: func(ids []int64) {
		for _, id := range ids {
			cancel(id)
		}
	}}
	drainOnce(t, drainRouter(database, firings, stub))
	rows := firingsFor(t, database, entityID)
	if f := rows[len(rows)-1]; f.Status != workitem.StatusCancelled {
		t.Fatalf("leased row with a request = %+v, want cancelled at the renewal", f)
	}
	if stub.calls != 0 {
		t.Errorf("Delegate called %d times for cancelled rows", stub.calls)
	}
}

// TestFiringWorker_ReclaimsADeadHoldersRowWithoutASweep: a firing whose
// holder died is fired by the first claim after its lease expires, with no
// boot and no sweep in between.
func TestFiringWorker_ReclaimsADeadHoldersRowWithoutASweep(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	// The dead holder: a claim by another worker whose lease then runs out.
	batch, err := sqlitestore.New(database).PendingFirings.Claim(t.Context(), workitem.Owner{ID: "dead-worker", Epoch: 1}, 1)
	if err != nil || len(batch.Firings) != 1 {
		t.Fatalf("claim as the dead worker: %+v err=%v", batch, err)
	}
	expireFiringLease(t, database, batch.Firings[0].Firing.ID)

	stub := &stubDelegator{db: database}
	drainOnce(t, drainRouter(database, sqlitestore.New(database).PendingFirings, stub))
	f := firingsFor(t, database, entityID)[0]
	requireFired(t, f)
	if f.Attempt != 2 {
		t.Errorf("attempt = %d, want the dead holder's charge plus the reclaim's", f.Attempt)
	}
}

// TestFiringWorker_FiresAsTheClaimedAgent: the run a queued firing produces
// is attributed to the agent holding the task's claim, and the task's claim
// is not re-stamped.
func TestFiringWorker_FiresAsTheClaimedAgent(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	drainOnce(t, drainRouter(database, sqlitestore.New(database).PendingFirings, &stubDelegator{db: database}))

	f := firingsFor(t, database, entityID)[0]
	requireFired(t, f)
	var actor sql.NullString
	if err := database.QueryRow(`SELECT actor_agent_id FROM blueprint_runs WHERE id = ?`, *f.FiredBlueprintRunID).Scan(&actor); err != nil {
		t.Fatalf("read run actor: %v", err)
	}
	if actor.String != runmode.LocalDefaultAgentID {
		t.Errorf("run actor = %q, want the task's claimed agent %q", actor.String, runmode.LocalDefaultAgentID)
	}
}
