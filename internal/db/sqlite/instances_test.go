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
	matched, draining, err := stores.Instances.Heartbeat(ctx, id, epoch, domain.InstanceHeartbeat{
		MaxRuns:        &maxRuns,
		ActiveRuns:     &activeRuns,
		MemTotalMB:     &memTotal,
		MemAvailableMB: &memAvail,
		DispatchGated:  &gated,
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !matched {
		t.Fatal("Heartbeat against the current boot_epoch should match the row")
	}
	if draining {
		t.Fatal("a freshly-registered instance must not read back draining=true")
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
	if after.LastHeartbeatAt.Before(before.LastHeartbeatAt) {
		t.Errorf("expected last_heartbeat_at to advance (or hold, under coarse clock resolution): before=%v after=%v", before.LastHeartbeatAt, after.LastHeartbeatAt)
	}

	// draining and labels are operator/control-plane intent, written
	// out-of-band (the drain verb / placement tooling). A heartbeat must
	// never touch them — a 4s renewal loop that reset draining=false would
	// silently cancel a drain within one tick of the operator setting it.
	if _, err := conn.Exec(`UPDATE instances SET draining = 1, labels_json = '{"pool":"gpu"}' WHERE id = ?`, id); err != nil {
		t.Fatalf("seed operator intent: %v", err)
	}

	// A heartbeat with every pointer left nil clears the capacity snapshot
	// back to NULL (capacity is heartbeat-owned and overwritten wholesale)
	// — while the intent columns survive untouched. draining is now true
	// (seeded above), and Heartbeat must read it back.
	matched, draining, err = stores.Instances.Heartbeat(ctx, id, epoch, domain.InstanceHeartbeat{})
	if err != nil {
		t.Fatalf("Heartbeat (clear): %v", err)
	}
	if !matched {
		t.Fatal("clearing heartbeat should still match the row")
	}
	if !draining {
		t.Fatal("Heartbeat must read back the current draining value, not the stale one from registration")
	}
	cleared, err := stores.Instances.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get (after clear): %v", err)
	}
	if cleared.MaxRuns != nil || cleared.ActiveRuns != nil || cleared.MemTotalMB != nil ||
		cleared.MemAvailableMB != nil || cleared.DispatchGated != nil {
		t.Errorf("expected every capacity field to read back nil after a nil-valued heartbeat, got %+v", cleared)
	}
	if !cleared.Draining {
		t.Error("heartbeat clobbered draining — operator intent must survive renewals")
	}
	if cleared.LabelsJSON != `{"pool":"gpu"}` {
		t.Errorf("heartbeat clobbered labels: %q, want the operator-set json", cleared.LabelsJSON)
	}

	// A restart (Register) bumps the epoch and clears the dead boot's
	// capacity snapshot — but operator intent survives restarts too: a
	// drained instance stays drained until explicitly un-drained.
	freshHB := domain.InstanceHeartbeat{MaxRuns: &maxRuns}
	if _, _, err := stores.Instances.Heartbeat(ctx, id, epoch, freshHB); err != nil {
		t.Fatalf("Heartbeat (repopulate): %v", err)
	}
	if _, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v2"); err != nil {
		t.Fatalf("Register (restart): %v", err)
	}
	rebooted, err := stores.Instances.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get (after restart): %v", err)
	}
	if rebooted.MaxRuns != nil {
		t.Errorf("Register left the prior boot's capacity snapshot in place: MaxRuns=%v, want nil", rebooted.MaxRuns)
	}
	if !rebooted.Draining {
		t.Error("Register clobbered draining — operator intent must survive restarts")
	}
	if rebooted.LabelsJSON != `{"pool":"gpu"}` {
		t.Errorf("Register clobbered labels: %q, want the operator-set json", rebooted.LabelsJSON)
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

	matched, _, err := stores.Instances.Heartbeat(ctx, id, staleEpoch, domain.InstanceHeartbeat{})
	if err != nil {
		t.Fatalf("Heartbeat (stale epoch): %v", err)
	}
	if matched {
		t.Fatal("a heartbeat carrying a superseded boot_epoch must not match the row")
	}
}

// TestInstanceStore_SQLite_SetDrainingAndList pins the CLI drain verb's
// store-layer contract (TFAC-586) in local mode: SetDraining flips the
// flag and reports whether the id was known, and List surfaces every
// registered instance (trivially one, at N=1).
func TestInstanceStore_SQLite_SetDrainingAndList(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	const id = "66666666-6666-6666-6666-666666666666"
	if _, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	matched, err := stores.Instances.SetDraining(ctx, id, true)
	if err != nil {
		t.Fatalf("SetDraining(true): %v", err)
	}
	if !matched {
		t.Fatal("SetDraining should match a registered id")
	}

	rows, err := stores.Instances.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || !rows[0].Draining {
		t.Fatalf("List = %+v, want exactly one drained row", rows)
	}

	matched, err = stores.Instances.SetDraining(ctx, "unknown-id", true)
	if err != nil {
		t.Fatalf("SetDraining(unknown): %v", err)
	}
	if matched {
		t.Error("SetDraining against an unregistered id must report matched=false")
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
