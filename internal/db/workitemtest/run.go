package workitemtest

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
)

// Run is the whole conformance suite. Every subtest builds its own world
// through mk, so a dialect's isolation story (a fresh in-memory database, a
// truncated container) is the factory's business and not the suite's.
func Run(t *testing.T, mk Factory) {
	t.Run("AdmitClaimComplete", func(t *testing.T) { testAdmitClaimComplete(t, mk) })
	t.Run("RequeueOutcomes", func(t *testing.T) { testRequeueOutcomes(t, mk) })
	t.Run("PermanentParksRegardlessOfBudget", func(t *testing.T) { testPermanentParks(t, mk) })
	t.Run("BudgetExhaustionParksAtClaim", func(t *testing.T) { testBudgetExhaustion(t, mk) })
	t.Run("MarkDoneIsFencedReplayOnly", func(t *testing.T) { testStrategyIsEnforced(t, mk) })
	t.Run("SameOwnerReclaimRejectsOldReceipt", func(t *testing.T) { testSameOwnerReclaim(t, mk) })
	t.Run("TakeoverRejectsStragglerWrites", func(t *testing.T) { testTakeover(t, mk) })
	t.Run("ExpiryWithNoSuccessorEndsAuthority", func(t *testing.T) { testExpiryNoSuccessor(t, mk) })
	t.Run("Renewal", func(t *testing.T) { testRenewal(t, mk) })
	t.Run("ExpiryWhileWaitingForRowLock", func(t *testing.T) { testExpiryUnderRowLock(t, mk) })
	t.Run("ExpiryInsideSingleTxClosure", func(t *testing.T) { testExpiryInsideClosure(t, mk) })
	t.Run("ExpiryInsideDeferPredicate", func(t *testing.T) { testExpiryInsideDeferPredicate(t, mk) })
	t.Run("SingleTxLockOrderingWithCancel", func(t *testing.T) { testLockOrderingWithCancel(t, mk) })
	t.Run("HolderObservesCancelAtNextOperation", func(t *testing.T) { testHolderObservesCancel(t, mk) })
	t.Run("CancelOfDeferredReadyItem", func(t *testing.T) { testCancelDeferred(t, mk) })
	t.Run("UniqueWhileUnsettled", func(t *testing.T) { testUniqueWhileUnsettled(t, mk) })
	t.Run("UniqueForever", func(t *testing.T) { testUniqueForever(t, mk) })
	t.Run("UniqueNone", func(t *testing.T) { testUniqueNone(t, mk) })
	t.Run("Defer", func(t *testing.T) { testDefer(t, mk) })
	t.Run("Backoff", func(t *testing.T) { testBackoff(t) })
	t.Run("Fairness", func(t *testing.T) { testFairness(t, mk) })
	t.Run("ClaimBatch", func(t *testing.T) { testClaimBatch(t, mk) })
	t.Run("ClaimRoundIsAllOrNothing", func(t *testing.T) { testClaimRoundIsAllOrNothing(t, mk) })
	t.Run("FrozenColumns", func(t *testing.T) { testFrozenColumns(t, mk) })
	t.Run("Measure", func(t *testing.T) { testMeasure(t, mk) })
	t.Run("IndexPresence", func(t *testing.T) { testIndexPresence(t, mk) })
}

