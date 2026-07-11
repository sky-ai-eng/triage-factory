package delegate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSetExecutorID_OverridesConstructorDefault pins that a Spawner has NO
// identity until SetExecutorID wires the persistent registry id (an empty
// default is what makes a missed wiring observable — a random uuid default
// would stamp plausible-looking but unregistered ids onto runs), and that
// stampExecutor picks up the override — runs.executor_id on claim must
// equal the registry id.
func TestSetExecutorID_OverridesConstructorDefault(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")

	defaultID, defaultEpoch := s.executorIdentity()
	if defaultID != "" {
		t.Fatalf("expected NewSpawner to default to an EMPTY executor id (unset identity), got %q", defaultID)
	}
	if defaultEpoch != 0 {
		t.Fatalf("expected the constructor default boot epoch to be 0, got %d", defaultEpoch)
	}

	s.SetExecutorID("persistent-instance-id", 7)
	gotID, gotEpoch := s.executorIdentity()
	if gotID != "persistent-instance-id" || gotEpoch != 7 {
		t.Fatalf("executorIdentity() = (%q, %d), want (%q, %d)", gotID, gotEpoch, "persistent-instance-id", 7)
	}

	seedRun(t, database, "run-stamp", "sess-1", "/tmp/wt-run-stamp")
	s.stampExecutor(runmode.LocalDefaultOrgID, "run-stamp")

	var stored string
	if err := database.QueryRow(`SELECT executor_id FROM runs WHERE id = ?`, "run-stamp").Scan(&stored); err != nil {
		t.Fatalf("read back executor_id: %v", err)
	}
	if stored != "persistent-instance-id" {
		t.Errorf("runs.executor_id = %q, want the persistent id %q", stored, "persistent-instance-id")
	}
}

// TestHeartbeatOnce_WritesLiveCapacitySnapshot pins that heartbeatOnce
// writes the semaphore occupancy/cap and the memory gate state onto the
// registry row — the row must visibly track gated/headroom transitions,
// not just a snapshot frozen at boot.
func TestHeartbeatOnce_WritesLiveCapacitySnapshot(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)
	s := NewSpawner(database, stores, nil, nil, "")
	s.SetMaxConcurrentRuns(3)

	ctx := context.Background()
	const id = "hb-instance"
	epoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "test-version", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	s.SetExecutorID(id, epoch)

	// Occupy one semaphore slot so active_runs is observably non-zero and
	// distinct from max_runs.
	sem := s.semaphore()
	sem <- struct{}{}
	defer func() { <-sem }()

	s.heartbeatOnce(ctx)

	got, err := stores.Instances.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("expected a registered row")
	}
	if got.MaxRuns == nil || *got.MaxRuns != 3 {
		t.Errorf("MaxRuns = %v, want 3 (SetMaxConcurrentRuns)", got.MaxRuns)
	}
	if got.ActiveRuns == nil || *got.ActiveRuns != 1 {
		t.Errorf("ActiveRuns = %v, want 1 (one held semaphore slot)", got.ActiveRuns)
	}
	if got.DispatchGated == nil {
		t.Error("DispatchGated should be set (even if false) by every heartbeat")
	}
}

// TestHeartbeatOnce_TracksGateTransition pins that a heartbeat taken while
// the dispatch memory floor is tripped writes dispatch_gated=true, and a
// later heartbeat taken once memory recovers writes it back to false —
// the row visibly tracks the gate transition, not just a snapshot at boot.
func TestHeartbeatOnce_TracksGateTransition(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)
	s := NewSpawner(database, stores, nil, nil, "")
	s.SetDispatchMemFloor(4096)

	low := true
	s.mu.Lock()
	s.memAvailMB = func() int {
		if low {
			return 100
		}
		return 8192
	}
	s.mu.Unlock()

	ctx := context.Background()
	const id = "hb-gate-instance"
	epoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "test-version", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	s.SetExecutorID(id, epoch)

	s.heartbeatOnce(ctx)
	got, err := stores.Instances.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get (gated): %v", err)
	}
	if got.DispatchGated == nil || !*got.DispatchGated {
		t.Fatalf("DispatchGated = %v, want true while memory is below the floor", got.DispatchGated)
	}
	if got.MemAvailableMB == nil || *got.MemAvailableMB != 100 {
		t.Errorf("MemAvailableMB = %v, want 100", got.MemAvailableMB)
	}

	low = false
	s.heartbeatOnce(ctx)
	got, err = stores.Instances.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get (recovered): %v", err)
	}
	if got.DispatchGated == nil || *got.DispatchGated {
		t.Errorf("DispatchGated = %v, want false once memory recovers", got.DispatchGated)
	}
}

