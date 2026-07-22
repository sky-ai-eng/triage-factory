package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// entityStore is the Postgres impl of db.EntityStore. Holds two pools:
//
//   - q: app pool (tf_app, RLS-active). Every request-equivalent
//     consumer (server panels, delegate context loaders, the classifier
//     once it runs inside a user-scoped goroutine) hits this side. RLS
//     policy entities_all gates every read/write on
//     (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id)).
//
//   - admin: admin pool (supabase_admin, BYPASSRLS). System services
//     that legitimately operate across users hit the `...System`
//     methods — the tracker (writes entities for every polled repo
//     regardless of which user configured the repo) and the project
//     classifier (reads every org's unclassified entities to triage
//     them). Mirrors the AgentRunStore precedent: explicit `...System`
//     method names keep call-site intent grep-able; the impl routes
//     per-method internally.
//
// org_id is in every WHERE clause as defense in depth on both pools,
// so even the admin-pool variants only see the requested org's rows.
//
// SQL is written fresh against D3's schema: $N placeholders, JSONB
// cast on snapshot_json reads/writes, explicit timestamptz binds so
// poll cycles share a time source with the SQLite path rather than
// drifting onto Postgres's now().
type entityStore struct {
	q     queryer
	admin queryer
}

func newEntityStore(q, admin queryer) db.EntityStore {
	return &entityStore{q: q, admin: admin}
}

var _ db.EntityStore = (*entityStore)(nil)

// pgEntitySelectCols is the column list shared by every entity read.
// snapshot_json is cast to text so the Go side gets the same string
// shape SQLite returns; the caller pipes that through json.Unmarshal
// when it needs structured data.
const pgEntitySelectCols = `id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
       COALESCE(snapshot_json::text, ''), COALESCE(description, ''), state, project_id,
       COALESCE(classification_rationale, ''), created_at, last_polled_at, closed_at, poll_seq`

// --- Lookup ---

func (s *entityStore) Get(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	return getEntity(ctx, s.q, orgID, id)
}

func (s *entityStore) GetSystem(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	return getEntity(ctx, s.admin, orgID, id)
}

func getEntity(ctx context.Context, q queryer, orgID, id string) (*domain.Entity, error) {
	row := q.QueryRowContext(ctx, `SELECT `+pgEntitySelectCols+` FROM entities WHERE org_id = $1 AND id = $2`, orgID, id)
	return scanEntityRow(row)
}

// ClassificationStatusSystem reads classified_at for one entity through
// the admin pool. System-only: its sole caller, projectclassify.WaitFor,
// runs in the background spawner with no request JWT claims, so the
// app pool (tf_app) would RLS-deny the read (current_org_id() is NULL).
// org_id stays in the WHERE clause as defense in depth. A missing row is
// (false, false, nil) so the wait stops polling a deleted entity.
func (s *entityStore) ClassificationStatusSystem(ctx context.Context, orgID, id string) (classified, exists bool, err error) {
	var ts sql.NullTime
	err = s.admin.QueryRowContext(ctx, `SELECT classified_at FROM entities WHERE org_id = $1 AND id = $2`, orgID, id).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return ts.Valid, true, nil
}

func (s *entityStore) OwningTeamForEntitySystem(ctx context.Context, orgID, entityID string) (string, error) {
	// Admin pool: the router resolves ownership with no JWT claims. Tier 1
	// (owning_team_id override) wins; tier 2 falls back to the team that owns
	// the entity's project, but only a team-visibility project (private/org
	// projects have no single owning team). COALESCE yields NULL when neither
	// resolves, scanned to "" so the router falls through to its later tiers.
	var team sql.NullString
	err := s.admin.QueryRowContext(ctx, `
		SELECT COALESCE(
			e.owning_team_id,
			(SELECT p.team_id FROM projects p
			   WHERE p.id = e.project_id AND p.org_id = e.org_id
			     AND p.visibility = 'team'
			     AND p.team_id IS NOT NULL)
		)
		FROM entities e
		WHERE e.org_id = $1 AND e.id = $2
	`, orgID, entityID).Scan(&team)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return team.String, nil
}

