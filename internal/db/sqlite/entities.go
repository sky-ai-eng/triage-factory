package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// entityStore is the SQLite impl of db.EntityStore. SQL bodies are
// ported from the pre-D2 internal/db/entities.go; the only behavioral
// change is the orgID assertion at each method entry. SQLite tables
// have no org_id column — local mode is single-tenant by construction.
//
// The constructor takes two queryers for signature parity with the
// Postgres impl (SKY-296), but SQLite has only one connection — both
// arguments collapse onto the same queryer. The `...System` admin-
// pool variants therefore run identically to the non-System variants
// here; the pool distinction is purely a multi-mode concept.
type entityStore struct{ q queryer }

func newEntityStore(q, _ queryer) db.EntityStore { return &entityStore{q: q} }

var _ db.EntityStore = (*entityStore)(nil)

// entityIDInChunkSize caps the number of `?` placeholders per batched
// WHERE id IN (...) query so we stay well under SQLite's default
// SQLITE_LIMIT_VARIABLE_NUMBER (999). Chunking runs multiple
// round-trips but keeps the query schema compatible with the default
// build — the scorer's entity set can easily exceed 1k tasks on
// large repos.
const entityIDInChunkSize = 500

const entitySelectCols = `id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
       COALESCE(snapshot_json, ''), COALESCE(description, ''), state, project_id,
       COALESCE(classification_rationale, ''), created_at, last_polled_at, closed_at`

// --- Lookup ---

func (s *entityStore) Get(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `SELECT `+entitySelectCols+` FROM entities WHERE id = ?`, id)
	return scanEntityRow(row)
}

func (s *entityStore) GetBySource(ctx context.Context, orgID, source, sourceID string) (*domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `SELECT `+entitySelectCols+` FROM entities WHERE source = ? AND source_id = ?`, source, sourceID)
	return scanEntityRow(row)
}

func (s *entityStore) Descriptions(ctx context.Context, orgID string, ids []string) (map[string]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	for start := 0; start < len(unique); start += entityIDInChunkSize {
		end := start + entityIDInChunkSize
		if end > len(unique) {
			end = len(unique)
		}
		chunk := unique[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		query := `SELECT id, COALESCE(description, '') FROM entities WHERE id IN (` +
			strings.Join(placeholders, ",") + `)`
		rows, err := s.q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, desc string
			if err := rows.Scan(&id, &desc); err != nil {
				rows.Close()
				return nil, err
			}
			if desc != "" {
				out[id] = desc
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	return out, nil
}

func (s *entityStore) ListUnclassified(ctx context.Context, orgID string) ([]domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+entitySelectCols+`
		FROM entities
		WHERE project_id IS NULL AND classified_at IS NULL AND state = 'active'
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.Entity{}
	for rows.Next() {
		e, err := scanEntityFromRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (s *entityStore) ListActive(ctx context.Context, orgID, source string) ([]domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+entitySelectCols+`
		FROM entities
		WHERE source = ? AND state = 'active'
		ORDER BY last_polled_at ASC
	`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.Entity{}
	for rows.Next() {
		e, err := scanEntityFromRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// ListActiveJiraTeamScoped returns the full active Jira set in local
// mode — N=1, so there is no other team to scope away. The Postgres
// impl applies the jira_project_status_rules team semi-join; SQLite
// deliberately does not (see the interface doc on EntityStore for the
// asymmetry rationale, which mirrors FactoryReadStore.Entities).
func (s *entityStore) ListActiveJiraTeamScoped(ctx context.Context, orgID, _ string) ([]domain.Entity, error) {
	// teamID ignored: N=1 local mode has one team, so the deck's team
	// filter has nothing to narrow (the frontend never renders it below
	// 2 teams). Mirrors the FactoryReadStore.Entities asymmetry.
	return s.ListActive(ctx, orgID, "jira")
}

func (s *entityStore) ListProjectPanel(ctx context.Context, orgID, projectID string) ([]domain.ProjectPanelEntity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
		       state, COALESCE(classification_rationale, ''), created_at, last_polled_at
		FROM entities
		WHERE project_id = ? AND state = 'active'
		ORDER BY last_polled_at DESC NULLS LAST
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.ProjectPanelEntity{}
	for rows.Next() {
		var e domain.ProjectPanelEntity
		if err := rows.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
			&e.State, &e.ClassificationRationale, &e.CreatedAt, &e.LastPolledAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- Mutation ---

func (s *entityStore) FindOrCreate(ctx context.Context, orgID, source, sourceID, kind, title, url string) (*domain.Entity, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, false, err
	}
	existing, err := s.GetBySource(ctx, orgID, source, sourceID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}

	id := uuid.New().String()
	now := time.Now()
	_, err = s.q.ExecContext(ctx, `
		INSERT INTO entities (id, source, source_id, kind, title, url, state, created_at, last_polled_at)
		VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?)
	`, id, source, sourceID, kind, title, url, now, now)
	if err != nil {
		// Concurrent first-discovery: another goroutine inserted
		// between our SELECT and INSERT. Re-read so both callers
		// see a populated entity.
		existing, err2 := s.GetBySource(ctx, orgID, source, sourceID)
		if err2 == nil && existing != nil {
			return existing, false, nil
		}
		return nil, false, err
	}

	return &domain.Entity{
		ID:           id,
		Source:       source,
		SourceID:     sourceID,
		Kind:         kind,
		Title:        title,
		URL:          url,
		State:        "active",
		CreatedAt:    now,
		LastPolledAt: &now,
	}, true, nil
}

func (s *entityStore) UpdateSnapshot(ctx context.Context, orgID, id, snapshotJSON string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE entities SET snapshot_json = ?, last_polled_at = ? WHERE id = ?
	`, snapshotJSON, time.Now(), id)
	return err
}

func (s *entityStore) PatchSnapshot(ctx context.Context, orgID, id, snapshotJSON string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE entities SET snapshot_json = ? WHERE id = ?`, snapshotJSON, id)
	return err
}

func (s *entityStore) UpdateTitle(ctx context.Context, orgID, id, title string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE entities SET title = ? WHERE id = ?`, title, id)
	return err
}