func testAdmitClaimComplete(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 3, Lease: time.Minute}})
	id := e.admit("k1", "initial", 7)

	r := e.claimOne("worker-a", 9)
	if r.ItemID != id || r.OrgID != e.org {
		t.Fatalf("receipt addresses %d/%s, want %d/%s", r.ItemID, r.OrgID, id, e.org)
	}
	if r.Attempt != 1 || r.LeaseGeneration != 1 {
		t.Fatalf("receipt attempt=%d generation=%d, want 1/1", r.Attempt, r.LeaseGeneration)
	}
	if r.UniqueKey != "k1" {
		t.Fatalf("receipt unique key = %q, want k1", r.UniqueKey)
	}
	if r.LeaseExpiresAt.IsZero() {
		t.Fatal("receipt carries no lease expiry")
	}

	row := e.row(id)
	if got := asString(row["status"]); got != "leased" {
		t.Fatalf("status = %q, want leased", got)
	}
	if got := asString(row["lease_owner"]); got != "worker-a" {
		t.Fatalf("lease_owner = %q, want worker-a", got)
	}
	if got := asInt(row["lease_epoch"]); got != 9 {
		t.Fatalf("lease_epoch = %d, want 9", got)
	}

	if err := workitem.Complete(e.ctx, e.conn, e.kind, r, e.writePayload(id, "completed")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	row = e.row(id)
	if got := asString(row["status"]); got != "done" {
		t.Fatalf("status = %q, want done", got)
	}
	if got := asString(row["last_outcome"]); got != "done" {
		t.Fatalf("last_outcome = %q, want done", got)
	}
	if got := asString(row["payload"]); got != "completed" {
		t.Fatalf("payload = %q, want the closure's write", got)
	}
	if row["done_at"] == nil {
		t.Error("done_at is NULL on a completed row")
	}
	e.requireLeaseCleared(id)
}

func testRequeueOutcomes(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute}})
	for _, outcome := range []workitem.Outcome{
		workitem.OutcomeTransient, workitem.OutcomeDependencyDown,
		workitem.OutcomePoisonSuspected, workitem.OutcomeDeadline,
	} {
		id := e.admit(string(outcome), "p", 0)
		r := e.claimOne("worker-a", 1)
		cause := fmt.Errorf("upstream said no (%s)", outcome)
		if err := workitem.Requeue(e.ctx, e.conn, e.kind, r, outcome, cause); err != nil {
			t.Fatalf("Requeue %s: %v", outcome, err)
		}
		row := e.row(id)
		if got := asString(row["status"]); got != "ready" {
			t.Fatalf("%s: status = %q, want ready", outcome, got)
		}
		if got := asString(row["last_outcome"]); got != string(outcome) {
			t.Fatalf("%s: last_outcome = %q", outcome, got)
		}
		if got := asString(row["last_error"]); got != cause.Error() {
			t.Fatalf("%s: last_error = %q, want %q", outcome, got, cause.Error())
		}
		if got := asInt(row["attempt"]); got != 1 {
			t.Fatalf("%s: attempt = %d, want the charge kept at 1", outcome, got)
		}
		if row["next_attempt_at"] == nil {
			t.Fatalf("%s: next_attempt_at is NULL, want a backoff", outcome)
		}
		e.requireLeaseCleared(id)
	}

	// An outcome outside the vocabulary is refused rather than stored: the
	// typed value is what parking reasons and metrics key on.
	id := e.admit("bogus", "p", 0)
	r := e.claimOne("worker-a", 1)
	if err := workitem.Requeue(e.ctx, e.conn, e.kind, r, workitem.Outcome("whatever"), nil); err == nil {
		t.Error("Requeue accepted an unknown outcome")
	}
	e.requireStatus(id, "leased")
}

func testPermanentParks(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 9, Lease: time.Minute}})
	id := e.admit("perm", "p", 0)
	r := e.claimOne("worker-a", 1)
	if err := workitem.Requeue(e.ctx, e.conn, e.kind, r, workitem.OutcomePermanent, errors.New("rejected")); err != nil {
		t.Fatalf("Requeue permanent: %v", err)
	}
	row := e.row(id)
	if got := asString(row["status"]); got != "parked" {
		t.Fatalf("status = %q, want parked with 8 attempts left", got)
	}
	if got := asString(row["last_outcome"]); got != "permanent" {
		t.Fatalf("last_outcome = %q", got)
	}
	e.requireLeaseCleared(id)
}

