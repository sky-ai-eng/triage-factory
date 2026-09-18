package workitemtest

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
)

// assertStragglerLoses is the acceptance criterion made executable: a holder
// carrying a receipt it no longer owns cannot change any column of any row,
// under any operation. It compares the WHOLE table before and after, so a
// write that landed somewhere other than the row under test still fails.
func (e *env) assertStragglerLoses(stale workitem.Receipt, situation string) {
	e.t.Helper()
	before := e.table()

	fenced := e.withKind(func(k *workitem.Kind) { k.Strategy = workitem.FencedReplay })
	ops := []struct {
		name string
		run  func() error
	}{
		{"Complete", func() error {
			return workitem.Complete(e.ctx, e.conn, e.kind, stale, e.writePayload(stale.ItemID, "straggler"))
		}},
		{"MarkDone", func() error {
			return workitem.MarkDone(fenced.ctx, fenced.conn, fenced.kind, stale)
		}},
		{"Requeue", func() error {
			return workitem.Requeue(e.ctx, e.conn, e.kind, stale, workitem.OutcomeTransient, errors.New("straggler"))
		}},
		{"Park", func() error {
			return workitem.Park(e.ctx, e.conn, e.kind, stale, "straggler")
		}},
		{"Defer", func() error {
			return workitem.Defer(e.ctx, e.conn, e.kind, stale, "straggler", time.Now().Add(time.Hour), truePredicate)
		}},
		{"RenewLease", func() error {
			_, err := workitem.RenewLease(e.ctx, e.conn, e.kind, stale)
			return err
		}},
	}
	for _, op := range ops {
		if err := op.run(); !errors.Is(err, workitem.ErrLeaseLost) {
			e.t.Errorf("%s: %s with a stale receipt = %v, want ErrLeaseLost", situation, op.name, err)
		}
	}

	if after := e.table(); !reflect.DeepEqual(before, after) {
		e.t.Errorf("%s: the table changed under a stale receipt\nbefore: %v\nafter:  %v", situation, before, after)
	}
}

// testExpiryUnderRowLock is Postgres-only: it needs a second session holding
// the item row while the lease runs out underneath the blocked holder. SQLite
// has one writer, so there is no such wait to test.
func testExpiryUnderRowLock(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: shortLease}})
	if e.dialect != workitem.Postgres {
		t.Skip("row-lock contention is a Postgres shape; SQLite serializes writers")
	}
	id := e.admit("locked", "original", 0)
	r := e.claimOne("worker-a", 1)

	// A second session holds the row. The blocker runs in its own goroutine so
	// the lock is held across the wait below rather than across a hand-rolled
	// transaction this test would have to unwind itself.
	locked, release := make(chan struct{}), make(chan struct{})
	blocked := make(chan error, 1)
	go func() {
		blocked <- db.InTx(e.ctx, e.conn, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(e.ctx, e.q("SELECT id FROM "+e.kind.Table+" WHERE id = ? FOR UPDATE"), id); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-blocked:
		t.Fatalf("the blocking session never took the lock: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("timed out taking the blocking lock")
	}

	done := make(chan error, 1)
	go func() {
		done <- workitem.Complete(e.ctx, e.conn, e.kind, r, e.writePayload(id, "should not land"))
	}()

	// Complete is now waiting for the row lock. Let the lease run out while it
	// waits, then release: the guard is evaluated after the lock, on fresh
	// database time, so it must find the lease gone.
	e.expireLease()
	close(release)
	if err := <-blocked; err != nil {
		t.Fatalf("blocking session: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, workitem.ErrLeaseLost) {
			t.Fatalf("Complete after waiting out its own lease = %v, want ErrLeaseLost", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Complete never returned after the lock was released")
	}
	if got := asString(e.row(id)["payload"]); got != "original" {
		t.Fatalf("payload = %q, want the closure's write to have rolled back", got)
	}
	e.requireStatus(id, "leased")
}

// testExpiryInsideClosure puts the expiry inside the transaction itself: the
// closure's own writes move the lease into the past, so the terminal flip's
// re-evaluated guard misses and takes the domain writes down with it.
func testExpiryInsideClosure(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute}})
	id := e.admit("expire-inside", "original", 0)
	r := e.claimOne("worker-a", 1)

	err := workitem.Complete(e.ctx, e.conn, e.kind, r, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(e.ctx, e.q("UPDATE "+e.kind.Table+" SET payload = ? WHERE id = ?"), "domain", id); err != nil {
			return err
		}
		_, err := tx.ExecContext(e.ctx,
			e.q("UPDATE "+e.kind.Table+" SET lease_expires_at = "+e.pastExpr()+" WHERE id = ?"), id)
		return err
	})
	if !errors.Is(err, workitem.ErrLeaseLost) {
		t.Fatalf("Complete with a lease that lapsed inside the closure = %v, want ErrLeaseLost", err)
	}
	row := e.row(id)
	if got := asString(row["payload"]); got != "original" {
		t.Fatalf("payload = %q, want the closure rolled back", got)
	}
	if got := asString(row["status"]); got != "leased" {
		t.Fatalf("status = %q, want the lease-expiry write rolled back too", got)
	}
}

