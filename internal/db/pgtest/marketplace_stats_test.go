package pgtest

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// seedMarketplaceStatsTask inserts the minimal entity+event+task chain a
// runs/blueprint_runs row needs (both carry a NOT NULL task_id, and
// blueprint_runs additionally needs blueprint_id — see
// internal/db/postgres/prompts_test.go's seedPgRunsForStats, which this
// mirrors). Writes go through h.AdminDB — this is test fixture setup, not
// the flow under test, so RLS doesn't need to be exercised here.
func seedMarketplaceStatsTask(t *testing.T, h *Harness, orgID, userID, teamID string) string {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	eventID := uuid.New().String()
	taskID := uuid.New().String()
	MustExec(t, h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Stats Fixture Entity', 'https://example/x', '{}'::jsonb, $4)
	`, entityID, orgID, "stats-fixture-"+entityID, now)
	MustExec(t, h.AdminDB, `
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:opened', '', '{}'::jsonb, $4)
	`, eventID, orgID, entityID, now)
	MustExec(t, h.AdminDB, `
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, created_at)
		VALUES ($1, $2, $3, $4, 'team', $5, 'github:pr:opened', '', $6, 'queued', 'pending', $7)
	`, taskID, orgID, userID, teamID, entityID, eventID, now)
	return taskID
}

// seedPromptRun records one runs row against promptID, started at
// startedAt, with the given terminal status ('completed' or 'failed').
// runs.blueprint_run_id is NOT NULL, so this mints a throwaway
// single-step blueprint_run to hang the run off, mirroring
// seedPgRunsForStats.
func seedPromptRun(t *testing.T, h *Harness, orgID, userID, teamID, taskID, promptID, status string, startedAt time.Time) {
	t.Helper()
	bpID := uuid.New().String()
	MustExec(t, h.AdminDB, `
		INSERT INTO blueprints (id, org_id, creator_user_id, team_id, source, name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'user', 'Stats Fixture BP', now(), now())
	`, bpID, orgID, userID, teamID)
	brID := uuid.New().String()
	MustExec(t, h.AdminDB, `
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, started_at, step_plan)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'completed', '/tmp/wt', $6, '[]')
	`, brID, orgID, userID, bpID, taskID, startedAt)
	MustExec(t, h.AdminDB, `
		INSERT INTO runs (id, org_id, creator_user_id, team_id, visibility, task_id, prompt_id, status, started_at, total_cost_usd, duration_ms, blueprint_run_id)
		VALUES ($1, $2, $3, $4, 'team', $5, $6, $7, $8, 0.01, 100, $9)
	`, uuid.New().String(), orgID, userID, teamID, taskID, promptID, status, startedAt, brID)
}

// seedMarketplaceBlueprintRun records one blueprint_runs row directly
// against blueprintID — the kind=blueprint mirror of seedPromptRun.
func seedMarketplaceBlueprintRun(t *testing.T, h *Harness, orgID, userID, taskID, blueprintID, status string, startedAt time.Time) {
	t.Helper()
	MustExec(t, h.AdminDB, `
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, started_at, step_plan)
		VALUES ($1, $2, $3, $4, $5, 'manual', $6, '/tmp/wt', $7, '[]')
	`, uuid.New().String(), orgID, userID, blueprintID, taskID, status, startedAt)
}

// TestMarketplaceStats_PromptAggregation_TwoTeamsAndDeletedCopy pins the
// core TFAC-540 aggregation contract for a kind=prompt listing: runs on two
// different teams' copies each count once toward total_runs/success_rate,
// teams_using counts both while both copies exist, and once one copy is
// soft-deleted its team drops out of teams_using while its historical runs
// still count toward total_runs — the documented asymmetry (root_object_id
// survives copy deletion on the install row, so lifetime usage doesn't
// regress just because a consumer cleaned up).
func TestMarketplaceStats_PromptAggregation_TwoTeamsAndDeletedCopy(t *testing.T) {
	h := Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, SecretKey)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	teamB := SeedTeam(t, h, orgA, "teamB")
	bob := SeedUser(t, h, "bob")
	AddOrgMember(t, h, bob, orgA, teamB, "member", "admin")
	teamC := SeedTeam(t, h, orgA, "teamC")
	carol := SeedUser(t, h, "carol")
	AddOrgMember(t, h, carol, orgA, teamC, "member", "admin")

	listingID := publishAsTeamWriter(t, h, orgA, alice, teamA, "Stats Prompt", "")
	snap := marketplaceSnapshot("Stats Prompt")

	var teamBPromptID, teamCPromptID string
	if err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		var e error
		teamBPromptID, _, e = pgstore.NewForTx(tx, SecretKey).Marketplace.MaterializeListing(t.Context(), orgA, teamB, snap, listingID, 1, bob)
		return e
	}); err != nil {
		t.Fatalf("teamB install: %v", err)
	}
	if err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		var e error
		teamCPromptID, _, e = pgstore.NewForTx(tx, SecretKey).Marketplace.MaterializeListing(t.Context(), orgA, teamC, snap, listingID, 1, carol)
		return e
	}); err != nil {
		t.Fatalf("teamC install: %v", err)
	}

	taskB := seedMarketplaceStatsTask(t, h, orgA, bob, teamB)
	taskC := seedMarketplaceStatsTask(t, h, orgA, carol, teamC)
	base := time.Now().UTC().Add(-24 * time.Hour)
	seedPromptRun(t, h, orgA, bob, teamB, taskB, teamBPromptID, "completed", base)
	seedPromptRun(t, h, orgA, bob, teamB, taskB, teamBPromptID, "failed", base.Add(time.Hour))
	latestRunAt := base.Add(2 * time.Hour)
	seedPromptRun(t, h, orgA, carol, teamC, taskC, teamCPromptID, "completed", latestRunAt)

	if err := stores.Marketplace.RecomputeStatsSystem(t.Context(), orgA); err != nil {
		t.Fatalf("RecomputeStatsSystem: %v", err)
	}

	var teamsUsing, totalRuns int
	var successRate sql.NullFloat64
	var lastRunAt sql.NullTime
	if err := h.AdminDB.QueryRow(`
		SELECT teams_using, total_runs, success_rate, last_run_at FROM marketplace_listing_stats WHERE listing_id = $1
	`, listingID).Scan(&teamsUsing, &totalRuns, &successRate, &lastRunAt); err != nil {
		t.Fatalf("read stats row: %v", err)
	}
	if teamsUsing != 2 {
		t.Errorf("teams_using = %d, want 2 (both copies still exist)", teamsUsing)
	}
	if totalRuns != 3 {
		t.Errorf("total_runs = %d, want 3", totalRuns)
	}
	if !successRate.Valid || successRate.Float64 < 0.665 || successRate.Float64 > 0.667 {
		t.Errorf("success_rate = %v, want ~0.6667 (2 of 3 completed)", successRate)
	}
	if !lastRunAt.Valid || !lastRunAt.Time.Equal(latestRunAt) {
		t.Errorf("last_run_at = %v, want %v", lastRunAt.Time, latestRunAt)
	}

	// Soft-delete teamC's copy — mirrors PromptStore.Delete (UPDATE ...
	// deleted_at = now()), not a hard DELETE.
	MustExec(t, h.AdminDB, `UPDATE prompts SET deleted_at = now() WHERE id = $1`, teamCPromptID)

	if err := stores.Marketplace.RecomputeStatsSystem(t.Context(), orgA); err != nil {
		t.Fatalf("RecomputeStatsSystem after delete: %v", err)
	}
	if err := h.AdminDB.QueryRow(`
		SELECT teams_using, total_runs FROM marketplace_listing_stats WHERE listing_id = $1
	`, listingID).Scan(&teamsUsing, &totalRuns); err != nil {
		t.Fatalf("read stats row after delete: %v", err)
	}
	if teamsUsing != 1 {
		t.Errorf("teams_using after teamC's copy deleted = %d, want 1 (only teamB's copy still exists)", teamsUsing)
	}
	if totalRuns != 3 {
		t.Errorf("total_runs after teamC's copy deleted = %d, want still 3 (historical runs survive copy deletion)", totalRuns)
	}
}

// TestMarketplaceStats_BlueprintAggregation pins that the kind=blueprint
// path aggregates through blueprint_runs.blueprint_id rather than
// runs.prompt_id.
func TestMarketplaceStats_BlueprintAggregation(t *testing.T) {
	h := Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, SecretKey)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	teamB := SeedTeam(t, h, orgA, "teamB")
	bob := SeedUser(t, h, "bob")
	AddOrgMember(t, h, bob, orgA, teamB, "member", "admin")

	listingID := publishAsTeamWriter(t, h, orgA, alice, teamA, "Stats Blueprint", "")
	snap := marketplaceBlueprintSnapshot("Stats Blueprint")

	var rootBlueprintID string
	if err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		var e error
		rootBlueprintID, _, e = pgstore.NewForTx(tx, SecretKey).Marketplace.MaterializeListing(t.Context(), orgA, teamB, snap, listingID, 1, bob)
		return e
	}); err != nil {
		t.Fatalf("teamB install: %v", err)
	}

	taskB := seedMarketplaceStatsTask(t, h, orgA, bob, teamB)
	now := time.Now().UTC()
	seedMarketplaceBlueprintRun(t, h, orgA, bob, taskB, rootBlueprintID, "completed", now)
	seedMarketplaceBlueprintRun(t, h, orgA, bob, taskB, rootBlueprintID, "aborted", now.Add(time.Hour))

	if err := stores.Marketplace.RecomputeStatsSystem(t.Context(), orgA); err != nil {
		t.Fatalf("RecomputeStatsSystem: %v", err)
	}

	var teamsUsing, totalRuns int
	var successRate sql.NullFloat64
	if err := h.AdminDB.QueryRow(`
		SELECT teams_using, total_runs, success_rate FROM marketplace_listing_stats WHERE listing_id = $1
	`, listingID).Scan(&teamsUsing, &totalRuns, &successRate); err != nil {
		t.Fatalf("read stats row: %v", err)
	}
	if teamsUsing != 1 || totalRuns != 2 {
		t.Errorf("teams_using/total_runs = %d/%d, want 1/2", teamsUsing, totalRuns)
	}
	if !successRate.Valid || successRate.Float64 != 0.5 {
		t.Errorf("success_rate = %v, want 0.5 (1 of 2 completed)", successRate)
	}
}

// TestMarketplaceStats_NoRunsYet pins the no-wrong-fallbacks rule at the
// storage layer: a published listing with installs but zero runs gets a
// stats row (teams_using > 0, total_runs = 0) whose success_rate is NULL,
// never a fake 0%.
func TestMarketplaceStats_NoRunsYet(t *testing.T) {
	h := Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, SecretKey)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	listingID := publishAsTeamWriter(t, h, orgA, alice, teamA, "Never Run", "")

	if err := stores.Marketplace.RecomputeStatsSystem(t.Context(), orgA); err != nil {
		t.Fatalf("RecomputeStatsSystem: %v", err)
	}

	var teamsUsing, totalRuns int
	var successRate sql.NullFloat64
	var lastRunAt sql.NullTime
	if err := h.AdminDB.QueryRow(`
		SELECT teams_using, total_runs, success_rate, last_run_at FROM marketplace_listing_stats WHERE listing_id = $1
	`, listingID).Scan(&teamsUsing, &totalRuns, &successRate, &lastRunAt); err != nil {
		t.Fatalf("read stats row: %v", err)
	}
	if teamsUsing != 0 || totalRuns != 0 {
		t.Errorf("teams_using/total_runs = %d/%d, want 0/0 (never installed or run)", teamsUsing, totalRuns)
	}
	if successRate.Valid {
		t.Errorf("success_rate = %v, want NULL (no wrong fallbacks — never a fake 0%%)", successRate.Float64)
	}
	if lastRunAt.Valid {
		t.Errorf("last_run_at = %v, want NULL", lastRunAt.Time)
	}
}

// TestMarketplaceStats_Idempotent pins job idempotency: re-running
// RecomputeStatsSystem with unchanged underlying data converges to the same
// aggregate values rather than drifting (e.g. double-counting on a naive
// non-ON-CONFLICT insert).
func TestMarketplaceStats_Idempotent(t *testing.T) {
	h := Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, SecretKey)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	teamB := SeedTeam(t, h, orgA, "teamB")
	bob := SeedUser(t, h, "bob")
	AddOrgMember(t, h, bob, orgA, teamB, "member", "admin")

	listingID := publishAsTeamWriter(t, h, orgA, alice, teamA, "Idempotent Prompt", "")
	snap := marketplaceSnapshot("Idempotent Prompt")
	var rootPromptID string
	if err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		var e error
		rootPromptID, _, e = pgstore.NewForTx(tx, SecretKey).Marketplace.MaterializeListing(t.Context(), orgA, teamB, snap, listingID, 1, bob)
		return e
	}); err != nil {
		t.Fatalf("teamB install: %v", err)
	}
	taskB := seedMarketplaceStatsTask(t, h, orgA, bob, teamB)
	seedPromptRun(t, h, orgA, bob, teamB, taskB, rootPromptID, "completed", time.Now().UTC())

	if err := stores.Marketplace.RecomputeStatsSystem(t.Context(), orgA); err != nil {
		t.Fatalf("first RecomputeStatsSystem: %v", err)
	}
	first := readStatsRow(t, h, listingID)

	for i := 0; i < 2; i++ {
		if err := stores.Marketplace.RecomputeStatsSystem(t.Context(), orgA); err != nil {
			t.Fatalf("repeat RecomputeStatsSystem #%d: %v", i, err)
		}
	}
	second := readStatsRow(t, h, listingID)

	if first.teamsUsing != second.teamsUsing || first.totalRuns != second.totalRuns || first.successRate != second.successRate {
		t.Errorf("stats drifted across repeated recomputes: first=%+v second=%+v", first, second)
	}
	var rowCount int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_listing_stats WHERE listing_id = $1`, listingID).Scan(&rowCount); err != nil {
		t.Fatalf("count stats rows: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("stats row count after repeated recomputes = %d, want 1 (upsert, not insert-append)", rowCount)
	}
}

