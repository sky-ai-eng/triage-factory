package routing

import (
	"context"
	"database/sql"
	"testing"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// claimScenario is one entity + task + trigger with the org bot wired and
// enabled for the local team, plus the router and delegator under test. Every
// assertion below is about one thing: the task's agent claim is written by the
// same durable step that commits the engagement, so there is no reachable
// state where the engagement exists and the claim silently doesn't.
type claimScenario struct {
	db      *sql.DB
	router  *Router
	stub    *stubDelegator
	task    *domain.Task
	trigger domain.EventHandler
	entity  string
	event   string
}

func setupClaimScenario(t *testing.T, suffix string) claimScenario {
	t.Helper()
	database := newTestDB(t)
	stores := sqlitestore.New(database)
	ctx := t.Context()

	if _, err := database.Exec(
		`INSERT INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team: %v", err)
	}

	entity, _, err := stores.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github",
		"owner/repo#"+suffix, "pr", "Claim PR "+suffix, "https://example.com/"+suffix)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	createTestPrompt(t, database, domain.Prompt{ID: "p-" + suffix, Name: "P", Body: "x", Source: "user"})
	eventID, err := stores.Events.Record(ctx, runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		DedupKey:     "build",
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		entity.ID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	trigger := domain.EventHandler{
		ID:                     "trig-" + suffix,
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "p-" + suffix,
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              domain.EventGitHubPRCICheckFailed,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(0),
		Enabled:                true,
	}
	createTriggerForTestRouting(t, database, trigger)
	// createTriggerForTestRouting remaps BlueprintID to the wrapping
	// blueprint; these tests call tryAutoDelegate directly, so resolve the
	// real id onto the local struct for the blueprint_run's FK.
	if err := database.QueryRow(`SELECT blueprint_id FROM event_handlers WHERE id = ?`, trigger.ID).Scan(&trigger.BlueprintID); err != nil {
		t.Fatalf("resolve trigger blueprint id: %v", err)
	}

	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database),
		stores.Agents, stores.TeamAgents, nil, testTaskStore(database), stores.Conversations, stores.Entities,
		stores.PendingFirings, stores.Events, stores.Orgs, stores.Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())

	return claimScenario{db: database, router: router, stub: stub, task: task, trigger: trigger, entity: entity.ID, event: eventID}
}

func (s claimScenario) claim(t *testing.T) (agentID, userID string) {
	t.Helper()
	var agent, user sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_agent_id, claimed_by_user_id FROM tasks WHERE id = ?`, s.task.ID,
	).Scan(&agent, &user); err != nil {
		t.Fatalf("read task claim: %v", err)
	}
	return agent.String, user.String
}

func (s claimScenario) autoRuns(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM blueprint_runs WHERE task_id = ? AND trigger_type = 'event'`, s.task.ID,
	).Scan(&n); err != nil {
		t.Fatalf("count auto runs: %v", err)
	}
	return n
}

// TestFire_ClaimCommitsWithTheRun is the headline of the coupling: a
// successful immediate fire leaves the run and the claim both written, and the
// router's in-memory task reflects it (the cross-team guard reads that field
// for the next matched trigger on this same event).
func TestFire_ClaimCommitsWithTheRun(t *testing.T) {
	s := setupClaimScenario(t, "fire-claims")

	if fired := mustAutoDelegate(t, s.router, s.task, s.trigger, s.entity, s.event, runmode.LocalDefaultTeamID); !fired {
		t.Fatal("expected the trigger to fire")
	}
	if s.autoRuns(t) != 1 {
		t.Fatalf("auto runs = %d, want 1", s.autoRuns(t))
	}
	agent, user := s.claim(t)
	if agent != runmode.LocalDefaultAgentID || user != "" {
		t.Errorf("claim = (agent=%q, user=%q), want the bot's claim written with the run", agent, user)
	}
	if s.task.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("in-memory task claim = %q, want %q — the cross-team guard reads this on the next matched trigger",
			s.task.ClaimedByAgentID, runmode.LocalDefaultAgentID)
	}
}

// TestFire_UserClaimMidFireWinsAndTheRunStillLands pins the no-steal race,
// which coupling the stamp to the run insert must not change: the user's claim
// stands, and the run — already resolved and committed — is NOT rolled back by
// the refusal. A refused stamp is an outcome, not a failure.
func TestFire_UserClaimMidFireWinsAndTheRunStillLands(t *testing.T) {
	s := setupClaimScenario(t, "fire-race")

	// A human takes the task over before the fire commits. tryAutoDelegate's
	// cross-team guard only reads the BOT claim, so the trigger proceeds and
	// the stamp is what has to refuse.
	if _, err := testTaskStore(s.db).SetClaimedByUser(t.Context(), runmode.LocalDefaultOrgID, s.task.ID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("SetClaimedByUser: %v", err)
	}

	if fired := mustAutoDelegate(t, s.router, s.task, s.trigger, s.entity, s.event, runmode.LocalDefaultTeamID); !fired {
		t.Fatal("expected the trigger to fire despite the user claim")
	}
	if got := s.autoRuns(t); got != 1 {
		t.Errorf("auto runs = %d, want 1 — a refused stamp must not roll the run back", got)
	}
	agent, user := s.claim(t)
	if agent != "" || user != runmode.LocalDefaultUserID {
		t.Errorf("claim = (agent=%q, user=%q), want the user's claim to stand", agent, user)
	}
	if s.task.ClaimedByAgentID != "" {
		t.Errorf("in-memory task claim = %q, want empty — the bot lost the race", s.task.ClaimedByAgentID)
	}
}

// TestDefer_ClaimCommitsWithTheQueuedFiring covers the second commitment
// point: when the task is busy, the firing lands on pending_firings and the
// claim rides that insert. Without it the drain would later read an empty
// claim and drop the queued intent as claim_changed.
func TestDefer_ClaimCommitsWithTheQueuedFiring(t *testing.T) {
	s := setupClaimScenario(t, "defer-claims")

	// First fire takes the task's one auto-run slot.
	if fired := mustAutoDelegate(t, s.router, s.task, s.trigger, s.entity, s.event, runmode.LocalDefaultTeamID); !fired {
		t.Fatal("expected the first trigger to fire")
	}
	// Clear the claim the fire wrote so the deferral's own stamp is what the
	// assertion sees — this is the "user requeued while the run stayed live"
	// state, which a NEW event legitimately re-commits.
	if ok, err := sqlitestore.New(s.db).Swipes.RequeueTask(t.Context(), runmode.LocalDefaultOrgID, s.task.ID); err != nil || !ok {
		t.Fatalf("RequeueTask: ok=%v err=%v", ok, err)
	}
	s.task.ClaimedByAgentID = ""

	// A second trigger on the same task, with the run still live: the gate is
	// busy, so this defers onto pending_firings. Its own blueprint keeps the
	// one-trigger-per-blueprint constraint happy.
	createTestPrompt(t, s.db, domain.Prompt{ID: "p-defer-2", Name: "P2", Body: "x", Source: "user"})
	second := domain.EventHandler{
		ID:                     "trig-defer-2",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "p-defer-2",
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              domain.EventGitHubPRCICheckFailed,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(0),
		Enabled:                true,
	}
	createTriggerForTestRouting(t, s.db, second)
	secondEvent, err := sqlitestore.New(s.db).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &s.entity, DedupKey: "build",
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	if fired := mustAutoDelegate(t, s.router, s.task, second, s.entity, secondEvent, runmode.LocalDefaultTeamID); !fired {
		t.Fatal("expected the busy-gate deferral to report a committed firing")
	}
	rows, err := sqlitestore.New(s.db).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, s.entity)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending firings = (%d rows, %v), want exactly 1", len(rows), err)
	}
	if agent, _ := s.claim(t); agent != runmode.LocalDefaultAgentID {
		t.Errorf("claim = %q, want the bot's claim written with the queued firing", agent)
	}
}

// TestDefer_CommittedClaimClearsTheStaleUserClaimInMemory pins the in-memory
// half of the same commitment. The store's guard only lets a stamp land when
// claimed_by_user_id IS NULL, so a landed stamp is proof the router's copy of
// that field — read before the commit — is stale. Carrying it forward would
// leave one task struct holding both claims at once, which the DB's XOR makes
// unrepresentable and which reDeriveTask's "already claimed" guard
// (rederive.go:70) would read as a user's ownership.
func TestDefer_CommittedClaimClearsTheStaleUserClaimInMemory(t *testing.T) {
	s := setupClaimScenario(t, "defer-stale-user")

	// First fire takes the task's one auto-run slot, so the next trigger hits
	// the busy gate and defers.
	if fired := mustAutoDelegate(t, s.router, s.task, s.trigger, s.entity, s.event, runmode.LocalDefaultTeamID); !fired {
		t.Fatal("expected the first trigger to fire")
	}
	if ok, err := sqlitestore.New(s.db).Swipes.RequeueTask(t.Context(), runmode.LocalDefaultOrgID, s.task.ID); err != nil || !ok {
		t.Fatalf("RequeueTask: ok=%v err=%v", ok, err)
	}
	s.task.ClaimedByAgentID = ""
	// The stale read: a user claim the router loaded before the requeue that
	// cleared it. The DB row has no user claim, so the deferral's stamp lands.
	s.task.ClaimedByUserID = runmode.LocalDefaultUserID

	createTestPrompt(t, s.db, domain.Prompt{ID: "p-stale-2", Name: "P2", Body: "x", Source: "user"})
	second := domain.EventHandler{
		ID:                     "trig-stale-2",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "p-stale-2",
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              domain.EventGitHubPRCICheckFailed,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(0),
		Enabled:                true,
	}
	createTriggerForTestRouting(t, s.db, second)
	secondEvent, err := sqlitestore.New(s.db).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &s.entity, DedupKey: "build",
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	if fired := mustAutoDelegate(t, s.router, s.task, second, s.entity, secondEvent, runmode.LocalDefaultTeamID); !fired {
		t.Fatal("expected the busy-gate deferral to report a committed firing")
	}
	if agent, user := s.claim(t); agent != runmode.LocalDefaultAgentID || user != "" {
		t.Fatalf("row claim = (agent=%q, user=%q), want the bot's claim written with the queued firing", agent, user)
	}
	if s.task.ClaimedByAgentID != runmode.LocalDefaultAgentID || s.task.ClaimedByUserID != "" {
		t.Errorf("in-memory claim = (agent=%q, user=%q), want the committed bot claim with the stale user claim dropped",
			s.task.ClaimedByAgentID, s.task.ClaimedByUserID)
	}
}

// TestDrain_DoesNotReStampAClearedClaim is the constraint that rules out the
// obvious fixes, pinned as behavior: a user requeue with a live run is a
// legitimate state, and the drain path must never quietly put the bot's claim
// back. It re-validates the claim instead, and skips the firing when it's gone.
func TestDrain_DoesNotReStampAClearedClaim(t *testing.T) {
	s := setupClaimScenario(t, "drain-noclaim")

	if fired := mustAutoDelegate(t, s.router, s.task, s.trigger, s.entity, s.event, runmode.LocalDefaultTeamID); !fired {
		t.Fatal("expected the first trigger to fire")
	}
	// Queue a firing behind the live run, then have the user requeue the task
	// — clearing both claim columns while the run keeps going, exactly what
	// the AgentCard's "Return to queue" does.
	queuedEvent, err := sqlitestore.New(s.db).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &s.entity, DedupKey: "build",
	})
	if err != nil {
		t.Fatalf("record queued event: %v", err)
	}
	if _, _, err := sqlitestore.New(s.db).PendingFirings.Enqueue(t.Context(), runmode.LocalDefaultOrgID,
		runmode.LocalDefaultUserID, s.entity, s.task.ID, s.trigger.ID, queuedEvent, dbpkg.AgentClaimStamp{}); err != nil {
		t.Fatalf("seed pending firing: %v", err)
	}
	if ok, err := sqlitestore.New(s.db).Swipes.RequeueTask(t.Context(), runmode.LocalDefaultOrgID, s.task.ID); err != nil || !ok {
		t.Fatalf("RequeueTask: ok=%v err=%v", ok, err)
	}
	// Terminate the run so the drain is eligible to pop.
	if _, err := s.db.Exec(`UPDATE blueprint_runs SET status = 'completed' WHERE task_id = ?`, s.task.ID); err != nil {
		t.Fatalf("terminate run: %v", err)
	}

	before := s.autoRuns(t)
	s.router.DrainTask(runmode.LocalDefaultOrgID, s.task.ID)

	if agent, user := s.claim(t); agent != "" || user != "" {
		t.Errorf("claim after drain = (agent=%q, user=%q), want the requeue's cleared claim to hold", agent, user)
	}
	if got := s.autoRuns(t); got != before {
		t.Errorf("auto runs = %d, want %d — the drain fired against a task the user had taken back", got, before)
	}
	rows, _ := sqlitestore.New(s.db).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, s.entity)
	if len(rows) != 1 || rows[0].SkipReason != domain.PendingFiringSkipClaimChanged {
		t.Errorf("firing = %+v, want one row skipped as %q", rows, domain.PendingFiringSkipClaimChanged)
	}
}
