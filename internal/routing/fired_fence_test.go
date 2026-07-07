package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// fenceStubDelegator mirrors the production spawner's event-path fence
// (delegate.Spawner.Delegate → BlueprintStore.CreateRunIfNotFiredSystem): the
// relocated replay fence lives on blueprint_runs, so it mints a blueprint_run
// fenced on (triggering_event_id, trigger_id) + a linked run row, and returns
// delegate.ErrAlreadyFired when the fence trips. This lets the router
// integration tests exercise the real ErrAlreadyFired handling in
// tryAutoDelegate without standing up a worktree + agent. A manual call
// (TriggerType != "event") inserts unconditionally, matching the spawner's
// manual branch.
type fenceStubDelegator struct {
	db    *sql.DB
	calls int64
}

func (s *fenceStubDelegator) Delegate(task domain.Task, opts delegate.DelegateOpts) (string, error) {
	atomic.AddInt64(&s.calls, 1)
	return stubDelegateRun(s.db, task, opts)
}

func (s *fenceStubDelegator) Cancel(orgID, runID, userID string) error { return nil }

// setupFenceScenario seeds entity + prompt + an immediate-fire trigger
// (min_autonomy=0) so HandleEvent reaches the immediate auto-delegate path.
// No claim is pre-stamped and agents/teamAgents are left nil in fenceRouter,
// so the bot-disabled gate is skipped and the fence — not a claim — is what
// the assertions isolate.
func setupFenceScenario(t *testing.T, database *sql.DB) (entityID string) {
	t.Helper()
	// The PR author resolves to the one team so the owning-team ladder picks
	// it as the owner — the owner's automation (the trigger below) then fires.
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#fence", "pr",
		"Fence PR", "https://github.com/owner/repo/pull/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	createTestPrompt(t, database, domain.Prompt{ID: "p-fence", Name: "P", Body: "x", Source: "user"})
	createTriggerForTestRouting(t, database, domain.EventHandler{
		ID:                     "t-fence",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "p-fence",
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              domain.EventGitHubPRCICheckFailed,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(0),
		Enabled:                true,
	})
	return entity.ID
}

// recordFenceEvent appends a fresh ci_check_failed event row and returns it
// with its persisted ID set — modeling the production path where the
// ingestor wrote the events row and the drain worker hands HandleEvent a
// pre-persisted event. Two calls produce two distinct event instances on the
// same (entity, type, dedup), which dedup to one task but fence independently.
func recordFenceEvent(t *testing.T, database *sql.DB, entityID string) domain.Event {
	t.Helper()
	meta := events.GitHubPRCICheckFailedMetadata{Author: "aidan", CheckName: "build", Repo: "owner/repo", HeadSHA: "abc123"}
	metaJSON, _ := json.Marshal(meta)
	evt := domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entityID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	}
	id, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, evt)
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	evt.ID = id
	return evt
}

func fenceRouter(database *sql.DB, spawner Delegator) *Router {
	st := sqlitestore.New(database)
	// Users wired so the author-centric ladder resolves the owner (whose
	// automation fires); teamRepos/jiraRules nil → scope gate fails open.
	return NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database),
		nil, nil, st.Users, testTaskStore(database), st.AgentRuns, st.Entities, st.PendingFirings, st.Events,
		st.Orgs, st.Teams, nil, nil, nil, spawner, noopScorer{}, websocket.NewHub())
}

