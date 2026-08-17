package postgres_test

import (
	"context"
	"database/sql"
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
	// userID + agentID back the claim-coupling test below: the fenced insert
	// carries the task's agent claim, so its assertions need a real agents row
	// to stamp and a real user to lose the claim race to.
	userID  string
	agentID string
	newTask func(t *testing.T) string
	firing  func(t *testing.T, taskID string) domain.BlueprintRun
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
	agentID := uuid.New().String()
	if _, err := h.AdminDB.Exec(
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Race Bot')`, agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return raceFixture{orgID: orgID, entityID: entityID, userID: userID, agentID: agentID, newTask: newTask, firing: firing}
}

// TestBlueprintStore_Postgres_OneActiveAutoRunPerTask: the task gate
// (HasActiveAutoConversationForTaskSystem) is check-then-act, so two different
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
			insertedFlags[i], _, errs[i] = stores.Blueprints.CreateRunIfNotFiredSystem(ctx, fx.orgID, firings[i], db.AgentClaimStamp{})
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
	insertedNext, _, err := stores.Blueprints.CreateRunIfNotFiredSystem(ctx, fx.orgID, fx.firing(t, taskID), db.AgentClaimStamp{})
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
		inserted, _, err := stores.Blueprints.CreateRunIfNotFiredSystem(ctx, fx.orgID, fx.firing(t, tc.taskID), db.AgentClaimStamp{})
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

// TestBlueprintStore_Postgres_FencedInsertCarriesTaskClaim is the Postgres
// twin of the SQLite test of the same shape: the fenced insert is a
// delegation's commitment point, so the task's agent claim commits in the same
// transaction as the run row. Without that, a failed stamp leaves the board
// showing a free task under a live run and no replay can repair it — the
// (triggering_event_id, trigger_id) fence closes first.
func TestBlueprintStore_Postgres_FencedInsertCarriesTaskClaim(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	fx := newRaceFixture(t, h)

	readClaim := func(t *testing.T, taskID string) (string, string) {
		t.Helper()
		var agent, user sql.NullString
		if err := h.AdminDB.QueryRow(
			`SELECT claimed_by_agent_id, claimed_by_user_id FROM tasks WHERE id = $1`, taskID,
		).Scan(&agent, &user); err != nil {
			t.Fatalf("read task claim: %v", err)
		}
		return agent.String, user.String
	}

	t.Run("stamps_with_the_run", func(t *testing.T) {
		taskID := fx.newTask(t)
		inserted, claimed, err := stores.Blueprints.CreateRunIfNotFiredSystem(ctx, fx.orgID, fx.firing(t, taskID), db.AgentClaimStamp{AgentID: fx.agentID})
		if err != nil {
			t.Fatalf("CreateRunIfNotFiredSystem: %v", err)
		}
		if !inserted || !claimed {
			t.Fatalf("(inserted=%v, claimed=%v), want (true, true)", inserted, claimed)
		}
		if agent, _ := readClaim(t, taskID); agent != fx.agentID {
			t.Errorf("claimed_by_agent_id = %q, want %q — the run committed without its claim", agent, fx.agentID)
		}
	})

	t.Run("refused_stamp_does_not_roll_back_the_run", func(t *testing.T) {
		taskID := fx.newTask(t)
		// The user claims the task in the window before the insert: they win
		// the claim, and the run must still commit.
		if err := stores.Tasks.SetClaimedByUser(ctx, fx.orgID, taskID, fx.userID); err != nil {
			t.Fatalf("SetClaimedByUser: %v", err)
		}
		br := fx.firing(t, taskID)
		inserted, claimed, err := stores.Blueprints.CreateRunIfNotFiredSystem(ctx, fx.orgID, br, db.AgentClaimStamp{AgentID: fx.agentID})
		if err != nil {
			t.Fatalf("CreateRunIfNotFiredSystem against a user-claimed task: %v", err)
		}
		if !inserted {
			t.Error("a refused stamp rolled the run insert back")
		}
		if claimed {
			t.Error("claimed=true on a user-claimed task — the stamp stole the claim")
		}
		if agent, user := readClaim(t, taskID); agent != "" || user == "" {
			t.Errorf("claim = (agent=%q, user=%q), want the user's claim untouched", agent, user)
		}
		var runs int
		if err := h.AdminDB.QueryRow(`SELECT count(*) FROM blueprint_runs WHERE task_id = $1`, taskID).Scan(&runs); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		if runs != 1 {
			t.Errorf("blueprint_runs for the task = %d, want 1 (the refused stamp must not undo the run)", runs)
		}
	})

	t.Run("fenced_replay_re_stamps_nothing", func(t *testing.T) {
		taskID := fx.newTask(t)
		br := fx.firing(t, taskID)
		if inserted, _, err := stores.Blueprints.CreateRunIfNotFiredSystem(ctx, fx.orgID, br, db.AgentClaimStamp{AgentID: fx.agentID}); err != nil || !inserted {
			t.Fatalf("first fire: inserted=%v err=%v", inserted, err)
		}
		// The user requeues while the run stays live — the deliberate
		// "unclaimed with a live run" state. A replayed event must not put the
		// bot's claim back.
		if ok, err := stores.Swipes.RequeueTask(ctx, fx.orgID, taskID); err != nil || !ok {
			t.Fatalf("RequeueTask: ok=%v err=%v", ok, err)
		}
		replay := br
		replay.ID = ""
		inserted, claimed, err := stores.Blueprints.CreateRunIfNotFiredSystem(ctx, fx.orgID, replay, db.AgentClaimStamp{AgentID: fx.agentID})
		if err != nil {
			t.Fatalf("replay fire: %v", err)
		}
		if inserted || claimed {
			t.Errorf("replay = (inserted=%v, claimed=%v), want (false, false)", inserted, claimed)
		}
		if agent, user := readClaim(t, taskID); agent != "" || user != "" {
			t.Errorf("claim after replay = (agent=%q, user=%q), want the requeue's cleared claim to hold", agent, user)
		}
	})
}
