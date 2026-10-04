package delegate

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/suspendclock"
)

// recordingQueue wraps the real conversation queue and counts the two lease
// verbs. before, when set, runs at the top of the first renewal: it is how a
// test puts a suspend exactly between a renewal's issue and its statement,
// rather than racing the loop's ticker to get there first. beforeReacquire
// runs at the top of every re-acquire, for the same reason. reacquireErr,
// when set, is what every re-acquire returns instead of reaching the store:
// the database failing the call rather than refusing it.
type recordingQueue struct {
	db.ConversationQueueStore

	mu              sync.Mutex
	renewed         int
	refused         []error
	reacquires      int
	before          func()
	beforeReacquire func()
	reacquireErr    error
	// reacquiredAt is when the first re-acquire the store accepted returned.
	reacquiredAt time.Time
}

func (q *recordingQueue) RenewClaimLeaseSystem(ctx context.Context, orgID, conversationID, claimID string, lease time.Duration, activity db.ClaimActivity) (db.ClaimRenewal, error) {
	q.mu.Lock()
	before := q.before
	q.before = nil
	q.mu.Unlock()
	if before != nil {
		before()
	}
	r, err := q.ConversationQueueStore.RenewClaimLeaseSystem(ctx, orgID, conversationID, claimID, lease, activity)
	q.mu.Lock()
	if err == nil {
		q.renewed++
	} else {
		q.refused = append(q.refused, err)
	}
	q.mu.Unlock()
	return r, err
}

func (q *recordingQueue) ReacquireClaimLeaseSystem(ctx context.Context, orgID, conversationID, claimID, executorID string, bootEpoch int64, lease time.Duration) (db.ClaimRenewal, error) {
	q.mu.Lock()
	q.reacquires++
	before, fail := q.beforeReacquire, q.reacquireErr
	q.mu.Unlock()
	if before != nil {
		before()
	}
	if fail != nil {
		return db.ClaimRenewal{}, fail
	}
	r, err := q.ConversationQueueStore.ReacquireClaimLeaseSystem(ctx, orgID, conversationID, claimID, executorID, bootEpoch, lease)
	if err == nil {
		q.mu.Lock()
		if q.reacquiredAt.IsZero() {
			q.reacquiredAt = time.Now()
		}
		q.mu.Unlock()
	}
	return r, err
}

func (q *recordingQueue) firstReacquire() time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reacquiredAt
}

func (q *recordingQueue) counts() (renewed, refused, reacquires int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.renewed, len(q.refused), q.reacquires
}

func (q *recordingQueue) refusals() []error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]error(nil), q.refused...)
}

// countingInserts counts the transcript inserts that reach the real store,
// underneath the recovering wrapper, so a test can see a refused write and its
// retry as two calls.
type countingInserts struct {
	db.ConversationStore
	calls atomic.Int32
}

func (c *countingInserts) InsertMessageForClaimSystem(ctx context.Context, orgID, claimID string, msg *domain.Message) (int64, error) {
	c.calls.Add(1)
	return c.ConversationStore.InsertMessageForClaimSystem(ctx, orgID, claimID, msg)
}

// The ages of the last accepted renewal the tests start from: one well inside
// the self-fence deadline, so the engagement was renewing healthily when it
// slept, and one past it, so it was not. suspendPastTheLease is a sleep long
// enough to lapse the lease; the tests lapse it on database time themselves,
// so its length only has to read as a suspend.
const (
	renewedRecently     = DefaultClaimSelfFenceDeadline / 4
	renewedTooLongAgo   = DefaultClaimSelfFenceDeadline + 5*time.Second
	suspendPastTheLease = 2 * DefaultClaimLease
)

// suspendFixture is one claimed conversation in a real SQLite store, driven by
// a spawner whose suspend clock is faked. The claim is minted as the spawner's
// own executor boot, which is what the re-acquire requires.
type suspendFixture struct {
	s        *Spawner
	database *sql.DB
	queue    *recordingQueue
	inserts  *countingInserts
	conv     *domain.Conversation

	claimCtx context.Context
	fence    context.CancelCauseFunc

	asleep atomic.Int64 // the fake suspend clock's reading, in nanoseconds

	// recoveries reads the suspend-recovery counter for one outcome, from a
	// manual reader the fixture swapped in for the test's duration.
	recoveries func(outcome string) int64
}

