package workitemtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
)

// Run is the whole conformance suite over the fixture tables. Every subtest
// builds its own world through mk, so a dialect's isolation story (a fresh
// in-memory database, a truncated container) is the factory's business and
// not the suite's.
//
// The subtests that read and write only the shared block are the same
// functions RunTable runs over a production table; the rest write the
// fixture's payload, freeze its frozen_col, need a SingleTx closure with a
// domain write, or need several orgs, and so exist here alone.
func Run(t *testing.T, mk Factory) {
	fixture := func(t *testing.T, policy workitem.Policy) *env {
		return setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: policy})
	}
	runShared(t, fixture)
	t.Run("ExpiryWhileWaitingForRowLock", func(t *testing.T) { testExpiryUnderRowLock(t, mk) })
	t.Run("ExpiryInsideSingleTxClosure", func(t *testing.T) { testExpiryInsideClosure(t, mk) })
	t.Run("ExpiryInsideDeferPredicate", func(t *testing.T) { testExpiryInsideDeferPredicate(t, mk) })
	t.Run("SingleTxLockOrderingWithCancel", func(t *testing.T) { testLockOrderingWithCancel(t, mk) })
	t.Run("AdmitUnderRowLock", func(t *testing.T) { testAdmitUnderRowLock(t, mk) })
	t.Run("UniqueForever", func(t *testing.T) { testUniqueForever(t, mk) })
	t.Run("UniqueNone", func(t *testing.T) { testUniqueNone(t, mk) })
	t.Run("Backoff", func(t *testing.T) { testBackoff(t) })
	t.Run("Fairness", func(t *testing.T) { testFairness(t, mk) })
	t.Run("MeasureByOrgAcrossOrgs", func(t *testing.T) { testMeasureByOrgAcrossOrgs(t, mk) })
	t.Run("ClaimAcrossOrgsReportsPerOrg", func(t *testing.T) { testClaimAcrossOrgsReportsPerOrg(t, mk) })
	t.Run("FrozenColumns", func(t *testing.T) { testFrozenColumns(t, mk) })
	t.Run("ClaimFilter", func(t *testing.T) { testClaimFilter(t, mk) })
	t.Run("FixtureIndexPresence", func(t *testing.T) { testFixtureIndexPresence(t, mk) })
}

// RunTable is the table-agnostic half of the suite, run against an adopting
// table as production declares it: its Kind's strategy and uniqueness mode
// decide which completion verb and which uniqueness subtest run, and its own
// columns come from the factory's hook. Timing is the suite's: every subtest
// copies the Kind and swaps the policy for its own short lease and fast
// backoff, exactly as the fixture subtests do, so production timing never
// enters a test.
func RunTable(t *testing.T, mk TableFactory) {
	runShared(t, func(t *testing.T, policy workitem.Policy) *env {
		return setupTable(t, mk, policy)
	})
}

// runShared is the list both entry points run.
func runShared(t *testing.T, mk envFactory) {
	t.Run("AdmitClaimComplete", func(t *testing.T) { testAdmitClaimComplete(t, mk) })
	t.Run("RequeueOutcomes", func(t *testing.T) { testRequeueOutcomes(t, mk) })
	t.Run("PermanentParksRegardlessOfBudget", func(t *testing.T) { testPermanentParks(t, mk) })
	t.Run("BudgetExhaustionParksAtClaim", func(t *testing.T) { testBudgetExhaustion(t, mk) })
	t.Run("MarkDoneIsFencedReplayOnly", func(t *testing.T) { testStrategyIsEnforced(t, mk) })
	t.Run("SameOwnerReclaimRejectsOldReceipt", func(t *testing.T) { testSameOwnerReclaim(t, mk) })
	t.Run("TakeoverRejectsStragglerWrites", func(t *testing.T) { testTakeover(t, mk) })
	t.Run("ExpiryWithNoSuccessorEndsAuthority", func(t *testing.T) { testExpiryNoSuccessor(t, mk) })
	t.Run("Renewal", func(t *testing.T) { testRenewal(t, mk) })
	t.Run("HolderObservesCancelAtNextOperation", func(t *testing.T) { testHolderObservesCancel(t, mk) })
	t.Run("CancelOfDeferredReadyItem", func(t *testing.T) { testCancelDeferred(t, mk) })
	t.Run("Uniqueness", func(t *testing.T) { testUniquenessByMode(t, mk) })
	t.Run("Defer", func(t *testing.T) { testDefer(t, mk) })
	t.Run("ClaimBatch", func(t *testing.T) { testClaimBatch(t, mk) })
	t.Run("ClaimRoundIsAllOrNothing", func(t *testing.T) { testClaimRoundIsAllOrNothing(t, mk) })
	t.Run("Measure", func(t *testing.T) { testMeasure(t, mk) })
	t.Run("MeasureByOrg", func(t *testing.T) { testMeasureByOrg(t, mk) })
	t.Run("ListAndGet", func(t *testing.T) { testListAndGet(t, mk) })
	t.Run("NilObserver", func(t *testing.T) { testNilObserver(t, mk) })
	t.Run("IndexPresence", func(t *testing.T) { testIndexPresence(t, mk) })
}

// complete finishes a leased row through the kind's own strategy: Complete
// with a no-op closure for SingleTx, MarkDone for FencedReplay.
func (e *env) complete(r workitem.Receipt) error {
	if e.kind.Strategy == workitem.FencedReplay {
		return workitem.MarkDone(e.ctx, e.conn, e.kind, r)
	}
	return workitem.Complete(e.ctx, e.conn, e.kind, r, func(*sql.Tx) error { return nil })
}

func testAdmitClaimComplete(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{MaxAttempts: 3, Lease: time.Minute})
	id := e.admit("k1")

	res := e.claim(workitem.Owner{ID: "worker-a", Epoch: 9}, 1)
	if len(res.Claimed) != 1 {
		t.Fatalf("claimed %d receipts, want 1", len(res.Claimed))
	}
	r := res.Claimed[0]
	if r.ItemID != id || r.OrgID != e.org {
		t.Fatalf("receipt addresses %d/%s, want %d/%s", r.ItemID, r.OrgID, id, e.org)
	}
	if r.Attempt != 1 || r.LeaseGeneration != 1 {
		t.Fatalf("receipt attempt=%d generation=%d, want 1/1", r.Attempt, r.LeaseGeneration)
	}
	if r.UniqueKey != e.key("k1") {
		t.Fatalf("receipt unique key = %q, want %q", r.UniqueKey, e.key("k1"))
	}
	if r.LeaseExpiresAt.IsZero() {
		t.Fatal("receipt carries no lease expiry")
	}
	// A first acquisition of a ready row is not a reclaim, on the receipt or
	// in the count.
	if r.Reclaimed || r.PreviousOwner != "" || res.Reclaimed != 0 {
		t.Fatalf("fresh claim reported a reclaim: receipt=%v/%q result=%d", r.Reclaimed, r.PreviousOwner, res.Reclaimed)
	}

	row := e.row(id)
	if got := asString(row["status"]); got != workitem.StatusLeased {
		t.Fatalf("status = %q, want leased", got)
	}
	if got := asString(row["lease_owner"]); got != "worker-a" {
		t.Fatalf("lease_owner = %q, want worker-a", got)
	}
	if got := asInt(row["lease_epoch"]); got != 9 {
		t.Fatalf("lease_epoch = %d, want 9", got)
	}
	e.requireObserved("claimed 1/0/0/0")

	if err := e.complete(r); err != nil {
		t.Fatalf("complete: %v", err)
	}
	row = e.row(id)
	if got := asString(row["status"]); got != workitem.StatusDone {
		t.Fatalf("status = %q, want done", got)
	}
	if got := asString(row["last_outcome"]); got != "done" {
		t.Fatalf("last_outcome = %q, want done", got)
	}
	if row["done_at"] == nil {
		t.Error("done_at is NULL on a completed row")
	}
	e.requireLeaseCleared(id)
	e.requireObserved("completed")
}

func testRequeueOutcomes(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{MaxAttempts: 5, Lease: time.Minute})
	for _, outcome := range []workitem.Outcome{
		workitem.OutcomeTransient, workitem.OutcomeDependencyDown,
		workitem.OutcomePoisonSuspected, workitem.OutcomeDeadline,
	} {
		id := e.admit(string(outcome))
		r := e.claimOne("worker-a", 1)
		cause := fmt.Errorf("upstream said no (%s)", outcome)
		parked, err := workitem.Requeue(e.ctx, e.conn, e.kind, r, outcome, cause)
		if err != nil {
			t.Fatalf("Requeue %s: %v", outcome, err)
		}
		if parked {
			t.Fatalf("%s: Requeue reported parked with budget left", outcome)
		}
		row := e.row(id)
		if got := asString(row["status"]); got != workitem.StatusReady {
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
		e.requireObserved("claimed 1/0/0/0", "requeued "+string(outcome))
	}

	// An outcome outside the vocabulary is refused rather than stored: the
	// typed value is what parking reasons and metrics key on. Nothing was
	// written, so nothing is reported.
	id := e.admit("bogus")
	r := e.claimOne("worker-a", 1)
	if _, err := workitem.Requeue(e.ctx, e.conn, e.kind, r, workitem.Outcome("whatever"), nil); err == nil {
		t.Error("Requeue accepted an unknown outcome")
	}
	e.requireStatus(id, workitem.StatusLeased)
	e.requireObserved("claimed 1/0/0/0")
}

func testPermanentParks(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{MaxAttempts: 9, Lease: time.Minute})
	id := e.admit("perm")
	r := e.claimOne("worker-a", 1)
	parked, err := workitem.Requeue(e.ctx, e.conn, e.kind, r, workitem.OutcomePermanent, errors.New("rejected"))
	if err != nil {
		t.Fatalf("Requeue permanent: %v", err)
	}
	if !parked {
		t.Fatal("Requeue permanent reported the row returned to ready")
	}
	row := e.row(id)
	if got := asString(row["status"]); got != workitem.StatusParked {
		t.Fatalf("status = %q, want parked with 8 attempts left", got)
	}
	if got := asString(row["last_outcome"]); got != "permanent" {
		t.Fatalf("last_outcome = %q", got)
	}
	e.requireLeaseCleared(id)
	e.requireObserved("claimed 1/0/0/0", "parked permanent")
}

func testBudgetExhaustion(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{MaxAttempts: 2, Lease: shortLease, Backoff: fastBackoff})

	// A requeue that leaves budget returns the row to ready; the claim that
	// spends the last attempt leases it normally. The park happens only when
	// that last attempt dies without a terminal write — at claim time, by the
	// next claimer, with no requeue involved.
	id := e.admit("budget")
	r := e.claimOne("worker-a", 1)
	if parked, err := workitem.Requeue(e.ctx, e.conn, e.kind, r, workitem.OutcomeTransient, errors.New("blip")); err != nil || parked {
		t.Fatalf("Requeue: parked=%v err=%v, want ready", parked, err)
	}
	e.requireStatus(id, workitem.StatusReady)
	e.requireObserved("claimed 1/0/0/0", "requeued transient")

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
	if got := asString(row["status"]); got != workitem.StatusParked {
		t.Fatalf("status = %q, want parked", got)
	}
	if got := asString(row["last_outcome"]); got != "transient" {
		t.Fatalf("last_outcome = %q, want the failure that spent the budget", got)
	}
	e.requireLeaseCleared(id)
	// The claim-time park is reported under the budget reason whatever the
	// row's own last_outcome kept: the metric says why the claim parked it.
	e.requireObserved("claimed 1/0/0/0", "claimed 0/0/0/1", "parked budget_exhausted")

	// A row that never recorded an outcome at all parks under the fallback
	// reason, which is the only thing there is to say about it.
	e3 := mk(t, workitem.Policy{MaxAttempts: 1, Lease: shortLease})
	id3 := e3.admit("never-reported")
	e3.claimOne("worker-a", 1)
	e3.expireLease()
	if res := e3.claim(workitem.Owner{ID: "worker-b", Epoch: 1}, 1); res.Parked != 1 {
		t.Fatalf("reclaim parked %d rows, want 1", res.Parked)
	}
	if got := asString(e3.row(id3)["last_outcome"]); got != "budget_exhausted" {
		t.Fatalf("last_outcome = %q, want budget_exhausted", got)
	}
	e3.requireObserved("claimed 1/0/0/0", "claimed 0/0/0/1", "parked budget_exhausted")

	// A requeue arriving while the row is already at its budget parks there
	// instead, which is the other half of the same rule — and says so.
	e2 := mk(t, workitem.Policy{MaxAttempts: 1, Lease: time.Minute})
	id2 := e2.admit("spent")
	r3 := e2.claimOne("worker-a", 1)
	parked, err := workitem.Requeue(e2.ctx, e2.conn, e2.kind, r3, workitem.OutcomeTransient, errors.New("blip"))
	if err != nil {
		t.Fatalf("Requeue at budget: %v", err)
	}
	if !parked {
		t.Fatal("Requeue at budget reported the row returned to ready")
	}
	e2.requireStatus(id2, workitem.StatusParked)
	e2.requireObserved("claimed 1/0/0/0", "parked transient")
}

