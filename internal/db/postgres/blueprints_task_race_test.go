package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// raceFixture is one entity plus a factory for firings against a chosen task
// on it. Each firing carries its own blueprint, trigger and triggering event
// (event_handlers_one_trigger_per_blueprint means each trigger needs its own
// blueprint), so the (triggering_event_id, trigger_id) replay fence never
// engages and the only thing that can stop a firing is the one-active-run
// index under test.
type raceFixture struct {
	orgID    string
	entityID string
	newTask  func(t *testing.T) string
	firing   func(t *testing.T, taskID string) domain.BlueprintRun
}

func newRaceFixture(t *testing.T, h *pgtest.Harness) raceFixture {
	t.Helper()
	orgID, userID := seedPgOrgForBlueprints(t, h)
	teamID := seedPgDefaultTeam(t, h, orgID, userID)
	entityID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Race Entity', '', '{}'::jsonb, now())
	`, entityID, orgID, "owner/repo#race-"+entityID[:8]); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	newEvent := func(t *testing.T) string {
		t.Helper()
		eventID := uuid.New().String()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
		`, eventID, orgID, entityID, domain.EventGitHubPRCICheckFailed); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		return eventID
	}

	newTask := func(t *testing.T) string {
		t.Helper()
		taskID := uuid.New().String()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO tasks (id, org_id, creator_user_id, team_id, entity_id, event_type, dedup_key, primary_event_id, status, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued', now())
		`, taskID, orgID, userID, teamID, entityID, domain.EventGitHubPRCICheckFailed, "dedup-"+taskID[:8], newEvent(t)); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		return taskID
	}

	firing := func(t *testing.T, taskID string) domain.BlueprintRun {
		t.Helper()
		blueprintID := uuid.New().String()
		seedPgBlueprint(t, h, orgID, userID, blueprintID)
		triggerID := uuid.New().String()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO event_handlers (id, org_id, team_id, creator_user_id, kind, event_type, enabled, source, blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'trigger', $5, true, 'user', $6, 4, 0, now(), now())
		`, triggerID, orgID, teamID, userID, domain.EventGitHubPRCICheckFailed, blueprintID); err != nil {
			t.Fatalf("seed trigger: %v", err)
		}
		return domain.BlueprintRun{
			BlueprintID:       blueprintID,
			TaskID:            taskID,
			TriggerType:       domain.BlueprintTriggerEvent,
			TriggerID:         triggerID,
			TriggeringEventID: newEvent(t),
			WorktreePath:      "/tmp/wt-race",
			StepPlan:          []domain.BlueprintPlanStep{{StepIndex: 0, PromptID: "p", PromptName: "p", PromptBody: "b"}},
		}
	}
	return raceFixture{orgID: orgID, entityID: entityID, newTask: newTask, firing: firing}
}

// TestBlueprintStore_Postgres_OneActiveAutoRunPerTask: the task gate
// (HasActiveAutoRunForTaskSystem) is check-then-act, so two different
// (event, trigger) pairs racing to auto-fire on the SAME task could both
// pass the check and each mint an active blueprint_run —
// blueprint_runs_one_active_auto_run_per_task is the DB-enforced backstop.
//
// Several distinct (trigger, triggering_event) pairs on one task fire
// CreateRunIfNotFiredSystem CONCURRENTLY (real goroutines behind a start
// barrier, racing genuinely simultaneous transactions — not sequential
// calls, which a unique index would trivially survive regardless of whether
// the constraint is actually correct): exactly one may land; every loser
// reports the task-busy sentinel, not inserted=false (which means "replay,
// permanently satisfied" and would silently drop the loser's intent) and
// never a raw unique-violation error.
func TestBlueprintStore_Postgres_OneActiveAutoRunPerTask(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	fx := newRaceFixture(t, h)
	taskID := fx.newTask(t)

	const racers = 6
	firings := make([]domain.BlueprintRun, racers)
	for i := range firings {
		firings[i] = fx.firing(t, taskID)
	}

	insertedFlags := make([]bool, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	ready.Add(racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			insertedFlags[i], errs[i] = stores.Blueprints.CreateRunIfNotFiredSystem(ctx, fx.orgID, firings[i])
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	winners := 0
	for i, ins := range insertedFlags {
		if ins {
			if errs[i] != nil {
				t.Fatalf("racer %d: winner returned an error: %v", i, errs[i])
			}
			winners++
			continue
		}
		if !errors.Is(errs[i], db.ErrTaskBusyActiveAutoRun) {
			t.Fatalf("racer %d: loser must surface ErrTaskBusyActiveAutoRun (defer, don't drop), got inserted=false err=%v", i, errs[i])
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly 1 racer to insert under concurrency, got %d (double-mint or all-lost)", winners)
	}

	var count int
	if err := h.AdminDB.QueryRow(`
		SELECT COUNT(*) FROM blueprint_runs
		WHERE org_id = $1 AND task_id = $2 AND trigger_type = 'event' AND status = 'running'
	`, fx.orgID, taskID).Scan(&count); err != nil {
		t.Fatalf("count active runs: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 active auto run on the task, got %d", count)
	}

	// Once the (single winning) run terminates, a new firing on the task is
	// allowed again — the index only excludes status='running'.
	if _, err := h.AdminDB.Exec(`
		UPDATE blueprint_runs SET status = 'completed'
		WHERE org_id = $1 AND task_id = $2 AND status = 'running'
	`, fx.orgID, taskID); err != nil {
		t.Fatalf("complete winning run: %v", err)
	}
	insertedNext, err := stores.Blueprints.CreateRunIfNotFiredSystem(ctx, fx.orgID, fx.firing(t, taskID))
	if err != nil {
		t.Fatalf("CreateRunIfNotFiredSystem after termination: %v", err)
	}
	if !insertedNext {
		t.Fatal("firing after the task's active run terminated should insert")
	}
}

// TestBlueprintStore_Postgres_SiblingTasksOnOneEntityBothFire is the other
// half of re-keying the index: two tasks on one pull request are two
// different situations, and each may have an agent in flight. Under the
// former entity-keyed index the second insert here lost to the first and
// its intent had to be queued behind a run that had nothing to do with it.
func TestBlueprintStore_Postgres_SiblingTasksOnOneEntityBothFire(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	fx := newRaceFixture(t, h)
	taskA, taskB := fx.newTask(t), fx.newTask(t)

	for _, tc := range []struct {
		name   string
		taskID string
	}{{"first task", taskA}, {"sibling task", taskB}} {
		inserted, err := stores.Blueprints.CreateRunIfNotFiredSystem(ctx, fx.orgID, fx.firing(t, tc.taskID))
		if err != nil {
			t.Fatalf("%s: CreateRunIfNotFiredSystem: %v", tc.name, err)
		}
		if !inserted {
			t.Fatalf("%s: firing should insert — a busy sibling task must not block it", tc.name)
		}
	}

	var count int
	if err := h.AdminDB.QueryRow(`
		SELECT COUNT(*) FROM blueprint_runs
		WHERE org_id = $1 AND entity_id = $2 AND trigger_type = 'event' AND status = 'running'
	`, fx.orgID, fx.entityID).Scan(&count); err != nil {
		t.Fatalf("count active runs: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 concurrent auto runs on the entity (one per task), got %d", count)
	}
}
