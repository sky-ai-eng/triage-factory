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
const pgEntitySelectCols = `id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
       COALESCE(snapshot_json::text, ''), COALESCE(description, ''), state,
       created_at, last_polled_at, closed_at, poll_seq`

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

func (s *entityStore) ListActive(ctx context.Context, orgID, source string) ([]domain.Entity, error) {
	return listActiveEntities(ctx, s.q, orgID, source)
}

func (s *entityStore) ListActiveSystem(ctx context.Context, orgID, source string) ([]domain.Entity, error) {
	return listActiveEntities(ctx, s.admin, orgID, source)
}

// ListActiveTerminalCandidatesSystem selects active entities whose stored
// snapshot already reads terminal. Admin pool: the reconciliation sweep is a
// background job with no JWT claims. snapshot_json is jsonb, so `->>` yields
// NULL for a missing key or an empty object — COALESCE makes those a plain
// non-match rather than a NULL-propagating predicate, and the IS NOT NULL
// guard skips rows that never stored a snapshot at all.
func (s *entityStore) ListActiveTerminalCandidatesSystem(ctx context.Context, orgID string, jiraDone []domain.JiraStatusRef, limit int) ([]domain.Entity, error) {
	args := []any{orgID}
	// Each arm is omitted rather than emitted empty: `IN ()` is a syntax
	// error, and a ref set with no ids (or no names) genuinely has nothing to
	// match on that side. With neither, no Jira entity can be terminal. Both
	// halves are user-configured text, so they bind as placeholders rather
	// than riding an array literal.
	arm := func(key string, values []string) string {
		placeholders := make([]string, len(values))
		for i, v := range values {
			args = append(args, v)
			placeholders[i] = "$" + strconv.Itoa(len(args))
		}
		return `snapshot_json->>'` + key + `' IN (` + strings.Join(placeholders, ", ") + `)`
	}
	var matches []string
	if ids := domain.JiraStatusIDs(jiraDone); len(ids) > 0 {
		matches = append(matches, arm("status_id", ids))
	}
	if names := domain.JiraStatusNames(jiraDone); len(names) > 0 {
		matches = append(matches, arm("status", names))
	}
	jiraArm := ""
	if len(matches) > 0 {
		jiraArm = ` OR (source = 'jira' AND (` + strings.Join(matches, " OR ") + `))`
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
		  AND (
		    (source = 'github' AND (
		       snapshot_json->>'merged' = 'true'
		       OR upper(COALESCE(snapshot_json->>'state', '')) IN ('CLOSED', 'MERGED')
		    ))`+jiraArm+`
		  )
		ORDER BY created_at ASC`+limitClause, args...)
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
	now := time.Now().UTC()
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

func (s *entityStore) UpdateSnapshot(ctx context.Context, orgID, id, snapshotJSON string) (domain.Entity, error) {
	// The blind-write system variant was removed with TFAC-579: every
	// tracker snapshot write goes through UpdateSnapshotCASSystem. This
	// app-pool variant survives for non-tracker writers with no poll_seq
	// read-then-write cycle to protect.
	return scanWrittenEntity(s.q.QueryRowContext(ctx, `
		UPDATE entities
		SET snapshot_json = $1::jsonb, last_polled_at = $2
		WHERE org_id = $3 AND id = $4
		RETURNING `+pgEntitySelectCols,
		snapshotJSON, time.Now().UTC(), orgID, id))
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

func (s *entityStore) RekeyOrMergeSystem(ctx context.Context, orgID, id, newSourceID string) (string, bool, error) {
	var survivor string
	merged := false
	err := inTx(ctx, s.admin, func(q queryer) error {
		if err := q.QueryRowContext(ctx, `SELECT id FROM entities WHERE org_id=$1 AND source=(SELECT source FROM entities WHERE org_id=$1 AND id=$2) AND source_id=$3`, orgID, id, newSourceID).Scan(&survivor); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			res, err := q.ExecContext(ctx, `UPDATE entities SET source_id=$1, last_polled_at=$2 WHERE org_id=$3 AND id=$4`, newSourceID, time.Now().UTC(), orgID, id)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return sql.ErrNoRows
			}
			survivor = id
			return nil
		}
		if survivor == id {
			return nil
		}
		merged = true
		if _, err := q.ExecContext(ctx, `UPDATE tasks SET status='dismissed', closed_at=$1, close_reason='duplicate_entity_merged' WHERE org_id=$2 AND entity_id=$3 AND status NOT IN ('done','dismissed') AND EXISTS (SELECT 1 FROM tasks s WHERE s.org_id=$2 AND s.entity_id=$4 AND s.event_type=tasks.event_type AND s.dedup_key=tasks.dedup_key AND s.status NOT IN ('done','dismissed'))`, time.Now().UTC(), orgID, id, survivor); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `UPDATE blueprint_runs SET cancel_requested=true WHERE org_id=$1 AND status='running' AND cancel_requested=false AND id IN (SELECT c.blueprint_run_id FROM conversations c JOIN tasks t ON t.id=c.task_id WHERE t.org_id=$1 AND t.entity_id=$2 AND t.close_reason='duplicate_entity_merged' AND c.blueprint_run_id IS NOT NULL AND (c.status IS NULL OR c.status NOT IN ('completed','failed')))`, orgID, id); err != nil {
			return err
		}
		for _, table := range []string{"tasks", "events", "event_queue", "pending_firings", "conversation_memory"} {
			if _, err := q.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET entity_id=$1 WHERE org_id=$2 AND entity_id=$3`, table), survivor, orgID, id); err != nil {
				return err
			}
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO conversation_memory_entities(org_id,conversation_id,entity_id,role,created_at) SELECT org_id,conversation_id,$1,role,created_at FROM conversation_memory_entities WHERE entity_id=$2 ON CONFLICT DO NOTHING`, survivor, id); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM conversation_memory_entities WHERE entity_id=$1`, id); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM entity_links WHERE (from_entity_id=$1 OR to_entity_id=$1) AND (CASE WHEN from_entity_id=$1 THEN $2 ELSE from_entity_id END)=(CASE WHEN to_entity_id=$1 THEN $2 ELSE to_entity_id END)`, id, survivor); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO entity_links(from_entity_id,to_entity_id,kind,origin,org_id,created_at) SELECT CASE WHEN from_entity_id=$1 THEN $2 ELSE from_entity_id END,CASE WHEN to_entity_id=$1 THEN $2 ELSE to_entity_id END,kind,origin,org_id,created_at FROM entity_links WHERE from_entity_id=$1 OR to_entity_id=$1 ON CONFLICT DO NOTHING`, id, survivor); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM entity_links WHERE from_entity_id=$1 OR to_entity_id=$1`, id); err != nil {
			return err
		}
		_, err := q.ExecContext(ctx, `DELETE FROM entities WHERE org_id=$1 AND id=$2`, orgID, id)
		return err
	})
	return survivor, merged, err
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

func (s *entityStore) MarkClosedSystem(ctx context.Context, orgID, id string) (domain.Entity, error) {
	return markEntityClosed(ctx, s.admin, orgID, id)
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
	err := row.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
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
		if err := rows.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
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
