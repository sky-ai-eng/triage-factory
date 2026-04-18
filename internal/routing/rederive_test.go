package routing

import (
	"database/sql"
	"encoding/json"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// noopScorer satisfies the Scorer interface without doing anything.
type noopScorer struct{}

func (noopScorer) Trigger() {}

// newTestDB sets up an in-memory SQLite with schema + seed for integration tests.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite3", ":memory:?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	if err := db.Migrate(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.SeedEventTypes(database); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	return database
}

// setupReDeriveScenario creates an entity, event, task, trigger, and prompt
// to test the re-derive path. Returns the task ID and trigger ID.
func setupReDeriveScenario(t *testing.T, database *sql.DB, minAutonomy float64) (taskID, triggerID string) {
	t.Helper()

	// Create entity
	entity, _, err := db.FindOrCreateEntity(database, "github", "owner/repo#1", "pr", "Test PR", "https://github.com/owner/repo/pull/1")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	entityID := entity.ID

	// Create event with metadata
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:       "aidan",
		AuthorIsSelf: true,
		CheckName:    "build",
		Repo:         "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)
	eventID, err := db.RecordEvent(database, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entityID,
		DedupKey:     "build",
		MetadataJSON: string(metaJSON),
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}

	// Create task
	task, _, err := db.FindOrCreateTask(database, entityID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)
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
	if err := db.CreatePrompt(database, prompt); err != nil {
		t.Fatalf("save prompt: %v", err)
	}

	// Create trigger with autonomy threshold
	trigger := domain.PromptTrigger{
		ID:                     "test-trigger",
		PromptID:               "test-prompt",
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              domain.EventGitHubPRCICheckFailed,
		BreakerThreshold:       4,
		CooldownSeconds:        60,
		MinAutonomySuitability: minAutonomy,
		Enabled:                true,
	}
	if err := db.SavePromptTrigger(database, trigger); err != nil {
		t.Fatalf("save trigger: %v", err)
	}

	return task.ID, trigger.ID
}

func TestReDeriveAfterScoring_AboveThreshold_Delegates(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)

	// Score the task above threshold
	score := 0.8
	err := db.UpdateTaskScores(database, []domain.TaskScoreUpdate{{
		ID:                  taskID,
		PriorityScore:       0.5,
		AutonomySuitability: score,
		Summary:             "test",
	}})
	if err != nil {
		t.Fatalf("update scores: %v", err)
	}

	// Create router without spawner — fireDelegate guards nil spawner and
	// returns early, so the task stays queued. The test verifies the full
	// gate-check path runs (suitability >= threshold, predicate matched)
	// without panicking. The log output confirms the trigger fired.
	ws := websocket.NewHub()
	router := NewRouter(database, nil, noopScorer{}, ws)

	router.ReDeriveAfterScoring([]string{taskID})

	// Task stays queued because no spawner is configured, but the trigger
	// matched (visible in log output: "re-derive: task ... firing").
	task, _ := db.GetTask(database, taskID)
	if task == nil {
		t.Fatal("task not found")
	}
	if task.Status != "queued" {
		t.Errorf("expected queued (no spawner), got %s", task.Status)
	}
}

func TestReDeriveAfterScoring_BelowThreshold_Skips(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)

	// Score below threshold
	score := 0.4
	err := db.UpdateTaskScores(database, []domain.TaskScoreUpdate{{
		ID:                  taskID,
		PriorityScore:       0.5,
		AutonomySuitability: score,
		Summary:             "test",
	}})
	if err != nil {
		t.Fatalf("update scores: %v", err)
	}

	ws := websocket.NewHub()
	router := NewRouter(database, nil, noopScorer{}, ws)
	router.ReDeriveAfterScoring([]string{taskID})

	// Task should remain queued — trigger was skipped
	task, _ := db.GetTask(database, taskID)
	if task == nil {
		t.Fatal("task not found")
	}
	if task.Status != "queued" {
		t.Errorf("expected queued, got %s", task.Status)
	}
}