func testBudgetExhaustion(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{
		unique: workitem.UniqueWhileUnsettled,
		policy: workitem.Policy{MaxAttempts: 2, Lease: shortLease, Backoff: fastBackoff},
	})

	// A requeue that leaves budget returns the row to ready; the claim that
	// spends the last attempt leases it normally. The park happens only when
	// that last attempt dies without a terminal write — at claim time, by the
	// next claimer, with no requeue involved.
	id := e.admit("budget", "p", 0)
	r := e.claimOne("worker-a", 1)
	if err := workitem.Requeue(e.ctx, e.conn, e.kind, r, workitem.OutcomeTransient, errors.New("blip")); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	e.requireStatus(id, "ready")

	time.Sleep(10 * time.Millisecond) // let the 1ms backoff ripen
	r2 := e.claimOne("worker-a", 1)
	if r2.Attempt != 2 {
		t.Fatalf("second claim charged attempt %d, want 2", r2.Attempt)
	}
	e.expireLease() // the holder dies mid-attempt

	res := e.claim(workitem.Owner{ID: "worker-b", Epoch: 1}, 1)
	if len(res.Claimed) != 0 || res.Parked != 1 {
		t.Fatalf("reclaim of a spent row: claimed=%d parked=%d, want 0/1", len(res.Claimed), res.Parked)
	}
	row := e.row(id)
	if got := asString(row["status"]); got != "parked" {
		t.Fatalf("status = %q, want parked", got)
	}
	if got := asString(row["last_outcome"]); got != "transient" {
		t.Fatalf("last_outcome = %q, want the failure that spent the budget", got)
	}
	e.requireLeaseCleared(id)

	// A row that never recorded an outcome at all parks under the fallback
	// reason, which is the only thing there is to say about it.
	e3 := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 1, Lease: shortLease}})
	id3 := e3.admit("never-reported", "p", 0)
	e3.claimOne("worker-a", 1)
	e3.expireLease()
	if res := e3.claim(workitem.Owner{ID: "worker-b", Epoch: 1}, 1); res.Parked != 1 {
		t.Fatalf("reclaim parked %d rows, want 1", res.Parked)
	}
	if got := asString(e3.row(id3)["last_outcome"]); got != "budget_exhausted" {
		t.Fatalf("last_outcome = %q, want budget_exhausted", got)
	}

	// A requeue arriving while the row is already at its budget parks there
	// instead, which is the other half of the same rule.
	e2 := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 1, Lease: time.Minute}})
	id2 := e2.admit("spent", "p", 0)
	r3 := e2.claimOne("worker-a", 1)
	if err := workitem.Requeue(e2.ctx, e2.conn, e2.kind, r3, workitem.OutcomeTransient, errors.New("blip")); err != nil {
		t.Fatalf("Requeue at budget: %v", err)
	}
	e2.requireStatus(id2, "parked")
}

func testStrategyIsEnforced(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, strategy: workitem.SingleTx, policy: workitem.Policy{Lease: time.Minute}})
	id := e.admit("strategy", "p", 0)
	r := e.claimOne("worker-a", 1)

	if err := workitem.MarkDone(e.ctx, e.conn, e.kind, r); err == nil {
		t.Error("MarkDone accepted a SingleTx kind")
	}
	fenced := e.withKind(func(k *workitem.Kind) { k.Strategy = workitem.FencedReplay })
	if err := workitem.Complete(fenced.ctx, fenced.conn, fenced.kind, r, func(*sql.Tx) error { return nil }); err == nil {
		t.Error("Complete accepted a FencedReplay kind")
	}
	e.requireStatus(id, "leased")

	if err := workitem.MarkDone(fenced.ctx, fenced.conn, fenced.kind, r); err != nil {
		t.Fatalf("MarkDone on a FencedReplay kind: %v", err)
	}
	e.requireStatus(id, "done")
	e.requireLeaseCleared(id)
}

