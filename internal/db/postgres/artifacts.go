package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// artifactStore is the Postgres impl of db.ArtifactStore. The artifacts_*
// RLS policies scope reads/writes by team_id exactly like runs; org_id stays
// in every WHERE/INSERT clause as defense in depth. Mirrors the runs store
// (agentrun.go) for $N placeholders + scan conventions. See TFAC-455.
//
// Holds two pools, the same split AgentRuns / RunWorktrees use:
//
//   - q: app pool (tf_app, RLS-active). Manual-run exec writes (under
//     synthetic claims) and run-detail / C2 reads route here.
//
//   - admin: admin pool (BYPASSRLS). UpsertSystem routes here for the exec
//     choke point on an event-triggered run, whose insert is unreachable
//     through tf_app — the run carries no creator user, so the
//     artifacts_insert policy's team-write check can never pass. See
//     TFAC-459.
type artifactStore struct {
	q     queryer
	admin queryer
}

func newArtifactStore(q, admin queryer) db.ArtifactStore {
	return &artifactStore{q: q, admin: admin}
}

var _ db.ArtifactStore = (*artifactStore)(nil)

// pgArtifactColumns is the SELECT/RETURNING list scanned into a
// domain.Artifact via scanArtifact. Nullable text columns are coalesced to
// an empty string so the scan targets are plain strings, the same shape
// pgRunColumns uses.
const pgArtifactColumns = `
	id, COALESCE(run_id::text, ''), org_id, team_id, provider, kind, target,
	COALESCE(external_id, ''), COALESCE(url, ''), state, dedup_key,
	COALESCE(details_json, ''), created_at, updated_at
`

func (s *artifactStore) Upsert(ctx context.Context, orgID string, a domain.Artifact) (domain.Artifact, error) {
	return s.upsert(ctx, s.q, orgID, a)
}

// UpsertSystem runs the same UPSERT on the admin pool (BYPASSRLS) for
// event-triggered exec writers that have no JWT-claims context. See the type
// doc and TFAC-459.
func (s *artifactStore) UpsertSystem(ctx context.Context, orgID string, a domain.Artifact) (domain.Artifact, error) {
	return s.upsert(ctx, s.admin, orgID, a)
}

func (s *artifactStore) upsert(ctx context.Context, q queryer, orgID string, a domain.Artifact) (domain.Artifact, error) {
	// ON CONFLICT(org_id, dedup_key) updates the documented mutable fields
	// from the proposed row (EXCLUDED.*) and bumps updated_at; id/created_at
	// on the existing row are preserved. provider/kind are deliberately NOT
	// updated: they are encoded into dedup_key (the conflict target), so a
	// conflicting row that disagreed on them would be keyed wrong — the
	// insert side pins them, the update side leaves them. A caller-supplied
	// a.ID is honored on insert (parity with SQLite); an empty a.ID falls
	// back to gen_random_uuid() server-side.
	//
	// target/external_id/url are preserved-on-empty: they are the backing
	// object's stable coordinates (resource key / PR number / issue key, html
	// link) — once known they only ever fill in or migrate to a more specific
	// value (pending PR owner/repo → owner/repo#123), never legitimately clear.
	// A later upsert that can't supply them — a reconciliation pass, a Jira
	// mutation whose run can't compute the browse URL, or a GitHub
	// comment-update/delete that only knows the comment id, not its PR number —
	// must not blank a value an earlier writer already stored. external_id/url
	// are NULLIFed to NULL on insert, so COALESCE preserves them; target is NOT
	// NULL, so NULLIF('','') folds an empty incoming target to NULL first. A
	// non-empty incoming value still overwrites (the intentional-change /
	// migration path). state/details_json stay last-writer-wins by design
	// (state tracks the latest action).
	row := q.QueryRowContext(ctx, `
		INSERT INTO artifacts
			(id, run_id, org_id, team_id, provider, kind, target,
			 external_id, url, state, dedup_key, details_json, updated_at)
		VALUES (COALESCE(NULLIF($1, '')::uuid, gen_random_uuid()),
		        NULLIF($2, '')::uuid, $3, $4, $5, $6, $7,
		        NULLIF($8, ''), NULLIF($9, ''), $10, $11, NULLIF($12, ''), now())
		ON CONFLICT (org_id, dedup_key) DO UPDATE SET
			run_id       = EXCLUDED.run_id,
			team_id      = EXCLUDED.team_id,
			target       = COALESCE(NULLIF(EXCLUDED.target, ''), artifacts.target),
			external_id  = COALESCE(EXCLUDED.external_id, artifacts.external_id),
			url          = COALESCE(EXCLUDED.url, artifacts.url),
			state        = EXCLUDED.state,
			details_json = EXCLUDED.details_json,
			updated_at   = now()
		RETURNING `+pgArtifactColumns,
		a.ID, a.RunID, orgID, a.TeamID, a.Provider, a.Kind, a.Target,
		a.ExternalID, a.URL, a.State, a.DedupKey, a.DetailsJSON,
	)
	var out domain.Artifact
	if err := scanArtifact(row, &out); err != nil {
		return domain.Artifact{}, err
	}
	return out, nil
}

func (s *artifactStore) ListByRun(ctx context.Context, orgID, runID string) ([]domain.Artifact, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgArtifactColumns+`
		FROM artifacts
		WHERE org_id = $1 AND run_id = $2
		ORDER BY created_at DESC, id DESC
	`, orgID, runID)
	if err != nil {
		return nil, err
	}
	return scanArtifactRows(rows)
}

func (s *artifactStore) ListByTeam(ctx context.Context, orgID, teamID string, opts db.ArtifactListOpts) ([]domain.Artifact, error) {
	query := `
		SELECT ` + pgArtifactColumns + `
		FROM artifacts
		WHERE org_id = $1 AND team_id = $2
		ORDER BY created_at DESC, id DESC
	`
	args := []any{orgID, teamID}
	if opts.Limit > 0 {
		// Placeholder numbered from the current arg count, not hardcoded, so
		// a future opt appended before this one (a Kind/State filter, say)
		// can't silently shift LIMIT onto the wrong $N.
		args = append(args, opts.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
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

// rowScanner unifies *sql.Row and *sql.Rows so scanArtifact serves both
// the single-row Upsert RETURNING and the list paths.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanArtifact(sc rowScanner, a *domain.Artifact) error {
	return sc.Scan(
		&a.ID, &a.RunID, &a.OrgID, &a.TeamID, &a.Provider, &a.Kind, &a.Target,
		&a.ExternalID, &a.URL, &a.State, &a.DedupKey, &a.DetailsJSON, &a.CreatedAt, &a.UpdatedAt,
	)
}
