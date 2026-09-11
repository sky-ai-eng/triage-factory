package pgtest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestHarness_RevivesAfterContainerDeath pins the self-heal path: on a
// loaded CI runner the kernel OOM killer sometimes takes out the shared
// container's postmaster mid-suite, which without recovery fails every
// remaining Postgres test in the binary at Reset ("unexpected EOF" from
// the dying connections, then "connection refused" forever after).
// Terminating the container simulates that death; Reset must boot a
// replacement and leave the harness fully usable — migrated schema,
// seeds, RLS ceremony — through the same Harness pointer tests already
// hold.
//
// It also pins the layer below, which is where the recovery is easiest
// to break: the pools and the stores built over them are captured
// before the death and must still work after it. That is how the
// conformance suites hold their stores — once, above the per-subtest
// factory — so a revive that handed back replacement handles would
// leave the captures on a closed pool and cascade anyway, one level
// down from where the cascade was fixed.
//
// Costs one extra container boot and one unit of the process's revive
// budget; the suites that run after this one exercise the replacement,
// which is extra validation, not a hazard.
func TestHarness_RevivesAfterContainerDeath(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	// Capture before the death, the way a conformance suite does.
	// Nothing below may re-read h.AdminDB to get a working handle —
	// that would test a harness field, not what a store holds.
	adminDB, appDB, systemDB := h.AdminDB, h.AppDB, h.SystemDB
	stores := pgstore.New(h.AdminDB, h.AppDB, SecretKey)

	termCtx, termCancel := context.WithTimeout(context.Background(), 60*time.Second)
	if err := h.Container.Terminate(termCtx); err != nil {
		termCancel()
		t.Fatalf("terminate container: %v", err)
	}
	termCancel()

	// The first DB touch after the death — must revive, not t.Fatal.
	h.Reset(t)

	if h.AdminDB != adminDB || h.AppDB != appDB || h.SystemDB != systemDB {
		t.Fatalf("revive replaced the harness's pool handles; every store captured before the death now holds a closed pool")
	}

	var n int
	if err := adminDB.QueryRow(`SELECT COUNT(*) FROM events_catalog`).Scan(&n); err != nil {
		t.Fatalf("query events_catalog through the captured admin pool: %v", err)
	}
	if n == 0 {
		t.Errorf("events_catalog has no rows on revived container — migrations/seeds didn't run")
	}

	// A real store method, on the bundle built before the death.
	// Instances is admin-pooled and needs no org or RLS ceremony, so
	// what it proves is exactly the pool swap.
	if _, err := stores.Instances.Register(t.Context(), "revive-check", "control", "test", ""); err != nil {
		t.Fatalf("Register through the store captured before the death: %v", err)
	}
	got, err := stores.Instances.Get(t.Context(), "revive-check")
	if err != nil {
		t.Fatalf("Get through the store captured before the death: %v", err)
	}
	if got == nil {
		t.Fatalf("registered instance reads back as missing on the revived container")
	}

	orgID, userID, _ := SeedOrgWithUser(t, h, "revive-check")
	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		var one int
		return tx.QueryRow(`SELECT 1`).Scan(&one)
	}); err != nil {
		t.Fatalf("WithUser on revived container: %v", err)
	}

	// SystemDB is aimed by the same bring-up; a pool nothing else in
	// this test touches would otherwise only fail much later, in the
	// tf_system suite.
	if err := systemDB.PingContext(t.Context()); err != nil {
		t.Fatalf("ping the captured system pool after revive: %v", err)
	}
}
