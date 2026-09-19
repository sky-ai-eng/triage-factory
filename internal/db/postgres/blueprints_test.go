package postgres_test

import (
	"context"
	"database/sql"
	"errors"
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
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		seedPrompt := func(t *testing.T, idHint string) string {
			t.Helper()
			id := idHint + "-" + orgID[:8]
			seedPgPrompt(t, h, orgID, userID, id)
			return id
		}
		return stores.Blueprints, orgID, teamID, seedPrompt
	})
}

// TestBlueprintStore_Postgres_RunWriteConformance runs the shared returned-row
// suite for the blueprint_runs writes against the Postgres impl.
func TestBlueprintStore_Postgres_RunWriteConformance(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunBlueprintRunWriteConformance(t, func(t *testing.T) (db.BlueprintStore, dbtest.BlueprintSequenceScaffold) {
		return pgSequenceScaffold(t, h)
	})
}

// TestBlueprintStore_Postgres_OneActiveRunPerTaskConformance runs the shared
// one-running-blueprint_run-per-task suite against the Postgres impl, whose
// index widened from the auto-only half it shipped with. Its sibling in
// blueprints_task_race_test.go covers the same index under real concurrency.
func TestBlueprintStore_Postgres_OneActiveRunPerTaskConformance(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunOneActiveRunPerTaskConformance(t, func(t *testing.T) (db.BlueprintStore, dbtest.BlueprintSequenceScaffold) {
		return pgSequenceScaffold(t, h)
	})
}

// TestBlueprintStore_Postgres_IsNewestRunForTask runs the shared newest-run
// suite against the Postgres impl, on the admin pool. Its creator-scoped
// sibling below covers what only a claims-carrying connection can show.
func TestBlueprintStore_Postgres_IsNewestRunForTask(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunIsNewestRunForTaskConformance(t, func(t *testing.T) (db.BlueprintStore, dbtest.BlueprintSequenceScaffold) {
		return pgSequenceScaffold(t, h)
	})
}

