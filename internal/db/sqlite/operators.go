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

func (s *operatorStore) Add(ctx context.Context, email, addedBy string) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO operators (email, added_by) VALUES (?, NULLIF(?, ''))
		ON CONFLICT(email) DO NOTHING
	`, normalizeEmail(email), strings.TrimSpace(addedBy))
	return err
}

func (s *operatorStore) Remove(ctx context.Context, email string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `DELETE FROM operators WHERE email = ?`, normalizeEmail(email))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// IsOperatorUserSystem always returns (false, nil): an operator is matched
// through a principal's login identities, which local mode does not have, and
// the fleet gate admits local mode's single user before it asks.
func (s *operatorStore) IsOperatorUserSystem(context.Context, string) (bool, error) {
	return false, nil
}

func (s *operatorStore) List(ctx context.Context) ([]domain.Operator, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT email, added_at, added_by FROM operators ORDER BY email ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Operator
	for rows.Next() {
		var op domain.Operator
		var addedBy sql.NullString
		if err := rows.Scan(&op.Email, &op.AddedAt, &addedBy); err != nil {
			return nil, err
		}
		op.AddedBy = addedBy.String
		out = append(out, op)
	}
	return out, rows.Err()
}
