package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestBlueprintStore_Postgres_Conformance runs the shared blueprint
// seed-idempotency + step round-trip suite against the Postgres impl. Both
// pools wire to AdminDB (BYPASSRLS) so Seed (admin, no JWT) and reads work
// without per-subtest plumbing — same shape the other Postgres conformance
// tests use.
func TestBlueprintStore_Postgres_Conformance(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunBlueprintStoreConformance(t, func(t *testing.T) (db.BlueprintStore, string, string, dbtest.PromptSeederForBlueprints) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgOrgForBlueprints(t, h)
		seedPgDefaultTeam(t, h, orgID, userID)
		teamID := firstTeamForOrg(t, h, orgID)
		stores := pgstore.New(h.AdminDB, h.AdminDB)
		seedPrompt := func(t *testing.T, idHint string) string {
			t.Helper()
			id := idHint + "-" + orgID[:8]
			seedPgPrompt(t, h, orgID, userID, id)
			return id
		}
		return stores.Blueprints, orgID, teamID, seedPrompt
	})
}

// TestBlueprintStore_Postgres_ReplaceAndListSteps pins the dialect-aware
// SQL: ReplaceSteps must INSERT the org_id column (Postgres-only),
// ListSteps must filter on org_id, and CountStepReferences must count
// distinct blueprints within the org. The regression this guards
// against is the original chains.go free-function shape that wrote
// ? placeholders against the Postgres tables with no org_id — every
// INSERT would 23502 (NULL constraint) and every WHERE would either
// 42703 (no such column) or leak across tenants.
func TestBlueprintStore_Postgres_ReplaceAndListSteps(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, userID := seedPgOrgForBlueprints(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	blueprints := stores.Blueprints
	ctx := context.Background()

	blueprintID := "blueprint-" + orgID[:8]
	stepAID := "step-a-" + orgID[:8]
	stepBID := "step-b-" + orgID[:8]
	seedPgBlueprint(t, h, orgID, userID, blueprintID)
	seedPgPrompt(t, h, orgID, userID, stepAID)
	seedPgPrompt(t, h, orgID, userID, stepBID)

	if err := blueprints.ReplaceSteps(ctx, orgID, blueprintID,
		[]string{stepAID, stepBID}, []string{"brief A", "brief B"}); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}

	steps, err := blueprints.ListSteps(ctx, orgID, blueprintID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("len(steps)=%d, want 2", len(steps))
	}
	if steps[0].StepIndex != 0 || steps[0].StepPromptID != stepAID || steps[0].Brief != "brief A" {
		t.Errorf("step 0 = %+v, want index=0 prompt=%s brief='brief A'", steps[0], stepAID)
	}
	if steps[1].StepIndex != 1 || steps[1].StepPromptID != stepBID || steps[1].Brief != "brief B" {
		t.Errorf("step 1 = %+v, want index=1 prompt=%s brief='brief B'", steps[1], stepBID)
	}

	// CountStepReferences should see one blueprint referencing stepAID.
	n, err := blueprints.CountStepReferences(ctx, orgID, stepAID)
	if err != nil {
		t.Fatalf("CountStepReferences: %v", err)
	}
	if n != 1 {
		t.Errorf("CountStepReferences = %d, want 1", n)
	}

	// Re-ReplaceSteps with a smaller list — the DELETE+INSERT path
	// inside one tx must collapse to the new list, not append.
	if err := blueprints.ReplaceSteps(ctx, orgID, blueprintID, []string{stepBID}, nil); err != nil {
		t.Fatalf("ReplaceSteps (shrink): %v", err)
	}
	steps2, err := blueprints.ListSteps(ctx, orgID, blueprintID)
	if err != nil {
		t.Fatalf("ListSteps after shrink: %v", err)
	}
	if len(steps2) != 1 || steps2[0].StepPromptID != stepBID || steps2[0].Brief != "" {
		t.Errorf("after shrink: %+v, want one step with prompt=%s and empty brief", steps2, stepBID)
	}
}