func (s *entityStore) GetBySource(ctx context.Context, orgID, source, sourceID string) (*domain.Entity, error) {
	return getEntityBySource(ctx, s.q, orgID, source, sourceID)
}

func (s *entityStore) GetBySourceSystem(ctx context.Context, orgID, source, sourceID string) (*domain.Entity, error) {
	return getEntityBySource(ctx, s.admin, orgID, source, sourceID)
}

func getEntityBySource(ctx context.Context, q queryer, orgID, source, sourceID string) (*domain.Entity, error) {
	row := q.QueryRowContext(ctx, `SELECT `+pgEntitySelectCols+` FROM entities WHERE org_id = $1 AND source = $2 AND source_id = $3`, orgID, source, sourceID)
	return scanEntityRow(row)
}

func (s *entityStore) Descriptions(ctx context.Context, orgID string, ids []string) (map[string]string, error) {
	return entityDescriptions(ctx, s.q, orgID, ids)
}

func (s *entityStore) DescriptionsSystem(ctx context.Context, orgID string, ids []string) (map[string]string, error) {
	return entityDescriptions(ctx, s.admin, orgID, ids)
}

func entityDescriptions(ctx context.Context, q queryer, orgID string, ids []string) (map[string]string, error) {
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
	if len(unique) == 0 {
		return out, nil
	}

	// Postgres can take an array directly via = ANY($2); no manual
	// chunking needed (the parameter count cap that drives SQLite's
	// chunked path doesn't apply when the list is a single array
	// bind).
	rows, err := q.QueryContext(ctx, `
		SELECT id, COALESCE(description, '')
		FROM entities
		WHERE org_id = $1 AND id = ANY($2)
	`, orgID, pgUUIDArray(unique))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, desc string
		if err := rows.Scan(&id, &desc); err != nil {
			return nil, err
		}
		if desc != "" {
			out[id] = desc
		}
	}
	return out, rows.Err()
}

func (s *entityStore) ListUnclassified(ctx context.Context, orgID string) ([]domain.Entity, error) {
	return listUnclassifiedEntities(ctx, s.q, orgID)
}

func (s *entityStore) ListUnclassifiedSystem(ctx context.Context, orgID string) ([]domain.Entity, error) {
	return listUnclassifiedEntities(ctx, s.admin, orgID)
}

