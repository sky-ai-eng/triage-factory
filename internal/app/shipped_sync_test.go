package app_test

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/promptseed"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// openLocalStores provisions a fresh in-memory local install seeded with the
// REAL shipped defaults — the "Start your factory" path — and returns the wired
// stores plus the raw handle for simulating an old install's direct DB edits.
func openLocalStores(t *testing.T) (db.Stores, *sql.DB) {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
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
	if err := db.BootstrapLocalOrg(ctx, stores, promptseed.Prompts(), promptseed.Blueprints()); err != nil {
		t.Fatalf("provision local tenant: %v", err)
	}
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID

	const slug = "system-ci-fix"
	var shippedBody string
	for _, p := range promptseed.Prompts() {
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
	if err := db.SyncShippedDefaultsForAllTeams(ctx, stores, promptseed.Prompts(), promptseed.Blueprints()); err != nil {
		t.Fatalf("sync (restore): %v", err)
	}
	restored, _ := stores.Prompts.GetBySystemSlug(ctx, org, team, slug)
	if restored == nil || restored.Body != shippedBody {
		t.Fatalf("direct-edited shipped prompt not restored: body=%q, want shipped content", bodyOf(restored))
	}

	// A store edit stamps user_modified; the next sweep must leave it alone.
	if _, err := stores.Prompts.Update(ctx, org, restored.ID, "My CI Fix", "my custom body", ""); err != nil {
		t.Fatalf("user update: %v", err)
	}
	if err := db.SyncShippedDefaultsForAllTeams(ctx, stores, promptseed.Prompts(), promptseed.Blueprints()); err != nil {
		t.Fatalf("sync (protect): %v", err)
	}
	survived, _ := stores.Prompts.GetBySystemSlug(ctx, org, team, slug)
	if survived == nil || survived.Body != "my custom body" || survived.Name != "My CI Fix" {
		t.Fatalf("user edit overwritten by sweep: %+v", survived)
	}
}

// TestShippedDefaultsSync_EveryPromptRoundTrips walks the whole shipped set
// rather than one representative slug: each body must survive the seed
// verbatim, and a direct DB edit of any of them must be restored by the boot
// sweep. A prompt reachable by the seeder but not by the drift sync (one no
// blueprint wraps) passes the first half and fails the second, which is
// exactly the failure this catches.
func TestShippedDefaultsSync_EveryPromptRoundTrips(t *testing.T) {
	stores, conn := openLocalStores(t)
	ctx := context.Background()
	if err := db.BootstrapLocalOrg(ctx, stores, promptseed.Prompts(), promptseed.Blueprints()); err != nil {
		t.Fatalf("provision local tenant: %v", err)
	}
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID

	for _, want := range promptseed.Prompts() {
		seeded, err := stores.Prompts.GetBySystemSlug(ctx, org, team, want.SystemSlug)
		if err != nil || seeded == nil {
			t.Fatalf("%s: not seeded: (%v, %v)", want.SystemSlug, seeded, err)
		}
		if seeded.Body != want.Body {
			t.Errorf("%s: seeded body differs from the shipped file", want.SystemSlug)
		}
		if _, err := conn.Exec(`UPDATE prompts SET body = 'stale hand-edit' WHERE id = ?`, seeded.ID); err != nil {
			t.Fatalf("%s: simulate direct edit: %v", want.SystemSlug, err)
		}
	}

	if err := db.SyncShippedDefaultsForAllTeams(ctx, stores, promptseed.Prompts(), promptseed.Blueprints()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	for _, want := range promptseed.Prompts() {
		got, err := stores.Prompts.GetBySystemSlug(ctx, org, team, want.SystemSlug)
		if err != nil || got == nil {
			t.Fatalf("%s: missing after sync: (%v, %v)", want.SystemSlug, got, err)
		}
		if got.Body != want.Body {
			t.Errorf("%s: drift sync did not restore the shipped body (got %q) — is it wrapped by a shipped blueprint?",
				want.SystemSlug, bodyOf(got))
		}
	}
}

// TestShippedDefaultsSync_NoTenant pins that the sweep no-ops cleanly on a fresh
// install with zero provisioned tenants (the common local first-run state).
func TestShippedDefaultsSync_NoTenant(t *testing.T) {
	stores, _ := openLocalStores(t)
	if err := db.SyncShippedDefaultsForAllTeams(context.Background(), stores, promptseed.Prompts(), promptseed.Blueprints()); err != nil {
		t.Fatalf("sweep on unprovisioned install returned error: %v", err)
	}
}

func bodyOf(p *domain.Prompt) string {
	if p == nil {
		return "<nil>"
	}
	return p.Body
}
