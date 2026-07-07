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
// tests. Rules + triggers are unified into event_handlers;
// trigger-shaped fixtures use kind='trigger' rows on this store.
func testEventHandlerStore(database *sql.DB) db.EventHandlerStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).EventHandlers
}

// blueprintWrappingPromptForSkills creates a single-step user blueprint on the
// local default team wrapping promptID and returns the blueprint id. A trigger
// now FKs a same-team blueprint (not a prompt), so trigger fixtures wrap their
// prompt here.
func blueprintWrappingPromptForSkills(t *testing.T, database *sql.DB, promptID string) string {
	t.Helper()
	blueprintID := "bp-" + promptID
	store := sqlitestore.New(database).Blueprints
	ctx := context.Background()
	if existing, _ := store.Get(ctx, runmode.LocalDefaultOrgID, blueprintID); existing == nil {
		if err := store.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
			ID: blueprintID, Name: blueprintID, Source: "user", TeamID: runmode.LocalDefaultTeamID,
		}); err != nil {
			t.Fatalf("blueprintWrappingPromptForSkills create %s: %v", blueprintID, err)
		}
		if err := store.ReplaceSteps(ctx, runmode.LocalDefaultOrgID, blueprintID, []string{promptID}, nil); err != nil {
			t.Fatalf("blueprintWrappingPromptForSkills steps %s: %v", blueprintID, err)
		}
	}
	return blueprintID
}

// createTriggerForTestSkills + getTriggerForTestSkills exercise the
// trigger-shaped CRUD on EventHandlerStore. Used by importer_test.go. The
// trigger's BlueprintID field is supplied holding a PROMPT id; this helper
// wraps that prompt in a single-step team-A blueprint and rebinds BlueprintID
// to the blueprint id so the same-team blueprint FK holds.
func createTriggerForTestSkills(t *testing.T, database *sql.DB, trig domain.EventHandler) {
	t.Helper()
	trig.Kind = domain.EventHandlerKindTrigger
	if trig.TriggerType == "" {
		trig.TriggerType = domain.TriggerTypeEvent
	}
	if trig.BlueprintID != "" {
		trig.BlueprintID = blueprintWrappingPromptForSkills(t, database, trig.BlueprintID)
	}
	if err := testEventHandlerStore(database).Create(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, trig); err != nil {
		t.Fatalf("createTriggerForTestSkills %s: %v", trig.ID, err)
	}
}

func getTriggerForTestSkills(t *testing.T, database *sql.DB, id string) *domain.EventHandler {
	t.Helper()
	got, err := testEventHandlerStore(database).Get(context.Background(), runmode.LocalDefaultOrgID, id)
	if err != nil {
		t.Fatalf("getTriggerForTestSkills %s: %v", id, err)
	}
	return got
}

func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }
