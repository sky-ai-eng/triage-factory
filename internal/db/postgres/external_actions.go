package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// externalActionStore is the Postgres impl of db.ExternalActionStore — the
// append-only external-action audit log (TFAC-483). Holds the same two-pool
// split the artifacts store uses:
//
//   - q: app pool (tf_app, RLS-active). Record composes inside the claims-bearing
//     WithTx that runs the audited action (manual bot runs under synthetic
//     claims, the server approval/board handlers under the actor's claims), so
//     external_actions_all's WITH CHECK gates the write by org. ListByTeam reads
//     here under the same org-scoped policy.
//   - admin: admin pool (BYPASSRLS). RecordSystem routes here for the
//     event-triggered bot exec choke point + the Jira board mirror (no JWT
//     claims). ListByOrgSystem reads here, org-wide across teams.
//
// org_id stays in every statement as defense in depth. Append-only: both Record
// paths ON CONFLICT(org_id, dedup_key) DO NOTHING — a duplicate is rejected,
// never a mutation.
type externalActionStore struct {
	q     queryer
	admin queryer
}

func newExternalActionStore(q, admin queryer) db.ExternalActionStore {
	return &externalActionStore{q: q, admin: admin}
}

var _ db.ExternalActionStore = (*externalActionStore)(nil)

// pgExternalActionColumns is the SELECT list scanned into a domain.ExternalAction
// via scanExternalAction. Nullable columns coalesce to ” so the scan targets are
// plain strings, the same shape pgArtifactColumns uses.
const pgExternalActionColumns = `
	id::text, org_id::text, COALESCE(team_id::text, ''), provider, action, target,
	COALESCE(external_id, ''), COALESCE(url, ''), COALESCE(from_state, ''),
	COALESCE(to_state, ''), COALESCE(conversation_id::text, ''),
	COALESCE(actor_user_id::text, ''), credential, dedup_key,
	COALESCE(detail_json, ''), occurred_at
`

func (s *externalActionStore) Record(ctx context.Context, orgID string, e domain.ExternalAction) error {
	return s.record(ctx, s.q, orgID, e)
}

// RecordSystem runs the same insert on the admin pool (BYPASSRLS) for
// event-triggered bot runs + the Jira mirror, which hold an org identity but no
// JWT-claims context. See the type doc.
func (s *externalActionStore) RecordSystem(ctx context.Context, orgID string, e domain.ExternalAction) error {
	return s.record(ctx, s.admin, orgID, e)
}

// record inserts one audit row. A caller-supplied e.ID / e.DedupKey is honored
// (parity with SQLite); an empty one falls back to gen_random_uuid() server-side
// — for dedup_key that yields a unique, non-colliding key so the append never
// drops a repeated action (only the branch push passes a deterministic key).
// occurred_at is left to the column DEFAULT now(). Empty nullable columns (”) →
// NULL via NULLIF; the uuid columns cast after the NULLIF so an empty string
// never reaches ::uuid. ON CONFLICT DO NOTHING keeps it append-only.
func (s *externalActionStore) record(ctx context.Context, q queryer, orgID string, e domain.ExternalAction) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO external_actions
			(id, org_id, team_id, provider, action, target, external_id, url,
			 from_state, to_state, conversation_id, actor_user_id, credential, dedup_key, detail_json)
		VALUES (
			COALESCE(NULLIF($1, '')::uuid, gen_random_uuid()), $2, NULLIF($3, '')::uuid,
			$4, $5, $6, NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''),
			NULLIF($11, '')::uuid, NULLIF($12, '')::uuid, $13,
			COALESCE(NULLIF($14, ''), gen_random_uuid()::text), NULLIF($15, '')
		)
		ON CONFLICT (org_id, dedup_key) DO NOTHING
	`,
		e.ID, orgID, e.TeamID, e.Provider, e.Action, e.Target, e.ExternalID, e.URL,
		e.FromState, e.ToState, e.ConversationID, e.ActorUserID, e.Credential, e.DedupKey, e.DetailJSON,
	)
	return err
}

func (s *externalActionStore) ListByOrgSystem(ctx context.Context, orgID string, opts domain.ExternalActionListOpts) ([]domain.ExternalAction, error) {
	query := `SELECT ` + pgExternalActionColumns + ` FROM external_actions WHERE org_id = $1`
	query, args := appendPgExternalActionFilters(query, []any{orgID}, opts)
	rows, err := s.admin.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanExternalActionRows(rows)
}

func (s *externalActionStore) ListByTeam(ctx context.Context, orgID, teamID string, opts domain.ExternalActionListOpts) ([]domain.ExternalAction, error) {
	query := `SELECT ` + pgExternalActionColumns + ` FROM external_actions WHERE org_id = $1 AND team_id = $2`
	query, args := appendPgExternalActionFilters(query, []any{orgID, teamID}, opts)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanExternalActionRows(rows)
}

// appendPgExternalActionFilters appends opts' optional provider/action/actor/time
// predicates, the newest-first ORDER BY, and LIMIT/OFFSET to a SELECT whose WHERE
// is already open. Placeholders number from len(args)+1 so the leading binds
// (org_id[, team_id]) never collide — the same defensive numbering
// appendPgArtifactFilters uses.
func appendPgExternalActionFilters(query string, args []any, opts domain.ExternalActionListOpts) (string, []any) {
	if opts.Provider != "" {
		args = append(args, opts.Provider)
		query += fmt.Sprintf(" AND provider = $%d", len(args))
	}
	if opts.Action != "" {
		args = append(args, opts.Action)
		query += fmt.Sprintf(" AND action = $%d", len(args))
	}
	if opts.ActorUserID != "" {
		args = append(args, opts.ActorUserID)
		query += fmt.Sprintf(" AND actor_user_id = $%d", len(args))
	}
	if !opts.Since.IsZero() {
		args = append(args, opts.Since)
		query += fmt.Sprintf(" AND occurred_at >= $%d", len(args))
	}
	if !opts.Until.IsZero() {
		args = append(args, opts.Until)
		query += fmt.Sprintf(" AND occurred_at < $%d", len(args))
	}
	query += " ORDER BY occurred_at DESC, id DESC"
	if opts.Limit > 0 {
		args = append(args, opts.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
		if opts.Offset > 0 {
			args = append(args, opts.Offset)
			query += fmt.Sprintf(" OFFSET $%d", len(args))
		}
	}
	return query, args
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

func scanExternalAction(sc rowScanner, e *domain.ExternalAction) error {
	return sc.Scan(
		&e.ID, &e.OrgID, &e.TeamID, &e.Provider, &e.Action, &e.Target,
		&e.ExternalID, &e.URL, &e.FromState, &e.ToState, &e.ConversationID,
		&e.ActorUserID, &e.Credential, &e.DedupKey, &e.DetailJSON, &e.OccurredAt,
	)
}
