package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// IsRetryable reports whether err is a failure that the same write, attempted
// again, can get past: the connection to the database was lost or could not be
// made, the server refused the transaction under concurrency (a serialization
// failure, a deadlock, a lock it could not take in time), or a timeout fired.
// Anything else — a constraint, an RLS refusal, a malformed statement — fails
// the same way every time, and so is not retryable.
//
// A cancelled context is not retryable: the caller has given up, and another
// attempt would fail at once on the same context. A deadline that ran out is,
// because it is a timeout; a retry loop must still stop when its own context's
// deadline is the one that ran out.
//
// Retrying is only safe for a write that is idempotent across attempts. A
// connection lost after the server committed looks exactly like one lost
// before it did, so a second attempt has to be one the first cannot have
// already applied — an upsert, or an insert whose conflict key was fixed
// before the first attempt.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) || pgconn.Timeout(err) || pgconn.SafeToRetry(err) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return retryableSQLState(pgErr.Code)
	}
	var liteErr *sqlite.Error
	if errors.As(err, &liteErr) {
		// Code is the extended result code; its low byte is the primary one,
		// so SQLITE_BUSY_SNAPSHOT and friends land on SQLITE_BUSY.
		switch liteErr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		}
	}
	return false
}

// retryableSQLState reports whether a Postgres SQLSTATE names a transient
// failure: the connection exception class (but not 08P01, a protocol
// violation, which a new connection repeats), the concurrency refusals of
// class 40 (but not 40002, an integrity violation at commit), a lock or
// statement timeout, the server shutting down or not yet accepting
// connections, and a full connection table.
func retryableSQLState(code string) bool {
	if len(code) == 5 && code[:2] == "08" && code != "08P01" {
		return true
	}
	switch code {
	case "40001", // serialization_failure
		"40P01", // deadlock_detected
		"40003", // statement_completion_unknown
		"55P03", // lock_not_available
		"57014", // query_canceled (statement_timeout)
		"57P01", // admin_shutdown
		"57P02", // crash_shutdown
		"57P03", // cannot_connect_now
		"53300": // too_many_connections
		return true
	}
	return false
}
