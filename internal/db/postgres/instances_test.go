package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestInstanceStore_Postgres_RegisterMintsAndBumpsEpoch pins the acceptance
// criterion directly: a first Register mints boot_epoch=1, and a second
// Register against the SAME id (simulating a restart) bumps it to 2 while
// refreshing role/version — never minting a second row. Admin-pool only
// (no org to scope, so no RLS to exercise).
func TestInstanceStore_Postgres_RegisterMintsAndBumpsEpoch(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	const id = "11111111-1111-1111-1111-111111111111"

	epoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1.0.0", "")
	if err != nil {
		t.Fatalf("Register (first boot): %v", err)
	}
	if epoch != 1 {
		t.Fatalf("first boot epoch = %d, want 1", epoch)
	}

	epoch, err = stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1.0.1", "")
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
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM instances`).Scan(&count); err != nil {
		t.Fatalf("count instances: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one row across two Registers of the same id, got %d", count)
	}
}

// TestInstanceStore_Postgres_HeartbeatRoundTripsCapacitySnapshot pins that
// Heartbeat renews last_heartbeat_at and writes the full capacity +
// admission snapshot (wholesale — an all-nil heartbeat clears it), that
// nil pointer fields read back as nil (not a spurious zero), and that the
// operator-intent columns (draining, labels) survive both heartbeats and
// restarts untouched.
func TestInstanceStore_Postgres_HeartbeatRoundTripsCapacitySnapshot(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	const id = "22222222-2222-2222-2222-222222222222"

	epoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
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

	// draining and labels are operator/control-plane intent, written
	// out-of-band (the drain verb / placement tooling). A heartbeat must
	// never touch them — a 4s renewal loop that reset draining=false would
	// silently cancel a drain within one tick of the operator setting it.
	if _, err := h.AdminDB.Exec(`UPDATE instances SET draining = true, labels = '{"pool":"gpu"}'::jsonb WHERE id = $1`, id); err != nil {
		t.Fatalf("seed operator intent: %v", err)
	}

	// An all-nil heartbeat clears the capacity snapshot back to NULL
	// (capacity is heartbeat-owned, overwritten wholesale) — while the
	// intent columns survive untouched. draining is now true (seeded
	// above), and Heartbeat must read it back — the mechanism the drain
	// verb relies on for a running instance to learn it was drained.
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
	var labels map[string]any
	if err := json.Unmarshal([]byte(cleared.LabelsJSON), &labels); err != nil {
		t.Fatalf("labels not valid json after heartbeat: %q (%v)", cleared.LabelsJSON, err)
	}
	if labels["pool"] != "gpu" {
		t.Errorf("heartbeat clobbered labels: %v, want pool=gpu", labels)
	}

	// A restart (Register) bumps the epoch and clears the dead boot's
	// capacity snapshot — but operator intent survives restarts too: a
	// drained instance stays drained until explicitly un-drained.
	if _, _, err := stores.Instances.Heartbeat(ctx, id, epoch, domain.InstanceHeartbeat{MaxRuns: &maxRuns}); err != nil {
		t.Fatalf("Heartbeat (repopulate): %v", err)
	}
	if _, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v2", ""); err != nil {
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
	if err := json.Unmarshal([]byte(rebooted.LabelsJSON), &labels); err != nil {
		t.Fatalf("labels not valid json after restart: %q (%v)", rebooted.LabelsJSON, err)
	}
	if labels["pool"] != "gpu" {
		t.Errorf("Register clobbered labels: %v, want pool=gpu", labels)
	}
}

// TestInstanceStore_Postgres_HeartbeatFencedOnBootEpoch pins the
// split-identity fence: a heartbeat carrying a STALE boot_epoch
// (superseded by a later Register) matches no row.
func TestInstanceStore_Postgres_HeartbeatFencedOnBootEpoch(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	const id = "33333333-3333-3333-3333-333333333333"

	staleEpoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1", "")
	if err != nil {
		t.Fatalf("Register (boot 1): %v", err)
	}
	// A second boot of the same id (e.g. a duplicated state root) bumps the
	// epoch out from under the first boot's in-memory copy.
	if _, err := stores.Instances.Register(ctx, id, domain.InstanceRoleAll, "v1", ""); err != nil {
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

// TestInstanceStore_Postgres_SetDrainingAndList pins the CLI drain verb's
// store-layer contract (TFAC-586): SetDraining flips the flag and reports
// whether the id was known, and List surfaces every registered instance —
// the substrate `triagefactory instance list/drain/undrain` reads and
// writes.
func TestInstanceStore_Postgres_SetDrainingAndList(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	const idA = "44444444-4444-4444-4444-444444444444"
	const idB = "55555555-5555-5555-5555-555555555555"

	if _, err := stores.Instances.Register(ctx, idA, domain.InstanceRoleExecutor, "v1", ""); err != nil {
		t.Fatalf("Register A: %v", err)
	}
	if _, err := stores.Instances.Register(ctx, idB, domain.InstanceRoleExecutor, "v1", ""); err != nil {
		t.Fatalf("Register B: %v", err)
	}

	matched, err := stores.Instances.SetDraining(ctx, idA, true)
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
	if len(rows) != 2 {
		t.Fatalf("List returned %d rows, want 2", len(rows))
	}
	byID := map[string]domain.Instance{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if !byID[idA].Draining {
		t.Errorf("instance %s: Draining = false, want true after SetDraining(true)", idA)
	}
	if byID[idB].Draining {
		t.Errorf("instance %s: Draining = true, want false (never drained)", idB)
	}

	matched, err = stores.Instances.SetDraining(ctx, idA, false)
	if err != nil {
		t.Fatalf("SetDraining(false): %v", err)
	}
	if !matched {
		t.Fatal("SetDraining(false) should match a registered id")
	}
	after, err := stores.Instances.Get(ctx, idA)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Draining {
		t.Error("expected Draining=false after SetDraining(false)")
	}

	matched, err = stores.Instances.SetDraining(ctx, "unknown-id", true)
	if err != nil {
		t.Fatalf("SetDraining(unknown): %v", err)
	}
	if matched {
		t.Error("SetDraining against an unregistered id must report matched=false")
	}
}

// TestInstanceStore_Postgres_GetUnknownIDReturnsNil pins the "no row"
// contract: Get returns (nil, nil) rather than an error for an id never
// registered.
func TestInstanceStore_Postgres_GetUnknownIDReturnsNil(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	got, err := stores.Instances.Get(context.Background(), "unknown-id")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for an unregistered id, got %+v", got)
	}
}

// TestInstanceStore_Postgres_GCConformance runs the shared registry GC suite
// against the admin pool, the only pool the instances table is written on.
func TestInstanceStore_Postgres_GCConformance(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunInstanceGCConformance(t, func(t *testing.T) dbtest.InstanceGCFixture {
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		return dbtest.InstanceGCFixture{
			Store: stores.Instances,
			BackdateHeartbeat: func(t *testing.T, id string, ago time.Duration) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`UPDATE instances SET last_heartbeat_at = now() - make_interval(secs => $1) WHERE id = $2`, ago.Seconds(), id)
			},
		}
	})
}

// TestClaimsExecutorID_HasNoForeignKeyToInstances pins the schema invariant
// the registry GC's safety depends on: claims.executor_id is a plain text
// column with no FK constraint into instances, so deleting an instances row
// can never cascade into (or be blocked by) claims.
func TestClaimsExecutorID_HasNoForeignKeyToInstances(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	var count int
	if err := h.AdminDB.QueryRow(`
		SELECT COUNT(*)
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema
		WHERE tc.table_name = 'claims' AND kcu.column_name = 'executor_id' AND tc.constraint_type = 'FOREIGN KEY'
	`).Scan(&count); err != nil {
		t.Fatalf("query constraints: %v", err)
	}
	if count != 0 {
		t.Fatalf("claims.executor_id has %d foreign key constraint(s), want 0 — a FK here would make GC deletes cascade into (or be blocked by) audit history", count)
	}
}
