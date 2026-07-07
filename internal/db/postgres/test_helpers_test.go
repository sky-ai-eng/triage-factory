package postgres_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// seedPgMember inserts a fresh users + org_memberships row pair for
// org-policy tests that need a distinct caller identity. role is one
// of 'member' / 'admin' / 'owner'. Returns the user id. Lifted out of
// the per-store test files (the predecessor task_rules /
// prompt_triggers test files defined this helper inline) so it's
// available to every postgres-package test.
func seedPgMember(t *testing.T, h *pgtest.Harness, orgID, label, role string) string {
	t.Helper()
	userID := uuid.New().String()
	h.SeedAuthUser(t, userID, fmt.Sprintf("%s-%s@test.local", label, userID[:8]))
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, label,
	); err != nil {
		t.Fatalf("seed user %s: %v", label, err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, $3)`,
		orgID, userID, role,
	); err != nil {
		t.Fatalf("seed %s membership: %v", label, err)
	}
	return userID
}

// mustOwnerUserForOrg returns the founder/owner user_id stamped on an
// org row at bootstrap. Tests that need a valid claims principal but
// don't want to fabricate a member use this — the owner always
// exists and passes every policy that checks org access.
func mustOwnerUserForOrg(t *testing.T, h *pgtest.Harness, orgID string) string {
	t.Helper()
	var userID string
	if err := h.AdminDB.QueryRow(
		`SELECT owner_user_id FROM orgs WHERE id = $1`, orgID,
	).Scan(&userID); err != nil {
		t.Fatalf("read owner_user_id for org %s: %v", orgID, err)
	}
	return userID
}

// seedPgDefaultTeam creates a default team for orgID and adds userID as
// an admin member. Required because the team-default
// visibility schema needs every team-scoped row to point at a team, and
// RLS gates team-visible reads on memberships. Per-store test seeders
// that pre-date the team reframe (org + user only) need to call this
// before inserting tasks/runs/event_handlers/prompts/projects with
// visibility='team'. Returns the new team id.
func seedPgDefaultTeam(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	teamID := uuid.New().String()
	if _, err := h.AdminDB.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES ($1, $2, $3, $4)`,
		teamID, orgID, "default-"+teamID[:8], "Default",
	); err != nil {
		t.Fatalf("seed team for org %s: %v", orgID, err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`,
		userID, teamID,
	); err != nil {
		t.Fatalf("seed team membership for %s in %s: %v", userID, teamID, err)
	}
	return teamID
}

// firstTeamForOrg returns the first (by created_at) team id for an org.
// Helper for tests that want to refer to "the org's default team" after
// it's been created via seedPgDefaultTeam (or via the harness's
// seedOrgWithUser path). t.Fatal's on any query error — including
// sql.ErrNoRows — so the caller doesn't need to nil-check.
func firstTeamForOrg(t *testing.T, h *pgtest.Harness, orgID string) string {
	t.Helper()
	var teamID string
	err := h.AdminDB.QueryRow(
		`SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1`, orgID,
	).Scan(&teamID)
	if err != nil {
		t.Fatalf("first team for org %s: %v", orgID, err)
	}
	return teamID
}
