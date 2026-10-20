// Package dbseed creates the rows a raw-SQL test fixture needs before it can
// write a row that references them. It is the one helper every fixture seeding
// a GitHub entity by hand goes through, in either dialect: a GitHub entity row
// references its repository's registry row (entities.repository_id, required),
// so the registry row has to exist first and the INSERT has to name it. The
// same holds for a Jira rule, which references its project's registry row
// (jira_project_status_rules.jira_project_id).
//
// It also renames a registry row the way no production path does
// (RenameRepositoryOnly), which is how a test proves a reader resolves a
// repository by id.
//
// It imports nothing from internal/db, so package db's own tests can use it.
package dbseed

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Conn is a SQLite or Postgres connection, pool or transaction. The statements
// below are written to run on both: $N placeholders, which SQLite binds by
// number, and CAST rather than ::.
type Conn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// GitHubRepository returns the id of the registry row for owner/repo on host
// in orgID, creating a bare row (identity columns only) when none answers to
// the name. The match folds case on owner/repo, as the store's get-or-create
// does, so seeding a repository a fixture already seeded returns its row. On
// Postgres conn has to be one that may write the org's rows (the harness's
// admin connection).
func GitHubRepository(t testing.TB, conn Conn, orgID, host, owner, repo string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO repositories (id, org_id, source, host, owner, repo)
		VALUES ($1, $2, 'github', $3, $4, $5)
		ON CONFLICT DO NOTHING
	`, uuid.NewString(), orgID, host, owner, repo); err != nil {
		t.Fatalf("dbseed: create repository %s/%s on %s: %v", owner, repo, host, err)
	}
	var id string
	if err := conn.QueryRowContext(ctx, `
		SELECT CAST(id AS TEXT) FROM repositories
		 WHERE org_id = $1 AND source = 'github' AND host = $2
		   AND lower(owner) = lower($3) AND lower(repo) = lower($4)
	`, orgID, host, owner, repo).Scan(&id); err != nil {
		t.Fatalf("dbseed: read repository %s/%s on %s: %v", owner, repo, host, err)
	}
	return id
}

// GitHubEntityRepository is GitHubRepository for the repository a GitHub
// entity key names: the owner/repo of "owner/repo#N", on host (the entity's
// scope). It stands in for the entity create, the boundary where a key's name
// is resolved to a row (db.GitHubEntityRepoRef), and returns the id the
// fixture's INSERT INTO entities sets as repository_id. A key with no
// owner/repo fails the test.
func GitHubEntityRepository(t testing.TB, conn Conn, orgID, host, sourceID string) string {
	t.Helper()
	owner, repo, _ := domain.SplitGitHubEntitySourceID(sourceID)
	if owner == "" || repo == "" {
		t.Fatalf("dbseed: github entity key %q names no owner/repo", sourceID)
	}
	return GitHubRepository(t, conn, orgID, host, owner, repo)
}

// RenameRepositoryOnly renames the registry row id to owner/repo and moves
// nothing else. RepositoryStore.RenameSystem also rewrites every key that
// spells the old name, which is what a reader parsing a key would need; after
// this, an entity's key still spells the old name and only its repository_id
// reaches the new one. A reader that answers for the renamed repository
// afterwards resolved it by id.
func RenameRepositoryOnly(t testing.TB, conn Conn, id, owner, repo string) {
	t.Helper()
	res, err := conn.ExecContext(context.Background(),
		`UPDATE repositories SET owner = $1, repo = $2 WHERE id = $3`, owner, repo, id)
	if err != nil {
		t.Fatalf("dbseed: rename repository %s to %s/%s: %v", id, owner, repo, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("dbseed: rename repository %s to %s/%s: %d rows, %v", id, owner, repo, n, err)
	}
}

// JiraSite is the Jira site fixtures register projects on unless they need a
// particular one: what domain.EntityScope("jira", ...) answers for an org whose
// Jira base URL is https://acme.atlassian.net.
const JiraSite = "https://acme.atlassian.net"

// JiraProject returns the id of the registry row for the project keyed key on
// site in orgID, creating it when none answers to the key there. A created
// row carries a Jira project id derived from the key, so it is resolved, as
// every row the settings write creates is. On Postgres conn has to be one that
// may write the org's rows (the harness's admin connection).
func JiraProject(t testing.TB, conn Conn, orgID, site, key string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	err := conn.QueryRowContext(ctx, `
		SELECT CAST(id AS TEXT) FROM jira_projects
		 WHERE org_id = $1 AND site = $2 AND project_key = $3
	`, orgID, site, key).Scan(&id)
	if err == nil {
		return id
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dbseed: read jira project %s on %s: %v", key, site, err)
	}
	id = uuid.NewString()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO jira_projects (id, org_id, site, external_id, project_key)
		VALUES ($1, $2, $3, $4, $5)
	`, id, orgID, site, "id-"+key, key); err != nil {
		t.Fatalf("dbseed: create jira project %s on %s: %v", key, site, err)
	}
	return id
}