// TestBlueprintStore_Postgres_IsNewestRunForTask_ReadsPastTheCreatorScope is
// the reason IsNewestRunForTask is a predicate over a SECURITY DEFINER
// function rather than a plain query for the newest run.
//
// blueprint_runs_select is creator-scoped for manual runs. So when Alice's
// delegation leaves a draft PR, the task is requeued, and BOB re-delegates it,
// Bob's run is invisible to Alice — and the terminal-on-last close, which runs
// on the app pool under the claims of whoever resolved the artifact, would ask
// "is Alice's run still the task's newest?", be told yes by a query that cannot
// see Bob's, and take the task to done under Bob's live engagement.
//
// Only a claims-carrying connection can show this: the conformance suite wires
// both pools to the admin (BYPASSRLS) connection, where every run is visible
// and the bug cannot reproduce.
func TestBlueprintStore_Postgres_IsNewestRunForTask_ReadsPastTheCreatorScope(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, alice := seedPgOrgForBlueprints(t, h)
	teamID := seedPgDefaultTeam(t, h, orgID, alice)
	blueprintID := "bp-creator-scope-" + orgID[:8]
	seedPgBlueprintInTeam(t, h, orgID, alice, teamID, blueprintID)
	taskID := seedPgTask(t, h, orgID, alice)
	bob := seedPgMember(t, h, orgID, "bob", "member")

	// Alice's engagement, finished; then Bob's, live and later. Both MANUAL,
	// which is what puts them on opposite sides of the SELECT policy — and
	// seeded at the admin pool so each run carries its real creator rather than
	// whichever identity the insert ran as.
	aliceRun, bobRun := uuid.New().String(), uuid.New().String()
	for _, r := range []struct {
		id, creator, status string
		startedAt           string
	}{
		{aliceRun, alice, "completed", "now() - interval '1 hour'"},
		{bobRun, bob, "running", "now()"},
	} {
		if _, err := h.AdminDB.Exec(`
			INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, step_plan, worktree_path, started_at)
			VALUES ($1, $2, $3, $4, $5, 'manual', $6, '[]'::jsonb, '', `+r.startedAt+`)
		`, r.id, orgID, r.creator, blueprintID, taskID, r.status); err != nil {
			t.Fatalf("seed blueprint_run %s: %v", r.id, err)
		}
	}

	admin := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).Blueprints
	if err := h.WithUser(t, alice, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).Blueprints

		// The premise. If Alice could read Bob's run the rest of this test
		// would pass on a plain query too, and prove nothing.
		hidden, err := store.GetRun(ctx, orgID, bobRun)
		if err != nil {
			t.Fatalf("GetRun(bob's run) as Alice: %v", err)
		}
		if hidden != nil {
			t.Fatalf("Alice can read Bob's manual run; blueprint_runs_select is no longer creator-scoped and this test needs rewriting")
		}

		// The fix. Whether a run has been superseded is a fact about the task,
		// so the answer may not vary with who is asking — the app pool under
		// Alice's claims must agree with the admin pool on both runs.
		for _, tc := range []struct {
			name, runID string
		}{
			{"alice's superseded run", aliceRun},
			{"bob's superseding run", bobRun},
		} {
			want, err := admin.IsNewestRunForTaskSystem(ctx, orgID, taskID, tc.runID)
			if err != nil {
				t.Fatalf("IsNewestRunForTaskSystem (%s): %v", tc.name, err)
			}
			got, err := store.IsNewestRunForTask(ctx, orgID, taskID, tc.runID)
			if err != nil {
				t.Fatalf("IsNewestRunForTask (%s): %v", tc.name, err)
			}
			if got != want {
				t.Errorf("IsNewestRunForTask(%s) as Alice = %v, admin pool says %v", tc.name, got, want)
			}
		}

		// Spelled out, because this one is the bug: a resolve on the artifact
		// Alice's run left behind must not find her run current.
		superseded, err := store.IsNewestRunForTask(ctx, orgID, taskID, aliceRun)
		if err != nil {
			t.Fatalf("IsNewestRunForTask (alice's run): %v", err)
		}
		if superseded {
			t.Error("Alice's superseded run reports as the task's newest, so resolving its carried artifact would close the task under Bob's live run")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestBlueprintStore_Postgres_DuplicationConformance runs the shared
// DuplicatePrompts deep-copy suite against the Postgres impl. Both pools wire
// to AdminDB; prompts are seeded through the store so the
// system-has-no-creator + slug invariants hold.
func TestBlueprintStore_Postgres_DuplicationConformance(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunBlueprintDuplicationConformance(t, func(t *testing.T) (db.BlueprintStore, string, string, dbtest.DuplicationPromptSeeder, dbtest.DuplicationPromptGetter) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgOrgForBlueprints(t, h)
		seedPgDefaultTeam(t, h, orgID, userID)
		teamID := firstTeamForOrg(t, h, orgID)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		ctx := context.Background()
		seed := func(t *testing.T, p domain.Prompt) string {
			t.Helper()
			if p.Source == "system" {
				return seedPgSystemPrompt(t, h, orgID, teamID, p)
			}
			if p.ID == "" {
				p.ID = uuid.New().String()
			}
			if _, err := stores.Prompts.Create(ctx, orgID, teamID, p); err != nil {
				t.Fatalf("create prompt: %v", err)
			}
			return p.ID
		}
		getPrompt := func(t *testing.T, id string) domain.Prompt {
			t.Helper()
			pr, err := stores.Prompts.GetSystem(ctx, orgID, id)
			if err != nil || pr == nil {
				t.Fatalf("getPrompt %s: (%v, %v)", id, pr, err)
			}
			return *pr
		}
		return stores.Blueprints, orgID, teamID, seed, getPrompt
	})
}

