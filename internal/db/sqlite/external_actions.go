package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// externalActionStore is the SQLite impl of db.ExternalActionStore — the
// append-only external-action audit log (TFAC-483). SQLite is single-tenant
// (local mode, N=1) with no RLS: the org_id column exists for parity with the
// Postgres baseline but every row defaults to LocalDefaultOrgID. The Postgres
// impl splits app/admin pools (Record vs RecordSystem); here both collapse to the
// one connection, and the `...System` variant forwards to the shared body.
type externalActionStore struct{ q queryer }

func newExternalActionStore(q queryer) db.ExternalActionStore {
	return &externalActionStore{q: q}
}

var _ db.ExternalActionStore = (*externalActionStore)(nil)

// externalActionColumns is the SELECT list scanned into a domain.ExternalAction
// via scanExternalAction. Same order the Postgres impl projects. The url slot
// serves the maintained pointer — current_url when a repository rename has
// moved the object, the captured url otherwise — so every feed's link resolves
// without any reader knowing the pointer column exists.
const externalActionColumns = `
	id, org_id, COALESCE(team_id, ''), provider, scope, action, target,
	COALESCE(external_id, ''), COALESCE(current_url, url, ''), COALESCE(from_state, ''),
	COALESCE(to_state, ''), COALESCE(conversation_id, ''), COALESCE(actor_user_id, ''),
	credential, dedup_key, COALESCE(detail_json, ''), COALESCE(node_id, ''),
	COALESCE(entity_id, ''), COALESCE(repository_id, ''), performed_by, COALESCE(object_kind, ''),
	COALESCE(object_key, ''), occurred_at
`

// sqliteActionObjectIDSQL is the SQL twin of domain.ObjectID, written against
// an external_actions alias `ea` — the SQLite mirror of the Postgres twin,
// which carries the model. The ids are TEXT here, so they concatenate without
// a cast.
const sqliteActionObjectIDSQL = `
	CASE
	  WHEN ea.object_kind = 'pull_request' OR (ea.object_kind = 'issue' AND ea.provider = 'github')
	    THEN ea.object_kind || '|' || NULLIF(ea.entity_id, '')
	  WHEN ea.object_kind = 'repository'
	    THEN ea.object_kind || '|' || NULLIF(ea.repository_id, '')
	  WHEN ea.object_kind IN ('branch', 'label', 'release', 'workflow', 'workflow_run')
	    THEN ea.object_kind || '|' || NULLIF(ea.repository_id, '') || '|' || NULLIF(ea.object_key, '')
	  WHEN ea.object_kind IN ('issue', 'review', 'comment', 'message')
	    THEN ea.object_kind || '|' || NULLIF(ea.scope, '') || '|' || NULLIF(ea.object_key, '')
	END`

