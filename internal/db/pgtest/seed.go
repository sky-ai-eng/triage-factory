package pgtest

import (
	"database/sql"
	"testing"
)

// Shared seed helpers used by both the pgtest baseline suite and the
// per-store postgres_test files. Extracted from baseline_test.go so the
// postgres_test package can call them without duplicating the SQL.
//
// All helpers go through h.AdminDB (BYPASSRLS) — these are fixture
// inserts, not the thing under test. Test-time RLS exercise happens via
// h.WithUser and the app pool.

// SeedUser inserts a row in auth.users + public.users and returns the
// new user ID. displayName is also used to derive the email
// ("<displayName>@test").
func SeedUser(t *testing.T, h *Harness, displayName string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`SELECT gen_random_uuid()`).Scan(&id); err != nil {
		t.Fatalf("gen uuid: %v", err)
	}
	h.SeedAuthUser(t, id, displayName+"@test")
	MustExec(t, h.AdminDB,
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`, id, displayName)
	return id
}

// SeedOrg inserts a row in orgs with the given slug (also used as name)
// and owner, returning the new org ID. ownerID must already exist in
// users (see SeedUser).
func SeedOrg(t *testing.T, h *Harness, slug, ownerID string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO orgs (slug, name, owner_user_id) VALUES ($1, $1, $2) RETURNING id
	`, slug, ownerID).Scan(&id); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	return id
}

// SeedTeam inserts a row in teams with the given slug (also used as
// name), returning the new team ID. orgID must already exist.
func SeedTeam(t *testing.T, h *Harness, orgID, slug string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO teams (org_id, slug, name) VALUES ($1, $2, $2) RETURNING id
	`, orgID, slug).Scan(&id); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	return id
}

// SeedRepository mints a registry row for owner/repo in orgID and returns its
// id — the value every repository reference in the schema stores.
// team_github_repos and conversation_worktrees both
// point at it, and none of the stores behind them will create it (the
// executor's role holds no INSERT on repositories at all), so a fixture that
// tracks a repository or checks one out has to bring the row into existence
// first, exactly as production tracking does. Idempotent on the folded
// identity key, so repeated calls for one repository return its single row.
func SeedRepository(t *testing.T, h *Harness, orgID, owner, repo string) string {
	t.Helper()
	MustExec(t, h.AdminDB, `
		INSERT INTO repositories (org_id, source, owner, repo) VALUES ($1, 'github', $2, $3)
		ON CONFLICT DO NOTHING
	`, orgID, owner, repo)
	var id string
	if err := h.AdminDB.QueryRow(`
		SELECT id::text FROM repositories
		 WHERE org_id = $1 AND source = 'github'
		   AND lower(owner) = lower($2) AND lower(repo) = lower($3)
	`, orgID, owner, repo).Scan(&id); err != nil {
		t.Fatalf("seed repository %s/%s: %v", owner, repo, err)
	}
	return id
}

// SeedTrackedRepo records that teamID tracks owner/repo, minting the registry
// row the tracking row references. Returns the repository id.
func SeedTrackedRepo(t *testing.T, h *Harness, orgID, teamID, owner, repo string) string {
	t.Helper()
	repositoryID := SeedRepository(t, h, orgID, owner, repo)
	MustExec(t, h.AdminDB, `
		INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING
	`, teamID, repositoryID, orgID)
	return repositoryID
}

// SeedOrgWithUser bootstraps a new org with a single founder: a fresh
// user, an org owned by them, a default team, and the two-axis
// memberships (org_memberships role=owner, memberships role=admin).
// Returns (orgID, userID, teamID). This is the canonical "give me a
// fully wired org" helper.
func SeedOrgWithUser(t *testing.T, h *Harness, displayName string) (orgID, userID, teamID string) {
	t.Helper()
	userID = SeedUser(t, h, displayName)
	orgID = SeedOrg(t, h, displayName+"-org", userID)
	teamID = SeedTeam(t, h, orgID, "default")
	// Two-axis roles (matches GitHub/GitLab/Linear): the founder is
	// org-level 'owner' (via org_memberships) AND team-level 'admin'
	// (via memberships). membership_role no longer has 'owner';
	// owning is an org concept now.
	MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')`, userID, orgID)
	MustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`, userID, teamID)
	return
}

// SeedIdentity inserts a public.user_identities row linking a fresh
// auth.users login identity to principalID, carrying email with the given
// verified flag. principalID must already exist in users (see SeedUser) —
// a principal can hold more than one login identity (auth_user_id is the
// table's PK, so each call mints its own fresh auth.users row), which is
// how a test pins a specific email/verified combination against an
// existing user (e.g. an unverified address, or a second verified email
// shared with another principal to exercise ambiguous-match handling).
// provider is always "github"; callers that need a different provider
// value seed the row directly.
func SeedIdentity(t *testing.T, h *Harness, principalID, email string, verified bool) (authUserID string) {
	t.Helper()
	if err := h.AdminDB.QueryRow(`SELECT gen_random_uuid()`).Scan(&authUserID); err != nil {
		t.Fatalf("gen auth_user_id: %v", err)
	}
	h.SeedAuthUser(t, authUserID, authUserID+"@auth.test")
	MustExec(t, h.AdminDB, `
		INSERT INTO user_identities (auth_user_id, user_id, provider, email, email_verified)
		VALUES ($1, $2, 'github', $3, $4)
	`, authUserID, principalID, email, verified)
	return authUserID
}

// AddOrgMember adds an existing user to an existing org with the given
// org_role and the given team_role on the named team. Models the
// production "admin invites a user" flow which materializes both rows.
func AddOrgMember(t *testing.T, h *Harness, userID, orgID, teamID, orgRole, teamRole string) {
	t.Helper()
	MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, $3)`, userID, orgID, orgRole)
	MustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, $3)`, userID, teamID, teamRole)
}

// MustExec runs an Exec and t.Fatalf's on error. Sugar for fixture
// seeding where every insert is expected to succeed.
func MustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