// testStrategyIsEnforced pins that the declaration is load-bearing: the other
// strategy's completion verb refuses the kind, and its own completes it.
func testStrategyIsEnforced(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{Lease: time.Minute})
	id := e.admit("strategy")
	r := e.claimOne("worker-a", 1)

	single := e.withKind(func(k *workitem.Kind) { k.Strategy = workitem.SingleTx })
	fenced := e.withKind(func(k *workitem.Kind) { k.Strategy = workitem.FencedReplay })
	if err := workitem.MarkDone(single.ctx, single.conn, single.kind, r); err == nil {
		t.Error("MarkDone accepted a SingleTx kind")
	}
	if err := workitem.Complete(fenced.ctx, fenced.conn, fenced.kind, r, func(*sql.Tx) error { return nil }); err == nil {
		t.Error("Complete accepted a FencedReplay kind")
	}
	e.requireStatus(id, workitem.StatusLeased)

	if err := e.complete(r); err != nil {
		t.Fatalf("completion through the declared %s strategy: %v", e.kind.Strategy, err)
	}
	e.requireStatus(id, workitem.StatusDone)
	e.requireLeaseCleared(id)
}

func testSameOwnerReclaim(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{MaxAttempts: 5, Lease: shortLease})
	id := e.admit("same-owner")
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
	if !second.Reclaimed || second.PreviousOwner != "worker-a" {
		t.Fatalf("same-owner reclaim receipt: reclaimed=%v previous=%q, want true/worker-a", second.Reclaimed, second.PreviousOwner)
	}
	e.assertStragglerLoses(first, "same-owner reclaim")
	e.requireStatus(id, workitem.StatusLeased)
}

func testTakeover(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{MaxAttempts: 5, Lease: shortLease})
	e.admit("takeover")
	e.admit("bystander")

	straggler := e.claimOne("worker-a", 1)
	e.expireLease()
	res := e.claim(workitem.Owner{ID: "worker-b", Epoch: 1}, 1)
	if len(res.Claimed) != 1 {
		t.Fatalf("successor claimed %d rows, want 1", len(res.Claimed))
	}
	successor := res.Claimed[0]
	if successor.ItemID != straggler.ItemID {
		t.Fatalf("successor claimed row %d, want the abandoned %d", successor.ItemID, straggler.ItemID)
	}
	// A takeover is exactly one reclaim, reported on the receipt with the
	// holder it was taken from and counted once on the result.
	if !successor.Reclaimed || successor.PreviousOwner != "worker-a" {
		t.Fatalf("takeover receipt: reclaimed=%v previous=%q, want true/worker-a", successor.Reclaimed, successor.PreviousOwner)
	}
	if res.Reclaimed != 1 {
		t.Fatalf("takeover result reported %d reclaims, want 1", res.Reclaimed)
	}
	e.requireObserved("claimed 1/0/0/0", "claimed 1/1/0/0")
	e.assertStragglerLoses(straggler, "takeover by another owner")

	// The bystander, never leased before, is a plain claim.
	fresh := e.claim(workitem.Owner{ID: "worker-b", Epoch: 1}, 1)
	if len(fresh.Claimed) != 1 || fresh.Claimed[0].Reclaimed || fresh.Reclaimed != 0 {
		t.Fatalf("fresh claim after a takeover reported a reclaim: %+v", fresh)
	}
}

func testExpiryNoSuccessor(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{MaxAttempts: 5, Lease: shortLease})
	e.admit("lapsed")
	r := e.claimOne("worker-a", 1)
	e.expireLease()
	// Nobody has taken over. Expiry alone ends authority.
	e.assertStragglerLoses(r, "expiry with no successor")
}

