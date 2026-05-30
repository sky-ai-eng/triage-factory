package routing

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// testPromptStore returns a real SQLite-backed PromptStore for test
// fixtures. Tests pass this to NewRouter so prompt-loading code
// paths exercise the same store interface production uses.
func testPromptStore(database *sql.DB) db.PromptStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).Prompts
}

// testEventHandlerStore returns a SQLite-backed EventHandlerStore for
// routing tests. Post-SKY-259 the router takes one store covering both
// rules and triggers; tests construct it the same way as production.
func testEventHandlerStore(database *sql.DB) db.EventHandlerStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).EventHandlers
}

// testTaskStore returns a SQLite-backed TaskStore for routing tests.
// SKY-283 — Router takes one store for task lifecycle/claim/queue
// queries; the per-method orgID arg defaults to LocalDefaultOrg.
func testTaskStore(database *sql.DB) db.TaskStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).Tasks
}

// createTriggerForTestRouting + setTriggerEnabledForTestRouting are
// trigger-shape helpers used by drain_test + rederive_test. Post-
// SKY-259 they wrap EventHandlerStore.Create / SetEnabled, building a
// kind='trigger' EventHandler from the legacy-shaped fields.
func createTriggerForTestRouting(t *testing.T, database *sql.DB, trig domain.EventHandler) {
	t.Helper()
	trig.Kind = domain.EventHandlerKindTrigger
	if trig.TriggerType == "" {
		trig.TriggerType = domain.TriggerTypeEvent
	}
	if err := testEventHandlerStore(database).Create(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, trig); err != nil {
		t.Fatalf("createTriggerForTestRouting %s: %v", trig.ID, err)
	}
}

func setTriggerEnabledForTestRouting(t *testing.T, database *sql.DB, id string, enabled bool) {
	t.Helper()
	if err := testEventHandlerStore(database).SetEnabled(context.Background(), runmode.LocalDefaultOrg, id, enabled); err != nil {
		t.Fatalf("setTriggerEnabledForTestRouting %s: %v", id, err)
	}
}

// createTestPrompt is the replacement for the deleted db.CreatePrompt
// free-function. Routes through the store so test fixtures and
// production share the same insert SQL.
func createTestPrompt(t *testing.T, database *sql.DB, p domain.Prompt) {
	t.Helper()
	store := testPromptStore(database)
	if err := store.Create(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, p); err != nil {
		t.Fatalf("createTestPrompt %s: %v", p.ID, err)
	}
}

// intPtr / floatPtr are convenience wrappers for the pointer-shaped
// per-kind fields on domain.EventHandler. Trigger rows carry
// BreakerThreshold (*int) and MinAutonomySuitability (*float64); test
// fixtures with literal values pass them through these helpers.
func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }

// seedHandlerFKTargets seeds the prompts that shipped triggers reference
// so EventHandlerStore.Seed's trigger rows resolve their FK to prompts.
// Production calls seedDefaultPrompts which seeds prompts THEN handlers in
// the same loop; tests that call Seed directly need to replicate that
// ordering manually.
func seedHandlerFKTargets(t *testing.T, database *sql.DB) {
	t.Helper()
	store := testPromptStore(database)
	for _, p := range []domain.Prompt{
		{ID: "system-pr-review", Name: "PR Review", Body: "x", Source: "system"},
		{ID: "system-conflict-resolution", Name: "Conflicts", Body: "x", Source: "system"},
		{ID: "system-ci-fix", Name: "CI Fix", Body: "x", Source: "system"},
		{ID: "system-jira-implement", Name: "Jira Implement", Body: "x", Source: "system"},
		{ID: "system-fix-review-feedback", Name: "Fix Review", Body: "x", Source: "system"},
	} {
		if err := store.SeedOrUpdate(context.Background(), runmode.LocalDefaultOrg, p); err != nil {
			t.Fatalf("seed prompt %s: %v", p.ID, err)
		}
	}
}
