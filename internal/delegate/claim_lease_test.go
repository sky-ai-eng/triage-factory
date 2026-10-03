package delegate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// renewalRecord is one call the loop made, as the fake saw it.
type renewalRecord struct {
	orgID, conversationID, claimID string
	lease                          time.Duration
	at                             time.Time
	idle                           time.Duration
	op                             string
	checkpointAge                  *time.Duration
}

// fakeRenewalStore stands in for the conversation queue on the one verb the
// renewal loop calls. Everything else delegates, so a fixture's real store
// still answers the dispatcher.
type fakeRenewalStore struct {
	db.ConversationQueueStore

	mu    sync.Mutex
	calls []renewalRecord

	// err, when set, is what every renewal returns once refuseAfter has
	// opened (immediately, when it is nil).
	err error
	// refuseAfter gates err: renewals succeed until it is closed. It exists so
	// a test can decide WHERE in the engagement the lease is lost, rather than
	// leaving it to whichever of the ticker and the bring-up wins a race.
	refuseAfter <-chan struct{}
	// block, when non-nil, is what the call waits on INSTEAD of honouring its
	// context — the driver that ignores its deadline, which is exactly the
	// failure the watchdog's own timer exists for.
	block chan struct{}
	// stop, when set, is the stop intent every successful renewal reads back
	// off the conversation.
	stop *db.ClaimRenewal
}

func (f *fakeRenewalStore) RenewClaimLeaseSystem(ctx context.Context, orgID, conversationID, claimID string, lease time.Duration, activity db.ClaimActivity) (db.ClaimRenewal, error) {
	f.mu.Lock()
	f.calls = append(f.calls, renewalRecord{orgID, conversationID, claimID, lease, time.Now(), activity.Idle, activity.Op, activity.CheckpointAge})
	block, err, gate, stop := f.block, f.err, f.refuseAfter, f.stop
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if gate != nil {
		select {
		case <-gate:
		default:
			err = nil // not yet: this renewal succeeds
		}
	}
	if err != nil {
		return db.ClaimRenewal{}, err
	}
	out := db.ClaimRenewal{ExpiresAt: time.Now().Add(lease)}
	if stop != nil {
		out.StopRequested, out.StopRequestedBy = stop.StopRequested, stop.StopRequestedBy
	}
	return out, nil
}

func (f *fakeRenewalStore) seen() []renewalRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]renewalRecord(nil), f.calls...)
}

// leaseTestSpawner is a spawner wired to nothing but the fake, for the loop's
// own tests: renewClaimLease touches the queue store and the clock and
// nothing else.
func leaseTestSpawner(t *testing.T, fake *fakeRenewalStore, renew, selfFence, lease time.Duration) *Spawner {
	t.Helper()
	s := NewSpawner(nil, db.Stores{ConversationQueue: fake}, nil, nil, "")
	s.setClaimLease(renew, selfFence, lease)
	return s
}

func leaseTestConversation() *domain.Conversation {
	return &domain.Conversation{
		ID: "conv-lease", OrgID: runmode.LocalDefaultOrgID, ClaimID: "claim-lease",
	}
}

// TestClaimLeaseTimings holds the ordering the three constants have to keep:
// a holder must have fenced itself before the lease it can no longer prove
// lapses, and it must have tried to renew well before that. Nothing reads
// these from the environment, so this is where a change to one of them
// without the others is caught.
func TestClaimLeaseTimings(t *testing.T) {
	if DefaultClaimRenewInterval >= DefaultClaimSelfFenceDeadline {
		t.Errorf("renew interval %s does not leave room for a retry before the self-fence at %s",
			DefaultClaimRenewInterval, DefaultClaimSelfFenceDeadline)
	}
	if DefaultClaimSelfFenceDeadline >= DefaultClaimLease {
		t.Errorf("self-fence at %s lands at or after the lease expiry at %s, so a successor could take a conversation its holder is still driving",
			DefaultClaimSelfFenceDeadline, DefaultClaimLease)
	}
}

