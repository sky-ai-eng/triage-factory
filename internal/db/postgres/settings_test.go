package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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
		// The suite starts from an org with no settings of its own.
		pgtest.ClearOrgGitHubURL(t, h, orgID)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		return dbtest.SettingsStores{
				MultiMode:        true,
				Orgs:             stores.Orgs,
				Teams:            stores.Teams,
				Users:            stores.Users,
				JiraStatusRules:  stores.JiraStatusRules,
				JiraProjects:     stores.JiraProjects,
				TeamGitHubGroups: stores.TeamGitHubGroups,
				TeamGitHubRepos:  stores.TeamGitHubRepos,
				Repos:            stores.Repos,
				OrgEventSources:  stores.OrgEventSources,
				SeedTeam:         func(t *testing.T, slug string) string { return pgtest.SeedTeam(t, h, orgID, slug) },
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
	dbtest.SeedOrgSettings(t, stores.Orgs, orgB, domain.OrgSettings{
		GitHubBaseURL:       "https://b.example.com",
		GitHubPollInterval:  5 * time.Minute,
		GitHubCloneProtocol: "ssh",
		JiraPollInterval:    5 * time.Minute,
	})

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

// TestOrgsStore_Postgres_UpdateSettingsVersioned_AdminGated pins the
// admin-vs-member write contract on both arms of the settings writer. A
// non-admin's create is an INSERT, which the org_settings_insert WITH CHECK
// (tf.user_is_org_admin()) fails outright with SQLSTATE 42501. A non-admin's
// update is an UPDATE whose rows org_settings_update's USING clause filters
// out, so it matches nothing and reports the ErrOrgSettingsVersion a moved
// version would. Neither writes anything.
func TestOrgsStore_Postgres_UpdateSettingsVersioned_AdminGated(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "admin-gate")
	member := pgtest.SeedUser(t, h, "plain-member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")

	// save is the settings handler's shape: read the row and write it back at
	// the version read, in one claims-bound transaction. The error is returned
	// rather than swallowed — a 42501 aborts the tx, and h.WithUser rolls back
	// and surfaces it.
	save := func(userID, baseURL string) error {
		return h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			stores := pgstore.NewForTx(tx, pgtest.SecretKey)
			cur, err := stores.Orgs.GetSettings(ctx, orgID)
			if err != nil {
				return err
			}
			cur.GitHubBaseURL = baseURL
			_, err = stores.Orgs.UpdateSettingsVersioned(ctx, orgID, cur, cur.Version)
			return err
		})
	}
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	stored := func() domain.OrgSettings {
		t.Helper()
		got, err := stores.Orgs.GetSettingsSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		return got
	}

	// No row yet: the member reads version 0, so its write is the create arm.
	err := save(member, "https://member-create.example.com")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("member create: expected 42501 RLS error, got %v", err)
	}
	if got := stored(); got.Version != 0 {
		t.Fatalf("member create materialized a row: %+v", got)
	}

	if err := save(owner, "https://owner-set.example.com"); err != nil {
		t.Fatalf("owner create: %v", err)
	}

	// The member reads the owner's row at version 1, and its UPDATE at that
	// version matches nothing.
	if err := save(member, "https://member-overwrite.example.com"); !errors.Is(err, db.ErrOrgSettingsVersion) {
		t.Fatalf("member update: err = %v, want ErrOrgSettingsVersion", err)
	}
	if got := stored(); got.GitHubBaseURL != "https://owner-set.example.com" || got.Version != 1 {
		t.Errorf("non-admin overwrote org_settings: GitHubBaseURL=%q version=%d", got.GitHubBaseURL, got.Version)
	}

	// The owner's update lands — the positive side of the gate.
	if err := save(owner, "https://owner-update.example.com"); err != nil {
		t.Fatalf("owner update: %v", err)
	}
	if got := stored(); got.GitHubBaseURL != "https://owner-update.example.com" || got.Version != 2 {
		t.Errorf("owner update did not land: GitHubBaseURL=%q version=%d", got.GitHubBaseURL, got.Version)
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
	const site = "https://acme.atlassian.net"
	sky, err := stores.JiraProjects.FindOrCreate(context.Background(), orgID, site, "10000", "SKY", "")
	if err != nil {
		t.Fatalf("register SKY: %v", err)
	}
	eng, err := stores.JiraProjects.FindOrCreate(context.Background(), orgID, site, "10001", "ENG", "")
	if err != nil {
		t.Fatalf("register ENG: %v", err)
	}
	seed := []domain.JiraProjectStatusRules{{
		JiraProjectID:       sky.ID,
		PickupMembers:       jiraRefs("To Do"),
		InProgressMembers:   jiraRefs("In Progress"),
		InProgressCanonical: jiraRef("In Progress"),
		DoneMembers:         jiraRefs("Done"),
		DoneCanonical:       jiraRef("Done"),
	}}
	if err := stores.JiraStatusRules.ReplaceForTeam(context.Background(), teamID, site, seed); err != nil {
		t.Fatalf("owner seed ReplaceForTeam: %v", err)
	}

	// Plain member's ReplaceForTeam must be refused.
	err = h.WithUser(t, member, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		return stores.JiraStatusRules.ReplaceForTeam(context.Background(), teamID, site, []domain.JiraProjectStatusRules{{
			JiraProjectID:       eng.ID,
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
	got, err := stores.JiraStatusRules.ListForTeamSystem(context.Background(), teamID, site)
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
	seed := []domain.TeamGitHubGroup{{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "backend"}}
	if err := stores.TeamGitHubGroups.SetForTeam(context.Background(), teamID, dbtest.TestGitHubHost, seed); err != nil {
		t.Fatalf("owner seed SetForTeam: %v", err)
	}

	// Plain member's SetForTeam must be refused — SetForTeam's
	// delete-then-insert trips team_github_groups_delete (or _insert)
	// WITH CHECK / USING under a non-admin claim.
	err := h.WithUser(t, member, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		return stores.TeamGitHubGroups.SetForTeam(context.Background(), teamID, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
			{GitHubTeamID: "12", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "frontend"},
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
	got, err := stores.TeamGitHubGroups.ListForTeam(context.Background(), teamID, dbtest.TestGitHubHost)
	if err != nil {
		t.Fatalf("ListForTeam: %v", err)
	}
	if len(got) != 1 || got[0].TeamSlug != "backend" {
		t.Errorf("after refused write, groups=%+v; want one backend row", got)
	}

	// Owner can write freely.
	err = h.WithUser(t, owner, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		return stores.TeamGitHubGroups.SetForTeam(context.Background(), teamID, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
			{GitHubTeamID: "12", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "frontend"},
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
	if err := stores.TeamGitHubGroups.SetForTeam(context.Background(), teamA, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "backend"},
	}); err != nil {
		t.Fatalf("seed teamA: %v", err)
	}
	if err := stores.TeamGitHubGroups.SetForTeam(context.Background(), teamB, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		{GitHubTeamID: "12", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "frontend"},
	}); err != nil {
		t.Fatalf("seed teamB: %v", err)
	}

	// Alice (member of teamA only) sees teamA's mapping but not teamB's.
	err := h.WithUser(t, alice, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		a, e := stores.TeamGitHubGroups.ListForTeam(context.Background(), teamA, dbtest.TestGitHubHost)
		if e != nil {
			return e
		}
		if len(a) != 1 || a[0].TeamSlug != "backend" {
			t.Errorf("alice ListForTeam(teamA) = %+v; want one backend row", a)
		}
		b, e := stores.TeamGitHubGroups.ListForTeam(context.Background(), teamB, dbtest.TestGitHubHost)
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