// TestBlueprintStore_Postgres_DuplicatePrompts_CrossTeamRejected pins the
// same-team assertion: a prompt-id set whose blueprints span two teams in the
// org is rejected with ErrDuplicateCrossTeam rather than silently copied.
// Multi-team is only realizable on Postgres (SQLite is N=1).
func TestBlueprintStore_Postgres_DuplicatePrompts_CrossTeamRejected(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, userID := seedPgOrgForBlueprints(t, h)
	teamA := seedPgDefaultTeam(t, h, orgID, userID)
	teamB := seedPgNamedTeam(t, h, orgID, userID, "team-b")
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	// One blueprint + step prompt per team.
	bpA := "dup-xteam-a-" + orgID[:8]
	bpB := "dup-xteam-b-" + orgID[:8]
	pA := "dup-xteam-pa-" + orgID[:8]
	pB := "dup-xteam-pb-" + orgID[:8]
	seedPgBlueprintInTeam(t, h, orgID, userID, teamA, bpA)
	seedPgBlueprintInTeam(t, h, orgID, userID, teamB, bpB)
	seedPgPromptInTeam(t, h, orgID, userID, teamA, pA)
	seedPgPromptInTeam(t, h, orgID, userID, teamB, pB)
	if _, err := stores.Blueprints.ReplaceSteps(ctx, orgID, bpA, []string{pA}, nil); err != nil {
		t.Fatalf("ReplaceSteps A: %v", err)
	}
	if _, err := stores.Blueprints.ReplaceSteps(ctx, orgID, bpB, []string{pB}, nil); err != nil {
		t.Fatalf("ReplaceSteps B: %v", err)
	}

	// Acting team A, but the set mixes in team B's prompt → cross-team reject.
	if _, err := stores.Blueprints.DuplicatePrompts(ctx, orgID, teamA, []string{pA, pB}); !errors.Is(err, db.ErrDuplicateCrossTeam) {
		t.Fatalf("mixed-team duplicate err = %v, want ErrDuplicateCrossTeam", err)
	}
	// Team A's prompt alone duplicates fine.
	newIDs, err := stores.Blueprints.DuplicatePrompts(ctx, orgID, teamA, []string{pA})
	if err != nil {
		t.Fatalf("same-team duplicate: %v", err)
	}
	if len(newIDs) != 1 {
		t.Fatalf("same-team duplicate produced %d blueprints, want 1", len(newIDs))
	}
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
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	blueprints := stores.Blueprints
	ctx := context.Background()

	blueprintID := "blueprint-" + orgID[:8]
	stepAID := "step-a-" + orgID[:8]
	stepBID := "step-b-" + orgID[:8]
	seedPgBlueprint(t, h, orgID, userID, blueprintID)
	seedPgPrompt(t, h, orgID, userID, stepAID)
	seedPgPrompt(t, h, orgID, userID, stepBID)

	if _, err := blueprints.ReplaceSteps(ctx, orgID, blueprintID,
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
	if _, err := blueprints.ReplaceSteps(ctx, orgID, blueprintID, []string{stepBID}, nil); err != nil {
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

// TestBlueprintStore_Postgres_RunLifecycle exercises GetRun →
// ConversationsForBlueprint (surfacing the step conversation's terminal
// conversations.outcome) →
// TestBlueprintStore_Postgres_MarkRunStatus_ParksOrphanedChild pins the
// atomic guarantee: flipping a blueprint_run to a terminal status parks any
// still-mid-flight child conversation in the same transaction (stamping
// parked_at), so a terminal parent is never observed alongside a live
// child. Mirrors the SQLite coverage to prevent Postgres/SQLite divergence.
func TestBlueprintStore_Postgres_MarkRunStatus_ParksOrphanedChild(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, userID := seedPgOrgForBlueprints(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	blueprintID := "blueprint-oc-" + orgID[:8]
	stepPromptID := "step-oc-" + orgID[:8]
	seedPgBlueprint(t, h, orgID, userID, blueprintID)
	seedPgPrompt(t, h, orgID, userID, stepPromptID)
	taskID := seedPgTask(t, h, orgID, userID)

	brID := insertPgBlueprintRun(t, h, orgID, userID, domain.BlueprintRun{
		BlueprintID: blueprintID, TaskID: taskID, WorktreePath: "/tmp/wt-oc",
		StepPlan: []domain.BlueprintPlanStep{{StepIndex: 0, PromptID: stepPromptID, PromptName: "S", PromptBody: "b", Source: "user"}},
	})
	childID := seedPgStepConversation(t, h, orgID, userID, taskID, stepPromptID, brID, 0)
	if _, err := h.AdminDB.Exec(`UPDATE conversations SET status = NULL WHERE id = $1`, childID); err != nil {
		t.Fatalf("set child running: %v", err)
	}
	// The racing dispatcher already claimed the child; the cancel must
	// release the engagement along with the status flip, not leave it for
	// the janitor.
	seedChildClaim := func(convID, executor string) {
		t.Helper()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO claims (org_id, conversation_id, executor_id, boot_epoch)
			VALUES ($1, $2, $3, 1)
		`, orgID, convID, executor); err != nil {
			t.Fatalf("seed claim for %s: %v", convID, err)
		}
	}
	assertClaimReleased := func(convID string) {
		t.Helper()
		var released bool
		var outcome string
		if err := h.AdminDB.QueryRow(`
			SELECT released_at IS NOT NULL, COALESCE(outcome, '')
			FROM claims WHERE conversation_id = $1
		`, convID).Scan(&released, &outcome); err != nil {
			t.Fatalf("read claim for %s: %v", convID, err)
		}
		if !released || outcome != "cancelled" {
			t.Errorf("parked child's claim = (released=%v, outcome=%q), want (true, cancelled)", released, outcome)
		}
	}
	seedChildClaim(childID, "exec-oc")

	changed, err := stores.Blueprints.MarkRunStatusSystem(ctx, orgID, brID, domain.BlueprintRunStatusCancelled, "user_cancelled", nil)
	if err != nil {
		t.Fatalf("MarkRunStatusSystem: %v", err)
	}
	if !changed {
		t.Fatal("MarkRunStatusSystem reported no change")
	}
	if st, parked := pgConversationParked(t, h, childID); st != "open" || !parked {
		t.Errorf("child = (%q, parked=%v), want (open, true) — a terminal parent must not strand a live child", st, parked)
	}
	assertClaimReleased(childID)

	// Same guarantee on the app-pool path, where the claim release lands
	// adjacently on the admin pool instead of inside the cancel statement.
	brID2 := insertPgBlueprintRun(t, h, orgID, userID, domain.BlueprintRun{
		BlueprintID: blueprintID, TaskID: taskID, WorktreePath: "/tmp/wt-oc2",
		StepPlan: []domain.BlueprintPlanStep{{StepIndex: 0, PromptID: stepPromptID, PromptName: "S", PromptBody: "b", Source: "user"}},
	})
	childID2 := seedPgStepConversation(t, h, orgID, userID, taskID, stepPromptID, brID2, 0)
	if _, err := h.AdminDB.Exec(`UPDATE conversations SET status = NULL WHERE id = $1`, childID2); err != nil {
		t.Fatalf("set child 2 running: %v", err)
	}
	seedChildClaim(childID2, "exec-oc2")

	changed, err = stores.Blueprints.MarkRunStatus(ctx, orgID, brID2, domain.BlueprintRunStatusCancelled, "user_cancelled", nil)
	if err != nil {
		t.Fatalf("MarkRunStatus: %v", err)
	}
	if !changed {
		t.Fatal("MarkRunStatus reported no change")
	}
	if st, parked := pgConversationParked(t, h, childID2); st != "open" || !parked {
		t.Errorf("child 2 = (%q, parked=%v), want (open, true)", st, parked)
	}
	assertClaimReleased(childID2)
}

// MarkRunStatus → GetRunForConversation on a real Postgres tx. Covers the UUID/TEXT
// column split (blueprint_runs.id UUID, blueprint_id TEXT).
func TestBlueprintStore_Postgres_RunLifecycle(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, userID := seedPgOrgForBlueprints(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	blueprints := stores.Blueprints
	ctx := context.Background()

	blueprintID := "blueprint-rl-" + orgID[:8]
	stepPromptID := "step-rl-" + orgID[:8]
	seedPgBlueprint(t, h, orgID, userID, blueprintID)
	seedPgPrompt(t, h, orgID, userID, stepPromptID)
	taskID := seedPgTask(t, h, orgID, userID)

	blueprintRunID := insertPgBlueprintRun(t, h, orgID, userID, domain.BlueprintRun{
		BlueprintID:  blueprintID,
		TaskID:       taskID,
		WorktreePath: "/tmp/wt-pg-blueprint",
		StepPlan: []domain.BlueprintPlanStep{
			{StepIndex: 0, PromptID: stepPromptID, PromptName: "Step", PromptBody: "do the step", Source: "user", Brief: "b0"},
		},
	})

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
	// The frozen step plan round-trips through the step_plan column.
	if len(cr.StepPlan) != 1 {
		t.Fatalf("round-tripped step plan length = %d, want 1", len(cr.StepPlan))
	}
	if cr.StepPlan[0].PromptID != stepPromptID || cr.StepPlan[0].PromptBody != "do the step" {
		t.Errorf("round-tripped step plan[0] = %+v, want frozen prompt %s body 'do the step'", cr.StepPlan[0], stepPromptID)
	}

	// Seed a step conversation, persist a terminal outcome the way
	// processCompletion does, and confirm ConversationsForBlueprint surfaces it — the channel the
	// orchestrator advances on (the successor to the old per-step verdict).
	stepConversationID := seedPgStepConversation(t, h, orgID, userID, taskID, stepPromptID, blueprintRunID, 0)
	if _, err := stores.Conversations.CompleteSystem(ctx, orgID, stepConversationID, "completed", 0, 0, 0, "did the thing", "finish", "", ""); err != nil {
		t.Fatalf("complete step conversation: %v", err)
	}
	stepConversations, err := blueprints.ConversationsForBlueprint(ctx, orgID, blueprintRunID)
	if err != nil {
		t.Fatalf("ConversationsForBlueprint: %v", err)
	}
	if len(stepConversations) != 1 || stepConversations[0].Outcome != "finish" {
		t.Errorf("ConversationsForBlueprint = %+v, want one conversation with outcome=finish", stepConversations)
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

	// GetRunForConversation resolves step → blueprint.
	cr2, idx, err := blueprints.GetRunForConversation(ctx, orgID, stepConversationID)
	if err != nil {
		t.Fatalf("GetRunForConversation: %v", err)
	}
	if cr2 == nil || cr2.ID != blueprintRunID {
		t.Errorf("GetRunForConversation blueprint = %+v, want id=%s", cr2, blueprintRunID)
	}
	if idx == nil || *idx != 0 {
		t.Errorf("GetRunForConversation stepIdx = %v, want 0", idx)
	}

	// GetRunForConversationSystem mirrors GetRunForConversation for goroutine-internal
	// callers (blueprint orchestrator cleanup, post-resume finalize) that
	// have no JWT-claims context. The contract is identical — both
	// arms read the same row — so the assertion is just that the
	// admin-pool variant returns the same values.
	cr3, idx3, err := blueprints.GetRunForConversationSystem(ctx, orgID, stepConversationID)
	if err != nil {
		t.Fatalf("GetRunForConversationSystem: %v", err)
	}
	if cr3 == nil || cr3.ID != blueprintRunID {
		t.Errorf("GetRunForConversationSystem blueprint = %+v, want id=%s", cr3, blueprintRunID)
	}
	if idx3 == nil || *idx3 != 0 {
		t.Errorf("GetRunForConversationSystem stepIdx = %v, want 0", idx3)
	}
}

// TestBlueprintStore_Postgres_StepPlanLengths pins the batched plan-length
// read the run projection uses to say whether a step is its chain's last one:
// one query over many blueprint runs, counted in SQL over the frozen plan, and
// a run it cannot resolve is absent from the map rather than reported as a
// zero-step plan.
func TestBlueprintStore_Postgres_StepPlanLengths(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, userID := seedPgOrgForBlueprints(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	blueprints := stores.Blueprints
	ctx := context.Background()

	blueprintID := "blueprint-spl-" + orgID[:8]
	stepPromptID := "step-spl-" + orgID[:8]
	seedPgBlueprint(t, h, orgID, userID, blueprintID)
	seedPgPrompt(t, h, orgID, userID, stepPromptID)
	taskID := seedPgTask(t, h, orgID, userID)

	plan := []domain.BlueprintPlanStep{
		{StepIndex: 0, PromptID: stepPromptID, PromptName: "One", PromptBody: "body one", Source: "user"},
		{StepIndex: 1, PromptID: stepPromptID, PromptName: "Two", PromptBody: "body two", Source: "user"},
		{StepIndex: 2, PromptID: stepPromptID, PromptName: "Three", PromptBody: "body three", Source: "user"},
	}
	threeID := insertPgBlueprintRun(t, h, orgID, userID, domain.BlueprintRun{
		BlueprintID: blueprintID, TaskID: taskID,
		WorktreePath: "/tmp/wt-pg-spl-3", StepPlan: plan,
	})
	// Its own task: one running blueprint_run per task is a schema invariant,
	// and nothing here is about two runs sharing a task.
	oneStepTaskID := seedPgTask(t, h, orgID, userID)
	oneID := insertPgBlueprintRun(t, h, orgID, userID, domain.BlueprintRun{
		BlueprintID: blueprintID, TaskID: oneStepTaskID,
		WorktreePath: "/tmp/wt-pg-spl-1", StepPlan: plan[:1],
	})
	missingID := uuid.NewString()

	got, err := blueprints.StepPlanLengths(ctx, orgID, []string{threeID, oneID, missingID})
	if err != nil {
		t.Fatalf("StepPlanLengths: %v", err)
	}
	if got[threeID] != 3 {
		t.Errorf("three-step plan length = %d, want 3", got[threeID])
	}
	if got[oneID] != 1 {
		t.Errorf("one-step plan length = %d, want 1", got[oneID])
	}
	if _, ok := got[missingID]; ok {
		t.Error("an unresolvable blueprint run must be absent from the map, not reported as a zero-step plan")
	}

	empty, err := blueprints.StepPlanLengths(ctx, orgID, nil)
	if err != nil {
		t.Fatalf("StepPlanLengths(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("StepPlanLengths(nil) = %v, want an empty map", empty)
	}
}

// TestBlueprintStore_Postgres_FiredRunCreatorUnderRLS pins the creator half of
// the firing door against actual RLS, not the AdminDB-bypassed conformance
// setup:
//
//  1. An event firing lands with creator_user_id NULL, which
//     blueprint_runs_creator_matches_trigger_type requires of an event row —
//     there is no human author to name.
//
//  2. A manual firing lands with the creator the call named. The door runs on
//     the admin pool, where there is no tf.current_user_id() to resolve one
//     from, so the value has to arrive on the call and be written verbatim.
//
//  3. blueprint_runs_select admits a NULL-creator row through plain org
//     membership rather than the creator-equals-caller predicate. This is the
//     one that has to hold under real claims: without it the request-facing
//     GetRun / GetRunForConversation / CancelBlueprintRun paths would 404 on
//     every auto-fired blueprint, because an app-pool SELECT cannot match a
//     NULL creator.
func TestBlueprintStore_Postgres_FiredRunCreatorUnderRLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, userID := seedPgOrgForBlueprints(t, h)
	seedPgDefaultTeam(t, h, orgID, userID)
	blueprintID := "blueprint-rls-" + orgID[:8]
	seedPgBlueprint(t, h, orgID, userID, blueprintID)
	promptID := "step-rls-" + orgID[:8]
	seedPgPrompt(t, h, orgID, userID, promptID)
	taskID := seedPgTask(t, h, orgID, userID)

	// The real app pool (RLS-active under tf_app via WithTx) beside the real
	// admin pool, so the reads below go through the policy rather than past it.
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	readRun := func(t *testing.T, id string) (trigger string, creator sql.NullString) {
		t.Helper()
		if err := h.AdminDB.QueryRow(
			`SELECT trigger_type, creator_user_id::text FROM blueprint_runs WHERE id = $1`, id,
		).Scan(&trigger, &creator); err != nil {
			t.Fatalf("read back blueprint_run: %v", err)
		}
		return trigger, creator
	}

	// ---- The event arm ----
	// blueprint_runs.trigger_id carries a foreign key, so the fence key needs
	// a real handler behind it, and triggering_event_id the task's own event.
	eventRunID := uuid.NewString()
	triggerID := uuid.NewString()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO event_handlers (id, org_id, team_id, creator_user_id, kind, event_type, enabled, source,
		                            blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'trigger', $5, true, 'user', $6, 4, 0, now(), now())
	`, triggerID, orgID, pgBlueprintTeam(t, h, blueprintID), userID, domain.EventGitHubPRCICheckFailed, blueprintID)
	var eventID string
	if err := h.AdminDB.QueryRow(`SELECT primary_event_id::text FROM tasks WHERE id = $1`, taskID).Scan(&eventID); err != nil {
		t.Fatalf("read the task's primary event: %v", err)
	}
	firePgRun(t, h, stores, orgID, domain.BlueprintRun{
		ID: eventRunID, BlueprintID: blueprintID, TaskID: taskID,
		TriggerType: domain.BlueprintTriggerEvent, TriggerID: triggerID,
		TriggeringEventID: eventID, WorktreePath: "/tmp/wt-blueprint-event",
	}, domain.Conversation{PromptID: promptID})

	if trigger, creator := readRun(t, eventRunID); trigger != string(domain.BlueprintTriggerEvent) {
		t.Errorf("trigger_type = %q, want event", trigger)
	} else if creator.Valid {
		t.Errorf("creator_user_id = %q, want NULL (event-trigger CHECK)", creator.String)
	}

	// ---- The manual arm ----
	// Its own task — the event run above still holds the first one's single
	// running slot.
	manualRunID := uuid.NewString()
	manualTaskID := seedPgTask(t, h, orgID, userID)
	firePgRun(t, h, stores, orgID, domain.BlueprintRun{
		ID: manualRunID, BlueprintID: blueprintID, TaskID: manualTaskID,
		TriggerType: domain.BlueprintTriggerManual, WorktreePath: "/tmp/wt-blueprint-manual",
	}, domain.Conversation{PromptID: promptID, CreatorUserID: userID})

	_, manualCreator := readRun(t, manualRunID)
	if !manualCreator.Valid {
		t.Fatalf("manual creator_user_id is NULL; want %s (the creator the firing named)", userID)
	}
	if manualCreator.String != userID {
		t.Errorf("manual creator_user_id = %q, want %q", manualCreator.String, userID)
	}

	// ---- The select policy on a NULL-creator row ----
	var sawEventRun bool
	if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		cr, err := tx.Blueprints.GetRun(ctx, orgID, eventRunID)
		if err != nil {
			return err
		}
		sawEventRun = cr != nil
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
// Mirrors the ConversationStore cross-org leakage suite.
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

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	blueprints := stores.Blueprints
	ctx := context.Background()

	if _, err := blueprints.ReplaceSteps(ctx, orgA, blueprintIDA, []string{stepIDA}, nil); err != nil {
		t.Fatalf("replace A: %v", err)
	}
	if _, err := blueprints.ReplaceSteps(ctx, orgB, blueprintIDB, []string{stepIDB}, nil); err != nil {
		t.Fatalf("replace B: %v", err)
	}

	crA := insertPgBlueprintRun(t, h, orgA, userA, domain.BlueprintRun{
		BlueprintID: blueprintIDA, TaskID: taskA, WorktreePath: "/tmp/leak-a",
	})
	crB := insertPgBlueprintRun(t, h, orgB, userB, domain.BlueprintRun{
		BlueprintID: blueprintIDB, TaskID: taskB, WorktreePath: "/tmp/leak-b",
	})
	// Seed a step conversation under each blueprint so the cross-org
	// ConversationsForBlueprintSystem check below has rows to (not) leak.
	seedPgStepConversation(t, h, orgA, userA, taskA, stepIDA, crA, 0)
	seedPgStepConversation(t, h, orgB, userB, taskB, stepIDB, crB, 0)

	// ListStepsSystem on org A must not see blueprint B's step.
	stepsA, err := blueprints.ListStepsSystem(ctx, orgA, blueprintIDB)
	if err != nil {
		t.Fatalf("ListStepsSystem A→blueprint B: %v", err)
	}
	if len(stepsA) != 0 {
		t.Errorf("ListStepsSystem(orgA, blueprintIDB) leaked %d rows, want 0", len(stepsA))
	}

	// ConversationsForBlueprintSystem on org A must not return cross-org step
	// conversations.
	convsCrossOrg, err := blueprints.ConversationsForBlueprintSystem(ctx, orgA, crB)
	if err != nil {
		t.Fatalf("ConversationsForBlueprintSystem cross-org: %v", err)
	}
	if len(convsCrossOrg) != 0 {
		t.Errorf("ConversationsForBlueprintSystem(orgA, crB) leaked %d rows, want 0", len(convsCrossOrg))
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
// a cross-org blueprint_runs insert raises 42501 from
// blueprint_runs_modify WITH CHECK.
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

	ctx := context.Background()

	// Seed a blueprint run in orgA with creator_user_id=alice, the shape a
	// manual delegation of hers leaves. Whether bob can see or mutate it is
	// the question.
	blueprintRunA := insertPgBlueprintRun(t, h, orgA, alice, domain.BlueprintRun{
		BlueprintID: blueprintIDA, TaskID: taskA, WorktreePath: "/tmp/rls-a",
	})

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			cr, err := pgstore.NewForTx(tx, pgtest.SecretKey).Blueprints.GetRun(ctx, orgA, blueprintRunA)
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
			cr, err := pgstore.NewForTx(tx, pgtest.SecretKey).Blueprints.GetRun(ctx, orgA, blueprintRunA)
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
		// bob's claims point at orgB; the row would land with
		// org_id=orgA. blueprint_runs_modify WITH CHECK requires the
		// row's org_id to match tf.current_org_id(), so 42501 is the
		// expected outcome.
		//
		// Written as SQL rather than through a store method because the
		// subject is the policy, and no app-pool door writes this table:
		// a run is minted on the admin pool, by the one firing door. The
		// policy is what stands behind that, and it is asked here
		// directly.
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			_, e := tx.ExecContext(ctx, `
				INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id,
				                            trigger_type, status, step_plan, worktree_path, started_at)
				VALUES ($1, $2, $3::uuid, $4, $5, 'manual', 'running', '[]', '/tmp/rls-x', now())
			`, uuid.NewString(), orgA, bob, blueprintIDA, taskA)
			return e
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

// seedPgOrgForBlueprints creates the (org, user, membership) triplet
// blueprint row writes need to satisfy creator_user_id FK resolution.
// Returns both ids — blueprint tests also need the userID directly for
// seeding tasks / conversations whose RLS predicate gates on
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
// composite FK ((step_prompt_id, team_id) → prompts(id, team_id))
// holds across a blueprint and its steps.
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

// seedPgSystemPrompt inserts a shipped-shape prompt row directly: source='system',
// creator_user_id NULL, and the given system_slug — the shape PromptStore.Create
// can't produce (it forces a non-NULL creator). Returns the id.
func seedPgSystemPrompt(t *testing.T, h *pgtest.Harness, orgID, teamID string, p domain.Prompt) string {
	t.Helper()
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, team_id, system_slug, creator_user_id, name, body, source, allowed_tools, model, usage_count, user_modified, created_at, updated_at)
		VALUES ($1, $2, $3::uuid, $4, NULL, $5, $6, 'system', $7, $8, 0, FALSE, now(), now())
	`, p.ID, orgID, teamID, p.SystemSlug, p.Name, p.Body, p.AllowedTools, p.Model); err != nil {
		t.Fatalf("seed system prompt %s: %v", p.ID, err)
	}
	return p.ID
}

// seedPgNamedTeam creates a second team (+ admin membership) in an org so the
// cross-team duplication guard has two real teams to span.
func seedPgNamedTeam(t *testing.T, h *pgtest.Harness, orgID, userID, slug string) string {
	t.Helper()
	teamID := uuid.New().String()
	if _, err := h.AdminDB.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES ($1, $2, $3, $4)`,
		teamID, orgID, slug+"-"+teamID[:8], slug,
	); err != nil {
		t.Fatalf("seed named team %s: %v", slug, err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`,
		userID, teamID,
	); err != nil {
		t.Fatalf("seed membership in %s: %v", teamID, err)
	}
	return teamID
}

// seedPgPromptInTeam / seedPgBlueprintInTeam insert into an explicit team
// (vs ensurePgTeamForOrg's "first team") so a single org can hold rows in two
// teams for the cross-team guard.
func seedPgPromptInTeam(t *testing.T, h *pgtest.Harness, orgID, userID, teamID, id string) {
	t.Helper()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, team_id, creator_user_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3::uuid, $4, $5, 'body', 'user', '', now(), now())
	`, id, orgID, teamID, userID, id); err != nil {
		t.Fatalf("seed prompt %s in team %s: %v", id, teamID, err)
	}
}

// TestBlueprintStore_Postgres_ActorAgentRoundTrip is the Postgres parity of the
// SQLite actor round-trip: a firing freezes blueprint_runs.actor_agent_id under
// the composite (actor_agent_id, org_id) FK to agents, and GetRunSystem reads it
// back. Validates the in-place baseline column + FK.
func TestBlueprintStore_Postgres_ActorAgentRoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, userID := seedPgOrgForBlueprints(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	blueprintID := "blueprint-actor-" + orgID[:8]
	stepPromptID := "step-actor-" + orgID[:8]
	seedPgBlueprint(t, h, orgID, userID, blueprintID)
	seedPgPrompt(t, h, orgID, userID, stepPromptID)
	taskID := seedPgTask(t, h, orgID, userID)

	agentID := uuid.New().String()
	if _, err := h.AdminDB.Exec(
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Bot')`, agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	brID := uuid.NewString()
	firePgRun(t, h, stores, orgID, domain.BlueprintRun{
		ID: brID, BlueprintID: blueprintID, TaskID: taskID,
		WorktreePath: "/tmp/wt-actor", ActorAgentID: agentID,
		StepPlan: []domain.BlueprintPlanStep{{StepIndex: 0, PromptID: stepPromptID, PromptName: "S", PromptBody: "b", Source: "user"}},
	}, domain.Conversation{PromptID: stepPromptID, CreatorUserID: userID})
	got, err := stores.Blueprints.GetRunSystem(ctx, orgID, brID)
	if err != nil || got == nil {
		t.Fatalf("GetRunSystem = (%v, %v)", got, err)
	}
	if got.ActorAgentID != agentID {
		t.Errorf("actor round-trip = %q, want %q", got.ActorAgentID, agentID)
	}
}

// pgBlueprintTeam reads a blueprint's team. event_handlers FKs its blueprint on
// (blueprint_id, team_id), so a trigger has to be seeded into the blueprint's
// own team rather than whichever one the fixture happens to hold.
func pgBlueprintTeam(t *testing.T, h *pgtest.Harness, blueprintID string) string {
	t.Helper()
	var teamID string
	if err := h.AdminDB.QueryRow(`SELECT team_id::text FROM blueprints WHERE id = $1`, blueprintID).Scan(&teamID); err != nil {
		t.Fatalf("read blueprint team: %v", err)
	}
	return teamID
}

func seedPgBlueprintInTeam(t *testing.T, h *pgtest.Harness, orgID, userID, teamID, id string) {
	t.Helper()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO blueprints (id, org_id, team_id, creator_user_id, name, source, created_at, updated_at)
		VALUES ($1, $2, $3::uuid, $4, $5, 'user', now(), now())
	`, id, orgID, teamID, userID, id); err != nil {
		t.Fatalf("seed blueprint %s in team %s: %v", id, teamID, err)
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
// if none exists. Prompts and blueprints are team-scoped (team_id NOT NULL)
// and a blueprint's steps must share its team, so multiple
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

// seedPgStepConversation inserts a conversations row linked to a blueprint_run for verdict /
// step-to-blueprint resolution tests.
func seedPgStepConversation(t *testing.T, h *pgtest.Harness, orgID, userID, taskID, promptID, blueprintRunID string, stepIdx int) string {
	t.Helper()
	conversationID := uuid.New().String()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO conversations (id, org_id, creator_user_id, team_id, task_id, prompt_id, status, model, started_at, blueprint_run_id, blueprint_step_index)
		VALUES ($1, $2, $3, $4, $5, $6, 'running', 'claude-sonnet-4-6', now(), $7, $8)
	`, conversationID, orgID, userID, teamID, taskID, promptID, blueprintRunID, stepIdx); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return conversationID
}