// TestRenewClaimLease_RenewsAtTheCadence pins what the loop actually sends:
// this claim's id, in this claim's org, for the configured lease — on the
// configured tick.
func TestRenewClaimLease_RenewsAtTheCadence(t *testing.T) {
	fake := &fakeRenewalStore{}
	s := leaseTestSpawner(t, fake, 20*time.Millisecond, time.Second, 90*time.Second)
	conv := leaseTestConversation()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.renewClaimLease(ctx, conv, time.Now(), context.Background(), func(error) {})
	}()

	deadline := time.After(5 * time.Second)
	for len(fake.seen()) < 3 {
		select {
		case <-deadline:
			t.Fatalf("the loop made %d renewals in 5s at a 20ms cadence", len(fake.seen()))
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done

	for i, c := range fake.seen() {
		if c.orgID != conv.OrgID || c.conversationID != conv.ID || c.claimID != conv.ClaimID {
			t.Errorf("renewal %d named (%s, %s, %s), want this engagement's own claim", i, c.orgID, c.conversationID, c.claimID)
		}
		if c.lease != 90*time.Second {
			t.Errorf("renewal %d asked for a %s lease, want the configured 90s", i, c.lease)
		}
	}
}

// TestRenewClaimLease_LostLeaseFencesOnce pins the terminal answer: a refused
// renewal fences with errClaimLeaseLost and the loop stops. Retrying would be
// asking again for authority the database has already said is gone.
func TestRenewClaimLease_LostLeaseFencesOnce(t *testing.T) {
	fake := &fakeRenewalStore{err: db.ErrClaimReleased}
	s := leaseTestSpawner(t, fake, 10*time.Millisecond, time.Minute, 90*time.Second)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan struct{})
	go func() { defer close(done); s.renewClaimLease(ctx, leaseTestConversation(), time.Now(), ctx, cancel) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop kept ticking after the database refused its renewal")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, errClaimLeaseLost) {
		t.Fatalf("fence cause = %v, want errClaimLeaseLost", cause)
	}
	if !leaseFenced(ctx) {
		t.Error("leaseFenced does not recognise a lost lease")
	}
	if n := len(fake.seen()); n != 1 {
		t.Errorf("%d renewals after a refusal, want exactly the one that was refused", n)
	}
}

// TestRenewClaimLease_WatchdogFiresThroughABlockedCall is the failure the
// watchdog exists for: a renewal that ignores its context and never returns.
// A deadline check sharing the loop's goroutine would never run; the runtime's
// timer fires regardless.
func TestRenewClaimLease_WatchdogFiresThroughABlockedCall(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	fake := &fakeRenewalStore{block: block}
	s := leaseTestSpawner(t, fake, 10*time.Millisecond, 150*time.Millisecond, 90*time.Second)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go s.renewClaimLease(ctx, leaseTestConversation(), time.Now(), ctx, cancel)

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog never fired while a renewal sat blocked past its deadline")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, errClaimSelfFenced) {
		t.Fatalf("fence cause = %v, want errClaimSelfFenced", cause)
	}
	if !leaseFenced(ctx) {
		t.Error("leaseFenced does not recognise a self-fence")
	}
}

// TestRenewClaimLease_SelfFenceEndsTheLoop pins the loser contract's other
// half: once the watchdog has fenced, the loop stops asking. The database
// coming back a moment later must not extend the lease of an engagement that
// is tearing down and will write nothing.
func TestRenewClaimLease_SelfFenceEndsTheLoop(t *testing.T) {
	block := make(chan struct{})
	fake := &fakeRenewalStore{block: block}
	s := leaseTestSpawner(t, fake, 10*time.Millisecond, 150*time.Millisecond, 90*time.Second)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.renewClaimLease(context.Background(), leaseTestConversation(), time.Now(), ctx, cancel)
	}()

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog never fired while a renewal sat blocked past its deadline")
	}
	// The database answers again: the blocked call returns success.
	close(block)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop kept renewing after its watchdog had fenced the engagement")
	}
	if n := len(fake.seen()); n != 1 {
		t.Errorf("%d renewals, want only the one that was in flight when the watchdog fired", n)
	}
}

