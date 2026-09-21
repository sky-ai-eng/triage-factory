package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
	_ "modernc.org/sqlite"
)

// updateScores writes scores through the SQLite ScoreStore, which is also
// what admits the task's re-evaluation row: every test here scores the way
// production scores, so the queue row comes from the same transaction.
func updateScores(t *testing.T, database *sql.DB, updates []domain.TaskScoreUpdate) error {
	t.Helper()
	return sqlitestore.New(database).Scores.UpdateTaskScores(context.Background(), runmode.LocalDefaultOrgID, updates)
}

// scoreTask scores one task with the given autonomy suitability.
func scoreTask(t *testing.T, database *sql.DB, taskID string, autonomy float64) {
	t.Helper()
	if err := updateScores(t, database, []domain.TaskScoreUpdate{{
		ID: taskID, PriorityScore: 0.5, AutonomySuitability: autonomy, Summary: "test",
	}}); err != nil {
		t.Fatalf("update scores: %v", err)
	}
}

// noopScorer satisfies the Scorer interface without doing anything.
type noopScorer struct{}

func (noopScorer) Trigger(string) {}

// newTestDB sets up an in-memory SQLite with schema + seed for integration tests.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}

// seedLocalBot stages the org's agent and its membership of the local
// default team, so a planned firing resolves an agent and stamps the bot's
// claim the way production's preflight does.
func seedLocalBot(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := sqlitestore.New(database).TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to the default team: %v", err)
	}
}

// reDeriveRouter builds a router over the database wired for the re-derive
// worker: the re-evaluation queue, the agent and team_agents stores so a
// planned firing stamps the bot's claim, and an executor identity so the
// claim has an owner. spawner may be nil — the worker never fires inline.
func reDeriveRouter(t *testing.T, database *sql.DB, spawner Delegator) *Router {
	t.Helper()
	seedLocalBot(t, database)
	st := sqlitestore.New(database)
	r := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), st.Agents, st.TeamAgents, nil, testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings, st.Events, st.Orgs, st.Teams, nil, nil, nil, spawner, noopScorer{}, websocket.NewHub())
	r.SetTaskReDerive(st.TaskReDerive)
	r.SetExecutorID("rederive-worker-test", 1)
	return r
}

// drainReDeriveOnce runs one full drain pass of the re-derive worker
// synchronously.
func drainReDeriveOnce(t *testing.T, r *Router) {
	t.Helper()
	if err := r.drainReDeriveQueue(context.Background()); err != nil {
		t.Fatalf("drainReDeriveQueue: %v", err)
	}
}

// firingsForTask lists the firings admitted against the task's entity.
func firingsForTask(t *testing.T, database *sql.DB, taskID string) []domain.PendingFiring {
	t.Helper()
	task, err := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if err != nil || task == nil {
		t.Fatalf("get task %s: %v", taskID, err)
	}
	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, task.EntityID)
	if err != nil {
		t.Fatalf("list firings: %v", err)
	}
	return rows
}

// reDeriveRow is the task's one task_rederive_queue row as the tests read it.
type reDeriveRow struct {
	id          int64
	status      string
	attempt     int
	revision    int64
	generation  int64
	lastOutcome string
	nextAttempt sql.NullString
}

