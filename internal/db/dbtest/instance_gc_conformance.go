package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// InstanceGCFixture is one subtest's world for the registry GC suite.
type InstanceGCFixture struct {
	Store db.InstanceStore

	// BackdateHeartbeat moves an instance's last_heartbeat_at `ago` into the
	// past, on the clock and in the layout the dialect's heartbeat stamps it
	// with.
	BackdateHeartbeat func(t *testing.T, id string, ago time.Duration)
}

// InstanceGCFactory builds a fresh fixture per subtest.
type InstanceGCFactory func(t *testing.T) InstanceGCFixture

// RunInstanceGCConformance is the shared assertion suite for
// InstanceStore.DeleteStaleSystem: a row whose heartbeat is older than the
// threshold goes, a fresher one stays, and an id that comes back after being
// collected simply registers again from boot epoch 1.
func RunInstanceGCConformance(t *testing.T, mk InstanceGCFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("DeletesOnlyRowsStalerThanTheThreshold", func(t *testing.T) {
		f := mk(t)
		const stale, fresh = "gc-stale-instance", "gc-fresh-instance"
		for _, id := range []string{stale, fresh} {
			if _, err := f.Store.Register(ctx, id, domain.InstanceRoleExecutor, "v1", ""); err != nil {
				t.Fatalf("Register(%s): %v", id, err)
			}
		}
		f.BackdateHeartbeat(t, stale, 8*24*time.Hour)
		f.BackdateHeartbeat(t, fresh, time.Hour)

		n, err := f.Store.DeleteStaleSystem(ctx, 7*24*time.Hour)
		if err != nil {
			t.Fatalf("DeleteStaleSystem: %v", err)
		}
		if n != 1 {
			t.Fatalf("deleted %d rows, want 1", n)
		}
		if got, err := f.Store.Get(ctx, stale); err != nil || got != nil {
			t.Errorf("the stale row survived: (%+v, %v)", got, err)
		}
		if got, err := f.Store.Get(ctx, fresh); err != nil || got == nil {
			t.Errorf("the fresh row was deleted: (%+v, %v)", got, err)
		}
		if again, err := f.Store.DeleteStaleSystem(ctx, 7*24*time.Hour); err != nil || again != 0 {
			t.Errorf("a second sweep = (%d, %v), want (0, nil)", again, err)
		}
	})

	t.Run("ACollectedIDRegistersAgainFromEpochOne", func(t *testing.T) {
		f := mk(t)
		const id = "gc-returning-instance"
		for i := 0; i < 3; i++ {
			if _, err := f.Store.Register(ctx, id, domain.InstanceRoleExecutor, "v1", ""); err != nil {
				t.Fatalf("Register: %v", err)
			}
		}
		f.BackdateHeartbeat(t, id, 8*24*time.Hour)
		if _, err := f.Store.DeleteStaleSystem(ctx, 7*24*time.Hour); err != nil {
			t.Fatalf("DeleteStaleSystem: %v", err)
		}
		epoch, err := f.Store.Register(ctx, id, domain.InstanceRoleExecutor, "v2", "")
		if err != nil {
			t.Fatalf("Register after collection: %v", err)
		}
		if epoch != 1 {
			t.Errorf("boot epoch after collection = %d, want 1 — the row has no memory of the one deleted", epoch)
		}
	})
}
