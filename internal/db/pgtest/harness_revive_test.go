package pgtest

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestHarness_RevivesAfterContainerDeath pins the self-heal path: on a
// loaded CI runner the kernel OOM killer sometimes takes out the shared
// container's postmaster mid-suite, which used to fail every remaining
// Postgres test in the binary at Reset ("unexpected EOF" from the dying
// connections, then "connection refused" forever after). Stopping the
// container simulates that death; Reset must boot a replacement and
// leave the harness fully usable — migrated schema, seeds, RLS ceremony
// — through the same Harness pointer tests already hold.
//
// Costs one extra container boot and one unit of the process's revive
// budget; the suites that run after this one exercise the replacement,
// which is extra validation, not a hazard.
func TestHarness_RevivesAfterContainerDeath(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stopTimeout := 10 * time.Second
	if err := h.Container.Stop(ctx, &stopTimeout); err != nil {
		t.Fatalf("stop container: %v", err)
	}

	// The first DB touch after the death — must revive, not t.Fatal.
	h.Reset(t)

	var n int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM events_catalog`).Scan(&n); err != nil {
		t.Fatalf("query events_catalog on revived container: %v", err)
	}
	if n == 0 {
		t.Errorf("events_catalog has no rows on revived container — migrations/seeds didn't run")
	}

	orgID, userID, _ := SeedOrgWithUser(t, h, "revive-check")
	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		var one int
		return tx.QueryRow(`SELECT 1`).Scan(&one)
	}); err != nil {
		t.Fatalf("WithUser on revived container: %v", err)
	}
}
