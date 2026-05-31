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
// production share the same insert SQL. The SQLite store pins
// LocalDefaultTeamID (team A), so this owns the row to team A — for a
// prompt a second team's trigger/chain must reference, use
// insertPromptForTeam.
func createTestPrompt(t *testing.T, database *sql.DB, p domain.Prompt) {
	t.Helper()
	store := testPromptStore(database)
	if err := store.Create(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, p); err != nil {
		t.Fatalf("createTestPrompt %s: %v", p.ID, err)
	}
}

// insertPromptForTeam seeds a user prompt owned by a specific team via a
// raw INSERT. createTestPrompt routes through PromptStore.Create which pins
// LocalDefaultTeamID (team A); a team-B trigger or chain step can only bind
// a prompt its own team owns under the same-team composite FK (SKY-380), so
// team-B fixtures seed their prompt here with the matching team_id.
func insertPromptForTeam(t *testing.T, database *sql.DB, id, teamID string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO prompts (id, org_id, team_id, creator_user_id, visibility, source, kind, name, body, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'team', 'user', 'leaf', ?, 'x', datetime('now'), datetime('now'))
	`, id, runmode.LocalDefaultOrg, teamID, runmode.LocalDefaultUserID, id); err != nil {
		t.Fatalf("insertPromptForTeam %s (team %s): %v", id, teamID, err)
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
// the same loop; tests that call Seed directly replicate that ordering
// manually. Returns the system_slug → prompt-id map the caller threads into
// Seed (the id is a random UUID per team copy post-SKY-380; the two-phase
// seed resolves a trigger's prompt slug through this map).
func seedHandlerFKTargets(t *testing.T, database *sql.DB) map[string]string {
	t.Helper()
	store := testPromptStore(database)
	out := map[string]string{}
	for _, p := range []domain.Prompt{
		{SystemSlug: "system-pr-review", Name: "PR Review", Body: "x", Source: "system"},
		{SystemSlug: "system-conflict-resolution", Name: "Conflicts", Body: "x", Source: "system"},
		{SystemSlug: "system-ci-fix", Name: "CI Fix", Body: "x", Source: "system"},
		{SystemSlug: "system-jira-implement", Name: "Jira Implement", Body: "x", Source: "system"},
		{SystemSlug: "system-fix-review-feedback", Name: "Fix Review", Body: "x", Source: "system"},
	} {
		id, err := store.SeedOrUpdate(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, p)
		if err != nil {
			t.Fatalf("seed prompt %s: %v", p.SystemSlug, err)
		}
		out[p.SystemSlug] = id
	}
	return out
}
