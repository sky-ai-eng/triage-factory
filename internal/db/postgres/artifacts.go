package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// artifactStore is the Postgres impl of db.ArtifactStore: reads over
// external_actions grouped by pgActionObjectIDSQL. q is the app pool (or the
// claims tx), where external_actions_all scopes by org; admin is the admin
// pool for the ...System reads. org_id stays bound in every statement.
type artifactStore struct {
	q     queryer
	admin queryer
}

func newArtifactStore(q, admin queryer) db.ArtifactStore {
	return &artifactStore{q: q, admin: admin}
}

var _ db.ArtifactStore = (*artifactStore)(nil)

// pgArtifactRowFilter is what every artifact read adds to its own WHERE: the
// agent's rows that name an object, of an action that can name one.
var pgArtifactRowFilter = ` AND ea.performed_by = 'agent' AND ea.action NOT IN (` + db.ArtifactExcludedActionsSQL() +
	`) AND (` + pgActionObjectIDSQL + `) IS NOT NULL`

// pgArtifactSelect is the grouped read over the rows where selects, ending
// in a WHERE the caller may extend with ORDER BY and a window. Each group's
// latest row (greatest occurred_at, id) supplies the action, target and url;
// the window aggregates supply the count and the earliest occurred_at. The
// entity is joined only for a kind identified by entity_id (the kinds and
// the split on provider are pgActionObjectIDSQL's entity arms), and its key
// and url win over the row's; the row's stand in when the entity is not
// readable or has no url yet.
func pgArtifactSelect(where string) string {
	return `
	WITH scoped AS (
		SELECT ea.id, ea.org_id, ea.provider, ea.scope, ea.action, ea.target,
		       COALESCE(ea.current_url, ea.url, '') AS url, ea.entity_id, ea.repository_id,
		       ea.object_kind, ea.object_key, ea.occurred_at,
		       ` + pgActionObjectIDSQL + ` AS object_id
		  FROM external_actions ea
		 WHERE ` + where + pgArtifactRowFilter + `
	), grouped AS (
		SELECT s.*,
		       ROW_NUMBER() OVER (PARTITION BY s.object_id ORDER BY s.occurred_at DESC, s.id DESC) AS rn,
		       COUNT(*) OVER (PARTITION BY s.object_id) AS action_count,
		       MIN(s.occurred_at) OVER (PARTITION BY s.object_id) AS first_at
		  FROM scoped s
	)
	SELECT g.object_id, g.object_kind, g.provider, g.scope,
	       COALESCE(g.entity_id::text, ''), COALESCE(g.repository_id::text, ''), COALESCE(g.object_key, ''),
	       COALESCE(e.source_id, g.target), COALESCE(NULLIF(e.url, ''), g.url),
	       g.action, g.action_count, g.first_at, g.occurred_at
	  FROM grouped g
	  LEFT JOIN entities e ON e.id = g.entity_id AND e.org_id = g.org_id
	       AND (g.object_kind = 'pull_request' OR (g.object_kind = 'issue' AND g.provider = 'github'))
	 WHERE g.rn = 1`
}

// pgArtifactObjectIDOrder orders object ids bytewise, as SQLite's BINARY
// collation does, so the tiebreaker is the same in both dialects.
const pgArtifactObjectIDOrder = `g.object_id COLLATE "C"`

func (s *artifactStore) ListByConversation(ctx context.Context, orgID, conversationID string, opts db.ListOpts) ([]domain.Artifact, int, error) {
	return listPgArtifactsByConversation(ctx, s.q, orgID, conversationID, opts)
}

func (s *artifactStore) ListByConversationSystem(ctx context.Context, orgID, conversationID string, opts db.ListOpts) ([]domain.Artifact, int, error) {
	return listPgArtifactsByConversation(ctx, s.admin, orgID, conversationID, opts)
}

// listPgArtifactsByConversation is both pools' ListByConversation. The
// conversation_id bind casts to uuid so a malformed id is a query error
// rather than a text comparison against a uuid column. An unwindowed read
// holds every group, so its length is the total and no count runs.
func listPgArtifactsByConversation(ctx context.Context, q queryer, orgID, conversationID string, opts db.ListOpts) ([]domain.Artifact, int, error) {
	if err := db.RequireArtifactOffsetPage(opts); err != nil {
		return nil, 0, err
	}
	const where = `ea.org_id = $1 AND ea.conversation_id = $2::uuid`
	args := []any{orgID, conversationID}
	windowed := opts.Limit > 0 || opts.CountOnly
	var total int
	if windowed {
		if err := q.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT `+pgActionObjectIDSQL+`)
			  FROM external_actions ea
			 WHERE `+where+pgArtifactRowFilter, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
		if opts.CountOnly {
			return []domain.Artifact{}, total, nil
		}
	}
	query := pgArtifactSelect(where) + ` ORDER BY g.occurred_at DESC, ` + pgArtifactObjectIDOrder
	if opts.Limit > 0 {
		args = append(args, opts.Limit, opts.Offset)
		query += fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanPgArtifacts(rows)
	if err != nil {
		return nil, 0, err
	}
	if !windowed {
		total = len(out)
	}
	return out, total, nil
}

// CountByConversations counts each conversation's distinct object ids per
// kind. Every object id starts with its kind, so grouping by object_kind
// splits no artifact. The ids go through pgUUIDArray, as the deliverable
// store's batch reads do.
func (s *artifactStore) CountByConversations(ctx context.Context, orgID string, conversationIDs []string) (map[string]map[string]int, error) {
	counts := make(map[string]map[string]int, len(conversationIDs))
	if len(conversationIDs) == 0 {
		return counts, nil
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT ea.conversation_id::text, ea.object_kind, COUNT(DISTINCT `+pgActionObjectIDSQL+`)
		  FROM external_actions ea
		 WHERE ea.org_id = $1 AND ea.conversation_id = ANY($2)`+pgArtifactRowFilter+`
		 GROUP BY ea.conversation_id, ea.object_kind
	`, orgID, pgUUIDArray(conversationIDs))
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
	var exists bool
	err := s.admin.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM external_actions ea
			 WHERE ea.org_id = $1 AND ea.conversation_id = $2::uuid`+pgArtifactRowFilter+`
		)`, orgID, conversationID).Scan(&exists)
	return exists, err
}

func (s *artifactStore) ListForConversationsSystem(ctx context.Context, orgID string, conversationIDs []string) ([]domain.Artifact, error) {
	if len(conversationIDs) == 0 {
		return nil, nil
	}
	rows, err := s.admin.QueryContext(ctx,
		pgArtifactSelect(`ea.org_id = $1 AND ea.conversation_id = ANY($2)`)+
			` ORDER BY g.first_at, `+pgArtifactObjectIDOrder,
		orgID, pgUUIDArray(conversationIDs))
	if err != nil {
		return nil, err
	}
	return scanPgArtifacts(rows)
}

func scanPgArtifacts(rows *sql.Rows) ([]domain.Artifact, error) {
	defer rows.Close()
	out := []domain.Artifact{}
	for rows.Next() {
		var a domain.Artifact
		if err := rows.Scan(
			&a.ObjectID, &a.ObjectKind, &a.Provider, &a.Scope,
			&a.EntityID, &a.RepositoryID, &a.ObjectKey,
			&a.Target, &a.URL,
			&a.LastAction, &a.ActionCount, &a.FirstAt, &a.LastAt,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