// testLockOrderingWithCancel pins both orders of the SingleTx cancellation
// race. The item row is locked first, so which side of that lock a request
// lands on decides the outcome — and both outcomes are correct.
func testLockOrderingWithCancel(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute}})

	// Before the lock: the domain writes never run.
	beforeID := e.admit("cancel-before", "original", 0)
	rb := e.claimOne("worker-a", 1)
	if err := workitem.RequestCancel(e.ctx, e.conn, e.kind, e.org, beforeID, "operator", "changed my mind"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	ran := false
	err := workitem.Complete(e.ctx, e.conn, e.kind, rb, func(tx *sql.Tx) error {
		ran = true
		return e.writePayload(beforeID, "domain")(tx)
	})
	if !errors.Is(err, workitem.ErrCancelled) {
		t.Fatalf("Complete over a pending cancellation = %v, want ErrCancelled", err)
	}
	if ran {
		t.Error("the closure ran despite a cancellation committed before the lock")
	}
	row := e.row(beforeID)
	if got := asString(row["status"]); got != "cancelled" {
		t.Fatalf("status = %q, want cancelled", got)
	}
	if got := asString(row["payload"]); got != "original" {
		t.Fatalf("payload = %q, want untouched", got)
	}
	if got := asString(row["cancel_requested_by"]); got != "operator" {
		t.Fatalf("cancel_requested_by = %q, want the terminal record to retain it", got)
	}
	if got := asString(row["cancel_reason"]); got != "changed my mind" {
		t.Fatalf("cancel_reason = %q, want the terminal record to retain it", got)
	}
	e.requireLeaseCleared(beforeID)

	// After the lock: the request waits behind the completion and cannot undo
	// it. Requesting from inside the closure IS "after the lock" — the
	// transaction already holds the row.
	afterID := e.admit("cancel-after", "original", 0)
	ra := e.claimOne("worker-a", 1)
	err = workitem.Complete(e.ctx, e.conn, e.kind, ra, func(tx *sql.Tx) error {
		if err := workitem.RequestCancel(e.ctx, tx, e.kind, e.org, afterID, "operator", "too late"); err != nil {
			return err
		}
		return e.writePayload(afterID, "domain")(tx)
	})
	if err != nil {
		t.Fatalf("Complete with a cancellation behind its lock = %v, want success", err)
	}
	row = e.row(afterID)
	if got := asString(row["status"]); got != "done" {
		t.Fatalf("status = %q, want done", got)
	}
	if got := asString(row["payload"]); got != "domain" {
		t.Fatalf("payload = %q, want the closure's write", got)
	}
	if row["cancel_requested_at"] == nil {
		t.Error("the late request was lost rather than recorded on the completed row")
	}

	// A request against completed work is not cancellable: nothing retroactive.
	if err := workitem.RequestCancel(e.ctx, e.conn, e.kind, e.org, afterID, "operator", "undo"); !errors.Is(err, workitem.ErrNotCancellable) {
		t.Fatalf("RequestCancel on a done row = %v, want ErrNotCancellable", err)
	}
	// A second request against an already-requested row is refused too, so the
	// first requester and reason are never overwritten.
	if err := workitem.RequestCancel(e.ctx, e.conn, e.kind, e.org, beforeID, "someone-else", "again"); !errors.Is(err, workitem.ErrNotCancellable) {
		t.Fatalf("RequestCancel on a settled row = %v, want ErrNotCancellable", err)
	}
}

