package postgres

import (
	"context"
	"database/sql"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// operatorStore is the Postgres impl of db.OperatorStore — the deployment-
// operator grants. Wired against the ADMIN (BYPASSRLS) pool, same posture as
// instances: operators is a system-only table (an operator is deployment
// config, not tenant data), so the control plane's admin pool does all I/O.
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

// operatorColumns is the projection Add returns and List reads, so the write
// shape and the read shape are one list.
const operatorColumns = `email, user_id::text, added_at, added_by`

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

// Add binds the row to the principal holding the address verified. At most one
// principal holds a verified address (user_identities_one_principal_per_
// verified_email), so the subquery names at most one, however many of its
// identities carry the address. The conflict arm writes user_id only onto a
// pending row, and DO UPDATE rather than DO NOTHING so a re-add returns the
// row too.
func (s *operatorStore) Add(ctx context.Context, email, addedBy string) (domain.Operator, error) {
	row := s.admin.QueryRowContext(ctx, `
		INSERT INTO operators (email, user_id, added_by)
		VALUES ($1,
		        (SELECT i.user_id FROM public.user_identities i
		          WHERE lower(i.email) = lower($1) AND i.email_verified
		          LIMIT 1),
		        NULLIF($2, ''))
		ON CONFLICT (email) DO UPDATE
		   SET user_id = COALESCE(operators.user_id, EXCLUDED.user_id)
		RETURNING `+operatorColumns,
		normalizeEmail(email), strings.TrimSpace(addedBy))
	op, err := scanOperator(row)
	if err != nil {
		return domain.Operator{}, wrapAdminPoolPermErr(err, "operators.Add")
	}
	return op, nil
}

func (s *operatorStore) Remove(ctx context.Context, email string) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `DELETE FROM operators WHERE email = $1`, normalizeEmail(email))
	if err != nil {
		return false, wrapAdminPoolPermErr(err, "operators.Remove")
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *operatorStore) IsOperatorUserSystem(ctx context.Context, userID string) (bool, error) {
	if strings.TrimSpace(userID) == "" {
		return false, nil
	}
	// The address arm reads pending rows only, and folds both sides: Add
	// stores the address lower-cased, but a row written by hand in another
	// case would otherwise never match, silently.
	var ok bool
	err := s.admin.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM public.operators o WHERE o.user_id = $1
		) OR EXISTS (
			SELECT 1
			  FROM public.operators o
			  JOIN public.user_identities i ON lower(i.email) = lower(o.email)
			 WHERE o.user_id IS NULL AND i.user_id = $1 AND i.email_verified
		)`, userID).Scan(&ok)
	if err != nil {
		return false, wrapAdminPoolPermErr(err, "operators.IsOperatorUserSystem")
	}
	return ok, nil
}

func (s *operatorStore) List(ctx context.Context) ([]domain.Operator, error) {
	rows, err := s.admin.QueryContext(ctx, `SELECT `+operatorColumns+` FROM operators ORDER BY email ASC`)
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "operators.List")
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