// reDeriveRowFor reads the task's single queue row and fails if there is
// not exactly one.
func reDeriveRowFor(t *testing.T, database *sql.DB, taskID string) reDeriveRow {
	t.Helper()
	rows, err := database.Query(`
		SELECT id, status, attempt, requested_revision, lease_generation, COALESCE(last_outcome, ''), next_attempt_at
		FROM task_rederive_queue WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		t.Fatalf("read task_rederive_queue: %v", err)
	}
	defer rows.Close()
	var out []reDeriveRow
	for rows.Next() {
		var r reDeriveRow
		if err := rows.Scan(&r.id, &r.status, &r.attempt, &r.revision, &r.generation, &r.lastOutcome, &r.nextAttempt); err != nil {
			t.Fatalf("scan task_rederive_queue: %v", err)
		}
		out = append(out, r)
	}
	if len(out) != 1 {
		t.Fatalf("task %s has %d task_rederive_queue rows, want 1: %+v", taskID, len(out), out)
	}
	return out[0]
}

// requireReDeriveDone asserts the task's re-evaluation row was completed:
// the evaluation reached a verdict, whatever it was.
func requireReDeriveDone(t *testing.T, database *sql.DB, taskID string) {
	t.Helper()
	if row := reDeriveRowFor(t, database, taskID); row.status != workitem.StatusDone {
		t.Errorf("task_rederive_queue row = %+v, want done", row)
	}
}

// requireNoFirings asserts the evaluation admitted nothing for the task.
func requireNoFirings(t *testing.T, database *sql.DB, taskID string) {
	t.Helper()
	if firings := firingsForTask(t, database, taskID); len(firings) != 0 {
		t.Errorf("re-derive admitted %d firing(s), want 0: %+v", len(firings), firings)
	}
}

// setupReDeriveScenario creates an entity, event, task, trigger, and prompt
// to test the re-derive path. Returns the task ID and trigger ID.
func setupReDeriveScenario(t *testing.T, database *sql.DB, minAutonomy float64) (taskID, triggerID string) {
	t.Helper()

	// Create entity
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#1", "pr", "Test PR", "https://github.com/owner/repo/pull/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	entityID := entity.ID

	// Create event with metadata
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:    "aidan",
		CheckName: "build",
		Repo:      "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)
	eventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entityID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}

	// Create task
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entityID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Create prompt
	prompt := domain.Prompt{
		ID:     "test-prompt",
		Name:   "Test",
		Body:   "Do something",
		Source: "user",
	}
	createTestPrompt(t, database, prompt)

	// Create trigger with autonomy threshold
	trigger := domain.EventHandler{
		ID:                     "test-trigger",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "test-prompt",
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              domain.EventGitHubPRCICheckFailed,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(minAutonomy),
		Enabled:                true,
	}
	createTriggerForTestRouting(t, database, trigger)

	return task.ID, trigger.ID
}

// TestReDeriveWorker_AboveThreshold_AdmitsAFiringWithTheClaim is the happy
// path: the score crosses the deferred trigger's threshold, so the worker
// admits one firing for the (task, trigger) and the admission stamps the
// bot's claim, exactly as an event-time busy-task firing does. The firing
// worker fires it later; nothing fires here.
func TestReDeriveWorker_AboveThreshold_AdmitsAFiringWithTheClaim(t *testing.T) {
	database := newTestDB(t)
	taskID, triggerID := setupReDeriveScenario(t, database, 0.6)
	scoreTask(t, database, taskID, 0.8)

	r := reDeriveRouter(t, database, nil)
	drainReDeriveOnce(t, r)

	firings := firingsForTask(t, database, taskID)
	if len(firings) != 1 {
		t.Fatalf("re-derive admitted %d firing(s), want 1: %+v", len(firings), firings)
	}
	if f := firings[0]; f.TriggerID != triggerID || f.Status != workitem.StatusReady || f.UniqueKey != workkinds.PendingFiringKey(taskID, triggerID) {
		t.Errorf("firing = %+v, want a ready row for the trigger under the (task, trigger) key", f)
	}
	task, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if task.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("ClaimedByAgentID = %q, want the bot's claim stamped with the admission", task.ClaimedByAgentID)
	}
	requireReDeriveDone(t, database, taskID)

	// A second pass has nothing to claim and admits nothing more.
	drainReDeriveOnce(t, r)
	if firings := firingsForTask(t, database, taskID); len(firings) != 1 {
		t.Errorf("a second drain changed the firings to %d", len(firings))
	}
}

func TestReDeriveWorker_BelowThreshold_AdmitsNothing(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)
	scoreTask(t, database, taskID, 0.4)

	drainReDeriveOnce(t, reDeriveRouter(t, database, nil))

	requireNoFirings(t, database, taskID)
	requireReDeriveDone(t, database, taskID)
	task, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if task.Status != "queued" || task.ClaimedByAgentID != "" {
		t.Errorf("task = %s claimed by %q, want queued and unclaimed", task.Status, task.ClaimedByAgentID)
	}
}

// TestReDeriveWorker_BotClaimed_AdmitsNothing covers the shape of "already
// delegated": the responsibility axis lives on claimed_by_agent_id. A
// bot-claimed task is not the re-derive's business; without this guard a
// queued-but-bot-claimed task would get a duplicate firing on every
// re-evaluation.
func TestReDeriveWorker_BotClaimed_AdmitsNothing(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)
	scoreTask(t, database, taskID, 0.9)
	seedLocalBot(t, database)
	if ok, err := testTaskStore(database).StampAgentClaimIfUnclaimed(t.Context(), runmode.LocalDefaultOrgID, taskID, runmode.LocalDefaultAgentID, runmode.LocalDefaultTeamID); err != nil || !ok {
		t.Fatalf("stamp agent claim: ok=%v err=%v", ok, err)
	}

	drainReDeriveOnce(t, reDeriveRouter(t, database, nil))

	task, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if task.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("ClaimedByAgentID = %q, want %q (re-derive must not clear the claim)", task.ClaimedByAgentID, runmode.LocalDefaultAgentID)
	}
	if task.Status != "in_progress" {
		t.Errorf("Status = %q, want in_progress", task.Status)
	}
	requireNoFirings(t, database, taskID)
	requireReDeriveDone(t, database, taskID)
}

// TestReDeriveWorker_UserClaimed_AdmitsNothing is the human-side guard: a
// user who claimed the task keeps it, and the re-derive never stamps the
// bot's claim over theirs.
func TestReDeriveWorker_UserClaimed_AdmitsNothing(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)
	scoreTask(t, database, taskID, 0.9)
	if ok, err := testTaskStore(database).ClaimQueuedForUser(t.Context(), runmode.LocalDefaultOrgID, taskID, runmode.LocalDefaultUserID); err != nil || !ok {
		t.Fatalf("stamp user claim: ok=%v err=%v", ok, err)
	}

	drainReDeriveOnce(t, reDeriveRouter(t, database, nil))

	task, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if task.ClaimedByUserID != runmode.LocalDefaultUserID {
		t.Errorf("ClaimedByUserID = %q; want %q (re-derive must not clear a user claim)", task.ClaimedByUserID, runmode.LocalDefaultUserID)
	}
	if task.ClaimedByAgentID != "" {
		t.Errorf("ClaimedByAgentID = %q; want empty (re-derive must not steal a user-claimed task)", task.ClaimedByAgentID)
	}
	requireNoFirings(t, database, taskID)
	requireReDeriveDone(t, database, taskID)
}

// TestReDeriveWorker_Snoozed_AdmitsNothing guards the lifecycle axis: a
// snoozed task is a "do not act" signal, and the wake-on-bump path is the
// way it revives, not a deferred re-evaluation.
func TestReDeriveWorker_Snoozed_AdmitsNothing(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)
	scoreTask(t, database, taskID, 0.9)
	if _, err := database.Exec(
		`UPDATE tasks SET status='snoozed', snooze_until='2099-01-01 00:00:00' WHERE id = ?`,
		taskID,
	); err != nil {
		t.Fatalf("snooze task: %v", err)
	}

	drainReDeriveOnce(t, reDeriveRouter(t, database, nil))

	task, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if task.Status != "snoozed" || task.ClaimedByAgentID != "" {
		t.Errorf("task = %s claimed by %q, want snoozed and unclaimed", task.Status, task.ClaimedByAgentID)
	}
	requireNoFirings(t, database, taskID)
	requireReDeriveDone(t, database, taskID)
}

// TestReDeriveWorker_CrossTeamTrigger_AdmitsOnlyTheVisibleTeams pins the
// cross-team isolation: a task created for team A with team B holding its
// own deferred trigger on the same event type and predicate must not get a
// firing for team B's trigger.
func TestReDeriveWorker_CrossTeamTrigger_AdmitsOnlyTheVisibleTeams(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)

	teamB := "00000000-0000-0000-0000-0000000000b0"
	if _, err := database.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
		teamB, runmode.LocalDefaultOrgID, "team-b-rederive", "Team B Rederive",
	); err != nil {
		t.Fatalf("seed team B: %v", err)
	}
	// The prompt + blueprint must be owned by team B so the same-team
	// trigger→blueprint FK holds.
	insertPromptForTeam(t, database, "p-teamB", teamB)
	bpTeamB := insertBlueprintForTeam(t, database, "bp-teamB", "p-teamB", teamB)
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source,
			 blueprint_id, breaker_threshold, min_autonomy_suitability,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'trigger', ?,
		        NULL, 1, 'user',
		        ?, 4, 0.6,
		        datetime('now'), datetime('now'))
	`, "trigger-teamB", runmode.LocalDefaultOrgID, teamB, runmode.LocalDefaultUserID,
		domain.EventGitHubPRCICheckFailed, bpTeamB); err != nil {
		t.Fatalf("seed team B trigger: %v", err)
	}

	// Score above both teams' thresholds so the only reason to skip team B
	// is the team-mismatch guard.
	scoreTask(t, database, taskID, 0.9)
	drainReDeriveOnce(t, reDeriveRouter(t, database, nil))

	for _, f := range firingsForTask(t, database, taskID) {
		if f.TriggerID == "trigger-teamB" {
			t.Errorf("team B trigger got a firing against team A's task; re-derive must filter by team")
		}
	}
	requireReDeriveDone(t, database, taskID)
}

