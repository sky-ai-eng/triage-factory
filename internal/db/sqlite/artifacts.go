package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// artifactStore is the SQLite impl of db.ArtifactStore: the Postgres
// impl's reads over external_actions, grouped by sqliteActionObjectIDSQL.
// Local mode is N=1 with no RLS, so the ...System methods share the one
// connection.
type artifactStore struct{ q queryer }

func newArtifactStore(q queryer) db.ArtifactStore {
	return &artifactStore{q: q}
}

var _ db.ArtifactStore = (*artifactStore)(nil)

// sqliteArtifactRowFilter is what every artifact read adds to its own WHERE:
// the rows of an artifact performer that name an object, of an action that
// can name one.
var sqliteArtifactRowFilter = ` AND ` + db.ArtifactRowSQL() + ` AND (` + sqliteActionObjectIDSQL + `) IS NOT NULL`

// sqliteArtifactCTEs is the Postgres impl's pgArtifactCTEs (see there for
// the model). occurred_at is text here, ordered as text the way the ledger's
// list reads order it, and whole seconds, so rows of one second break on
// rowid, the order they were written in.
func sqliteArtifactCTEs(where string) string {
	return `
	WITH scoped AS (
		SELECT ea.id, ea.rowid AS seq, ea.org_id, ea.provider, ea.scope, ea.action, ea.target,
		       COALESCE(ea.current_url, ea.url, '') AS url, ea.entity_id, ea.repository_id,
		       ea.object_kind, ea.object_key, ea.occurred_at, ea.conversation_id, ea.team_id,
		       ` + sqliteActionObjectIDSQL + ` AS object_id
		  FROM external_actions ea
		 WHERE ` + where + sqliteArtifactRowFilter + `
	), grouped AS (
		SELECT s.*,
		       ROW_NUMBER() OVER (PARTITION BY s.object_id ORDER BY s.occurred_at DESC, s.seq DESC) AS rn,
		       COUNT(*) OVER (PARTITION BY s.object_id) AS action_count,
		       MIN(s.occurred_at) OVER (PARTITION BY s.object_id) AS first_at,
		       FIRST_VALUE(s.url) OVER (PARTITION BY s.object_id
		           ORDER BY s.url = '', s.occurred_at DESC, s.seq DESC) AS display_url,
		       FIRST_VALUE(s.entity_id) OVER (PARTITION BY s.object_id
		           ORDER BY s.entity_id IS NULL, s.occurred_at DESC, s.seq DESC) AS display_entity_id
		  FROM scoped s
	)`
}

// sqliteArtifactColumns is the Postgres impl's pgArtifactColumns. first_at
// comes out of a window aggregate with no declared type, so both timestamps
// are scanned as text and parsed.
const sqliteArtifactColumns = `g.object_id, g.object_kind, g.provider, g.scope,
	       COALESCE(g.display_entity_id, ''), COALESCE(g.repository_id, ''), COALESCE(g.object_key, ''),
	       COALESCE(e.source_id, g.target),
	       CASE WHEN g.object_kind IN ('pull_request', 'issue') THEN COALESCE(NULLIF(e.url, ''), g.display_url)
	            ELSE g.display_url END,
	       g.action, g.action_count, g.first_at, g.occurred_at`

// sqliteArtifactEntityJoin is the Postgres impl's pgArtifactEntityJoin.
const sqliteArtifactEntityJoin = `
	  LEFT JOIN entities e ON e.id = g.display_entity_id AND e.org_id = g.org_id
	       AND g.object_kind IN ('pull_request', 'issue', 'comment', 'review', 'message')`

// sqliteArtifactSelect is the Postgres impl's pgArtifactSelect.
func sqliteArtifactSelect(where string) string {
	return sqliteArtifactCTEs(where) + `
	SELECT ` + sqliteArtifactColumns + `
	  FROM grouped g` + sqliteArtifactEntityJoin + `
	 WHERE g.rn = 1`
}

// sqliteObjectActivitySelect is the Postgres impl's pgObjectActivitySelect,
// total included. group_concat's default separator is a comma.
func sqliteObjectActivitySelect(where string) string {
	return sqliteArtifactCTEs(where) + `, spread AS (
		SELECT s.object_id, COUNT(DISTINCT s.conversation_id) AS conversation_count,
		       COALESCE(group_concat(DISTINCT s.team_id), '') AS team_ids
		  FROM scoped s
		 GROUP BY s.object_id
	)
	SELECT ` + sqliteArtifactColumns + `, sp.conversation_count, sp.team_ids, COUNT(*) OVER () AS total
	  FROM grouped g
	  JOIN spread sp ON sp.object_id = g.object_id` + sqliteArtifactEntityJoin + `
	 WHERE g.rn = 1`
}