func (s *externalActionStore) Record(ctx context.Context, orgID string, e domain.ExternalAction) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	if err := db.RequireExternalObjectScope(e.Scope); err != nil {
		return err
	}
	if err := db.RequirePerformedBy(e.PerformedBy); err != nil {
		return err
	}
	id := e.ID
	if id == "" {
		id = uuid.New().String()
	}
	// An empty dedup_key is filled with a uuid so the append never collides (a
	// repeated action is recorded, not dropped); a caller-supplied key gets the
	// ON CONFLICT collapse (who passes one is on db.ExternalActionStore).
	// occurred_at is left to the column DEFAULT. Empty nullable fields → SQL NULL via nullIfEmpty.
	dedupKey := e.DedupKey
	if dedupKey == "" {
		dedupKey = uuid.New().String()
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO external_actions
			(id, org_id, team_id, provider, scope, action, target, external_id, url,
			 from_state, to_state, conversation_id, actor_user_id, credential, dedup_key, detail_json, node_id,
			 entity_id, repository_id, performed_by, object_kind, object_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(org_id, scope, dedup_key) DO NOTHING
	`,
		id, orgID, nullIfEmpty(e.TeamID), e.Provider, e.Scope, e.Action, e.Target,
		nullIfEmpty(e.ExternalID), nullIfEmpty(e.URL), nullIfEmpty(e.FromState),
		nullIfEmpty(e.ToState), nullIfEmpty(e.ConversationID), nullIfEmpty(e.ActorUserID),
		e.Credential, dedupKey, nullIfEmpty(e.DetailJSON), nullIfEmpty(e.NodeID),
		nullIfEmpty(e.EntityID), nullIfEmpty(e.RepositoryID), e.PerformedBy,
		nullIfEmpty(e.ObjectKind), nullIfEmpty(e.ObjectKey),
	)
	return err
}

// RecordSystem is identical to Record in SQLite: local mode is single-tenant
// (N=1) with no RLS, so there is no admin/app pool split. The method exists for
// parity with the Postgres store, where the event-triggered bot conversations +
// the Jira mirror (no JWT-claims context) need the admin pool.
func (s *externalActionStore) RecordSystem(ctx context.Context, orgID string, e domain.ExternalAction) error {
	return s.Record(ctx, orgID, e)
}

// ListByOrgSystem is identical to a plain org read in SQLite (N=1, no RLS). It
// returns the org's actions newest-first, filtered + paged by opts.
func (s *externalActionStore) ListByOrgSystem(ctx context.Context, orgID string, opts domain.ExternalActionListOpts) ([]domain.ExternalAction, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	total, err := s.countExternalActions(ctx, `org_id = ?`, []any{orgID}, opts)
	if err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.ExternalAction{}, total, nil
	}
	query := `SELECT ` + externalActionColumns + ` FROM external_actions ea WHERE org_id = ?`
	query, args := appendExternalActionFilters(query, []any{orgID}, opts)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanExternalActionRows(rows)
	return out, total, err
}

func (s *externalActionStore) ListByTeam(ctx context.Context, orgID, teamID string, opts domain.ExternalActionListOpts) ([]domain.ExternalAction, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	total, err := s.countExternalActions(ctx, `org_id = ? AND team_id = ?`, []any{orgID, teamID}, opts)
	if err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.ExternalAction{}, total, nil
	}
	query := `SELECT ` + externalActionColumns + ` FROM external_actions ea WHERE org_id = ? AND team_id = ?`
	query, args := appendExternalActionFilters(query, []any{orgID, teamID}, opts)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanExternalActionRows(rows)
	return out, total, err
}

// ListByConversation returns one conversation's actions, newest first. A detached row
// (conversation_id NULL after the conversation was purged) never matches, so a
// purged conversation's actions survive in the org feed but no longer answer
// for a conversation that is gone.
func (s *externalActionStore) ListByConversation(ctx context.Context, orgID, conversationID string, opts domain.ExternalActionListOpts) ([]domain.ExternalAction, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	total, err := s.countExternalActions(ctx, `org_id = ? AND conversation_id = ?`, []any{orgID, conversationID}, opts)
	if err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.ExternalAction{}, total, nil
	}
	query := `SELECT ` + externalActionColumns + ` FROM external_actions ea WHERE org_id = ? AND conversation_id = ?`
	query, args := appendExternalActionFilters(query, []any{orgID, conversationID}, opts)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanExternalActionRows(rows)
	return out, total, err
}

// prCreatedWithoutDeliverableQuery is ListPRCreatedWithoutDeliverableSystem's
// statement, binding org_id, action, the time bound and scope in that order.
// The time bound compares occurred_at directly, unlike the feeds' datetime()
// form: a function around the column keeps it from narrowing the index, and
// this read runs every reconcile cycle.
const prCreatedWithoutDeliverableQuery = `
	SELECT ` + externalActionColumns + `
	  FROM external_actions ea
	 WHERE ea.org_id = ? AND ea.action = ? AND ea.occurred_at >= ?
	   AND ea.scope = ? AND ea.node_id IS NOT NULL AND ea.conversation_id IS NOT NULL
	   AND NOT EXISTS (
	       SELECT 1 FROM deliverables a
	        WHERE a.org_id = ea.org_id AND a.scope = ea.scope AND a.node_id = ea.node_id
	   )
	 ORDER BY ea.occurred_at, ea.id
`

// ListPRCreatedWithoutDeliverableSystem is the PR backstop's read (see the
// interface). occurred_at holds offset-free UTC text ('YYYY-MM-DD HH:MM:SS'
// from the column default, with a fractional part on rows the UTC
// normalization rewrote), so a bound rendered in the default's layout orders
// against both shapes as text.
func (s *externalActionStore) ListPRCreatedWithoutDeliverableSystem(ctx context.Context, orgID, scope string, since time.Time) ([]domain.ExternalAction, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if err := db.RequireExternalObjectScope(scope); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, prCreatedWithoutDeliverableQuery,
		orgID, domain.ActionPRCreated, since.UTC().Format(time.DateTime), scope)
	if err != nil {
		return nil, err
	}
	return scanExternalActionRows(rows)
}

// presentPostedCommentKeysQuery is PresentPostedCommentKeysSystem's statement,
// binding org_id, scope, the object kind, the two comment actions and the keys
// as one JSON array, in that order. SQLite binds no arrays, and json_each
// reads the list from one bind, so the statement is the same for any number
// of keys and the object-key index is searched once per key.
const presentPostedCommentKeysQuery = `
	SELECT object_key
	  FROM external_actions
	 WHERE org_id = ? AND scope = ? AND object_kind = ? AND action IN (?, ?)
	   AND object_key IN (SELECT value FROM json_each(?))`

// PresentPostedCommentKeysSystem is the tracker's read of which comments TF
// posted (see the interface).
func (s *externalActionStore) PresentPostedCommentKeysSystem(ctx context.Context, orgID, scope string, keys []string) (map[string]bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if err := db.RequireExternalObjectScope(scope); err != nil {
		return nil, err
	}
	present := map[string]bool{}
	if len(keys) == 0 {
		return present, nil
	}
	list, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, presentPostedCommentKeysQuery,
		orgID, scope, domain.ObjectKindComment, domain.ActionCommentPosted, domain.ActionIssueCommentPosted, string(list))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		present[key] = true
	}
	return present, rows.Err()
}

// CountByConversations is the Postgres impl's grouped COUNT (see the
// interface). The ids bind as one JSON array read through json_each, so the
// set needs no chunking against SQLite's variable limit.
func (s *externalActionStore) CountByConversations(ctx context.Context, orgID string, conversationIDs []string) (map[string]int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(conversationIDs))
	if len(conversationIDs) == 0 {
		return counts, nil
	}
	ids, err := json.Marshal(conversationIDs)
	if err != nil {
		return nil, fmt.Errorf("encode conversation ids: %w", err)
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT conversation_id, COUNT(*)
		  FROM external_actions
		 WHERE org_id = ? AND conversation_id IN (SELECT value FROM json_each(?))
		 GROUP BY conversation_id
	`, orgID, string(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var conversationID string
		var n int
		if err := rows.Scan(&conversationID, &n); err != nil {
			return nil, err
		}
		counts[conversationID] = n
	}
	return counts, rows.Err()
}

// ListAgentActionsForBlueprintRunSystem is the close phase's read of a run's
// own actions (see the interface). occurred_at is whole seconds here, so the
// rows of one second break on rowid, which is the order they were written.
func (s *externalActionStore) ListAgentActionsForBlueprintRunSystem(ctx context.Context, orgID, blueprintRunID, action string) ([]domain.ExternalAction, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+externalActionColumns+`
		  FROM external_actions
		 WHERE org_id = ? AND action = ? AND performed_by = ?
		   AND conversation_id IN (
		       SELECT c.id FROM conversations c WHERE c.blueprint_run_id = ?
		   )
		 ORDER BY occurred_at DESC, rowid DESC
	`, orgID, action, domain.PerformedByAgent, blueprintRunID)
	if err != nil {
		return nil, err
	}
	return scanExternalActionRows(rows)
}

