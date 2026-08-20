package routing

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
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
// routing tests. The router takes one store covering both
// rules and triggers; tests construct it the same way as production.
func testEventHandlerStore(database *sql.DB) db.EventHandlerStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).EventHandlers
}

// testTaskStore returns a SQLite-backed TaskStore for routing tests.
// The Router takes one store for task lifecycle/claim/queue
// queries; the per-method orgID arg defaults to LocalDefaultOrgID.
func testTaskStore(database *sql.DB) db.TaskStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).Tasks
}

// testBlueprintStore returns a SQLite-backed BlueprintStore for routing
// tests. NewRouter takes the blueprint store immediately after the prompt
// store (triggers fire blueprints, not prompts).
func testBlueprintStore(database *sql.DB) db.BlueprintStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).Blueprints
}

// blueprintWrappingPrompt creates a single-step user blueprint owned by
// teamID wrapping promptID and returns the blueprint id. A trigger now FKs a
// same-team blueprint (not a prompt), so trigger fixtures wrap their prompt
// here. blueprintID is derived from promptID so repeat calls for the same
// (prompt, team) are stable; the caller points the trigger's BlueprintID at
// the returned id.
func blueprintWrappingPrompt(t *testing.T, database *sql.DB, promptID, teamID string) string {
	t.Helper()
	blueprintID := "bp-" + promptID
	store := sqlitestore.New(database).Blueprints
	ctx := context.Background()
	if existing, _ := store.Get(ctx, runmode.LocalDefaultOrgID, blueprintID); existing == nil {
		if _, err := store.Create(ctx, runmode.LocalDefaultOrgID, teamID, domain.Blueprint{
			ID: blueprintID, Name: blueprintID, Source: "user", TeamID: teamID,
		}); err != nil {
			t.Fatalf("blueprintWrappingPrompt create %s (team %s): %v", blueprintID, teamID, err)
		}
		if _, err := store.ReplaceSteps(ctx, runmode.LocalDefaultOrgID, blueprintID, []string{promptID}, nil); err != nil {
			t.Fatalf("blueprintWrappingPrompt steps %s: %v", blueprintID, err)
		}
	}
	return blueprintID
}

// createTriggerForTestRouting + setTriggerEnabledForTestRouting are
// trigger-shape helpers used by drain_test + rederive_test. They
// wrap EventHandlerStore.Create / Update, building a
// kind='trigger' EventHandler from the legacy-shaped fields.
// createTriggerForTestRouting builds a kind='trigger' EventHandler on the
// local default team (team A). The trigger's BlueprintID field is supplied
// holding a PROMPT id (the legacy shape); this helper wraps that prompt in a
// single-step team-A blueprint and rebinds BlueprintID to the blueprint id, so
// the trigger satisfies the same-team blueprint FK.
func createTriggerForTestRouting(t *testing.T, database *sql.DB, trig domain.EventHandler) {
	t.Helper()
	trig.Kind = domain.EventHandlerKindTrigger
	if trig.TriggerType == "" {
		trig.TriggerType = domain.TriggerTypeEvent
	}
	if trig.BlueprintID != "" {
		trig.BlueprintID = blueprintWrappingPrompt(t, database, trig.BlueprintID, runmode.LocalDefaultTeamID)
	}
	if _, err := testEventHandlerStore(database).Create(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, trig); err != nil {
		t.Fatalf("createTriggerForTestRouting %s: %v", trig.ID, err)
	}
}

// setTriggerEnabledForTestRouting flips a trigger's enabled bit through the
// same read-then-Update the PATCH route performs, so the fixture and the
// product reach the stored row by one path.
func setTriggerEnabledForTestRouting(t *testing.T, database *sql.DB, id string, enabled bool) {
	t.Helper()
	ctx := context.Background()
	store := testEventHandlerStore(database)
	trig, err := store.Get(ctx, runmode.LocalDefaultOrgID, id)
	if err != nil || trig == nil {
		t.Fatalf("setTriggerEnabledForTestRouting %s: get: %v", id, err)
	}
	trig.Enabled = enabled
	if _, err := store.Update(ctx, runmode.LocalDefaultOrgID, *trig); err != nil {
		t.Fatalf("setTriggerEnabledForTestRouting %s: %v", id, err)
	}
}

// createTestPrompt is the replacement for the deleted db.CreatePrompt
// free-function. Routes through the store so test fixtures and
// production share the same insert SQL. The SQLite store pins
// LocalDefaultTeamID (team A), so this owns the row to team A — for a
// prompt a second team's trigger/chain must reference, use
// insertPromptForTeam.
func createTestPrompt(t *testing.T, database *sql.DB, p domain.Prompt) {
	t.Helper()
	store := testPromptStore(database)
	if _, err := store.Create(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, p); err != nil {
		t.Fatalf("createTestPrompt %s: %v", p.ID, err)
	}
}

