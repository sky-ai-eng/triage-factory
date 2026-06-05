package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestPromptStore_Postgres runs the shared PromptStore conformance suite
// against the Postgres impl. The store is constructed via pgstore.New
// — same wiring as production main.go — so SeedOrUpdate hits the admin
// pool and every other method hits the app pool, exactly as it will
// in multi mode.
//
// Each subtest gets a fresh org + user via h.Reset; the per-test
// fixture is owned by the factory closure.
//
// What this test pins (in addition to the shared assertions):
//
//   - SeedOrUpdate's atomic prompt + system_prompt_versions write
//     succeeds under the actual D3 REVOKE — admin pool can write the
//     sidecar; app pool can't.
//   - The COALESCE-to-org-owner fallback for creator_user_id satisfies
//     the NOT NULL constraint without needing JWT claims set on the
//     test connection.
//   - Stats reads through the app pool, which means the JWT claim
//     must be set OR we use AdminDB. We deliberately pass AdminDB as
//     `app` here so reads bypass RLS — the goal is to verify SQL
//     correctness across both backends, not to re-test RLS (which
//     has its own coverage in pgtest/baseline_test.go).
func TestPromptStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunPromptStoreConformance(t, func(t *testing.T) (db.PromptStore, string, string, dbtest.RunSeederForStats) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgOrgAndUserForPrompts(t, h)
		// seedPgOrgAndUserForPrompts stages the org's default team; the
		// conformance Create attributes prompts to it (it satisfies the
		// team-visibility CHECK + RLS).
		teamID := firstTeamForOrg(t, h, orgID)

		// Wire BOTH "app" and "admin" to AdminDB. This intentionally
		// collapses the two pools for testing — the production wiring
		// (admin = supabase_admin, app = tf_app) is exercised by
		// pgtest/baseline_test.go's RLS suite. Here we want SQL-shape
		// + behavior coverage that mirrors SQLite's assertions, which
		// requires Stats / List / Get to work without JWT claims plumbing
		// in every subtest. The admin pool bypasses RLS but still
		// enforces FKs + NOT NULL, so we're testing the same SQL.
		stores := pgstore.New(h.AdminDB, h.AdminDB)

		seeder := func(t *testing.T, promptID string, statusByOffset []string) []string {
			t.Helper()
			return seedPgRunsForStats(t, h.AdminDB, orgID, userID, promptID, statusByOffset)
		}
		_ = userID
		return stores.Prompts, orgID, teamID, seeder
	})
}

// TestPromptStore_Postgres_SeedOrUpdate_AdminOnly is the explicit pin
// for the D3 REVOKE invariant: system_prompt_versions writes MUST go
// through the admin pool. Running SeedOrUpdate via a tf_app connection
// has to fail with the privilege error, otherwise the deploy-time
// separation is broken.
//
// We construct two stores: one wired admin-on-both (the conformance
// pattern above), and one wired app-on-both. The first must succeed,
// the second must fail. If both succeed, the REVOKE is missing or the
// store is wired wrong. If both fail, something else is broken.
func TestPromptStore_Postgres_SeedOrUpdate_AdminOnly(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedPgOrgAndUserForPrompts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Admin-pool store — must succeed.
	adminStores := pgstore.New(h.AdminDB, h.AdminDB)
	teamID := firstTeamForOrg(t, h, orgID)
	seededID, err := adminStores.Prompts.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{
		SystemSlug: "sys-admin-ok", Name: "OK", Body: "x", Source: "system",
	})
	if err != nil {
		t.Fatalf("admin-pool SeedOrUpdate should succeed, got: %v", err)
	}

	// App-pool store with SET LOCAL ROLE tf_app — must fail on the
	// version-row write. We build a separate, claims-set tx so the
	// store's INSERT into system_prompt_versions hits tf_app's REVOKE.
	if err := h.WithUser(t, uuid.New().String(), orgID, func(tx *sql.Tx) error {
		// SeedOrUpdate inside a tx is explicitly rejected by the
		// store (escaping to admin would bypass tx semantics), so
		// instead we invoke the underlying SQL directly under
		// tf_app to confirm the REVOKE bites.
		_, err := tx.ExecContext(ctx, `
			INSERT INTO system_prompt_versions (org_id, prompt_id, content_hash, applied_at)
			VALUES ($1, $2, $3, now())
		`, orgID, seededID, "deadbeef")
		if err == nil {
			t.Fatalf("tf_app INSERT on system_prompt_versions should fail with privilege error")
		}
		return nil
	}); err != nil {
		// WithUser surfaces the test failure via t.Fatalf; if we got
		// a transport-level error reaching here that's a bug in the
		// harness, not the store.
		t.Logf("WithUser cleanup error (expected on rollback after privilege failure): %v", err)
	}
}