// appendExternalActionFilters appends opts' optional provider/action/actor/time
// predicates, the newest-first ORDER BY, and LIMIT/OFFSET to a SELECT whose WHERE
// is already open. occurred_at is whole seconds, so rows of one second break on
// rowid, newest write first. Time bounds wrap both sides in datetime() — the
// spend/deliverable store pattern — so a bound time.Time compares correctly
// against the CURRENT_TIMESTAMP-formatted occurred_at regardless of the
// driver's bind serialization.
func appendExternalActionFilters(query string, args []any, opts domain.ExternalActionListOpts) (string, []any) {
	query, args = appendExternalActionPredicates(query, args, opts)
	query += ` ORDER BY occurred_at DESC, ea.rowid DESC`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
		if opts.Offset > 0 {
			query += ` OFFSET ?`
			args = append(args, opts.Offset)
		}
	}
	return query, args
}

// appendExternalActionPredicates is the filter half alone — no ordering, no
// window — so the count and the page apply exactly the same predicate. The
// statement names external_actions as `ea`, which the object-id filter's
// expression reads.
func appendExternalActionPredicates(query string, args []any, opts domain.ExternalActionListOpts) (string, []any) {
	if opts.Provider != "" {
		query += ` AND provider = ?`
		args = append(args, opts.Provider)
	}
	if opts.Action != "" {
		query += ` AND action = ?`
		args = append(args, opts.Action)
	}
	if opts.ActorUserID != "" {
		query += ` AND actor_user_id = ?`
		args = append(args, opts.ActorUserID)
	}
	if opts.ObjectID != "" {
		query += ` AND (` + sqliteActionObjectIDSQL + `) = ? AND ` + db.ArtifactRowSQL()
		args = append(args, opts.ObjectID)
	}
	if opts.Refused {
		placeholders, refusals := inListArgs(domain.RefusalOrFailureActions())
		query += ` AND action IN (` + placeholders + `)`
		args = append(args, refusals...)
	}
	if !opts.Since.IsZero() {
		query += ` AND datetime(occurred_at) >= datetime(?)`
		args = append(args, opts.Since)
	}
	if !opts.Until.IsZero() {
		query += ` AND datetime(occurred_at) < datetime(?)`
		args = append(args, opts.Until)
	}
	return query, args
}

