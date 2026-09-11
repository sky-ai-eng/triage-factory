package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Claims is the per-request identity payload that RLS policies read via
// the `request.jwt.claims` GUC. Mirrors the subset of fields the SQL
// helpers in `tf.*` actually look at — `sub` and `org_id`.
//
// Both fields are strings (UUID-shaped) because the GUC payload is JSON
// and the SQL helpers parse via `(current_setting(...))::jsonb ->> 'sub'`.
// Real uuid.UUIDs marshal as quoted strings anyway, but using strings
// directly here keeps callers from accidentally passing a uuid.Nil.
type Claims struct {
	Sub   string `json:"sub"`
	OrgID string `json:"org_id,omitempty"`
}

// WithTx runs fn inside a transaction with `request.jwt.claims` set to
// the JSON payload of claims. SET LOCAL bounds the GUC change to the
// transaction so it doesn't bleed across the connection pool.
//
// Use this for every read/write that needs RLS enforcement. D7 wires
// only the new auth handlers through this helper; D9 retrofits the
// existing /api/* handlers.
//
// Caveats:
//   - The connection should be an APP-pool connection (the role bound
//     to RLS policies — `tf_app` or `authenticated` depending on the
//     compose vs test harness convention). Calling this on the admin
//     (BYPASSRLS) pool is technically harmless — the GUC gets set but
//     policies don't gate the queries — but it's a footgun, so the
//     test harness's WithUser explicitly switches role.
//   - fn must not capture the *sql.Tx beyond the call. Using it after
//     return is a use-after-commit/rollback bug.
//   - Rollback on fn error is best-effort; if Rollback itself errors,
//     the original fn error takes precedence (it's the meaningful one
//     for the caller).
func WithTx(ctx context.Context, dbConn *sql.DB, claims Claims, fn func(*sql.Tx) error) error {
	tx, err := dbConn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	payload, err := json.Marshal(claims)
	if err != nil {
		return fmt.Errorf("marshal claims: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT set_config('request.jwt.claims', $1, true)`, string(payload),
	); err != nil {
		return TxCause(ctx, fmt.Errorf("set request.jwt.claims: %w", err))
	}

	if err := fn(tx); err != nil {
		return TxCause(ctx, err)
	}
	return TxCause(ctx, tx.Commit())
}
