package app_test

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// openLocalStores provisions a fresh in-memory local install seeded with the
// REAL shipped defaults — the "Start your factory" path — and returns the wired
// stores plus the raw handle for simulating an old install's direct DB edits.
func openLocalStores(t *testing.T) (db.Stores, *sql.DB) {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return sqlitestore.New(conn), conn
}

// TestShippedDefaultsSync_LocalBoot exercises the boot-time sweep
// (db.SyncShippedDefaultsForAllTeams — what startBrain spawns) against the REAL
// shipped lists, pinning the ticket's acceptance criteria in local mode:
//
//   - a shipped prompt body edited directly in the DB (an old install with no
//     user_modified flag) is restored to the shipped content on the next boot;
//   - a body edited through the store (which stamps user_modified) survives.
func TestShippedDefaultsSync_LocalBoot(t *testing.T) {
	stores, conn := openLocalStores(t)
	ctx := context.Background()
	if err := db.BootstrapLocalOrg(ctx, stores, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("provision local tenant: %v", err)
	}
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID

	const slug = "system-ci-fix"
	var shippedBody string
	for _, p := range ai.ShippedPrompts() {
		if p.SystemSlug == slug {
			shippedBody = p.Body
		}
	}
	if shippedBody == "" {
		t.Fatalf("shipped prompt %q not found or empty", slug)
	}

	seeded, err := stores.Prompts.GetBySystemSlug(ctx, org, team, slug)
	if err != nil || seeded == nil {
		t.Fatalf("GetBySystemSlug after provision: (%v, %v)", seeded, err)
	}

	// Simulate an old install: a shipped body edited directly in the DB, with no
	// user_modified flag. The boot sweep must restore it to shipped content.
	if _, err := conn.Exec(`UPDATE prompts SET body = 'stale hand-edit' WHERE id = ?`, seeded.ID); err != nil {
		t.Fatalf("simulate direct edit: %v", err)
	}
	if err := db.SyncShippedDefaultsForAllTeams(ctx, stores, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("sync (restore): %v", err)
	}
	restored, _ := stores.Prompts.GetBySystemSlug(ctx, org, team, slug)
	if restored == nil || restored.Body != shippedBody {
		t.Fatalf("direct-edited shipped prompt not restored: body=%q, want shipped content", bodyOf(restored))
	}

	// A store edit stamps user_modified; the next sweep must leave it alone.
	if err := stores.Prompts.Update(ctx, org, restored.ID, "My CI Fix", "my custom body", ""); err != nil {
		t.Fatalf("user update: %v", err)
	}
	if err := db.SyncShippedDefaultsForAllTeams(ctx, stores, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("sync (protect): %v", err)
	}
	survived, _ := stores.Prompts.GetBySystemSlug(ctx, org, team, slug)
	if survived == nil || survived.Body != "my custom body" || survived.Name != "My CI Fix" {
		t.Fatalf("user edit overwritten by sweep: %+v", survived)
	}
}

// TestShippedDefaultsSync_NoTenant pins that the sweep no-ops cleanly on a fresh
// install with zero provisioned tenants (the common local first-run state).
func TestShippedDefaultsSync_NoTenant(t *testing.T) {
	stores, _ := openLocalStores(t)
	if err := db.SyncShippedDefaultsForAllTeams(context.Background(), stores, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("sweep on unprovisioned install returned error: %v", err)
	}
}

func bodyOf(p *domain.Prompt) string {
	if p == nil {
		return "<nil>"
	}
	return p.Body
}
