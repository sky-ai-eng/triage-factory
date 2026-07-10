package lease

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeStore is an in-process simulation of the Postgres row semantics —
// good enough to drive Manager's decision logic without a real database.
// Time comparisons use a caller-controlled clock so tests can jump time
// deterministically instead of sleeping.
type fakeStore struct {
	mu       sync.Mutex
	now      time.Time
	holderID string
	term     int64
	renewed  time.Time
	exists   bool

	// Injected failures, consumed once each (nil after firing).
	acquireErr   error
	renewErr     error
	ensureRowErr error
}

// newFakeStore returns a store as if EnsureRow had already run: the row
// exists, unheld, with renewed_at far enough in the past to be
// immediately acquirable — tests exercise tick()/Run() acquisition logic
// directly without needing to call EnsureRow themselves.
func newFakeStore(now time.Time) *fakeStore {
	return &fakeStore{now: now, exists: true}
}

// newFakeStoreNoRow returns a store as if this were a genuinely fresh
// leases table — no row for any name yet, exactly the state EnsureRow
// exists to fix. Exercises the acquisition path's own EnsureRow retry
// (tick calls it every not-holder attempt, not just once at boot).
func newFakeStoreNoRow(now time.Time) *fakeStore {
	return &fakeStore{now: now, exists: false}
}

func (s *fakeStore) advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = s.now.Add(d)
}

func (s *fakeStore) EnsureRow(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ensureRowErr != nil {
		err := s.ensureRowErr
		s.ensureRowErr = nil
		return err
	}
	if !s.exists {
		s.exists = true
		s.holderID = ""
		s.term = 0
		s.renewed = time.Time{} // far in the past — immediately acquirable
	}
	return nil
}

func (s *fakeStore) TryAcquire(ctx context.Context, name, holderID string, ttl time.Duration) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acquireErr != nil {
		err := s.acquireErr
		s.acquireErr = nil
		return 0, false, err
	}
	if !s.exists {
		return 0, false, nil
	}
	if s.now.Sub(s.renewed) < ttl {
		return 0, false, nil // fresh under someone else
	}
	s.term++
	s.holderID = holderID
	s.renewed = s.now
	return s.term, true, nil
}

func (s *fakeStore) Renew(ctx context.Context, name, holderID string, term int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.renewErr != nil {
		err := s.renewErr
		s.renewErr = nil
		return false, err
	}
	if s.holderID != holderID || s.term != term {
		return false, nil
	}
	s.renewed = s.now
	return true, nil
}

func (s *fakeStore) Read(ctx context.Context, name string) (string, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holderID, s.term, nil
}

func TestNewManager_RejectsDemoteDeadlineNotLessThanTTL(t *testing.T) {
	store := newFakeStore(time.Now())
	if _, err := NewManager(store, "x", "me", time.Second, 20*time.Second, 20*time.Second, nil, nil, nil); err == nil {
		t.Fatal("expected an error when demoteDeadline == ttl")
	}
	if _, err := NewManager(store, "x", "me", time.Second, 20*time.Second, 25*time.Second, nil, nil, nil); err == nil {
		t.Fatal("expected an error when demoteDeadline > ttl")
	}
	if _, err := NewManager(store, "x", "me", time.Second, 20*time.Second, 15*time.Second, nil, nil, nil); err != nil {
		t.Fatalf("expected no error for a valid deadline < ttl, got %v", err)
	}
}