func testRenewal(t *testing.T, mk envFactory) {
	const lease = 2 * time.Second
	e := mk(t, workitem.Policy{MaxAttempts: 5, Lease: lease})
	id := e.admit("renew")
	r := e.claimOne("worker-a", 1)

	// A second row, claimed straight after the renewal, is the yardstick: a
	// fresh acquisition's expiry IS database-now plus the policy lease, so a
	// renewal that computes the same thing must land beside it. Comparing
	// against it rather than against a window around the old expiry keeps the
	// assertion entirely on database time and tight enough that a wrong formula
	// has nowhere to hide.
	e.admit("yardstick")

	const elapsed = 400 * time.Millisecond
	time.Sleep(elapsed)
	renewed, err := workitem.RenewLease(e.ctx, e.conn, e.kind, r)
	if err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	fresh := e.claimOne("worker-b", 1)

	// Tolerance covers one claim's round trip on a loaded runner, and still sits
	// well below what any wrong formula shows: old+Lease lands ~1.6s out, and a
	// renewal that never moved lands a whole `elapsed` early — which the
	// monotonicity check below catches independently anyway.
	const tolerance = time.Second
	if drift := renewed.LeaseExpiresAt.Sub(fresh.LeaseExpiresAt); drift < -tolerance || drift > tolerance {
		t.Fatalf("renewed expiry is %s from a lease acquired moments later; want now+Lease, not old+Lease", drift)
	}
	if !renewed.LeaseExpiresAt.After(r.LeaseExpiresAt) {
		t.Fatalf("renewal left expiry at %s, no later than the original %s", renewed.LeaseExpiresAt, r.LeaseExpiresAt)
	}
	e.requireStatus(id, workitem.StatusLeased)

	short := mk(t, workitem.Policy{MaxAttempts: 5, Lease: shortLease})
	short.admit("late-renew")
	lr := short.claimOne("worker-a", 1)
	short.expireLease()
	if _, err := workitem.RenewLease(short.ctx, short.conn, short.kind, lr); !errors.Is(err, workitem.ErrLeaseLost) {
		t.Fatalf("late RenewLease = %v, want ErrLeaseLost", err)
	}
}

func testClaimBatch(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{MaxAttempts: 5, Lease: time.Minute})
	var ids []int64
	for i := 0; i < 5; i++ {
		ids = append(ids, e.admit(fmt.Sprintf("batch-%d", i)))
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
		e.requireStatus(id, workitem.StatusCancelled)
		e.requireLeaseCleared(id)
	}
	// Two rounds: the first picks three, settles two and leases one, so the
	// second picks the remaining two. Each settlement is reported on its own
	// beside its round's shape.
	e.requireObserved("claimed 1/0/2/0", "cancelled", "cancelled", "claimed 2/0/0/0")
}

// testClaimRoundIsAllOrNothing pins the boundary between a round and the result
// it reports. A round is one transaction; a row that fails partway through
// rolls its predecessors' leases back, and a receipt for a rolled-back lease is
// worse than no receipt — its holder would act on authority the database never
// granted, and every later operation would answer ErrLeaseLost.
func testClaimRoundIsAllOrNothing(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{MaxAttempts: 5, Lease: time.Minute})
	cancelled := e.admit("settles")
	first := e.admit("leases-first")
	breaks := e.admit("breaks-the-round")
	untouched := e.admit("never-picked")
	if err := workitem.RequestCancel(e.ctx, e.conn, e.kind, e.org, cancelled, "operator", "not needed"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	// One live lease per owner: a constraint the round's SECOND lease violates.
	// It makes the mid-round failure deterministic on both dialects without a
	// trigger, and it fails on exactly the statement the package issues. It is
	// dropped again on the way out because a production table outlives the
	// subtest on Postgres, where the harness truncates rather than recreates.
	e.exec("CREATE UNIQUE INDEX one_lease_per_owner ON " + e.kind.Table + " (lease_owner) WHERE status = 'leased'")
	t.Cleanup(func() {
		if _, err := e.conn.ExecContext(context.Background(), "DROP INDEX IF EXISTS one_lease_per_owner"); err != nil {
			t.Errorf("drop one_lease_per_owner: %v", err)
		}
	})

	before := e.table()
	res, err := workitem.Claim(e.ctx, e.conn, e.kind, workitem.Owner{ID: "worker-a", Epoch: 1}, e.org, 3)
	if err == nil {
		t.Fatal("Claim succeeded despite a constraint its second lease violates")
	}
	if len(res.Claimed) != 0 || res.Cancelled != 0 || res.Parked != 0 || res.Reclaimed != 0 {
		t.Fatalf("a rolled-back round reported %d receipts, %d cancelled, %d parked, %d reclaimed; want nothing",
			len(res.Claimed), res.Cancelled, res.Parked, res.Reclaimed)
	}
	if after := e.table(); !reflect.DeepEqual(before, after) {
		t.Errorf("a rolled-back round left changes behind\nbefore: %v\nafter:  %v", before, after)
	}
	for _, id := range []int64{cancelled, first, breaks, untouched} {
		e.requireStatus(id, workitem.StatusReady)
	}
	e.requireObserved()
}

func testFrozenColumns(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: shortLease}})
	id := e.admitWith("frozen", "p", 41)
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

func testMeasure(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{MaxAttempts: 5, Lease: time.Minute})

	empty, err := workitem.Measure(e.ctx, e.conn, e.kind, e.org)
	if err != nil {
		t.Fatalf("Measure on an empty table: %v", err)
	}
	if empty != (workitem.Depths{}) {
		t.Fatalf("empty table measured %+v, want all zeros", empty)
	}

	ripe := e.admit("ripe")
	e.admit("leased")
	parked := e.admit("parked")
	deferredID := e.admit("deferred")

	leasedReceipt := e.claim(workitem.Owner{ID: "worker-a", Epoch: 1}, 4)
	// Claim took all four; put three of them back into the shapes we want to
	// measure and leave one leased.
	byID := map[int64]workitem.Receipt{}
	for _, r := range leasedReceipt.Claimed {
		byID[r.ItemID] = r
	}
	if _, err := workitem.Requeue(e.ctx, e.conn, e.kind, byID[ripe], workitem.OutcomeTransient, errors.New("x")); err != nil {
		t.Fatalf("requeue ripe: %v", err)
	}
	e.exec("UPDATE "+e.kind.Table+" SET next_attempt_at = NULL WHERE id = ?", ripe)
	if err := workitem.Park(e.ctx, e.conn, e.kind, byID[parked], "stuck"); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := workitem.Defer(e.ctx, e.conn, e.kind, byID[deferredID], "waiting", time.Now().Add(time.Hour), truePredicate); err != nil {
		t.Fatalf("defer: %v", err)
	}

	// Ages are measured from first_enqueued_at, which SQLite stores at
	// millisecond resolution: an admit and a measure in the same millisecond
	// read as zero. Backdating the rows makes the age a fact of the fixture
	// rather than of how fast this machine ran the lines above.
	e.exec("UPDATE "+e.kind.Table+" SET first_enqueued_at = "+e.pastExpr()+" WHERE org_id = ?", e.org)
	d, err := workitem.Measure(e.ctx, e.conn, e.kind, e.org)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if d.Ready != 1 || d.Leased != 1 || d.Parked != 1 || d.Deferred != 1 {
		t.Fatalf("depths = %+v, want one of each", d)
	}
	if d.OldestReadyAge < 59*time.Minute {
		t.Errorf("OldestReadyAge = %s with a ripe ready row enqueued an hour ago", d.OldestReadyAge)
	}
	if d.OldestDeferredAge < 59*time.Minute {
		t.Errorf("OldestDeferredAge = %s with a deferred row enqueued an hour ago", d.OldestDeferredAge)
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

// testIndexPresence asserts every index the kind's IndexDDL renders is on the
// table under test, by name: the three claim arms, the parked list, and the
// uniqueness index when the kind declares one.
func testIndexPresence(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{Lease: time.Minute})
	e.requireIndexes(e.kind)
}

