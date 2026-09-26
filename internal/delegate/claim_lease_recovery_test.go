package delegate

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
// runs at the top of every re-acquire, for the same reason.
type recordingQueue struct {
	db.ConversationQueueStore

	mu              sync.Mutex
	renewed         int
	refused         []error
	reacquires      int
	before          func()
	beforeReacquire func()
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
	before := q.beforeReacquire
	q.mu.Unlock()
	if before != nil {
		before()
	}
	return q.ConversationQueueStore.ReacquireClaimLeaseSystem(ctx, orgID, conversationID, claimID, executorID, bootEpoch, lease)
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
}

func newSuspendFixture(t *testing.T) *suspendFixture {
	t.Helper()
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	const convID = "conv-suspend"
	seedConversation(t, database, convID, "sess-suspend", "/tmp/wt-suspend")
	claimID := markEngaged(t, database, convID)

	f := &suspendFixture{database: database}
	suspendclock.SetSourceForTest(t, func() (time.Duration, bool) {
		return time.Duration(f.asleep.Load()), true
	})

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
// two-minute suspend lapsed the lease, from an engagement that renewed ten
// seconds before it slept, takes the claim back and carries on.
func TestSuspendRecovery_RefusedRenewalReacquires(t *testing.T) {
	f := newSuspendFixture(t)
	f.s.setClaimLease(20*time.Millisecond, 0, 0)
	// The machine sleeps as the first renewal goes out.
	f.queue.before = func() {
		f.lapse(t)
		f.sleep(120 * time.Second)
	}
	f.runLoop(t, time.Now().Add(-10*time.Second))

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
}

// TestSuspendRecovery_SinkWriteRetriedThroughTheWrapper: an SDK sink write
// that meets the lapse first — before the loop has noticed the wake — takes
// the claim back through the store wrapper, is retried once, and lands once.
func TestSuspendRecovery_SinkWriteRetriedThroughTheWrapper(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-10*time.Second))
	f.lapse(t)
	f.sleep(120 * time.Second)

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
}

// TestSuspendRecovery_NoSuspendFencesAsToday: the same lapse with no suspend
// on the clock is a lease the engagement really lost, and it fences.
func TestSuspendRecovery_NoSuspendFencesAsToday(t *testing.T) {
	f := newSuspendFixture(t)
	f.s.setClaimLease(20*time.Millisecond, 0, 0)
	f.queue.before = func() { f.lapse(t) }
	f.runLoop(t, time.Now().Add(-10*time.Second))

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
}

// TestSuspendRecovery_AlreadyFailingToRenewFences: a suspend on the clock does
// not rescue an engagement whose last accepted renewal is older than the
// self-fence deadline on the monotonic clock. It was failing to renew before
// it slept, and without the sleep its lease would have lapsed anyway.
func TestSuspendRecovery_AlreadyFailingToRenewFences(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-50*time.Second))
	f.lapse(t)
	f.sleep(120 * time.Second)

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
}

// TestSuspendRecovery_ClaimTakenDuringTheSuspendFences: the takeover that
// releases the claim wins the race with the re-acquire. The re-acquire is
// refused, and the engagement fences with nothing written.
func TestSuspendRecovery_ClaimTakenDuringTheSuspendFences(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-10*time.Second))
	f.lapse(t)
	f.sleep(120 * time.Second)
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
}

// TestSuspendRecovery_SelfFencedEngagementDoesNotReacquire: a watchdog that
// fired is final. Recovery against the engagement's own teardown finds a
// claim context already cancelled with a lease cause and leaves the lapse.
func TestSuspendRecovery_SelfFencedEngagementDoesNotReacquire(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-10*time.Second))
	f.fence(errClaimSelfFenced)
	f.lapse(t)
	f.sleep(120 * time.Second)

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
}

// TestSuspendRecovery_StoppedEngagementStillReacquires: a stop is not a lease
// cause. A stopped engagement settles through a fenced park, and the park has
// to land after a suspend like any other write.
func TestSuspendRecovery_StoppedEngagementStillReacquires(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-10*time.Second))
	f.fence(errStopRequested)
	f.lapse(t)
	f.sleep(120 * time.Second)

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

// TestSuspendRecovery_TwoRefusalsOneReacquire: the renewal and a sink write
// meeting the same lapse spend one re-acquire, and both proceed on it.
func TestSuspendRecovery_TwoRefusalsOneReacquire(t *testing.T) {
	f := newSuspendFixture(t)
	f.register(t, time.Now().Add(-10*time.Second))
	f.lapse(t)
	f.sleep(120 * time.Second)

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
}

// TestSuspendRecovery_PollRenewsOnWake: the suspend poll renews as soon as the
// clock moves, taking the claim back well before the next cadence tick would
// have asked.
func TestSuspendRecovery_PollRenewsOnWake(t *testing.T) {
	f := newSuspendFixture(t)
	// The product's timings: the cadence tick is twenty seconds out, so a
	// renewal inside the next few is the poll's.
	f.runLoop(t, time.Now())
	waitUntil(t, 5*time.Second, "the loop to register its lease", func() bool {
		return f.s.claimLeaseFor(f.conv.ClaimID) != nil
	})

	f.lapse(t)
	f.sleep(120 * time.Second)
	woke := time.Now()
	waitUntil(t, 3*suspendPollInterval, "the wake renewal", func() bool {
		_, _, reacquires := f.queue.counts()
		return reacquires == 1
	})
	if took := time.Since(woke); took > 2*suspendPollInterval {
		t.Errorf("re-acquired %s after the wake, want within about one poll", took)
	}
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