func testSameOwnerReclaim(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: shortLease}})
	id := e.admit("same-owner", "p", 0)
	first := e.claimOne("worker-a", 3)
	e.expireLease()

	// Same process, same boot: the generation still moves, because the fence
	// is the acquisition and not the identity.
	second := e.claimOne("worker-a", 3)
	if second.LeaseGeneration != first.LeaseGeneration+1 {
		t.Fatalf("reclaim generation = %d, want %d", second.LeaseGeneration, first.LeaseGeneration+1)
	}
	if second.Attempt != first.Attempt+1 {
		t.Fatalf("reclaim attempt = %d, want %d", second.Attempt, first.Attempt+1)
	}
	e.assertStragglerLoses(first, "same-owner reclaim")
	e.requireStatus(id, "leased")
}

func testTakeover(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: shortLease}})
	e.admit("takeover", "p", 1)
	e.admit("bystander", "p", 2)

	straggler := e.claimOne("worker-a", 1)
	e.expireLease()
	successor := e.claimOne("worker-b", 1)
	if successor.ItemID != straggler.ItemID {
		t.Fatalf("successor claimed row %d, want the abandoned %d", successor.ItemID, straggler.ItemID)
	}
	e.assertStragglerLoses(straggler, "takeover by another owner")
}

func testExpiryNoSuccessor(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: shortLease}})
	e.admit("lapsed", "p", 1)
	r := e.claimOne("worker-a", 1)
	e.expireLease()
	// Nobody has taken over. Expiry alone ends authority.
	e.assertStragglerLoses(r, "expiry with no successor")
}

func testRenewal(t *testing.T, mk Factory) {
	const lease = 2 * time.Second
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: lease}})
	id := e.admit("renew", "p", 0)
	r := e.claimOne("worker-a", 1)

	// A second row, claimed straight after the renewal, is the yardstick: a
	// fresh acquisition's expiry IS database-now plus the policy lease, so a
	// renewal that computes the same thing must land beside it. Comparing
	// against it rather than against a window around the old expiry keeps the
	// assertion entirely on database time and tight enough that a wrong formula
	// has nowhere to hide.
	e.admit("yardstick", "p", 0)

	const elapsed = 400 * time.Millisecond
	time.Sleep(elapsed)
	renewed, err := workitem.RenewLease(e.ctx, e.conn, e.kind, r)
	if err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	fresh := e.claimOne("worker-b", 1)

	// Tolerance covers one claim's round trip, and is an order of magnitude
	// below what any wrong formula would show: old+Lease lands a whole `lease`
	// late, half-lease or no-move a whole `elapsed` early.
	const tolerance = 500 * time.Millisecond
	if drift := renewed.LeaseExpiresAt.Sub(fresh.LeaseExpiresAt); drift < -tolerance || drift > tolerance {
		t.Fatalf("renewed expiry is %s from a lease acquired moments later; want now+Lease, not old+Lease", drift)
	}
	if !renewed.LeaseExpiresAt.After(r.LeaseExpiresAt) {
		t.Fatalf("renewal left expiry at %s, no later than the original %s", renewed.LeaseExpiresAt, r.LeaseExpiresAt)
	}
	e.requireStatus(id, "leased")

	short := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: shortLease}})
	short.admit("late-renew", "p", 0)
	lr := short.claimOne("worker-a", 1)
	short.expireLease()
	if _, err := workitem.RenewLease(short.ctx, short.conn, short.kind, lr); !errors.Is(err, workitem.ErrLeaseLost) {
		t.Fatalf("late RenewLease = %v, want ErrLeaseLost", err)
	}
}

func testClaimBatch(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute}})
	var ids []int64
	for i := 0; i < 5; i++ {
		ids = append(ids, e.admit(fmt.Sprintf("batch-%d", i), "p", i))
	}
	for _, id := range ids[:2] {
		if err := workitem.RequestCancel(e.ctx, e.conn, e.kind, e.org, id, "operator", "not needed"); err != nil {
			t.Fatalf("RequestCancel %d: %v", id, err)
		}
	}

	res := e.claim(workitem.Owner{ID: "worker-a", Epoch: 1}, 3)
	if len(res.Claimed) != 3 {
		t.Fatalf("claimed %d receipts, want 3 — settled rows must not consume the batch", len(res.Claimed))
	}
	if res.Cancelled != 2 {
		t.Fatalf("settled %d cancellations, want 2", res.Cancelled)
	}
	for i, r := range res.Claimed {
		if r.ItemID != ids[i+2] {
			t.Fatalf("receipt %d addresses row %d, want %d", i, r.ItemID, ids[i+2])
		}
	}
	for _, id := range ids[:2] {
		e.requireStatus(id, "cancelled")
		e.requireLeaseCleared(id)
	}
}

