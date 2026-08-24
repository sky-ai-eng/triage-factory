package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestSettingsStores_Postgres runs the shared settings conformance
// suite against the Postgres impl. AdminDB serves both pool slots so
// the conformance suite — which exercises the round-trip contract via
// `...System` reads — bypasses RLS. The RLS gates themselves are pinned
// by the dedicated cross-tenant / admin-vs-member tests below.
func TestSettingsStores_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunSettingsStoresConformance(t, func(t *testing.T) (dbtest.SettingsStores, dbtest.SettingsIDs) {
		t.Helper()
		h.Reset(t)
		orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "settings-conf")
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		return dbtest.SettingsStores{
				MultiMode:        true,
				Orgs:             stores.Orgs,
				Teams:            stores.Teams,
				Users:            stores.Users,
				JiraStatusRules:  stores.JiraStatusRules,
				TeamGitHubGroups: stores.TeamGitHubGroups,
				TeamGitHubRepos:  stores.TeamGitHubRepos,
				OrgEventSources:  stores.OrgEventSources,
			}, dbtest.SettingsIDs{
				OrgID:  orgID,
				TeamID: teamID,
				UserID: userID,
			}
	})
}

// TestOrgsStore_Postgres_GetSettings_IsolatesPerOrg pins the cross-
// tenant guarantee that a request bound to org A must not be able
// to read org B's
// org_settings. The app-pool GetSettings runs under JWT claims
// {sub=user, org_id=A}; org_settings_select RLS requires
// org_id = tf.current_org_id() AND user_has_org_access — so the
// query against org B's row trivially returns no rows.
func TestOrgsStore_Postgres_GetSettings_IsolatesPerOrg(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "iso-a")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "iso-b")

	// Seed a real settings row on orgB so the negative read has
	// something to (fail to) return.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if _, err := stores.Orgs.UpdateSettings(context.Background(), orgB, domain.OrgSettings{
		GitHubBaseURL:       "https://b.example.com",
		GitHubPollInterval:  5 * time.Minute,
		GitHubCloneProtocol: "ssh",
		JiraPollInterval:    5 * time.Minute,
	}); err != nil {
		t.Fatalf("seed orgB settings: %v", err)
	}

	// userA, scoped to orgA, must not see orgB's row. The store
	// returns domain.DefaultOrgSettings() on the underlying
	// sql.ErrNoRows that RLS filtering produces — that's the
	// "no row visible" signal. The cross-tenant
	// concern is "did userA observe orgB's actual configured
	// state?", so probe against orgB's distinctive seeded
	// BaseURL rather than the (default-or-zero) struct shape.
	// If RLS were broken, got.GitHubBaseURL would equal
	// "https://b.example.com"; under correctly-functioning RLS
	// it stays empty (the default fallback's nullable empty).
	err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		got, err := stores.Orgs.GetSettings(context.Background(), orgB)
		if err != nil {
			return err
		}
		if got.GitHubBaseURL == "https://b.example.com" {
			t.Errorf("cross-tenant read leaked orgB's GitHubBaseURL: got %+v (RLS gate broken)", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestOrgsStore_Postgres_UpdateSettings_AdminGated pins the
// admin-vs-member write contract: org_settings_update RLS gates writes
// on tf.user_is_org_admin(). A non-admin member's UPDATE filters every
// row out (RowsAffected=0), and the org_settings_insert WITH CHECK on
// the upsert's INSERT side fails outright with SQLSTATE 42501.
func TestOrgsStore_Postgres_UpdateSettings_AdminGated(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "admin-gate")
	member := pgtest.SeedUser(t, h, "plain-member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")

	// Seed a row as owner so the non-admin update path takes the
	// UPDATE branch (where RLS filters out the row) rather than the
	// INSERT branch (which 42501-errors).
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if _, err := stores.Orgs.UpdateSettings(context.Background(), orgID, domain.OrgSettings{
		GitHubBaseURL:       "https://owner-set.example.com",
		GitHubPollInterval:  5 * time.Minute,
		GitHubCloneProtocol: "ssh",
		JiraPollInterval:    5 * time.Minute,
	}); err != nil {
		t.Fatalf("owner seed UpdateSettings: %v", err)
	}

	// Member attempt: INSERT ... ON CONFLICT DO UPDATE always runs the
	// INSERT-side WITH CHECK first, and org_settings_insert gates on
	// tf.user_is_org_admin(). A non-admin trips the gate with a
	// SQLSTATE 42501 RLS violation. The error aborts the tx, so we
	// have to return it (not swallow it) — h.WithUser then rolls back
	// cleanly and surfaces the error to the test for assertion.
	wantPoll := 5 * time.Minute
	memberErr := h.WithUser(t, member, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		_, err := stores.Orgs.UpdateSettings(context.Background(), orgID, domain.OrgSettings{
			GitHubBaseURL:       "https://member-overwrite.example.com",
			GitHubPollInterval:  9 * time.Minute,
			GitHubCloneProtocol: "ssh",
			JiraPollInterval:    9 * time.Minute,
		})
		return err
	})
	if memberErr == nil {
		t.Fatal("member UpdateSettings succeeded; admin gate broken")
	}
	var pgErr *pgconn.PgError
	if !errors.As(memberErr, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("expected 42501 RLS error, got %v", memberErr)
	}

	// Owner's row must still be intact.
	got, err := stores.Orgs.GetSettingsSystem(context.Background(), orgID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	if got.GitHubBaseURL != "https://owner-set.example.com" {
		t.Errorf("non-admin overwrote org_settings: GitHubBaseURL=%q", got.GitHubBaseURL)
	}
	if got.GitHubPollInterval != wantPoll {
		t.Errorf("non-admin overwrote org_settings: GitHubPollInterval=%v want %v", got.GitHubPollInterval, wantPoll)
	}

	// Owner can update freely — pins the positive side of the gate.
	err = h.WithUser(t, owner, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		_, err := stores.Orgs.UpdateSettings(context.Background(), orgID, domain.OrgSettings{
			GitHubBaseURL:       "https://owner-update.example.com",
			GitHubPollInterval:  7 * time.Minute,
			GitHubCloneProtocol: "ssh",
			JiraPollInterval:    7 * time.Minute,
		})
		return err
	})
	if err != nil {
		t.Fatalf("owner UpdateSettings: %v", err)
	}
	got, err = stores.Orgs.GetSettingsSystem(context.Background(), orgID)
	if err != nil {
		t.Fatalf("GetSettingsSystem post-owner-update: %v", err)
	}
	if got.GitHubBaseURL != "https://owner-update.example.com" {
		t.Errorf("owner update did not land: GitHubBaseURL=%q", got.GitHubBaseURL)
	}
}

