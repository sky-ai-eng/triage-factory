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

	// wrote asserts a drain write found its row and returned it as stored:
	// the same row, through the same columns, a point read finds.
	wrote := func(t *testing.T, s db.InstanceStore, what, id string, inst *domain.Instance, err error) {
		t.Helper()
		if err != nil || inst == nil {
			t.Fatalf("%s = (%+v, %v); want the row", what, inst, err)
		}
		AssertWriteReturnedStoredRow(t, what, *inst, func() (*domain.Instance, error) { return s.Get(ctx, id) })
	}

	t.Run("ADrainUntilRestartLapsesAtTheNextRegister", func(t *testing.T) {
		s := mk(t)
		const id = "drain-until-restart"
		epoch := register(t, s, id)
		wantDrain(t, s, id, epoch, false, false)

		inst, err := s.SetDrainUntilRestart(ctx, id)
		wrote(t, s, "SetDrainUntilRestart", id, inst, err)
		if !inst.Draining || !inst.DrainUntilRestart {
			t.Errorf("SetDrainUntilRestart returned Draining=%t DrainUntilRestart=%t; want true, true", inst.Draining, inst.DrainUntilRestart)
		}
		wantDrain(t, s, id, epoch, true, true)

		epoch = register(t, s, id)
		wantDrain(t, s, id, epoch, false, false)

		// The value left behind names an epoch that never comes back.
		epoch = register(t, s, id)
		wantDrain(t, s, id, epoch, false, false)
		// An unknown id matches nothing.
		{
			inst, err := s.SetDrainUntilRestart(ctx, "never-registered")
			if err != nil {
				t.Fatalf("SetDrainUntilRestart: %v", err)
			}
			if inst != nil {
				t.Errorf("SetDrainUntilRestart on an unregistered id returned %+v", inst)
			}
			if inst, err := s.SetDraining(ctx, "never-registered", true); err != nil || inst != nil {
				t.Errorf("SetDraining on an unregistered id = (%+v, %v); want (nil, nil)", inst, err)
			}
		}
	})

	t.Run("APersistentDrainSurvivesRegister", func(t *testing.T) {
		s := mk(t)
		const id = "drain-persistent"
		register(t, s, id)
		inst, err := s.SetDraining(ctx, id, true)
		wrote(t, s, "SetDraining(true)", id, inst, err)
		if !inst.Draining || inst.DrainUntilRestart {
			t.Errorf("SetDraining(true) returned Draining=%t DrainUntilRestart=%t; want true, false", inst.Draining, inst.DrainUntilRestart)
		}
		epoch := register(t, s, id)
		wantDrain(t, s, id, epoch, true, false)
	})

	t.Run("UndrainEndsEitherKind", func(t *testing.T) {
		s := mk(t)
		const restartOnly, both = "undrain-restart-only", "undrain-both"
		restartEpoch := register(t, s, restartOnly)
		bothEpoch := register(t, s, both)

		if inst, err := s.SetDrainUntilRestart(ctx, restartOnly); err != nil || inst == nil {
			t.Fatalf("SetDrainUntilRestart = (%+v, %v)", inst, err)
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
			inst, err := s.SetDraining(ctx, id, false)
			wrote(t, s, "SetDraining(false)", id, inst, err)
			if inst.Draining || inst.DrainUntilRestart {
				t.Errorf("SetDraining(%s, false) returned Draining=%t DrainUntilRestart=%t; want false, false", id, inst.Draining, inst.DrainUntilRestart)
			}
			wantDrain(t, s, id, epoch, false, false)
		}
	})

}
