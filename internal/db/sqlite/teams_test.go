package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func strptr(s string) *string { return &s }

// TestTeamsStore_SQLite_GetDefaultForOrgSystem_ReturnsSentinelTeam
// pins local-mode behavior: the seeded "default" team for the
// LocalDefaultOrgID sentinel is the resolved row, regardless of how
// many handlers call this lookup. Handler-side sites that previously
// hardcoded runmode.LocalDefaultTeamID now route through this helper.
func TestTeamsStore_SQLite_GetDefaultForOrgSystem_ReturnsSentinelTeam(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	got, err := stores.Teams.GetDefaultForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetDefaultForOrgSystem: %v", err)
	}
	if got != runmode.LocalDefaultTeamID {
		t.Errorf("GetDefaultForOrgSystem = %q; want %q (seeded default team)", got, runmode.LocalDefaultTeamID)
	}
}

// TestTeamsStore_SQLite_GetDefaultForOrgSystem_NoTeamsReturnsEmpty
// — an org with zero teams returns ("", nil) rather than an error.
// Callers treat the empty string as a bootstrap bug (handlers in
// projects/stock/factory_delegate surface a 500 rather than mint a
// wrong-team row).
func TestTeamsStore_SQLite_GetDefaultForOrgSystem_NoTeamsReturnsEmpty(t *testing.T) {
	conn := openSQLiteForTest(t)
	if _, err := conn.Exec(
		`INSERT INTO orgs (id, slug, name) VALUES ($1, $2, $3)`,
		"00000000-0000-0000-0000-000000000200", "teamless", "Teamless",
	); err != nil {
		t.Fatalf("seed extra org: %v", err)
	}
	stores := sqlitestore.New(conn)
	got, err := stores.Teams.GetDefaultForOrgSystem(context.Background(), "00000000-0000-0000-0000-000000000200")
	if err != nil {
		t.Fatalf("GetDefaultForOrgSystem: %v", err)
	}
	if got != "" {
		t.Errorf("GetDefaultForOrgSystem on teamless org = %q; want empty string", got)
	}
}

// TestTeamsStore_SQLite_GetDefaultForOrgSystem_OldestWins pins the
// ordering rule: when multiple teams exist for an org, the oldest
// (by created_at) wins. This is the "single-team-per-org" assumption
// safety net — even if a future fixture seeds extra teams, the
// resolved default stays deterministic.
func TestTeamsStore_SQLite_GetDefaultForOrgSystem_OldestWins(t *testing.T) {
	conn := openSQLiteForTest(t)
	if _, err := conn.Exec(
		`INSERT INTO orgs (id, slug, name) VALUES ($1, $2, $3)`,
		"00000000-0000-0000-0000-000000000200", "multi-team", "Multi-Team",
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO teams (id, org_id, slug, name, created_at)
		VALUES ($1, $2, 'older', 'older', '2026-01-01 00:00:00')
	`, "team-older", "00000000-0000-0000-0000-000000000200"); err != nil {
		t.Fatalf("seed older team: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO teams (id, org_id, slug, name, created_at)
		VALUES ($1, $2, 'newer', 'newer', '2026-06-01 00:00:00')
	`, "team-newer", "00000000-0000-0000-0000-000000000200"); err != nil {
		t.Fatalf("seed newer team: %v", err)
	}
	stores := sqlitestore.New(conn)
	got, err := stores.Teams.GetDefaultForOrgSystem(context.Background(), "00000000-0000-0000-0000-000000000200")
	if err != nil {
		t.Fatalf("GetDefaultForOrgSystem: %v", err)
	}
	if got != "team-older" {
		t.Errorf("GetDefaultForOrgSystem = %q; want \"team-older\" (oldest by created_at)", got)
	}
}

