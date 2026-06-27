package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// artifactNonTerminalPredicate is the SQL WHERE fragment selecting the
// reconciler's working set — non-terminal artifacts backed by a fetchable
// GitHub object (TFAC-464). Built from the domain consts (no bind params, all
// literals) so it can't drift from domain.IsReconcilableNonTerminal, which
// TestArtifactStore_SQLite_ListNonTerminal pins it equal to. The state strings
// carry no quotes, so the concatenation is injection-safe.
var artifactNonTerminalPredicate = fmt.Sprintf(`(
		   (kind = '%s' AND state IN ('%s', '%s'))
		OR (kind = '%s' AND state = '%s')
		OR (kind = '%s' AND state = '%s')
	)`,
	domain.ArtifactKindPullRequest, domain.ArtifactStatePRDraft, domain.ArtifactStatePROpen,
	domain.ArtifactKindReview, domain.ArtifactStateReviewPending,
	domain.ArtifactKindBranch, domain.ArtifactStateBranchPushed,
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
	// ON CONFLICT(org_id, dedup_key) updates the documented mutable fields
	// from the proposed row (excluded.*) and bumps updated_at. id/created_at
	// are preserved on the existing row — a conflicting insert keeps the
	// original identity, the same UPSERT contract the Postgres impl has.
	// provider/kind are deliberately NOT updated: they are encoded into
	// dedup_key (the conflict target), so the insert side pins them and the
	// update side leaves them rather than risk a row whose discriminators
	// disagree with its key.
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
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO artifacts
			(id, run_id, org_id, team_id, provider, kind, target,
			 external_id, url, state, dedup_key, details_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(org_id, dedup_key) DO UPDATE SET
			run_id       = excluded.run_id,
			team_id      = excluded.team_id,
			target       = COALESCE(NULLIF(excluded.target, ''), artifacts.target),
			external_id  = COALESCE(excluded.external_id, artifacts.external_id),
			url          = COALESCE(excluded.url, artifacts.url),
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

// UpsertSystem is identical to Upsert in SQLite: local mode is single-tenant
// (N=1) with no RLS, so there is no admin/app pool split — both run on the one
// connection. The method exists for parity with the Postgres store, where the
// exec choke point's event-triggered writers (no JWT-claims context) need an
// admin-pool path. See TFAC-459.
func (s *artifactStore) UpsertSystem(ctx context.Context, orgID string, a domain.Artifact) (domain.Artifact, error) {
	return s.Upsert(ctx, orgID, a)
}

func (s *artifactStore) Get(ctx context.Context, orgID, id string) (*domain.Artifact, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT `+artifactColumns+`
		FROM artifacts
		WHERE org_id = ? AND id = ?
	`, orgID, id)
	var a domain.Artifact
	if err := scanArtifact(row, &a); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
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

// ListByRunSystem is identical to ListByRun in SQLite: local mode is
// single-tenant (N=1) with no RLS, so there is no admin/app pool split. The
// method exists for parity with the Postgres store, where the spawner's park
// check (no JWT-claims context) needs an admin-pool path.
func (s *artifactStore) ListByRunSystem(ctx context.Context, orgID, runID string) ([]domain.Artifact, error) {
	return s.ListByRun(ctx, orgID, runID)
}

func (s *artifactStore) CountByRun(ctx context.Context, orgID string, runIDs []string) (map[string]int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(runIDs))
	if len(runIDs) == 0 {
		return counts, nil
	}
	// Expand to a ?-placeholder IN list (SQLite has no array bind). A NULL
	// run_id never matches IN, so detached artifacts are excluded for free.
	placeholders := make([]string, len(runIDs))
	args := make([]any, 0, len(runIDs)+1)
	args = append(args, orgID)
	for i, id := range runIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT run_id, COUNT(*)
		FROM artifacts
		WHERE org_id = ? AND run_id IN (`+strings.Join(placeholders, ", ")+`)
		GROUP BY run_id
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var runID string
		var n int
		if err := rows.Scan(&runID, &n); err != nil {
			return nil, err
		}
		counts[runID] = n
	}
	return counts, rows.Err()
}

func (s *artifactStore) ListByRuns(ctx context.Context, orgID string, runIDs []string) ([]domain.Artifact, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if len(runIDs) == 0 {
		return nil, nil
	}
	// Expand to a ?-placeholder IN list (SQLite has no array bind). A NULL
	// run_id never matches IN, so detached artifacts are excluded for free.
	placeholders := make([]string, len(runIDs))
	args := make([]any, 0, len(runIDs)+1)
	args = append(args, orgID)
	for i, id := range runIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+artifactColumns+`
		FROM artifacts
		WHERE org_id = ? AND run_id IN (`+strings.Join(placeholders, ", ")+`)
		ORDER BY created_at DESC, id DESC
	`, args...)
	if err != nil {
		return nil, err
	}
	return scanArtifactRows(rows)
}

func (s *artifactStore) ListByTeam(ctx context.Context, orgID, teamID string, opts db.ArtifactListOpts) ([]domain.Artifact, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Base WHERE only — the filter helper appends the optional predicates, the
	// ORDER BY, and LIMIT/OFFSET so this and ListByOrgSystem share one builder.
	query := `SELECT ` + artifactColumns + ` FROM artifacts WHERE org_id = ? AND team_id = ?`
	query, args := appendArtifactFilters(query, []any{orgID, teamID}, opts)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanArtifactRows(rows)
}

// appendArtifactFilters appends opts' optional provider/kind/state/time
// predicates, the newest-first ORDER BY, and LIMIT/OFFSET paging to a SELECT
// whose WHERE clause is already open. Time bounds wrap both sides in datetime()
// — exactly the spend store's pattern — so a bound time.Time compares correctly
// against the CURRENT_TIMESTAMP-formatted created_at regardless of the driver's
// bind serialization.
func appendArtifactFilters(query string, args []any, opts db.ArtifactListOpts) (string, []any) {
	if opts.Provider != "" {
		query += ` AND provider = ?`
		args = append(args, opts.Provider)
	}
	if opts.Kind != "" {
		query += ` AND kind = ?`
		args = append(args, opts.Kind)
	}
	if opts.State != "" {
		query += ` AND state = ?`
		args = append(args, opts.State)
	}
	if !opts.Since.IsZero() {
		query += ` AND datetime(created_at) >= datetime(?)`
		args = append(args, opts.Since)
	}
	if !opts.Until.IsZero() {
		query += ` AND datetime(created_at) < datetime(?)`
		args = append(args, opts.Until)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
		// OFFSET only with a LIMIT — paging the newest-first feed.
		if opts.Offset > 0 {
			query += ` OFFSET ?`
			args = append(args, opts.Offset)
		}
	}
	return query, args
}

// ListByOrgSystem is identical to a plain org read in SQLite: local mode is
// single-tenant (N=1) with no RLS, so there is no admin/app pool split. It
// returns ALL states (terminal included) for the bot-activity audit feed
// (TFAC-483), in contrast to ListNonTerminalBySystem's reconciler working set.
func (s *artifactStore) ListByOrgSystem(ctx context.Context, orgID string, opts db.ArtifactListOpts) ([]domain.Artifact, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	query := `SELECT ` + artifactColumns + ` FROM artifacts WHERE org_id = ?`
	query, args := appendArtifactFilters(query, []any{orgID}, opts)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanArtifactRows(rows)
}

// ListNonTerminalBySystem is identical to a plain org read in SQLite: local
// mode is single-tenant (N=1) with no RLS, so there is no admin/app pool
// split. The method exists for parity with the Postgres store, where the
// reconciler (a background system job with no JWT-claims context) needs the
// admin pool to see every team's non-terminal artifacts. TFAC-464.
func (s *artifactStore) ListNonTerminalBySystem(ctx context.Context, orgID string) ([]domain.Artifact, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+artifactColumns+`
		FROM artifacts
		WHERE org_id = ? AND `+artifactNonTerminalPredicate+`
		ORDER BY created_at DESC, id DESC
	`, orgID)
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