// suspendRecoveryCounter swaps the suspend-recovery counter for one on a
// manual reader and returns a reader of its value for an outcome.
func suspendRecoveryCounter(t *testing.T) func(outcome string) int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := suspendRecoveries.Swap(newSuspendRecoveryStats(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { suspendRecoveries.Store(prev) })
	return func(outcome string) int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "claims.suspend_recoveries" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("claims.suspend_recoveries is %T, want an int64 sum", m.Data)
				}
				for _, dp := range sum.DataPoints {
					if v, ok := dp.Attributes.Value(attribute.Key("outcome")); ok && v.AsString() == outcome {
						return dp.Value
					}
				}
			}
		}
		return 0
	}
}

// assertRecoveries checks the counter against want for every outcome, so an
// outcome a test did not expect is caught as well as a missing one.
func (f *suspendFixture) assertRecoveries(t *testing.T, want map[string]int64) {
	t.Helper()
	for _, outcome := range []string{suspendRecoveryReacquired, suspendRecoveryTaken, suspendRecoveryFailed} {
		if got := f.recoveries(outcome); got != want[outcome] {
			t.Errorf("claims.suspend_recoveries{outcome=%q} = %d, want %d", outcome, got, want[outcome])
		}
	}
}

func newSuspendFixture(t *testing.T) *suspendFixture {
	t.Helper()
	f := newClaimLeaseFixture(t)
	suspendclock.SetSourceForTest(t, func() (time.Duration, bool) {
		return time.Duration(f.asleep.Load()), true
	})
	return f
}

// newClaimLeaseFixture is newSuspendFixture on the platform's real suspend
// clock, for the test that waits for an actual sleep.
func newClaimLeaseFixture(t *testing.T) *suspendFixture {
	t.Helper()
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	const convID = "conv-suspend"
	seedConversation(t, database, convID, "sess-suspend", "/tmp/wt-suspend")
	claimID := markEngaged(t, database, convID)

	f := &suspendFixture{database: database, recoveries: suspendRecoveryCounter(t)}

	stores := testSpawnerStores(database)
	f.queue = &recordingQueue{ConversationQueueStore: stores.ConversationQueue}
	f.inserts = &countingInserts{ConversationStore: stores.Conversations}
	stores.ConversationQueue = f.queue
	stores.Conversations = f.inserts
	f.s = NewSpawner(database, stores, nil, nil, "claude-sonnet-4-6")
	f.s.SetExecutorID("test-engagement", 1)

	f.conv = &domain.Conversation{ID: convID, OrgID: runmode.LocalDefaultOrgID, ClaimID: claimID}
	f.claimCtx, f.fence = context.WithCancelCause(context.Background())
	t.Cleanup(func() { f.fence(nil) })
	return f
}

// sleep advances the fake suspend clock: the machine was asleep for d.
func (f *suspendFixture) sleep(d time.Duration) { f.asleep.Add(int64(d)) }

// lapse moves the claim's lease a minute into the past on database time,
// which is what a suspend longer than the lease's remaining time leaves. It
// and release run inside store hooks on the loop's goroutine, so they report
// with Errorf rather than Fatalf.
func (f *suspendFixture) lapse(t *testing.T) {
	t.Helper()
	if _, err := f.database.Exec(
		`UPDATE claims SET lease_expires_at = strftime('%Y-%m-%d %H:%M:%f','now','-60.000 seconds') WHERE id = ?`,
		f.conv.ClaimID,
	); err != nil {
		t.Errorf("lapse the lease: %v", err)
	}
}

// release releases the claim the way another executor's takeover does.
func (f *suspendFixture) release(t *testing.T) {
	t.Helper()
	if _, err := f.database.Exec(
		`UPDATE claims SET released_at = strftime('%Y-%m-%d %H:%M:%f','now'), outcome = 'reaped' WHERE id = ?`,
		f.conv.ClaimID,
	); err != nil {
		t.Errorf("release the claim: %v", err)
	}
}

// leaseLive reports whether the claim is unreleased with a lease in the
// future, on database time.
func (f *suspendFixture) leaseLive(t *testing.T) bool {
	t.Helper()
	var live bool
	if err := f.database.QueryRow(
		`SELECT released_at IS NULL AND lease_expires_at > strftime('%Y-%m-%d %H:%M:%f','now') FROM claims WHERE id = ?`,
		f.conv.ClaimID,
	).Scan(&live); err != nil {
		t.Fatalf("read the lease: %v", err)
	}
	return live
}