func listUnclassifiedEntities(ctx context.Context, q queryer, orgID string) ([]domain.Entity, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+pgEntitySelectCols+`
		FROM entities
		WHERE org_id = $1 AND project_id IS NULL AND classified_at IS NULL AND state = 'active'
		ORDER BY created_at ASC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntityList(rows)
}

func (s *entityStore) ListActive(ctx context.Context, orgID, source string) ([]domain.Entity, error) {
	return listActiveEntities(ctx, s.q, orgID, source)
}

func (s *entityStore) ListActiveSystem(ctx context.Context, orgID, source string) ([]domain.Entity, error) {
	return listActiveEntities(ctx, s.admin, orgID, source)
}

func listActiveEntities(ctx context.Context, q queryer, orgID, source string) ([]domain.Entity, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+pgEntitySelectCols+`
		FROM entities
		WHERE org_id = $1 AND source = $2 AND state = 'active'
		ORDER BY last_polled_at ASC
	`, orgID, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntityList(rows)
}

// jiraTeamProjectMembershipExists scopes a Jira entities row (alias e)
// to the projects attached to the viewer's teams. A team "attaches" a
// Jira project by configuring its status rules, so an entity belongs in
// the viewer's discovery deck iff a jira_project_status_rules row exists
// for the entity's project key (the prefix of source_id, e.g. "PROJ" in
// "PROJ-123"). split_part(source_id, '-', 1) extracts the key — Jira keys
// are hyphen-free, so the first segment is always the project key.
//
// The team scoping is free: under the app pool (tf_app, RLS active) the
// jira_rules_select policy already constrains visible rows to the
// viewer's team memberships, so the EXISTS auto-scopes with no explicit
// team_id — the same RLS-does-the-scoping pattern as the factory belt's
// task-membership semi-join. The teams join binds org_id as
// defense-in-depth (jira_project_status_rules has no org_id column) so
// the filter holds even on the admin pool where RLS is bypassed.
const jiraTeamProjectMembershipExists = `EXISTS (
	SELECT 1 FROM jira_project_status_rules jr
	JOIN teams tm ON tm.id = jr.team_id
	WHERE jr.project_key = split_part(e.source_id, '-', 1)
	  AND tm.org_id = $1
)`

// jiraTeamProjectMembershipForTeam is the single-team variant of
// jiraTeamProjectMembershipExists used by the carry-over deck's team
// filter: the project must be tracked by the specific team in $2 (still
// org-bound, still under jira_rules_select RLS so $2 must be one of the
// viewer's teams). Mirrors the unfiltered fragment's defense-in-depth
// org bind.
const jiraTeamProjectMembershipForTeam = `EXISTS (
	SELECT 1 FROM jira_project_status_rules jr
	JOIN teams tm ON tm.id = jr.team_id
	WHERE jr.project_key = split_part(e.source_id, '-', 1)
	  AND tm.org_id = $1
	  AND jr.team_id = $2
)`

func (s *entityStore) ListActiveJiraTeamScoped(ctx context.Context, orgID, teamID string) ([]domain.Entity, error) {
	// jira_rules_select RLS already scopes the project semi-join to the
	// viewer's teams. The optional teamID narrows it further to a single
	// team's tracked projects — load-bearing for the carry-over deck's
	// team filter (otherwise a deck switched to team B still surfaces
	// team A's tickets, and a claim then stamps a task on B for a project
	// B doesn't track). $2 is referenced only when teamID is set.
	membership := jiraTeamProjectMembershipExists
	args := []any{orgID}
	if teamID != "" {
		membership = jiraTeamProjectMembershipForTeam
		args = append(args, teamID)
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgEntitySelectCols+`
		FROM entities e
		WHERE e.org_id = $1 AND e.source = 'jira' AND e.state = 'active'
		  AND `+membership+`
		ORDER BY e.last_polled_at ASC
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntityList(rows)
}

func (s *entityStore) ListProjectPanel(ctx context.Context, orgID, projectID string) ([]domain.ProjectPanelEntity, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
		       state, COALESCE(classification_rationale, ''), created_at, last_polled_at
		FROM entities
		WHERE org_id = $1 AND project_id = $2 AND state = 'active'
		ORDER BY last_polled_at DESC NULLS LAST
	`, orgID, projectID)
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
	return findOrCreateEntity(ctx, s.q, orgID, source, sourceID, kind, title, url)
}

func (s *entityStore) FindOrCreateSystem(ctx context.Context, orgID, source, sourceID, kind, title, url string) (*domain.Entity, bool, error) {
	return findOrCreateEntity(ctx, s.admin, orgID, source, sourceID, kind, title, url)
}

func findOrCreateEntity(ctx context.Context, q queryer, orgID, source, sourceID, kind, title, url string) (*domain.Entity, bool, error) {
	existing, err := getEntityBySource(ctx, q, orgID, source, sourceID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}

	id := uuid.New().String()
	now := time.Now()
	_, err = q.ExecContext(ctx, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, state, created_at, last_polled_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'active', $8, $9)
	`, id, orgID, source, sourceID, kind, title, url, now, now)
	if err != nil {
		// Concurrent first-discovery race: the unique key
		// (org_id, source, source_id) just fired. Re-read so both
		// callers see a populated entity. If the re-read also
		// fails, surface the original error.
		existing, err2 := getEntityBySource(ctx, q, orgID, source, sourceID)
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
	// The blind-write system variant was removed with TFAC-579: every
	// tracker snapshot write goes through UpdateSnapshotCASSystem. This
	// app-pool variant survives for non-tracker writers with no poll_seq
	// read-then-write cycle to protect.
	_, err := s.q.ExecContext(ctx, `
		UPDATE entities
		SET snapshot_json = $1::jsonb, last_polled_at = $2
		WHERE org_id = $3 AND id = $4
	`, snapshotJSON, time.Now(), orgID, id)
	return err
}