// testClaimRoundIsAllOrNothing pins the boundary between a round and the result
// it reports. A round is one transaction; a row that fails partway through
// rolls its predecessors' leases back, and a receipt for a rolled-back lease is
// worse than no receipt — its holder would act on authority the database never
// granted, and every later operation would answer ErrLeaseLost.
func testClaimRoundIsAllOrNothing(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute}})
	cancelled := e.admit("settles", "p", 0)
	first := e.admit("leases-first", "p", 0)
	breaks := e.admit("breaks-the-round", "p", 0)
	untouched := e.admit("never-picked", "p", 0)
	if err := workitem.RequestCancel(e.ctx, e.conn, e.kind, e.org, cancelled, "operator", "not needed"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	// One live lease per owner: a constraint the round's SECOND lease violates.
	// It makes the mid-round failure deterministic on both dialects without a
	// trigger, and it fails on exactly the statement the package issues.
	e.exec("CREATE UNIQUE INDEX one_lease_per_owner ON " + e.kind.Table + " (lease_owner) WHERE status = 'leased'")

	before := e.table()
	res, err := workitem.Claim(e.ctx, e.conn, e.kind, workitem.Owner{ID: "worker-a", Epoch: 1}, e.org, 3)
	if err == nil {
		t.Fatal("Claim succeeded despite a constraint its second lease violates")
	}
	if len(res.Claimed) != 0 || res.Cancelled != 0 || res.Parked != 0 {
		t.Fatalf("a rolled-back round reported %d receipts, %d cancelled, %d parked; want nothing",
			len(res.Claimed), res.Cancelled, res.Parked)
	}
	if after := e.table(); !reflect.DeepEqual(before, after) {
		t.Errorf("a rolled-back round left changes behind\nbefore: %v\nafter:  %v", before, after)
	}
	for _, id := range []int64{cancelled, first, breaks, untouched} {
		e.requireStatus(id, "ready")
	}
}

func testFrozenColumns(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: shortLease}})
	id := e.admit("frozen", "p", 41)
	r := e.claimOne("worker-a", 1)
	if got := asInt(r.Frozen["frozen_col"]); got != 41 {
		t.Fatalf("frozen_col = %v, want 41", r.Frozen["frozen_col"])
	}

	e.exec("UPDATE "+e.kind.Table+" SET frozen_col = ? WHERE id = ?", 99, id)
	if got := asInt(r.Frozen["frozen_col"]); got != 41 {
		t.Fatalf("frozen_col changed to %v under a live receipt", r.Frozen["frozen_col"])
	}

	// The next acquisition is a new generation, so it freezes what the row
	// says now.
	e.expireLease()
	r2 := e.claimOne("worker-b", 1)
	if got := asInt(r2.Frozen["frozen_col"]); got != 99 {
		t.Fatalf("re-claimed frozen_col = %v, want 99", r2.Frozen["frozen_col"])
	}
}

