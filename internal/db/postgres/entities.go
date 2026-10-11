package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
//     consumer (the Jira stock deck, the factory handler, task-creation
//     handlers) hits this side. RLS policy entities_all gates every
//     read/write on
//     (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id)).
//
//   - admin: admin pool (supabase_admin, BYPASSRLS). System services
//     that legitimately operate across users hit the `...System`
//     methods — the tracker, which writes entities for every polled repo
//     regardless of which user configured the repo. Mirrors the
//     ConversationStore precedent: explicit `...System` method names keep
//     call-site intent grep-able; the impl routes per-method internally.
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
const pgEntitySelectCols = `id, source, scope, source_id, COALESCE(external_id, ''), kind,
       COALESCE(title, ''), COALESCE(url, ''),
       COALESCE(snapshot_json::text, ''), COALESCE(description, ''), state,
       created_at, last_polled_at, closed_at, poll_seq`

// pgEntityKeyOrder is the key lookup's preference among rows sharing a key:
// the active one (there is at most one), otherwise the most recently closed.
// id breaks the tie so the answer is stable.
const pgEntityKeyOrder = `ORDER BY (state = 'active') DESC, closed_at DESC NULLS LAST, created_at DESC, id DESC`

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

func (s *entityStore) OwningTeamForEntitySystem(ctx context.Context, orgID, entityID string) (string, error) {
	// Admin pool: the router resolves ownership with no JWT claims. Returns
	// the owning_team_id override, or "" when unset, scanned via NullString
	// so the router falls through to its later tiers.
	var team sql.NullString
	err := s.admin.QueryRowContext(ctx, `
		SELECT owning_team_id
		FROM entities
		WHERE org_id = $1 AND id = $2
	`, orgID, entityID).Scan(&team)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return team.String, nil
}