// TestJiraStatusRulesStore_Postgres_ReplaceForTeam_TeamAdminGated pins
// the team-admin gate on jira_rules_insert / _update / _delete. A
// plain team member's ReplaceForTeam fails with a 42501 RLS error
// (insert WITH CHECK refuses) and the existing rows survive.
func TestJiraStatusRulesStore_Postgres_ReplaceForTeam_TeamAdminGated(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "rules-admin-gate")
	member := pgtest.SeedUser(t, h, "rules-plain-member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")

	// Owner seeds a baseline rule.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	seed := []domain.JiraProjectStatusRules{{
		ProjectKey:          "SKY",
		PickupMembers:       jiraRefs("To Do"),
		InProgressMembers:   jiraRefs("In Progress"),
		InProgressCanonical: jiraRef("In Progress"),
		DoneMembers:         jiraRefs("Done"),
		DoneCanonical:       jiraRef("Done"),
	}}
	if err := stores.JiraStatusRules.ReplaceForTeam(context.Background(), teamID, seed); err != nil {
		t.Fatalf("owner seed ReplaceForTeam: %v", err)
	}

	// Plain member's ReplaceForTeam must be refused.
	err := h.WithUser(t, member, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		return stores.JiraStatusRules.ReplaceForTeam(context.Background(), teamID, []domain.JiraProjectStatusRules{{
			ProjectKey:          "ENG",
			PickupMembers:       jiraRefs("New"),
			InProgressMembers:   jiraRefs("Doing"),
			InProgressCanonical: jiraRef("Doing"),
			DoneMembers:         jiraRefs("Closed"),
			DoneCanonical:       jiraRef("Closed"),
		}})
	})
	if err == nil {
		t.Fatal("member ReplaceForTeam succeeded; admin gate broken")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("expected 42501 RLS error, got %v", err)
	}

	// Original rule survives.
	got, err := stores.JiraStatusRules.ListForTeamSystem(context.Background(), teamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if len(got) != 1 || got[0].ProjectKey != "SKY" {
		t.Errorf("after refused write, rules=%+v; want one SKY row", got)
	}

	_ = owner // referenced via pgtest.AddOrgMember + RLS context above
}

