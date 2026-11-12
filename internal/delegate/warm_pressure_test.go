package delegate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// pressureScan is the scan interval these tests drive the dispatcher at.
const pressureScan = 20 * time.Millisecond

// pressureSettle is how long a test waits before it decides an engagement
// was not let go. The let-go itself is synchronous, but the stand-in
// engagement sees it on its own goroutine.
const pressureSettle = 5 * pressureScan

// pressureStore answers the dispatcher's questions about queued work from
// fields the test sets. Its claim records that the dispatcher got as far as
// claiming and takes nothing, so no engagement starts.
type pressureStore struct {
	db.ConversationQueueStore

	mu         sync.Mutex
	claimable  bool
	capBlocked map[string]bool
	claims     int
	probes     int
}

func (p *pressureStore) ClaimableExistsSystem(context.Context, string, db.ClaimPlacement, time.Duration) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probes++
	return p.claimable, nil
}

func (p *pressureStore) OrgCapBlockedWorkExistsSystem(_ context.Context, orgID string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capBlocked[orgID], nil
}

func (p *pressureStore) ClaimNextConversation(context.Context, string, int64, db.ClaimPlacement, time.Duration) (*domain.Conversation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.claims++
	return nil, nil
}

func (p *pressureStore) CountQueuedSystem(context.Context) (int, error) { return 1, nil }

func (p *pressureStore) counts() (claims, probes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.claims, p.probes
}

// newPressureSpawner returns a spawner on store with slots concurrency slots.
func newPressureSpawner(t *testing.T, store *pressureStore, slots int) *Spawner {
	t.Helper()
	stores := testSpawnerStores(nil)
	stores.ConversationQueue = store
	s := NewSpawner(nil, stores, nil, nil, "")
	s.SetMaxConcurrentClaims(slots)
	// Every stand-in engagement still waiting at the end is let go, so its
	// goroutine returns.
	t.Cleanup(s.warmWaits.LetGoAll)
	return s
}

// standInWarm is a native engagement waiting warm on the executor: it holds
// a slot until the registry lets it go, and then hands the slot back once
// its teardown is allowed to finish.
type standInWarm struct {
	letGo    chan struct{}
	teardown chan struct{}
}

// holdWarm registers a stand-in engagement on conversationID. With
// slowTeardown its slot is held after the let-go until finishTeardown.
func holdWarm(t *testing.T, s *Spawner, conversationID, orgID string, slowTeardown bool) *standInWarm {
	t.Helper()
	sem := s.semaphore()
	sem <- struct{}{}
	w := s.warmWaits.Register(conversationID, orgID)
	e := &standInWarm{letGo: make(chan struct{}), teardown: make(chan struct{})}
	if !slowTeardown {
		close(e.teardown)
	}
	go func() {
		<-w.LetGo()
		close(e.letGo)
		<-e.teardown
		<-sem
	}()
	t.Cleanup(e.finishTeardown)
	return e
}

func (e *standInWarm) finishTeardown() {
	select {
	case <-e.teardown:
	default:
		close(e.teardown)
	}
}

// wasLetGo reports whether the engagement has been let go, waiting up to
// within for it.
func (e *standInWarm) wasLetGo(within time.Duration) bool {
	select {
	case <-e.letGo:
		return true
	case <-time.After(within):
		return false
	}
}

// drainInBackground runs one drain and returns a channel that receives when
// it returns.
func drainInBackground(ctx context.Context, s *Spawner) <-chan bool {
	done := make(chan bool, 1)
	go func() { done <- s.drainConversationQueue(ctx, pressureScan) }()
	return done
}

func TestWarmPressure_FullExecutorLetsGoOfTheOlderForClaimableWork(t *testing.T) {
	store := &pressureStore{claimable: true}
	s := newPressureSpawner(t, store, 2)
	older := holdWarm(t, s, "conv-older", "org-a", false)
	newer := holdWarm(t, s, "conv-newer", "org-a", false)

	done := drainInBackground(context.Background(), s)
	if !older.wasLetGo(5 * time.Second) {
		t.Fatal("the older warm claim was not let go with claimable work queued behind a full executor")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain did not take the slot the let-go freed")
	}
	if claims, _ := store.counts(); claims != 1 {
		t.Errorf("claims = %d, want 1: the drain claims with the freed slot", claims)
	}
	if newer.wasLetGo(pressureSettle) {
		t.Error("the newer warm claim was let go too")
	}
	if n := s.warmWaits.Len(); n != 1 {
		t.Errorf("warm claims waiting = %d, want 1", n)
	}
}

