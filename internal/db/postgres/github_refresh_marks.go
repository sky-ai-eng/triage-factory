package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// gitHubRefreshMarkStore is the Postgres impl of db.GitHubRefreshMarkStore.
// q is the admin pool, or the admin-pool transaction a db.SystemTxStores is
// bound to.
type gitHubRefreshMarkStore struct{ q queryer }

func newGitHubRefreshMarkStore(q queryer) db.GitHubRefreshMarkStore {
	return &gitHubRefreshMarkStore{q: q}
}

var _ db.GitHubRefreshMarkStore = (*gitHubRefreshMarkStore)(nil)

const pgRefreshMarkColumns = `id::text, org_id::text, repository_id::text, kind, target, revision, first_marked_at, marked_at`

func scanPGRefreshMark(row interface{ Scan(...any) error }) (domain.GitHubRefreshMark, error) {
	var m domain.GitHubRefreshMark
	err := row.Scan(&m.ID, &m.OrgID, &m.RepositoryID, &m.Target.Kind, &m.Target.Value, &m.Revision, &m.FirstMarkedAt, &m.MarkedAt)
	return m, err
}

func (s *gitHubRefreshMarkStore) MarkSystem(ctx context.Context, orgID, repositoryID string, target domain.GitHubRefreshTarget) (domain.GitHubRefreshMark, error) {
	if err := target.Validate(); err != nil {
		return domain.GitHubRefreshMark{}, err
	}
	// statement_timestamp() rather than now(): the mark is written inside the
	// delivery's transaction, and now() is that transaction's start.
	m, err := scanPGRefreshMark(s.q.QueryRowContext(ctx, `
		INSERT INTO github_refresh_marks AS m (org_id, repository_id, kind, target, first_marked_at, marked_at)
		VALUES ($1, $2, $3, $4, statement_timestamp(), statement_timestamp())
		ON CONFLICT (repository_id, kind, target) DO UPDATE SET
			revision  = m.revision + 1,
			marked_at = excluded.marked_at
		RETURNING `+pgRefreshMarkColumns,
		orgID, repositoryID, target.Kind, target.Value))
	if err != nil {
		return domain.GitHubRefreshMark{}, fmt.Errorf("upsert github_refresh_marks: %w", err)
	}
	return m, nil
}

func (s *gitHubRefreshMarkStore) GetSystem(ctx context.Context, orgID, id string) (*domain.GitHubRefreshMark, error) {
	m, err := scanPGRefreshMark(s.q.QueryRowContext(ctx, `
		SELECT `+pgRefreshMarkColumns+`
		FROM github_refresh_marks
		WHERE org_id = $1 AND id = $2
	`, orgID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *gitHubRefreshMarkStore) ListSystem(ctx context.Context, orgID string, limit int) ([]domain.GitHubRefreshMark, error) {
	if err := db.ValidateGitHubRefreshMarkLimit(limit); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgRefreshMarkColumns+`
		FROM github_refresh_marks
		WHERE org_id = $1
		ORDER BY first_marked_at, id
		LIMIT $2
	`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.GitHubRefreshMark{}
	for rows.Next() {
		m, err := scanPGRefreshMark(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *gitHubRefreshMarkStore) ClearSystem(ctx context.Context, orgID string, read []domain.GitHubRefreshMark) (int, error) {
	if len(read) == 0 {
		return 0, nil
	}
	receipts, err := db.GitHubRefreshMarkReceiptsJSON(read)
	if err != nil {
		return 0, err
	}
	res, err := s.q.ExecContext(ctx, `
		DELETE FROM github_refresh_marks m
		USING jsonb_to_recordset($2::jsonb) AS r(id uuid, revision bigint)
		WHERE m.org_id = $1 AND m.id = r.id AND m.revision = r.revision
	`, orgID, receipts)
	if err != nil {
		return 0, fmt.Errorf("clear github_refresh_marks: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