func (s *artifactStore) ListByConversation(ctx context.Context, orgID, conversationID string, opts db.ListOpts) ([]domain.Artifact, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	if err := db.RequireArtifactOffsetPage(opts); err != nil {
		return nil, 0, err
	}
	const where = `ea.org_id = ? AND ea.conversation_id = ?`
	args := []any{orgID, conversationID}
	// An unwindowed read holds every group, so its length is the total and no
	// count runs.
	windowed := opts.Limit > 0 || opts.CountOnly
	var total int
	if windowed {
		if err := s.q.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT `+sqliteActionObjectIDSQL+`)
			  FROM external_actions ea
			 WHERE `+where+sqliteArtifactRowFilter, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
		if opts.CountOnly {
			return []domain.Artifact{}, total, nil
		}
	}
	query := sqliteArtifactSelect(where) + ` ORDER BY g.occurred_at DESC, g.object_id`
	if opts.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanSQLiteArtifacts(rows)
	if err != nil {
		return nil, 0, err
	}
	if !windowed {
		total = len(out)
	}
	return out, total, nil
}

// ListByConversationSystem is ListByConversation: N=1, no pool split.
func (s *artifactStore) ListByConversationSystem(ctx context.Context, orgID, conversationID string, opts db.ListOpts) ([]domain.Artifact, int, error) {
	return s.ListByConversation(ctx, orgID, conversationID, opts)
}

// CountByConversations binds the conversation ids as one JSON array read
// through json_each, so the set needs no chunking against SQLite's variable
// limit.
func (s *artifactStore) CountByConversations(ctx context.Context, orgID string, conversationIDs []string) (map[string]map[string]int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	counts := make(map[string]map[string]int, len(conversationIDs))
	if len(conversationIDs) == 0 {
		return counts, nil
	}
	ids, err := json.Marshal(conversationIDs)
	if err != nil {
		return nil, fmt.Errorf("encode conversation ids: %w", err)
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT ea.conversation_id, ea.object_kind, COUNT(DISTINCT `+sqliteActionObjectIDSQL+`)
		  FROM external_actions ea
		 WHERE ea.org_id = ? AND ea.conversation_id IN (SELECT value FROM json_each(?))`+sqliteArtifactRowFilter+`
		 GROUP BY ea.conversation_id, ea.object_kind
	`, orgID, string(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var conversationID, kind string
		var n int
		if err := rows.Scan(&conversationID, &kind, &n); err != nil {
			return nil, err
		}
		if counts[conversationID] == nil {
			counts[conversationID] = map[string]int{}
		}
		counts[conversationID][kind] = n
	}
	return counts, rows.Err()
}