// TestReDeriveWorker_TeamNotInVisibilitySet_AdmitsNothing pins the leak the
// visibility gate closes: a team with auto-delegation fully enabled and a
// deferred trigger matching the stored event must not get a firing against
// — nor consolidate ownership of — a task whose visibility set doesn't
// include it. Every other gate is open here, so only the task_teams
// membership check can stop it.
func TestReDeriveWorker_TeamNotInVisibilitySet_AdmitsNothing(t *testing.T) {
	database := newTestDB(t)
	stores := sqlitestore.New(database)

	// Entity + event + task owned by the local-default team, with NO
	// task_teams rows, so its visibility set is just the owner.
	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#novis", "pr", "No-vis PR", "https://example.com/novis")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	meta := events.GitHubPRCICheckFailedMetadata{Author: "aidan", CheckName: "build", Repo: "owner/repo"}
	metaJSON, _ := json.Marshal(meta)
	eventID, err := stores.Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID,
		DedupKey: "build", MetadataJSON: string(metaJSON),
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Team B: fully opted into auto-delegation, with a deferred trigger
	// that matches the same event — but not in the task's visibility set.
	teamB := "00000000-0000-0000-0000-0000000000bb"
	if _, err := database.Exec(`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'team-b-novis', 'Team B No-Vis')`, teamB, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed team B: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO team_settings (team_id, auto_delegate_enabled) VALUES (?, 1)`, teamB); err != nil {
		t.Fatalf("seed team B settings: %v", err)
	}
	seedLocalBot(t, database)
	if err := stores.TeamAgents.AddForTeam(t.Context(), runmode.LocalDefaultOrgID, teamB, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team B: %v", err)
	}
	insertPromptForTeam(t, database, "p-novis", teamB)
	bpNovis := insertBlueprintForTeam(t, database, "bp-novis", "p-novis", teamB)
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source,
			 blueprint_id, breaker_threshold, min_autonomy_suitability,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'trigger', ?,
		        NULL, 1, 'user',
		        ?, 4, 0.6,
		        datetime('now'), datetime('now'))
	`, "trigger-novis", runmode.LocalDefaultOrgID, teamB, runmode.LocalDefaultUserID,
		domain.EventGitHubPRCICheckFailed, bpNovis); err != nil {
		t.Fatalf("seed team B trigger: %v", err)
	}

	scoreTask(t, database, task.ID, 0.9)
	stub := &stubDelegator{db: database}
	drainReDeriveOnce(t, reDeriveRouter(t, database, stub))

	if stub.calls != 0 {
		t.Errorf("the re-derive delegated (%d calls); it admits firings and never fires inline", stub.calls)
	}
	requireNoFirings(t, database, task.ID)
	got, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, task.ID)
	if got.ClaimedByAgentID != "" {
		t.Errorf("task claimed by agent (%q); a team outside the visibility set must not consolidate ownership", got.ClaimedByAgentID)
	}
	if teamIDValue(got) != runmode.LocalDefaultTeamID {
		t.Errorf("owner team_id = %q, want unchanged %q (no cross-team consolidation)", teamIDValue(got), runmode.LocalDefaultTeamID)
	}
	requireReDeriveDone(t, database, task.ID)
}