// TestTeamGitHubGroupsStore_Postgres_SetForTeam_TeamAdminGated pins the
// team-admin gate on team_github_groups_insert / _delete. A plain team
// member's SetForTeam fails with a 42501 RLS error and the existing
// mappings survive; the team admin can write freely.
func TestTeamGitHubGroupsStore_Postgres_SetForTeam_TeamAdminGated(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "gh-groups-admin-gate")
	member := pgtest.SeedUser(t, h, "gh-groups-plain-member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	seed := []domain.TeamGitHubGroup{{OrgLogin: "acme", TeamSlug: "backend"}}
	if err := stores.TeamGitHubGroups.SetForTeam(context.Background(), teamID, seed); err != nil {
		t.Fatalf("owner seed SetForTeam: %v", err)
	}

	// Plain member's SetForTeam must be refused — SetForTeam's
	// delete-then-insert trips team_github_groups_delete (or _insert)
	// WITH CHECK / USING under a non-admin claim.
	err := h.WithUser(t, member, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		return stores.TeamGitHubGroups.SetForTeam(context.Background(), teamID, []domain.TeamGitHubGroup{
			{OrgLogin: "acme", TeamSlug: "frontend"},
		})
	})
	if err == nil {
		t.Fatal("member SetForTeam succeeded; admin gate broken")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("expected 42501 RLS error, got %v", err)
	}

	// Original mapping survives.
	got, err := stores.TeamGitHubGroups.ListForTeamSystem(context.Background(), teamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if len(got) != 1 || got[0].TeamSlug != "backend" {
		t.Errorf("after refused write, groups=%+v; want one backend row", got)
	}

	// Owner can write freely.
	err = h.WithUser(t, owner, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		return stores.TeamGitHubGroups.SetForTeam(context.Background(), teamID, []domain.TeamGitHubGroup{
			{OrgLogin: "acme", TeamSlug: "frontend"},
		})
	})
	if err != nil {
		t.Fatalf("owner SetForTeam: %v", err)
	}
}