// testHolderObservesCancel covers §1.7's other half: a request landing on a
// live lease is observed by the holder's next operation, renewal included.
func testHolderObservesCancel(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute}})

	for _, tc := range []struct {
		name string
		run  func(r workitem.Receipt) error
	}{
		{"RenewLease", func(r workitem.Receipt) error {
			_, err := workitem.RenewLease(e.ctx, e.conn, e.kind, r)
			return err
		}},
		{"Requeue", func(r workitem.Receipt) error {
			return workitem.Requeue(e.ctx, e.conn, e.kind, r, workitem.OutcomeTransient, errors.New("blip"))
		}},
		{"Park", func(r workitem.Receipt) error {
			return workitem.Park(e.ctx, e.conn, e.kind, r, "stuck")
		}},
		{"Defer", func(r workitem.Receipt) error {
			return workitem.Defer(e.ctx, e.conn, e.kind, r, "waiting", time.Now().Add(time.Hour), truePredicate)
		}},
		{"MarkDone", func(r workitem.Receipt) error {
			fenced := e.withKind(func(k *workitem.Kind) { k.Strategy = workitem.FencedReplay })
			return workitem.MarkDone(fenced.ctx, fenced.conn, fenced.kind, r)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := e.admit("observe-"+tc.name, "p", 0)
			r := e.claimOne("worker-a", 1)
			if err := workitem.RequestCancel(e.ctx, e.conn, e.kind, e.org, id, "operator", "stop"); err != nil {
				t.Fatalf("RequestCancel: %v", err)
			}
			if err := tc.run(r); !errors.Is(err, workitem.ErrCancelled) {
				t.Fatalf("%s over a pending cancellation = %v, want ErrCancelled", tc.name, err)
			}
			row := e.row(id)
			if got := asString(row["status"]); got != "cancelled" {
				t.Fatalf("status = %q, want cancelled", got)
			}
			if got := asString(row["last_outcome"]); got != "cancelled" {
				t.Fatalf("last_outcome = %q, want cancelled", got)
			}
			if row["done_at"] == nil {
				t.Error("done_at is NULL on a cancelled row")
			}
			e.requireLeaseCleared(id)
		})
	}
}

// testCancelDeferred is the reason Claim's selection has a second arm: a
// deferred ready row has no holder to observe a request, so without it the row
// would sit on its unique key until its retry time came round.
func testCancelDeferred(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute}})
	id := e.admit("deferred-cancel", "p", 0)
	r := e.claimOne("worker-a", 1)
	if err := workitem.Defer(e.ctx, e.conn, e.kind, r, "waiting on a prerequisite", time.Now().Add(time.Hour), truePredicate); err != nil {
		t.Fatalf("Defer: %v", err)
	}
	e.requireStatus(id, "ready")

	if err := workitem.RequestCancel(e.ctx, e.conn, e.kind, e.org, id, "operator", "no longer needed"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	res := e.claim(workitem.Owner{ID: "worker-b", Epoch: 1}, 1)
	if len(res.Claimed) != 0 || res.Cancelled != 1 {
		t.Fatalf("claim over a cancelled deferred row: claimed=%d cancelled=%d, want 0/1", len(res.Claimed), res.Cancelled)
	}
	e.requireStatus(id, "cancelled")

	// Settling frees the key, an hour before the retry time it was carrying.
	newID, dup, err := workitem.Admit(e.ctx, e.conn, e.kind, e.org, "deferred-cancel", map[string]any{"payload": "again", "frozen_col": 0})
	if err != nil {
		t.Fatalf("re-admit: %v", err)
	}
	if dup {
		t.Fatalf("re-admit deduplicated against the cancelled row %d", newID)
	}
	if newID == id {
		t.Fatal("re-admit returned the cancelled row's id")
	}
}

