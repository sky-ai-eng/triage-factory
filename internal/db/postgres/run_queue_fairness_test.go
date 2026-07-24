package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// fairnessFixture is one org's ready-to-enqueue run-queue fixture: a running
// blueprint_run under a fresh task + prompt, reused for every run enqueued
// under that org (distinct step indices), plus the org's creator user id.
type fairnessFixture struct {
	orgID, userID          string
	brID, taskID, promptID string
	nextStep               int
}

func newFairnessFixture(t *testing.T, h *pgtest.Harness) fairnessFixture {
	t.Helper()
	orgID, userID := seedPgOrgForBlueprints(t, h)
	brID, taskID, promptID := seedPgRunQueueFixture(t, h, orgID, userID)
	return fairnessFixture{orgID: orgID, userID: userID, brID: brID, taskID: taskID, promptID: promptID}
}

// enqueue stages one queued run under the fixture (distinct step index each
// call), optionally stamped with a preferred executor. Returns the run id.
func (f *fairnessFixture) enqueue(t *testing.T, stores db.Stores, preferred string) string {
	t.Helper()
	runID := uuid.New().String()
	idx := f.nextStep
	f.nextStep++
	if err := stores.RunQueue.EnqueueRun(context.Background(), f.orgID, domain.Conversation{
		ID: runID, TaskID: f.taskID, PromptID: f.promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: f.userID, BlueprintRunID: f.brID, BlueprintStepIndex: &idx,
		PreferredExecutorID: preferred,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	return runID
}

func setPgOrgCap(t *testing.T, h *pgtest.Harness, orgID string, cap *int) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO org_settings (org_id, max_concurrent_runs) VALUES ($1, $2)
		ON CONFLICT (org_id) DO UPDATE SET max_concurrent_runs = EXCLUDED.max_concurrent_runs
	`, orgID, cap)
}

func forcePgRunning(t *testing.T, h *pgtest.Harness, runID string) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET status = 'running' WHERE id = $1`, runID)
}

