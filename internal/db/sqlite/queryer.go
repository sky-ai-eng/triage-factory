package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// queryer is the minimum interface a SQLite store impl needs from its
// underlying handle. Both *sql.DB and *sql.Tx satisfy it, so the same
// store body runs inside or outside a transaction — WithTx constructs
// a fresh TxStores against a *sql.Tx, every method outside WithTx
// runs against the *sql.DB held on Store. database/sql doesn't ship
// a common interface that both types satisfy, so we declare our own.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// inListChunkSize bounds the number of `?` placeholders an IN-list query
// binds in one statement. SQLite's SQLITE_LIMIT_VARIABLE_NUMBER is 999 on
// older builds (32766 on modern ones); 500 stays comfortably inside both,
// matching the chunking in entities.go / factory.go / tasks.go. Read methods
// that take a caller-supplied id slice run their query once per chunk and
// merge the results, so an unbounded id set degrades to extra round-trips
// rather than a "too many SQL variables" error.
const inListChunkSize = 500

// chunkIDs splits ids into consecutive sub-slices of at most inListChunkSize.
// Returns nil for an empty input (the caller's range loop then no-ops). The
// sub-slices alias ids — callers only read them.
func chunkIDs(ids []string) [][]string {
	if len(ids) == 0 {
		return nil
	}
	chunks := make([][]string, 0, (len(ids)+inListChunkSize-1)/inListChunkSize)
	for start := 0; start < len(ids); start += inListChunkSize {
		end := start + inListChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[start:end])
	}
	return chunks
}

// inTx runs fn against a queryer that's guaranteed to be a transaction:
//   - if q is already a *sql.Tx (caller composed us inside Stores.Tx.WithTx),
//     fn runs against that tx directly so the outer commit/rollback wins.
//   - if q is a *sql.DB, inTx opens a fresh tx, runs fn against it, and
//     commits or rolls back based on fn's return.
//
// Used by store methods that must apply a multi-statement operation
// atomically — e.g. UpdateTaskScores chunks its UPDATE into batches
// to keep the placeholder count conservatively low across SQLite
// builds, and wraps all chunks in a single tx so a mid-stream
// failure rolls back any chunks that already committed.
func inTx(ctx context.Context, q queryer, fn func(queryer) error) error {
	switch v := q.(type) {
	case *sql.Tx:
		return fn(v)
	case *sql.DB:
		tx, err := v.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return fmt.Errorf("sqlite store: unexpected queryer type %T", q)
	}
}