// testUniqueWhileUnsettled walks a key through the states that hold it and the
// two operator controls that release it.
func testUniqueWhileUnsettled(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute}})
	id := e.admit("held", "p", 0)
	replacement := e.admit("replacement", "p", 0)

	r := e.claimOne("worker-a", 1)
	if err := workitem.Park(e.ctx, e.conn, e.kind, r, "needs a human"); err != nil {
		t.Fatalf("Park: %v", err)
	}
	e.requireStatus(id, "parked")

	// Parked work keeps its key, so a replacement cannot be admitted behind
	// its back.
	gotID, dup := e.admitDup("held")
	if !dup || gotID != id {
		t.Fatalf("admit against a parked key: id=%d dup=%v, want %d/true", gotID, dup, id)
	}

	if err := workitem.Redrive(e.ctx, e.conn, e.kind, e.org, id, "operator"); err != nil {
		t.Fatalf("Redrive: %v", err)
	}
	row := e.row(id)
	if got := asString(row["status"]); got != "ready" {
		t.Fatalf("status = %q, want ready", got)
	}
	if got := asInt(row["attempt"]); got != 0 {
		t.Fatalf("attempt = %d, want a fresh budget", got)
	}
	if got := asString(row["last_outcome"]); got != "redriven" {
		t.Fatalf("last_outcome = %q", got)
	}
	if row["last_error"] != nil {
		t.Errorf("last_error = %v, want cleared", row["last_error"])
	}
	if row["done_at"] != nil {
		t.Errorf("done_at = %v, want cleared on a row returned to ready", row["done_at"])
	}
	if got := asInt(row["lease_generation"]); got != r.LeaseGeneration+1 {
		t.Fatalf("lease_generation = %d, want %d — a redrive must kill the pre-park receipt", got, r.LeaseGeneration+1)
	}
	if e.row(id)["first_enqueued_at"] == nil {
		t.Error("first_enqueued_at was cleared by the redrive")
	}
	gotID, dup = e.admitDup("held")
	if !dup || gotID != id {
		t.Fatalf("admit against a redriven key: id=%d dup=%v, want %d/true", gotID, dup, id)
	}

	// Park it again and supersede: the key is released for replacement work,
	// and the parked row keeps its history.
	r2 := e.claimOne("worker-a", 1)
	if err := workitem.Park(e.ctx, e.conn, e.kind, r2, "still stuck"); err != nil {
		t.Fatalf("Park again: %v", err)
	}
	if err := workitem.Supersede(e.ctx, e.conn, e.kind, e.org, id, "operator", replacement); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	row = e.row(id)
	if got := asString(row["status"]); got != "cancelled" {
		t.Fatalf("status = %q, want cancelled", got)
	}
	if got := asInt(row["superseded_by"]); got != replacement {
		t.Fatalf("superseded_by = %d, want %d", got, replacement)
	}
	if got := asString(row["last_outcome"]); got != "superseded" {
		t.Fatalf("last_outcome = %q", got)
	}
	if got := asString(row["cancel_requested_by"]); got != "operator" {
		t.Fatalf("cancel_requested_by = %q", got)
	}
	freshID, dup := e.admitDup("held")
	if dup || freshID == id {
		t.Fatalf("admit after supersede: id=%d dup=%v, want a new row", freshID, dup)
	}

	// A parked row carrying a cancellation request is not redrivable — the
	// answer to "stop this" is supersede, not another run.
	//
	// The column is staged directly because the package's own API cannot reach
	// that state: RequestCancel refuses a parked row, and every path that could
	// park a requested one settles it as cancelled first. The clause is a guard
	// against a row written by hand or by a future adopter, so the test has to
	// write one by hand too.
	r3 := e.claimOne("worker-a", 1)
	blocked := r3.ItemID
	if err := workitem.Park(e.ctx, e.conn, e.kind, r3, "stuck"); err != nil {
		t.Fatalf("Park for the redrive refusal: %v", err)
	}
	e.exec("UPDATE "+e.kind.Table+" SET cancel_requested_at = "+e.nowSQL()+", cancel_requested_by = 'operator', cancel_reason = 'stop' WHERE id = ?", blocked)
	if err := workitem.Redrive(e.ctx, e.conn, e.kind, e.org, blocked, "operator"); !errors.Is(err, workitem.ErrNotParked) {
		t.Fatalf("Redrive of a cancel-requested parked row = %v, want ErrNotParked", err)
	}

	// Neither control reaches a row that is not parked.
	if err := workitem.Redrive(e.ctx, e.conn, e.kind, e.org, freshID, "operator"); !errors.Is(err, workitem.ErrNotParked) {
		t.Fatalf("Redrive of a ready row = %v, want ErrNotParked", err)
	}
	if err := workitem.Supersede(e.ctx, e.conn, e.kind, e.org, freshID, "operator", replacement); !errors.Is(err, workitem.ErrNotParked) {
		t.Fatalf("Supersede of a ready row = %v, want ErrNotParked", err)
	}
}

