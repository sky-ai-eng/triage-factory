package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestTaskTeams_VisibilityAndClaimConsolidation drives the multi-team
// task model against real RLS through the app pool. One task is shared
// by two teams via task_teams; while unclaimed both teams see it, and
// claiming consolidates the card to the claimer's team — after which
// only that team sees it. AdminDB bypasses RLS, so the visibility
// assertions run under h.WithUser (tf_app + JWT claims).
func TestTaskTeams_VisibilityAndClaimConsolidation(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	// orgA with alice in the default team (teamA). bob is added to a
	// second team (teamB) in the same org.
	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	teamB := pgtest.SeedTeam(t, h, orgA, "teamb")
	bob := pgtest.SeedUser(t, h, "bob")
	pgtest.AddOrgMember(t, h, bob, orgA, teamB, "member", "member")

	// One task on a fresh entity, owned by teamA, unclaimed, with both
	// teams in the visibility set.
	taskID := seedSharedTask(t, h, orgA, alice, teamA, []string{teamA, teamB})

	// --- Unclaimed: both teams see it ---
	assertVisible(t, h, alice, orgA, taskID, true, "alice (owner team) should see the unclaimed task")
	assertVisible(t, h, bob, orgA, taskID, true, "bob (visibility-set team) should see the unclaimed task")

	// --- bob claims it: owner consolidates to teamB ---
	if err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		ok, e := pgstore.NewForTx(tx).Tasks.ClaimQueuedForUser(ctx, orgA, taskID, bob)
		if e != nil {
			return e
		}
		if !ok {
			t.Error("bob's claim returned ok=false on an unclaimed task")
		}
		return nil
	}); err != nil {
		t.Fatalf("bob claim path: %v", err)
	}

	// team_id consolidated to bob's acting team (teamB).
	var ownerTeam, claimedByUser string
	if err := h.AdminDB.QueryRow(
		`SELECT team_id, COALESCE(claimed_by_user_id::text, '') FROM tasks WHERE id = $1`, taskID,
	).Scan(&ownerTeam, &claimedByUser); err != nil {
		t.Fatalf("read task post-claim: %v", err)
	}
	if ownerTeam != teamB {
		t.Errorf("owner team_id = %q post-claim, want teamB %q (acting-team consolidation)", ownerTeam, teamB)
	}
	if claimedByUser != bob {
		t.Errorf("claimed_by_user_id = %q, want bob %q", claimedByUser, bob)
	}

	// --- Claimed: only the owner team sees it ---
	assertVisible(t, h, bob, orgA, taskID, true, "bob (owner after claim) should still see the task")
	assertVisible(t, h, alice, orgA, taskID, false, "alice should NOT see the task once it is claimed by teamB")

	// --- Second claim no-ops (CAS) ---
	if err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		ok, e := pgstore.NewForTx(tx).Tasks.ClaimQueuedForUser(ctx, orgA, taskID, bob)
		if e != nil {
			return e
		}
		if ok {
			t.Error("second claim returned ok=true; CAS guard broken")
		}
		return nil
	}); err != nil {
		t.Fatalf("second claim path: %v", err)
	}
}

// TestTaskTeams_ClaimPrefersOwnerWhenAmbiguous pins the acting-team
// derivation for a claimer who belongs to more than one team in the
// visibility set: the existing owner team is kept (not arbitrarily
// reassigned to the lowest id), honoring the "keep owner when
// ambiguous" contract. The chosen team is still always one the claimer
// belongs to, which the RLS update check requires.
func TestTaskTeams_ClaimPrefersOwnerWhenAmbiguous(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	// alice owns teamA (via SeedOrgWithUser) and is also added to teamB.
	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	teamB := pgtest.SeedTeam(t, h, orgA, "teamb")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, alice, teamB)

	// Owner is teamA; visibility set is {teamA, teamB} and alice is in
	// both — the ambiguous case.
	taskID := seedSharedTask(t, h, orgA, alice, teamA, []string{teamA, teamB})

	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		ok, e := pgstore.NewForTx(tx).Tasks.ClaimQueuedForUser(ctx, orgA, taskID, alice)
		if e != nil {
			return e
		}
		if !ok {
			t.Error("alice's claim returned ok=false on an unclaimed task")
		}
		return nil
	}); err != nil {
		t.Fatalf("alice claim path: %v", err)
	}

	var ownerTeam string
	if err := h.AdminDB.QueryRow(`SELECT team_id FROM tasks WHERE id = $1`, taskID).Scan(&ownerTeam); err != nil {
		t.Fatalf("read task post-claim: %v", err)
	}
	if ownerTeam != teamA {
		t.Errorf("owner team_id = %q post-claim, want teamA %q (owner kept when claimer is a member and choice is ambiguous)", ownerTeam, teamA)
	}
}