// TestBlueprintStore_Postgres_RunLifecycle exercises CreateRun → GetRun →
// MarkRunStatus → GetLatestVerdict on a real Postgres tx. Covers the
// UUID/TEXT column split (blueprint_runs.id UUID, blueprint_id TEXT)
// and the now()-based verdict timestamp ordering.
func TestBlueprintStore_Postgres_RunLifecycle(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, userID := seedPgOrgForBlueprints(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	blueprints := stores.Blueprints
	ctx := context.Background()

	blueprintID := "blueprint-rl-" + orgID[:8]
	stepPromptID := "step-rl-" + orgID[:8]
	seedPgBlueprint(t, h, orgID, userID, blueprintID)
	seedPgPrompt(t, h, orgID, userID, stepPromptID)
	taskID := seedPgTask(t, h, orgID, userID)

	blueprintRunID, err := blueprints.CreateRun(ctx, orgID, domain.BlueprintRun{
		BlueprintID:  blueprintID,
		TaskID:       taskID,
		TriggerType:  domain.BlueprintTriggerManual,
		WorktreePath: "/tmp/wt-pg-blueprint",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := uuid.Parse(blueprintRunID); err != nil {
		t.Errorf("CreateRun returned non-UUID id %q", blueprintRunID)
	}

	cr, err := blueprints.GetRun(ctx, orgID, blueprintRunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if cr == nil {
		t.Fatal("GetRun returned nil for a row we just inserted")
	}
	if cr.Status != domain.BlueprintRunStatusRunning {
		t.Errorf("status = %q, want running", cr.Status)
	}

	// Seed a step run and write two verdicts; latest wins.
	stepRunID := seedPgRun(t, h, orgID, userID, taskID, stepPromptID, blueprintRunID, 0)
	advanceJSON, _ := json.Marshal(domain.ChainVerdict{Outcome: domain.ChainVerdictAdvance, Reason: "ok"})
	finalJSON, _ := json.Marshal(domain.ChainVerdict{Outcome: domain.ChainVerdictFinal, Reason: "done"})
	if err := blueprints.InsertVerdict(ctx, orgID, stepRunID, string(advanceJSON)); err != nil {
		t.Fatalf("InsertVerdict advance: %v", err)
	}
	if err := blueprints.InsertVerdict(ctx, orgID, stepRunID, string(finalJSON)); err != nil {
		t.Fatalf("InsertVerdict final: %v", err)
	}

	latest, err := blueprints.GetLatestVerdict(ctx, orgID, stepRunID)
	if err != nil {
		t.Fatalf("GetLatestVerdict: %v", err)
	}
	if latest == nil || latest.Outcome != domain.ChainVerdictFinal {
		t.Errorf("latest = %+v, want final", latest)
	}

	mapped, err := blueprints.LatestVerdictsForRuns(ctx, orgID, []string{stepRunID})
	if err != nil {
		t.Fatalf("LatestVerdictsForRuns: %v", err)
	}
	if v := mapped[stepRunID]; v == nil || v.Outcome != domain.ChainVerdictFinal {
		t.Errorf("mapped[stepRunID] = %+v, want final", v)
	}

	// Mark the blueprint completed; second attempt should be no-op.
	changed, err := blueprints.MarkRunStatus(ctx, orgID, blueprintRunID, domain.BlueprintRunStatusCompleted, "", nil)
	if err != nil {
		t.Fatalf("MarkRunStatus: %v", err)
	}
	if !changed {
		t.Error("expected changed=true for running → completed")
	}
	changed2, err := blueprints.MarkRunStatus(ctx, orgID, blueprintRunID, domain.BlueprintRunStatusAborted, "late", nil)
	if err != nil {
		t.Fatalf("MarkRunStatus second: %v", err)
	}
	if changed2 {
		t.Error("expected changed=false on terminal row (race guard)")
	}

	// GetRunForRun resolves step → blueprint.
	cr2, idx, err := blueprints.GetRunForRun(ctx, orgID, stepRunID)
	if err != nil {
		t.Fatalf("GetRunForRun: %v", err)
	}
	if cr2 == nil || cr2.ID != blueprintRunID {
		t.Errorf("GetRunForRun blueprint = %+v, want id=%s", cr2, blueprintRunID)
	}
	if idx == nil || *idx != 0 {
		t.Errorf("GetRunForRun stepIdx = %v, want 0", idx)
	}

	// GetRunForRunSystem mirrors GetRunForRun for goroutine-internal
	// callers (blueprint orchestrator cleanup, post-yield resume) that
	// have no JWT-claims context. The contract is identical — both
	// arms read the same row — so the assertion is just that the
	// admin-pool variant returns the same values.
	cr3, idx3, err := blueprints.GetRunForRunSystem(ctx, orgID, stepRunID)
	if err != nil {
		t.Fatalf("GetRunForRunSystem: %v", err)
	}
	if cr3 == nil || cr3.ID != blueprintRunID {
		t.Errorf("GetRunForRunSystem blueprint = %+v, want id=%s", cr3, blueprintRunID)
	}
	if idx3 == nil || *idx3 != 0 {
		t.Errorf("GetRunForRunSystem stepIdx = %v, want 0", idx3)
	}
}

// TestBlueprintStore_Postgres_CreateRun_UnderAppPoolRLS pins the
// internal trigger_type routing in CreateRun against actual RLS,
// not the AdminDB-bypassed conformance setup:
//
//  1. trigger_type='event' routes to the admin pool and lands with
//     creator_user_id NULL. The blueprint_runs_creator_matches_trigger_type
//     CHECK requires NULL for event rows; the blueprint_runs_modify RLS
//     policy on the app pool requires creator_user_id =
//     tf.current_user_id(), which is mutually exclusive. Admin
//     (BYPASSRLS) is the only path that can satisfy the CHECK.
//
//  2. trigger_type='manual' routes to the app pool and the COALESCE
//     pulls tf.current_user_id() (set via WithTx) into the row, so
//     the manual blueprint run reads back with the JWT-claimed user as
//     creator_user_id.
//
// Mirrors TestAgentRunStore_Postgres_Create_UnderAppPoolRLS — same
// fix-against-actual-RLS shape.
func TestBlueprintStore_Postgres_CreateRun_UnderAppPoolRLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, userID := seedPgOrgForBlueprints(t, h)
	seedPgDefaultTeam(t, h, orgID, userID)
	blueprintID := "blueprint-rls-" + orgID[:8]
	seedPgBlueprint(t, h, orgID, userID, blueprintID)
	taskID := seedPgTask(t, h, orgID, userID)

	// Wire BlueprintStore against the real admin pool (BYPASSRLS) for the
	// event-triggered insert and the real app pool (RLS-active under
	// tf_app via WithTx) for the manual insert.
	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	// ---- Event-triggered CreateRun ----
	// No JWT claims tx — the admin pool handles the insert directly.
	eventRunID, err := stores.Blueprints.CreateRun(ctx, orgID, domain.BlueprintRun{
		BlueprintID:  blueprintID,
		TaskID:       taskID,
		TriggerType:  domain.BlueprintTriggerEvent,
		WorktreePath: "/tmp/wt-blueprint-event",
	})
	if err != nil {
		t.Fatalf("event-triggered CreateRun under app-pool wiring: %v", err)
	}
	var landedTrigger string
	var landedCreator sql.NullString
	if err := h.AdminDB.QueryRow(
		`SELECT trigger_type, creator_user_id::text FROM blueprint_runs WHERE id = $1`,
		eventRunID,
	).Scan(&landedTrigger, &landedCreator); err != nil {
		t.Fatalf("read back event blueprint_run: %v", err)
	}
	if landedTrigger != string(domain.BlueprintTriggerEvent) {
		t.Errorf("trigger_type = %q, want event", landedTrigger)
	}
	if landedCreator.Valid {
		t.Errorf("creator_user_id = %q, want NULL (event-trigger CHECK)", landedCreator.String)
	}

	// ---- Manual CreateRun ----
	// Inside WithTx so JWT claims are set; the COALESCE in
	// createRunManual resolves tf.current_user_id() to userID.
	var manualRunID string
	if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		id, err := tx.Blueprints.CreateRun(ctx, orgID, domain.BlueprintRun{
			BlueprintID:  blueprintID,
			TaskID:       taskID,
			TriggerType:  domain.BlueprintTriggerManual,
			WorktreePath: "/tmp/wt-blueprint-manual",
		})
		if err != nil {
			return err
		}
		manualRunID = id
		return nil
	}); err != nil {
		t.Fatalf("manual CreateRun under app-pool: %v", err)
	}
	var manualCreator sql.NullString
	if err := h.AdminDB.QueryRow(
		`SELECT creator_user_id::text FROM blueprint_runs WHERE id = $1`,
		manualRunID,
	).Scan(&manualCreator); err != nil {
		t.Fatalf("read back manual blueprint_run: %v", err)
	}
	if !manualCreator.Valid {
		t.Fatalf("manual creator_user_id is NULL; want %s (resolved from JWT claims)", userID)
	}
	if manualCreator.String != userID {
		t.Errorf("manual creator_user_id = %q, want %q (JWT-claimed user)",
			manualCreator.String, userID)
	}

	// The blueprint_runs_select RLS policy was widened so event-triggered
	// rows (creator_user_id NULL) resolve via plain org membership
	// rather than the creator-equals-caller predicate. Without that,
	// the request-facing GetRun / GetRunForRun / CancelBlueprint paths
	// would silently 404 on every auto-fired blueprint because the
	// app-pool SELECT can't match a NULL creator. Verify a WithTx
	// read of the event-triggered row succeeds.
	var sawEventRun bool
	if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		cr, err := tx.Blueprints.GetRun(ctx, orgID, eventRunID)
		if err != nil {
			return err
		}
		if cr != nil {
			sawEventRun = true
		}
		return nil
	}); err != nil {
		t.Fatalf("WithTx GetRun on event blueprint: %v", err)
	}
	if !sawEventRun {
		t.Error("WithTx GetRun on event-triggered blueprint returned nil; RLS policy still excludes NULL-creator rows")
	}
}

// TestBlueprintStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// org_id filter on every admin-pool variant: even with RLS bypassed,
// a System read for org A must never return rows that live in org B.
// Mirrors the AgentRunStore cross-org leakage suite.
func TestBlueprintStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, userA := seedPgOrgForBlueprints(t, h)
	orgB, userB := seedPgOrgForBlueprints(t, h)
	seedPgDefaultTeam(t, h, orgA, userA)
	seedPgDefaultTeam(t, h, orgB, userB)

	blueprintIDA := "blueprint-leak-a-" + orgA[:8]
	blueprintIDB := "blueprint-leak-b-" + orgB[:8]
	stepIDA := "step-leak-a-" + orgA[:8]
	stepIDB := "step-leak-b-" + orgB[:8]
	seedPgBlueprint(t, h, orgA, userA, blueprintIDA)
	seedPgPrompt(t, h, orgA, userA, stepIDA)
	seedPgBlueprint(t, h, orgB, userB, blueprintIDB)
	seedPgPrompt(t, h, orgB, userB, stepIDB)
	taskA := seedPgTask(t, h, orgA, userA)
	taskB := seedPgTask(t, h, orgB, userB)

	stores := pgstore.New(h.AdminDB, h.AdminDB)
	blueprints := stores.Blueprints
	ctx := context.Background()

	if err := blueprints.ReplaceSteps(ctx, orgA, blueprintIDA, []string{stepIDA}, nil); err != nil {
		t.Fatalf("replace A: %v", err)
	}
	if err := blueprints.ReplaceSteps(ctx, orgB, blueprintIDB, []string{stepIDB}, nil); err != nil {
		t.Fatalf("replace B: %v", err)
	}

	crA, err := blueprints.CreateRun(ctx, orgA, domain.BlueprintRun{
		BlueprintID: blueprintIDA, TaskID: taskA,
		TriggerType: domain.BlueprintTriggerManual, WorktreePath: "/tmp/leak-a",
	})
	if err != nil {
		t.Fatalf("CreateRun A: %v", err)
	}
	crB, err := blueprints.CreateRun(ctx, orgB, domain.BlueprintRun{
		BlueprintID: blueprintIDB, TaskID: taskB,
		TriggerType: domain.BlueprintTriggerManual, WorktreePath: "/tmp/leak-b",
	})
	if err != nil {
		t.Fatalf("CreateRun B: %v", err)
	}
	stepRunA := seedPgRun(t, h, orgA, userA, taskA, stepIDA, crA, 0)
	stepRunB := seedPgRun(t, h, orgB, userB, taskB, stepIDB, crB, 0)

	verdictA, _ := json.Marshal(domain.ChainVerdict{Outcome: domain.ChainVerdictFinal, Reason: "A"})
	verdictB, _ := json.Marshal(domain.ChainVerdict{Outcome: domain.ChainVerdictFinal, Reason: "B"})
	if err := blueprints.InsertVerdictSystem(ctx, orgA, stepRunA, string(verdictA)); err != nil {
		t.Fatalf("InsertVerdictSystem A: %v", err)
	}
	if err := blueprints.InsertVerdictSystem(ctx, orgB, stepRunB, string(verdictB)); err != nil {
		t.Fatalf("InsertVerdictSystem B: %v", err)
	}

	// ListStepsSystem on org A must not see blueprint B's step.
	stepsA, err := blueprints.ListStepsSystem(ctx, orgA, blueprintIDB)
	if err != nil {
		t.Fatalf("ListStepsSystem A→blueprint B: %v", err)
	}
	if len(stepsA) != 0 {
		t.Errorf("ListStepsSystem(orgA, blueprintIDB) leaked %d rows, want 0", len(stepsA))
	}

	// GetLatestVerdictSystem on org A reading org B's step run must
	// return (nil, nil) — the run_id is unique across orgs but the
	// admin pool bypasses RLS, so the org_id WHERE clause on
	// run_artifacts is the only thing standing between the system
	// variant and a cross-tenant verdict leak.
	verdictCrossOrg, err := blueprints.GetLatestVerdictSystem(ctx, orgA, stepRunB)
	if err != nil {
		t.Fatalf("GetLatestVerdictSystem cross-org: %v", err)
	}
	if verdictCrossOrg != nil {
		t.Errorf("GetLatestVerdictSystem(orgA, stepRunB) returned %+v, want nil (cross-org leak)", verdictCrossOrg)
	}
	// Same-org read still returns the verdict.
	verdictSameOrg, err := blueprints.GetLatestVerdictSystem(ctx, orgB, stepRunB)
	if err != nil {
		t.Fatalf("GetLatestVerdictSystem same-org: %v", err)
	}
	if verdictSameOrg == nil || verdictSameOrg.Reason != "B" {
		t.Errorf("GetLatestVerdictSystem(orgB, stepRunB) = %+v, want reason=B", verdictSameOrg)
	}

	// RunsForBlueprintSystem on org A must not return cross-org step runs.
	runsCrossOrg, err := blueprints.RunsForBlueprintSystem(ctx, orgA, crB)
	if err != nil {
		t.Fatalf("RunsForBlueprintSystem cross-org: %v", err)
	}
	if len(runsCrossOrg) != 0 {
		t.Errorf("RunsForBlueprintSystem(orgA, crB) leaked %d rows, want 0", len(runsCrossOrg))
	}

	// MarkRunStatusSystem on org A against org B's blueprint run is a no-op.
	changed, err := blueprints.MarkRunStatusSystem(ctx, orgA, crB, domain.BlueprintRunStatusAborted, "leak", nil)
	if err != nil {
		t.Fatalf("MarkRunStatusSystem cross-org: %v", err)
	}
	if changed {
		t.Error("MarkRunStatusSystem(orgA, crB) returned changed=true; cross-org write leaked")
	}
	// blueprint B should still be running.
	crBRead, err := blueprints.GetRun(ctx, orgB, crB)
	if err != nil {
		t.Fatalf("GetRun B: %v", err)
	}
	if crBRead == nil || crBRead.Status != domain.BlueprintRunStatusRunning {
		t.Errorf("blueprint B status = %v, want running (cross-org write should not have flipped it)", crBRead)
	}
}

