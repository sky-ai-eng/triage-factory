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
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// renewalRecord is one call the loop made, as the fake saw it.
type renewalRecord struct {
	orgID, conversationID, claimID string
	lease                          time.Duration
	at                             time.Time
}

// fakeRenewalStore stands in for the conversation queue on the one verb the
// renewal loop calls. Everything else delegates, so a fixture's real store
// still answers the dispatcher.
type fakeRenewalStore struct {
	db.ConversationQueueStore

	mu    sync.Mutex
	calls []renewalRecord

	// err, when set, is what every renewal returns.
	err error
	// block, when non-nil, is what the call waits on INSTEAD of honouring its
	// context — the driver that ignores its deadline, which is exactly the
	// failure the watchdog's own timer exists for.
	block chan struct{}
}

func (f *fakeRenewalStore) RenewClaimLeaseSystem(ctx context.Context, orgID, conversationID, claimID string, lease time.Duration) (time.Time, error) {
	f.mu.Lock()
	f.calls = append(f.calls, renewalRecord{orgID, conversationID, claimID, lease, time.Now()})
	block, err := f.block, f.err
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Now().Add(lease), nil
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
	s.SetClaimLease(renew, selfFence, lease)
	return s
}

func leaseTestConversation() *domain.Conversation {
	return &domain.Conversation{
		ID: "conv-lease", OrgID: runmode.LocalDefaultOrgID, ClaimID: "claim-lease",
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
	go func() { defer close(done); s.renewClaimLease(ctx, conv, time.Now(), func(error) {}) }()

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
	go func() { defer close(done); s.renewClaimLease(ctx, leaseTestConversation(), time.Now(), cancel) }()

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
	go s.renewClaimLease(ctx, leaseTestConversation(), time.Now(), cancel)

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

// TestRenewClaimLease_KeepsRenewingPastTheLease is the credentials wait: an
// engagement parked awaiting its bundle outlives its own lease several times
// over, and stays alive because the loop started before the wait did.
func TestRenewClaimLease_KeepsRenewingPastTheLease(t *testing.T) {
	fake := &fakeRenewalStore{}
	const lease = 60 * time.Millisecond
	s := leaseTestSpawner(t, fake, 10*time.Millisecond, 40*time.Millisecond, lease)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go s.renewClaimLease(ctx, leaseTestConversation(), time.Now(), cancel)

	// Four lease-lengths of waiting. A loop whose watchdog were not re-armed
	// by its successful renewals would have fenced long before.
	select {
	case <-ctx.Done():
		t.Fatalf("the engagement was fenced while its renewals were succeeding: %v", context.Cause(ctx))
	case <-time.After(4 * lease):
	}
	if n := len(fake.seen()); n < 4 {
		t.Errorf("%d renewals across four lease-lengths, want the loop still ticking", n)
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
		s.renewClaimLease(ctx, leaseTestConversation(), time.Now(), func(err error) { fenced <- err })
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
// It is deliberately the same shape as the user-stop test beside it, because
// the distinction being pinned is exactly that the two must NOT end the same
// way: a stop parks with user_cancelled and releases the claim, a lease fence
// writes nothing at all.
func TestDispatch_LostLeaseFencesTheEngagementWithoutWriting(t *testing.T) {
	fx := newLaunchFixtureWithWorktree(t, "1033", "")

	fetchEntered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(fetchEntered)
		<-r.Context().Done()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	fx.s.SetRunCredentialResolvers(bringUpResolver{client: ghclient.NewClient(server.URL, "test-token")}, nil, nil)

	// A renewal that refuses as soon as the loop's first tick lands, on a
	// cadence short enough to land during the held-open fetch.
	fake := &fakeRenewalStore{ConversationQueueStore: fx.stores.ConversationQueue, err: db.ErrClaimReleased}
	fx.s.conversationQueue = fake
	fx.s.SetClaimLease(20*time.Millisecond, 10*time.Second, 30*time.Second)

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
