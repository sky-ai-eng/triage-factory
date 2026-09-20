package workitem

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"
)

// TestInTxAttributesCancellation pins inTx's context attribution: a body that
// fails while its ctx is done reports both its own error and the context's,
// so a caller classifying by either stays correct.
func TestInTxAttributesCancellation(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)

	ctx, cancel := context.WithCancel(context.Background())
	body := errors.New("body failed")
	err = inTx(ctx, conn, func(*sql.Tx) error {
		cancel()
		return body
	})
	if !errors.Is(err, body) {
		t.Fatalf("err = %v, want the body's error", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation as its cause", err)
	}

	// A live ctx passes the body's error through untouched.
	err = inTx(context.Background(), conn, func(*sql.Tx) error { return body })
	if !errors.Is(err, body) || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the bare body error", err)
	}

	// And a clean body commits.
	if err := inTx(context.Background(), conn, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE t (x INTEGER)")
		return err
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
