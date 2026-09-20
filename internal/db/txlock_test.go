package db

import (
	"context"
	"database/sql"
	"path/filepath"
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