func TestReDeriveWorker_ZeroThresholdTrigger_IsNotReDerived(t *testing.T) {
	database := newTestDB(t)

	entity2, _, _ := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#2", "pr", "Test PR 2", "https://github.com/owner/repo/pull/2")
	entityID := entity2.ID
	meta := events.GitHubPRCICheckFailedMetadata{
		Author: "aidan", CheckName: "lint", Repo: "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)
	eventID, _ := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entityID,
		DedupKey: "lint", MetadataJSON: string(metaJSON),
	})
	task, _, _ := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entityID, domain.EventGitHubPRCICheckFailed, "lint", eventID, 0.5)

	createTestPrompt(t, database, domain.Prompt{ID: "p2", Name: "Test2", Body: "Do", Source: "user"})

	// Trigger with min_autonomy_suitability=0 (immediate fire, not deferred)
	createTriggerForTestRouting(t, database, domain.EventHandler{
		ID: "t-zero", Kind: domain.EventHandlerKindTrigger,
		BlueprintID: "p2", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: intPtr(4),
		MinAutonomySuitability: floatPtr(0.0), Enabled: true,
	})

	scoreTask(t, database, task.ID, 0.9)
	drainReDeriveOnce(t, reDeriveRouter(t, database, nil))

	// A zero-threshold trigger is HandleEvent's to fire; the re-derive
	// leaves it alone.
	requireNoFirings(t, database, task.ID)
	requireReDeriveDone(t, database, task.ID)
	got, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, task.ID)
	if got.Status != "queued" {
		t.Errorf("expected queued (zero-threshold trigger skipped), got %s", got.Status)
	}
}