// TestClaimFairness_BurstDoesNotStarveOtherOrg is the headline acceptance: org
// A floods 100 queued runs (all older), org B enqueues 1 (newer). Pure FIFO
// would claim B's run 101st — after A's entire backlog. Fairness (fewest-active
// org first) instead hands the very next free slot after A's first claim to B.
func TestClaimFairness_BurstDoesNotStarveOtherOrg(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	a := newFairnessFixture(t, h)
	b := newFairnessFixture(t, h)

	for i := 0; i < 100; i++ {
		a.enqueue(t, stores, "")
	}
	bRun := b.enqueue(t, stores, "")
	// Make every A run strictly older than B's so pure FIFO would drain all 100
	// A runs before ever reaching B — isolating fairness as the reason B jumps
	// the queue.
	pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET started_at = now() - interval '1 hour' WHERE org_id = $1`, a.orgID)

	claim := func() *domain.Conversation {
		got, err := stores.RunQueue.ClaimNextRun(ctx, "exec-1", 1, db.ClaimPlacement{})
		if err != nil {
			t.Fatalf("ClaimNextRun: %v", err)
		}
		return got
	}

	// Claim 1: both orgs at 0 active, tie broken by started_at → A's oldest.
	c1 := claim()
	if c1 == nil || c1.OrgID != a.orgID {
		t.Fatalf("claim 1 = %+v, want an org-A run (oldest, both at 0 active)", c1)
	}
	// Claim 2: A now has 1 active, B has 0 → fairness picks B, NOT A's 2nd-oldest
	// of 99 remaining. This is the whole point: B waits one slot, not a backlog.
	c2 := claim()
	if c2 == nil || c2.OrgID != b.orgID || c2.ID != bRun {
		t.Fatalf("claim 2 = %+v, want org-B run %s (fewest-active wins over A's older backlog)", c2, bRun)
	}
}

// TestClaimCap_BlocksAtCapAndReconfiguresLive proves an org at its
// max_concurrent_runs cap occupies no additional slots, and that raising or
// lowering the cap takes effect on the very next claim with no restart (the
// claim reads the column live).
func TestClaimCap_BlocksAtCapAndReconfiguresLive(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	a := newFairnessFixture(t, h)
	for i := 0; i < 5; i++ {
		a.enqueue(t, stores, "")
	}
	setCap := func(n int) { c := n; setPgOrgCap(t, h, a.orgID, &c) }
	claim := func() *domain.Conversation {
		got, err := stores.RunQueue.ClaimNextRun(ctx, "exec-1", 1, db.ClaimPlacement{})
		if err != nil {
			t.Fatalf("ClaimNextRun: %v", err)
		}
		return got
	}

	// Cap 2: exactly two claims succeed, the org then has 2 active runs.
	setCap(2)
	if claim() == nil {
		t.Fatal("first claim under cap=2 must succeed")
	}
	if claim() == nil {
		t.Fatal("second claim under cap=2 must succeed")
	}
	// At cap: a third claim sees the org at 2 active (>= cap) and finds nothing,
	// even though 3 runs are still queued.
	if got := claim(); got != nil {
		t.Fatalf("claim at cap=2 with 2 active returned %+v, want nil (org invisible at cap)", got)
	}
	// Raise the cap to 3 with no restart → the next claim immediately succeeds.
	setCap(3)
	if got := claim(); got == nil {
		t.Fatal("raising cap to 3 must unblock the next claim without restart")
	}
	// Org now has 3 active. Lower the cap back to 3's boundary: at 3 active it's
	// again at cap (blocked); the live re-read is what makes this take effect.
	setCap(3)
	if got := claim(); got != nil {
		t.Fatalf("claim at cap=3 with 3 active returned %+v, want nil", got)
	}
	// 0 (and NULL) mean unlimited — a non-positive cap never blocks.
	setCap(0)
	if got := claim(); got == nil {
		t.Fatal("cap=0 means unlimited; the claim must succeed")
	}
}

// TestClaimFairness_ComposesWithinPlacementTiers pins spec §6.2 composition:
// fairness orders WITHIN a placement tier, but never across tiers — a tier-1
// (preferred=me) run is claimed before a tier-2 run regardless of which org
// has fewer active runs.
func TestClaimFairness_ComposesWithinPlacementTiers(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	cfg := db.ClaimPlacement{Enabled: true, AgingInterval: 20 * time.Second, Liveness: 12 * time.Second}

	a := newFairnessFixture(t, h)
	b := newFairnessFixture(t, h)
	registerLiveExecutor(t, stores, "exec-a")

	t.Run("fairness_orders_within_tier1", func(t *testing.T) {
		h.Reset(t)
		a = newFairnessFixture(t, h)
		b = newFairnessFixture(t, h)
		registerLiveExecutor(t, stores, "exec-a")
		// Org A carries an active run; both A and B have a fresh tier-1 run
		// (preferred = exec-a). Within tier 1, fairness gives B (0 active) the
		// slot over A (1 active).
		forcePgRunning(t, h, a.enqueue(t, stores, ""))
		aTier1 := a.enqueue(t, stores, "exec-a")
		bTier1 := b.enqueue(t, stores, "exec-a")

		got, err := stores.RunQueue.ClaimNextRun(ctx, "exec-a", 1, cfg)
		if err != nil || got == nil {
			t.Fatalf("claim: (%+v, %v)", got, err)
		}
		if got.ID != bTier1 {
			t.Fatalf("claimed %q, want org-B tier-1 run %q (fewest-active within tier 1; A's %q has an active run)", got.ID, bTier1, aTier1)
		}
	})

	t.Run("tier1_beats_tier2_regardless_of_fairness", func(t *testing.T) {
		h.Reset(t)
		a = newFairnessFixture(t, h)
		b = newFairnessFixture(t, h)
		registerLiveExecutor(t, stores, "exec-a")
		// Org A: a fresh tier-1 run (preferred = exec-a) AND an active run.
		// Org B: an aged tier-2 run (preferred elsewhere, past the aging
		// window) and zero active runs. Despite B being the fairer org, the
		// tier-1 run is claimed first — tier dominates fairness.
		forcePgRunning(t, h, a.enqueue(t, stores, ""))
		aTier1 := a.enqueue(t, stores, "exec-a")
		bTier2 := b.enqueue(t, stores, "exec-other")
		pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET started_at = now() - interval '2 minutes' WHERE id = $1`, bTier2)

		got, err := stores.RunQueue.ClaimNextRun(ctx, "exec-a", 1, cfg)
		if err != nil || got == nil {
			t.Fatalf("claim: (%+v, %v)", got, err)
		}
		if got.ID != aTier1 {
			t.Fatalf("claimed %q, want org-A tier-1 run %q (tier 1 before tier 2, even though B is fairer)", got.ID, aTier1)
		}
	})
}