// testUniqueForever proves the mode's whole difference: a settled row still
// owns its key.
func testUniqueForever(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{
		table: FixtureForeverTable, unique: workitem.UniqueForever,
		policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute},
	})
	id := e.admit("forever", "p", 0)
	r := e.claimOne("worker-a", 1)
	if err := workitem.Complete(e.ctx, e.conn, e.kind, r, e.writePayload(id, "done")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	e.requireStatus(id, "done")

	gotID, dup := e.admitDup("forever")
	if !dup || gotID != id {
		t.Fatalf("admit against a done key: id=%d dup=%v, want %d/true", gotID, dup, id)
	}
}

// testUniqueNone covers the mode with no index: every admission inserts, and a
// key is refused rather than stored somewhere it would dedup nothing.
func testUniqueNone(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueNone, policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute}})
	first := e.admit("", "p", 0)
	second := e.admit("", "p", 0)
	if first == second {
		t.Fatal("two unkeyed admissions returned the same row")
	}
	if _, _, err := workitem.Admit(e.ctx, e.conn, e.kind, e.org, "a-key", map[string]any{"payload": "p"}); err == nil {
		t.Error("Admit accepted a unique key for a kind that dedups nothing")
	}
	// A column the kind never declared is an error, not an interpolation.
	if _, _, err := workitem.Admit(e.ctx, e.conn, e.kind, e.org, "", map[string]any{"status": "done"}); err == nil {
		t.Error("Admit accepted an undeclared column")
	}
}

