package delegate

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestHealthAccessors_Defaults pins the executor healthz seam's initial
// readings on a fresh spawner: the dispatcher isn't alive until RunDispatcher
// starts, no heartbeat has been written, active runs reflect the semaphore,
// and draining toggles through Set/Get.
func TestHealthAccessors_Defaults(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")

	if s.DispatcherAlive() {
		t.Error("DispatcherAlive() = true before RunDispatcher started")
	}
	if _, everWritten := s.LastHeartbeatWriteAge(); everWritten {
		t.Error("LastHeartbeatWriteAge everWritten = true before any heartbeat")
	}
	if s.ActiveRuns() != 0 {
		t.Errorf("ActiveRuns() = %d, want 0 on a fresh spawner", s.ActiveRuns())
	}
	if s.Draining() {
		t.Error("Draining() = true by default")
	}
	s.SetDraining(true)
	if !s.Draining() {
		t.Error("Draining() = false after SetDraining(true)")
	}

	// ActiveRuns tracks the concurrency semaphore.
	sem := s.semaphore()
	sem <- struct{}{}
	defer func() { <-sem }()
	if s.ActiveRuns() != 1 {
		t.Errorf("ActiveRuns() = %d, want 1 with one held slot", s.ActiveRuns())
	}
}

// TestHeartbeatOnce_StampsWriteTime pins that a successful heartbeat makes
// LastHeartbeatWriteAge report everWritten=true (the executor healthz's
// liveness signal).
func TestHeartbeatOnce_StampsWriteTime(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)
	s := NewSpawner(database, stores, nil, nil, "")

	ctx := context.Background()
	const id = "hb-write-time"
	epoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleExecutor, "v")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	s.SetExecutorID(id, epoch)

	if !s.heartbeatOnce(ctx) {
		t.Fatal("heartbeatOnce returned false (unexpected supersession)")
	}
	if _, everWritten := s.LastHeartbeatWriteAge(); !everWritten {
		t.Error("LastHeartbeatWriteAge everWritten = false after a successful heartbeat")
	}
}

// TestHeartbeatOnce_ReportCapacityFalse_LeavesCapacityNil pins the
// control-pod shape: with SetReportCapacity(false), a heartbeat still renews
// liveness but leaves the executor-only capacity columns NULL (domain.Instance
// documents them as "meaningful only for executor-capable roles").
func TestHeartbeatOnce_ReportCapacityFalse_LeavesCapacityNil(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)
	s := NewSpawner(database, stores, nil, nil, "")
	s.SetReportCapacity(false)

	ctx := context.Background()
	const id = "hb-control"
	epoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleControl, "v")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	s.SetExecutorID(id, epoch)

	if !s.heartbeatOnce(ctx) {
		t.Fatal("heartbeatOnce returned false (unexpected supersession)")
	}

	got, err := stores.Instances.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("expected a registered row")
	}
	if got.MaxRuns != nil || got.ActiveRuns != nil || got.DispatchGated != nil {
		t.Errorf("control heartbeat wrote capacity fields: MaxRuns=%v ActiveRuns=%v DispatchGated=%v — want all nil",
			got.MaxRuns, got.ActiveRuns, got.DispatchGated)
	}
	// Liveness still renewed.
	if _, everWritten := s.LastHeartbeatWriteAge(); !everWritten {
		t.Error("control heartbeat should still stamp the write time")
	}
}