// seedSQLiteTeam inserts an org + a single team and returns the team id. The
// Update tests below mutate that row and read it back.
func seedSQLiteTeam(t *testing.T, conn *sql.DB) (orgID, teamID string) {
	t.Helper()
	orgID = "00000000-0000-0000-0000-000000000300"
	teamID = "team-update-target"
	if _, err := conn.Exec(
		`INSERT INTO orgs (id, slug, name) VALUES ($1, $2, $3)`,
		orgID, "rename-org", "Rename Org",
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES ($1, $2, $3, $4)`,
		teamID, orgID, "platform", "Platform",
	); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	return orgID, teamID
}

// TestTeamsStore_SQLite_Update_RenamesAndSetsDescription: a full rename — name,
// slug, and description all supplied — round-trips through the returned row and
// the persisted columns.
func TestTeamsStore_SQLite_Update_RenamesAndSetsDescription(t *testing.T) {
	conn := openSQLiteForTest(t)
	_, teamID := seedSQLiteTeam(t, conn)
	stores := sqlitestore.New(conn)

	got, err := stores.Teams.Update(context.Background(), teamID,
		strptr("Platform Eng"), strptr("platform-eng"), strptr("Owns the build"))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Name != "Platform Eng" || got.Slug != "platform-eng" || got.Description != "Owns the build" {
		t.Errorf("returned team = %+v; want name/slug/description updated", got)
	}

	var name, slug, desc string
	if err := conn.QueryRow(
		`SELECT name, slug, COALESCE(description, '') FROM teams WHERE id = ?`, teamID,
	).Scan(&name, &slug, &desc); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "Platform Eng" || slug != "platform-eng" || desc != "Owns the build" {
		t.Errorf("persisted = (%q, %q, %q); want (Platform Eng, platform-eng, Owns the build)", name, slug, desc)
	}
}

// TestTeamsStore_SQLite_Update_DescriptionOnly: a nil name leaves name + slug
// untouched (the partial-PATCH contract) while the description is rewritten.
func TestTeamsStore_SQLite_Update_DescriptionOnly(t *testing.T) {
	conn := openSQLiteForTest(t)
	_, teamID := seedSQLiteTeam(t, conn)
	stores := sqlitestore.New(conn)

	got, err := stores.Teams.Update(context.Background(), teamID, nil, nil, strptr("just a blurb"))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Name != "Platform" || got.Slug != "platform" {
		t.Errorf("name/slug changed on description-only update: %+v", got)
	}
	if got.Description != "just a blurb" {
		t.Errorf("description = %q; want %q", got.Description, "just a blurb")
	}
}

// TestTeamsStore_SQLite_Update_NotFound: a bogus team id matches no row and
// surfaces db.ErrTeamNotFound (the handler maps it to a 404) rather than a
// silent success.
func TestTeamsStore_SQLite_Update_NotFound(t *testing.T) {
	conn := openSQLiteForTest(t)
	seedSQLiteTeam(t, conn)
	stores := sqlitestore.New(conn)

	_, err := stores.Teams.Update(context.Background(), "no-such-team", strptr("X"), strptr("x"), nil)
	if !errors.Is(err, db.ErrTeamNotFound) {
		t.Fatalf("Update on missing team = %v; want ErrTeamNotFound", err)
	}
}

// --- Archive / restore soft-delete (TFAC-448) ---

// TestTeamsStore_SQLite_Archive_HidesFromReadsButResolvesViaSystem pins the
// core soft-delete read filtering: archive stamps deleted_at, ListForUser stops
// returning the team (the request-facing filter), but GetSystem still resolves
// it with DeletedAt populated (the lifecycle path).
func TestTeamsStore_SQLite_Archive_HidesFromReadsButResolvesViaSystem(t *testing.T) {
	conn := openSQLiteForTest(t)
	orgID, teamID := seedSQLiteTeam(t, conn)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	// Visible before archive.
	teams, err := stores.Teams.ListForUser(ctx, orgID)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if !containsTeam(teams, teamID) {
		t.Fatalf("team %s not in ListForUser before archive", teamID)
	}

	if err := stores.Teams.Archive(ctx, teamID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Gone from the request-facing read.
	teams, err = stores.Teams.ListForUser(ctx, orgID)
	if err != nil {
		t.Fatalf("ListForUser after archive: %v", err)
	}
	if containsTeam(teams, teamID) {
		t.Errorf("archived team %s still in ListForUser", teamID)
	}

	// Still resolvable via the System read, with DeletedAt set.
	got, err := stores.Teams.GetSystem(ctx, orgID, teamID)
	if err != nil {
		t.Fatalf("GetSystem: %v", err)
	}
	if got == nil {
		t.Fatalf("GetSystem returned nil for archived team")
	}
	if got.DeletedAt == nil {
		t.Errorf("GetSystem.DeletedAt is nil for archived team; want non-nil")
	}
}

// TestTeamsStore_SQLite_Archive_AlreadyArchivedIsNotFound: re-archiving an
// already-archived team affects zero rows → ErrTeamNotFound (the handler maps it
// to 409 "already archived").
func TestTeamsStore_SQLite_Archive_AlreadyArchivedIsNotFound(t *testing.T) {
	conn := openSQLiteForTest(t)
	_, teamID := seedSQLiteTeam(t, conn)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	if err := stores.Teams.Archive(ctx, teamID); err != nil {
		t.Fatalf("first Archive: %v", err)
	}
	if err := stores.Teams.Archive(ctx, teamID); !errors.Is(err, db.ErrTeamNotFound) {
		t.Fatalf("second Archive = %v; want ErrTeamNotFound", err)
	}
}

// TestTeamsStore_SQLite_Restore_RevivesTeam: restore clears deleted_at, the team
// returns to ListForUser, and GetSystem.DeletedAt goes back to nil.
func TestTeamsStore_SQLite_Restore_RevivesTeam(t *testing.T) {
	conn := openSQLiteForTest(t)
	orgID, teamID := seedSQLiteTeam(t, conn)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	if err := stores.Teams.Archive(ctx, teamID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := stores.Teams.Restore(ctx, teamID); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	teams, err := stores.Teams.ListForUser(ctx, orgID)
	if err != nil {
		t.Fatalf("ListForUser after restore: %v", err)
	}
	if !containsTeam(teams, teamID) {
		t.Errorf("restored team %s not back in ListForUser", teamID)
	}
	got, err := stores.Teams.GetSystem(ctx, orgID, teamID)
	if err != nil {
		t.Fatalf("GetSystem: %v", err)
	}
	if got == nil || got.DeletedAt != nil {
		t.Errorf("GetSystem after restore = %+v; want non-nil row with DeletedAt nil", got)
	}
}

// TestTeamsStore_SQLite_Restore_NotArchivedIsNotFound: restoring an active team
// affects zero rows → ErrTeamNotFound (the handler maps it to 409 "not archived").
func TestTeamsStore_SQLite_Restore_NotArchivedIsNotFound(t *testing.T) {
	conn := openSQLiteForTest(t)
	_, teamID := seedSQLiteTeam(t, conn)
	stores := sqlitestore.New(conn)

	if err := stores.Teams.Restore(context.Background(), teamID); !errors.Is(err, db.ErrTeamNotFound) {
		t.Fatalf("Restore on active team = %v; want ErrTeamNotFound", err)
	}
}

// TestTeamsStore_SQLite_GetDefaultForOrg_SkipsArchived: an archived team is never
// the org's default — the default-team pick filters deleted_at IS NULL.
func TestTeamsStore_SQLite_GetDefaultForOrg_SkipsArchived(t *testing.T) {
	conn := openSQLiteForTest(t)
	orgID, teamID := seedSQLiteTeam(t, conn)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	if got, _ := stores.Teams.GetDefaultForOrg(ctx, orgID); got != teamID {
		t.Fatalf("default before archive = %q; want %q", got, teamID)
	}
	if err := stores.Teams.Archive(ctx, teamID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if got, _ := stores.Teams.GetDefaultForOrg(ctx, orgID); got != "" {
		t.Errorf("default after archiving sole team = %q; want \"\"", got)
	}
}

// TestTeamsStore_SQLite_ListArchivedForOrgSystem: returns only archived teams in
// the org, with DeletedAt populated.
func TestTeamsStore_SQLite_ListArchivedForOrgSystem(t *testing.T) {
	conn := openSQLiteForTest(t)
	orgID, teamID := seedSQLiteTeam(t, conn)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	// A second, active team in the same org — must NOT appear in the archived list.
	if _, err := conn.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'live', 'Live')`,
		"team-live", orgID,
	); err != nil {
		t.Fatalf("seed live team: %v", err)
	}

	if got, err := stores.Teams.ListArchivedForOrgSystem(ctx, orgID); err != nil || len(got) != 0 {
		t.Fatalf("ListArchivedForOrgSystem before archive = %v, %v; want empty", got, err)
	}
	if err := stores.Teams.Archive(ctx, teamID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	got, err := stores.Teams.ListArchivedForOrgSystem(ctx, orgID)
	if err != nil {
		t.Fatalf("ListArchivedForOrgSystem: %v", err)
	}
	if len(got) != 1 || got[0].ID != teamID {
		t.Fatalf("ListArchivedForOrgSystem = %+v; want exactly the archived team %s", got, teamID)
	}
	if got[0].DeletedAt == nil {
		t.Errorf("archived team DeletedAt is nil; want non-nil")
	}
}

func containsTeam(teams []domain.Team, id string) bool {
	for _, t := range teams {
		if t.ID == id {
			return true
		}
	}
	return false
}