// register stands in for a running renewal loop: the engagement's lease,
// registered with its last accepted renewal at lastRenewal on the monotonic
// clock and no timers running. The suspend base is the fake clock's reading
// now, as the loop takes it.
func (f *suspendFixture) register(t *testing.T, lastRenewal time.Time) *claimLeaseState {
	t.Helper()
	st := newClaimLeaseState(f.conv, lastRenewal, f.claimCtx, f.fence, DefaultClaimSelfFenceDeadline, DefaultClaimLease, time.Second)
	f.s.registerClaimLease(st)
	t.Cleanup(func() { f.s.deregisterClaimLease(st) })
	return st
}

// transcript is every message row the conversation holds with this content.
func (f *suspendFixture) transcript(t *testing.T, content string) int {
	t.Helper()
	var n int
	if err := f.database.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE conversation_id = ? AND content = ?`, f.conv.ID, content,
	).Scan(&n); err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	return n
}

// runLoop starts the engagement's renewal loop, anchored at anchor, and stops
// it when the test ends.
func (f *suspendFixture) runLoop(t *testing.T, anchor time.Time) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.s.renewClaimLease(ctx, f.conv, anchor, f.claimCtx, f.fence) }()
	t.Cleanup(func() { stop(); <-done })
}

func waitUntil(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSuspendRecovery_RefusedRenewalReacquires: a renewal refused because a
// suspend lapsed the lease, from an engagement that renewed well inside its
// self-fence deadline before it slept, takes the claim back and carries on.
func TestSuspendRecovery_RefusedRenewalReacquires(t *testing.T) {
	f := newSuspendFixture(t)
	f.s.setClaimLease(20*time.Millisecond, 0, 0)
	// The machine sleeps as the first renewal goes out.
	f.queue.before = func() {
		f.lapse(t)
		f.sleep(suspendPastTheLease)
	}
	f.runLoop(t, time.Now().Add(-renewedRecently))

	waitUntil(t, 5*time.Second, "a renewal after the re-acquire", func() bool {
		renewed, _, reacquires := f.queue.counts()
		return reacquires == 1 && renewed >= 1
	})
	if refused := f.queue.refusals(); len(refused) != 1 || !errors.Is(refused[0], db.ErrClaimLeaseExpired) {
		t.Errorf("refused renewals = %v, want exactly the one the suspend lapsed, as ErrClaimLeaseExpired", refused)
	}
	if _, _, reacquires := f.queue.counts(); reacquires != 1 {
		t.Errorf("re-acquires = %d, want 1", reacquires)
	}
	if err := context.Cause(f.claimCtx); err != nil {
		t.Errorf("claim context cancelled with %v; a recovered engagement carries on", err)
	}
	if !f.leaseLive(t) {
		t.Error("the claim's lease is not live after the re-acquire")
	}
	f.assertRecoveries(t, map[string]int64{suspendRecoveryReacquired: 1})
}

// TestSuspendRecovery_SinkWriteRetriedThroughTheWrapper: an SDK sink write
// that meets the lapse first — before the loop has noticed the wake — takes
// the claim back through the store wrapper, is retried once, and lands once.
func TestSuspendRecovery_SinkWriteRetriedThroughTheWrapper(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-renewedRecently))
	f.lapse(t)
	f.sleep(suspendPastTheLease)

	sink := newConversationSink(f.s, f.conv.OrgID, f.conv.ID, f.conv.ClaimID, "event", "")
	if err := sink.OnMessage(&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: "after the wake"}); err != nil {
		t.Fatalf("OnMessage after a suspend: %v", err)
	}
	if sink.fenceTripped() {
		t.Error("the sink tripped its fence on a write the recovery retried")
	}
	if n := f.inserts.calls.Load(); n != 2 {
		t.Errorf("inserts reaching the store = %d, want 2: the refused one and its retry", n)
	}
	if n := f.transcript(t, "after the wake"); n != 1 {
		t.Errorf("transcript rows = %d, want exactly 1", n)
	}
	if _, _, reacquires := f.queue.counts(); reacquires != 1 {
		t.Errorf("re-acquires = %d, want 1", reacquires)
	}
	if err := context.Cause(f.claimCtx); err != nil {
		t.Errorf("claim context cancelled with %v", err)
	}
	if !f.leaseLive(t) {
		t.Error("the claim's lease is not live after the re-acquire")
	}
	f.assertRecoveries(t, map[string]int64{suspendRecoveryReacquired: 1})
}

// claimOutcome reads the fixture claim's release outcome, "" while it is
// unreleased.
func (f *suspendFixture) claimOutcome(t *testing.T) string {
	t.Helper()
	var outcome sql.NullString
	if err := f.database.QueryRow(
		`SELECT CASE WHEN released_at IS NULL THEN NULL ELSE outcome END FROM claims WHERE id = ?`,
		f.conv.ClaimID,
	).Scan(&outcome); err != nil {
		t.Fatalf("read the claim's outcome: %v", err)
	}
	return outcome.String
}

// TestSuspendRecovery_ClaimReleasesRetriedAfterTheReacquire: a requeue or a
// hand-back that meets a lapse a suspend caused takes the claim back and
// releases it with its own outcome, as any other holder write would.
func TestSuspendRecovery_ClaimReleasesRetriedAfterTheReacquire(t *testing.T) {
	cases := []struct {
		name    string
		release func(f *suspendFixture) bool
		want    string
	}{
		{"requeue", func(f *suspendFixture) bool {
			return f.s.requeueClaim(f.conv.OrgID, *f.conv, db.RequeueSetupFailure, 0, errors.New("clone failed"), "after a pre-agent failure")
		}, string(db.RequeueSetupFailure)},
		{"hand_back", func(f *suspendFixture) bool {
			return !f.s.handBackClaim(context.Background(), liveParkContext{orgID: f.conv.OrgID, conversationID: f.conv.ID, claimID: f.conv.ClaimID}, db.HandBackShutdown, 0, "")
		}, db.HandBackShutdown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSuspendFixture(t)
			f.register(t, time.Now().Add(-renewedRecently))
			f.lapse(t)
			f.sleep(suspendPastTheLease)

			if !tc.release(f) {
				t.Fatal("the release was refused after a suspend the engagement recovers from")
			}
			if got := f.claimOutcome(t); got != tc.want {
				t.Errorf("claim outcome = %q, want %q", got, tc.want)
			}
			if _, _, reacquires := f.queue.counts(); reacquires != 1 {
				t.Errorf("re-acquires = %d, want 1", reacquires)
			}
			f.assertRecoveries(t, map[string]int64{suspendRecoveryReacquired: 1})
		})
	}
}

// TestSuspendRecovery_ClaimReleasesRefusedOnALapseWithNoSuspend: the same
// releases on a lapse with no suspend behind it record nothing, and leave the
// claim unreleased for the takeover to release as reaped.
func TestSuspendRecovery_ClaimReleasesRefusedOnALapseWithNoSuspend(t *testing.T) {
	cases := []struct {
		name    string
		release func(f *suspendFixture) bool
	}{
		{"requeue", func(f *suspendFixture) bool {
			return f.s.requeueClaim(f.conv.OrgID, *f.conv, db.RequeueSetupFailure, 0, errors.New("clone failed"), "after a pre-agent failure")
		}},
		{"hand_back", func(f *suspendFixture) bool {
			return !f.s.handBackClaim(context.Background(), liveParkContext{orgID: f.conv.OrgID, conversationID: f.conv.ID, claimID: f.conv.ClaimID}, db.HandBackShutdown, 0, "")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSuspendFixture(t)
			f.register(t, time.Now().Add(-renewedRecently))
			f.lapse(t)

			if tc.release(f) {
				t.Fatal("the release landed on a lapsed lease with no suspend behind it")
			}
			if got := f.claimOutcome(t); got != "" {
				t.Errorf("claim released as %q, want it left unreleased for the takeover", got)
			}
			if _, _, reacquires := f.queue.counts(); reacquires != 0 {
				t.Errorf("re-acquires = %d, want none without a suspend", reacquires)
			}
		})
	}
}

// TestSuspendRecovery_NoSuspendFencesAsToday: the same lapse with no suspend
// on the clock is a lease the engagement really lost, and it fences.
func TestSuspendRecovery_NoSuspendFencesAsToday(t *testing.T) {
	f := newSuspendFixture(t)
	f.s.setClaimLease(20*time.Millisecond, 0, 0)
	f.queue.before = func() { f.lapse(t) }
	f.runLoop(t, time.Now().Add(-renewedRecently))

	waitUntil(t, 5*time.Second, "the fence", func() bool { return f.claimCtx.Err() != nil })
	if cause := context.Cause(f.claimCtx); !errors.Is(cause, errClaimLeaseLost) {
		t.Errorf("fence cause = %v, want errClaimLeaseLost", cause)
	}
	if _, _, reacquires := f.queue.counts(); reacquires != 0 {
		t.Errorf("re-acquires = %d, want none without a suspend", reacquires)
	}
	if f.leaseLive(t) {
		t.Error("the lease came back with no suspend behind the lapse")
	}
	if !hasActiveClaim(t, f.database, f.conv.ID) {
		t.Error("the fence released the claim; releasing it is the executor's, after the engagement returns")
	}
	f.assertRecoveries(t, nil)
}

// TestSuspendRecovery_AlreadyFailingToRenewFences: a suspend on the clock does
// not rescue an engagement whose last accepted renewal is older than the
// self-fence deadline on the monotonic clock. It was failing to renew before
// it slept, and without the sleep its lease would have lapsed anyway.
func TestSuspendRecovery_AlreadyFailingToRenewFences(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-renewedTooLongAgo))
	f.lapse(t)
	f.sleep(suspendPastTheLease)

	sink := newConversationSink(f.s, f.conv.OrgID, f.conv.ID, f.conv.ClaimID, "event", "")
	err := sink.OnMessage(&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: "stale"})
	if !errors.Is(err, db.ErrClaimLeaseExpired) {
		t.Fatalf("OnMessage = %v, want the lapse refused", err)
	}
	if !sink.fenceTripped() {
		t.Error("the sink did not trip its fence on a refusal nothing recovered")
	}
	if _, _, reacquires := f.queue.counts(); reacquires != 0 {
		t.Errorf("re-acquires = %d, want none", reacquires)
	}
	if n := f.transcript(t, "stale"); n != 0 {
		t.Errorf("transcript rows = %d, want nothing written", n)
	}
	if f.leaseLive(t) {
		t.Error("the lease came back for an engagement that was already failing to renew")
	}
	f.assertRecoveries(t, nil)
}

// TestSuspendRecovery_ClaimTakenDuringTheSuspendFences: the takeover that
// releases the claim wins the race with the re-acquire. The re-acquire is
// refused, and the engagement fences with nothing written.
func TestSuspendRecovery_ClaimTakenDuringTheSuspendFences(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-renewedRecently))
	f.lapse(t)
	f.sleep(suspendPastTheLease)
	f.queue.beforeReacquire = func() { f.release(t) }

	sink := newConversationSink(f.s, f.conv.OrgID, f.conv.ID, f.conv.ClaimID, "event", "")
	err := sink.OnMessage(&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: "taken"})
	if !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("OnMessage = %v, want a refusal", err)
	}
	if _, _, reacquires := f.queue.counts(); reacquires != 1 {
		t.Errorf("re-acquires = %d, want the one the takeover beat", reacquires)
	}
	if cause := context.Cause(f.claimCtx); !errors.Is(cause, errClaimLeaseLost) {
		t.Errorf("fence cause = %v, want errClaimLeaseLost", cause)
	}
	if n := f.transcript(t, "taken"); n != 0 {
		t.Errorf("transcript rows = %d, want nothing written", n)
	}
	if hasActiveClaim(t, f.database, f.conv.ID) {
		t.Error("the released claim is live again")
	}
	f.assertRecoveries(t, map[string]int64{suspendRecoveryTaken: 1})
}

// TestSuspendRecovery_ReacquireErrorFences: a re-acquire the database fails
// rather than refuses cannot show the claim is still this engagement's, so it
// fences exactly as a refusal does, and the refused write is not retried.
func TestSuspendRecovery_ReacquireErrorFences(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-renewedRecently))
	f.lapse(t)
	f.sleep(suspendPastTheLease)
	f.queue.reacquireErr = errors.New("database is locked")

	_, err := f.s.conversations.InsertMessageForClaimSystem(context.Background(), f.conv.OrgID, f.conv.ClaimID,
		&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: "unproven"})
	if !errors.Is(err, db.ErrClaimLeaseExpired) {
		t.Fatalf("InsertMessageForClaimSystem = %v, want the original lapse refusal", err)
	}
	if n := f.inserts.calls.Load(); n != 1 {
		t.Errorf("inserts reaching the store = %d, want only the refused one", n)
	}
	if cause := context.Cause(f.claimCtx); !errors.Is(cause, errClaimLeaseLost) {
		t.Errorf("fence cause = %v, want errClaimLeaseLost", cause)
	}
	if f.leaseLive(t) {
		t.Error("the lease came back although the re-acquire failed")
	}
	if !hasActiveClaim(t, f.database, f.conv.ID) {
		t.Error("the claim was released; releasing it is the executor's, after the engagement returns")
	}
	f.assertRecoveries(t, map[string]int64{suspendRecoveryFailed: 1})
}

// TestSuspendRecovery_SelfFencedEngagementDoesNotReacquire: a watchdog that
// fired is final. Recovery against the engagement's own teardown finds a
// claim context already cancelled with a lease cause and leaves the lapse.
func TestSuspendRecovery_SelfFencedEngagementDoesNotReacquire(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-renewedRecently))
	f.fence(errClaimSelfFenced)
	f.lapse(t)
	f.sleep(suspendPastTheLease)

	_, err := f.s.conversations.InsertMessageForClaimSystem(context.Background(), f.conv.OrgID, f.conv.ClaimID,
		&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: "fenced"})
	if !errors.Is(err, db.ErrClaimLeaseExpired) {
		t.Fatalf("InsertMessageForClaimSystem = %v, want the lapse refused", err)
	}
	if _, _, reacquires := f.queue.counts(); reacquires != 0 {
		t.Errorf("re-acquires = %d, want none for a self-fenced engagement", reacquires)
	}
	if f.leaseLive(t) {
		t.Error("the lease came back for a self-fenced engagement")
	}
	f.assertRecoveries(t, nil)
}

// TestSuspendRecovery_StoppedEngagementStillReacquires: a stop is not a lease
// cause. A stopped engagement settles through a fenced park, and the park has
// to land after a suspend like any other write.
func TestSuspendRecovery_StoppedEngagementStillReacquires(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-renewedRecently))
	f.fence(errStopRequested)
	f.lapse(t)
	f.sleep(suspendPastTheLease)

	parked, err := f.s.conversations.ParkOpenForClaimSystem(context.Background(), f.conv.OrgID, f.conv.ID, f.conv.ClaimID,
		db.ParkStopped(domain.ParkReasonUserCancelled, "Stopped by user"))
	if err != nil || !parked {
		t.Fatalf("ParkOpenForClaimSystem after a suspend = (%v, %v), want the park to land", parked, err)
	}
	if _, _, reacquires := f.queue.counts(); reacquires != 1 {
		t.Errorf("re-acquires = %d, want 1", reacquires)
	}
	if got := storedStatus(t, f.database, f.conv.ID); got != "open" {
		t.Errorf("stored status = %q, want open", got)
	}
}

// TestSuspendRecovery_WriteThatReacquiresStopsOnAPendingStop: a stop requested
// while the machine slept is read back by the re-acquire, whichever of the
// engagement's calls makes it. When a write makes it, the write still lands,
// and the engagement stops exactly as a renewal that read the stop would
// stop it.
func TestSuspendRecovery_WriteThatReacquiresStopsOnAPendingStop(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-renewedRecently))
	f.lapse(t)
	f.sleep(suspendPastTheLease)
	if _, err := f.database.Exec(
		`UPDATE conversations SET stop_requested_at = CURRENT_TIMESTAMP, stop_requested_by = ? WHERE id = ?`,
		"user-who-stopped", f.conv.ID,
	); err != nil {
		t.Fatalf("request a stop: %v", err)
	}

	sink := newConversationSink(f.s, f.conv.OrgID, f.conv.ID, f.conv.ClaimID, "event", "")
	if err := sink.OnMessage(&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: "after the wake"}); err != nil {
		t.Fatalf("OnMessage after a suspend: %v", err)
	}
	if n := f.transcript(t, "after the wake"); n != 1 {
		t.Errorf("transcript rows = %d, want exactly 1", n)
	}
	if _, _, reacquires := f.queue.counts(); reacquires != 1 {
		t.Errorf("re-acquires = %d, want 1", reacquires)
	}
	if cause := context.Cause(f.claimCtx); !errors.Is(cause, errStopRequested) {
		t.Errorf("claim context cause = %v, want errStopRequested", cause)
	}
	// The stop settles through the engagement's fenced park, which needs the
	// lease the re-acquire took back.
	if !f.leaseLive(t) {
		t.Error("the claim's lease is not live after the re-acquire")
	}
}

// TestSuspendRecovery_TwoRefusalsOneReacquire: the renewal and a sink write
// meeting the same lapse spend one re-acquire, and both proceed on it.
func TestSuspendRecovery_TwoRefusalsOneReacquire(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-renewedRecently))
	f.lapse(t)
	f.sleep(suspendPastTheLease)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, content := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.s.conversations.InsertMessageForClaimSystem(context.Background(), f.conv.OrgID, f.conv.ClaimID,
				&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: content})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("write %d after a suspend: %v", i, err)
		}
	}
	if _, _, reacquires := f.queue.counts(); reacquires != 1 {
		t.Errorf("re-acquires = %d, want one for one suspend", reacquires)
	}
	for _, content := range []string{"first", "second"} {
		if n := f.transcript(t, content); n != 1 {
			t.Errorf("transcript rows for %q = %d, want exactly 1", content, n)
		}
	}
	f.assertRecoveries(t, map[string]int64{suspendRecoveryReacquired: 1})
}

// TestSuspendRecovery_SecondSuspendReacquiresAgain: an engagement that wakes,
// takes its claim back, and sleeps again before anything else renews (a macOS
// dark wake does this) recovers the second lapse too. The second refusal is
// issued after the first re-acquire, so it must not be read as one that
// re-acquire already answered.
func TestSuspendRecovery_SecondSuspendReacquiresAgain(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-renewedRecently))
	contents := []string{"after the first sleep", "after the second sleep"}
	for i, content := range contents {
		f.lapse(t)
		f.sleep(suspendPastTheLease)
		if _, err := f.s.conversations.InsertMessageForClaimSystem(context.Background(), f.conv.OrgID, f.conv.ClaimID,
			&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: content}); err != nil {
			t.Fatalf("write after sleep %d: %v", i+1, err)
		}
	}
	if _, _, reacquires := f.queue.counts(); reacquires != 2 {
		t.Errorf("re-acquires = %d, want one per sleep", reacquires)
	}
	for _, content := range contents {
		if n := f.transcript(t, content); n != 1 {
			t.Errorf("transcript rows for %q = %d, want exactly 1", content, n)
		}
	}
	if err := context.Cause(f.claimCtx); err != nil {
		t.Errorf("claim context cancelled with %v; a recovered engagement carries on", err)
	}
	if !f.leaseLive(t) {
		t.Error("the claim's lease is not live after the second re-acquire")
	}
	f.assertRecoveries(t, map[string]int64{suspendRecoveryReacquired: 2})
}

// TestSuspendRecovery_StaleRenewalDoesNotRewindTheBase: a renewal issued
// before the sleep that comes back accepted after a write's recovery took the
// claim back is older news. Recording it would rewind the suspend base to
// before the sleep, and a later lapse with no suspend behind it (a wall-clock
// step on the database's clock) would count that sleep again.
func TestSuspendRecovery_StaleRenewalDoesNotRewindTheBase(t *testing.T) {
	f := newSuspendFixture(t)
	st := f.register(t, time.Now().Add(-renewedRecently))
	issuedBeforeTheSleep, baseBeforeTheSleep := time.Now(), time.Duration(f.asleep.Load())
	f.lapse(t)
	f.sleep(suspendPastTheLease)
	if _, err := f.s.conversations.InsertMessageForClaimSystem(context.Background(), f.conv.OrgID, f.conv.ClaimID,
		&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: "after the sleep"}); err != nil {
		t.Fatalf("write after the sleep: %v", err)
	}
	// The renewal that was out across the sleep returns, accepted.
	st.accepted(issuedBeforeTheSleep, baseBeforeTheSleep, true)

	f.lapse(t)
	_, err := f.s.conversations.InsertMessageForClaimSystem(context.Background(), f.conv.OrgID, f.conv.ClaimID,
		&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: "after the step"})
	if !errors.Is(err, db.ErrClaimLeaseExpired) {
		t.Fatalf("write after a lapse with no suspend = %v, want the lapse refused", err)
	}
	if _, _, reacquires := f.queue.counts(); reacquires != 1 {
		t.Errorf("re-acquires = %d, want only the sleep's", reacquires)
	}
	f.assertRecoveries(t, map[string]int64{suspendRecoveryReacquired: 1})
}

// TestSuspendRecovery_AbandonedReacquireDoesNotFence: a write whose caller
// gives up while its re-acquire is out learns nothing about the claim, so it
// neither fences the engagement nor counts an attempt, and the next write
// recovers.
func TestSuspendRecovery_AbandonedReacquireDoesNotFence(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-renewedRecently))
	f.lapse(t)
	f.sleep(suspendPastTheLease)
	ctx, abandon := context.WithCancel(context.Background())
	f.queue.beforeReacquire = abandon

	_, err := f.s.conversations.InsertMessageForClaimSystem(ctx, f.conv.OrgID, f.conv.ClaimID,
		&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: "abandoned"})
	if !errors.Is(err, db.ErrClaimLeaseExpired) {
		t.Fatalf("abandoned write = %v, want the lapse it met", err)
	}
	if err := context.Cause(f.claimCtx); err != nil {
		t.Errorf("claim context cancelled with %v; an abandoned re-acquire fences nothing", err)
	}
	f.assertRecoveries(t, nil)

	f.queue.mu.Lock()
	f.queue.beforeReacquire = nil
	f.queue.mu.Unlock()
	if _, err := f.s.conversations.InsertMessageForClaimSystem(context.Background(), f.conv.OrgID, f.conv.ClaimID,
		&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: "after"}); err != nil {
		t.Fatalf("the next write after an abandoned re-acquire: %v", err)
	}
	if !f.leaseLive(t) {
		t.Error("the claim's lease is not live after the second write's re-acquire")
	}
	f.assertRecoveries(t, map[string]int64{suspendRecoveryReacquired: 1})
}

// TestSuspendRecovery_NoSuspendClockFencesAsToday: on a platform that cannot
// report suspended time, no lapse is recoverable, so a refused renewal fences
// exactly as it did before the recovery existed.
func TestSuspendRecovery_NoSuspendClockFencesAsToday(t *testing.T) {
	f := newSuspendFixture(t)
	suspendclock.SetSourceForTest(t, func() (time.Duration, bool) { return 0, false })
	f.s.setClaimLease(20*time.Millisecond, 0, 0)
	f.queue.before = func() { f.lapse(t) }
	f.runLoop(t, time.Now().Add(-renewedRecently))

	waitUntil(t, 5*time.Second, "the fence", func() bool { return f.claimCtx.Err() != nil })
	if cause := context.Cause(f.claimCtx); !errors.Is(cause, errClaimLeaseLost) {
		t.Errorf("fence cause = %v, want errClaimLeaseLost", cause)
	}
	if _, _, reacquires := f.queue.counts(); reacquires != 0 {
		t.Errorf("re-acquires = %d, want none without a suspend clock", reacquires)
	}
	f.assertRecoveries(t, nil)
}

// TestSuspendRecovery_PollRenewsOnWake: the suspend poll renews as soon as the
// clock moves, taking the claim back well before the next cadence tick would
// have asked.
func TestSuspendRecovery_PollRenewsOnWake(t *testing.T) {
	f := newSuspendFixture(t)
	// The product's timings, so the cadence tick is a full interval out.
	f.runLoop(t, time.Now())
	waitUntil(t, 5*time.Second, "the loop to register its lease", func() bool {
		return f.s.claimLeaseFor(f.conv.ClaimID) != nil
	})

	f.lapse(t)
	f.sleep(suspendPastTheLease)
	// The cadence tick is a full interval out, so a re-acquire inside half of
	// one is the poll's. The poll takes about a second; the margin is for a
	// loaded runner.
	waitUntil(t, DefaultClaimRenewInterval/2, "the wake renewal", func() bool {
		_, _, reacquires := f.queue.counts()
		return reacquires == 1
	})
	if _, refused, _ := f.queue.counts(); refused != 1 {
		t.Errorf("refused renewals = %d, want the one wake renewal", refused)
	}
	if err := context.Cause(f.claimCtx); err != nil {
		t.Errorf("claim context cancelled with %v", err)
	}
	if !f.leaseLive(t) {
		t.Error("the claim's lease is not live after the wake")
	}
}
