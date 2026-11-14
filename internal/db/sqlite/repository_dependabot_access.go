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

const sqliteDependabotAccessColumns = `repository_id, org_id, access, checked_at, alerts_etag`

func scanSQLiteDependabotAccess(row interface{ Scan(...any) error }) (domain.RepositoryDependabotAccess, error) {
	var a domain.RepositoryDependabotAccess
	err := row.Scan(&a.RepositoryID, &a.OrgID, &a.Access, &a.CheckedAt, &a.AlertsETag)
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

func (s *repositoryDependabotAccessStore) RecordAlertListingSystem(ctx context.Context, orgID, repositoryID, etag string) (domain.RepositoryDependabotAccess, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.RepositoryDependabotAccess{}, err
	}
	return scanWrittenDependabotAccess(s.q.QueryRowContext(ctx, `
		UPDATE repository_dependabot_access
		   SET alerts_etag = ?
		 WHERE org_id = ? AND repository_id = ?
		RETURNING `+sqliteDependabotAccessColumns,
		etag, orgID, repositoryID))
}

func (s *repositoryDependabotAccessStore) ClearUntrackedAlertListingsSystem(ctx context.Context, orgID, host string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	if err := db.RequireRepoHost(host); err != nil {
		return 0, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE repository_dependabot_access
		   SET alerts_etag = ''
		  FROM repositories
		 WHERE repositories.id = repository_dependabot_access.repository_id
		   AND repository_dependabot_access.org_id = ?
		   AND repository_dependabot_access.alerts_etag <> ''
		   AND NOT (repositories.host = ? AND repositories.retired_at IS NULL
		            AND (`+repoTrackedByATeam+` OR `+repoHoldsVulnerablePackage+`))
	`, orgID, host)
	if err != nil {
		return 0, fmt.Errorf("clear untracked dependabot alert listings: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// scanWrittenDependabotAccess decodes an UPDATE … RETURNING of one answer's
// row, passing sql.ErrNoRows through as the miss.
func scanWrittenDependabotAccess(row *sql.Row) (domain.RepositoryDependabotAccess, error) {
	a, err := scanSQLiteDependabotAccess(row)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.RepositoryDependabotAccess{}, fmt.Errorf("update repository_dependabot_access listing: %w", err)
	}
	return a, err
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
		SELECT `+sqliteDependabotAccessColumns+`
		FROM repository_dependabot_access
		WHERE org_id = ?
		  AND repository_id IN (
		    SELECT id FROM repositories WHERE host = ? AND retired_at IS NULL
		  )
		ORDER BY repository_id
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

func (s *repositoryDependabotAccessStore) CountTrackedSystem(ctx context.Context, orgID, host string, checkedSince time.Time) (domain.DependabotAccessCounts, error) {
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
		LEFT JOIN repository_dependabot_access a
		  ON a.repository_id = repositories.id AND a.checked_at >= ?
		WHERE repositories.host = ?
		  AND repositories.retired_at IS NULL
		  AND `+repoTrackedByATeam,
		checkedSince.UTC(), host).Scan(&c.Readable, &c.NoPermission, &c.Disabled, &c.Unchecked)
	if err != nil {
		return domain.DependabotAccessCounts{}, err
	}
	return c, nil
}
