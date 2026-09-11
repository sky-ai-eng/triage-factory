package pgtest

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestWatchContention_NamesTheSessionAStatementWaitsOn is the diagnosis a
// Reset that loses a lock is supposed to hand its reader. The sample has to be
// taken while the statement is still waiting: by the time the error surfaces,
// the session that beat it has usually committed and gone idle, which is how a
// leaked goroutine stayed invisible behind a bare "deadlock detected".
func TestWatchContention_NamesTheSessionAStatementWaitsOn(t *testing.T) {
	h := Shared(t)
	h.Reset(t)
	ctx := context.Background()

	// The session a later statement loses to: a transaction left open holding
	// a lock — what an unjoined goroutine looks like from the server's side.
	blocker, err := h.AdminDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback() }()
	if _, err := blocker.ExecContext(ctx, `LOCK TABLE users IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("blocker takes the lock: %v", err)
	}
	// The statement pg_stat_activity will report for it, chosen so the
	// rendering can be checked for the blocker specifically — both sessions
	// here run the same LOCK otherwise.
	if _, err := blocker.ExecContext(ctx, `SELECT 'the blocker still holds it'`); err != nil {
		t.Fatalf("blocker marker statement: %v", err)
	}

	stop := watchContention(h.AdminDB)
	waiter, err := h.AdminDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin waiter: %v", err)
	}
	lockCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := waiter.ExecContext(lockCtx, `LOCK TABLE users IN ACCESS EXCLUSIVE MODE`); err == nil {
		t.Fatal("the second lock was granted; the blocker is not holding what this test needs it to")
	}
	_ = waiter.Rollback()
	sampled := stop()

	if len(sampled) == 0 {
		t.Fatal("nothing was sampled while a statement sat waiting on a lock; a Reset failure would name nobody")
	}
	rendered := describeBusyBackends(sampled)
	if !strings.Contains(rendered, "the blocker still holds it") {
		t.Errorf("the sample does not name the session holding the lock or its statement:%s", rendered)
	}
	var sawOpenTransaction, sawWaiter bool
	for _, b := range sampled {
		if b.state == "idle in transaction" {
			sawOpenTransaction = true
		}
		if strings.HasPrefix(b.waitEvent, "Lock") {
			sawWaiter = true
		}
	}
	if !sawOpenTransaction {
		t.Errorf("no sampled session reads as idle in transaction:%s", rendered)
	}
	if !sawWaiter {
		t.Errorf("no sampled session reads as waiting on a lock:%s", rendered)
	}

	// And the other direction: once both transactions are done, the harness
	// reports nothing. An assertion that fired on ordinary pooled connections
	// would be worthless. The wait is for the cancelled LOCK above, which this
	// test creates on purpose and a fixture's cleanup does not.
	if err := blocker.Rollback(); err != nil {
		t.Fatalf("rollback blocker: %v", err)
	}
	var quiet []busyBackend
	for deadline := time.Now().Add(10 * time.Second); ; {
		quiet, err = busyBackends(ctx, h.AdminDB)
		if err != nil {
			t.Fatalf("busyBackends on a quiet harness: %v", err)
		}
		if len(quiet) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(quiet) != 0 {
		t.Errorf("a harness with nothing running reports %d busy session(s):%s", len(quiet), describeBusyBackends(quiet))
	}
}