// TestRunInstanceHeartbeat_NilStoreIsNoOp pins that a Spawner with no
// InstanceStore wired (the test / no-seam path) returns immediately
// instead of blocking, so callers can always safely `go s.RunInstanceHeartbeat(...)`.
func TestRunInstanceHeartbeat_NilStoreIsNoOp(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	// No InstanceStore in an empty db.Stores{} — a real ticker would block
	// forever on the interval, so returning promptly here is the whole point.
	s.RunInstanceHeartbeat(context.Background(), DefaultInstanceHeartbeatInterval)
}

// TestRunInstanceHeartbeat_NoExecutorIDIsNoOp pins that an InstanceStore
// alone isn't enough to start the loop — SetExecutorID must have run too
// (production always calls it before starting this loop). This is the real
// missed-wiring state, reachable exactly because the constructor default
// is empty.
func TestRunInstanceHeartbeat_NoExecutorIDIsNoOp(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")
	s.RunInstanceHeartbeat(context.Background(), DefaultInstanceHeartbeatInterval)
}

// TestHeartbeatOnce_SupersededIdentityFences pins the split-identity
// reaction: a heartbeat that matches no row (another boot of the same id
// re-registered and bumped the epoch) latches the identity fence — the
// dispatcher must stop claiming, resumes are refused, and heartbeatOnce
// reports the loop should stop. The latch is sticky: only a restart
// (re-register) clears it.
func TestHeartbeatOnce_SupersededIdentityFences(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)
	s := NewSpawner(database, stores, nil, nil, "")

	ctx := context.Background()
	const id = "fence-instance"
	staleEpoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1", "")
	if err != nil {
		t.Fatalf("Register (boot 1): %v", err)
	}
	s.SetExecutorID(id, staleEpoch)

	if s.IdentityFenced() {
		t.Fatal("fence must not be latched before any supersession evidence")
	}
	if !s.heartbeatOnce(ctx) {
		t.Fatal("a matching heartbeat must keep the loop running")
	}

	// A second boot of the same id (duplicated state root) bumps the epoch
	// out from under this process.
	if _, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1", ""); err != nil {
		t.Fatalf("Register (boot 2): %v", err)
	}

	if s.heartbeatOnce(ctx) {
		t.Fatal("a superseded heartbeat must tell the loop to stop")
	}
	if !s.IdentityFenced() {
		t.Fatal("supersession must latch the identity fence")
	}
	// Sticky: another (still-superseded) probe doesn't flip it back, and
	// the fence holds without further heartbeats.
	if !s.IdentityFenced() {
		t.Fatal("fence must be sticky")
	}
}

// TestFenceIdentity_KillsSandboxesAndInvokesExitHook pins fence completion
// (spec §4.1(4) second half, TFAC-586): a supersession must kill every live
// sandbox via the existing cancel machinery AND invoke the wired
// onSupersessionFence hook exactly once, even under repeated supersession
// evidence (the sticky CompareAndSwap guard).
func TestFenceIdentity_KillsSandboxesAndInvokesExitHook(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)
	s := NewSpawner(database, stores, nil, nil, "")

	ctx := context.Background()
	const id = "fence-kill-instance"
	staleEpoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1", "")
	if err != nil {
		t.Fatalf("Register (boot 1): %v", err)
	}
	s.SetExecutorID(id, staleEpoch)

	killedA, killedB := false, false
	s.mu.Lock()
	s.cancels["run-a"] = func() { killedA = true }
	s.cancels["run-b"] = func() { killedB = true }
	s.mu.Unlock()

	var exitCalls int
	var exitMu sync.Mutex
	s.SetOnSupersessionFence(func() {
		exitMu.Lock()
		exitCalls++
		exitMu.Unlock()
	})

	if _, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1", ""); err != nil {
		t.Fatalf("Register (boot 2): %v", err)
	}
	s.heartbeatOnce(ctx)

	if !killedA || !killedB {
		t.Errorf("expected every registered cancel to fire: killedA=%v killedB=%v", killedA, killedB)
	}
	exitMu.Lock()
	got := exitCalls
	exitMu.Unlock()
	if got != 1 {
		t.Fatalf("onSupersessionFence called %d times, want exactly 1", got)
	}

	// A second (still-superseded) heartbeat must not re-fire the hook or
	// re-kill anything — fenceIdentity's CompareAndSwap makes this once
	// per process life.
	s.heartbeatOnce(ctx)
	exitMu.Lock()
	got = exitCalls
	exitMu.Unlock()
	if got != 1 {
		t.Fatalf("onSupersessionFence called %d times after a second fenced heartbeat, want still 1", got)
	}
}

