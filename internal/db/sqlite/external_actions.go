package sqlite

import (
	"context"
	"database/sql"

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
// via scanExternalAction. Same order the Postgres impl projects.
const externalActionColumns = `
	id, org_id, COALESCE(team_id, ''), provider, action, target,
	COALESCE(external_id, ''), COALESCE(url, ''), COALESCE(from_state, ''),
	COALESCE(to_state, ''), COALESCE(conversation_id, ''), COALESCE(actor_user_id, ''),
	credential, dedup_key, COALESCE(detail_json, ''), occurred_at
`

func (s *externalActionStore) Record(ctx context.Context, orgID string, e domain.ExternalAction) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	id := e.ID
	if id == "" {
		id = uuid.New().String()
	}
	// An empty dedup_key is filled with a uuid so the append never collides (a
	// repeated action is recorded, not dropped); only the branch push passes a
	// deterministic key so its hook+proxy twin collapses. occurred_at is left to
	// the column DEFAULT. Empty nullable fields → SQL NULL via nullIfEmpty.
	dedupKey := e.DedupKey
	if dedupKey == "" {
		dedupKey = uuid.New().String()
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO external_actions
			(id, org_id, team_id, provider, action, target, external_id, url,
			 from_state, to_state, conversation_id, actor_user_id, credential, dedup_key, detail_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(org_id, dedup_key) DO NOTHING
	`,
		id, orgID, nullIfEmpty(e.TeamID), e.Provider, e.Action, e.Target,
		nullIfEmpty(e.ExternalID), nullIfEmpty(e.URL), nullIfEmpty(e.FromState),
		nullIfEmpty(e.ToState), nullIfEmpty(e.ConversationID), nullIfEmpty(e.ActorUserID),
		e.Credential, dedupKey, nullIfEmpty(e.DetailJSON),
	)
	return err
}

// RecordSystem is identical to Record in SQLite: local mode is single-tenant
// (N=1) with no RLS, so there is no admin/app pool split. The method exists for
// parity with the Postgres store, where the event-triggered bot runs + the Jira
// mirror (no JWT-claims context) need the admin pool.
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
	query := `SELECT ` + externalActionColumns + ` FROM external_actions WHERE org_id = ?`
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
	query := `SELECT ` + externalActionColumns + ` FROM external_actions WHERE org_id = ? AND team_id = ?`
	query, args := appendExternalActionFilters(query, []any{orgID, teamID}, opts)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanExternalActionRows(rows)
	return out, total, err
}

// ListByRun returns one conversation's actions, newest first. A detached row
// (conversation_id NULL after the run was purged) never matches, so a purged
// run's actions survive in the org feed but no longer answer for a run that is
// gone.
func (s *externalActionStore) ListByRun(ctx context.Context, orgID, conversationID string, opts domain.ExternalActionListOpts) ([]domain.ExternalAction, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	total, err := s.countExternalActions(ctx, `org_id = ? AND conversation_id = ?`, []any{orgID, conversationID}, opts)
	if err != nil {
		return nil, 0, err
	}
	query := `SELECT ` + externalActionColumns + ` FROM external_actions WHERE org_id = ? AND conversation_id = ?`
	query, args := appendExternalActionFilters(query, []any{orgID, conversationID}, opts)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanExternalActionRows(rows)
	return out, total, err
}

// appendExternalActionFilters appends opts' optional provider/action/actor/time
// predicates, the newest-first ORDER BY, and LIMIT/OFFSET to a SELECT whose WHERE
// is already open. Time bounds wrap both sides in datetime() — the spend/artifact
// store pattern — so a bound time.Time compares correctly against the
// CURRENT_TIMESTAMP-formatted occurred_at regardless of the driver's bind
// serialization.
func appendExternalActionFilters(query string, args []any, opts domain.ExternalActionListOpts) (string, []any) {
	query, args = appendExternalActionPredicates(query, args, opts)
	query += ` ORDER BY occurred_at DESC, id DESC`
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
// window — so the count and the page apply exactly the same predicate.
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
	query, args := appendExternalActionPredicates(`SELECT COUNT(*) FROM external_actions WHERE `+where, base, opts)
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
		&e.ID, &e.OrgID, &e.TeamID, &e.Provider, &e.Action, &e.Target,
		&e.ExternalID, &e.URL, &e.FromState, &e.ToState, &e.ConversationID,
		&e.ActorUserID, &e.Credential, &e.DedupKey, &e.DetailJSON, &e.OccurredAt,
	)
}