// TestRenewClaimLease_KeepsRenewingPastTheLease is the credentials wait: an
// engagement parked awaiting its bundle outlives its own lease many times
// over, and stays alive because each successful renewal re-arms the watchdog.
//
// The three timings are picked against each other rather than for speed. The
// self-fence is 15 cadences, so a false fence needs fifteen consecutive
// missed ticks — a loaded machine stalling this goroutine for a few tens of
// milliseconds cannot produce one, which a tighter margin could. The
// observation window is several times the self-fence, so the test is still
// sharp: a loop that stopped re-arming would fence well inside it rather than
// after it, which is what would make this pass vacuously.
func TestRenewClaimLease_KeepsRenewingPastTheLease(t *testing.T) {
	fake := &fakeRenewalStore{}
	const (
		cadence   = 20 * time.Millisecond
		selfFence = 15 * cadence
		lease     = 50 * time.Millisecond // inert here: the loop only passes it on
		observe   = 5 * selfFence
	)
	if observe <= selfFence {
		t.Fatal("the observation window must outlast the self-fence, or a broken re-arm goes unnoticed and this passes vacuously")
	}
	s := leaseTestSpawner(t, fake, cadence, selfFence, lease)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go s.renewClaimLease(ctx, leaseTestConversation(), time.Now(), ctx, cancel)

	select {
	case <-ctx.Done():
		t.Fatalf("the engagement was fenced while its renewals were succeeding: %v", context.Cause(ctx))
	case <-time.After(observe):
	}
	// Still ticking, not merely un-fenced: a loop that stopped renewing
	// altogether would also never fence, and that is not the same thing.
	if n := len(fake.seen()); n < int(observe/cadence)/3 {
		t.Errorf("%d renewals across %s at a %s cadence; the loop stopped ticking", n, observe, cadence)
	}
}

// TestRenewClaimLease_StopsWithTheEngagement pins the exit: the loop returns
// when its context is cancelled and a renewal in flight at that moment neither
// panics nor fences anything.
func TestRenewClaimLease_StopsWithTheEngagement(t *testing.T) {
	block := make(chan struct{})
	fake := &fakeRenewalStore{block: block}
	s := leaseTestSpawner(t, fake, 10*time.Millisecond, time.Minute, 90*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	fenced := make(chan error, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.renewClaimLease(ctx, leaseTestConversation(), time.Now(), context.Background(), func(err error) { fenced <- err })
	}()

	for len(fake.seen()) == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()     // the engagement returned; its deferred stop cancels the loop
	close(block) // the in-flight renewal comes back into a cancelled loop

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop outlived its engagement")
	}
	select {
	case err := <-fenced:
		t.Fatalf("the loop fenced on its way out: %v", err)
	default:
	}
}