// TestHeartbeatOnce_ReadsBackDrainingFlag pins the mechanism the drain CLI
// verb relies on: an operator-set draining flag (written out-of-band via
// InstanceStore.SetDraining, e.g. by `triagefactory instance drain`) is
// visible on this Spawner within one heartbeat, in both directions.
func TestHeartbeatOnce_ReadsBackDrainingFlag(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)
	s := NewSpawner(database, stores, nil, nil, "")

	ctx := context.Background()
	const id = "drain-flag-instance"
	epoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	s.SetExecutorID(id, epoch)

	if s.Draining() {
		t.Fatal("must not report draining before any operator action")
	}

	if _, err := stores.Instances.SetDraining(ctx, id, true); err != nil {
		t.Fatalf("SetDraining(true): %v", err)
	}
	s.heartbeatOnce(ctx)
	if !s.Draining() {
		t.Fatal("heartbeatOnce must read back the operator-set draining flag")
	}

	if _, err := stores.Instances.SetDraining(ctx, id, false); err != nil {
		t.Fatalf("SetDraining(false): %v", err)
	}
	s.heartbeatOnce(ctx)
	if s.Draining() {
		t.Fatal("heartbeatOnce must read back an un-drain too")
	}
}

// toggleableInstanceStore is an in-memory db.InstanceStore whose Heartbeat
// can be told to fail on demand — the partition self-fence needs a
// heartbeat WRITE FAILURE (distinct from the supersession "matched=false"
// case a real sqlite-backed store produces via a second Register), which
// nothing else in this test file can manufacture on command.
type toggleableInstanceStore struct {
	mu   sync.Mutex
	fail bool
}

func (f *toggleableInstanceStore) Register(context.Context, string, string, string, string) (int64, error) {
	return 1, nil
}
func (f *toggleableInstanceStore) Heartbeat(context.Context, string, int64, domain.InstanceHeartbeat) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return false, false, errors.New("simulated heartbeat write failure")
	}
	return true, false, nil
}
func (f *toggleableInstanceStore) Get(context.Context, string) (*domain.Instance, error) {
	return nil, nil
}
func (f *toggleableInstanceStore) List(context.Context) ([]domain.Instance, error) { return nil, nil }
func (f *toggleableInstanceStore) SetDraining(context.Context, string, bool) (bool, error) {
	return false, nil
}
func (f *toggleableInstanceStore) setFail(v bool) {
	f.mu.Lock()
	f.fail = v
	f.mu.Unlock()
}

// delayedInstanceStore is a db.InstanceStore whose Heartbeat either blocks
// until its ctx is done (mirroring a real driver aborting an in-flight
// query once a context deadline/cancel fires — the shape a silently
// black-holed connection takes) or sleeps a fixed delay before succeeding.
// Used to prove (a) heartbeatOnce's per-call write timeout actually bounds
// an otherwise-permanently-hung write, and (b) a write that succeeds only
// AFTER the self-fence deadline has already elapsed still triggers the
// fence (the "late success" case) instead of silently resuming as if
// nothing happened.
type delayedInstanceStore struct {
	block bool
	delay time.Duration
}

func (f *delayedInstanceStore) Register(context.Context, string, string, string, string) (int64, error) {
	return 1, nil
}
func (f *delayedInstanceStore) Heartbeat(ctx context.Context, _ string, _ int64, _ domain.InstanceHeartbeat) (bool, bool, error) {
	if f.block {
		<-ctx.Done()
		return false, false, ctx.Err()
	}
	select {
	case <-time.After(f.delay):
		return true, false, nil
	case <-ctx.Done():
		return false, false, ctx.Err()
	}
}
func (f *delayedInstanceStore) Get(context.Context, string) (*domain.Instance, error) {
	return nil, nil
}
func (f *delayedInstanceStore) List(context.Context) ([]domain.Instance, error) { return nil, nil }
func (f *delayedInstanceStore) SetDraining(context.Context, string, bool) (bool, error) {
	return false, nil
}