// UpdateSnapshotCASSystem is the tracker's snapshot write with a poll_seq
// CAS (TFAC-579): the WHERE clause pins expectedPollSeq alongside
// org_id/id, so a straggler ex-leader's late write (its expectedPollSeq is
// stale by the time it lands) affects zero rows instead of overwriting a
// newer snapshot. poll_seq bumps by 1 on every successful write so the
// caller's next-read-then-write cycle has a fresh value to CAS against.
func (s *entityStore) UpdateSnapshotCASSystem(ctx context.Context, orgID, id, snapshotJSON string, expectedPollSeq int64) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `
		UPDATE entities
		SET snapshot_json = $1::jsonb, last_polled_at = $2, poll_seq = poll_seq + 1
		WHERE org_id = $3 AND id = $4 AND poll_seq = $5
	`, snapshotJSON, time.Now(), orgID, id, expectedPollSeq)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *entityStore) PatchSnapshot(ctx context.Context, orgID, id, snapshotJSON string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE entities SET snapshot_json = $1::jsonb WHERE org_id = $2 AND id = $3`, snapshotJSON, orgID, id)
	return err
}

func (s *entityStore) UpdateTitle(ctx context.Context, orgID, id, title string) error {
	return updateEntityTitle(ctx, s.q, orgID, id, title)
}

func (s *entityStore) UpdateTitleSystem(ctx context.Context, orgID, id, title string) error {
	return updateEntityTitle(ctx, s.admin, orgID, id, title)
}

func updateEntityTitle(ctx context.Context, q queryer, orgID, id, title string) error {
	_, err := q.ExecContext(ctx, `UPDATE entities SET title = $1 WHERE org_id = $2 AND id = $3`, title, orgID, id)
	return err
}

func (s *entityStore) UpdateDescription(ctx context.Context, orgID, id, description string) error {
	return updateEntityDescription(ctx, s.q, orgID, id, description)
}

func (s *entityStore) UpdateDescriptionSystem(ctx context.Context, orgID, id, description string) error {
	return updateEntityDescription(ctx, s.admin, orgID, id, description)
}

func updateEntityDescription(ctx context.Context, q queryer, orgID, id, description string) error {
	_, err := q.ExecContext(ctx, `UPDATE entities SET description = $1 WHERE org_id = $2 AND id = $3`, description, orgID, id)
	return err
}

// UpdateURLSystem sets the entity's url through the admin pool. No
// non-System counterpart exists (see the interface doc) — the only caller
// (ee/slack's permalink resolver) runs detached with no JWT claims, so
// there is no app-pool-equivalent request context to route through.
func (s *entityStore) UpdateURLSystem(ctx context.Context, orgID, id, url string) error {
	_, err := s.admin.ExecContext(ctx, `UPDATE entities SET url = $1 WHERE org_id = $2 AND id = $3`, url, orgID, id)
	return err
}

func (s *entityStore) AssignProject(ctx context.Context, orgID, id string, projectID *string, rationale string) error {
	return assignEntityProject(ctx, s.q, orgID, id, projectID, rationale)
}

func (s *entityStore) AssignProjectSystem(ctx context.Context, orgID, id string, projectID *string, rationale string) error {
	return assignEntityProject(ctx, s.admin, orgID, id, projectID, rationale)
}

func assignEntityProject(ctx context.Context, q queryer, orgID, id string, projectID *string, rationale string) error {
	var projectArg any
	if projectID != nil && *projectID != "" {
		projectArg = *projectID
	}
	var rationaleArg any
	if rationale != "" {
		rationaleArg = rationale
	}
	res, err := q.ExecContext(ctx, `
		UPDATE entities
		SET project_id = $1,
		    classification_rationale = $2,
		    classified_at = now()
		WHERE org_id = $3 AND id = $4
	`, projectArg, rationaleArg, orgID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists int
		err := q.QueryRowContext(ctx, `SELECT 1 FROM entities WHERE org_id = $1 AND id = $2`, orgID, id).Scan(&exists)
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
	return markEntityClosed(ctx, s.q, orgID, id)
}

func (s *entityStore) MarkClosedSystem(ctx context.Context, orgID, id string) error {
	return markEntityClosed(ctx, s.admin, orgID, id)
}

func markEntityClosed(ctx context.Context, q queryer, orgID, id string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE entities SET state = 'closed', closed_at = $1 WHERE org_id = $2 AND id = $3
	`, time.Now(), orgID, id)
	return err
}

