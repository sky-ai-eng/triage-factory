package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// gitHubRefreshMarkStore is the SQLite impl of db.GitHubRefreshMarkStore. q
// is the connection, or the transaction a db.SystemTxStores is bound to.
type gitHubRefreshMarkStore struct{ q queryer }

func newGitHubRefreshMarkStore(q queryer) db.GitHubRefreshMarkStore {
	return &gitHubRefreshMarkStore{q: q}
}

var _ db.GitHubRefreshMarkStore = (*gitHubRefreshMarkStore)(nil)

const sqliteRefreshMarkColumns = `id, org_id, repository_id, kind, target, revision, first_marked_at, marked_at`

func scanSQLiteRefreshMark(row interface{ Scan(...any) error }) (domain.GitHubRefreshMark, error) {
	var m domain.GitHubRefreshMark
	err := row.Scan(&m.ID, &m.OrgID, &m.RepositoryID, &m.Target.Kind, &m.Target.Value, &m.Revision, &m.FirstMarkedAt, &m.MarkedAt)
	return m, err
}

func (s *gitHubRefreshMarkStore) MarkSystem(ctx context.Context, orgID, repositoryID string, target domain.GitHubRefreshTarget) (domain.GitHubRefreshMark, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.GitHubRefreshMark{}, err
	}
	if err := target.Validate(); err != nil {
		return domain.GitHubRefreshMark{}, err
	}
	now := time.Now().UTC()
	m, err := scanSQLiteRefreshMark(s.q.QueryRowContext(ctx, `
		INSERT INTO github_refresh_marks AS m (id, org_id, repository_id, kind, target, first_marked_at, marked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repository_id, kind, target) DO UPDATE SET
			revision  = m.revision + 1,
			marked_at = excluded.marked_at
		RETURNING `+sqliteRefreshMarkColumns,
		uuid.New().String(), orgID, repositoryID, target.Kind, target.Value, now, now))
	if err != nil {
		return domain.GitHubRefreshMark{}, fmt.Errorf("upsert github_refresh_marks: %w", err)
	}
	return m, nil
}

func (s *gitHubRefreshMarkStore) GetSystem(ctx context.Context, orgID, id string) (*domain.GitHubRefreshMark, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	m, err := scanSQLiteRefreshMark(s.q.QueryRowContext(ctx, `
		SELECT `+sqliteRefreshMarkColumns+`
		FROM github_refresh_marks
		WHERE org_id = ? AND id = ?
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
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if err := db.ValidateGitHubRefreshMarkLimit(limit); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteRefreshMarkColumns+`
		FROM github_refresh_marks
		WHERE org_id = ?
		ORDER BY first_marked_at, id
		LIMIT ?
	`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.GitHubRefreshMark{}
	for rows.Next() {
		m, err := scanSQLiteRefreshMark(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *gitHubRefreshMarkStore) ClearSystem(ctx context.Context, orgID string, read []domain.GitHubRefreshMark) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	if len(read) == 0 {
		return 0, nil
	}
	receipts, err := db.GitHubRefreshMarkReceiptsJSON(read)
	if err != nil {
		return 0, err
	}
	res, err := s.q.ExecContext(ctx, `
		DELETE FROM github_refresh_marks
		WHERE org_id = ?
		  AND EXISTS (
			SELECT 1 FROM json_each(?) r
			WHERE json_extract(r.value, '$.id') = github_refresh_marks.id
			  AND json_extract(r.value, '$.revision') = github_refresh_marks.revision)
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