// TestBlueprintStore_Postgres_CrossOrgRLSDenied pins the production RLS
// layer for blueprint_runs. Where CrossOrgLeakage above proves the
// defense-in-depth WHERE-clause filter is intact, this test runs the
// store through the app pool under tf_app with real JWT claims so the
// actual blueprint_runs_select / blueprint_runs_modify policies are exercised.
// Same-org reads succeed; cross-org reads are silently filtered (USING);
// cross-org manual CreateRun raises 42501 from blueprint_runs_modify
// WITH CHECK.
func TestBlueprintStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice := seedPgOrgForBlueprints(t, h)
	orgB, bob := seedPgOrgForBlueprints(t, h)
	seedPgDefaultTeam(t, h, orgA, alice)
	seedPgDefaultTeam(t, h, orgB, bob)

	blueprintIDA := "blueprint-rls-a-" + orgA[:8]
	stepIDA := "step-rls-a-" + orgA[:8]
	seedPgBlueprint(t, h, orgA, alice, blueprintIDA)
	seedPgPrompt(t, h, orgA, alice, stepIDA)
	taskA := seedPgTask(t, h, orgA, alice)

	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	// Seed a blueprint run in orgA via WithTx (manual path lands in
	// blueprint_runs with creator_user_id=alice). Whether bob can see or
	// mutate it is the question.
	var blueprintRunA string
	if err := stores.Tx.WithTx(ctx, orgA, alice, func(tx db.TxStores) error {
		id, err := tx.Blueprints.CreateRun(ctx, orgA, domain.BlueprintRun{
			BlueprintID: blueprintIDA, TaskID: taskA,
			TriggerType: domain.BlueprintTriggerManual, WorktreePath: "/tmp/rls-a",
		})
		if err != nil {
			return err
		}
		blueprintRunA = id
		return nil
	}); err != nil {
		t.Fatalf("seed blueprint run A: %v", err)
	}

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			cr, err := pgstore.NewForTx(tx).Blueprints.GetRun(ctx, orgA, blueprintRunA)
			if err != nil {
				return fmt.Errorf("GetRun: %w", err)
			}
			if cr == nil {
				t.Errorf("alice GetRun(orgA, blueprintRunA) returned nil; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			cr, err := pgstore.NewForTx(tx).Blueprints.GetRun(ctx, orgA, blueprintRunA)
			if err != nil {
				return fmt.Errorf("GetRun: %w", err)
			}
			if cr != nil {
				t.Errorf("bob GetRun(orgA, blueprintRunA) returned %+v; RLS USING filter leaked orgA's blueprint run to orgB", cr)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_denied", func(t *testing.T) {
		// bob's claims point at orgB; the manual CreateRun would
		// land with org_id=orgA. blueprint_runs_modify WITH CHECK
		// requires the row's org_id to match tf.current_org_id(), so
		// 42501 is the expected outcome.
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			_, e := pgstore.NewForTx(tx).Blueprints.CreateRun(ctx, orgA, domain.BlueprintRun{
				BlueprintID: blueprintIDA, TaskID: taskA,
				TriggerType: domain.BlueprintTriggerManual, WorktreePath: "/tmp/rls-x",
			})
			return e
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

// seedPgOrgForBlueprints creates the (org, user, membership) triplet
// blueprint row writes need to satisfy creator_user_id FK resolution.
// Returns both ids — blueprint tests also need the userID directly for
// seeding tasks / runs whose RLS predicate gates on
// creator_user_id = tf.current_user_id().
func seedPgOrgForBlueprints(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("blueprints-conf-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Blueprints Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Blueprints Conformance Org "+orgID[:8], "blueprints-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	return orgID, userID
}

// seedPgPrompt inserts a prompt row at the admin pool so blueprint step
// FKs resolve. Postgres prompts.id is TEXT (slug-shaped) — same
// surface SQLite uses. All prompts for one org resolve to that org's
// single team via ensurePgTeamForOrg so the same-team blueprint→step
// composite FK ((step_prompt_id, team_id) → prompts(id, team_id),
// SKY-380) holds across a blueprint and its steps.
func seedPgPrompt(t *testing.T, h *pgtest.Harness, orgID, userID, id string) {
	t.Helper()
	teamID := ensurePgTeamForOrg(t, h, orgID, userID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, team_id, creator_user_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3::uuid, $4, $5, 'body', 'user', '[]'::jsonb, now(), now())
	`, id, orgID, teamID, userID, id); err != nil {
		t.Fatalf("seed prompt %s: %v", id, err)
	}
}

// seedPgBlueprint inserts a user-source blueprint row at the admin pool so
// blueprint_steps / blueprint_runs / event_handler-trigger FKs resolve. The
// blueprint shares its org's single team (via ensurePgTeamForOrg) so the
// same-team composite FKs hold across the blueprint, its steps, and any
// trigger that references it.
func seedPgBlueprint(t *testing.T, h *pgtest.Harness, orgID, userID, id string) {
	t.Helper()
	teamID := ensurePgTeamForOrg(t, h, orgID, userID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO blueprints (id, org_id, team_id, creator_user_id, name, source, created_at, updated_at)
		VALUES ($1, $2, $3::uuid, $4, $5, 'user', now(), now())
	`, id, orgID, teamID, userID, id); err != nil {
		t.Fatalf("seed blueprint %s: %v", id, err)
	}
}

// ensurePgTeamForOrg returns the org's first team, creating a default one
// if none exists. Prompts and blueprints are team-scoped (team_id NOT NULL,
// SKY-380) and a blueprint's steps must share its team, so multiple
// seedPgPrompt / seedPgBlueprint calls for one org all resolve to a single
// stable team here.
func ensurePgTeamForOrg(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	var teamID string
	if err := h.AdminDB.QueryRow(
		`SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1`, orgID,
	).Scan(&teamID); err != nil {
		return seedPgDefaultTeam(t, h, orgID, userID)
	}
	return teamID
}

// seedPgTask inserts a minimal task row so blueprint_runs.task_id FK is
// satisfied. Tests don't drive the task lifecycle; the row only needs
// to exist in the right org with the right creator.
func seedPgTask(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	entityID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Blueprints Test Entity', 'https://example/x', '{}'::jsonb, now())
	`, entityID, orgID, "blueprints-test-"+entityID[:8]); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eventID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
	`, eventID, orgID, entityID, domain.EventGitHubPRCICheckFailed); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	teamID := seedPgDefaultTeam(t, h, orgID, userID)
	taskID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, entity_id, event_type, dedup_key, primary_event_id, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, '', $7, 'queued', now())
	`, taskID, orgID, userID, teamID, entityID, domain.EventGitHubPRCICheckFailed, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

// seedPgRun inserts a runs row linked to a blueprint_run for verdict /
// step-to-blueprint resolution tests.
func seedPgRun(t *testing.T, h *pgtest.Harness, orgID, userID, taskID, promptID, blueprintRunID string, stepIdx int) string {
	t.Helper()
	runID := uuid.New().String()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO runs (id, org_id, creator_user_id, team_id, task_id, prompt_id, status, model, started_at, blueprint_run_id, blueprint_step_index)
		VALUES ($1, $2, $3, $4, $5, $6, 'initializing', 'claude-sonnet-4-6', now(), $7, $8)
	`, runID, orgID, userID, teamID, taskID, promptID, blueprintRunID, stepIdx); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}