// testDefer covers the refund's whole contract: once per acquisition, never on
// a stale receipt, never on a refused predicate, and never a way to run out of
// budget by waiting.
func testDefer(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{
		unique: workitem.UniqueWhileUnsettled,
		policy: workitem.Policy{MaxAttempts: 2, Lease: time.Minute, Backoff: fastBackoff},
	})

	// The refund is once per acquisition: the write that spends the receipt
	// also drops the row out of 'leased', so the same receipt cannot refund
	// twice.
	id := e.admit("refund", "p", 0)
	r := e.claimOne("worker-a", 1)
	past := time.Now().Add(-time.Second)
	if err := workitem.Defer(e.ctx, e.conn, e.kind, r, "waiting", past, truePredicate); err != nil {
		t.Fatalf("Defer: %v", err)
	}
	row := e.row(id)
	if got := asInt(row["attempt"]); got != 0 {
		t.Fatalf("attempt = %d, want the charge refunded", got)
	}
	if got := asString(row["last_outcome"]); got != "deferred" {
		t.Fatalf("last_outcome = %q", got)
	}
	e.requireLeaseCleared(id)
	if err := workitem.Defer(e.ctx, e.conn, e.kind, r, "waiting", past, truePredicate); !errors.Is(err, workitem.ErrLeaseLost) {
		t.Fatalf("second Defer on one receipt = %v, want ErrLeaseLost", err)
	}
	if got := asInt(e.row(id)["attempt"]); got != 0 {
		t.Fatalf("attempt = %d after a refused second refund, want 0", got)
	}

	// A false predicate is not a failure and not a refund. The row stays
	// leased and the holder still owes it a disposition.
	r2 := e.claimOne("worker-a", 1)
	beforeExpiry := e.row(id)["lease_expires_at"]
	err := workitem.Defer(e.ctx, e.conn, e.kind, r2, "waiting", past, func(*sql.Tx) (bool, error) { return false, nil })
	if !errors.Is(err, workitem.ErrDeferRefused) {
		t.Fatalf("Defer with a false predicate = %v, want ErrDeferRefused", err)
	}
	row = e.row(id)
	if got := asString(row["status"]); got != "leased" {
		t.Fatalf("status = %q, want the row still leased", got)
	}
	if got := asInt(row["attempt"]); got != 1 {
		t.Fatalf("attempt = %d, want no refund", got)
	}
	if row["lease_expires_at"] != beforeExpiry {
		t.Errorf("lease_expires_at moved on a refused deferral: %v -> %v", beforeExpiry, row["lease_expires_at"])
	}

	// A predicate error is the caller's, and rolls back with it.
	boom := errors.New("predicate exploded")
	err = workitem.Defer(e.ctx, e.conn, e.kind, r2, "waiting", past, func(*sql.Tx) (bool, error) { return false, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("Defer with a failing predicate = %v, want the predicate's error", err)
	}
	e.requireStatus(id, "leased")

	// A real failure after a deferral charges normally.
	if err := workitem.Requeue(e.ctx, e.conn, e.kind, r2, workitem.OutcomeTransient, errors.New("blip")); err != nil {
		t.Fatalf("Requeue after deferral: %v", err)
	}
	if got := asInt(e.row(id)["attempt"]); got != 1 {
		t.Fatalf("attempt = %d after a real failure, want the charge kept", got)
	}

	// Healthy waiting, repeated well past the budget, never parks the row.
	waitID := e.admit("patient", "p", 0)
	for i := 0; i < 6; i++ {
		wr := e.claim(workitem.Owner{ID: "worker-a", Epoch: 1}, 5)
		var found bool
		for _, rr := range wr.Claimed {
			if rr.ItemID != waitID {
				continue
			}
			found = true
			if err := workitem.Defer(e.ctx, e.conn, e.kind, rr, "still waiting", past, truePredicate); err != nil {
				t.Fatalf("Defer round %d: %v", i, err)
			}
		}
		if !found {
			t.Fatalf("round %d: the patient row was not claimable (status %q)", i, asString(e.row(waitID)["status"]))
		}
		if got := asString(e.row(waitID)["status"]); got != "ready" {
			t.Fatalf("round %d: status = %q, want ready", i, got)
		}
		if got := asInt(e.row(waitID)["attempt"]); got != 0 {
			t.Fatalf("round %d: attempt = %d, want 0", i, got)
		}
	}
}

func testBackoff(t *testing.T) {
	spec := workitem.BackoffSpec{Base: time.Second, Cap: 10 * time.Second, Jitter: 0.25}
	mid := func() float64 { return 0.5 } // the band's midpoint is the pre-jitter delay

	var prev time.Duration
	for attempt := 1; attempt <= 10; attempt++ {
		got := workitem.Backoff(spec, attempt, mid)
		if got < prev {
			t.Fatalf("attempt %d: %s is shorter than attempt %d's %s", attempt, got, attempt-1, prev)
		}
		if got > spec.Cap {
			t.Fatalf("attempt %d: %s exceeds the %s cap", attempt, got, spec.Cap)
		}
		prev = got
	}
	if got := workitem.Backoff(spec, 1, mid); got != time.Second {
		t.Fatalf("first attempt = %s, want the base", got)
	}
	if got := workitem.Backoff(spec, 3, mid); got != 4*time.Second {
		t.Fatalf("third attempt = %s, want base*4", got)
	}
	if got := workitem.Backoff(spec, 20, mid); got != spec.Cap {
		t.Fatalf("a far attempt = %s, want the cap", got)
	}
	// An attempt below the first is clamped rather than producing a fraction
	// of the base.
	if got := workitem.Backoff(spec, 0, mid); got != time.Second {
		t.Fatalf("attempt 0 = %s, want the base", got)
	}

	lo := workitem.Backoff(spec, 4, func() float64 { return 0 })
	hi := workitem.Backoff(spec, 4, func() float64 { return 0.999999 })
	base := workitem.Backoff(spec, 4, mid)
	if lo < time.Duration(float64(base)*0.75)-time.Millisecond || lo > base {
		t.Fatalf("jitter floor %s is outside [0.75, 1] x %s", lo, base)
	}
	if hi < base || hi > time.Duration(float64(base)*1.25)+time.Millisecond {
		t.Fatalf("jitter ceiling %s is outside [1, 1.25] x %s", hi, base)
	}

	// A zero spec is every default rather than a zero delay.
	if got := workitem.Backoff(workitem.BackoffSpec{}, 1, mid); got != 5*time.Second {
		t.Fatalf("zero spec first attempt = %s, want the 5s default base", got)
	}
}

