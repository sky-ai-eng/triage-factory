package skills

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
// fixtures so ImportAll exercises the same store interface
// production uses.
func testPromptStore(database *sql.DB) db.PromptStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).Prompts
}

// testEventHandlerStore mirrors testPromptStore for trigger-touching
// tests. Post-SKY-259 rules + triggers are unified into event_handlers;
// trigger-shaped fixtures use kind='trigger' rows on this store.
func testEventHandlerStore(database *sql.DB) db.EventHandlerStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).EventHandlers
}

// createTriggerForTestSkills + getTriggerForTestSkills exercise the
// trigger-shaped CRUD on EventHandlerStore. Used by importer_test.go.
func createTriggerForTestSkills(t *testing.T, database *sql.DB, trig domain.EventHandler) {
	t.Helper()
	trig.Kind = domain.EventHandlerKindTrigger
	if trig.TriggerType == "" {
		trig.TriggerType = domain.TriggerTypeEvent
	}
	if err := testEventHandlerStore(database).Create(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, trig); err != nil {
		t.Fatalf("createTriggerForTestSkills %s: %v", trig.ID, err)
	}
}

func getTriggerForTestSkills(t *testing.T, database *sql.DB, id string) *domain.EventHandler {
	t.Helper()
	got, err := testEventHandlerStore(database).Get(context.Background(), runmode.LocalDefaultOrg, id)
	if err != nil {
		t.Fatalf("getTriggerForTestSkills %s: %v", id, err)
	}
	return got
}

func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }
