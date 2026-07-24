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

// TestTaskMemoryStore_Postgres runs the shared conformance suite
// against the Postgres TaskMemoryStore impl. Wires both pools
// against AdminDB (BYPASSRLS) so the behavior tests stay independent
// of the auth path; the cross-org isolation tests below exercise
// the org_id filter directly.
func TestTaskMemoryStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

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
			BlueprintRun: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedPgBlueprintRunForTaskMemory(t, h, orgID, userID, suffix)
			},
			Role: func(t *testing.T, runID, entityID string) string {
				t.Helper()
				return roleForPgJoinRow(t, h, runID, entityID)
			},
		}
		return stores.TaskMemory, orgID, seed
	})
}

// roleForPgJoinRow reads back conversation_memory_entities.role directly via the
// admin pool (bypasses RLS) — the store interface has no role-returning
// read, so the RecordEntityTouchSystem precedence conformance subtest
// needs a raw escape hatch. Returns "" if no row exists.
func roleForPgJoinRow(t *testing.T, h *pgtest.Harness, runID, entityID string) string {
	t.Helper()
	var role string
	err := h.AdminDB.QueryRow(`SELECT role FROM conversation_memory_entities WHERE conversation_id = $1 AND entity_id = $2`, runID, entityID).Scan(&role)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read conversation_memory_entities role: %v", err)
	}
	return role
}

// TestTaskMemoryStore_Postgres_CrossOrgLeakage pins the defense-in-
// depth org_id filter on every read + write path. RLS via
// conversation_memory_all also enforces this in production; the org_id = $N
// clause in each query is the belt to RLS's suspenders that fires
// regardless of whether the admin pool bypasses RLS.
func TestTaskMemoryStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgA, userA := seedPgTaskMemoryOrg(t, h)
	orgB, userB := seedPgTaskMemoryOrg(t, h)
	promptA := seedPgTaskMemoryPrompt(t, h, orgA, userA)
	promptB := seedPgTaskMemoryPrompt(t, h, orgB, userB)

	runA, entA := seedPgRunForTaskMemory(t, h, orgA, userA, promptA, "cross-A")
	_, entB := seedPgRunForTaskMemory(t, h, orgB, userB, promptB, "cross-B")
	ctx := context.Background()

	// Write the memory in orgA via the admin-pool variant (the
	// delegate spawner's call site).
	if err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgA, runA, entA, "", "orgA narrative"); err != nil {
		t.Fatalf("UpsertAgentMemorySystem orgA: %v", err)
	}

	// GetMemoriesForEntity (app-pool variant) under orgB must NOT
	// see orgA's row — both because the entity belongs to a
	// different org and because the org_id WHERE filter is applied
	// to conversation_memory in addition to entity_links/entities.
	memsB, err := stores.TaskMemory.GetMemoriesForEntity(ctx, orgB, entA)
	if err != nil {
		t.Fatalf("GetMemoriesForEntity orgB on orgA entity: %v", err)
	}
	if len(memsB) != 0 {
		t.Errorf("orgB read %d memories on orgA entity — org_id filter leaked", len(memsB))
	}

	// Admin-pool variant must also honor the data-column filter even
	// though it bypasses RLS — the impl binds org_id in every WHERE
	// clause as defense in depth. teamID is orgB's team; the org_id
	// filter excludes orgA's row before the team branch matters.
	memsBSystem, err := stores.TaskMemory.GetMemoriesForEntitySystem(ctx, orgB, entA, firstTeamForOrg(t, h, orgB))
	if err != nil {
		t.Fatalf("GetMemoriesForEntitySystem orgB on orgA entity: %v", err)
	}
	if len(memsBSystem) != 0 {
		t.Errorf("orgB admin-pool read %d memories on orgA entity — data-column filter leaked", len(memsBSystem))
	}

	// A direct (org_id, conversation_id) lookup of orgA's run under orgB returns
	// nothing — the same org_id filter GetMemoriesForEntity/System apply,
	// pinned here at the raw-row level since GetRunMemory (which used to
	// carry this check) is gone.
	if content, ok := queryRunMemoryAgentContent(t, h, orgB, runA); ok {
		t.Errorf("orgB read orgA run memory: %q", content)
	}

	// UpdateRunMemoryHumanContent under orgB on orgA's run is a
	// no-op (no row matches the (org_id, conversation_id) predicate) and is
	// logged-not-fatal — same shape as the missing-row case in the
	// conformance suite.
	if err := stores.TaskMemory.UpdateRunMemoryHumanContent(ctx, orgB, runA, "should-not-land"); err != nil {
		t.Errorf("UpdateRunMemoryHumanContent orgB on orgA run errored: %v", err)
	}
	// Confirm orgA's row is unchanged.
	contentA, ok := queryRunMemoryAgentContent(t, h, orgA, runA)
	if !ok {
		t.Fatalf("orgA conversation_memory row missing")
	}
	if contentA != "orgA narrative" {
		t.Errorf("orgA Content mutated by orgB-scoped UPDATE: got %q, want %q", contentA, "orgA narrative")
	}
	_ = entB
}

// queryRunMemoryAgentContent reads conversation_memory.agent_content directly via
// the admin pool for a (orgID, runID) pair — a raw-row escape hatch for
// tests that need to pin the org_id filter independent of the
// entity-scoped read path (GetMemoriesForEntity), now that GetRunMemory
// (a direct conversation_id lookup) has been removed. Returns ("", false) when no
// row matches.
func queryRunMemoryAgentContent(t *testing.T, h *pgtest.Harness, orgID, runID string) (string, bool) {
	t.Helper()
	var content sql.NullString
	err := h.AdminDB.QueryRow(`SELECT agent_content FROM conversation_memory WHERE org_id = $1 AND conversation_id = $2`, orgID, runID).Scan(&content)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("query conversation_memory: %v", err)
	}
	return content.String, true
}

// TestTaskMemoryStore_Postgres_CrossOrgRLSDenied pins the production
// RLS layer for conversation_memory. Where CrossOrgLeakage above wires both
// pools against AdminDB to prove the defense-in-depth WHERE-clause
// filter is intact, this test runs the store through the app pool
// under tf_app with real JWT claims so the actual conversation_memory_all
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

	// Seed a memory row in orgA via admin so the row exists, plus its
	// primary join row (the run-end attach a sibling ticket wires up —
	// GetMemoriesForEntity's join-based read needs it to find anything).
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	if err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgA, runA, entA, "", "orgA memory"); err != nil {
		t.Fatalf("seed memory in orgA: %v", err)
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, orgA, runA, entA, domain.MemoryRolePrimary); err != nil {
		t.Fatalf("seed join row in orgA: %v", err)
	}

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).TaskMemory.GetMemoriesForEntity(ctx, orgA, entA)
			if err != nil {
				return fmt.Errorf("GetMemoriesForEntity: %w", err)
			}
			if len(got) == 0 {
				t.Errorf("alice GetMemoriesForEntity(orgA, entA) returned nothing; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).TaskMemory.GetMemoriesForEntity(ctx, orgA, entA)
			if err != nil {
				return fmt.Errorf("GetMemoriesForEntity: %w", err)
			}
			if len(got) != 0 {
				t.Errorf("bob GetMemoriesForEntity(orgA, entA) returned %+v; RLS USING filter leaked orgA's memory to orgB", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_denied", func(t *testing.T) {
		// bob's claims point at orgB; UpsertAgentMemory against orgA
		// would land a row with org_id=orgA. conversation_memory_all WITH CHECK
		// requires org_id = tf.current_org_id(), so 42501 is the
		// expected outcome. Seed a second run in orgA with NO existing
		// memory row so the UPSERT exercises the INSERT side of the
		// policy (WITH CHECK) rather than the ON CONFLICT UPDATE side
		// (USING) — runA above already has a seeded memory row from
		// line 141 above, which would route through ON CONFLICT and
		// hit USING instead.
		freshRun, freshEnt := seedPgRunForTaskMemory(t, h, orgA, alice, promptA, "rls-write")
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			return pgstore.NewForTx(tx, pgtest.SecretKey).TaskMemory.UpsertAgentMemory(ctx, orgA, freshRun, freshEnt, "", "cross-org write")
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

// TestTaskMemoryStore_Postgres_BlueprintRunCrossOrgFKRejected pins the
// tenant-isolation guarantee of the composite (blueprint_run_id, org_id) FK:
// a conversation_memory row in orgA cannot reference a blueprint_run that lives in orgB.
// RLS's WITH CHECK only validates org_id, not which blueprint_run the row
// points at, so without the composite FK a confused/hostile write (even one
// that passes RLS) could plant a cross-org reference. The FK applies on the
// admin pool too (it bypasses RLS, not foreign keys), so this is real defense
// in depth.
func TestTaskMemoryStore_Postgres_BlueprintRunCrossOrgFKRejected(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, userA := seedPgTaskMemoryOrg(t, h)
	orgB, userB := seedPgTaskMemoryOrg(t, h)
	promptA := seedPgTaskMemoryPrompt(t, h, orgA, userA)

	runA, entA := seedPgRunForTaskMemory(t, h, orgA, userA, promptA, "fk-A")
	blueprintRunB := seedPgBlueprintRunForTaskMemory(t, h, orgB, userB, "fk-B")

	// orgA memory referencing orgB's blueprint_run → FK violation. The
	// composite (blueprint_run_id, org_id) target (blueprintRunB, orgA) does
	// not exist in blueprint_runs (that row is org_id=orgB).
	if err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgA, runA, entA, blueprintRunB, "cross-org blueprint ref"); err == nil {
		t.Fatalf("expected FK violation writing orgA memory with orgB blueprint_run_id; got nil")
	}

	// Same-org blueprint_run is accepted — the FK only blocks cross-org refs.
	blueprintRunA := seedPgBlueprintRunForTaskMemory(t, h, orgA, userA, "fk-A2")
	if err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgA, runA, entA, blueprintRunA, "same-org blueprint ref"); err != nil {
		t.Fatalf("same-org blueprint_run_id should be accepted: %v", err)
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, orgA, runA, entA, domain.MemoryRolePrimary); err != nil {
		t.Fatalf("seed join row: %v", err)
	}
	mems, err := stores.TaskMemory.GetMemoriesForEntity(ctx, orgA, entA)
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetMemoriesForEntity: mems=%+v err=%v", mems, err)
	}
	if mems[0].BlueprintRunID != blueprintRunA {
		t.Errorf("BlueprintRunID = %q, want %q", mems[0].BlueprintRunID, blueprintRunA)
	}
}

// TestTaskMemoryStore_Postgres_SystemReadTeamScoped is the TFAC-506 pin:
// the admin-pool (BYPASSRLS) System read must scope an entity's memory to
// the materializing run's owning team, NOT return the whole org's memory
// for that entity. Two teams in ONE org both run on a single SHARED entity
// (entities are org-wide); each team's run authors its own memory. A System
// read scoped to team T1 must see only T1's memory — never T2's — and vice
// versa, matching what the app-pool RLS path (conversation_memory_all → runs_select)
// shows a member of that team.
func TestTaskMemoryStore_Postgres_SystemReadTeamScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgTaskMemoryOrg(t, h)
	promptID := seedPgTaskMemoryPrompt(t, h, orgID, userID)

	team1 := firstTeamForOrg(t, h, orgID)           // the org's default team
	team2 := seedPgDefaultTeam(t, h, orgID, userID) // a second team in the same org

	// One entity, shared by both teams (entities are org-wide).
	entityID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, state)
		VALUES ($1, $2, 'github', $3, 'pr', 'Shared Entity', 'https://example/shared', '{}'::jsonb, now(), 'active')
	`, entityID, orgID, "shared-"+entityID[:8]); err != nil {
		t.Fatalf("seed shared entity: %v", err)
	}

	// Each team owns a run on the shared entity and authors its own memory.
	// Each write is paired with the primary join row a real run's completion
	// will carry once the run-end attach ticket lands — GetMemoriesForEntitySystem's
	// join-based read needs it to find anything.
	run1 := seedPgTeamRunOnEntity(t, h, orgID, userID, promptID, team1, entityID, "t1")
	if err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgID, run1, entityID, "", "team1 narrative"); err != nil {
		t.Fatalf("UpsertAgentMemorySystem team1: %v", err)
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, orgID, run1, entityID, domain.MemoryRolePrimary); err != nil {
		t.Fatalf("RecordEntityTouchSystem team1: %v", err)
	}
	run2 := seedPgTeamRunOnEntity(t, h, orgID, userID, promptID, team2, entityID, "t2")
	if err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgID, run2, entityID, "", "team2 narrative"); err != nil {
		t.Fatalf("UpsertAgentMemorySystem team2: %v", err)
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, orgID, run2, entityID, domain.MemoryRolePrimary); err != nil {
		t.Fatalf("RecordEntityTouchSystem team2: %v", err)
	}

	// A run owned by team1 materializes only team1's memory — team2's is
	// NOT bled in (the bug: pre-fix the System read returned both).
	mems1, err := stores.TaskMemory.GetMemoriesForEntitySystem(ctx, orgID, entityID, team1)
	if err != nil {
		t.Fatalf("GetMemoriesForEntitySystem team1: %v", err)
	}
	if len(mems1) != 1 {
		t.Fatalf("team1 System read returned %d memories, want 1 (team2's must not bleed in): %+v", len(mems1), mems1)
	}
	if mems1[0].Content != "team1 narrative" {
		t.Errorf("team1 System read Content = %q, want %q", mems1[0].Content, "team1 narrative")
	}

	// Symmetric: a run owned by team2 sees only team2's memory.
	mems2, err := stores.TaskMemory.GetMemoriesForEntitySystem(ctx, orgID, entityID, team2)
	if err != nil {
		t.Fatalf("GetMemoriesForEntitySystem team2: %v", err)
	}
	if len(mems2) != 1 {
		t.Fatalf("team2 System read returned %d memories, want 1: %+v", len(mems2), mems2)
	}
	if mems2[0].Content != "team2 narrative" {
		t.Errorf("team2 System read Content = %q, want %q", mems2[0].Content, "team2 narrative")
	}

	// CountMemoriesForEntitySystem must respect the identical team-visibility
	// filter: each team counts only its own memory on the shared entity.
	count1, err := stores.TaskMemory.CountMemoriesForEntitySystem(ctx, orgID, entityID, team1)
	if err != nil {
		t.Fatalf("CountMemoriesForEntitySystem team1: %v", err)
	}
	if count1 != 1 {
		t.Errorf("team1 Count = %d, want 1 (team2's must not bleed in)", count1)
	}
	count2, err := stores.TaskMemory.CountMemoriesForEntitySystem(ctx, orgID, entityID, team2)
	if err != nil {
		t.Fatalf("CountMemoriesForEntitySystem team2: %v", err)
	}
	if count2 != 1 {
		t.Errorf("team2 Count = %d, want 1", count2)
	}
}

// TestTaskMemoryStore_Postgres_BackfillProducesPrimaryJoinRows pins the
// migration's backfill statement directly: a conversation_memory row inserted with
// NO corresponding conversation_memory_entities row (simulating a pre-migration
// row) gets exactly one 'primary' join row once the backfill INSERT runs,
// and the entity-scoped read then returns it — the read-path switch's
// result-identical claim for pre-existing data.
func TestTaskMemoryStore_Postgres_BackfillProducesPrimaryJoinRows(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgTaskMemoryOrg(t, h)
	promptID := seedPgTaskMemoryPrompt(t, h, orgID, userID)
	runID, entityID := seedPgRunForTaskMemory(t, h, orgID, userID, promptID, "backfill")

	if _, err := h.AdminDB.Exec(`
		INSERT INTO conversation_memory (id, org_id, conversation_id, entity_id, agent_content, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3, 'pre-migration note', now())
	`, orgID, runID, entityID); err != nil {
		t.Fatalf("seed pre-migration conversation_memory row: %v", err)
	}

	// No join row exists yet — the read path finds nothing.
	if mems, err := stores.TaskMemory.GetMemoriesForEntity(ctx, orgID, entityID); err != nil {
		t.Fatalf("GetMemoriesForEntity (pre-backfill): %v", err)
	} else if len(mems) != 0 {
		t.Fatalf("GetMemoriesForEntity (pre-backfill) = %+v, want empty", mems)
	}

	// The exact backfill statement from the baseline migration.
	if _, err := h.AdminDB.Exec(`
		INSERT INTO conversation_memory_entities (org_id, conversation_id, entity_id, role, created_at)
		SELECT org_id, conversation_id, entity_id, 'primary', created_at FROM conversation_memory
		WHERE org_id = $1
		ON CONFLICT (conversation_id, entity_id) DO NOTHING
	`, orgID); err != nil {
		t.Fatalf("run backfill: %v", err)
	}

	if role := roleForPgJoinRow(t, h, runID, entityID); role != domain.MemoryRolePrimary {
		t.Errorf("backfilled role = %q, want %q", role, domain.MemoryRolePrimary)
	}

	mems, err := stores.TaskMemory.GetMemoriesForEntity(ctx, orgID, entityID)
	if err != nil {
		t.Fatalf("GetMemoriesForEntity (post-backfill): %v", err)
	}
	if len(mems) != 1 || mems[0].Content != "pre-migration note" {
		t.Errorf("GetMemoriesForEntity (post-backfill) = %+v, want the pre-migration row", mems)
	}
}

// TestTaskMemoryStore_Postgres_MultiEntityAttachTeamScoped is the run-end
// attach acceptance on Postgres, where the team-scoped read matters: a run's
// memory, once attached to every entity it engaged (primary ∪ produced ∪
// touched), is reachable through the join from all three roles — and that
// reachability stays team-scoped. Two teams in ONE org each run a completion
// that PRODUCES the SAME shared entity; a team-scoped System read of that
// produced entity must surface only the reading team's memory, proving the
// join-based read applies runs_select team visibility regardless of the join
// role (not just the denormalized primary the SystemReadTeamScoped pin covers).
func TestTaskMemoryStore_Postgres_MultiEntityAttachTeamScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID := seedPgTaskMemoryOrg(t, h)
	promptID := seedPgTaskMemoryPrompt(t, h, orgID, userID)
	team1 := firstTeamForOrg(t, h, orgID)
	team2 := seedPgDefaultTeam(t, h, orgID, userID)

	// Shared entities both teams' runs engage (entities are org-wide): a
	// produced GitHub PR and a touched Jira issue, each with its real source.
	entB := seedPgSharedEntity(t, h, orgID, "github", "octo/repo#4242", "pr")
	entC := seedPgSharedEntity(t, h, orgID, "jira", "SKY-9", "issue")

	// Each team's run has its own primary (task) entity — a Slack thread, the
	// original motivating grain (source 'slack', kind 'thread'); both produce entB.
	entA1 := seedPgSharedEntity(t, h, orgID, "slack", domain.SlackSourceID("C0125", "1700000000.000100"), "thread")
	entA2 := seedPgSharedEntity(t, h, orgID, "slack", domain.SlackSourceID("C0999", "1700000000.000200"), "thread")
	run1 := seedPgTeamRunOnEntity(t, h, orgID, userID, promptID, team1, entA1, "attach-t1")
	run2 := seedPgTeamRunOnEntity(t, h, orgID, userID, promptID, team2, entA2, "attach-t2")

	// The attach sequence run-end runs for each: memory upsert, then primary +
	// produced (+ touched for run1) join rows.
	if err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgID, run1, entA1, "", "team1 narrative"); err != nil {
		t.Fatalf("upsert memory run1: %v", err)
	}
	for entID, role := range map[string]string{
		entA1: domain.MemoryRolePrimary, entB: domain.MemoryRoleProduced, entC: domain.MemoryRoleTouched,
	} {
		if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, orgID, run1, entID, role); err != nil {
			t.Fatalf("attach run1 %s: %v", role, err)
		}
	}
	if err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgID, run2, entA2, "", "team2 narrative"); err != nil {
		t.Fatalf("upsert memory run2: %v", err)
	}
	for entID, role := range map[string]string{
		entA2: domain.MemoryRolePrimary, entB: domain.MemoryRoleProduced,
	} {
		if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, orgID, run2, entID, role); err != nil {
			t.Fatalf("attach run2 %s: %v", role, err)
		}
	}

	// team1's memory is reachable from all three entities it engaged.
	for name, id := range map[string]string{"primary A1": entA1, "produced B": entB, "touched C": entC} {
		mems, err := stores.TaskMemory.GetMemoriesForEntitySystem(ctx, orgID, id, team1)
		if err != nil {
			t.Fatalf("GetMemoriesForEntitySystem team1 %s: %v", name, err)
		}
		if len(mems) != 1 || mems[0].RunID != run1 || mems[0].Content != "team1 narrative" {
			t.Errorf("team1 read of %s = %+v, want only run1's team1 narrative", name, mems)
		}
	}

	// The produced entity is shared, but the team-scoped read still separates
	// the two teams' memory — team scoping is role-agnostic across the join.
	memsB2, err := stores.TaskMemory.GetMemoriesForEntitySystem(ctx, orgID, entB, team2)
	if err != nil {
		t.Fatalf("GetMemoriesForEntitySystem team2 produced B: %v", err)
	}
	if len(memsB2) != 1 || memsB2[0].RunID != run2 || memsB2[0].Content != "team2 narrative" {
		t.Errorf("team2 read of produced B = %+v, want only run2's team2 narrative (no team1 bleed)", memsB2)
	}

	// The recorded roles are what the attach wrote.
	if role := roleForPgJoinRow(t, h, run1, entB); role != domain.MemoryRoleProduced {
		t.Errorf("run1/B role = %q, want produced", role)
	}
	if role := roleForPgJoinRow(t, h, run1, entC); role != domain.MemoryRoleTouched {
		t.Errorf("run1/C role = %q, want touched", role)
	}
	if role := roleForPgJoinRow(t, h, run1, entA1); role != domain.MemoryRolePrimary {
		t.Errorf("run1/A1 role = %q, want primary", role)
	}
}

// seedPgSharedEntity inserts a snapshot-carrying org-wide entity with an
// explicit (source, source_id, kind) — so a fixture that needs several entities
// more than one run engages uses each provider's real source/kind pair (a
// Slack thread is source 'slack', a Jira issue 'jira', a GitHub PR 'github')
// rather than pinning everything to one source. Returns its id.
func seedPgSharedEntity(t *testing.T, h *pgtest.Harness, orgID, source, sourceID, kind string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, state)
		VALUES ($1, $2, $3, $4, $5, $6, 'https://example/x', '{}'::jsonb, now(), 'active')
	`, id, orgID, source, sourceID, kind, sourceID); err != nil {
		t.Fatalf("seed shared entity %s/%s: %v", source, sourceID, err)
	}
	return id
}

// seedPgTeamRunOnEntity seeds the event + task + blueprint + blueprint_run +
// run FK chain for a run owned by teamID on a PRE-EXISTING entity, returning
// the runID. Unlike seedPgRunForTaskMemory (which mints a fresh entity and
// defaults to the org's first team), this takes the entity + team explicitly
// so two teams can own runs on the same shared entity. suffix discriminates
// the task's dedup_key so the active-task partial unique index never collides.
func seedPgTeamRunOnEntity(t *testing.T, h *pgtest.Harness, orgID, userID, promptID, teamID, entityID, suffix string) string {
	t.Helper()
	conn := h.AdminDB
	now := time.Now().UTC()

	eventID := uuid.New().String()
	const eventType = "github:pr:opened"
	if _, err := conn.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, $4, '', '{}'::jsonb, $5)
	`, eventID, orgID, entityID, eventType, now); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	taskID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id,
		                   status, scoring_status, priority_score, created_at)
		VALUES ($1, $2, $3, $4, 'team', $5, $6, $7, $8, 'done', 'pending', 0.5, $9)
	`, taskID, orgID, userID, teamID, entityID, eventType, suffix, eventID, now); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	bpID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprints (id, org_id, creator_user_id, team_id, source, name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'user', 'BP', now(), now())
	`, bpID, orgID, userID, teamID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	brID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, started_at, step_plan)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'running', '/tmp/wt', now(), '[]')
	`, brID, orgID, userID, bpID, taskID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}

	runID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility, task_id, prompt_id, trigger_type, status, blueprint_run_id)
		VALUES ($1, $2, $3, $4, 'team', $5, $6, 'manual', 'completed', $7)
	`, runID, orgID, userID, teamID, taskID, promptID, brID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}

// seedPgTaskMemoryOrg builds the auth user + public user + org +
// org_membership + default team graph the conversation_memory FK chain
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
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'Task Memory Test', 'body', 'user', '', now(), now())
	`, promptID, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return promptID
}