// insertPromptForTeam seeds a user prompt owned by a specific team via a
// raw INSERT. createTestPrompt routes through PromptStore.Create which pins
// LocalDefaultTeamID (team A); a team-B trigger or chain step can only bind
// a prompt its own team owns under the same-team composite FK, so
// team-B fixtures seed their prompt here with the matching team_id.
func insertPromptForTeam(t *testing.T, database *sql.DB, id, teamID string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO prompts (id, org_id, team_id, creator_user_id, source, name, body, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'user', ?, 'x', datetime('now'), datetime('now'))
	`, id, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID, id); err != nil {
		t.Fatalf("insertPromptForTeam %s (team %s): %v", id, teamID, err)
	}
}

// insertBlueprintForTeam seeds a user blueprint (+ a 1-step list wrapping
// promptID) owned by a specific team via raw INSERTs. Mirrors
// insertPromptForTeam: the SQLite BlueprintStore.Create / ReplaceSteps pin
// LocalDefaultTeamID (team A), so a team-B trigger fixture that must reference
// a team-B blueprint seeds it here with the matching team_id. Returns the
// blueprint id.
func insertBlueprintForTeam(t *testing.T, database *sql.DB, blueprintID, promptID, teamID string) string {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO blueprints (id, org_id, team_id, creator_user_id, source, name, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'user', ?, datetime('now'), datetime('now'))
	`, blueprintID, runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID, blueprintID); err != nil {
		t.Fatalf("insertBlueprintForTeam %s (team %s): %v", blueprintID, teamID, err)
	}
	if _, err := database.Exec(`
		INSERT INTO blueprint_steps (blueprint_id, step_index, step_prompt_id, team_id, created_at)
		VALUES (?, 0, ?, ?, datetime('now'))
	`, blueprintID, promptID, teamID); err != nil {
		t.Fatalf("insertBlueprintForTeam step %s (team %s): %v", blueprintID, teamID, err)
	}
	return blueprintID
}

// intPtr / floatPtr are convenience wrappers for the pointer-shaped
// per-kind fields on domain.EventHandler. Trigger rows carry
// BreakerThreshold (*int) and MinAutonomySuitability (*float64); test
// fixtures with literal values pass them through these helpers.
func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }

// seedHandlerFKTargets seeds the prompts AND blueprints that shipped triggers
// reference so EventHandlerStore.Seed's trigger rows resolve their FK to a
// same-team blueprint (a trigger fires a blueprint, not a prompt
// directly). Production's SeedShippedIntoTeam seeds prompts → blueprints (each
// wrapping its prompt as a 1-step list) → handlers; tests that call Seed
// directly replicate that ordering here. Returns the system_slug →
// blueprint-id map the caller threads into Seed (the id is a random UUID per
// team copy).
func seedHandlerFKTargets(t *testing.T, database *sql.DB) map[string]string {
	t.Helper()
	ctx := context.Background()
	prompts := testPromptStore(database)
	blueprints := testBlueprintStore(database)
	slugs := []struct {
		slug, name string
	}{
		{"system-pr-review", "PR Review"},
		{"system-conflict-resolution", "Conflicts"},
		{"system-ci-fix", "CI Fix"},
		{"system-jira-implement", "Jira Implement"},
		{"system-fix-review-feedback", "Fix Review"},
	}
	out := map[string]string{}
	for _, s := range slugs {
		// The handler FK only needs a same-team prompt + blueprint to exist; plain
		// user rows satisfy it (this test resolves the trigger's blueprint through
		// the returned slug→id map, not by system_slug). The id is a random UUID.
		promptID := uuid.New().String()
		if _, err := prompts.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
			domain.Prompt{ID: promptID, Name: s.name, Body: "x", Source: "user"}); err != nil {
			t.Fatalf("seed prompt %s: %v", s.slug, err)
		}
		bpID := uuid.New().String()
		if _, err := blueprints.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
			domain.Blueprint{ID: bpID, Name: s.name, Source: "user"}); err != nil {
			t.Fatalf("seed blueprint %s: %v", s.slug, err)
		}
		if _, err := blueprints.ReplaceSteps(ctx, runmode.LocalDefaultOrgID, bpID, []string{promptID}, nil); err != nil {
			t.Fatalf("seed blueprint steps %s: %v", s.slug, err)
		}
		out[s.slug] = bpID
	}
	return out
}
