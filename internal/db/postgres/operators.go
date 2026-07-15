package postgres

import (
	"context"
	"database/sql"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// operatorStore is the Postgres impl of db.OperatorStore — the deployment-
// operator identity. Wired against the ADMIN (BYPASSRLS) pool, same posture as
// instances: operators is a system-only table (an operator is deployment
// config, not tenant data), so the superuser/tf_system pool does all I/O.
type operatorStore struct{ admin queryer }

func newOperatorStore(admin queryer) db.OperatorStore {
	return &operatorStore{admin: admin}
}

// NewOperatorStore builds a standalone store against a pooled *sql.DB — the
// narrow constructor the `triagefactory operator` CLI uses.
func NewOperatorStore(admin *sql.DB) db.OperatorStore {
	return newOperatorStore(admin)
}

var _ db.OperatorStore = (*operatorStore)(nil)

func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func (s *operatorStore) Add(ctx context.Context, email, addedBy string) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO operators (email, added_by) VALUES ($1, NULLIF($2, ''))
		ON CONFLICT (email) DO NOTHING
	`, normalizeEmail(email), strings.TrimSpace(addedBy))
	if err != nil {
		return wrapAdminPoolPermErr(err, "operators.Add")
	}
	return nil
}

func (s *operatorStore) Remove(ctx context.Context, email string) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `DELETE FROM operators WHERE email = $1`, normalizeEmail(email))
	if err != nil {
		return false, wrapAdminPoolPermErr(err, "operators.Remove")
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *operatorStore) IsOperator(ctx context.Context, email string) (bool, error) {
	norm := normalizeEmail(email)
	if norm == "" {
		return false, nil
	}
	var exists int
	err := s.admin.QueryRowContext(ctx, `SELECT 1 FROM operators WHERE email = $1 LIMIT 1`, norm).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, wrapAdminPoolPermErr(err, "operators.IsOperator")
	}
	return true, nil
}

func (s *operatorStore) List(ctx context.Context) ([]domain.Operator, error) {
	rows, err := s.admin.QueryContext(ctx, `SELECT email, added_at, added_by FROM operators ORDER BY email ASC`)
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "operators.List")
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