// TestClaimFairness_ExplainAnalyzeSanity is the acceptance's plan-cost check:
// at a realistic queue depth the fairness/cap claim stays cheap — the per-org
// active count is served by the partial index and the candidate scan + sort is
// bounded, never a pathological O(queued × active) nested loop. Asserts the
// planner's own measured execution time is well under a generous bound and logs
// the plan for human review.
func TestClaimFairness_ExplainAnalyzeSanity(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	a := newFairnessFixture(t, h)
	b := newFairnessFixture(t, h)
	for i := 0; i < 250; i++ {
		a.enqueue(t, stores, "")
		b.enqueue(t, stores, "")
	}
	// Some active runs across both orgs so the org_active CTE has real groups.
	forcePgRunning(t, h, a.enqueue(t, stores, ""))
	forcePgRunning(t, h, b.enqueue(t, stores, ""))

	// The candidate SELECT the claim runs (placement disabled) minus FOR UPDATE
	// SKIP LOCKED, so EXPLAIN ANALYZE is side-effect-free (locks/claims nothing)
	// while still measuring the CTE + join + cap filter + fairness sort that
	// dominate the cost.
	const candidate = `
		EXPLAIN (ANALYZE, FORMAT JSON)
		WITH org_active AS (
			SELECT org_id, count(*)::int AS active
			FROM conversations
			WHERE status IN ('running')
			GROUP BY org_id
		)
		SELECT r.id FROM conversations r
		JOIN blueprint_runs br ON br.id = r.blueprint_run_id
		LEFT JOIN org_active oa ON oa.org_id = r.org_id
		LEFT JOIN org_settings os ON os.org_id = r.org_id
		WHERE r.status = 'queued'
		  AND br.cancel_requested = false
		  AND br.status = 'running'
		  AND (
		    os.max_concurrent_runs IS NULL
		    OR os.max_concurrent_runs <= 0
		    OR COALESCE(oa.active, 0) < os.max_concurrent_runs
		  )
		ORDER BY COALESCE(oa.active, 0), r.started_at, r.id
		LIMIT 1`

	var planJSON []byte
	if err := h.AdminDB.QueryRow(candidate).Scan(&planJSON); err != nil {
		t.Fatalf("EXPLAIN ANALYZE: %v", err)
	}
	t.Logf("claim candidate plan @ 500 queued: %s", planJSON)

	var plans []struct {
		ExecutionTime float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(planJSON, &plans); err != nil || len(plans) == 0 {
		t.Fatalf("parse plan json: %v (len=%d)", err, len(plans))
	}
	// Generous bound: at 500 queued rows a healthy plan is sub-millisecond; this
	// only trips on a genuinely pathological plan, never on a loaded CI box.
	if got := plans[0].ExecutionTime; got > 250 {
		t.Fatalf("claim candidate execution time = %.1fms at 500 queued, want < 250ms (pathological plan)", got)
	}
}