// TestDispatch_LostLeaseFencesTheEngagementWithoutWriting is the whole point,
// end to end: an engagement whose lease is refused mid-bring-up kills its own
// setup and records nothing. Not a park, not a terminal — the conversation's
// disposition belongs to whoever takes it over.
//
// It is deliberately the same shape as the user-stop test beside it, and the
// lease is lost at the same MOMENT that test's stop lands — while the first
// GitHub request is held open — because the distinction being pinned is
// exactly that the two must not end the same way from the same place: a stop
// parks with user_cancelled and releases the claim, a lease fence writes
// nothing at all.
//
// Gating the refusal on that moment is not convenience. Left on the ticker
// alone, the fence races the bring-up: win it and the engagement exits from a
// setup call, lose it and every best-effort read ahead of the runtime swallows
// the cancellation as a warning and the engagement wanders on to code this
// test is not about. Which exit ran would then be decided by how loaded the
// machine is.
func TestDispatch_LostLeaseFencesTheEngagementWithoutWriting(t *testing.T) {
	fx := newLaunchFixtureWithWorktree(t, "1033", "")

	fetchEntered := make(chan struct{})
	var enterOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enterOnce.Do(func() { close(fetchEntered) })
		<-r.Context().Done()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	fx.s.SetRunCredentialResolvers(bringUpResolver{client: ghclient.NewClient(server.URL, "test-token")}, nil, nil)

	// Renewals succeed until bring-up is inside the held-open fetch, then the
	// next tick is refused. The cadence is short so that tick lands while the
	// request is still in flight.
	fake := &fakeRenewalStore{
		ConversationQueueStore: fx.stores.ConversationQueue,
		err:                    db.ErrClaimReleased,
		refuseAfter:            fetchEntered,
	}
	fx.s.conversationQueue = fake
	fx.s.setClaimLease(20*time.Millisecond, 10*time.Second, 30*time.Second)

	conv := fx.conv
	conv.OrgID = runmode.LocalDefaultOrgID
	dispatched := make(chan struct{})
	go func() {
		defer close(dispatched)
		fx.s.dispatchClaimedConversation(context.Background(), &conv, time.Now())
	}()

	select {
	case <-fetchEntered:
	case <-time.After(15 * time.Second):
		t.Fatal("bring-up never reached the PR fetch")
	}
	select {
	case <-dispatched:
	case <-time.After(15 * time.Second):
		t.Fatal("the engagement was never fenced by its lost lease")
	}

	if n := len(fake.seen()); n == 0 {
		t.Fatal("the renewal loop never ran; nothing could have fenced the engagement")
	}
	if got := fx.storedStatus(t); got != "" {
		t.Errorf("stored status = %q, want nothing written: a fenced engagement records no disposition", got)
	}
	if got := fx.claimOutcomes(t); len(got) != 1 || got[0] != "" {
		t.Errorf("claim outcomes = %v, want the one claim left unreleased — releasing it is not this engagement's to do", got)
	}
	if got := fx.blueprintStatus(t); got != "running" {
		t.Errorf("blueprint status = %q, want running (a fenced engagement touches nothing)", got)
	}
	if rows := fx.transcript(t); len(rows) != 0 {
		t.Errorf("transcript = %+v, want nothing written", rows)
	}
}