// testFixtureIndexPresence is the same check over both fixture tables, each
// in its own uniqueness mode.
func testFixtureIndexPresence(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{Lease: time.Minute}})
	for _, spec := range []struct {
		table  string
		unique workitem.UniqueMode
	}{
		{FixtureTable, workitem.UniqueWhileUnsettled},
		{FixtureForeverTable, workitem.UniqueForever},
	} {
		e.requireIndexes(workitem.Kind{Table: spec.table, Dialect: e.dialect, Unique: spec.unique, Strategy: workitem.SingleTx})
	}
}

func (e *env) requireIndexes(k workitem.Kind) {
	e.t.Helper()
	want := workitem.IndexNames(k)
	wantCount := 4
	if k.Unique != workitem.UniqueNone {
		wantCount = 5
	}
	if len(want) != wantCount {
		e.t.Fatalf("%s: IndexNames returned %d names, want %d (three claim arms, the parked list, and uniqueness when declared)", k.Table, len(want), wantCount)
	}
	have := e.indexNames(k.Table)
	for _, name := range want {
		if !have[name] {
			e.t.Errorf("%s: missing index %s (present: %v)", k.Table, name, have)
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
// assert that fn's effects landed or rolled back with the disposition. Over a
// production table there is no payload column, so the closure does nothing:
// the assertions that read it back only run on the fixture.
func (e *env) writePayload(id int64, value string) func(*sql.Tx) error {
	if !e.fixture {
		return func(*sql.Tx) error { return nil }
	}
	return func(tx *sql.Tx) error {
		_, err := tx.ExecContext(e.ctx, e.q("UPDATE "+e.kind.Table+" SET payload = ? WHERE id = ?"), value, id)
		return err
	}
}

func truePredicate(*sql.Tx) (bool, error) { return true, nil }

// testMeasureByOrg pins that the grouped read agrees with the org-scoped one
// for the subtest's org, and reports nothing for an org with no unsettled
// rows — including this one once everything settles.
func testMeasureByOrg(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{MaxAttempts: 5, Lease: time.Minute})

	byOrg, err := workitem.MeasureByOrg(e.ctx, e.conn, e.kind)
	if err != nil {
		t.Fatalf("MeasureByOrg on an empty table: %v", err)
	}
	if _, ok := byOrg[e.org]; ok {
		t.Fatalf("an org with no rows is present in %+v", byOrg)
	}

	ripe := e.admit("ripe")
	e.admit("leased")
	parked := e.admit("parked")
	deferredID := e.admit("deferred")
	res := e.claim(workitem.Owner{ID: "worker-a", Epoch: 1}, 4)
	byID := map[int64]workitem.Receipt{}
	for _, r := range res.Claimed {
		byID[r.ItemID] = r
	}
	if _, err := workitem.Requeue(e.ctx, e.conn, e.kind, byID[ripe], workitem.OutcomeTransient, errors.New("x")); err != nil {
		t.Fatalf("requeue ripe: %v", err)
	}
	e.exec("UPDATE "+e.kind.Table+" SET next_attempt_at = NULL WHERE id = ?", ripe)
	if err := workitem.Park(e.ctx, e.conn, e.kind, byID[parked], "stuck"); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := workitem.Defer(e.ctx, e.conn, e.kind, byID[deferredID], "waiting", time.Now().Add(time.Hour), truePredicate); err != nil {
		t.Fatalf("defer: %v", err)
	}

	// Ages are measured from first_enqueued_at, which SQLite stores at
	// millisecond resolution: an admit and a measure in the same millisecond
	// read as zero. Backdating the rows makes the age a fact of the fixture
	// rather than of how fast this machine ran the lines above.
	e.exec("UPDATE "+e.kind.Table+" SET first_enqueued_at = "+e.pastExpr()+" WHERE org_id = ?", e.org)
	scoped, err := workitem.Measure(e.ctx, e.conn, e.kind, e.org)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	byOrg, err = workitem.MeasureByOrg(e.ctx, e.conn, e.kind)
	if err != nil {
		t.Fatalf("MeasureByOrg: %v", err)
	}
	got, ok := byOrg[e.org]
	if !ok {
		t.Fatalf("org %s absent from %+v", e.org, byOrg)
	}
	if got.Ready != scoped.Ready || got.Leased != scoped.Leased || got.Parked != scoped.Parked || got.Deferred != scoped.Deferred {
		t.Fatalf("grouped depths %+v disagree with the scoped read %+v", got, scoped)
	}
	if got.Ready != 1 || got.Leased != 1 || got.Parked != 1 || got.Deferred != 1 {
		t.Fatalf("depths = %+v, want one of each", got)
	}
	if got.OldestReadyAge < 59*time.Minute || got.OldestDeferredAge < 59*time.Minute {
		t.Errorf("ages = %s/%s, want both about an hour", got.OldestReadyAge, got.OldestDeferredAge)
	}

	// Terminal rows contribute to no depth: settle everything and the org
	// disappears from the grouped read rather than reporting zeros.
	e.exec("UPDATE " + e.kind.Table + " SET status = 'done', done_at = " + e.nowSQL() + ", lease_owner = NULL, lease_epoch = NULL, leased_at = NULL, lease_expires_at = NULL")
	byOrg, err = workitem.MeasureByOrg(e.ctx, e.conn, e.kind)
	if err != nil {
		t.Fatalf("MeasureByOrg after settling: %v", err)
	}
	if _, ok := byOrg[e.org]; ok {
		t.Fatalf("an org with only settled rows is present in %+v", byOrg)
	}
	if d, err := workitem.Measure(e.ctx, e.conn, e.kind, e.org); err != nil || d != (workitem.Depths{}) {
		t.Fatalf("Measure after settling = %+v err=%v, want zeros", d, err)
	}
}

// testMeasureByOrgAcrossOrgs is the fixture-only half: three orgs with
// different shapes in one table, each reported under its own key and
// matching its own scoped read.
func testMeasureByOrgAcrossOrgs(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: time.Minute}})
	a, b, c, idle := e.org, uuid.NewString(), uuid.NewString(), uuid.NewString()

	e.admitIn(a, "a-1")
	e.admitIn(a, "a-2")
	e.admitIn(b, "b-1")
	e.admitIn(c, "c-1")
	// b's row leased, c's row settled, idle never had one.
	if got := len(e.claimIn(workitem.Owner{ID: "w", Epoch: 1}, b, 1).Claimed); got != 1 {
		t.Fatalf("leased %d rows in b, want 1", got)
	}
	cres := e.claimIn(workitem.Owner{ID: "w", Epoch: 1}, c, 1)
	if len(cres.Claimed) != 1 {
		t.Fatalf("leased %d rows in c, want 1", len(cres.Claimed))
	}
	if err := e.complete(cres.Claimed[0]); err != nil {
		t.Fatalf("complete c: %v", err)
	}

	byOrg, err := workitem.MeasureByOrg(e.ctx, e.conn, e.kind)
	if err != nil {
		t.Fatalf("MeasureByOrg: %v", err)
	}
	for _, org := range []string{a, b} {
		scoped, err := workitem.Measure(e.ctx, e.conn, e.kind, org)
		if err != nil {
			t.Fatalf("Measure %s: %v", org, err)
		}
		got, ok := byOrg[org]
		if !ok {
			t.Fatalf("org %s absent from %+v", org, byOrg)
		}
		if got.Ready != scoped.Ready || got.Leased != scoped.Leased || got.Parked != scoped.Parked || got.Deferred != scoped.Deferred {
			t.Errorf("org %s: grouped %+v disagrees with scoped %+v", org, got, scoped)
		}
	}
	if byOrg[a].Ready != 2 || byOrg[b].Leased != 1 {
		t.Errorf("depths = %+v, want a: 2 ready, b: 1 leased", byOrg)
	}
	for _, org := range []string{c, idle} {
		if _, ok := byOrg[org]; ok {
			t.Errorf("org %s with no unsettled rows is present in %+v", org, byOrg)
		}
	}
}