// TestTeamGitHubGroupsStore_Postgres_SelectIsTeamScoped pins the
// read-side RLS: a user only sees mappings for teams they belong to.
// Two teams in one org, the caller a member of only one — the app-pool
// ListForTeam returns its own team's rows and nothing for the other.
func TestTeamGitHubGroupsStore_Postgres_SelectIsTeamScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, alice, teamA := pgtest.SeedOrgWithUser(t, h, "gh-groups-scope")
	teamB := pgtest.SeedTeam(t, h, orgID, "team-b")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if err := stores.TeamGitHubGroups.SetForTeam(context.Background(), teamA, []domain.TeamGitHubGroup{
		{OrgLogin: "acme", TeamSlug: "backend"},
	}); err != nil {
		t.Fatalf("seed teamA: %v", err)
	}
	if err := stores.TeamGitHubGroups.SetForTeam(context.Background(), teamB, []domain.TeamGitHubGroup{
		{OrgLogin: "acme", TeamSlug: "frontend"},
	}); err != nil {
		t.Fatalf("seed teamB: %v", err)
	}

	// Alice (member of teamA only) sees teamA's mapping but not teamB's.
	err := h.WithUser(t, alice, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		a, e := stores.TeamGitHubGroups.ListForTeam(context.Background(), teamA)
		if e != nil {
			return e
		}
		if len(a) != 1 || a[0].TeamSlug != "backend" {
			t.Errorf("alice ListForTeam(teamA) = %+v; want one backend row", a)
		}
		b, e := stores.TeamGitHubGroups.ListForTeam(context.Background(), teamB)
		if e != nil {
			return e
		}
		if len(b) != 0 {
			t.Errorf("alice ListForTeam(teamB) = %+v; want empty (cross-team read denied)", b)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestTeamGitHubGroupsStore_Postgres_ManyToOneRouting pins the primary
// "many GitHub teams → one TF team" direction plus the M:N reverse: a
// System routing lookup resolves every TF team that funneled in a given
// GitHub team. Three GitHub teams under teamA all resolve to teamA; one
// of them is also shared with teamB, so that slug resolves to both.
func TestTeamGitHubGroupsStore_Postgres_ManyToOneRouting(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, _, teamA := pgtest.SeedOrgWithUser(t, h, "gh-groups-route")
	teamB := pgtest.SeedTeam(t, h, orgID, "team-b")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if err := stores.TeamGitHubGroups.SetForTeam(context.Background(), teamA, []domain.TeamGitHubGroup{
		{OrgLogin: "acme", TeamSlug: "backend"},
		{OrgLogin: "acme", TeamSlug: "frontend"},
		{OrgLogin: "acme", TeamSlug: "platform"},
	}); err != nil {
		t.Fatalf("seed teamA: %v", err)
	}
	// Shared backlog: platform also funnels into teamB.
	if err := stores.TeamGitHubGroups.SetForTeam(context.Background(), teamB, []domain.TeamGitHubGroup{
		{OrgLogin: "acme", TeamSlug: "platform"},
	}); err != nil {
		t.Fatalf("seed teamB: %v", err)
	}

	ctx := context.Background()
	for _, slug := range []string{"backend", "frontend", "platform"} {
		teams, err := stores.TeamGitHubGroups.TeamsForGroupSystem(ctx, orgID, "acme", slug)
		if err != nil {
			t.Fatalf("TeamsForGroupSystem(%s): %v", slug, err)
		}
		if !contains(teams, teamA) {
			t.Errorf("TeamsForGroupSystem(acme/%s) = %v; want it to include teamA %s", slug, teams, teamA)
		}
	}
	// platform resolves to BOTH teams (shared).
	teams, err := stores.TeamGitHubGroups.TeamsForGroupSystem(ctx, orgID, "acme", "platform")
	if err != nil {
		t.Fatalf("TeamsForGroupSystem(platform): %v", err)
	}
	if len(teams) != 2 || !contains(teams, teamA) || !contains(teams, teamB) {
		t.Errorf("TeamsForGroupSystem(acme/platform) = %v; want both teamA %s and teamB %s", teams, teamA, teamB)
	}
}

// TestTeamGitHubGroupsStore_Postgres_PruneMissingSystem_DeletionLifecycle
// pins the GitHub-team-deletion reconcile floor across teams: when a
// GitHub team disappears from the live set, its mapping rows are removed
// from every TF team that referenced it, while the TF teams themselves
// and their other mappings survive.
func TestTeamGitHubGroupsStore_Postgres_PruneMissingSystem_DeletionLifecycle(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, _, teamA := pgtest.SeedOrgWithUser(t, h, "gh-groups-prune")
	teamB := pgtest.SeedTeam(t, h, orgID, "team-b")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	// Both teams map acme/legacy (the soon-deleted team); each also keeps
	// a distinct survivor mapping.
	if err := stores.TeamGitHubGroups.SetForTeam(ctx, teamA, []domain.TeamGitHubGroup{
		{OrgLogin: "acme", TeamSlug: "legacy"},
		{OrgLogin: "acme", TeamSlug: "backend"},
	}); err != nil {
		t.Fatalf("seed teamA: %v", err)
	}
	if err := stores.TeamGitHubGroups.SetForTeam(ctx, teamB, []domain.TeamGitHubGroup{
		{OrgLogin: "acme", TeamSlug: "legacy"},
		{OrgLogin: "acme", TeamSlug: "frontend"},
	}); err != nil {
		t.Fatalf("seed teamB: %v", err)
	}

	// GitHub now reports only backend + frontend (legacy was deleted).
	n, err := stores.TeamGitHubGroups.PruneMissingSystem(ctx, orgID, "acme", []string{"backend", "frontend"})
	if err != nil {
		t.Fatalf("PruneMissingSystem: %v", err)
	}
	if n != 2 {
		t.Errorf("PruneMissingSystem removed %d rows; want 2 (legacy from both teams)", n)
	}

	a, err := stores.TeamGitHubGroups.ListForTeamSystem(ctx, teamA)
	if err != nil {
		t.Fatalf("ListForTeamSystem(teamA): %v", err)
	}
	if len(a) != 1 || a[0].TeamSlug != "backend" {
		t.Errorf("teamA after prune = %+v; want only backend", a)
	}
	b, err := stores.TeamGitHubGroups.ListForTeamSystem(ctx, teamB)
	if err != nil {
		t.Fatalf("ListForTeamSystem(teamB): %v", err)
	}
	if len(b) != 1 || b[0].TeamSlug != "frontend" {
		t.Errorf("teamB after prune = %+v; want only frontend", b)
	}

	// The TF teams themselves are untouched.
	var teamCount int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM teams WHERE org_id = $1`, orgID).Scan(&teamCount); err != nil {
		t.Fatalf("count teams: %v", err)
	}
	if teamCount != 2 {
		t.Errorf("team count = %d after prune; want 2 (prune must never delete TF teams)", teamCount)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