// One claim let go for a slot frees the slot the wait needs, so however long
// its teardown takes, the wait lets no second one go.
func TestWarmPressure_FullExecutorLetsGoOfOneWhileItsSlotFrees(t *testing.T) {
	store := &pressureStore{claimable: true}
	s := newPressureSpawner(t, store, 2)
	older := holdWarm(t, s, "conv-older", "org-a", true)
	newer := holdWarm(t, s, "conv-newer", "org-a", false)

	done := drainInBackground(context.Background(), s)
	if !older.wasLetGo(5 * time.Second) {
		t.Fatal("the older warm claim was not let go")
	}
	if newer.wasLetGo(20 * pressureScan) {
		t.Fatal("a second warm claim was let go while the first one's slot was still freeing")
	}
	older.finishTeardown()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain did not take the slot once it freed")
	}
}

func TestWarmPressure_FullExecutorWithNoClaimableWorkLetsNothingGo(t *testing.T) {
	store := &pressureStore{claimable: false}
	s := newPressureSpawner(t, store, 2)
	older := holdWarm(t, s, "conv-older", "org-a", false)
	newer := holdWarm(t, s, "conv-newer", "org-a", false)

	ctx, cancel := context.WithCancel(context.Background())
	done := drainInBackground(ctx, s)
	if older.wasLetGo(20*pressureScan) || newer.wasLetGo(pressureSettle) {
		t.Error("a warm claim was let go with no claimable work queued")
	}
	if _, probes := store.counts(); probes == 0 {
		t.Error("the blocked acquire never asked whether claimable work was queued")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked acquire did not end with its context")
	}
}

func TestWarmPressure_MemoryGateLetsGoOfExactlyOne(t *testing.T) {
	store := &pressureStore{claimable: true}
	s := newPressureSpawner(t, store, 4)
	s.SetDispatchMemFloor(100)
	s.memAvailMB = func() int { return 1 }
	older := holdWarm(t, s, "conv-older", "org-a", false)
	newer := holdWarm(t, s, "conv-newer", "org-a", false)

	s.scanConversationQueue(context.Background(), pressureScan)
	if !older.wasLetGo(time.Second) {
		t.Fatal("the older warm claim was not let go with the memory gate tripped and claimable work queued")
	}
	if newer.wasLetGo(pressureSettle) {
		t.Error("a second warm claim was let go on the same tick")
	}
	// A wake drains without the tick's checks, so it lets nothing go.
	s.drainConversationQueue(context.Background(), pressureScan)
	if newer.wasLetGo(pressureSettle) {
		t.Error("a wake let a warm claim go")
	}
	if claims, _ := store.counts(); claims != 0 {
		t.Errorf("claims = %d, want 0 behind a tripped memory gate", claims)
	}
}

func TestWarmPressure_OrgAtItsCapLetsGoOfThatOrgsOldestWhileSlotsAreFree(t *testing.T) {
	store := &pressureStore{capBlocked: map[string]bool{"org-capped": true}}
	s := newPressureSpawner(t, store, 8)
	otherOrg := holdWarm(t, s, "conv-other-org", "org-free", false)
	cappedOlder := holdWarm(t, s, "conv-capped-older", "org-capped", false)
	cappedNewer := holdWarm(t, s, "conv-capped-newer", "org-capped", false)

	s.scanConversationQueue(context.Background(), pressureScan)
	if !cappedOlder.wasLetGo(time.Second) {
		t.Fatal("the capped org's oldest warm claim was not let go")
	}
	if cappedNewer.wasLetGo(pressureSettle) {
		t.Error("the capped org's newer warm claim was let go too")
	}
	if otherOrg.wasLetGo(pressureSettle) {
		t.Error("a warm claim in an org under its cap was let go")
	}
	if claims, _ := store.counts(); claims != 1 {
		t.Errorf("claims = %d, want 1: the tick still drains after the cap check", claims)
	}
}