func testMeasure(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute}})

	empty, err := workitem.Measure(e.ctx, e.conn, e.kind, e.org)
	if err != nil {
		t.Fatalf("Measure on an empty table: %v", err)
	}
	if empty != (workitem.Depths{}) {
		t.Fatalf("empty table measured %+v, want all zeros", empty)
	}

	ripe := e.admit("ripe", "p", 0)
	e.admit("leased", "p", 0)
	parked := e.admit("parked", "p", 0)
	deferredID := e.admit("deferred", "p", 0)

	leasedReceipt := e.claim(workitem.Owner{ID: "worker-a", Epoch: 1}, 4)
	// Claim took all four; put three of them back into the shapes we want to
	// measure and leave one leased.
	byID := map[int64]workitem.Receipt{}
	for _, r := range leasedReceipt.Claimed {
		byID[r.ItemID] = r
	}
	if err := workitem.Requeue(e.ctx, e.conn, e.kind, byID[ripe], workitem.OutcomeTransient, errors.New("x")); err != nil {
		t.Fatalf("requeue ripe: %v", err)
	}
	e.exec("UPDATE "+e.kind.Table+" SET next_attempt_at = NULL WHERE id = ?", ripe)
	if err := workitem.Park(e.ctx, e.conn, e.kind, byID[parked], "stuck"); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := workitem.Defer(e.ctx, e.conn, e.kind, byID[deferredID], "waiting", time.Now().Add(time.Hour), truePredicate); err != nil {
		t.Fatalf("defer: %v", err)
	}

	d, err := workitem.Measure(e.ctx, e.conn, e.kind, e.org)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if d.Ready != 1 || d.Leased != 1 || d.Parked != 1 || d.Deferred != 1 {
		t.Fatalf("depths = %+v, want one of each", d)
	}
	if d.OldestReadyAge <= 0 {
		t.Error("OldestReadyAge is zero with a ripe ready row")
	}
	if d.OldestDeferredAge <= 0 {
		t.Error("OldestDeferredAge is zero with a deferred row")
	}

	// An org with nothing in it measures zero, not the other org's rows.
	other, err := workitem.Measure(e.ctx, e.conn, e.kind, uuid.NewString())
	if err != nil {
		t.Fatalf("Measure other org: %v", err)
	}
	if other != (workitem.Depths{}) {
		t.Fatalf("empty org measured %+v", other)
	}

	// The unscoped read sees every org.
	all, err := workitem.Measure(e.ctx, e.conn, e.kind, "")
	if err != nil {
		t.Fatalf("Measure all orgs: %v", err)
	}
	if all.Ready != 1 || all.Leased != 1 || all.Parked != 1 || all.Deferred != 1 {
		t.Fatalf("unscoped depths = %+v", all)
	}
}

func testIndexPresence(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{Lease: time.Minute}})
	for _, spec := range []struct {
		table  string
		unique workitem.UniqueMode
	}{
		{FixtureTable, workitem.UniqueWhileUnsettled},
		{FixtureForeverTable, workitem.UniqueForever},
	} {
		k := workitem.Kind{Table: spec.table, Dialect: e.dialect, Unique: spec.unique, Strategy: workitem.SingleTx}
		want := workitem.IndexNames(k)
		if len(want) != 4 {
			t.Fatalf("%s: IndexNames returned %d names, want the three claim indexes plus uniqueness", spec.table, len(want))
		}
		have := e.indexNames(spec.table)
		for _, name := range want {
			if !have[name] {
				t.Errorf("%s: missing index %s (present: %v)", spec.table, name, have)
			}
		}
	}
}

// indexNames reads the indexes actually on a table, by whichever catalog the
// dialect keeps them in.
func (e *env) indexNames(table string) map[string]bool {
	e.t.Helper()
	query := "SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = ?"
	if e.dialect == workitem.Postgres {
		query = "SELECT indexname FROM pg_indexes WHERE tablename = ?"
	}
	rows, err := e.conn.QueryContext(e.ctx, e.q(query), table)
	if err != nil {
		e.t.Fatalf("read indexes: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			e.t.Fatalf("scan index name: %v", err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("read indexes: %v", err)
	}
	return out
}

// writePayload is the domain write a SingleTx closure performs, so a test can
// assert that fn's effects landed or rolled back with the disposition.
func (e *env) writePayload(id int64, value string) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		_, err := tx.ExecContext(e.ctx, e.q("UPDATE "+e.kind.Table+" SET payload = ? WHERE id = ?"), value, id)
		return err
	}
}

func truePredicate(*sql.Tx) (bool, error) { return true, nil }
