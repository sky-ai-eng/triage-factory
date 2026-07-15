package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// authEventStore is the Postgres impl of db.AuthEventStore — the SOC2
// authentication audit log of record (TFAC-76). Wired against the ADMIN
// (BYPASSRLS) pool in postgres.New: auth_events is a system-only table (RLS
// deny-by-default, REVOKEd from the app roles, exactly like
// public.user_identities) — its writes + reads never carry user claims and
// org_id is frequently NULL, so an org-scoped policy can't gate it. The
// superuser pool does all I/O; the same admin-only shape as systemLLMRunStore.
// Append-only — never UPDATE/DELETE.
type authEventStore struct{ admin queryer }

func newAuthEventStore(admin queryer) db.AuthEventStore {
	return &authEventStore{admin: admin}
}

// NewAuthEventStore builds a standalone db.AuthEventStore against a pooled
// *sql.DB — the narrow constructor the `triagefactory operator` CLI uses to
// append its operator-grant/revoke audit rows without the full db.Stores bundle.
func NewAuthEventStore(admin *sql.DB) db.AuthEventStore {
	return newAuthEventStore(admin)
}

var _ db.AuthEventStore = (*authEventStore)(nil)

// pgAuthEventColumns is the SELECT list scanned into a domain.AuthEvent via
// scanAuthEvent. Nullable columns coalesce to ” (uuid/inet/jsonb cast ::text
// first; host() yields the bare address with no /32 mask) so the scan targets
// stay plain strings.
const pgAuthEventColumns = `
	id::text, COALESCE(org_id::text, ''), COALESCE(user_id::text, ''),
	COALESCE(session_id::text, ''), event_type, COALESCE(host(ip_address), ''),
	COALESCE(user_agent, ''), COALESCE(detail_json::text, ''), created_at
`

func (s *authEventStore) RecordSystem(ctx context.Context, e domain.AuthEvent) error {
	// A caller-supplied e.ID is honored (parity with SQLite); an empty e.ID falls
	// back to gen_random_uuid() server-side. created_at is left to the column
	// DEFAULT now(). Empty nullable columns ('') → NULL via NULLIF; the
	// uuid/inet/jsonb columns cast AFTER the NULLIF so an empty string never
	// reaches the cast.
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO auth_events
			(id, org_id, user_id, session_id, event_type, ip_address, user_agent, detail_json)
		VALUES (
			COALESCE(NULLIF($1, '')::uuid, gen_random_uuid()),
			NULLIF($2, '')::uuid, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid,
			$5, NULLIF($6, '')::inet, NULLIF($7, ''), NULLIF($8, '')::jsonb
		)
	`,
		e.ID, e.OrgID, e.UserID, e.SessionID, e.EventType, e.IPAddress, e.UserAgent, e.DetailJSON,
	)
	return err
}

func (s *authEventStore) ListByOrgSystem(ctx context.Context, orgID string, opts domain.AuthEventListOpts) ([]domain.AuthEvent, error) {
	// org_id = $1 naturally excludes the NULL-org rows (NULL = $1 is never true).
	query := `SELECT ` + pgAuthEventColumns + ` FROM auth_events WHERE org_id = $1::uuid`
	query, args := appendPgAuthEventFilters(query, []any{orgID}, opts)
	rows, err := s.admin.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanAuthEventRows(rows)
}

func (s *authEventStore) ListByUserSystem(ctx context.Context, userID string, opts domain.AuthEventListOpts) ([]domain.AuthEvent, error) {
	query := `SELECT ` + pgAuthEventColumns + ` FROM auth_events WHERE user_id = $1::uuid`
	query, args := appendPgAuthEventFilters(query, []any{userID}, opts)
	rows, err := s.admin.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanAuthEventRows(rows)
}

// appendPgAuthEventFilters appends opts' optional event_type/time predicates, the
// newest-first ORDER BY, and LIMIT/OFFSET to a SELECT whose WHERE is already
// open. Placeholders number from len(args)+1 so the leading bind (org_id /
// user_id) never collides — the same defensive numbering the other PG stores
// use.
func appendPgAuthEventFilters(query string, args []any, opts domain.AuthEventListOpts) (string, []any) {
	if opts.EventType != "" {
		args = append(args, opts.EventType)
		query += fmt.Sprintf(" AND event_type = $%d", len(args))
	}
	if !opts.Since.IsZero() {
		args = append(args, opts.Since)
		query += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	if !opts.Until.IsZero() {
		args = append(args, opts.Until)
		query += fmt.Sprintf(" AND created_at < $%d", len(args))
	}
	query += " ORDER BY created_at DESC, id DESC"
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

func scanAuthEventRows(rows *sql.Rows) ([]domain.AuthEvent, error) {
	defer rows.Close()
	var out []domain.AuthEvent
	for rows.Next() {
		var e domain.AuthEvent
		if err := scanAuthEvent(rows, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanAuthEvent(sc rowScanner, e *domain.AuthEvent) error {
	return sc.Scan(
		&e.ID, &e.OrgID, &e.UserID, &e.SessionID, &e.EventType,
		&e.IPAddress, &e.UserAgent, &e.DetailJSON, &e.CreatedAt,
	)
}