// A claim let go for an org's cap frees memory as well, so the same tick lets
// no second claim go for the memory gate.
func TestWarmPressure_OrgCapLetGoCountsForTheGateOnTheSameTick(t *testing.T) {
	store := &pressureStore{claimable: true, capBlocked: map[string]bool{"org-capped": true}}
	s := newPressureSpawner(t, store, 8)
	s.SetDispatchMemFloor(100)
	s.memAvailMB = func() int { return 1 }
	otherOrg := holdWarm(t, s, "conv-other-org", "org-free", false)
	capped := holdWarm(t, s, "conv-capped", "org-capped", false)

	s.scanConversationQueue(context.Background(), pressureScan)
	if !capped.wasLetGo(time.Second) {
		t.Fatal("the capped org's warm claim was not let go")
	}
	if otherOrg.wasLetGo(pressureSettle) {
		t.Error("a second warm claim was let go for the memory gate on the same tick")
	}
}

// With every slot full the dispatcher waits inside the acquire, and the cap
// check runs there instead of on the scan tick. The claim it lets go frees a
// slot too, so the wait lets nothing else go for one.
func TestWarmPressure_OrgAtItsCapLetsGoWhileTheExecutorIsFull(t *testing.T) {
	store := &pressureStore{claimable: true, capBlocked: map[string]bool{"org-capped": true}}
	s := newPressureSpawner(t, store, 2)
	otherOrg := holdWarm(t, s, "conv-other-org", "org-free", false)
	capped := holdWarm(t, s, "conv-capped", "org-capped", true)

	done := drainInBackground(context.Background(), s)
	if !capped.wasLetGo(5 * time.Second) {
		t.Fatal("the capped org's warm claim was not let go during a blocked acquire")
	}
	if otherOrg.wasLetGo(20 * pressureScan) {
		t.Error("a second warm claim was let go for a slot the cap let-go is already freeing")
	}
	capped.finishTeardown()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain did not take the slot once it freed")
	}
}

func TestWarmPressure_DrainingLetsGoOfEveryWarmClaim(t *testing.T) {
	store := &pressureStore{}
	s := newPressureSpawner(t, store, 4)
	engagements := []*standInWarm{
		holdWarm(t, s, "conv-1", "org-a", false),
		holdWarm(t, s, "conv-2", "org-a", false),
		holdWarm(t, s, "conv-3", "org-b", false),
	}
	// Gated as well, so the drain is read ahead of the gates.
	s.SetDispatchMemFloor(100)
	s.memAvailMB = func() int { return 1 }
	s.SetDraining(true)

	s.drainConversationQueue(context.Background(), pressureScan)
	for i, e := range engagements {
		if !e.wasLetGo(time.Second) {
			t.Errorf("warm claim %d was not let go on drain", i+1)
		}
	}
	if n := s.warmWaits.Len(); n != 0 {
		t.Errorf("warm claims waiting = %d, want 0", n)
	}
	if claims, _ := store.counts(); claims != 0 {
		t.Errorf("claims = %d, want 0 while draining", claims)
	}
}

// A full executor waits for a slot rather than at the drain check, so a
// drain that latches during the wait lets every warm claim go from there,
// and the slots they free start no new claim.
func TestWarmPressure_DrainingLetsGoOfEveryWarmClaimOnAFullExecutor(t *testing.T) {
	store := &pressureStore{claimable: false}
	s := newPressureSpawner(t, store, 2)
	older := holdWarm(t, s, "conv-older", "org-a", false)
	newer := holdWarm(t, s, "conv-newer", "org-a", false)

	done := drainInBackground(context.Background(), s)
	if _, probes := waitForProbe(t, store); probes == 0 {
		t.Fatal("the drain never reached the blocked acquire")
	}
	s.SetDraining(true)
	if !older.wasLetGo(5*time.Second) || !newer.wasLetGo(5*time.Second) {
		t.Fatal("a warm claim was not let go when the drain latched during a blocked acquire")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked acquire did not end on the drain")
	}
	if claims, _ := store.counts(); claims != 0 {
		t.Errorf("claims = %d, want 0: the slots a drain frees start no new claim", claims)
	}
}

// waitForProbe waits until the dispatcher has asked the store whether work is
// claimable, which it does only from a blocked acquire, and returns the
// counts then.
func waitForProbe(t *testing.T, store *pressureStore) (claims, probes int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if claims, probes = store.counts(); probes > 0 {
			return claims, probes
		}
		time.Sleep(pressureScan / 2)
	}
	return store.counts()
}
