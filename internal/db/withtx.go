package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
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
	bound, err := withClaims(ctx, claims, fn)
	if err != nil {
		return err
	}
	return InTx(ctx, dbConn, bound)
}

// WithReadTx is WithTx for a body that only reads: the same claims, set the
// same way, on InReadTx's transaction — so a write inside fn is refused by
// the engine. Claims are a Postgres concept, which fixes the dialect.
func WithReadTx(ctx context.Context, dbConn *sql.DB, claims Claims, fn func(*sql.Tx) error) error {
	bound, err := withClaims(ctx, claims, fn)
	if err != nil {
		return err
	}
	return InReadTx(ctx, dbConn, DialectPostgres, bound)
}

// withClaims wraps fn so the transaction it runs in carries claims. Shared
// by WithTx and WithReadTx so the two doors cannot set them differently.
func withClaims(ctx context.Context, claims Claims, fn func(*sql.Tx) error) (func(*sql.Tx) error, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return nil, fmt.Errorf("marshal claims: %w", err)
	}
	return func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`SELECT set_config('request.jwt.claims', $1, true)`, string(payload),
		); err != nil {
			return fmt.Errorf("set request.jwt.claims: %w", err)
		}
		return fn(tx)
	}, nil
}

// InTx runs fn inside a transaction on conn, committing when fn returns nil
// and rolling back otherwise. It is the claims-less sibling of WithTx — the
// same begin/body/commit boundary for callers that set no
// `request.jwt.claims`: the admin (BYPASSRLS) pool, the SQLite handle, a
// store composing its own multi-statement write.
//
// Both the body's error and Commit's go through TxCause, which is the reason
// to reach for this rather than hand-roll the three lines. database/sql binds
// a Tx to its ctx and rolls it back from a background goroutine the moment
// that ctx is canceled, so whatever lands after the rollback reports
// sql.ErrTxDone instead of the cancellation that caused it — and a caller
// classifying a client disconnect by errors.Is(err, context.Canceled) would
// read half of them as internal faults.
//
// Same caveats as WithTx: fn must not retain tx past the call, and rollback
// on a body error is best-effort — the body's error is the one returned.
func InTx(ctx context.Context, conn *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return TxCause(ctx, err)
	}
	return TxCause(ctx, tx.Commit())
}

// Dialect names as the store constructors, Migrate and BuildStoreExtensions
// spell them — the value InReadTx keys its guard on.
const (
	DialectSQLite   = "sqlite"
	DialectPostgres = "postgres"
)

// readTxOpts selects the read-only BEGIN on both drivers: BEGIN READ ONLY on
// Postgres, and on modernc.org/sqlite a plain (DEFERRED) BEGIN in place of
// the handle's IMMEDIATE default.
var readTxOpts = &sql.TxOptions{ReadOnly: true}

// InReadTx is InTx for a body that only reads, and the difference is
// enforced rather than declared: a write inside fn fails with the engine's
// read-only error instead of committing.
//
// The door exists because the SQLite handle begins every ordinary
// transaction IMMEDIATE (see OpenAt): the write lock is taken at BEGIN so a
// read-then-write body never hits the unretryable lock upgrade. A body that
// never writes pays for that lock without needing it — it waits out another
// process's write, and holds that process off for its own duration — where
// a DEFERRED transaction in WAL mode is free in both directions. ReadOnly
// on the TxOptions is what selects DEFERRED again.
//
// It is enforced because ReadOnly alone only changes the BEGIN. SQLite would
// accept an UPDATE inside it, and that UPDATE is a DEFERRED lock upgrade —
// exactly the failure the handle's lock mode removes, reintroduced at one
// site by a body that was read-only when it was written. So on SQLite the
// transaction runs on a dedicated connection with PRAGMA query_only set for
// its lifetime, cleared (or the connection discarded) before the connection
// returns to the pool; Postgres enforces READ ONLY natively. A body that
// writes therefore fails on both dialects the first time a test runs it,
// rather than degrading a run on one of them.
//
// dialect is DialectSQLite or DialectPostgres. Anything else is refused
// rather than guessed: the guard differs per engine, and the handle cannot
// say which it is once the tracing driver has wrapped it.
func InReadTx(ctx context.Context, conn *sql.DB, dialect string, fn func(*sql.Tx) error) error {
	switch dialect {
	case DialectPostgres:
		return inReadTx(ctx, conn, fn)
	case DialectSQLite:
		return inSQLiteReadTx(ctx, conn, fn)
	default:
		return fmt.Errorf("read tx: unknown dialect %q", dialect)
	}
}

func inReadTx(ctx context.Context, conn *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := conn.BeginTx(ctx, readTxOpts)
	if err != nil {
		return fmt.Errorf("begin read tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return TxCause(ctx, err)
	}
	return TxCause(ctx, tx.Commit())
}

// inSQLiteReadTx pins one connection so the query_only pragma, which is
// connection state rather than transaction state, provably covers the
// transaction and nothing after it.
func inSQLiteReadTx(ctx context.Context, conn *sql.DB, fn func(*sql.Tx) error) error {
	c, err := conn.Conn(ctx)
	if err != nil {
		return fmt.Errorf("read tx: acquire connection: %w", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.ExecContext(ctx, `PRAGMA query_only = 1`); err != nil {
		return fmt.Errorf("read tx: set query_only: %w", err)
	}
	// The reset has to outlive the caller's ctx: database/sql rolls the
	// transaction back on cancellation, and a connection handed back to the
	// pool with the pragma still set would refuse every later write on it —
	// with one connection in the pool, every later write in the process. If
	// the reset fails anyway, the connection is discarded, never returned.
	defer func() {
		if _, err := c.ExecContext(context.WithoutCancel(ctx), `PRAGMA query_only = 0`); err != nil {
			_ = c.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()

	tx, err := c.BeginTx(ctx, readTxOpts)
	if err != nil {
		return fmt.Errorf("begin read tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return TxCause(ctx, err)
	}
	return TxCause(ctx, tx.Commit())
}