// TestHeartbeatOnce_WriteTimeoutBoundsAHungConnectionAndEventuallyFences is
// the direct regression test for the bug this ticket's review found: a
// heartbeat write against a silently black-holed connection (no RST —
// packets just vanish) must NOT be able to block heartbeatOnce forever.
// Without a per-call timeout, the ticker loop stalls and
// checkPartitionSelfFence never runs at all, so the self-fence deadline is
// never evaluated and the reaper could requeue this instance's claimed
// runs while its sandboxes are still live — the exact double-external-
// write hazard the self-fence exists to prevent. Run on a goroutine with a
// test-level timeout so a regression fails this test within 5s instead of
// hanging until the whole test binary's own (much longer) timeout.
func TestHeartbeatOnce_WriteTimeoutBoundsAHungConnectionAndEventuallyFences(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")
	s.instances = &delayedInstanceStore{block: true}
	s.SetExecutorID("hung-instance", 1)
	// A 2s self-fence deadline derives a 1s per-call write timeout
	// (deadline/3, floored at 1s) — two bounded, timed-out calls are
	// enough to cross the deadline, keeping this test's wall time small.
	s.SetSelfFenceDeadline(2 * time.Second)
	s.mu.Lock()
	s.lastGoodContactAt = time.Now()
	s.mu.Unlock()

	killed := false
	s.mu.Lock()
	s.cancels["run-hung"] = func() { killed = true }
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for !s.PartitionFenced() {
			if !s.heartbeatOnce(context.Background()) {
				t.Error("a heartbeat write failure must keep the loop running")
				return
			}
		}
	}()

	select {
	case <-done:
		if !killed {
			t.Error("expected the self-fence to kill live sandboxes once it fired")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("partition self-fence never fired within 5s against a permanently hung heartbeat write — the per-call write timeout isn't actually bounding the call, defeating the whole self-fence mechanism")
	}
}

// TestHeartbeatOnce_LateSuccessPastDeadlineStillFences pins the second
// variant the review found: a write that eventually SUCCEEDS, but only
// after taking long enough that the self-fence deadline had already
// elapsed since the last known-good contact, must still react (kill live
// sandboxes) — a bare "it succeeded" is not proof this instance was never
// partitioned for long enough that the reaper could have already requeued
// its claimed work while the write was still in flight.
func TestHeartbeatOnce_LateSuccessPastDeadlineStillFences(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")
	// The write itself takes 100ms — comfortably longer than the 30ms
	// self-fence deadline below, so by the time it succeeds the deadline
	// has already elapsed.
	s.instances = &delayedInstanceStore{delay: 100 * time.Millisecond}
	s.SetExecutorID("late-instance", 1)
	s.SetSelfFenceDeadline(30 * time.Millisecond)
	s.mu.Lock()
	s.lastGoodContactAt = time.Now()
	s.mu.Unlock()

	killed := false
	s.mu.Lock()
	s.cancels["run-late"] = func() { killed = true }
	s.mu.Unlock()

	if !s.heartbeatOnce(context.Background()) {
		t.Fatal("a successful heartbeat must keep the loop running")
	}
	if !killed {
		t.Error("a write that succeeds only after the self-fence deadline already elapsed must still kill live sandboxes defensively")
	}
}

// TestCheckPartitionSelfFence_KillsSandboxesReversibly pins the partition
// self-fence (TFAC-586): a heartbeat WRITE FAILURE that persists past
// selfFenceDeadline latches PartitionFenced and kills live sandboxes — the
// same reaction as identity supersession — but, unlike IdentityFenced,
// un-fences automatically the moment a heartbeat write succeeds again, and
// never invokes the supersession exit hook.
func TestCheckPartitionSelfFence_KillsSandboxesReversibly(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")
	store := &toggleableInstanceStore{}
	s.instances = store
	s.SetExecutorID("partition-instance", 1)
	s.SetSelfFenceDeadline(30 * time.Millisecond)
	s.mu.Lock()
	s.lastGoodContactAt = time.Now()
	s.mu.Unlock()

	killed := false
	s.mu.Lock()
	s.cancels["run-live"] = func() { killed = true }
	s.mu.Unlock()

	var exitCalls int
	s.SetOnSupersessionFence(func() { exitCalls++ })

	store.setFail(true)
	ctx := context.Background()

	if !s.heartbeatOnce(ctx) {
		t.Fatal("a heartbeat write failure must keep the loop running (not evidence of supersession)")
	}
	if s.PartitionFenced() {
		t.Fatal("must not fence before the self-fence deadline elapses")
	}
	if killed {
		t.Fatal("must not kill sandboxes before the deadline elapses")
	}

	time.Sleep(40 * time.Millisecond)
	if !s.heartbeatOnce(ctx) {
		t.Fatal("a partition self-fence must not stop the heartbeat loop — it is reversible")
	}
	if !s.PartitionFenced() {
		t.Fatal("expected the partition self-fence to latch past the deadline")
	}
	if !killed {
		t.Error("expected killAllLiveSandboxes to have cancelled the registered run")
	}
	if exitCalls != 0 {
		t.Errorf("onSupersessionFence called %d times, want 0 — a partition must never exit the process", exitCalls)
	}

	store.setFail(false)
	if !s.heartbeatOnce(ctx) {
		t.Fatal("a successful heartbeat must keep the loop running")
	}
	if s.PartitionFenced() {
		t.Error("a successful heartbeat write must un-fence the partition case")
	}
}
