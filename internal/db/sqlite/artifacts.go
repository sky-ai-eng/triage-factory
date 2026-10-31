package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

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
// the agent's rows that name an object, of an action that can name one.
var sqliteArtifactRowFilter = ` AND ea.performed_by = 'agent' AND ea.action NOT IN (` + db.ArtifactExcludedActionsSQL +
	`) AND (` + sqliteActionObjectIDSQL + `) IS NOT NULL`

// sqliteArtifactSelect is the Postgres impl's grouped read (see
// pgArtifactSelect for the model). occurred_at is text here, ordered as text
// the way the ledger's list reads order it; first_at comes out of a window
// aggregate with no declared type, so both timestamps are scanned as text and
// parsed.
func sqliteArtifactSelect(where string) string {
	return `
	WITH scoped AS (
		SELECT ea.id, ea.org_id, ea.provider, ea.scope, ea.action, ea.target,
		       COALESCE(ea.current_url, ea.url, '') AS url, ea.entity_id, ea.repository_id,
		       ea.object_kind, ea.object_key, ea.occurred_at,
		       ` + sqliteActionObjectIDSQL + ` AS object_id
		  FROM external_actions ea
		 WHERE ` + where + sqliteArtifactRowFilter + `
	), grouped AS (
		SELECT s.*,
		       ROW_NUMBER() OVER (PARTITION BY s.object_id ORDER BY s.occurred_at DESC, s.id DESC) AS rn,
		       COUNT(*) OVER (PARTITION BY s.object_id) AS action_count,
		       MIN(s.occurred_at) OVER (PARTITION BY s.object_id) AS first_at
		  FROM scoped s
	)
	SELECT g.object_id, g.object_kind, g.provider, g.scope,
	       COALESCE(g.entity_id, ''), COALESCE(g.repository_id, ''), COALESCE(g.object_key, ''),
	       COALESCE(e.source_id, g.target), COALESCE(NULLIF(e.url, ''), g.url),
	       g.action, g.action_count, g.first_at, g.occurred_at
	  FROM grouped g
	  LEFT JOIN entities e ON e.id = g.entity_id AND e.org_id = g.org_id
	       AND (g.object_kind = 'pull_request' OR (g.object_kind = 'issue' AND g.provider = 'github'))
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
		var err error
		if a.FirstAt, err = parseDBDatetime(first); err != nil {
			return nil, fmt.Errorf("parse artifact first_at %q: %w", first, err)
		}
		if a.LastAt, err = parseDBDatetime(latest); err != nil {
			return nil, fmt.Errorf("parse artifact last_at %q: %w", latest, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
