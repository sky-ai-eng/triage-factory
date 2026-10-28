package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// InstanceDrainFactory builds a fresh, empty InstanceStore per subtest.
type InstanceDrainFactory func(t *testing.T) db.InstanceStore

// RunInstanceDrainConformance is the shared assertion suite for the two
// kinds of drain: SetDraining's, which survives a restart, and
// SetDrainUntilRestart's, which is in effect until the instance next
// registers. Every surface that reports the drain — Heartbeat's read-back,
// Get and List — must report the one in effect.
func RunInstanceDrainConformance(t *testing.T, mk InstanceDrainFactory) {
	t.Helper()
	ctx := context.Background()

	register := func(t *testing.T, s db.InstanceStore, id string) int64 {
		t.Helper()
		epoch, err := s.Register(ctx, id, domain.InstanceRoleExecutor, "v1", "")
		if err != nil {
			t.Fatalf("Register(%s): %v", id, err)
		}
		return epoch
	}
	// wantDrain asserts the drain every read reports for id at bootEpoch.
	wantDrain := func(t *testing.T, s db.InstanceStore, id string, bootEpoch int64, draining, untilRestart bool) {
		t.Helper()
		got, err := s.Get(ctx, id)
		if err != nil || got == nil {
			t.Fatalf("Get(%s) = (%+v, %v)", id, got, err)
		}
		if got.Draining != draining || got.DrainUntilRestart != untilRestart {
			t.Errorf("Get: Draining=%t DrainUntilRestart=%t; want %t, %t", got.Draining, got.DrainUntilRestart, draining, untilRestart)
		}
		rows, err := s.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		found := false
		for _, r := range rows {
			if r.ID != id {
				continue
			}
			found = true
			if r.Draining != draining || r.DrainUntilRestart != untilRestart {
				t.Errorf("List: Draining=%t DrainUntilRestart=%t; want %t, %t", r.Draining, r.DrainUntilRestart, draining, untilRestart)
			}
		}
		if !found {
			t.Errorf("List has no row for %s", id)
		}
		matched, hbDraining, err := s.Heartbeat(ctx, id, bootEpoch, domain.InstanceHeartbeat{})
		if err != nil || !matched {
			t.Fatalf("Heartbeat(%s, %d) = (%t, %v)", id, bootEpoch, matched, err)
		}
		if hbDraining != draining {
			t.Errorf("Heartbeat read back draining=%t; want %t", hbDraining, draining)
		}
	}

	t.Run("ADrainUntilRestartLapsesAtTheNextRegister", func(t *testing.T) {
		s := mk(t)
		const id = "drain-until-restart"
		epoch := register(t, s, id)
		wantDrain(t, s, id, epoch, false, false)

		matched, err := s.SetDrainUntilRestart(ctx, id)
		if err != nil || !matched {
			t.Fatalf("SetDrainUntilRestart = (%t, %v); want (true, nil)", matched, err)
		}
		wantDrain(t, s, id, epoch, true, true)

		epoch = register(t, s, id)
		wantDrain(t, s, id, epoch, false, false)

		// The value left behind names an epoch that never comes back.
		epoch = register(t, s, id)
		wantDrain(t, s, id, epoch, false, false)
	})

	t.Run("APersistentDrainSurvivesRegister", func(t *testing.T) {
		s := mk(t)
		const id = "drain-persistent"
		register(t, s, id)
		if matched, err := s.SetDraining(ctx, id, true); err != nil || !matched {
			t.Fatalf("SetDraining(true) = (%t, %v); want (true, nil)", matched, err)
		}
		epoch := register(t, s, id)
		wantDrain(t, s, id, epoch, true, false)
	})

	t.Run("UndrainEndsEitherKind", func(t *testing.T) {
		s := mk(t)
		const restartOnly, both = "undrain-restart-only", "undrain-both"
		restartEpoch := register(t, s, restartOnly)
		bothEpoch := register(t, s, both)

		if matched, err := s.SetDrainUntilRestart(ctx, restartOnly); err != nil || !matched {
			t.Fatalf("SetDrainUntilRestart = (%t, %v)", matched, err)
		}
		if _, err := s.SetDraining(ctx, both, true); err != nil {
			t.Fatalf("SetDraining(true): %v", err)
		}
		if _, err := s.SetDrainUntilRestart(ctx, both); err != nil {
			t.Fatalf("SetDrainUntilRestart: %v", err)
		}
		// The persistent drain outlives the restart, so it is the one in
		// effect.
		wantDrain(t, s, both, bothEpoch, true, false)

		for id, epoch := range map[string]int64{restartOnly: restartEpoch, both: bothEpoch} {
			if matched, err := s.SetDraining(ctx, id, false); err != nil || !matched {
				t.Fatalf("SetDraining(%s, false) = (%t, %v)", id, matched, err)
			}
			wantDrain(t, s, id, epoch, false, false)
		}
	})

	t.Run("UnknownIDMatchesNothing", func(t *testing.T) {
		s := mk(t)
		matched, err := s.SetDrainUntilRestart(ctx, "never-registered")
		if err != nil {
			t.Fatalf("SetDrainUntilRestart: %v", err)
		}
		if matched {
			t.Error("SetDrainUntilRestart on an unregistered id reported a match")
		}
	})
}