// countExternalActions runs the page's predicate as a COUNT on the same
// connection, so "showing 50 of 213" cannot be assembled from two snapshots.
func (s *externalActionStore) countExternalActions(ctx context.Context, where string, base []any, opts domain.ExternalActionListOpts) (int, error) {
	query, args := appendExternalActionPredicates(`SELECT COUNT(*) FROM external_actions ea WHERE `+where, base, opts)
	var n int
	err := s.q.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

// --- Helpers ---

func scanExternalActionRows(rows *sql.Rows) ([]domain.ExternalAction, error) {
	defer rows.Close()
	var out []domain.ExternalAction
	for rows.Next() {
		var e domain.ExternalAction
		if err := scanExternalAction(rows, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// scanExternalAction fills e from one row. rowScanner (defined in repos.go)
// unifies *sql.Row and *sql.Rows.
func scanExternalAction(sc rowScanner, e *domain.ExternalAction) error {
	return sc.Scan(
		&e.ID, &e.OrgID, &e.TeamID, &e.Provider, &e.Scope, &e.Action, &e.Target,
		&e.ExternalID, &e.URL, &e.FromState, &e.ToState, &e.ConversationID,
		&e.ActorUserID, &e.Credential, &e.DedupKey, &e.DetailJSON, &e.NodeID, &e.EntityID,
		&e.RepositoryID, &e.PerformedBy, &e.ObjectKind, &e.ObjectKey, &e.OccurredAt,
	)
}