// testClaimAcrossOrgsReportsPerOrg pins the observer's rule for a cross-org
// claim: one round mixing tenants is reported once per org with that org's
// counts, in a fixed order, with each settlement and budget park beside it.
func testClaimAcrossOrgsReportsPerOrg(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 1, Lease: shortLease}})
	a, b := e.org, uuid.NewString()
	if a > b {
		a, b = b, a
	}
	e.admitIn(a, "a-lease")
	e.admitIn(a, "a-cancel")
	cancelled := e.admitIn(a, "a-cancel-2")
	spent := e.admitIn(b, "b-spent")
	e.admitIn(b, "b-lease")
	if err := workitem.RequestCancel(e.ctx, e.conn, e.kind, a, cancelled, "operator", "no"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	// b's first row spends its one attempt and dies, so the cross-org round
	// below parks it at claim.
	if got := e.claimIn(workitem.Owner{ID: "w", Epoch: 1}, b, 1); len(got.Claimed) != 1 || got.Claimed[0].ItemID != spent {
		t.Fatalf("pre-claim in b = %+v, want the spent row", got)
	}
	e.expireLease()
	e.obs.take()

	res := e.claimIn(workitem.Owner{ID: "w", Epoch: 2}, "", 10)
	if len(res.Claimed) != 3 || res.Cancelled != 1 || res.Parked != 1 {
		t.Fatalf("cross-org claim = %d leased, %d cancelled, %d parked; want 3/1/1", len(res.Claimed), res.Cancelled, res.Parked)
	}
	got := e.obs.take()
	want := []string{
		"claimed " + a + " 2/0/1/0",
		"cancelled " + a,
		"claimed " + b + " 1/0/0/1",
		"parked " + b + " " + workitem.ReasonBudgetExhausted,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observer saw\n  %q\nwant\n  %q", got, want)
	}
}

