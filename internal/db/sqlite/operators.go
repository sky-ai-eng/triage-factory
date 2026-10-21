package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// operatorStore is the SQLite impl of db.OperatorStore. N=1: the single local
// user is implicitly the operator, so this table is effectively unused in
// local mode — but the store is implemented for interface + conformance parity
// across dialects, the same rule instances / placement_overrides followed.
type operatorStore struct{ q queryer }

func newOperatorStore(q queryer) db.OperatorStore {
	return &operatorStore{q: q}
}

// NewOperatorStore builds a standalone store against a pooled *sql.DB — the
// narrow constructor the `triagefactory operator` CLI uses.
func NewOperatorStore(conn *sql.DB) db.OperatorStore {
	return newOperatorStore(conn)
}

var _ db.OperatorStore = (*operatorStore)(nil)

func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// operatorColumns is the projection Add returns and List reads, so the write
// shape and the read shape are one list.
const operatorColumns = `email, user_id, added_at, added_by`

func scanOperator(row interface{ Scan(...any) error }) (domain.Operator, error) {
	var op domain.Operator
	var userID, addedBy sql.NullString
	if err := row.Scan(&op.Email, &userID, &op.AddedAt, &addedBy); err != nil {
		return domain.Operator{}, err
	}
	op.UserID = userID.String
	op.AddedBy = addedBy.String
	return op, nil
}

// Add inserts the row pending: local mode has no login identities to resolve
// the address to a principal. The conflict arm is DO UPDATE rather than DO
// NOTHING so a re-add returns the row too, and it writes user_id the way the
// Postgres arm does, onto a pending row only.
func (s *operatorStore) Add(ctx context.Context, email, addedBy string) (domain.Operator, error) {
	return scanOperator(s.q.QueryRowContext(ctx, `
		INSERT INTO operators (email, user_id, added_by) VALUES (?, NULL, NULLIF(?, ''))
		ON CONFLICT(email) DO UPDATE SET user_id = COALESCE(operators.user_id, excluded.user_id)
		RETURNING `+operatorColumns,
		normalizeEmail(email), strings.TrimSpace(addedBy)))
}

func (s *operatorStore) Remove(ctx context.Context, email string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `DELETE FROM operators WHERE email = ?`, normalizeEmail(email))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// IsOperatorUserSystem matches a grant bound to userID. A pending grant is
// matched through a principal's verified login identities, which local mode
// does not have, so no pending grant matches here; the fleet gate admits local
// mode's single user before it asks.
func (s *operatorStore) IsOperatorUserSystem(ctx context.Context, userID string) (bool, error) {
	if strings.TrimSpace(userID) == "" {
		return false, nil
	}
	var ok bool
	err := s.q.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM operators WHERE user_id = ?)`, userID).Scan(&ok)
	return ok, err
}

func (s *operatorStore) List(ctx context.Context) ([]domain.Operator, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+operatorColumns+` FROM operators ORDER BY email ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Operator
	for rows.Next() {
		op, err := scanOperator(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}