// seedPgRunForTaskMemory seeds the entity + event + task + run FK
// chain conversation_memory needs. Returns (runID, entityID).
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
	// 'done' is the terminal task status. This seed used to write
	// 'completed' — never a valid task value (completed is run-level)
	// but silently accepted because the column had no CHECK. The
	// CHECK constraint surfaced the latent fixture bug; same fix as
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

	// runs.blueprint_run_id is NOT NULL — mint a 1-step blueprint_run for the task.
	bpID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprints (id, org_id, creator_user_id, team_id, source, name, created_at, updated_at)
		VALUES ($1, $2, $3, (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1), 'user', 'BP', now(), now())
	`, bpID, orgID, userID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	brID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, started_at, step_plan)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'running', '/tmp/wt', now(), '[]')
	`, brID, orgID, userID, bpID, taskID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}

	runID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility, task_id, prompt_id, trigger_type, status, blueprint_run_id)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'team', $4, $5, 'manual', 'completed', $6)
	`, runID, orgID, userID, taskID, promptID, brID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID, entityID
}

// seedPgBlueprintRunForTaskMemory seeds the entity + event + task + blueprint +
// blueprint_run FK chain a conversation_memory.blueprint_run_id can point at (the column
// FKs blueprint_runs(id) ON DELETE SET NULL). Returns the blueprint_run id.
func seedPgBlueprintRunForTaskMemory(t *testing.T, h *pgtest.Harness, orgID, userID, suffix string) string {
	t.Helper()
	conn := h.AdminDB
	now := time.Now().UTC()
	teamID := firstTeamForOrg(t, h, orgID)

	entityID := uuid.New().String()
	sourceID := fmt.Sprintf("bp-run-%s-%s", suffix, entityID[:8])
	if _, err := conn.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, state)
		VALUES ($1, $2, 'github', $3, 'pr', 'Blueprint Run Conformance', 'https://example/bp', '{}'::jsonb, $4, 'active')
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
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id,
		                   status, scoring_status, priority_score, created_at)
		VALUES ($1, $2, $3, $4, 'team', $5, $6, '', $7, 'done', 'pending', 0.5, $8)
	`, taskID, orgID, userID, teamID, entityID, eventType, eventID, now); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	blueprintID := "bp_" + uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'BP', 'user', now(), now())
	`, blueprintID, orgID, userID, teamID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}

	blueprintRunID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, worktree_path, step_plan)
		VALUES ($1, $2, $3, $4, $5, 'manual', $6, '[]')
	`, blueprintRunID, orgID, userID, blueprintID, taskID, "/tmp/wt-"+blueprintRunID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return blueprintRunID
}