// testListAndGet pins the operator reads: newest first, a correct total per
// status filter including the deferred partition, a clean miss, and the
// block's timestamps scanning on both dialects.
func testListAndGet(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{MaxAttempts: 5, Lease: time.Minute})

	ripe := e.admit("ripe")
	leasedID := e.admit("leased")
	parked := e.admit("parked")
	deferredID := e.admit("deferred")
	doneID := e.admit("done")
	res := e.claim(workitem.Owner{ID: "worker-a", Epoch: 7}, 5)
	byID := map[int64]workitem.Receipt{}
	for _, r := range res.Claimed {
		byID[r.ItemID] = r
	}
	if _, err := workitem.Requeue(e.ctx, e.conn, e.kind, byID[ripe], workitem.OutcomeTransient, errors.New("blip")); err != nil {
		t.Fatalf("requeue ripe: %v", err)
	}
	e.exec("UPDATE "+e.kind.Table+" SET next_attempt_at = NULL WHERE id = ?", ripe)
	if err := workitem.Park(e.ctx, e.conn, e.kind, byID[parked], "stuck"); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := workitem.Defer(e.ctx, e.conn, e.kind, byID[deferredID], "waiting", time.Now().Add(time.Hour), truePredicate); err != nil {
		t.Fatalf("defer: %v", err)
	}
	if err := e.complete(byID[doneID]); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := workitem.RequestCancel(e.ctx, e.conn, e.kind, e.org, leasedID, "operator", "stop"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	all, total, err := workitem.List(e.ctx, e.conn, e.kind, e.org, "", 50, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 5 || len(all) != 5 {
		t.Fatalf("unfiltered list: total=%d rows=%d, want 5", total, len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID >= all[i-1].ID {
			t.Fatalf("list is not newest first: %d then %d", all[i-1].ID, all[i].ID)
		}
	}
	for _, it := range all {
		if it.OrgID != e.org || it.FirstEnqueuedAt.IsZero() || it.CreatedAt.IsZero() || it.MaxAttempts != 5 {
			t.Errorf("row %d = %+v, want the block scanned", it.ID, it)
		}
	}

	// Each status filter counts and lists exactly its partition.
	for status, want := range map[string]int64{
		workitem.StatusReady:     ripe,
		workitem.StatusLeased:    leasedID,
		workitem.StatusParked:    parked,
		workitem.StatusDeferred:  deferredID,
		workitem.StatusDone:      doneID,
		workitem.StatusCancelled: 0,
	} {
		got, total, err := workitem.List(e.ctx, e.conn, e.kind, e.org, status, 50, 0)
		if err != nil {
			t.Fatalf("List %s: %v", status, err)
		}
		if want == 0 {
			if total != 0 || len(got) != 0 {
				t.Errorf("%s: total=%d rows=%d, want none", status, total, len(got))
			}
			continue
		}
		if total != 1 || len(got) != 1 || got[0].ID != want {
			t.Errorf("%s: total=%d rows=%v, want only row %d", status, total, got, want)
		}
	}
	if _, _, err := workitem.List(e.ctx, e.conn, e.kind, e.org, "whatever", 50, 0); err == nil {
		t.Error("List accepted an unknown status")
	}

	// A window of one pages the unfiltered set without dropping a row; a zero
	// window is the count alone.
	page1, _, err := workitem.List(e.ctx, e.conn, e.kind, e.org, "", 1, 0)
	if err != nil || len(page1) != 1 || page1[0].ID != all[0].ID {
		t.Fatalf("page 1 = %+v err=%v", page1, err)
	}
	page2, _, err := workitem.List(e.ctx, e.conn, e.kind, e.org, "", 1, 1)
	if err != nil || len(page2) != 1 || page2[0].ID != all[1].ID {
		t.Fatalf("page 2 = %+v err=%v", page2, err)
	}
	none, countOnly, err := workitem.List(e.ctx, e.conn, e.kind, e.org, workitem.StatusParked, 0, 0)
	if err != nil || countOnly != 1 || len(none) != 0 {
		t.Fatalf("count-only = %+v total=%d err=%v", none, countOnly, err)
	}

	// The single read answers with the list's row, lease and cancel columns
	// included; a miss and another org's id are (nil, nil).
	got, err := workitem.Get(e.ctx, e.conn, e.kind, e.org, leasedID)
	if err != nil || got == nil {
		t.Fatalf("Get: row=%v err=%v", got, err)
	}
	if got.Status != workitem.StatusLeased || got.LeaseOwner != "worker-a" || got.LeaseEpoch == nil || *got.LeaseEpoch != 7 {
		t.Errorf("leased row = %+v, want its lease columns", got)
	}
	if got.LeasedAt == nil || got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.After(*got.LeasedAt) {
		t.Errorf("lease timestamps = %v/%v, want an expiry after the acquisition", got.LeasedAt, got.LeaseExpiresAt)
	}
	if got.CancelRequestedAt == nil || got.CancelRequestedBy != "operator" || got.CancelReason != "stop" {
		t.Errorf("cancel columns = %v/%q/%q, want the recorded request", got.CancelRequestedAt, got.CancelRequestedBy, got.CancelReason)
	}
	var fromList *workitem.Item
	for i := range all {
		if all[i].ID == leasedID {
			fromList = &all[i]
		}
	}
	if fromList == nil || !reflect.DeepEqual(*fromList, *got) {
		t.Errorf("Get = %+v, want the list's row %+v", got, fromList)
	}
	parkedRow, err := workitem.Get(e.ctx, e.conn, e.kind, e.org, parked)
	if err != nil || parkedRow == nil || parkedRow.DoneAt == nil || parkedRow.LastOutcome != "stuck" {
		t.Errorf("parked row = %+v err=%v, want done_at and the park reason", parkedRow, err)
	}
	if miss, err := workitem.Get(e.ctx, e.conn, e.kind, e.org, 999999); err != nil || miss != nil {
		t.Errorf("Get of an unknown id = %+v err=%v, want (nil, nil)", miss, err)
	}
	if miss, err := workitem.Get(e.ctx, e.conn, e.kind, uuid.NewString(), leasedID); err != nil || miss != nil {
		t.Errorf("Get under another org = %+v err=%v, want (nil, nil)", miss, err)
	}
	if other, total, err := workitem.List(e.ctx, e.conn, e.kind, uuid.NewString(), "", 50, 0); err != nil || total != 0 || len(other) != 0 {
		t.Errorf("List under another org = %+v total=%d err=%v, want nothing", other, total, err)
	}
}

// testNilObserver pins that a Kind with no observer behaves identically: every
// disposition lands, and nothing is reported anywhere.
func testNilObserver(t *testing.T, mk envFactory) {
	e := mk(t, workitem.Policy{MaxAttempts: 5, Lease: time.Minute})
	bare := e.withKind(func(k *workitem.Kind) { k.Observer = nil })

	id := bare.admit("quiet")
	r := bare.claimOne("worker-a", 1)
	if _, err := workitem.Requeue(bare.ctx, bare.conn, bare.kind, r, workitem.OutcomeTransient, errors.New("blip")); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	bare.exec("UPDATE "+bare.kind.Table+" SET next_attempt_at = NULL WHERE id = ?", id)
	r2 := bare.claimOne("worker-a", 1)
	if err := workitem.Park(bare.ctx, bare.conn, bare.kind, r2, "stuck"); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if err := workitem.Redrive(bare.ctx, bare.conn, bare.kind, bare.org, id, "operator"); err != nil {
		t.Fatalf("Redrive: %v", err)
	}
	r3 := bare.claimOne("worker-a", 1)
	if err := bare.complete(r3); err != nil {
		t.Fatalf("complete: %v", err)
	}
	bare.requireStatus(id, workitem.StatusDone)
	// The recorder is still the shared env's, and the bare kind never spoke
	// to it.
	e.requireObserved()
}

// testClaimFilter pins the kind's claim filter: a ready row it excludes is
// not ripe — never picked, counted and listed as deferred — while a
// cancellation request on it still settles at the next claim and an expired
// lease on it is still reclaimed. The filter is a constant fragment over the
// alias t, so the fixture's copy of the kind blocks on its payload column.
func testClaimFilter(t *testing.T, mk Factory) {
	e := setup(t, mk, opts{unique: workitem.UniqueWhileUnsettled, policy: workitem.Policy{MaxAttempts: 5, Lease: shortLease}})
	e = e.withKind(func(k *workitem.Kind) { k.ClaimFilter = "t.payload <> 'blocked'" })

	open := e.admitWith("open", "p", 0)
	blocked := e.admitWith("blocked", "blocked", 0)

	// Only the admitted row passes the filter; the blocked one is never
	// picked however many rounds the claim runs.
	res := e.claim(workitem.Owner{ID: "worker-a", Epoch: 1}, 5)
	if len(res.Claimed) != 1 || res.Claimed[0].ItemID != open {
		t.Fatalf("claim returned %+v, want only the unblocked row %d", res.Claimed, open)
	}
	e.requireStatus(blocked, workitem.StatusReady)
	if err := e.complete(res.Claimed[0]); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Measure and List agree: the blocked row is deferred, not ready, and
	// its age is the deferred age.
	e.exec("UPDATE "+e.kind.Table+" SET first_enqueued_at = "+e.pastExpr()+" WHERE id = ?", blocked)
	d, err := workitem.Measure(e.ctx, e.conn, e.kind, e.org)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if d.Ready != 0 || d.Deferred != 1 {
		t.Fatalf("depths = %+v, want the blocked row deferred and nothing ready", d)
	}
	if d.OldestDeferredAge < 59*time.Minute || d.OldestReadyAge != 0 {
		t.Errorf("ages = ready %s / deferred %s, want the blocked row's hour on the deferred side only", d.OldestReadyAge, d.OldestDeferredAge)
	}
	byOrg, err := workitem.MeasureByOrg(e.ctx, e.conn, e.kind)
	if err != nil {
		t.Fatalf("MeasureByOrg: %v", err)
	}
	if got := byOrg[e.org]; got.Ready != 0 || got.Deferred != 1 {
		t.Errorf("MeasureByOrg[%s] = %+v, want one deferred", e.org, got)
	}
	ready, total, err := workitem.List(e.ctx, e.conn, e.kind, e.org, workitem.StatusReady, 50, 0)
	if err != nil || total != 0 || len(ready) != 0 {
		t.Errorf("List(ready) = %+v total=%d err=%v, want nothing", ready, total, err)
	}
	deferred, total, err := workitem.List(e.ctx, e.conn, e.kind, e.org, workitem.StatusDeferred, 50, 0)
	if err != nil || total != 1 || len(deferred) != 1 || deferred[0].ID != blocked {
		t.Errorf("List(deferred) = %+v total=%d err=%v, want the blocked row", deferred, total, err)
	}

	// Unblocking the row makes it ripe with no write of the package's own.
	e.exec("UPDATE "+e.kind.Table+" SET payload = 'p' WHERE id = ?", blocked)
	r := e.claimOne("worker-a", 1)
	if r.ItemID != blocked {
		t.Fatalf("claimed %d after unblocking, want %d", r.ItemID, blocked)
	}

	// A blocked row that is leased and whose lease expires is reclaimed
	// whatever the filter says: the unit it replays is fenced.
	e.exec("UPDATE "+e.kind.Table+" SET payload = 'blocked' WHERE id = ?", blocked)
	e.expireLease()
	res = e.claim(workitem.Owner{ID: "worker-b", Epoch: 1}, 5)
	if len(res.Claimed) != 1 || res.Claimed[0].ItemID != blocked || !res.Claimed[0].Reclaimed {
		t.Fatalf("claim after expiry = %+v, want the blocked row reclaimed", res.Claimed)
	}
	if _, err := workitem.Requeue(e.ctx, e.conn, e.kind, res.Claimed[0], workitem.OutcomeTransient, errors.New("blip")); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	e.exec("UPDATE "+e.kind.Table+" SET next_attempt_at = NULL WHERE id = ?", blocked)

	// A cancellation request on a blocked ready row settles at the next
	// claim whatever the filter says.
	if err := workitem.RequestCancel(e.ctx, e.conn, e.kind, e.org, blocked, "operator", "stop"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	res = e.claim(workitem.Owner{ID: "worker-a", Epoch: 1}, 5)
	if len(res.Claimed) != 0 || res.Cancelled != 1 {
		t.Fatalf("claim with a cancelled blocked row = %+v, want one settled and nothing leased", res)
	}
	e.requireStatus(blocked, workitem.StatusCancelled)

	// Validate refuses a fragment that could be a second statement, and one
	// that references nothing on the row.
	for _, bad := range []string{"t.payload <> 'x'; DROP TABLE " + e.kind.Table, "1 = 1"} {
		k := e.kind
		k.ClaimFilter = bad
		if err := k.Validate(); err == nil {
			t.Errorf("Validate accepted claim filter %q", bad)
		}
	}
}
