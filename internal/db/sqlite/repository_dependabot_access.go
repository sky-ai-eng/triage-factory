package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// repositoryDependabotAccessStore is the SQLite impl of
// db.RepositoryDependabotAccessStore.
type repositoryDependabotAccessStore struct{ q queryer }

func newRepositoryDependabotAccessStore(q queryer) db.RepositoryDependabotAccessStore {
	return &repositoryDependabotAccessStore{q: q}
}

var _ db.RepositoryDependabotAccessStore = (*repositoryDependabotAccessStore)(nil)

const sqliteDependabotAccessColumns = `repository_id, org_id, access, checked_at`

func scanSQLiteDependabotAccess(row interface{ Scan(...any) error }) (domain.RepositoryDependabotAccess, error) {
	var a domain.RepositoryDependabotAccess
	err := row.Scan(&a.RepositoryID, &a.OrgID, &a.Access, &a.CheckedAt)
	return a, err
}

func (s *repositoryDependabotAccessStore) RecordSystem(ctx context.Context, orgID, repositoryID string, access domain.DependabotAlertsAccess) (domain.RepositoryDependabotAccess, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.RepositoryDependabotAccess{}, err
	}
	if err := access.Validate(); err != nil {
		return domain.RepositoryDependabotAccess{}, err
	}
	a, err := scanSQLiteDependabotAccess(s.q.QueryRowContext(ctx, `
		INSERT INTO repository_dependabot_access (repository_id, org_id, access, checked_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (repository_id) DO UPDATE SET
			access     = excluded.access,
			checked_at = excluded.checked_at
		WHERE repository_dependabot_access.org_id = excluded.org_id
		RETURNING `+sqliteDependabotAccessColumns,
		repositoryID, orgID, access, time.Now().UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RepositoryDependabotAccess{}, fmt.Errorf("repository %s is not a registry row of org %s", repositoryID, orgID)
	}
	if err != nil {
		return domain.RepositoryDependabotAccess{}, fmt.Errorf("upsert repository_dependabot_access: %w", err)
	}
	return a, nil
}

func (s *repositoryDependabotAccessStore) GetSystem(ctx context.Context, orgID, repositoryID string) (*domain.RepositoryDependabotAccess, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	a, err := scanSQLiteDependabotAccess(s.q.QueryRowContext(ctx, `
		SELECT `+sqliteDependabotAccessColumns+`
		FROM repository_dependabot_access
		WHERE org_id = ? AND repository_id = ?
	`, orgID, repositoryID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *repositoryDependabotAccessStore) ListSystem(ctx context.Context, orgID, host string) ([]domain.RepositoryDependabotAccess, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT a.repository_id, a.org_id, a.access, a.checked_at
		FROM repository_dependabot_access a
		JOIN repositories ON repositories.id = a.repository_id
		WHERE a.org_id = ?
		  AND repositories.host = ?
		  AND repositories.retired_at IS NULL
		ORDER BY a.repository_id
	`, orgID, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RepositoryDependabotAccess{}
	for rows.Next() {
		a, err := scanSQLiteDependabotAccess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *repositoryDependabotAccessStore) CountTrackedSystem(ctx context.Context, orgID, host string) (domain.DependabotAccessCounts, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.DependabotAccessCounts{}, err
	}
	if err := db.RequireRepoHost(host); err != nil {
		return domain.DependabotAccessCounts{}, err
	}
	var c domain.DependabotAccessCounts
	err := s.q.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN a.access = 'readable' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN a.access = 'no_permission' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN a.access = 'disabled' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN a.access IS NULL THEN 1 ELSE 0 END), 0)
		FROM repositories
		LEFT JOIN repository_dependabot_access a ON a.repository_id = repositories.id
		WHERE repositories.host = ?
		  AND repositories.retired_at IS NULL
		  AND `+repoTrackedByATeam,
		host).Scan(&c.Readable, &c.NoPermission, &c.Disabled, &c.Unchecked)
	if err != nil {
		return domain.DependabotAccessCounts{}, err
	}
	return c, nil
}
