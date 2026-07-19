package sqlite

import (
	"context"
	"database/sql"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// authEventStore is the SQLite impl of db.AuthEventStore — the SOC2
// authentication audit log of record (TFAC-76). SQLite is single-tenant (local
// mode, N=1) with no RLS; local mode has no GoTrue, so in practice no auth events
// are ever written (the table is PARITY-ONLY, kept shape-identical to the
// Postgres baseline so a future multi-user SQLite needs no rewiring). The
// Postgres impl runs every method on the admin (BYPASSRLS) pool; here the one
// connection serves all I/O.
//
// No assertLocalOrg guard: org_id is nullable (a pre-auth failure carries none),
// and the write-sites are all gated on the multi-mode auth stack — none fire in
// local mode — so the local-org invariant other stores enforce doesn't apply.
type authEventStore struct{ q queryer }

func newAuthEventStore(q queryer) db.AuthEventStore {
	return &authEventStore{q: q}
}

// NewAuthEventStore builds a standalone db.AuthEventStore against a pooled
// *sql.DB — the narrow constructor the `triagefactory operator` CLI uses to
// append its operator-grant/revoke audit rows without the full db.Stores bundle.
func NewAuthEventStore(conn *sql.DB) db.AuthEventStore {
	return newAuthEventStore(conn)
}

var _ db.AuthEventStore = (*authEventStore)(nil)

// authEventColumns is the SELECT list scanned into a domain.AuthEvent via
// scanAuthEvent. Nullable columns coalesce to ” so the scan targets are plain
// strings. Same order the Postgres impl projects.
const authEventColumns = `
	id, COALESCE(org_id, ''), COALESCE(user_id, ''), COALESCE(session_id, ''),
	event_type, COALESCE(ip_address, ''), COALESCE(user_agent, ''),
	COALESCE(detail_json, ''), created_at
`

func (s *authEventStore) RecordSystem(ctx context.Context, e domain.AuthEvent) error {
	id := e.ID
	if id == "" {
		id = uuid.New().String()
	}
	// created_at is left to the column DEFAULT CURRENT_TIMESTAMP. Empty nullable
	// fields serialize to SQL NULL via nullIfEmpty.
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO auth_events
			(id, org_id, user_id, session_id, event_type, ip_address, user_agent, detail_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`,
		id, nullIfEmpty(e.OrgID), nullIfEmpty(e.UserID), nullIfEmpty(e.SessionID),
		e.EventType, nullIfEmpty(e.IPAddress), nullIfEmpty(e.UserAgent), nullIfEmpty(e.DetailJSON),
	)
	return err
}

func (s *authEventStore) ListByOrgSystem(ctx context.Context, orgID string, opts domain.AuthEventListOpts) ([]domain.AuthEvent, error) {
	// org_id = ? naturally excludes the pre-auth (NULL-org) rows — NULL = ? is
	// never true. The optional opts filters narrow the newest-first window.
	query := `SELECT ` + authEventColumns + ` FROM auth_events WHERE org_id = ?`
	query, args := appendAuthEventFilters(query, []any{orgID}, opts)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanAuthEventRows(rows)
}

func (s *authEventStore) ListByUserSystem(ctx context.Context, userID string, opts domain.AuthEventListOpts) ([]domain.AuthEvent, error) {
	query := `SELECT ` + authEventColumns + ` FROM auth_events WHERE user_id = ?`
	query, args := appendAuthEventFilters(query, []any{userID}, opts)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanAuthEventRows(rows)
}

func (s *authEventStore) ClaimSeatSystem(ctx context.Context, period, userID string, maxSeats int) (bool, int, error) {
	// SQLite serializes writers via the single database write lock, so the
	// count-then-claim inside one transaction is atomic without an advisory lock.
	// Local mode is N=1 with no GoTrue login, so this is parity-only in practice —
	// per-seat enforcement runs on the multi-mode (Postgres) login path.
	var (
		admitted bool
		seats    int
	)
	err := inTx(ctx, s.q, func(q queryer) error {
		var have int
		if err := q.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM seat_claims WHERE period = ? AND user_id = ?)`,
			period, userID).Scan(&have); err != nil {
			return err
		}
		if err := q.QueryRowContext(ctx,
			`SELECT count(*) FROM seat_claims WHERE period = ?`, period).Scan(&seats); err != nil {
			return err
		}
		if have == 1 {
			admitted = true // returning claimant — already holds a seat this period
			return nil
		}
		if seats >= maxSeats {
			admitted = false // a new claimant beyond the cap
			return nil
		}
		if _, err := q.ExecContext(ctx,
			`INSERT INTO seat_claims (period, user_id) VALUES (?, ?)
			 ON CONFLICT (period, user_id) DO NOTHING`, period, userID); err != nil {
			return err
		}
		admitted = true
		seats++ // this login's fresh claim
		return nil
	})
	if err != nil {
		return false, 0, err
	}
	return admitted, seats, nil
}

// appendAuthEventFilters appends opts' optional event_type/time predicates, the
// newest-first ORDER BY, and LIMIT/OFFSET to a SELECT whose WHERE is already
// open. Time bounds wrap both sides in datetime() — the external_actions store
// pattern — so a bound time.Time compares correctly against the
// CURRENT_TIMESTAMP-formatted created_at regardless of the driver's bind
// serialization.
func appendAuthEventFilters(query string, args []any, opts domain.AuthEventListOpts) (string, []any) {
	if opts.EventType != "" {
		query += ` AND event_type = ?`
		args = append(args, opts.EventType)
	}
	if !opts.Since.IsZero() {
		query += ` AND datetime(created_at) >= datetime(?)`
		args = append(args, opts.Since)
	}
	if !opts.Until.IsZero() {
		query += ` AND datetime(created_at) < datetime(?)`
		args = append(args, opts.Until)
	}
	query += ` ORDER BY created_at DESC, id DESC`
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

// scanAuthEvent fills e from one row. rowScanner (defined in repos.go) unifies
// *sql.Row and *sql.Rows.
func scanAuthEvent(sc rowScanner, e *domain.AuthEvent) error {
	return sc.Scan(
		&e.ID, &e.OrgID, &e.UserID, &e.SessionID, &e.EventType,
		&e.IPAddress, &e.UserAgent, &e.DetailJSON, &e.CreatedAt,
	)
}