func (s *entityStore) Close(ctx context.Context, orgID, id string) error {
	return closeActiveEntity(ctx, s.q, orgID, id)
}

func (s *entityStore) CloseSystem(ctx context.Context, orgID, id string) error {
	return closeActiveEntity(ctx, s.admin, orgID, id)
}

func closeActiveEntity(ctx context.Context, q queryer, orgID, id string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE entities SET state = 'closed', closed_at = $1 WHERE org_id = $2 AND id = $3 AND state = 'active'
	`, time.Now(), orgID, id)
	return err
}

func (s *entityStore) Reactivate(ctx context.Context, orgID, id string) (bool, error) {
	return reactivateEntity(ctx, s.q, orgID, id)
}

func (s *entityStore) ReactivateSystem(ctx context.Context, orgID, id string) (bool, error) {
	return reactivateEntity(ctx, s.admin, orgID, id)
}

func reactivateEntity(ctx context.Context, q queryer, orgID, id string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE entities SET state = 'active', closed_at = NULL WHERE org_id = $1 AND id = $2 AND state = 'closed'
	`, orgID, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// pgUUIDArray formats a Go string slice as a Postgres uuid[] literal
// for binding through a single $N parameter. The pgx stdlib driver
// accepts the textual array form for typed-array columns. Quoting
// rules: ids are uuid-shaped (no commas, braces, or backslashes), so
// raw element values are safe to emit inside the {…} envelope without
// escaping.
func pgUUIDArray(ids []string) string {
	if len(ids) == 0 {
		return "{}"
	}
	return "{" + strings.Join(ids, ",") + "}"
}

// pgIntArray formats a Go int slice as a Postgres array literal (bigint[],
// per run_messages.id) for binding through a single $N parameter, the same
// textual-literal technique as pgUUIDArray. No escaping needed — ints have
// no characters the {…} envelope needs quoted.
func pgIntArray(ids []int) string {
	if len(ids) == 0 {
		return "{}"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func scanEntityRow(row *sql.Row) (*domain.Entity, error) {
	var e domain.Entity
	var projectID sql.NullString
	err := row.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
		&e.SnapshotJSON, &e.Description, &e.State, &projectID, &e.ClassificationRationale,
		&e.CreatedAt, &e.LastPolledAt, &e.ClosedAt, &e.PollSeq)
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

func scanEntityList(rows *sql.Rows) ([]domain.Entity, error) {
	out := []domain.Entity{}
	for rows.Next() {
		var e domain.Entity
		var projectID sql.NullString
		if err := rows.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
			&e.SnapshotJSON, &e.Description, &e.State, &projectID, &e.ClassificationRationale,
			&e.CreatedAt, &e.LastPolledAt, &e.ClosedAt, &e.PollSeq); err != nil {
			return nil, err
		}
		if projectID.Valid {
			e.ProjectID = &projectID.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
