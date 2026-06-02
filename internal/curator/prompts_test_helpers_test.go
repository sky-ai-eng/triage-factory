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

// resolvePromptID returns the actual prompt id for a seeded fixture. System
// prompts get a random UUID id (slug-keyed), so the fixture's ID field is
// reinterpreted as the slug and resolved via GetBySystemSlug; user/imported
// prompts keep their explicit id.
func resolvePromptID(t *testing.T, database *sql.DB, source, idOrSlug string) string {
	t.Helper()
	store := testPromptStore(database)
	ctx := context.Background()
	if source == "system" {
		p, err := store.GetBySystemSlug(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, idOrSlug)
		if err != nil || p == nil {
			t.Fatalf("resolvePromptID system %s: p=%v err=%v", idOrSlug, p, err)
		}
		return p.ID
	}
	return idOrSlug
}

// seedTestBlueprint creates a single-step blueprint wrapping promptID and
// returns the blueprint's id. The project's spec-authorship resolution now
// goes project → blueprint → first step → prompt, so spec tests must wrap the
// prompt in a blueprint and point the project at the blueprint id. A non-empty
// systemSlug seeds a system-source blueprint (resolvable via GetBySystemSlug,
// the default-spec fallback path); an empty slug creates a user-source one.
func seedTestBlueprint(t *testing.T, database *sql.DB, promptID, systemSlug string) string {
	t.Helper()
	stores := sqlitestore.New(database)
	ctx := context.Background()
	var blueprintID string
	if systemSlug != "" {
		id, err := stores.Blueprints.SeedOrUpdate(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Blueprint{
			SystemSlug: systemSlug, Name: systemSlug, Source: "system",
		})
		if err != nil {
			t.Fatalf("seedTestBlueprint system %s: %v", systemSlug, err)
		}
		blueprintID = id
	} else {
		blueprintID = "bp-" + promptID
		if err := stores.Blueprints.Create(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Blueprint{
			ID: blueprintID, Name: "bp-" + promptID, Source: "user", TeamID: runmode.LocalDefaultTeamID,
		}); err != nil {
			t.Fatalf("seedTestBlueprint user %s: %v", promptID, err)
		}
	}
	if err := stores.Blueprints.ReplaceSteps(ctx, runmode.LocalDefaultOrg, blueprintID, []string{promptID}, nil); err != nil {
		t.Fatalf("seedTestBlueprint replace steps %s: %v", blueprintID, err)
	}
	return blueprintID
}
