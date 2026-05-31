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
)

// TestTaskMemoryStore_Postgres runs the shared conformance suite
// against the Postgres TaskMemoryStore impl. Wires both pools
// against AdminDB (BYPASSRLS) so the behavior tests stay independent
// of the auth path; the cross-org isolation tests below exercise
// the org_id filter directly.
func TestTaskMemoryStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)

	dbtest.RunTaskMemoryStoreConformance(t, func(t *testing.T) (db.TaskMemoryStore, string, dbtest.TaskMemorySeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgTaskMemoryOrg(t, h)
		promptID := seedPgTaskMemoryPrompt(t, h, orgID, userID)
		seed := dbtest.TaskMemorySeeder{
			Run: func(t *testing.T, suffix string) (string, string) {
				t.Helper()
				return seedPgRunForTaskMemory(t, h, orgID, userID, promptID, suffix)
			},
		}
		return stores.TaskMemory, orgID, seed
	})
}

// TestTaskMemoryStore_Postgres_CrossOrgLeakage pins the defense-in-
// depth org_id filter on every read + write path. RLS via
// run_memory_all also enforces this in production; the org_id = $N
// clause in each query is the belt to RLS's suspenders that fires
// regardless of whether the admin pool bypasses RLS.
func TestTaskMemoryStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB)

	orgA, userA := seedPgTaskMemoryOrg(t, h)
	orgB, userB := seedPgTaskMemoryOrg(t, h)
	promptA := seedPgTaskMemoryPrompt(t, h, orgA, userA)
	promptB := seedPgTaskMemoryPrompt(t, h, orgB, userB)

	runA, entA := seedPgRunForTaskMemory(t, h, orgA, userA, promptA, "cross-A")
	_, entB := seedPgRunForTaskMemory(t, h, orgB, userB, promptB, "cross-B")
	ctx := context.Background()

	// Write the memory in orgA via the admin-pool variant (the
	// delegate spawner's call site).
	if err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgA, runA, entA, "orgA narrative"); err != nil {
		t.Fatalf("UpsertAgentMemorySystem orgA: %v", err)
	}

	// GetMemoriesForEntity (app-pool variant) under orgB must NOT
	// see orgA's row — both because the entity belongs to a
	// different org and because the org_id WHERE filter is applied
	// to run_memory in addition to entity_links/entities.
	memsB, err := stores.TaskMemory.GetMemoriesForEntity(ctx, orgB, entA)
	if err != nil {
		t.Fatalf("GetMemoriesForEntity orgB on orgA entity: %v", err)
	}
	if len(memsB) != 0 {
		t.Errorf("orgB read %d memories on orgA entity — org_id filter leaked", len(memsB))
	}

	// Admin-pool variant must also honor the data-column filter even
	// though it bypasses RLS — the impl binds org_id in every WHERE
	// clause as defense in depth.
	memsBSystem, err := stores.TaskMemory.GetMemoriesForEntitySystem(ctx, orgB, entA)
	if err != nil {
		t.Fatalf("GetMemoriesForEntitySystem orgB on orgA entity: %v", err)
	}
	if len(memsBSystem) != 0 {
		t.Errorf("orgB admin-pool read %d memories on orgA entity — data-column filter leaked", len(memsBSystem))
	}

	// GetRunMemory by orgA's run id from orgB returns nil for the
	// same reason.
	gotB, err := stores.TaskMemory.GetRunMemory(ctx, orgB, runA)
	if err != nil {
		t.Fatalf("GetRunMemory orgB on orgA run: %v", err)
	}
	if gotB != nil {
		t.Errorf("orgB read orgA run memory: %+v", gotB)
	}

	// UpdateRunMemoryHumanContent under orgB on orgA's run is a
	// no-op (no row matches the (org_id, run_id) predicate) and is
	// logged-not-fatal — same shape as the missing-row case in the
	// conformance suite.
	if err := stores.TaskMemory.UpdateRunMemoryHumanContent(ctx, orgB, runA, "should-not-land"); err != nil {
		t.Errorf("UpdateRunMemoryHumanContent orgB on orgA run errored: %v", err)
	}
	// Confirm orgA's row is unchanged.
	memA, err := stores.TaskMemory.GetRunMemory(ctx, orgA, runA)
	if err != nil || memA == nil {
		t.Fatalf("GetRunMemory orgA: mem=%v err=%v", memA, err)
	}
	if memA.Content != "orgA narrative" {
		t.Errorf("orgA Content mutated by orgB-scoped UPDATE: got %q, want %q", memA.Content, "orgA narrative")
	}
	_ = entB
}

