package delegate

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
// fixtures. Tests pass this to NewSpawner so prompt-loading code
// paths exercise the same store interface production uses. Returns
// nil when database is nil (used by smoke tests that don't touch
// any DB).
func testPromptStore(database *sql.DB) db.PromptStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).Prompts
}

// testTaskStore returns a SQLite-backed TaskStore for spawner-test
// fixtures. Spawner consumes it for run lifecycle handlers; tests
// pass it through NewSpawner so production and test share one path.
// Returns nil for the nil-database smoke tests.
func testTaskStore(database *sql.DB) db.TaskStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).Tasks
}

// testSpawnerStores builds the full db.Stores bundle from the given
// SQLite handle and overrides Prompts + Tasks with the test-specific
// fixtures (which wrap the real stores so tests can intercept or
// observe specific calls). Returns the empty bundle for nil database
// so the nil-only smoke tests still compile.
func testSpawnerStores(database *sql.DB) db.Stores {
	if database == nil {
		return db.Stores{}
	}
	stores := sqlitestore.New(database)
	stores.Prompts = testPromptStore(database)
	stores.Tasks = testTaskStore(database)
	return stores
}

// ensureTestPrompt is the FindOrCreate replacement for the deleted
// db.GetPrompt + db.CreatePrompt pair tests used to seed a prompt
// row for run fixtures. Idempotent: subsequent calls on the same
// ID are no-ops.
func ensureTestPrompt(t *testing.T, database *sql.DB, p domain.Prompt) {
	t.Helper()
	store := testPromptStore(database)
	ctx := context.Background()
	existing, _ := store.Get(ctx, runmode.LocalDefaultOrg, p.ID)
	if existing != nil {
		return
	}
	if err := store.Create(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, p); err != nil {
		t.Fatalf("ensureTestPrompt %s: %v", p.ID, err)
	}
}