func TestReDeriveWorker_PredicateMismatch_AdmitsNothing(t *testing.T) {
	database := newTestDB(t)

	// The event's author isn't in the predicate's author_in allowlist.
	entity3, _, _ := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#3", "pr", "Test PR 3", "https://github.com/owner/repo/pull/3")
	entityID := entity3.ID
	meta := events.GitHubPRCICheckFailedMetadata{
		Author: "someone-else", CheckName: "build", Repo: "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)
	eventID, _ := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entityID,
		DedupKey: "build", MetadataJSON: string(metaJSON),
	})
	task, _, _ := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entityID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)

	createTestPrompt(t, database, domain.Prompt{ID: "p3", Name: "Test3", Body: "Do", Source: "user"})

	pred := `{"author_in":["aidan"]}`
	createTriggerForTestRouting(t, database, domain.EventHandler{
		ID: "t-pred", Kind: domain.EventHandlerKindTrigger,
		BlueprintID: "p3", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: intPtr(4),
		MinAutonomySuitability: floatPtr(0.5), Enabled: true,
		ScopePredicateJSON: &pred,
	})

	scoreTask(t, database, task.ID, 0.9)
	drainReDeriveOnce(t, reDeriveRouter(t, database, nil))

	requireNoFirings(t, database, task.ID)
	requireReDeriveDone(t, database, task.ID)
	got, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, task.ID)
	if got.Status != "queued" {
		t.Errorf("expected queued (predicate mismatch), got %s", got.Status)
	}
}