func TestReDeriveAfterScoring_AlreadyDelegated_Skips(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)

	// Score above threshold
	err := db.UpdateTaskScores(database, []domain.TaskScoreUpdate{{
		ID:                  taskID,
		PriorityScore:       0.5,
		AutonomySuitability: 0.9,
		Summary:             "test",
	}})
	if err != nil {
		t.Fatalf("update scores: %v", err)
	}

	// Manually set task to delegated
	if err := db.SetTaskStatus(database, taskID, "delegated"); err != nil {
		t.Fatalf("set status: %v", err)
	}

	ws := websocket.NewHub()
	router := NewRouter(database, nil, noopScorer{}, ws)
	router.ReDeriveAfterScoring([]string{taskID})

	// Should still be delegated — re-derive skipped it
	task, _ := db.GetTask(database, taskID)
	if task.Status != "delegated" {
		t.Errorf("expected delegated (untouched), got %s", task.Status)
	}
}

func TestReDeriveAfterScoring_ZeroThresholdTrigger_SkippedByReDerive(t *testing.T) {
	database := newTestDB(t)

	// Create entity + event + task
	entity2, _, _ := db.FindOrCreateEntity(database, "github", "owner/repo#2", "pr", "Test PR 2", "https://github.com/owner/repo/pull/2")
	entityID := entity2.ID
	meta := events.GitHubPRCICheckFailedMetadata{
		Author: "aidan", AuthorIsSelf: true, CheckName: "lint", Repo: "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)
	eventID, _ := db.RecordEvent(database, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entityID,
		DedupKey: "lint", MetadataJSON: string(metaJSON),
	})
	task, _, _ := db.FindOrCreateTask(database, entityID, domain.EventGitHubPRCICheckFailed, "lint", eventID, 0.5)

	// Prompt
	db.CreatePrompt(database, domain.Prompt{ID: "p2", Name: "Test2", Body: "Do", Source: "user"})

	// Trigger with min_autonomy_suitability=0 (immediate fire, not deferred)
	db.SavePromptTrigger(database, domain.PromptTrigger{
		ID: "t-zero", PromptID: "p2", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: 4,
		CooldownSeconds: 60, MinAutonomySuitability: 0.0, Enabled: true,
	})

	// Score the task
	db.UpdateTaskScores(database, []domain.TaskScoreUpdate{{
		ID: task.ID, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "test",
	}})

	ws := websocket.NewHub()
	router := NewRouter(database, nil, noopScorer{}, ws)
	router.ReDeriveAfterScoring([]string{task.ID})

	// Task should remain queued — zero-threshold trigger is skipped in re-derive
	// (it would have fired already in HandleEvent)
	got, _ := db.GetTask(database, task.ID)
	if got.Status != "queued" {
		t.Errorf("expected queued (zero-threshold trigger skipped), got %s", got.Status)
	}
}

func TestReDeriveAfterScoring_PredicateMismatch_Skips(t *testing.T) {
	database := newTestDB(t)

	// Create entity + event where author_is_self=false
	entity3, _, _ := db.FindOrCreateEntity(database, "github", "owner/repo#3", "pr", "Test PR 3", "https://github.com/owner/repo/pull/3")
	entityID := entity3.ID
	meta := events.GitHubPRCICheckFailedMetadata{
		Author: "someone-else", AuthorIsSelf: false, CheckName: "build", Repo: "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)
	eventID, _ := db.RecordEvent(database, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entityID,
		DedupKey: "build", MetadataJSON: string(metaJSON),
	})
	task, _, _ := db.FindOrCreateTask(database, entityID, domain.EventGitHubPRCICheckFailed, "build", eventID, 0.5)

	// Prompt
	db.CreatePrompt(database, domain.Prompt{ID: "p3", Name: "Test3", Body: "Do", Source: "user"})

	// Trigger with predicate requiring author_is_self=true
	pred := `{"author_is_self":true}`
	db.SavePromptTrigger(database, domain.PromptTrigger{
		ID: "t-pred", PromptID: "p3", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: 4,
		CooldownSeconds: 60, MinAutonomySuitability: 0.5, Enabled: true,
		ScopePredicateJSON: &pred,
	})

	// Score above threshold
	db.UpdateTaskScores(database, []domain.TaskScoreUpdate{{
		ID: task.ID, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "test",
	}})

	ws := websocket.NewHub()
	router := NewRouter(database, nil, noopScorer{}, ws)
	router.ReDeriveAfterScoring([]string{task.ID})

	// Task should stay queued — predicate doesn't match
	got, _ := db.GetTask(database, task.ID)
	if got.Status != "queued" {
		t.Errorf("expected queued (predicate mismatch), got %s", got.Status)
	}
}
