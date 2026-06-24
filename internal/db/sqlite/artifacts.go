package sqlite

import (
	"context"
	"database/sql"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// artifactStore is the SQLite impl of db.ArtifactStore. SQLite is
// single-tenant (local mode, N=1) with no RLS — org_id exists for parity
// with the Postgres baseline and every caller passes LocalDefaultOrgID
// (asserted at each entry). Mirrors the runs store (agentrun.go) for the
// single-queryer shape and scan conventions. See TFAC-455.
type artifactStore struct{ q queryer }

func newArtifactStore(q queryer) db.ArtifactStore { return &artifactStore{q: q} }

var _ db.ArtifactStore = (*artifactStore)(nil)

// artifactColumns is the SELECT list scanned into a domain.Artifact via
// scanArtifact. Same order the Postgres impl projects.
const artifactColumns = `
	id, run_id, org_id, team_id, provider, kind, target,
	external_id, url, state, dedup_key, details_json, created_at, updated_at
`

func (s *artifactStore) Upsert(ctx context.Context, orgID string, a domain.Artifact) (domain.Artifact, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Artifact{}, err
	}
	id := a.ID
	if id == "" {
		id = uuid.New().String()
	}
	// ON CONFLICT(org_id, dedup_key) updates the mutable fields from the
	// proposed row (excluded.*) and bumps updated_at. id/created_at are
	// preserved on the existing row — a conflicting insert keeps the
	// original identity, the same UPSERT contract the Postgres impl has.
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO artifacts
			(id, run_id, org_id, team_id, provider, kind, target,
			 external_id, url, state, dedup_key, details_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(org_id, dedup_key) DO UPDATE SET
			run_id       = excluded.run_id,
			team_id      = excluded.team_id,
			provider     = excluded.provider,
			kind         = excluded.kind,
			target       = excluded.target,
			external_id  = excluded.external_id,
			url          = excluded.url,
			state        = excluded.state,
			details_json = excluded.details_json,
			updated_at   = CURRENT_TIMESTAMP
		RETURNING `+artifactColumns,
		id, nullIfEmpty(a.RunID), orgID, a.TeamID, a.Provider, a.Kind, a.Target,
		nullIfEmpty(a.ExternalID), nullIfEmpty(a.URL), a.State, a.DedupKey, nullIfEmpty(a.DetailsJSON),
	)
	var out domain.Artifact
	if err := scanArtifact(row, &out); err != nil {
		return domain.Artifact{}, err
	}
	return out, nil
}

func (s *artifactStore) ListByRun(ctx context.Context, orgID, runID string) ([]domain.Artifact, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+artifactColumns+`
		FROM artifacts
		WHERE org_id = ? AND run_id = ?
		ORDER BY created_at DESC, id DESC
	`, orgID, runID)
	if err != nil {
		return nil, err
	}
	return scanArtifactRows(rows)
}

func (s *artifactStore) ListByTeam(ctx context.Context, orgID, teamID string, opts db.ArtifactListOpts) ([]domain.Artifact, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	query := `
		SELECT ` + artifactColumns + `
		FROM artifacts
		WHERE org_id = ? AND team_id = ?
		ORDER BY created_at DESC, id DESC
	`
	args := []any{orgID, teamID}
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanArtifactRows(rows)
}

// --- Helpers ---

func scanArtifactRows(rows *sql.Rows) ([]domain.Artifact, error) {
	defer rows.Close()
	var out []domain.Artifact
	for rows.Next() {
		var a domain.Artifact
		if err := scanArtifact(rows, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// scanArtifact fills a from one row. rowScanner (defined in repos.go)
// unifies *sql.Row and *sql.Rows so this serves both the single-row Upsert
// RETURNING and the list paths.
func scanArtifact(sc rowScanner, a *domain.Artifact) error {
	var runID, externalID, url, detailsJSON sql.NullString
	if err := sc.Scan(
		&a.ID, &runID, &a.OrgID, &a.TeamID, &a.Provider, &a.Kind, &a.Target,
		&externalID, &url, &a.State, &a.DedupKey, &detailsJSON, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return err
	}
	a.RunID = runID.String
	a.ExternalID = externalID.String
	a.URL = url.String
	a.DetailsJSON = detailsJSON.String
	return nil
}