// TestTaskMemoryStore_Postgres_CrossOrgRLSDenied pins the production
// RLS layer for run_memory. Where CrossOrgLeakage above wires both
// pools against AdminDB to prove the defense-in-depth WHERE-clause
// filter is intact, this test runs the store through the app pool
// under tf_app with real JWT claims so the actual run_memory_all
// policy is exercised. Same-org reads succeed; cross-org reads are
// silently filtered (USING); cross-org UpsertAgentMemory raises
// 42501 from the WITH CHECK side of the same policy.
func TestTaskMemoryStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice := seedPgTaskMemoryOrg(t, h)
	orgB, bob := seedPgTaskMemoryOrg(t, h)
	promptA := seedPgTaskMemoryPrompt(t, h, orgA, alice)
	promptB := seedPgTaskMemoryPrompt(t, h, orgB, bob)

	runA, entA := seedPgRunForTaskMemory(t, h, orgA, alice, promptA, "rls-A")
	_, _ = seedPgRunForTaskMemory(t, h, orgB, bob, promptB, "rls-B")

	// Seed a memory row in orgA via admin so the row exists.
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	ctx := context.Background()
	if err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgA, runA, entA, "orgA memory"); err != nil {
		t.Fatalf("seed memory in orgA: %v", err)
	}

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx).TaskMemory.GetRunMemory(ctx, orgA, runA)
			if err != nil {
				return fmt.Errorf("GetRunMemory: %w", err)
			}
			if got == nil {
				t.Errorf("alice GetRunMemory(orgA, runA) returned nil; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx).TaskMemory.GetRunMemory(ctx, orgA, runA)
			if err != nil {
				return fmt.Errorf("GetRunMemory: %w", err)
			}
			if got != nil {
				t.Errorf("bob GetRunMemory(orgA, runA) returned %+v; RLS USING filter leaked orgA's memory to orgB", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_denied", func(t *testing.T) {
		// bob's claims point at orgB; UpsertAgentMemory against orgA
		// would land a row with org_id=orgA. run_memory_all WITH CHECK
		// requires org_id = tf.current_org_id(), so 42501 is the
		// expected outcome. Seed a second run in orgA with NO existing
		// memory row so the UPSERT exercises the INSERT side of the
		// policy (WITH CHECK) rather than the ON CONFLICT UPDATE side
		// (USING) — runA above already has a seeded memory row from
		// line 141 above, which would route through ON CONFLICT and
		// hit USING instead.
		freshRun, freshEnt := seedPgRunForTaskMemory(t, h, orgA, alice, promptA, "rls-write")
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			return pgstore.NewForTx(tx).TaskMemory.UpsertAgentMemory(ctx, orgA, freshRun, freshEnt, "cross-org write")
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

// seedPgTaskMemoryOrg builds the auth user + public user + org +
// org_membership + default team graph the run_memory FK chain
// transitively needs (runs → tasks → events → entities → orgs).
// Mirrors seedPgFactoryOrg minus the prompt seed (which we want as
// a separate helper so a single org can host multiple prompts).
func seedPgTaskMemoryOrg(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("task-memory-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)

	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Task Memory Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Task Memory Org "+orgID[:8], "task-memory-"+orgID[:8], userID,
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

func seedPgTaskMemoryPrompt(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	promptID := "p_task_memory_" + uuid.New().String()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, kind, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'Task Memory Test', 'body', 'user', 'leaf', '', now(), now())
	`, promptID, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return promptID
}

// seedPgRunForTaskMemory seeds the entity + event + task + run FK
// chain run_memory needs. Returns (runID, entityID).
func seedPgRunForTaskMemory(t *testing.T, h *pgtest.Harness, orgID, userID, promptID, suffix string) (string, string) {
	t.Helper()
	conn := h.AdminDB
	now := time.Now().UTC()

	entityID := uuid.New().String()
	sourceID := fmt.Sprintf("task-memory-%s-%s", suffix, entityID[:8])
	if _, err := conn.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, state)
		VALUES ($1, $2, 'github', $3, 'pr', 'Task Memory Conformance', 'https://example/x', '{}'::jsonb, $4, 'active')
	`, entityID, orgID, sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	eventID := uuid.New().String()
	const eventType = "github:pr:opened"
	if _, err := conn.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, $4, '', '{}'::jsonb, $5)
	`, eventID, orgID, entityID, eventType, now); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	taskID := uuid.New().String()
	// 'done' is the terminal task status. Pre-SKY-330 this seed wrote
	// 'completed' — never a valid task value (completed is run-level)
	// but silently accepted because the column had no CHECK. SKY-330's
	// CHECK constraint surfaces the latent fixture bug; same fix as
	// the SQLite mirror.
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id,
		                   status, scoring_status, priority_score, created_at)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'team', $4, $5, '', $6, 'done', 'pending', 0.5, $7)
	`, taskID, orgID, userID, entityID, eventType, eventID, now); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	runID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO runs (id, org_id, creator_user_id, team_id, visibility, task_id, prompt_id, trigger_type, status)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'team', $4, $5, 'manual', 'completed')
	`, runID, orgID, userID, taskID, promptID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID, entityID
}