func (s *entityStore) UpdateDescription(ctx context.Context, orgID, id, description string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE entities SET description = ? WHERE id = ?`, description, id)
	return err
}

func (s *entityStore) AssignProject(ctx context.Context, orgID, id string, projectID *string, rationale string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	var projectArg any
	if projectID != nil && *projectID != "" {
		projectArg = *projectID
	}
	var rationaleArg any
	if rationale != "" {
		rationaleArg = rationale
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE entities
		SET project_id = ?,
		    classification_rationale = ?,
		    classified_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, projectArg, rationaleArg, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists int
		err := s.q.QueryRowContext(ctx, `SELECT 1 FROM entities WHERE id = ?`, id).Scan(&exists)
		if err == sql.ErrNoRows {
			return sql.ErrNoRows
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *entityStore) MarkClosed(ctx context.Context, orgID, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE entities SET state = 'closed', closed_at = ? WHERE id = ?
	`, time.Now(), id)
	return err
}

func (s *entityStore) Close(ctx context.Context, orgID, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE entities SET state = 'closed', closed_at = ? WHERE id = ? AND state = 'active'
	`, time.Now(), id)
	return err
}

func (s *entityStore) Reactivate(ctx context.Context, orgID, id string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE entities SET state = 'active', closed_at = NULL WHERE id = ? AND state = 'closed'
	`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// --- Admin-pool (`...System`) variants ---
//
// SKY-296 introduces `...System` methods on these stores so multi-mode
// consumers without JWT-claims context can route through the admin
// pool. SQLite has one connection and no auth concept, so each System
// variant just delegates to its non-System counterpart — preserving
// the same assertLocalOrg gate and behavior.

func (s *entityStore) GetSystem(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	return s.Get(ctx, orgID, id)
}

func (s *entityStore) ListActiveSystem(ctx context.Context, orgID, source string) ([]domain.Entity, error) {
	return s.ListActive(ctx, orgID, source)
}

func (s *entityStore) ListUnclassifiedSystem(ctx context.Context, orgID string) ([]domain.Entity, error) {
	return s.ListUnclassified(ctx, orgID)
}

func (s *entityStore) FindOrCreateSystem(ctx context.Context, orgID, source, sourceID, kind, title, url string) (*domain.Entity, bool, error) {
	return s.FindOrCreate(ctx, orgID, source, sourceID, kind, title, url)
}

func (s *entityStore) UpdateSnapshotSystem(ctx context.Context, orgID, id, snapshotJSON string) error {
	return s.UpdateSnapshot(ctx, orgID, id, snapshotJSON)
}

func (s *entityStore) UpdateTitleSystem(ctx context.Context, orgID, id, title string) error {
	return s.UpdateTitle(ctx, orgID, id, title)
}

func (s *entityStore) UpdateDescriptionSystem(ctx context.Context, orgID, id, description string) error {
	return s.UpdateDescription(ctx, orgID, id, description)
}

func (s *entityStore) AssignProjectSystem(ctx context.Context, orgID, id string, projectID *string, rationale string) error {
	return s.AssignProject(ctx, orgID, id, projectID, rationale)
}

func (s *entityStore) MarkClosedSystem(ctx context.Context, orgID, id string) error {
	return s.MarkClosed(ctx, orgID, id)
}

// CloseSystem mirrors Close (active→closed transition). The router's
// entity-lifecycle path consumes this through the admin pool in
// Postgres; SQLite collapses to the non-System variant.
func (s *entityStore) CloseSystem(ctx context.Context, orgID, id string) error {
	return s.Close(ctx, orgID, id)
}

func (s *entityStore) ReactivateSystem(ctx context.Context, orgID, id string) (bool, error) {
	return s.Reactivate(ctx, orgID, id)
}

func (s *entityStore) DescriptionsSystem(ctx context.Context, orgID string, ids []string) (map[string]string, error) {
	return s.Descriptions(ctx, orgID, ids)
}

// ClassificationStatusSystem reads classified_at for one entity. There is
// no non-System counterpart (the only caller, projectclassify.WaitFor, is
// a claims-free background job), so the read is inlined here rather than
// delegating. SQLite has one connection and no auth concept, so the
// assertLocalOrg gate is the whole "scope to the right tenant" story. A
// missing row is (false, false, nil) — definitive, not an error — so the
// wait can stop polling a deleted entity.
func (s *entityStore) ClassificationStatusSystem(ctx context.Context, orgID, id string) (classified, exists bool, err error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, false, err
	}
	var ts sql.NullTime
	err = s.q.QueryRowContext(ctx, `SELECT classified_at FROM entities WHERE id = ?`, id).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return ts.Valid, true, nil
}