// TestTaskTeams_NullOwner_VisibleViaTaskTeams_ConsolidatesOnClaim is the
// SKY-372 rendering acceptance: an unresolved-owner task (team_id NULL) with
// task_teams={A} renders for team A via the task_teams visibility branch and
// is absent from a non-member team B (the owner branch's team_id IS NOT NULL
// guard keeps NULL from granting anyone). A claim by A consolidates the owner
// to A, and A keeps seeing it.
func TestTaskTeams_NullOwner_VisibleViaTaskTeams_ConsolidatesOnClaim(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	teamB := pgtest.SeedTeam(t, h, orgA, "teamb")
	bob := pgtest.SeedUser(t, h, "bob")
	pgtest.AddOrgMember(t, h, bob, orgA, teamB, "member", "member")

	// Unresolved owner (team_id NULL), visible only to teamA.
	taskID := seedUnownedTask(t, h, orgA, alice, []string{teamA})

	// Unclaimed: alice (in task_teams) sees it; bob (not in task_teams, and the
	// NULL owner grants nobody) does not.
	assertVisible(t, h, alice, orgA, taskID, true, "alice (task_teams member) should see the unowned task")
	assertVisible(t, h, bob, orgA, taskID, false, "bob (not in task_teams; NULL owner) must NOT see the unowned task")

	// alice claims → owner consolidates to teamA.
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		ok, e := pgstore.NewForTx(tx).Tasks.ClaimQueuedForUser(ctx, orgA, taskID, alice)
		if e != nil {
			return e
		}
		if !ok {
			t.Error("alice's claim returned ok=false on an unowned task")
		}
		return nil
	}); err != nil {
		t.Fatalf("alice claim path: %v", err)
	}

	var ownerTeam string
	if err := h.AdminDB.QueryRow(`SELECT COALESCE(team_id::text, '') FROM tasks WHERE id = $1`, taskID).Scan(&ownerTeam); err != nil {
		t.Fatalf("read task post-claim: %v", err)
	}
	if ownerTeam != teamA {
		t.Errorf("owner team_id = %q post-claim, want teamA %q (NULL owner consolidates to the claimer's task_teams team)", ownerTeam, teamA)
	}
	assertVisible(t, h, alice, orgA, taskID, true, "alice (owner after claim) should still see the task")
}

// assertVisible checks whether a user can read a task through the app
// pool under their own JWT claims (RLS active).
func assertVisible(t *testing.T, h *pgtest.Harness, userID, orgID, taskID string, want bool, msg string) {
	t.Helper()
	err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		task, e := pgstore.NewForTx(tx).Tasks.Get(context.Background(), orgID, taskID)
		if e != nil {
			return e
		}
		if want && task == nil {
			t.Errorf("%s: got nil (not visible)", msg)
		}
		if !want && task != nil {
			t.Errorf("%s: got non-nil (visible)", msg)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("visibility check (%s): %v", msg, err)
	}
}

// seedSharedTask inserts an entity + event + one unclaimed team-visible
// task owned by ownerTeam, plus a task_teams row for every team in
// visibleTeams. All through AdminDB (BYPASSRLS) — fixture, not the
// thing under test.
func seedSharedTask(t *testing.T, h *pgtest.Harness, orgID, creatorUserID, ownerTeam string, visibleTeams []string) string {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	eventID := uuid.New().String()
	taskID := uuid.New().String()
	sourceID := fmt.Sprintf("shared-%d", now.UnixNano())

	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Shared PR', $4, '{}'::jsonb, $5)
	`, entityID, orgID, sourceID, "https://example/"+sourceID, now)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, $4)
	`, eventID, orgID, entityID, now)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type,
		                   dedup_key, primary_event_id, status, scoring_status, priority_score, created_at)
		VALUES ($1, $2, $3, $4, 'team', $5, 'github:pr:ci_check_failed', '', $6, 'queued', 'pending', 0.5, $7)
	`, taskID, orgID, creatorUserID, ownerTeam, entityID, eventID, now)
	for _, teamID := range visibleTeams {
		pgtest.MustExec(t, h.AdminDB,
			`INSERT INTO task_teams (task_id, team_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			taskID, teamID)
	}
	return taskID
}

// seedUnownedTask is seedSharedTask's NULL-owner sibling: it inserts an
// unclaimed task with team_id NULL (the unresolved-owner residual) plus a
// task_teams row per visibleTeams entry. The claimed-⇒-owned CHECK permits the
// NULL owner because the row is unclaimed. AdminDB (BYPASSRLS) — fixture only.
func seedUnownedTask(t *testing.T, h *pgtest.Harness, orgID, creatorUserID string, visibleTeams []string) string {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	eventID := uuid.New().String()
	taskID := uuid.New().String()
	sourceID := fmt.Sprintf("unowned-%d", now.UnixNano())

	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Unowned PR', $4, '{}'::jsonb, $5)
	`, entityID, orgID, sourceID, "https://example/"+sourceID, now)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, $4)
	`, eventID, orgID, entityID, now)
	// team_id omitted → NULL (unresolved owner).
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO tasks (id, org_id, creator_user_id, visibility, entity_id, event_type,
		                   dedup_key, primary_event_id, status, scoring_status, priority_score, created_at)
		VALUES ($1, $2, $3, 'team', $4, 'github:pr:ci_check_failed', '', $5, 'queued', 'pending', 0.5, $6)
	`, taskID, orgID, creatorUserID, entityID, eventID, now)
	for _, teamID := range visibleTeams {
		pgtest.MustExec(t, h.AdminDB,
			`INSERT INTO task_teams (task_id, team_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			taskID, teamID)
	}
	return taskID
}