func (s *artifactStore) ExistsForConversationSystem(ctx context.Context, orgID, conversationID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var exists bool
	err := s.q.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM external_actions ea
			 WHERE ea.org_id = ? AND ea.conversation_id = ?`+sqliteArtifactRowFilter+`
		)`, orgID, conversationID).Scan(&exists)
	return exists, err
}

func (s *artifactStore) ListForConversationsSystem(ctx context.Context, orgID string, conversationIDs []string) ([]domain.Artifact, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if len(conversationIDs) == 0 {
		return nil, nil
	}
	ids, err := json.Marshal(conversationIDs)
	if err != nil {
		return nil, fmt.Errorf("encode conversation ids: %w", err)
	}
	rows, err := s.q.QueryContext(ctx,
		sqliteArtifactSelect(`ea.org_id = ? AND ea.conversation_id IN (SELECT value FROM json_each(?))`)+
			` ORDER BY g.first_at, g.object_id`,
		orgID, string(ids))
	if err != nil {
		return nil, err
	}
	return scanSQLiteArtifacts(rows)
}

func (s *artifactStore) ListByTeam(ctx context.Context, orgID, teamID string, opts db.ArtifactListOpts) ([]domain.ObjectActivity, int, error) {
	return s.listObjectActivity(ctx, orgID, `ea.org_id = ? AND ea.team_id = ?`, []any{orgID, teamID}, opts)
}

// ListByOrgSystem is ListByTeam over every team: N=1, no pool split.
func (s *artifactStore) ListByOrgSystem(ctx context.Context, orgID string, opts db.ArtifactListOpts) ([]domain.ObjectActivity, int, error) {
	return s.listObjectActivity(ctx, orgID, `ea.org_id = ?`, []any{orgID}, opts)
}

// listObjectActivity is both scopes' read: base and args select the scope's
// rows, opts' filters narrow them, and the page is of objects.
func (s *artifactStore) listObjectActivity(ctx context.Context, orgID, base string, args []any, opts db.ArtifactListOpts) ([]domain.ObjectActivity, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	if err := db.RequireArtifactOffsetPage(opts.ListOpts); err != nil {
		return nil, 0, err
	}
	where, args := sqliteArtifactListWhere(base, args, opts)
	if opts.CountOnly {
		total, err := s.countObjectActivity(ctx, where, args)
		return []domain.ObjectActivity{}, total, err
	}
	query := sqliteObjectActivitySelect(where) + ` ORDER BY g.occurred_at DESC, g.object_id`
	pageArgs := args
	if opts.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		pageArgs = append(slices.Clip(args), opts.Limit, opts.Offset)
	}
	out, total, err := s.scanObjectActivity(ctx, query, pageArgs)
	if err != nil {
		return nil, 0, err
	}
	// A page past the last object has no row to carry the total. The rows are
	// closed by now, which the one-connection pool needs before another read.
	if len(out) == 0 && opts.Offset > 0 {
		if total, err = s.countObjectActivity(ctx, where, args); err != nil {
			return nil, 0, err
		}
	}
	return out, total, nil
}

// scanObjectActivity runs a sqliteObjectActivitySelect page and reads its
// items and the total the rows carry.
func (s *artifactStore) scanObjectActivity(ctx context.Context, query string, args []any) ([]domain.ObjectActivity, int, error) {
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.ObjectActivity{}
	var total int
	for rows.Next() {
		var (
			a                      domain.ObjectActivity
			first, latest, teamIDs string
		)
		if err := rows.Scan(
			&a.ObjectID, &a.ObjectKind, &a.Provider, &a.Scope,
			&a.EntityID, &a.RepositoryID, &a.ObjectKey,
			&a.Target, &a.URL,
			&a.LastAction, &a.ActionCount, &first, &latest,
			&a.ConversationCount, &teamIDs, &total,
		); err != nil {
			return nil, 0, err
		}
		if err := parseArtifactTimes(&a.Artifact, first, latest); err != nil {
			return nil, 0, err
		}
		a.TeamIDs = db.SplitArtifactTeamIDs(teamIDs)
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// countObjectActivity counts the objects the rows where selects name.
func (s *artifactStore) countObjectActivity(ctx context.Context, where string, args []any) (int, error) {
	var total int
	err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT `+sqliteActionObjectIDSQL+`)
		  FROM external_actions ea
		 WHERE `+where+sqliteArtifactRowFilter, args...).Scan(&total)
	return total, err
}

// sqliteArtifactListWhere appends opts' filters to base. The time bounds
// wrap both sides in datetime(), as the ledger's list reads do, so a bound
// time.Time compares correctly against the CURRENT_TIMESTAMP-formatted
// occurred_at whatever the driver's bind serialization.
func sqliteArtifactListWhere(base string, args []any, opts db.ArtifactListOpts) (string, []any) {
	where := base
	if opts.Provider != "" {
		where += ` AND ea.provider = ?`
		args = append(args, opts.Provider)
	}
	if opts.ObjectKind != "" {
		where += ` AND ea.object_kind = ?`
		args = append(args, opts.ObjectKind)
	}
	if !opts.Since.IsZero() {
		where += ` AND datetime(ea.occurred_at) >= datetime(?)`
		args = append(args, opts.Since)
	}
	if !opts.Until.IsZero() {
		where += ` AND datetime(ea.occurred_at) < datetime(?)`
		args = append(args, opts.Until)
	}
	return where, args
}

func scanSQLiteArtifacts(rows *sql.Rows) ([]domain.Artifact, error) {
	defer rows.Close()
	out := []domain.Artifact{}
	for rows.Next() {
		var (
			a             domain.Artifact
			first, latest string
		)
		if err := rows.Scan(
			&a.ObjectID, &a.ObjectKind, &a.Provider, &a.Scope,
			&a.EntityID, &a.RepositoryID, &a.ObjectKey,
			&a.Target, &a.URL,
			&a.LastAction, &a.ActionCount, &first, &latest,
		); err != nil {
			return nil, err
		}
		if err := parseArtifactTimes(&a, first, latest); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// parseArtifactTimes sets a's FirstAt and LastAt from their text columns.
func parseArtifactTimes(a *domain.Artifact, first, latest string) error {
	var err error
	if a.FirstAt, err = parseDBDatetime(first); err != nil {
		return fmt.Errorf("parse artifact first_at %q: %w", first, err)
	}
	if a.LastAt, err = parseDBDatetime(latest); err != nil {
		return fmt.Errorf("parse artifact last_at %q: %w", latest, err)
	}
	return nil
}