// TestManager_RecoversFromTransientEnsureRowFailure guards the exact stuck
// state a transient EnsureRow failure used to cause: EnsureRow was only
// ever called once, at Run's boot. If that single call failed (and no
// other pod had raced in to create the row), every subsequent tick's
// TryAcquire ran its UPDATE against a still-nonexistent row — which
// matches zero rows and returns (acquired=false, err=nil), the SAME
// signal as "someone else's lease is fresh" — so the manager span forever
// without ever acquiring, and without even logging an error past the
// first tick. Fixed by retrying EnsureRow on every not-holder tick (see
// tick's doc comment); this test drives two ticks against a store that
// starts with NO row at all, injects a failure on the first EnsureRow
// call, and asserts the second tick (EnsureRow succeeding this time)
// recovers and acquires.
func TestManager_RecoversFromTransientEnsureRowFailure(t *testing.T) {
	store := newFakeStoreNoRow(time.Now())
	store.ensureRowErr = errors.New("transient connection blip")
	m, err := NewManager(store, "x", "pod-a", time.Hour, 20*time.Second, 15*time.Second, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	m.tick(context.Background())
	if m.IsHolder() {
		t.Fatal("must not acquire on a tick whose EnsureRow failed against a nonexistent row")
	}
	if store.exists {
		t.Fatal("test setup invariant broken: the injected EnsureRow error should have prevented row creation")
	}

	m.tick(context.Background())
	if !m.IsHolder() {
		t.Fatal("expected the manager to recover and acquire once EnsureRow is retried and succeeds")
	}
}

// TestManager_AcquireOnFreshRow pins that a manager against a fresh
// (never-acquired) row wins on its very first tick, calling onAcquire.
func TestManager_AcquireOnFreshRow(t *testing.T) {
	store := newFakeStore(time.Now())
	var acquired bool
	var acquiredTerm int64
	m, err := NewManager(store, "background-brain", "pod-a", time.Hour, 20*time.Second, 15*time.Second, nil,
		func(term int64) { acquired = true; acquiredTerm = term },
		func(string) { t.Error("onDemote should not fire") },
	)
	if err != nil {
		t.Fatal(err)
	}
	m.tick(context.Background())
	if !acquired {
		t.Fatal("expected acquisition on a fresh row")
	}
	if acquiredTerm != 1 {
		t.Errorf("term = %d, want 1", acquiredTerm)
	}
	if !m.IsHolder() {
		t.Error("IsHolder() = false after acquiring")
	}
	st := m.Status()
	if st.HolderID != "pod-a" || st.Term != 1 || !st.IsHolder {
		t.Errorf("Status() = %+v, unexpected", st)
	}
}

// TestManager_SecondAcquirerBlockedUntilTTL pins the core mutual-exclusion
// property: a second manager can't acquire while the first's lease is
// fresh, and succeeds once the row goes stale past the TTL.
func TestManager_SecondAcquirerBlockedUntilTTL(t *testing.T) {
	now := time.Now()
	store := newFakeStore(now)
	ttl := 20 * time.Second

	a, err := NewManager(store, "background-brain", "pod-a", time.Hour, ttl, 15*time.Second, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewManager(store, "background-brain", "pod-b", time.Hour, ttl, 15*time.Second, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	a.tick(context.Background())
	if !a.IsHolder() {
		t.Fatal("pod-a should have acquired the fresh lease")
	}

	b.tick(context.Background())
	if b.IsHolder() {
		t.Fatal("pod-b must not acquire while pod-a's lease is fresh")
	}

	// pod-a keeps renewing; still blocks pod-b.
	store.advance(ttl / 2)
	a.tick(context.Background())
	b.tick(context.Background())
	if b.IsHolder() {
		t.Fatal("pod-b must not acquire while pod-a keeps renewing")
	}

	// pod-a goes silent (simulating a crash/partition): once the row is
	// stale past the TTL, pod-b acquires.
	store.advance(ttl + time.Second)
	b.tick(context.Background())
	if !b.IsHolder() {
		t.Fatal("pod-b should acquire once the lease is stale past the TTL")
	}
}

// TestManager_ExplicitRenewalLossDemotesImmediately pins that a renewal
// reporting "0 rows matched" (someone else already re-acquired) demotes
// the same tick, without waiting for the deadline ticker.
func TestManager_ExplicitRenewalLossDemotesImmediately(t *testing.T) {
	now := time.Now()
	store := newFakeStore(now)
	var demoteReason string
	a, err := NewManager(store, "x", "pod-a", time.Hour, 20*time.Second, 15*time.Second, nil, nil, func(r string) { demoteReason = r })
	if err != nil {
		t.Fatal(err)
	}
	a.tick(context.Background())
	if !a.IsHolder() {
		t.Fatal("expected pod-a to acquire")
	}

	// Simulate a stolen lease: another holder overwrites the row directly.
	store.mu.Lock()
	store.holderID = "pod-b"
	store.term = 99
	store.mu.Unlock()

	a.tick(context.Background())
	if a.IsHolder() {
		t.Fatal("pod-a must demote once its renewal reports 0 rows matched")
	}
	if demoteReason == "" {
		t.Error("expected onDemote to fire with a reason")
	}
}

// TestManager_DemoteDeadlineFiresOnPersistentRenewError pins the
// partition scenario: Renew keeps erroring (not explicitly losing), and
// the deadline ticker demotes once the monotonic deadline elapses, even
// though tick() itself never saw an explicit "lost" signal.
func TestManager_DemoteDeadlineFiresOnPersistentRenewError(t *testing.T) {
	store := newFakeStore(time.Now())
	demoteDeadline := 15 * time.Second
	a, err := NewManager(store, "x", "pod-a", time.Hour, 20*time.Second, demoteDeadline, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.tick(context.Background())
	if !a.IsHolder() {
		t.Fatal("expected pod-a to acquire")
	}

	// Every subsequent Renew errors (simulated DB partition) without ever
	// reporting an explicit loss.
	errPartition := errors.New("connection refused")
	for i := 0; i < 5; i++ {
		store.mu.Lock()
		store.renewErr = errPartition
		store.mu.Unlock()
		a.tick(context.Background())
		if !a.IsHolder() {
			t.Fatalf("tick %d: must not demote from a Renew error alone (only the deadline ticker may)", i)
		}
	}
	a.checkDemoteDeadline()
	if !a.IsHolder() {
		t.Fatal("expected checkDemoteDeadline to have not fired yet (real time hasn't advanced past the deadline)")
	}

	// Force the deadline to have elapsed by rewinding lastSuccessfulRenewal.
	a.mu.Lock()
	a.lastSuccessfulRenewal = time.Now().Add(-demoteDeadline - time.Second)
	a.mu.Unlock()
	a.checkDemoteDeadline()
	if a.IsHolder() {
		t.Fatal("expected self-demotion once the monotonic deadline elapsed with no successful renewal")
	}
}

// TestManager_FencedHolderDemotesImmediately pins the identity-fence
// interaction: a fenced holder must stop renewing and demote on the very
// next tick, regardless of lease freshness.
func TestManager_FencedHolderDemotesImmediately(t *testing.T) {
	store := newFakeStore(time.Now())
	fenced := false
	a, err := NewManager(store, "x", "pod-a", time.Hour, 20*time.Second, 15*time.Second, func() bool { return fenced }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.tick(context.Background())
	if !a.IsHolder() {
		t.Fatal("expected pod-a to acquire")
	}

	fenced = true
	a.tick(context.Background())
	if a.IsHolder() {
		t.Fatal("a fenced holder must demote on its next tick")
	}
}

// TestManager_FencedInstanceNeverAcquires pins that a fenced (never-was-
// holder) instance doesn't attempt acquisition at all.
func TestManager_FencedInstanceNeverAcquires(t *testing.T) {
	store := newFakeStore(time.Now())
	a, err := NewManager(store, "x", "pod-a", time.Hour, 20*time.Second, 15*time.Second, func() bool { return true }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.tick(context.Background())
	if a.IsHolder() {
		t.Fatal("a fenced instance must never acquire")
	}
}

// TestManager_RunDemotesOnShutdown pins that Run() demotes a current
// holder on ctx cancellation before returning.
func TestManager_RunDemotesOnShutdown(t *testing.T) {
	store := newFakeStore(time.Now())
	var demoted bool
	a, err := NewManager(store, "x", "pod-a", 10*time.Millisecond, 20*time.Second, 15*time.Second, nil, nil, func(string) { demoted = true })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()
	// Give Run's first synchronous tick a moment to acquire.
	deadline := time.Now().Add(2 * time.Second)
	for !a.IsHolder() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !a.IsHolder() {
		t.Fatal("expected Run to acquire the lease")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
	if !demoted {
		t.Error("expected onDemote to fire on shutdown while holding the lease")
	}
	if a.IsHolder() {
		t.Error("expected IsHolder() = false after shutdown demotion")
	}
}

func TestDeadlineCheckInterval_FloorsAtOneSecond(t *testing.T) {
	if got := deadlineCheckInterval(2 * time.Second); got != time.Second {
		t.Errorf("deadlineCheckInterval(2s) = %s, want 1s floor", got)
	}
	if got := deadlineCheckInterval(15 * time.Second); got != 3*time.Second {
		t.Errorf("deadlineCheckInterval(15s) = %s, want 3s", got)
	}
}

func TestKnobsFromEnv_Defaults(t *testing.T) {
	t.Setenv("TF_LEASE_RENEW_SEC", "")
	t.Setenv("TF_LEASE_TTL_SEC", "")
	t.Setenv("TF_LEASE_DEMOTE_SEC", "")
	renew, ttl, demote, err := KnobsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if renew != DefaultRenewInterval || ttl != DefaultTTL || demote != DefaultDemoteDeadline {
		t.Errorf("got (%s, %s, %s), want defaults", renew, ttl, demote)
	}
}

func TestKnobsFromEnv_RejectsDemoteGEQ_TTL(t *testing.T) {
	t.Setenv("TF_LEASE_RENEW_SEC", "5")
	t.Setenv("TF_LEASE_TTL_SEC", "10")
	t.Setenv("TF_LEASE_DEMOTE_SEC", "10")
	if _, _, _, err := KnobsFromEnv(); err == nil {
		t.Fatal("expected an error when TF_LEASE_DEMOTE_SEC >= TF_LEASE_TTL_SEC")
	}
}

func TestKnobsFromEnv_RejectsMalformed(t *testing.T) {
	t.Setenv("TF_LEASE_RENEW_SEC", "not-a-number")
	if _, _, _, err := KnobsFromEnv(); err == nil {
		t.Fatal("expected an error for a malformed TF_LEASE_RENEW_SEC")
	}
}
