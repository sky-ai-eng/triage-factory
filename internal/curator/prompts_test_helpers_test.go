package curator

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
// fixtures. Tests pass this to curator.New + materializeSpecSkill so
// prompt-loading code paths exercise the same store interface
// production uses.
func testPromptStore(database *sql.DB) db.PromptStore {
	if database == nil {
		return nil
	}
	return sqlitestore.New(database).Prompts
}

// seedTestPrompt is the replacement for the deleted db.SeedPrompt
// free-function (which inserted-if-missing). Routes through the
// PromptStore so test fixtures share insert SQL with production.
// Idempotent: re-seeding a present ID is a no-op.
func seedTestPrompt(t *testing.T, database *sql.DB, p domain.Prompt) {
	t.Helper()
	store := testPromptStore(database)
	ctx := context.Background()
	// System prompts seed through SeedOrUpdate so they carry a system_slug
	// (the curator's default-spec fallback resolves them by slug, not id,
	// post-SKY-380 — the row's id is a random UUID). The fixture's ID field
	// is reinterpreted as the slug. User/imported prompts keep their explicit
	// id via Create (the project-override path resolves those by id).
	if p.Source == "system" {
		if p.SystemSlug == "" {
			p.SystemSlug = p.ID
		}
		if existing, _ := store.GetBySystemSlug(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, p.SystemSlug); existing != nil {
			return
		}
		if _, err := store.SeedOrUpdate(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, p); err != nil {
			t.Fatalf("seedTestPrompt %s: %v", p.SystemSlug, err)
		}
		return
	}
	existing, _ := store.Get(ctx, runmode.LocalDefaultOrg, p.ID)
	if existing != nil {
		return
	}
	if err := store.Create(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, p); err != nil {
		t.Fatalf("seedTestPrompt %s: %v", p.ID, err)
	}
}