// StampOwningTeamIfUnsetSystem fills owning_team_id only where it is still
// NULL. The IS NULL predicate is the concurrency story as much as the semantic
// one: it lives in the UPDATE rather than in a read-then-write, so an executor
// recording a bot-opened PR and a control pod's poller minting the same entity
// cannot lose each other's write — whichever commits first wins the row, and
// RowsAffected tells the loser it lost.
func (s *entityStore) StampOwningTeamIfUnsetSystem(ctx context.Context, orgID, entityID, teamID string) (bool, error) {
	if teamID == "" {
		return false, nil
	}
	res, err := s.admin.ExecContext(ctx, `
		UPDATE entities
		SET owning_team_id = $1
		WHERE org_id = $2 AND id = $3 AND owning_team_id IS NULL
	`, teamID, orgID, entityID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// StampCommissionedByIfUnsetSystem is the same statement against the
// provenance column: the human whose ask produced this pull request. Written
// beside the owning-team stamp because that is the one moment both values are
// in hand — the columns are otherwise unrelated, and neither implies the
// other.
func (s *entityStore) StampCommissionedByIfUnsetSystem(ctx context.Context, orgID, entityID, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	res, err := s.admin.ExecContext(ctx, `
		UPDATE entities
		SET commissioned_by_user_id = $1
		WHERE org_id = $2 AND id = $3 AND commissioned_by_user_id IS NULL
	`, userID, orgID, entityID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *entityStore) GetBySource(ctx context.Context, orgID, source, scope, sourceID string) (*domain.Entity, error) {
	return entityByKey(ctx, s.q, orgID, source, scope, sourceID, "")
}

func (s *entityStore) GetBySourceSystem(ctx context.Context, orgID, source, scope, sourceID string) (*domain.Entity, error) {
	return entityByKey(ctx, s.admin, orgID, source, scope, sourceID, "")
}

// entityByKey is the key lookup. A non-empty externalID skips a row that
// carries a different id: that row is another object that had or has the key.
func entityByKey(ctx context.Context, q queryer, orgID, source, scope, sourceID, externalID string) (*domain.Entity, error) {
	return scanEntityRow(q.QueryRowContext(ctx, `
		SELECT `+pgEntitySelectCols+` FROM entities
		WHERE org_id = $1 AND source = $2 AND scope = $3 AND source_id = $4
		  AND ($5 = '' OR external_id IS NULL OR external_id = $5)
		`+pgEntityKeyOrder+` LIMIT 1`,
		orgID, source, scope, sourceID, externalID))
}

func entityByExternalID(ctx context.Context, q queryer, orgID, source, scope, externalID string) (*domain.Entity, error) {
	return scanEntityRow(q.QueryRowContext(ctx, `
		SELECT `+pgEntitySelectCols+` FROM entities
		WHERE org_id = $1 AND source = $2 AND scope = $3 AND external_id = $4
		ORDER BY id LIMIT 1`,
		orgID, source, scope, externalID))
}

func (s *entityStore) GetByExternalIDSystem(ctx context.Context, orgID, source, scope, externalID string) (*domain.Entity, error) {
	if externalID == "" {
		return nil, nil
	}
	return entityByExternalID(ctx, s.admin, orgID, source, scope, externalID)
}

// StampExternalIDSystem fills a NULL external_id; see the interface doc. The
// guard admits the id the row already carries, so a repeat stamp returns the
// row rather than reading as a decline.
func (s *entityStore) StampExternalIDSystem(ctx context.Context, orgID, entityID, externalID string) (*domain.Entity, error) {
	if externalID == "" {
		return nil, errors.New("stamp external id: empty id")
	}
	e, err := scanEntityRow(s.admin.QueryRowContext(ctx, `
		UPDATE entities SET external_id = $1
		WHERE org_id = $2 AND id = $3 AND (external_id IS NULL OR external_id = $1)
		RETURNING `+pgEntitySelectCols,
		externalID, orgID, entityID))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %s", db.ErrEntityIdentityAmbiguous, externalID)
		}
		return nil, err
	}
	if e != nil {
		return e, nil
	}
	existing, err := getEntity(ctx, s.admin, orgID, entityID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, sql.ErrNoRows
	}
	return nil, nil
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

func (s *entityStore) ListActive(ctx context.Context, orgID, source string) ([]domain.Entity, error) {
	return listActiveEntities(ctx, s.q, orgID, source)
}

func (s *entityStore) ListActiveSystem(ctx context.Context, orgID, source string) ([]domain.Entity, error) {
	return listActiveEntities(ctx, s.admin, orgID, source)
}

// ListActiveTerminalCandidatesSystem selects active entities whose stored
// snapshot already reads terminal, unpolled for at least unpolledFor, with
// no terminating close ready, leased or parked in the queue — a parked one
// still holds the entity's key, and is the alarm itself. Admin pool: the checker is a
// background job with no JWT claims. snapshot_json is jsonb, so `->>` yields
// NULL for a missing key or an empty object — COALESCE makes those a plain
// non-match rather than a NULL-propagating predicate, and the IS NOT NULL
// guard skips rows that never stored a snapshot at all. The unpolled cutoff
// is subtracted from the server clock, the one that stamped last_polled_at.
func (s *entityStore) ListActiveTerminalCandidatesSystem(ctx context.Context, orgID string, jiraDone []domain.JiraStatusRef, linearDone []domain.LinearStateRef, unpolledFor time.Duration, limit int) ([]domain.Entity, error) {
	args := []any{orgID, unpolledFor.Seconds(), domain.EntityCloseSettlingEventTypes()}
	// Each arm is omitted rather than emitted empty: `IN ()` is a syntax
	// error, and a ref set with no ids (or no names) genuinely has nothing to
	// match on that side. With neither, no Jira (or Linear) entity can be
	// terminal. Both halves are user-configured text, so they bind as
	// placeholders rather than riding an array literal. path is a jsonb
	// accessor chain ending in ->>.
	arm := func(path string, values []string) string {
		placeholders := make([]string, len(values))
		for i, v := range values {
			args = append(args, v)
			placeholders[i] = "$" + strconv.Itoa(len(args))
		}
		return `snapshot_json` + path + ` IN (` + strings.Join(placeholders, ", ") + `)`
	}
	var matches []string
	if ids := domain.JiraStatusIDs(jiraDone); len(ids) > 0 {
		matches = append(matches, arm("->>'status_id'", ids))
	}
	if names := domain.JiraStatusNames(jiraDone); len(names) > 0 {
		matches = append(matches, arm("->>'status'", names))
	}
	jiraArm := ""
	if len(matches) > 0 {
		jiraArm = ` OR (source = 'jira' AND (` + strings.Join(matches, " OR ") + `))`
	}
	var linearMatches []string
	if ids := domain.LinearStateIDs(linearDone); len(ids) > 0 {
		linearMatches = append(linearMatches, arm("->'state'->>'id'", ids))
	}
	if names := domain.LinearStateNames(linearDone); len(names) > 0 {
		linearMatches = append(linearMatches, arm("->'state'->>'name'", names))
	}
	linearArm := ""
	if len(linearMatches) > 0 {
		linearArm = ` OR (source = 'linear' AND (` + strings.Join(linearMatches, " OR ") + `))`
	}
	limitClause := ""
	if limit > 0 {
		args = append(args, limit)
		limitClause = " LIMIT $" + strconv.Itoa(len(args))
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+pgEntitySelectCols+`
		FROM entities
		WHERE org_id = $1 AND state = 'active' AND snapshot_json IS NOT NULL
		  AND (last_polled_at IS NULL OR last_polled_at < now() - make_interval(secs => $2::double precision))
		  AND NOT EXISTS (
		    SELECT 1 FROM event_queue q
		    WHERE q.org_id = entities.org_id AND q.entity_id = entities.id
		      AND q.status IN ('ready', 'leased', 'parked')
		      AND q.event_type = ANY($3)
		  )
		  AND (
		    (source = 'github' AND (
		       snapshot_json->>'merged' = 'true'
		       OR upper(COALESCE(snapshot_json->>'state', '')) IN ('CLOSED', 'MERGED')
		    ))`+jiraArm+linearArm+`
		  )
		ORDER BY created_at ASC, id ASC`+limitClause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntityList(rows)
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

// --- Mutation ---

func (s *entityStore) FindOrCreate(ctx context.Context, orgID, source, scope, sourceID, externalID, kind, title, url string) (*domain.Entity, bool, error) {
	return findOrCreateEntity(ctx, s.q, orgID, source, scope, sourceID, externalID, kind, title, url)
}

func (s *entityStore) FindOrCreateSystem(ctx context.Context, orgID, source, scope, sourceID, externalID, kind, title, url string) (*domain.Entity, bool, error) {
	return findOrCreateEntity(ctx, s.admin, orgID, source, scope, sourceID, externalID, kind, title, url)
}

func findOrCreateEntity(ctx context.Context, q queryer, orgID, source, scope, sourceID, externalID, kind, title, url string) (*domain.Entity, bool, error) {
	if scope == "" {
		return nil, false, fmt.Errorf("%w: %s %s", db.ErrEntityScopeRequired, source, sourceID)
	}
	find := func() (*domain.Entity, error) {
		if externalID != "" {
			if e, err := entityByExternalID(ctx, q, orgID, source, scope, externalID); err != nil || e != nil {
				return e, err
			}
		}
		return entityByKey(ctx, q, orgID, source, scope, sourceID, externalID)
	}
	existing, err := find()
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}

	id := uuid.New().String()
	now := time.Now().UTC()
	// DO NOTHING rather than a raised violation: a raised one would abort a
	// caller's enclosing transaction, and a lost race is an answer here, not
	// an error. The re-read below decides what the loser sees.
	res, err := q.ExecContext(ctx, `
		INSERT INTO entities (id, org_id, source, scope, source_id, external_id, kind, title, url, state, created_at, last_polled_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'active', $10, $11)
		ON CONFLICT DO NOTHING
	`, id, orgID, source, scope, sourceID, nullString(externalID), kind, title, url, now, now)
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	if n == 0 {
		// A concurrent first discovery won, or an active row already answers
		// to the key. The re-read finds the former; the latter carries a
		// different id, so the key lookup skips it and the create is refused.
		existing, err := find()
		if err != nil {
			return nil, false, err
		}
		if existing != nil {
			return existing, false, nil
		}
		return nil, false, fmt.Errorf("%w: %s %s in %s", db.ErrEntityKeyOccupied, source, sourceID, scope)
	}

	return &domain.Entity{
		ID:           id,
		Source:       source,
		Scope:        scope,
		SourceID:     sourceID,
		ExternalID:   externalID,
		Kind:         kind,
		Title:        title,
		URL:          url,
		State:        "active",
		CreatedAt:    now,
		LastPolledAt: &now,
	}, true, nil
}

// RenameSystem — see the interface doc. The FOR UPDATE on the identity row is
// the lock: two detections of the same rename serialize there, and the second
// re-reads the key the first committed and returns a no-op.
func (s *entityStore) RenameSystem(ctx context.Context, orgID, source, scope, externalID, newKey, newURL string) (domain.EntityRenameOutcome, error) {
	if externalID == "" {
		return domain.EntityRenameOutcome{}, nil
	}
	if newKey == "" {
		return domain.EntityRenameOutcome{}, errors.New("rename entity: empty key")
	}
	if scope == "" {
		return domain.EntityRenameOutcome{}, fmt.Errorf("%w: %s %s", db.ErrEntityScopeRequired, source, newKey)
	}
	var out domain.EntityRenameOutcome
	err := inTx(ctx, s.admin, func(tx queryer) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, source_id, COALESCE(url, '') FROM entities
			WHERE org_id = $1 AND source = $2 AND scope = $3 AND external_id = $4
			ORDER BY id
			FOR UPDATE`, orgID, source, scope, externalID)
		if err != nil {
			return err
		}
		type held struct{ id, key, url string }
		var found []held
		for rows.Next() {
			var h held
			if err := rows.Scan(&h.id, &h.key, &h.url); err != nil {
				rows.Close()
				return err
			}
			found = append(found, h)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		switch {
		case len(found) == 0:
			return nil
		case len(found) > 1:
			return fmt.Errorf("%w: %s", db.ErrEntityIdentityAmbiguous, externalID)
		case found[0].key == newKey:
			return nil
		}
		row := found[0]

		var holder string
		err = tx.QueryRowContext(ctx, `
			SELECT id FROM entities
			WHERE org_id = $1 AND source = $2 AND scope = $3 AND source_id = $4
			  AND state = 'active' AND id <> $5
			LIMIT 1`, orgID, source, scope, newKey, row.id).Scan(&holder)
		if err == nil {
			return fmt.Errorf("%w: %s %s in %s", db.ErrEntityKeyOccupied, source, newKey, scope)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		url := row.url
		if newURL != "" {
			url = newURL
		}
		var pollSeq int64
		if err := tx.QueryRowContext(ctx,
			`UPDATE entities SET source_id = $1, url = $2, poll_seq = poll_seq + 1 WHERE org_id = $3 AND id = $4 RETURNING poll_seq`,
			newKey, url, orgID, row.id).Scan(&pollSeq); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: %s %s in %s: %v", db.ErrEntityKeyOccupied, source, newKey, scope, err)
			}
			return fmt.Errorf("rename entity %s -> %s: %w", row.key, newKey, err)
		}
		if err := rewriteEntityArtifacts(ctx, tx, orgID, source, scope, domain.EntityArtifactResource(source, scope, externalID), row.key, newKey); err != nil {
			return err
		}
		if err := rewriteEntityActionURLs(ctx, tx, orgID, source, scope, row.url, url); err != nil {
			return err
		}
		out = domain.EntityRenameOutcome{Renamed: true, EntityID: row.id, From: row.key, To: newKey, PollSeq: pollSeq}
		return nil
	})
	if err != nil {
		return domain.EntityRenameOutcome{}, err
	}
	return out, nil
}

// rewriteEntityArtifacts moves the Target of the source's artifacts in the
// entity's scope keyed on the entity's id off the old key. resource is the
// dedup-key segment that id is written as (domain.EntityArtifactResource); the
// key carries it, so it does not move. The SQL over-approximates and
// domain.ArtifactKeyHasResource decides.
func rewriteEntityArtifacts(ctx context.Context, q queryer, orgID, source, scope, resource, from, to string) error {
	if resource == "" || from == to {
		return nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, dedup_key FROM artifacts
		WHERE org_id = $1 AND provider = $2 AND scope = $5 AND target = $3 AND strpos(dedup_key, $4) > 0`,
		orgID, source, from, resource, scope)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id, key string
		if err := rows.Scan(&id, &key); err != nil {
			rows.Close()
			return err
		}
		if domain.ArtifactKeyHasResource(key, source, resource) {
			ids = append(ids, id)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := q.ExecContext(ctx,
			`UPDATE artifacts SET target = $1, updated_at = now() WHERE id = $2 AND org_id = $3`,
			to, id, orgID); err != nil {
			return fmt.Errorf("rewrite artifact %s for %s -> %s: %w", id, from, to, err)
		}
	}
	return nil
}

// rewriteEntityActionURLs moves the audit ledger's pointer for actions in the
// entity's scope whose link resolves to the entity's old url. Only current_url
// is written; the record of the act is not. The pointer's current value is the
// rewrite base, so consecutive renames chain.
func rewriteEntityActionURLs(ctx context.Context, q queryer, orgID, source, scope, from, to string) error {
	if from == "" || to == "" || from == to {
		return nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, COALESCE(current_url, url) FROM external_actions
		WHERE org_id = $1 AND provider = $2 AND scope = $4 AND starts_with(COALESCE(current_url, url, ''), $3)`,
		orgID, source, from, scope)
	if err != nil {
		return err
	}
	type pending struct{ id, url string }
	var updates []pending
	for rows.Next() {
		var id, u string
		if err := rows.Scan(&id, &u); err != nil {
			rows.Close()
			return err
		}
		if moved, ok := domain.RewriteEntityURL(u, from, to); ok {
			updates = append(updates, pending{id: id, url: moved})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range updates {
		if _, err := q.ExecContext(ctx,
			`UPDATE external_actions SET current_url = $1 WHERE id = $2 AND org_id = $3`,
			u.url, u.id, orgID); err != nil {
			return fmt.Errorf("rewrite external action pointer %s: %w", u.id, err)
		}
	}
	return nil
}

// scanWrittenEntity decodes an id-keyed UPDATE … RETURNING. scanEntityRow maps
// a scanned-nothing to (nil, nil) — the read convention — so the write layer
// turns that back into sql.ErrNoRows, this store's miss sentinel for an id a
// caller resolved and then wrote against. Under the app pool that also covers
// a row RLS hides, which is the same answer for the same reason.
func scanWrittenEntity(row *sql.Row) (domain.Entity, error) {
	e, err := scanEntityRow(row)
	if err != nil {
		return domain.Entity{}, err
	}
	if e == nil {
		return domain.Entity{}, sql.ErrNoRows
	}
	return *e, nil
}

// UpdateSnapshotCASSystem is the tracker's snapshot write with a poll_seq
// CAS (TFAC-579): the WHERE clause pins expectedPollSeq alongside
// org_id/id, so a straggler ex-leader's late write (its expectedPollSeq is
// stale by the time it lands) affects zero rows instead of overwriting a
// newer snapshot. poll_seq bumps by 1 on every successful write so the
// caller's next-read-then-write cycle has a fresh value to CAS against.
func (s *entityStore) UpdateSnapshotCASSystem(ctx context.Context, orgID, id, snapshotJSON string, expectedPollSeq int64) (bool, error) {
	return updateSnapshotCAS(ctx, s.admin, orgID, id, snapshotJSON, expectedPollSeq)
}

// updateSnapshotCAS is the CAS statement itself, shared with the event
// queue's EnqueueBatchWithSnapshotCAS — which runs it against its own
// transaction so the snapshot advance and the transitions diffed against
// it commit together. One copy of the SQL so the two callers cannot drift
// on what "the CAS" means.
func updateSnapshotCAS(ctx context.Context, q queryer, orgID, id, snapshotJSON string, expectedPollSeq int64) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE entities
		SET snapshot_json = $1::jsonb, last_polled_at = $2, poll_seq = poll_seq + 1
		WHERE org_id = $3 AND id = $4 AND poll_seq = $5
	`, snapshotJSON, time.Now().UTC(), orgID, id, expectedPollSeq)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *entityStore) PatchSnapshot(ctx context.Context, orgID, id, snapshotJSON string) (domain.Entity, error) {
	return scanWrittenEntity(s.q.QueryRowContext(ctx,
		`UPDATE entities SET snapshot_json = $1::jsonb WHERE org_id = $2 AND id = $3
		 RETURNING `+pgEntitySelectCols,
		snapshotJSON, orgID, id))
}

// MarkPolledSystem stamps last_polled_at alone — no snapshot, no poll_seq
// bump, so it is deliberately outside the CAS the snapshot writes use. There
// is nothing for a CAS to protect here: the column is monotonic wall-clock and
// a straggler writing an older stamp only makes the row look *more* stale,
// which costs a redundant re-check rather than a lost transition.
// MarkPolledSystem is exempt from the returned-row rule; see the interface doc.
func (s *entityStore) MarkPolledSystem(ctx context.Context, orgID, id string) error {
	_, err := s.admin.ExecContext(ctx,
		`UPDATE entities SET last_polled_at = $1 WHERE org_id = $2 AND id = $3`,
		time.Now().UTC(), orgID, id)
	return err
}

// MergeDuplicateEntitiesSystem — see the interface doc. Both rows are locked
// (FOR UPDATE, in id order so two merges of one pair cannot deadlock) before
// the survivor is decided, so a concurrent writer cannot move either row
// between the decision and the fold.
func (s *entityStore) MergeDuplicateEntitiesSystem(ctx context.Context, orgID, entityID, otherID string) (string, error) {
	if entityID == otherID {
		return "", fmt.Errorf("merge entities: %s with itself", entityID)
	}
	var survivorID string
	err := inTx(ctx, s.admin, func(q queryer) error {
		rows, err := q.QueryContext(ctx, `
			SELECT `+pgEntitySelectCols+` FROM entities
			WHERE org_id = $1 AND id IN ($2, $3)
			ORDER BY id
			FOR UPDATE`, orgID, entityID, otherID)
		if err != nil {
			return err
		}
		found, err := scanEntityList(rows)
		if err != nil {
			return err
		}
		if len(found) != 2 {
			return sql.ErrNoRows
		}
		survivor, loser, err := db.DuplicateEntityPair(found[0], found[1])
		if err != nil {
			return err
		}
		survivorID = survivor.ID

		// The loser's active tasks whose slot the survivor already holds are
		// dismissed in place first, which takes them out of the partial unique
		// index the repoint below would otherwise trip.
		if _, err := q.ExecContext(ctx, `
			UPDATE tasks SET status = 'dismissed', closed_at = $1, close_reason = $2
			WHERE org_id = $3 AND entity_id = $4 AND status NOT IN ('done','dismissed')
			  AND EXISTS (SELECT 1 FROM tasks s WHERE s.org_id = $3 AND s.entity_id = $5
			              AND s.event_type = tasks.event_type AND s.dedup_key = tasks.dedup_key
			              AND s.status NOT IN ('done','dismissed'))`,
			time.Now().UTC(), db.DuplicateEntityMergedCloseReason, orgID, loser.ID, survivor.ID); err != nil {
			return fmt.Errorf("dismiss duplicate tasks: %w", err)
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE blueprint_runs SET cancel_requested = true
			WHERE org_id = $1 AND status = 'running' AND cancel_requested = false AND id IN (
				SELECT c.blueprint_run_id FROM conversations c JOIN tasks t ON t.id = c.task_id
				WHERE t.org_id = $1 AND t.entity_id = $2 AND t.close_reason = $3
				  AND c.blueprint_run_id IS NOT NULL AND `+db.UnsettledConversationSQL("c")+`)`,
			orgID, loser.ID, db.DuplicateEntityMergedCloseReason); err != nil {
			return fmt.Errorf("cancel duplicate tasks' runs: %w", err)
		}
		for _, table := range []string{"tasks", "events", "event_queue", "pending_firings"} {
			if _, err := q.ExecContext(ctx,
				`UPDATE `+table+` SET entity_id = $1 WHERE org_id = $2 AND entity_id = $3`,
				survivor.ID, orgID, loser.ID); err != nil {
				return fmt.Errorf("move %s: %w", table, err)
			}
		}
		if _, err := q.ExecContext(ctx, `
			INSERT INTO conversation_memory_entities (org_id, conversation_id, entity_id, role, created_at)
			SELECT org_id, conversation_id, $1, role, created_at FROM conversation_memory_entities
			WHERE org_id = $2 AND entity_id = $3
			ON CONFLICT (conversation_id, entity_id) DO UPDATE SET role = EXCLUDED.role
			WHERE `+fmt.Sprintf(memoryRoleRankCASE, "EXCLUDED.role")+` > `+fmt.Sprintf(memoryRoleRankCASE, "conversation_memory_entities.role"),
			survivor.ID, orgID, loser.ID); err != nil {
			return fmt.Errorf("move memory links: %w", err)
		}
		if _, err := q.ExecContext(ctx,
			`DELETE FROM conversation_memory_entities WHERE org_id = $1 AND entity_id = $2`, orgID, loser.ID); err != nil {
			return err
		}
		// A link between the two rows would become a self-link; it is dropped.
		if _, err := q.ExecContext(ctx, `
			DELETE FROM entity_links
			WHERE org_id = $1 AND ((from_entity_id = $2 AND to_entity_id = $3) OR (from_entity_id = $3 AND to_entity_id = $2))`,
			orgID, loser.ID, survivor.ID); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `
			INSERT INTO entity_links (from_entity_id, to_entity_id, kind, origin, org_id, created_at)
			SELECT CASE WHEN from_entity_id = $2 THEN $3 ELSE from_entity_id END,
			       CASE WHEN to_entity_id = $2 THEN $3 ELSE to_entity_id END,
			       kind, origin, org_id, created_at
			FROM entity_links WHERE org_id = $1 AND (from_entity_id = $2 OR to_entity_id = $2)
			ON CONFLICT DO NOTHING`,
			orgID, loser.ID, survivor.ID); err != nil {
			return fmt.Errorf("move entity links: %w", err)
		}
		if _, err := q.ExecContext(ctx,
			`DELETE FROM entity_links WHERE org_id = $1 AND (from_entity_id = $2 OR to_entity_id = $2)`, orgID, loser.ID); err != nil {
			return err
		}

		// The loser's own columns are read before it goes, and its row goes
		// before the survivor takes its key and id: both are unique among the
		// rows that would hold them.
		var loserSnapshot, loserTeam, loserCommissioner sql.NullString
		if err := q.QueryRowContext(ctx, `
			SELECT snapshot_json::text, owning_team_id::text, commissioned_by_user_id::text
			FROM entities WHERE org_id = $1 AND id = $2`, orgID, loser.ID,
		).Scan(&loserSnapshot, &loserTeam, &loserCommissioner); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM entities WHERE org_id = $1 AND id = $2`, orgID, loser.ID); err != nil {
			return fmt.Errorf("delete merged entity: %w", err)
		}
		// poll_seq is bumped whichever row's live state the survivor ends up
		// with: the merge is a new version of the row, so a poll cycle that
		// read the survivor before it misses its snapshot CAS, as it would
		// after a rename.
		if _, err := q.ExecContext(ctx, `
			UPDATE entities SET
			  external_id             = COALESCE(external_id, $1),
			  owning_team_id          = COALESCE(owning_team_id, $2::uuid),
			  commissioned_by_user_id = COALESCE(commissioned_by_user_id, $3::uuid),
			  poll_seq                = poll_seq + 1
			WHERE org_id = $4 AND id = $5`,
			nullString(loser.ExternalID), loserTeam, loserCommissioner, orgID, survivor.ID); err != nil {
			return fmt.Errorf("fold identity into survivor: %w", err)
		}
		if loser.State == "active" {
			if _, err := q.ExecContext(ctx, `
				UPDATE entities SET
				  state = 'active', closed_at = NULL,
				  source_id = $1, url = $2, snapshot_json = $3::jsonb, title = $4, description = $5,
				  last_polled_at = $6, poll_seq = GREATEST(poll_seq, $7) + 1
				WHERE org_id = $8 AND id = $9`,
				loser.SourceID, loser.URL, loserSnapshot, loser.Title, loser.Description,
				loser.LastPolledAt, loser.PollSeq, orgID, survivor.ID); err != nil {
				return fmt.Errorf("survivor takes live state: %w", err)
			}
		}
		kept, dropped := db.DuplicateEntityMergedKeys(survivor, loser)
		if err := rewriteEntityArtifacts(ctx, q, orgID, survivor.Source, survivor.Scope,
			domain.EntityArtifactResource(survivor.Source, survivor.Scope, kept.ExternalID), dropped.SourceID, kept.SourceID); err != nil {
			return err
		}
		return rewriteEntityActionURLs(ctx, q, orgID, survivor.Source, survivor.Scope, dropped.URL, kept.URL)
	})
	if err != nil {
		return "", err
	}
	return survivorID, nil
}

func (s *entityStore) UpdateTitle(ctx context.Context, orgID, id, title string) (domain.Entity, error) {
	return updateEntityTitle(ctx, s.q, orgID, id, title)
}

func (s *entityStore) UpdateTitleSystem(ctx context.Context, orgID, id, title string) (domain.Entity, error) {
	return updateEntityTitle(ctx, s.admin, orgID, id, title)
}

func updateEntityTitle(ctx context.Context, q queryer, orgID, id, title string) (domain.Entity, error) {
	return scanWrittenEntity(q.QueryRowContext(ctx,
		`UPDATE entities SET title = $1 WHERE org_id = $2 AND id = $3 RETURNING `+pgEntitySelectCols,
		title, orgID, id))
}

func (s *entityStore) UpdateDescription(ctx context.Context, orgID, id, description string) (domain.Entity, error) {
	return updateEntityDescription(ctx, s.q, orgID, id, description)
}

func (s *entityStore) UpdateDescriptionSystem(ctx context.Context, orgID, id, description string) (domain.Entity, error) {
	return updateEntityDescription(ctx, s.admin, orgID, id, description)
}

func updateEntityDescription(ctx context.Context, q queryer, orgID, id, description string) (domain.Entity, error) {
	return scanWrittenEntity(q.QueryRowContext(ctx,
		`UPDATE entities SET description = $1 WHERE org_id = $2 AND id = $3 RETURNING `+pgEntitySelectCols,
		description, orgID, id))
}

// UpdateURLSystem sets the entity's url through the admin pool. No
// non-System counterpart exists (see the interface doc) — the only caller
// (ee/slack's permalink resolver) runs detached with no JWT claims, so
// there is no app-pool-equivalent request context to route through.
func (s *entityStore) UpdateURLSystem(ctx context.Context, orgID, id, url string) (domain.Entity, error) {
	return scanWrittenEntity(s.admin.QueryRowContext(ctx,
		`UPDATE entities SET url = $1 WHERE org_id = $2 AND id = $3 RETURNING `+pgEntitySelectCols,
		url, orgID, id))
}

func (s *entityStore) MarkClosed(ctx context.Context, orgID, id string) (domain.Entity, error) {
	return markEntityClosed(ctx, s.q, orgID, id)
}

func markEntityClosed(ctx context.Context, q queryer, orgID, id string) (domain.Entity, error) {
	return scanWrittenEntity(q.QueryRowContext(ctx, `
		UPDATE entities SET state = 'closed', closed_at = $1 WHERE org_id = $2 AND id = $3
		RETURNING `+pgEntitySelectCols,
		time.Now().UTC(), orgID, id))
}

func (s *entityStore) Close(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	return closeActiveEntity(ctx, s.q, orgID, id)
}

func (s *entityStore) CloseSystem(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	return closeActiveEntity(ctx, s.admin, orgID, id)
}

// closeActiveEntity returns nil when the state='active' guard declined — the
// entity was already closed, or no row carries the id. Both are the idempotent
// no-op the method exists for, and neither is an error.
func closeActiveEntity(ctx context.Context, q queryer, orgID, id string) (*domain.Entity, error) {
	return scanEntityRow(q.QueryRowContext(ctx, `
		UPDATE entities SET state = 'closed', closed_at = $1 WHERE org_id = $2 AND id = $3 AND state = 'active'
		RETURNING `+pgEntitySelectCols,
		time.Now().UTC(), orgID, id))
}

// ReactivateWithSnapshotCASSystem is the discovery reopen: state, closed_at
// and the fresh snapshot in one statement under the poll_seq CAS. See the
// interface doc for why the two halves must not be separate writes.
func (s *entityStore) ReactivateWithSnapshotCASSystem(ctx context.Context, orgID, id, snapshotJSON string, expectedPollSeq int64) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `
		UPDATE entities
		SET state = 'active', closed_at = NULL, snapshot_json = $1::jsonb, last_polled_at = $2, poll_seq = poll_seq + 1
		WHERE org_id = $3 AND id = $4 AND state = 'closed' AND poll_seq = $5
	`, snapshotJSON, time.Now().UTC(), orgID, id, expectedPollSeq)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CloseWithSnapshotCASSystem is the poll's own close — a terminal snapshot
// and the closed state in one statement under the poll_seq CAS. No state
// guard: a seed may land on a row another writer already closed, and the
// CAS decides whose snapshot is current.
func (s *entityStore) CloseWithSnapshotCASSystem(ctx context.Context, orgID, id, snapshotJSON string, expectedPollSeq int64) (bool, error) {
	now := time.Now().UTC()
	res, err := s.admin.ExecContext(ctx, `
		UPDATE entities
		SET state = 'closed', closed_at = COALESCE(closed_at, $1), snapshot_json = $2::jsonb, last_polled_at = $1, poll_seq = poll_seq + 1
		WHERE org_id = $3 AND id = $4 AND poll_seq = $5
	`, now, snapshotJSON, orgID, id, expectedPollSeq)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CloseTerminalSystem is the terminating close — see the interface doc. The
// entity row is locked FOR UPDATE before anything else, which is the lock
// every task mint takes first too (lockActiveEntity), so the two serialize
// on it rather than on each other's rows.
func (s *entityStore) CloseTerminalSystem(ctx context.Context, orgID, entityID string, expected *int64, closeTypes []string, closeReason, closeEventType, closingEventID string) (db.TerminalCloseResult, error) {
	res := db.TerminalCloseResult{ActiveConversationIDs: map[string][]string{}}
	err := inTx(ctx, s.admin, func(q queryer) error {
		var state string
		var pollSeq int64
		err := q.QueryRowContext(ctx, `
			SELECT state, poll_seq FROM entities WHERE org_id = $1 AND id = $2 FOR UPDATE
		`, orgID, entityID).Scan(&state, &pollSeq)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock entity: %w", err)
		}
		if state != "active" || (expected != nil && pollSeq != *expected) {
			return nil
		}

		taskIDs, err := openTaskIDsByEntityAndTypes(ctx, q, orgID, entityID, closeTypes)
		if err != nil {
			return fmt.Errorf("list open tasks: %w", err)
		}
		for _, taskID := range taskIDs {
			closed, conversationIDs, err := closeTaskWithCancelIntent(ctx, q, orgID, taskID, closeReason, closeEventType, closingEventID)
			if err != nil {
				return fmt.Errorf("task %s: %w", taskID, err)
			}
			if !closed {
				continue
			}
			res.ClosedTaskIDs = append(res.ClosedTaskIDs, taskID)
			res.ActiveConversationIDs[taskID] = conversationIDs
		}

		// The guard on state runs a second time even though this transaction
		// holds the row lock: a declined flip here means the row changed
		// under the lock, which is a failure, not a no-op.
		flipped, err := closeActiveEntity(ctx, q, orgID, entityID)
		if err != nil {
			return fmt.Errorf("close entity: %w", err)
		}
		if flipped == nil {
			return errors.New("close entity: row changed under the lock")
		}
		res.Closed = true
		return nil
	})
	if err != nil {
		return db.TerminalCloseResult{}, err
	}
	if !res.Closed {
		return db.TerminalCloseResult{}, nil
	}
	return res, nil
}

// openTaskIDsByEntityAndTypes lists the entity's non-terminal tasks of the
// given types, oldest first, for the terminating close to walk. An empty
// type set matches nothing.
func openTaskIDsByEntityAndTypes(ctx context.Context, q queryer, orgID, entityID string, eventTypes []string) ([]string, error) {
	if len(eventTypes) == 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM tasks
		WHERE org_id = $1 AND entity_id = $2 AND event_type = ANY($3)
		  AND status NOT IN ('done', 'dismissed')
		ORDER BY created_at ASC, id ASC
	`, orgID, entityID, eventTypes)
	if err != nil {
		return nil, err
	}
	return scanIDs(rows, "tasks.id")
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
// per messages.id) for binding through a single $N parameter, the same
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
	err := row.Scan(&e.ID, &e.Source, &e.Scope, &e.SourceID, &e.ExternalID, &e.Kind, &e.Title, &e.URL,
		&e.SnapshotJSON, &e.Description, &e.State,
		&e.CreatedAt, &e.LastPolledAt, &e.ClosedAt, &e.PollSeq)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func scanEntityList(rows *sql.Rows) ([]domain.Entity, error) {
	out := []domain.Entity{}
	for rows.Next() {
		var e domain.Entity
		if err := rows.Scan(&e.ID, &e.Source, &e.Scope, &e.SourceID, &e.ExternalID, &e.Kind, &e.Title, &e.URL,
			&e.SnapshotJSON, &e.Description, &e.State,
			&e.CreatedAt, &e.LastPolledAt, &e.ClosedAt, &e.PollSeq); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *entityStore) ClearSnapshotsForSourceSystem(ctx context.Context, orgID, source string) (int, error) {
	res, err := s.admin.ExecContext(ctx, `
		UPDATE entities
		SET snapshot_json = NULL, poll_seq = poll_seq + 1
		WHERE org_id = $1 AND source = $2 AND state = 'active'
	`, orgID, source)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