// testFairness pins the claim ordering both ways round: interleaved by an
// org's live load, and strict FIFO when fairness is off.
func testFairness(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{
		unique: workitem.UniqueWhileUnsettled,
		policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute, Fairness: true},
	})
	busy, middling, idle := e.org, uuid.NewString(), uuid.NewString()

	// Two live leases for busy, one for middling, none for idle.
	e.admitIn(busy, "busy-live-1", "p", 0)
	e.admitIn(busy, "busy-live-2", "p", 0)
	e.admitIn(middling, "mid-live-1", "p", 0)
	if got := len(e.claimIn(workitem.Owner{ID: "pre", Epoch: 1}, "", 3).Claimed); got != 3 {
		t.Fatalf("seeded %d leases, want 3", got)
	}

	// One waiting row each, admitted busiest-first so id order and fairness
	// order disagree.
	busyID := e.admitIn(busy, "busy-wait", "p", 0)
	midID := e.admitIn(middling, "mid-wait", "p", 0)
	idleID := e.admitIn(idle, "idle-wait", "p", 0)

	res := e.claimIn(workitem.Owner{ID: "worker", Epoch: 1}, "", 3)
	if len(res.Claimed) != 3 {
		t.Fatalf("claimed %d, want 3", len(res.Claimed))
	}
	want := []int64{idleID, midID, busyID}
	for i, r := range res.Claimed {
		if r.ItemID != want[i] {
			var got []int64
			for _, c := range res.Claimed {
				got = append(got, c.ItemID)
			}
			t.Fatalf("fairness order %v, want %v (fewest live leases first)", got, want)
		}
	}

	// Fairness off is strict id order, whatever the orgs are doing.
	f := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute}})
	a, b := f.org, uuid.NewString()
	f.admitIn(a, "fifo-1", "p", 0)
	f.admitIn(a, "fifo-2", "p", 0)
	if got := len(f.claimIn(workitem.Owner{ID: "pre", Epoch: 1}, "", 2).Claimed); got != 2 {
		t.Fatalf("seeded %d leases, want 2", got)
	}
	first := f.admitIn(a, "fifo-3", "p", 0)
	second := f.admitIn(b, "fifo-4", "p", 0)
	res = f.claimIn(workitem.Owner{ID: "worker", Epoch: 1}, "", 2)
	if len(res.Claimed) != 2 || res.Claimed[0].ItemID != first || res.Claimed[1].ItemID != second {
		t.Fatalf("FIFO claim returned %v, want %d then %d", res.Claimed, first, second)
	}
}

// admitDup admits a key expecting the uniqueness mode to have an opinion.
func (e *env) admitDup(key string) (int64, bool) {
	e.t.Helper()
	id, dup, err := workitem.Admit(e.ctx, e.conn, e.kind, e.org, key, map[string]any{"payload": "again", "frozen_col": 0})
	if err != nil {
		e.t.Fatalf("admit %q: %v", key, err)
	}
	return id, dup
}

// nowSQL is database time in the dialect's spelling, for the few assertions
// that stage a column the package would otherwise own.
func (e *env) nowSQL() string {
	if e.dialect == workitem.Postgres {
		return "clock_timestamp()"
	}
	return `strftime('%Y-%m-%d %H:%M:%f','now')`
}
