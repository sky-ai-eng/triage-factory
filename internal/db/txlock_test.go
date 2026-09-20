package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOpenAt_ReadThenWriteWaitsForConcurrentWriter pins the lock mode the
// production DSN opens transactions with.
//
// A DEFERRED transaction that reads before it writes takes its locks in two
// steps, and SQLite refuses to run the busy handler on the second one: waiting
// on a lock upgrade can deadlock, so the upgrade fails immediately instead.
// busy_timeout never gets a say. Against a second process holding the write
// lock — an agent's `tfac exec` on macOS local mode, where the sandbox is off
// and every tool call opens the same file — that is an instant SQLITE_BUSY
// from a transaction that would have succeeded a moment later. Beginning
// IMMEDIATE takes the write lock up front, where the busy handler applies.
//
// The second handle here stands in for that other process: it is a distinct
// connection to the same file, which is all SQLite's file locking sees.
func TestOpenAt_ReadThenWriteWaitsForConcurrentWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "txlock.db")

	conn, err := OpenAt(path)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Exec(`CREATE TABLE q (id INTEGER PRIMARY KEY, claimed INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO q (id) VALUES (1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	other, err := OpenAt(path)
	if err != nil {
		t.Fatalf("OpenAt (second handle): %v", err)
	}
	t.Cleanup(func() { other.Close() })

	// Hold the write lock explicitly so the test does not depend on the
	// second handle's own begin mode.
	const hold = time.Second
	otherTx, err := other.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin on second handle: %v", err)
	}
	if _, err := otherTx.Exec(`INSERT INTO q (id) VALUES (2)`); err != nil {
		t.Fatalf("take write lock on second handle: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(hold)
		if err := otherTx.Commit(); err != nil {
			t.Errorf("commit on second handle: %v", err)
		}
	}()
	t.Cleanup(func() { <-released })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err = InTx(ctx, conn, func(tx *sql.Tx) error {
		var id int64
		if err := tx.QueryRow(`SELECT id FROM q WHERE claimed = 0 ORDER BY id LIMIT 1`).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE q SET claimed = 1 WHERE id = ?`, id)
		return err
	})
	waited := time.Since(start)

	if err != nil {
		t.Fatalf("read-then-write transaction failed after %v against a held write lock: %v", waited, err)
	}
	// A transaction that "succeeded" without waiting did not meet the lock
	// at all, which means the test is not exercising the contention it
	// claims to.
	if waited < hold/2 {
		t.Fatalf("transaction returned after %v; expected it to wait out the %v write lock", waited, hold)
	}

	var claimed int
	if err := conn.QueryRow(`SELECT claimed FROM q WHERE id = 1`).Scan(&claimed); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}
}

// TestInReadTx_SQLite_RefusesWrites pins the enforcement half of the read
// door: a write inside it fails, and the handle writes again afterwards,
// which is what proves the query_only guard was cleared before the pooled
// connection was handed back.
func TestInReadTx_SQLite_RefusesWrites(t *testing.T) {
	conn := openTxLockDB(t)

	err := InReadTx(context.Background(), conn, DialectSQLite, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO q (id) VALUES (2)`)
		return err
	})
	if err == nil {
		t.Fatal("write inside InReadTx committed; want a read-only refusal")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Fatalf("write inside InReadTx failed with %v; want SQLITE_READONLY", err)
	}
	assertRowCount(t, conn, 1)

	if _, err := conn.Exec(`INSERT INTO q (id) VALUES (2)`); err != nil {
		t.Fatalf("handle refuses writes after InReadTx returned: %v", err)
	}
	assertRowCount(t, conn, 2)
}

// TestInReadTx_SQLite_ClearsGuardAfterCancel is the failure mode the
// dedicated connection exists for: database/sql rolls a transaction back
// from a goroutine the moment its ctx is canceled, so a guard tied to the
// transaction would never get a chance to reset. The reset runs under a
// ctx that cannot be canceled, and the handle must write afterwards.
func TestInReadTx_SQLite_ClearsGuardAfterCancel(t *testing.T) {
	conn := openTxLockDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	err := InReadTx(ctx, conn, DialectSQLite, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM q`).Scan(&n); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if err == nil {
		t.Fatal("InReadTx returned nil after its ctx was canceled mid-body")
	}

	if _, err := conn.Exec(`INSERT INTO q (id) VALUES (2)`); err != nil {
		t.Fatalf("handle refuses writes after a canceled InReadTx: %v", err)
	}
	assertRowCount(t, conn, 2)
}

// TestInReadTx_SQLite_DoesNotWaitForConcurrentWriter is the other half of
// TestOpenAt_ReadThenWriteWaitsForConcurrentWriter. The handle's IMMEDIATE
// default makes an ordinary transaction wait out another process's write
// lock; a WAL reader never needed that lock, and the read door gives it a
// DEFERRED BEGIN so it does not queue behind the writer.
func TestInReadTx_SQLite_DoesNotWaitForConcurrentWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readtx.db")
	conn := openTxLockDBAt(t, path)

	other, err := OpenAt(path)
	if err != nil {
		t.Fatalf("OpenAt (second handle): %v", err)
	}
	t.Cleanup(func() { other.Close() })

	const hold = time.Second
	otherTx, err := other.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin on second handle: %v", err)
	}
	if _, err := otherTx.Exec(`INSERT INTO q (id) VALUES (2)`); err != nil {
		t.Fatalf("take write lock on second handle: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(hold)
		if err := otherTx.Commit(); err != nil {
			t.Errorf("commit on second handle: %v", err)
		}
	}()
	t.Cleanup(func() { <-released })

	start := time.Now()
	err = InReadTx(context.Background(), conn, DialectSQLite, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM q`).Scan(&n); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT id FROM q ORDER BY id LIMIT 1`).Scan(&n)
	})
	waited := time.Since(start)
	if err != nil {
		t.Fatalf("read-only transaction failed against a held write lock: %v", err)
	}
	if waited >= hold/2 {
		t.Fatalf("read-only transaction waited %v behind a %v write lock; a DEFERRED reader should not queue", waited, hold)
	}
}

func TestInReadTx_RefusesUnknownDialect(t *testing.T) {
	conn := openTxLockDB(t)
	err := InReadTx(context.Background(), conn, "mysql", func(*sql.Tx) error {
		t.Fatal("body ran under an unknown dialect")
		return nil
	})
	if err == nil {
		t.Fatal("InReadTx accepted an unknown dialect")
	}
}

// openTxLockDB opens the production handle on a fresh file with one seeded
// row in table q.
func openTxLockDB(t *testing.T) *sql.DB {
	t.Helper()
	return openTxLockDBAt(t, filepath.Join(t.TempDir(), "txlock.db"))
}

func openTxLockDBAt(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := OpenAt(path)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Exec(`CREATE TABLE q (id INTEGER PRIMARY KEY, claimed INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO q (id) VALUES (1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return conn
}

func assertRowCount(t *testing.T, conn *sql.DB, want int) {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM q`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != want {
		t.Fatalf("q has %d rows, want %d", n, want)
	}
}