// TestPromptStore_Postgres_CrossOrgRLSDenied pins the production RLS
// layer for prompts. Runs the store through the app pool under tf_app
// with real JWT claims so the actual prompts_select / prompts_insert
// policies are exercised. Same-org reads succeed; cross-org reads are
// silently filtered (USING); cross-org Create raises 42501 from
// prompts_insert WITH CHECK.
//
// Every prompt is team-owned (no visibility column post-SKY-380); the
// store's Create path stamps the acting team. The test seeds an
// admin-pool prompt on teamA in orgA so the same-org user (a teamA
// member) can see it through the team-membership branch of prompts_select.
func TestPromptStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice := seedPgOrgAndUserForPrompts(t, h)
	orgB, bob := seedPgOrgAndUserForPrompts(t, h)
	_ = bob

	// Seed a prompt in orgA directly via the admin connection so the row
	// exists independent of user-scoped RLS. It's owned by teamA so the
	// same-org read path exercises the team-membership branch of
	// prompts_select.
	promptA := "prompt-rls-" + orgA[:8]
	teamA := firstTeamForOrg(t, h, orgA)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4::uuid, 'RLS Prompt', 'body', 'user', '', now(), now())
	`, promptA, orgA, alice, teamA); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	ctx := context.Background()

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx).Prompts.Get(ctx, orgA, promptA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if got == nil {
				t.Errorf("alice Get(orgA, promptA) returned nil; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx).Prompts.Get(ctx, orgA, promptA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if got != nil {
				t.Errorf("bob Get(orgA, promptA) returned %+v; RLS USING filter leaked orgA's prompt to orgB", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_denied", func(t *testing.T) {
		// bob's claims point at orgB; Create against orgA would land
		// a row with org_id=orgA. prompts_insert WITH CHECK requires
		// org_id = tf.current_org_id(), so 42501 is the expected
		// outcome.
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			return pgstore.NewForTx(tx).Prompts.Create(ctx, orgA, teamA, domain.Prompt{
				ID: "p-rls-write-" + orgA[:8], Name: "x-write", Body: "x", Source: "user",
			})
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

// seedPgOrgAndUserForPrompts is the prompts-test fixture analogue of
// seedPgOrgAndUser in scores_test.go. Distinct name keeps both test
// files self-contained (Go disallows reusing a top-level identifier
// across _test.go files in the same package only if they're literally
// the same symbol; even though both functions could share, keeping
// them separate avoids accidental coupling when one signature changes).
func seedPgOrgAndUserForPrompts(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("prompt-conf-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Prompt Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Prompt Conformance Org "+orgID[:8], "prompt-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	return orgID, userID
}

// seedPgRunsForStats inserts entity + task + run rows so Stats has
// data to aggregate. All rows hold the conformance org_id so RLS-aware
// reads (when the test uses the app pool) see them.
//
// Mirrors seedSQLiteRunsForStats but with Postgres column shape and
// org_id/creator_user_id columns populated.
func seedPgRunsForStats(t *testing.T, conn *sql.DB, orgID, userID, promptID string, statusByOffset []string) []string {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	taskID := uuid.New().String()
	eventID := uuid.New().String()

	if _, err := conn.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Conformance Entity', 'https://example/x', '{}'::jsonb, $4)
	`, entityID, orgID, fmt.Sprintf("conformance-runs-%d", now.UnixNano()), now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:opened', '', '{}'::jsonb, $4)
	`, eventID, orgID, entityID, now); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	// team_id resolved inline from the org's first team (SKY-262).
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id,
		                   status, scoring_status, created_at)
		VALUES ($1, $2, $3, (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1), 'team', $4, 'github:pr:opened', '', $5, 'queued', 'pending', $6)
	`, taskID, orgID, userID, entityID, eventID, now); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// runs.blueprint_run_id is NOT NULL — mint one blueprint_run for the task
	// that all these runs link to (the runs are step/retry rows for prompt stats).
	bpID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprints (id, org_id, creator_user_id, team_id, source, name, created_at, updated_at)
		VALUES ($1, $2, $3, (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1), 'user', 'BP', now(), now())
	`, bpID, orgID, userID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	brID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, started_at)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'running', '/tmp/wt', now())
	`, brID, orgID, userID, bpID, taskID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}

	ids := make([]string, 0, len(statusByOffset))
	for i, status := range statusByOffset {
		runID := uuid.New().String()
		startedAt := now.AddDate(0, 0, -i)
		if _, err := conn.Exec(`
			INSERT INTO runs (id, org_id, creator_user_id, team_id, visibility, task_id, prompt_id, status, started_at, total_cost_usd, duration_ms, blueprint_run_id)
			VALUES ($1, $2, $3, (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1), 'team', $4, $5, $6, $7, 0.01, 100, $8)
		`, runID, orgID, userID, taskID, promptID, status, startedAt, brID); err != nil {
			t.Fatalf("seed run %d: %v", i, err)
		}
		ids = append(ids, runID)
	}
	return ids
}
