package postgres_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestJiraProjectStore_Postgres runs the shared registry suite with AdminDB in
// both pool slots, so it pins the store's behavior independent of RLS.
func TestJiraProjectStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunJiraProjectConformance(t, func(t *testing.T) dbtest.JiraProjectFixture {
		t.Helper()
		h.Reset(t)
		orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "jira-projects-conf")
		otherTeamID := pgtest.SeedTeam(t, h, orgID, "other")
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		return dbtest.JiraProjectFixture{
			Projects:    stores.JiraProjects,
			Rules:       stores.JiraStatusRules,
			Teams:       stores.Teams,
			Entities:    stores.Entities,
			OrgID:       orgID,
			TeamID:      teamID,
			OtherTeamID: otherTeamID,
			SeedUnresolved: func(t *testing.T, site, key string) string {
				t.Helper()
				var id string
				if err := h.AdminDB.QueryRow(
					`INSERT INTO jira_projects (org_id, site, project_key) VALUES ($1, $2, $3) RETURNING id`,
					orgID, site, key,
				).Scan(&id); err != nil {
					t.Fatalf("seed unresolved jira project: %v", err)
				}
				return id
			},
			SeedIssue: func(t *testing.T, site, key, jiraProjectID string) string {
				t.Helper()
				var id string
				if err := h.AdminDB.QueryRow(
					`INSERT INTO entities (org_id, source, scope, source_id, kind, jira_project_id) VALUES ($1, 'jira', $2, $3, 'issue', $4) RETURNING id`,
					orgID, site, key, jiraProjectID,
				).Scan(&id); err != nil {
					t.Fatalf("seed jira issue: %v", err)
				}
				return id
			},
		}
	})
}

// TestJiraProjectStore_Postgres_FindOrCreateUnderClaims runs the settings
// write's registry door the way the handler does — inside a claims-carrying
// transaction on the app role — through both of its arms: the insert, and the
// conflict arm that refreshes the key. Each returns the row it stored, which on
// the conflict arm depends on jira_projects_all admitting the read-back; the
// conformance run above cannot see that, since it rides the RLS-bypassing
// admin connection. A member of another org then sees none of it.
func TestJiraProjectStore_Postgres_FindOrCreateUnderClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, adminID, teamID := pgtest.SeedOrgWithUser(t, h, "jira-projects-rls")
	otherOrg, outsider, _ := pgtest.SeedOrgWithUser(t, h, "jira-projects-rls-outsider")
	const site = "https://acme.atlassian.net"
	admin := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).JiraProjects

	var created domain.JiraProject
	if err := h.WithUser(t, adminID, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		var err error
		if created, err = stores.JiraProjects.FindOrCreate(ctx, orgID, site, "10000", "SKY", "Skyline"); err != nil {
			return err
		}
		rule := domain.JiraProjectStatusRules{JiraProjectID: created.ID}
		return stores.JiraStatusRules.ReplaceForTeam(ctx, teamID, site, []domain.JiraProjectStatusRules{rule})
	}); err != nil {
		t.Fatalf("FindOrCreate (insert) under claims: %v", err)
	}
	dbtest.AssertWriteReturnedStoredRow(t, "FindOrCreate (insert, claims)", created, func() (*domain.JiraProject, error) {
		return admin.GetSystem(ctx, orgID, created.ID)
	})

	var renamed domain.JiraProject
	if err := h.WithUser(t, adminID, orgID, func(tx *sql.Tx) error {
		var err error
		renamed, err = pgstore.NewForTx(tx, pgtest.SecretKey).JiraProjects.FindOrCreate(ctx, orgID, site, "10000", "SKYL", "Skyline")
		return err
	}); err != nil {
		t.Fatalf("FindOrCreate (conflict) under claims: %v", err)
	}
	if renamed.ID != created.ID || renamed.Key != "SKYL" {
		t.Errorf("FindOrCreate's conflict arm = %+v, want row %s keyed SKYL", renamed, created.ID)
	}
	dbtest.AssertWriteReturnedStoredRow(t, "FindOrCreate (conflict, claims)", renamed, func() (*domain.JiraProject, error) {
		return admin.GetSystem(ctx, orgID, created.ID)
	})

	if err := h.WithUser(t, outsider, otherOrg, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM jira_projects WHERE id = $1`, created.ID).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("a member of another org read the registry row")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser(outsider): %v", err)
	}
}
