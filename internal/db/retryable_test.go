package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsRetryable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"cancelled", fmt.Errorf("write: %w", context.Canceled), false},
		{"deadline", fmt.Errorf("write: %w", context.DeadlineExceeded), true},
		{"bad conn", fmt.Errorf("exec: %w", driver.ErrBadConn), true},
		{"conn done", sql.ErrConnDone, true},
		{"network", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, true},
		{"connect", &pgconn.ConnectError{}, true},
		{"serialization failure", &pgconn.PgError{Code: "40001"}, true},
		{"deadlock", fmt.Errorf("commit: %w", &pgconn.PgError{Code: "40P01"}), true},
		{"connection exception class", &pgconn.PgError{Code: "08006"}, true},
		{"protocol violation", &pgconn.PgError{Code: "08P01"}, false},
		{"statement timeout", &pgconn.PgError{Code: "57014"}, true},
		{"admin shutdown", &pgconn.PgError{Code: "57P01"}, true},
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"rls refusal", &pgconn.PgError{Code: "42501"}, false},
		{"integrity violation at commit", &pgconn.PgError{Code: "40002"}, false},
		{"plain error", errors.New("artifact write refused"), false},
		{"transaction already finished", sql.ErrTxDone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.want {
				t.Fatalf("IsRetryable(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsRetryable_SQLiteBusy drives a real SQLITE_BUSY: one handle holds the
// write lock while a second, with no busy timeout to wait out, tries to write.
// That is the failure local mode meets when another process — an agent's exec
// verb with the local sandbox off — holds the file's write lock.
func TestIsRetryable_SQLiteBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	holder, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if _, err := holder.Exec(`CREATE TABLE t (v INTEGER)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ctx := context.Background()
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("take write lock: %v", err)
	}
	t.Cleanup(func() { _, _ = conn.ExecContext(ctx, `ROLLBACK`) })

	writer, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	_, err = writer.Exec(`INSERT INTO t (v) VALUES (1)`)
	if err == nil {
		t.Fatal("insert under another handle's write lock succeeded; want SQLITE_BUSY")
	}
	if !IsRetryable(err) {
		t.Fatalf("IsRetryable(%v) = false; want a busy database to be retryable", err)
	}
}