func (s *entityStore) OwningTeamForEntitySystem(ctx context.Context, orgID, entityID string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	// Tier 1 (owning_team_id override) takes precedence; tier 2 falls back to
	// the team that owns the entity's project, but only for a team-visibility
	// project (a private/org project has no single owning team). COALESCE
	// returns NULL when neither resolves; the NullString scans that to "".
	var team sql.NullString
	err := s.q.QueryRowContext(ctx, `
		SELECT COALESCE(
			e.owning_team_id,
			(SELECT p.team_id FROM projects p
			   WHERE p.id = e.project_id
			     AND p.visibility = 'team'
			     AND p.team_id IS NOT NULL)
		)
		FROM entities e
		WHERE e.id = ?
	`, entityID).Scan(&team)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return team.String, nil
}

// scanEntityRow / scanEntityFromRows return a fresh domain.Entity per
// invocation. The two flavors mirror database/sql's *Row vs *Rows
// types since Scan signatures aren't unifiable through a common
// interface in the standard library.
func scanEntityRow(row *sql.Row) (*domain.Entity, error) {
	var e domain.Entity
	var projectID sql.NullString
	err := row.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
		&e.SnapshotJSON, &e.Description, &e.State, &projectID, &e.ClassificationRationale,
		&e.CreatedAt, &e.LastPolledAt, &e.ClosedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if projectID.Valid {
		e.ProjectID = &projectID.String
	}
	return &e, nil
}

func scanEntityFromRows(rows *sql.Rows) (*domain.Entity, error) {
	var e domain.Entity
	var projectID sql.NullString
	if err := rows.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
		&e.SnapshotJSON, &e.Description, &e.State, &projectID, &e.ClassificationRationale,
		&e.CreatedAt, &e.LastPolledAt, &e.ClosedAt); err != nil {
		return nil, err
	}
	if projectID.Valid {
		e.ProjectID = &projectID.String
	}
	return &e, nil
}
