package postgres_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestBlueprintStore_Postgres_OneActiveAutoRunPerEntity is TFAC-579 item 2:
// the entity gate (HasActiveAutoRunForEntitySystem) is check-then-act, so
// two different (event, trigger) pairs racing to auto-fire on the SAME
// entity could both pass the check and each mint an active blueprint_run —
// the blueprint_runs_one_active_auto_run_per_entity partial unique index is
// the DB-enforced backstop. Several distinct (trigger, triggering_event)
// pairs on one entity fire CreateRunIfNotFiredSystem CONCURRENTLY (real
// goroutines behind a start barrier, racing genuinely simultaneous
// transactions — not sequential calls, which a unique index would trivially
// survive regardless of whether the constraint or its entity_id backfill
// subquery is actually correct): exactly one may land; every loser reports
// inserted=false with no error (the same "clean skip" contract as the
// existing event/trigger fence), not a raw unique-violation error.
func TestBlueprintStore_Postgres_OneActiveAutoRunPerEntity(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgOrgForBlueprints(t, h)
	teamID := seedPgDefaultTeam(t, h, orgID, userID)
	entityID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Race Entity', '', '{}'::jsonb, now())
	`, entityID, orgID, "owner/repo#race-"+entityID[:8]); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	// Several distinct tasks on the SAME entity, each with its own blueprint
	// (event_handlers_one_trigger_per_blueprint means each trigger needs
	// its own blueprint), trigger, and triggering event — independent
	// "firing attempts" that must not all mint an active run.
	mkFiring := func(i int) domain.BlueprintRun {
		blueprintID := uuid.New().String()
		seedPgBlueprint(t, h, orgID, userID, blueprintID)
		taskID := uuid.New().String()
		eventID := uuid.New().String()
		triggerID := uuid.New().String()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
		`, eventID, orgID, entityID, domain.EventGitHubPRCICheckFailed); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
		if _, err := h.AdminDB.Exec(`
			INSERT INTO tasks (id, org_id, creator_user_id, team_id, entity_id, event_type, dedup_key, primary_event_id, status, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued', now())
		`, taskID, orgID, userID, teamID, entityID, domain.EventGitHubPRCICheckFailed, "dedup-"+taskID[:8], eventID); err != nil {
			t.Fatalf("seed task %d: %v", i, err)
		}
		if _, err := h.AdminDB.Exec(`
			INSERT INTO event_handlers (id, org_id, team_id, creator_user_id, kind, event_type, enabled, source, blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'trigger', $5, true, 'user', $6, 4, 0, now(), now())
		`, triggerID, orgID, teamID, userID, domain.EventGitHubPRCICheckFailed, blueprintID); err != nil {
			t.Fatalf("seed trigger %d: %v", i, err)
		}
		return domain.BlueprintRun{
			BlueprintID:       blueprintID,
			TaskID:            taskID,
			TriggerType:       domain.BlueprintTriggerEvent,
			TriggerID:         triggerID,
			TriggeringEventID: eventID,
			WorktreePath:      "/tmp/wt-race",
			StepPlan:          []domain.BlueprintPlanStep{{StepIndex: 0, PromptID: "p", PromptName: "p", PromptBody: "b"}},
		}
	}

	const racers = 6
	firings := make([]domain.BlueprintRun, racers)
	for i := range firings {
		firings[i] = mkFiring(i)
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
			insertedFlags[i], errs[i] = stores.Blueprints.CreateRunIfNotFiredSystem(ctx, orgID, firings[i])
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	winners := 0
	for i, ins := range insertedFlags {
		if errs[i] != nil {
			t.Fatalf("racer %d: CreateRunIfNotFiredSystem should be a clean skip, not an error: %v", i, errs[i])
		}
		if ins {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly 1 racer to insert under concurrency, got %d (double-mint or all-lost)", winners)
	}

	var count int
	if err := h.AdminDB.QueryRow(`
		SELECT COUNT(*) FROM blueprint_runs
		WHERE org_id = $1 AND entity_id = $2 AND trigger_type = 'event' AND status = 'running'
	`, orgID, entityID).Scan(&count); err != nil {
		t.Fatalf("count active runs: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 active auto run on the entity, got %d", count)
	}

	// Once the (single winning) run terminates, a new firing is allowed
	// again — the index only excludes status='running'.
	if _, err := h.AdminDB.Exec(`
		UPDATE blueprint_runs SET status = 'completed'
		WHERE org_id = $1 AND entity_id = $2 AND status = 'running'
	`, orgID, entityID); err != nil {
		t.Fatalf("complete winning run: %v", err)
	}
	next := mkFiring(racers)
	insertedNext, err := stores.Blueprints.CreateRunIfNotFiredSystem(ctx, orgID, next)
	if err != nil {
		t.Fatalf("CreateRunIfNotFiredSystem after termination: %v", err)
	}
	if !insertedNext {
		t.Fatal("firing after the entity's active run terminated should insert")
	}
}
