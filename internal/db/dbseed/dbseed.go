// Package dbseed creates the rows a raw-SQL test fixture needs before it can
// write a row that references them. It is the one helper every fixture seeding
// a GitHub entity by hand goes through, in either dialect: a GitHub entity row
// references its repository's registry row (entities.repository_id, required),
// so the registry row has to exist first and the INSERT has to name it.
//
// It imports nothing from internal/db, so package db's own tests can use it.
package dbseed

import (
	"context"
	"database/sql"
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
// scope). It returns the id the fixture's INSERT INTO entities sets as
// repository_id. A key with no owner/repo fails the test.
func GitHubEntityRepository(t testing.TB, conn Conn, orgID, host, sourceID string) string {
	t.Helper()
	owner, repo, _ := domain.SplitGitHubEntitySourceID(sourceID)
	if owner == "" || repo == "" {
		t.Fatalf("dbseed: github entity key %q names no owner/repo", sourceID)
	}
	return GitHubRepository(t, conn, orgID, host, owner, repo)
}
