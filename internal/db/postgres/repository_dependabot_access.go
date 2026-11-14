package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// repositoryDependabotAccessStore is the Postgres impl of
// db.RepositoryDependabotAccessStore. q is the admin pool.
type repositoryDependabotAccessStore struct{ q queryer }

func newRepositoryDependabotAccessStore(q queryer) db.RepositoryDependabotAccessStore {
	return &repositoryDependabotAccessStore{q: q}
}

var _ db.RepositoryDependabotAccessStore = (*repositoryDependabotAccessStore)(nil)

const pgDependabotAccessColumns = `repository_id::text, org_id::text, access, checked_at, alerts_etag`

func scanPGDependabotAccess(row interface{ Scan(...any) error }) (domain.RepositoryDependabotAccess, error) {
	var a domain.RepositoryDependabotAccess
	err := row.Scan(&a.RepositoryID, &a.OrgID, &a.Access, &a.CheckedAt, &a.AlertsETag)
	return a, err
}

func (s *repositoryDependabotAccessStore) RecordSystem(ctx context.Context, orgID, repositoryID string, access domain.DependabotAlertsAccess) (domain.RepositoryDependabotAccess, error) {
	if err := access.Validate(); err != nil {
		return domain.RepositoryDependabotAccess{}, err
	}
	// The conflict arm is confined to the org's own row: the key is the
	// repository alone, so without it an answer recorded under another org
	// would overwrite this one rather than fail its foreign key.
	a, err := scanPGDependabotAccess(s.q.QueryRowContext(ctx, `
		INSERT INTO repository_dependabot_access AS a (repository_id, org_id, access, checked_at)
		VALUES ($1, $2, $3, statement_timestamp())
		ON CONFLICT (repository_id) DO UPDATE SET
			access     = excluded.access,
			checked_at = excluded.checked_at
		WHERE a.org_id = excluded.org_id
		RETURNING `+pgDependabotAccessColumns,
		repositoryID, orgID, access))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RepositoryDependabotAccess{}, fmt.Errorf("repository %s is not a registry row of org %s", repositoryID, orgID)
	}
	if err != nil {
		return domain.RepositoryDependabotAccess{}, fmt.Errorf("upsert repository_dependabot_access: %w", err)
	}
	return a, nil
}

func (s *repositoryDependabotAccessStore) RecordAlertListingSystem(ctx context.Context, orgID, repositoryID, etag string) (domain.RepositoryDependabotAccess, error) {
	return scanWrittenDependabotAccess(s.q.QueryRowContext(ctx, `
		UPDATE repository_dependabot_access
		   SET alerts_etag = $3
		 WHERE org_id = $1 AND repository_id = $2
		RETURNING `+pgDependabotAccessColumns,
		orgID, repositoryID, etag))
}

func (s *repositoryDependabotAccessStore) ClearUntrackedAlertListingsSystem(ctx context.Context, orgID, host string) (int, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return 0, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE repository_dependabot_access a
		   SET alerts_etag = ''
		  FROM repositories rp
		 WHERE rp.id = a.repository_id
		   AND rp.org_id = a.org_id
		   AND a.org_id = $1
		   AND a.alerts_etag <> ''
		   AND NOT (rp.host = $2 AND rp.retired_at IS NULL
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
	a, err := scanPGDependabotAccess(row)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.RepositoryDependabotAccess{}, fmt.Errorf("update repository_dependabot_access listing: %w", err)
	}
	return a, err
}

func (s *repositoryDependabotAccessStore) GetSystem(ctx context.Context, orgID, repositoryID string) (*domain.RepositoryDependabotAccess, error) {
	a, err := scanPGDependabotAccess(s.q.QueryRowContext(ctx, `
		SELECT `+pgDependabotAccessColumns+`
		FROM repository_dependabot_access
		WHERE org_id = $1 AND repository_id = $2
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
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgDependabotAccessColumns+`
		FROM repository_dependabot_access
		WHERE org_id = $1
		  AND repository_id IN (
		    SELECT id FROM repositories
		    WHERE org_id = $1 AND host = $2 AND retired_at IS NULL
		  )
		ORDER BY repository_id
	`, orgID, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RepositoryDependabotAccess{}
	for rows.Next() {
		a, err := scanPGDependabotAccess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *repositoryDependabotAccessStore) CountTrackedSystem(ctx context.Context, orgID, host string, checkedSince time.Time) (domain.DependabotAccessCounts, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return domain.DependabotAccessCounts{}, err
	}
	var c domain.DependabotAccessCounts
	err := s.q.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE a.access = 'readable'),
			count(*) FILTER (WHERE a.access = 'no_permission'),
			count(*) FILTER (WHERE a.access = 'disabled'),
			count(*) FILTER (WHERE a.access IS NULL)
		FROM repositories rp
		LEFT JOIN repository_dependabot_access a
		  ON a.repository_id = rp.id AND a.org_id = rp.org_id AND a.checked_at >= $3
		WHERE rp.org_id = $1
		  AND rp.host = $2
		  AND rp.retired_at IS NULL
		  AND `+repoTrackedByATeam,
		orgID, host, checkedSince).Scan(&c.Readable, &c.NoPermission, &c.Disabled, &c.Unchecked)
	if err != nil {
		return domain.DependabotAccessCounts{}, err
	}
	return c, nil
}