type statsSnapshot struct {
	teamsUsing, totalRuns int
	successRate           float64
}

func readStatsRow(t *testing.T, h *Harness, listingID string) statsSnapshot {
	t.Helper()
	var s statsSnapshot
	var sr sql.NullFloat64
	if err := h.AdminDB.QueryRow(`
		SELECT teams_using, total_runs, success_rate FROM marketplace_listing_stats WHERE listing_id = $1
	`, listingID).Scan(&s.teamsUsing, &s.totalRuns, &sr); err != nil {
		t.Fatalf("read stats row: %v", err)
	}
	if sr.Valid {
		s.successRate = sr.Float64
	}
	return s
}

// TestMarketplaceStats_RLS_ReadScopedToOwnOrg pins the RLS half of TFAC-540:
// an org member reads their own org's stats through the ordinary
// List/Get join (no separate endpoint), and a member of a different org
// never sees them — even though marketplace_listing_stats carries no
// per-team distinction, org isolation still holds.
func TestMarketplaceStats_RLS_ReadScopedToOwnOrg(t *testing.T) {
	h := Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, SecretKey)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	bob := SeedUser(t, h, "bob")
	AddOrgMember(t, h, bob, orgA, teamA, "member", "viewer")

	listingID := publishAsTeamWriter(t, h, orgA, alice, teamA, "RLS Stats Prompt", "")
	if err := stores.Marketplace.RecomputeStatsSystem(t.Context(), orgA); err != nil {
		t.Fatalf("RecomputeStatsSystem: %v", err)
	}

	// bob (a plain viewer on the publisher's own org) reads the listing and
	// sees a non-nil Stats block, computed even though nothing installed or
	// ran yet (teams_using=0, total_runs=0).
	if err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		detail, err := pgstore.NewForTx(tx, SecretKey).Marketplace.Get(t.Context(), orgA, listingID, bob)
		if err != nil {
			return err
		}
		if detail.Stats == nil {
			t.Error("bob's Get.Stats = nil, want a computed (zero-activity) stats block")
		} else if detail.Stats.TeamsUsing != 0 || detail.Stats.TotalRuns != 0 {
			t.Errorf("bob's Get.Stats = %+v, want zero-activity", detail.Stats)
		}
		return nil
	}); err != nil {
		t.Fatalf("bob's read: %v", err)
	}

	// dave is a member of an unrelated org (orgB is his session context, per
	// TestMarketplaceRLS_CrossOrgIsolation's convention — RLS keys off the
	// claims org, not a caller-supplied argument, so passing orgA explicitly
	// here proves the argument alone can't cross the boundary). orgA's
	// listing — and a fortiori its stats — resolve to nothing.
	orgB, dave, _ := SeedOrgWithUser(t, h, "dave")
	if err := h.WithUser(t, dave, orgB, func(tx *sql.Tx) error {
		if _, err := pgstore.NewForTx(tx, SecretKey).Marketplace.Get(t.Context(), orgA, listingID, dave); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("dave's cross-org Get = %v, want sql.ErrNoRows", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("dave's blocked cross-org read: %v", err)
	}

	// Defense in depth: even reading marketplace_listing_stats directly
	// (bypassing the Marketplace store's join) under dave's claims returns
	// no row for orgA's listing.
	if err := h.WithUser(t, dave, orgB, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM marketplace_listing_stats WHERE listing_id = $1`, listingID).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("dave's direct marketplace_listing_stats read = %d rows, want 0 (RLS should block)", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("dave's direct table read: %v", err)
	}
}
