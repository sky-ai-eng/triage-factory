package delegate

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSetExecutorID_OverridesConstructorDefault pins that SetExecutorID
// replaces NewSpawner's random per-boot uuid default, and that
// stampExecutor picks up the override — runs.executor_id on claim must
// equal the registry id, not a per-boot random uuid.
func TestSetExecutorID_OverridesConstructorDefault(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")

	defaultID, defaultEpoch := s.executorIdentity()
	if defaultID == "" {
		t.Fatal("expected NewSpawner to default to a non-empty random executor id")
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
	epoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "test-version")
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
	epoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "test-version")
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
// (production always calls it before starting this loop).
func TestRunInstanceHeartbeat_NoExecutorIDIsNoOp(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")
	s.mu.Lock()
	s.executorID = "" // simulate SetExecutorID never having run
	s.mu.Unlock()
	s.RunInstanceHeartbeat(context.Background(), DefaultInstanceHeartbeatInterval)
}
