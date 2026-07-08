package sqlite_test

import (
	"context"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestInstanceStore_SQLite_RegisterMintsAndBumpsEpoch pins the acceptance
// criterion directly: a first Register mints boot_epoch=1, and a second
// Register against the SAME id (simulating a restart) bumps it to 2 while
// refreshing role/version — never minting a second row.
func TestInstanceStore_SQLite_RegisterMintsAndBumpsEpoch(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	const id = "11111111-1111-1111-1111-111111111111"

	epoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1.0.0")
	if err != nil {
		t.Fatalf("Register (first boot): %v", err)
	}
	if epoch != 1 {
		t.Fatalf("first boot epoch = %d, want 1", epoch)
	}

	epoch, err = stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1.0.1")
	if err != nil {
		t.Fatalf("Register (restart): %v", err)
	}
	if epoch != 2 {
		t.Fatalf("restart boot epoch = %d, want 2", epoch)
	}

	got, err := stores.Instances.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("expected a row after two Registers")
	}
	if got.BootEpoch != 2 {
		t.Errorf("stored boot_epoch = %d, want 2", got.BootEpoch)
	}
	if got.Version != "v1.0.1" {
		t.Errorf("stored version = %q, want the latest register's %q", got.Version, "v1.0.1")
	}

	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM instances`).Scan(&count); err != nil {
		t.Fatalf("count instances: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one row across two Registers of the same id, got %d", count)
	}
}

// TestInstanceStore_SQLite_HeartbeatRoundTripsCapacitySnapshot pins that
// Heartbeat renews last_heartbeat_at and writes the full capacity +
// admission snapshot, and that nil pointer fields read back as nil (not a
// spurious zero).
func TestInstanceStore_SQLite_HeartbeatRoundTripsCapacitySnapshot(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	const id = "22222222-2222-2222-2222-222222222222"
	epoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	before, err := stores.Instances.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get (before heartbeat): %v", err)
	}

	maxRuns, activeRuns, memTotal, memAvail := 4, 1, 8192, 6000
	gated := true
	matched, err := stores.Instances.Heartbeat(ctx, id, epoch, domain.InstanceHeartbeat{
		Draining:       false,
		MaxRuns:        &maxRuns,
		ActiveRuns:     &activeRuns,
		MemTotalMB:     &memTotal,
		MemAvailableMB: &memAvail,
		DispatchGated:  &gated,
		LabelsJSON:     `{"zone":"us-east"}`,
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !matched {
		t.Fatal("Heartbeat against the current boot_epoch should match the row")
	}

	after, err := stores.Instances.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get (after heartbeat): %v", err)
	}
	if after == nil {
		t.Fatal("expected a row")
	}
	if after.MaxRuns == nil || *after.MaxRuns != maxRuns {
		t.Errorf("MaxRuns = %v, want %d", after.MaxRuns, maxRuns)
	}
	if after.ActiveRuns == nil || *after.ActiveRuns != activeRuns {
		t.Errorf("ActiveRuns = %v, want %d", after.ActiveRuns, activeRuns)
	}
	if after.MemTotalMB == nil || *after.MemTotalMB != memTotal {
		t.Errorf("MemTotalMB = %v, want %d", after.MemTotalMB, memTotal)
	}
	if after.MemAvailableMB == nil || *after.MemAvailableMB != memAvail {
		t.Errorf("MemAvailableMB = %v, want %d", after.MemAvailableMB, memAvail)
	}
	if after.DispatchGated == nil || !*after.DispatchGated {
		t.Errorf("DispatchGated = %v, want true", after.DispatchGated)
	}
	if after.LabelsJSON != `{"zone":"us-east"}` {
		t.Errorf("LabelsJSON = %q, want the seeded json", after.LabelsJSON)
	}
	if after.LastHeartbeatAt.Before(before.LastHeartbeatAt) {
		t.Errorf("expected last_heartbeat_at to advance (or hold, under coarse clock resolution): before=%v after=%v", before.LastHeartbeatAt, after.LastHeartbeatAt)
	}

	// A second heartbeat with every pointer left nil clears the snapshot back to
	// NULL — Heartbeat overwrites wholesale, it doesn't merge.
	matched, err = stores.Instances.Heartbeat(ctx, id, epoch, domain.InstanceHeartbeat{})
	if err != nil {
		t.Fatalf("Heartbeat (clear): %v", err)
	}
	if !matched {
		t.Fatal("clearing heartbeat should still match the row")
	}
	cleared, err := stores.Instances.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get (after clear): %v", err)
	}
	if cleared.MaxRuns != nil || cleared.ActiveRuns != nil || cleared.MemTotalMB != nil ||
		cleared.MemAvailableMB != nil || cleared.DispatchGated != nil || cleared.LabelsJSON != "" {
		t.Errorf("expected every optional field to read back nil/empty after a nil-valued heartbeat, got %+v", cleared)
	}
}

// TestInstanceStore_SQLite_HeartbeatFencedOnBootEpoch pins the
// split-identity fence: a heartbeat carrying a STALE boot_epoch
// (superseded by a later Register) matches no row.
func TestInstanceStore_SQLite_HeartbeatFencedOnBootEpoch(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	const id = "33333333-3333-3333-3333-333333333333"
	staleEpoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1")
	if err != nil {
		t.Fatalf("Register (boot 1): %v", err)
	}
	// A second boot of the same id (e.g. a duplicated state root) bumps the
	// epoch out from under the first boot's in-memory copy.
	if _, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1"); err != nil {
		t.Fatalf("Register (boot 2): %v", err)
	}

	matched, err := stores.Instances.Heartbeat(ctx, id, staleEpoch, domain.InstanceHeartbeat{})
	if err != nil {
		t.Fatalf("Heartbeat (stale epoch): %v", err)
	}
	if matched {
		t.Fatal("a heartbeat carrying a superseded boot_epoch must not match the row")
	}
}

// TestInstanceStore_SQLite_GetUnknownIDReturnsNil pins the "no row" contract:
// Get returns (nil, nil) rather than an error for an id never registered.
func TestInstanceStore_SQLite_GetUnknownIDReturnsNil(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	got, err := stores.Instances.Get(ctx, "unknown-id")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for an unregistered id, got %+v", got)
	}
}