func fenceRunCount(t *testing.T, database *sql.DB, entityID string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM runs r JOIN tasks t ON t.id = r.task_id WHERE t.entity_id = ?
	`, entityID).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return n
}

// fenceCompleteRuns moves every run on the entity to a terminal status,
// reopening the per-entity auto-run gate — exactly the boot-recovery
// window (the first run already went terminal) the replay re-fires into.
func fenceCompleteRuns(t *testing.T, database *sql.DB, entityID string) {
	t.Helper()
	if _, err := database.Exec(`
		UPDATE runs SET status = 'completed', completed_at = ?
		WHERE task_id IN (SELECT id FROM tasks WHERE entity_id = ?)
	`, time.Now(), entityID); err != nil {
		t.Fatalf("complete runs: %v", err)
	}
}

// TestHandleEvent_ReplayedEvent_FiresExactlyOnce is a regression test
// for the immediate-fire window: an event auto-fires a run, the run reaches
// terminal (reopening the per-entity gate), then the at-least-once router
// queue replays the SAME event after boot recovery. Pre-fence the replay
// passed the reopened gate and spawned a duplicate run; the
// (triggering_event_id, trigger_id) fence turns it into a clean skip.
func TestHandleEvent_ReplayedEvent_FiresExactlyOnce(t *testing.T) {
	database := newTestDB(t)
	entityID := setupFenceScenario(t, database)
	stub := &fenceStubDelegator{db: database}
	router := fenceRouter(database, stub)

	evt := recordFenceEvent(t, database, entityID)

	// First delivery: immediate fire creates run R1.
	router.HandleEvent(evt)
	if n := fenceRunCount(t, database, entityID); n != 1 {
		t.Fatalf("after first delivery: want 1 run, got %d", n)
	}

	// R1 goes terminal — the per-entity gate reopens.
	fenceCompleteRuns(t, database, entityID)

	// Replay the same event instance (same evt.ID, as boot recovery would).
	router.HandleEvent(evt)

	if n := fenceRunCount(t, database, entityID); n != 1 {
		t.Errorf("after replay: want exactly 1 run (fence dedups the replay), got %d", n)
	}
	// Delegate was attempted both times (the gate was open on the replay);
	// the fence, not the gate, is what prevented the duplicate run.
	if c := atomic.LoadInt64(&stub.calls); c != 2 {
		t.Errorf("want 2 Delegate attempts (fire + fenced replay), got %d", c)
	}
}

// TestHandleEvent_DistinctEvents_FireIndependently pins that the fence is
// per event instance, not per trigger: two distinct ci_check_failed events
// on the same entity (which dedup to one task) each fire their own run.
func TestHandleEvent_DistinctEvents_FireIndependently(t *testing.T) {
	database := newTestDB(t)
	entityID := setupFenceScenario(t, database)
	stub := &fenceStubDelegator{db: database}
	router := fenceRouter(database, stub)

	e1 := recordFenceEvent(t, database, entityID)
	router.HandleEvent(e1)
	// Terminal so the second event immediate-fires too (rather than queueing
	// behind an active run — that's the drain path, tested separately).
	fenceCompleteRuns(t, database, entityID)

	e2 := recordFenceEvent(t, database, entityID)
	if e2.ID == e1.ID {
		t.Fatal("expected two distinct event instances")
	}
	router.HandleEvent(e2)

	if n := fenceRunCount(t, database, entityID); n != 2 {
		t.Errorf("two distinct events on one entity: want 2 runs, got %d", n)
	}
}

// TestDrainEntity_AlreadyFiredRun_SkipsWithoutDuplicate covers the drain
// path's fence handling: a pending firing whose triggering event
// already has a committed run (a prior drain fired it but died before
// MarkFired, or the immediate path fired it before this firing was popped)
// must skip with reason "already_fired" rather than spawn a duplicate or
// retry forever.
func TestDrainEntity_AlreadyFiredRun_SkipsWithoutDuplicate(t *testing.T) {
	database := newTestDB(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	stub := &fenceStubDelegator{db: database}

	// A blueprint_run for (eventID, triggerID) already committed (the relocated
	// fence's firing unit), plus its step run — the prior firing that the drain
	// must detect as already-fired. Resolve the trigger's real blueprint id (the
	// trigger seed remapped it to a wrapping blueprint).
	var blueprintID string
	if err := database.QueryRow(`SELECT blueprint_id FROM event_handlers WHERE id = ?`, triggerID).Scan(&blueprintID); err != nil {
		t.Fatalf("resolve blueprint id: %v", err)
	}
	priorBlueprintRunID := uuid.New().String()
	inserted, err := sqlitestore.New(database).Blueprints.CreateRunIfNotFiredSystem(t.Context(), runmode.LocalDefaultOrgID, domain.BlueprintRun{
		ID:                priorBlueprintRunID,
		BlueprintID:       blueprintID,
		TaskID:            taskID,
		TriggerType:       domain.BlueprintTriggerEvent,
		TriggerID:         triggerID,
		TriggeringEventID: eventID,
		Status:            domain.BlueprintRunStatusRunning,
		WorktreePath:      "/tmp/wt-prior",
	})
	if err != nil || !inserted {
		t.Fatalf("seed prior blueprint_run: inserted=%v err=%v", inserted, err)
	}
	stepIdx := 0
	if err := sqlitestore.New(database).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrgID, domain.AgentRun{
		ID:                 uuid.New().String(),
		TaskID:             taskID,
		PromptID:           "p-drain",
		Status:             "completed",
		TriggerType:        "event",
		TriggerID:          triggerID,
		BlueprintRunID:     priorBlueprintRunID,
		BlueprintStepIndex: &stepIdx,
	}); err != nil {
		t.Fatalf("seed prior run: %v", err)
	}

	// Queue a firing carrying the same triggering event.
	if _, err := sqlitestore.New(database).PendingFirings.Enqueue(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, entityID, taskID, triggerID, eventID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	router := fenceRouter(database, stub)
	router.DrainEntity(runmode.LocalDefaultOrgID, entityID)

	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 firing row, got %d", len(rows))
	}
	if rows[0].Status != domain.PendingFiringStatusSkippedStale {
		t.Errorf("status = %q, want skipped_stale", rows[0].Status)
	}
	if rows[0].SkipReason != domain.PendingFiringSkipAlreadyFired {
		t.Errorf("skip_reason = %q, want %q", rows[0].SkipReason, domain.PendingFiringSkipAlreadyFired)
	}
	// Still exactly one run — the fence kept the drain from duplicating.
	if n := fenceRunCount(t, database, entityID); n != 1 {
		t.Errorf("want 1 run (drain fenced), got %d", n)
	}
}