// TestRenewClaimLease_PendingStopCancelsAndKeepsRenewing: a renewal that
// reads a pending stop cancels the claim context with errStopRequested — a
// stop, not a lease fence, so the engagement settles it as a deliberate park —
// and keeps renewing, because that park needs the lease to still be live when
// it lands.
func TestRenewClaimLease_PendingStopCancelsAndKeepsRenewing(t *testing.T) {
	fake := &fakeRenewalStore{stop: &db.ClaimRenewal{StopRequested: true, StopRequestedBy: "user-x"}}
	s := leaseTestSpawner(t, fake, 10*time.Millisecond, time.Minute, 90*time.Second)

	claimCtx, fence := context.WithCancelCause(context.Background())
	defer fence(nil)
	loopCtx, stopLoop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.renewClaimLease(loopCtx, leaseTestConversation(), time.Now(), claimCtx, fence)
	}()

	select {
	case <-claimCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a renewal reading a pending stop never cancelled the claim context")
	}
	if cause := context.Cause(claimCtx); !errors.Is(cause, errStopRequested) {
		t.Fatalf("cause = %v, want errStopRequested", cause)
	}
	if leaseFenced(claimCtx) {
		t.Error("leaseFenced reads a stop as a lease fence; the engagement would write nothing instead of settling it")
	}

	seen := len(fake.seen())
	deadline := time.Now().Add(5 * time.Second)
	for len(fake.seen()) < seen+3 {
		if time.Now().After(deadline) {
			t.Fatal("the loop stopped renewing after the stop; the settling park would find its lease lapsed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stopLoop()
	<-done
}

// TestReleaseOwnExpiredClaims_ReleasesOnlyWhatNoEngagementDrives pins the
// release that ends a fenced engagement's hold. A fenced engagement writes
// nothing, so its claim outlives it; the executor that minted the claim
// releases it once the lease has lapsed, but never while an engagement in
// this process is still on the conversation, which may be a fenced one
// still tearing down its cell. A stop pending on a released row is then
// settled by the ordinary settlement.
func TestReleaseOwnExpiredClaims_ReleasesOnlyWhatNoEngagementDrives(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	const tearingDown, finished = "r-expired-live", "r-expired-done"
	seedConversation(t, database, tearingDown, "sess-live", "/tmp/wt-live")
	seedConversation(t, database, finished, "sess-done", "/tmp/wt-done")
	markEngaged(t, database, tearingDown)
	markEngaged(t, database, finished)
	if _, err := database.Exec(
		`UPDATE claims SET lease_expires_at = strftime('%Y-%m-%d %H:%M:%f','now','-60.000 seconds') WHERE released_at IS NULL`,
	); err != nil {
		t.Fatalf("lapse the leases: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE conversations SET stop_requested_at = CURRENT_TIMESTAMP, stop_requested_by = ? WHERE id = ?`,
		runmode.LocalDefaultUserID, finished,
	); err != nil {
		t.Fatalf("stage a pending stop: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	s.SetExecutorID("test-engagement", 1)
	s.mu.Lock()
	s.engagements[tearingDown] = &engagement{}
	s.mu.Unlock()

	s.releaseOwnExpiredClaims(context.Background())
	s.settleUnclaimedStops(context.Background())

	if !hasActiveClaim(t, database, tearingDown) {
		t.Error("released the claim of a conversation an engagement in this process still drives")
	}
	if hasActiveClaim(t, database, finished) {
		t.Fatal("the expired claim no engagement drives is still held; nothing else releases it in local mode")
	}
	var outcome string
	if err := database.QueryRow(`SELECT outcome FROM claims WHERE conversation_id = ?`, finished).Scan(&outcome); err != nil {
		t.Fatalf("read the released claim: %v", err)
	}
	if outcome != "reaped" {
		t.Errorf("released claim outcome = %q, want reaped — a lost engagement counts toward the claim budget", outcome)
	}
	if got := storedStatus(t, database, finished); got != "open" {
		t.Errorf("status after the release and settlement = %q, want open — the pending stop settles once the claim is gone", got)
	}
}

// TestStopOutcome_NamesEachCancellation pins the trace label for each way an
// engagement's context ends before the agent is live, including the resume
// path's shape, whose parent is the claim context itself.
func TestStopOutcome_NamesEachCancellation(t *testing.T) {
	cancelledWith := func(parent context.Context, cause error) context.Context {
		ctx, cancel := context.WithCancelCause(parent)
		cancel(cause)
		return ctx
	}
	live := context.Background()
	shutdown := cancelledWith(live, context.Canceled)
	fenced := cancelledWith(live, errClaimSelfFenced)
	renewalStop := cancelledWith(live, errStopRequested)

	for name, tc := range map[string]struct {
		parent, step context.Context
		want         string
	}{
		"dispatcher shutdown":        {shutdown, cancelledWith(shutdown, context.Canceled), engagementShutdown},
		"local stop":                 {live, cancelledWith(live, context.Canceled), engagementCancelled},
		"lease fence":                {fenced, cancelledWith(fenced, context.Canceled), engagementFenced},
		"stop delivered by renewal":  {renewalStop, cancelledWith(renewalStop, context.Canceled), engagementCancelled},
		"lease fence on step itself": {live, cancelledWith(live, errClaimLeaseLost), engagementFenced},
	} {
		got, ok := stopOutcome(tc.parent, tc.step)
		if !ok || got != tc.want {
			t.Errorf("%s: stopOutcome = (%q, %v), want (%q, true)", name, got, ok, tc.want)
		}
	}
	if _, ok := stopOutcome(live, live); ok {
		t.Error("stopOutcome named an outcome for a context nobody cancelled")
	}
}
