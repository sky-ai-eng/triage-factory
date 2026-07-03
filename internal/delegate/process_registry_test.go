package delegate

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestProcRegistry_RegisterGetDeregister covers the registry accessors the
// control seam depends on: a registered run is reachable by id and gone
// after deregister, an unregistered run reads nil.
func TestProcRegistry_RegisterGetDeregister(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	if s.getProc("missing") != nil {
		t.Fatal("expected nil for an unregistered run")
	}
	lr := &agentproc.LiveRun{}
	s.registerProc("org-1", "run-1", lr)
	h := s.getProc("run-1")
	if h == nil {
		t.Fatal("expected a handle after register")
	}
	if h.lr != lr || h.runID != "run-1" || h.orgID != "org-1" {
		t.Errorf("handle mismatch: %+v", h)
	}
	s.deregisterProc("run-1")
	if s.getProc("run-1") != nil {
		t.Error("expected nil after deregister")
	}
}

// TestController_CancelRoutesToRegisteredCancel pins the seam Cancel relies
// on: the in-process controller fires the registered ctx cancel and reports
// found=true, and reports found=false for an unregistered run (the caller's
// DB-only branch).
func TestController_CancelRoutesToRegisteredCancel(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	called := false
	s.cancels["run-x"] = func() { called = true }

	if !s.controller.Cancel("run-x") {
		t.Fatal("expected Cancel to find the registered handle")
	}
	if !called {
		t.Error("expected the registered cancel func to fire")
	}
	if s.controller.Cancel("run-absent") {
		t.Error("expected Cancel to report not-found for an unregistered run")
	}
}

// TestController_InterruptSteerNoProcErrors guards the no-live-process
// branch the P3 endpoints will surface as a 4xx: Interrupt/Steer error
// rather than panic when the run has no registered handle.
func TestController_InterruptSteerNoProcErrors(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	if err := s.Interrupt(context.Background(), "nope"); !errors.Is(err, ErrNoLiveProcess) {
		t.Errorf("Interrupt err = %v, want ErrNoLiveProcess when no live process is registered", err)
	}
	if err := s.Steer(context.Background(), "nope", "hi"); !errors.Is(err, ErrNoLiveProcess) {
		t.Errorf("Steer err = %v, want ErrNoLiveProcess when no live process is registered", err)
	}
}

// TestSpawnerExecutorIDDistinctPerInstance pins that each spawner instance
// gets its own executor identity — the N=1 model where one process owns its
// runs, and a restart re-stamps re-claimed runs under a fresh id.
func TestSpawnerExecutorIDDistinctPerInstance(t *testing.T) {
	a := NewSpawner(nil, db.Stores{}, nil, nil, "")
	b := NewSpawner(nil, db.Stores{}, nil, nil, "")
	if a.executorID == "" || b.executorID == "" {
		t.Fatal("expected non-empty executor ids")
	}
	if a.executorID == b.executorID {
		t.Error("expected distinct executor ids per spawner instance")
	}
}

// TestStampExecutor_WritesExecutorID is the acceptance check for "a live run
// stamps executor_id": stampExecutor writes the instance id onto the row.
func TestStampExecutor_WritesExecutorID(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-exec", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	s.stampExecutor(runmode.LocalDefaultOrgID, "r-exec")

	var got sql.NullString
	if err := database.QueryRow(`SELECT executor_id FROM runs WHERE id='r-exec'`).Scan(&got); err != nil {
		t.Fatalf("read executor_id: %v", err)
	}
	if !got.Valid || got.String != s.executorID {
		t.Errorf("executor_id = %v, want %q", got, s.executorID)
	}
}

// TestCancel_ActiveRun_RoutesThroughController verifies the live-run cancel
// path: an active run (a registered cancel handle) is killed via the
// controller rather than the DB-only path.
func TestCancel_ActiveRun_RoutesThroughController(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-active", "sess", "/tmp/wt") // status running
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	fired := make(chan struct{}, 1)
	s.cancels["r-active"] = func() { fired <- struct{}{} }

	if err := s.Cancel(runmode.LocalDefaultOrgID, "r-active", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-fired:
	default:
		t.Error("expected Cancel to fire the registered cancel func via the controller")
	}
}

func TestParseMaxConcurrentRuns(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		want        int
		wantClamped bool
		wantErr     bool
	}{
		{"empty uses default", "", DefaultMaxConcurrentRuns, false, false},
		{"whitespace uses default", "  ", DefaultMaxConcurrentRuns, false, false},
		{"plain value", "32", 32, false, false},
		{"trims whitespace", " 8 ", 8, false, false},
		{"one is legal", "1", 1, false, false},
		{"ceiling exactly", "256", 256, false, false},
		{"above ceiling clamps", "1000", MaxConcurrentRunsCeiling, true, false},
		{"zero is invalid", "0", DefaultMaxConcurrentRuns, false, true},
		{"negative is invalid", "-3", DefaultMaxConcurrentRuns, false, true},
		{"garbage is invalid", "lots", DefaultMaxConcurrentRuns, false, true},
		{"float is invalid", "4.5", DefaultMaxConcurrentRuns, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, clamped, err := ParseMaxConcurrentRuns(tt.raw)
			if got != tt.want {
				t.Errorf("ParseMaxConcurrentRuns(%q) = %d, want %d", tt.raw, got, tt.want)
			}
			if clamped != tt.wantClamped {
				t.Errorf("ParseMaxConcurrentRuns(%q) clamped = %v, want %v", tt.raw, clamped, tt.wantClamped)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseMaxConcurrentRuns(%q) err = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
		})
	}
}

func TestParseDispatchMemFloorMB(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{"empty uses default", "", DefaultDispatchMemFloorMB, false},
		{"whitespace uses default", " ", DefaultDispatchMemFloorMB, false},
		{"zero disables", "0", 0, false},
		{"plain value", "8192", 8192, false},
		{"trims whitespace", " 2048 ", 2048, false},
		{"negative is invalid", "-1", DefaultDispatchMemFloorMB, true},
		{"garbage is invalid", "much", DefaultDispatchMemFloorMB, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDispatchMemFloorMB(tt.raw)
			if got != tt.want {
				t.Errorf("ParseDispatchMemFloorMB(%q) = %d, want %d", tt.raw, got, tt.want)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseDispatchMemFloorMB(%q) err = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
		})
	}
}

// TestDispatchMemGated pins the guardrail's decision table: disabled
// floor and unknown probe fail open; a probe below the floor gates;
// recovery ungates. The fake probe swaps values without sleeping so
// the transition logging path is exercised in both directions.
func TestDispatchMemGated(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")

	// Floor unset (zero) → never gates, regardless of probe.
	s.memAvailMB = func() int { return 1 }
	if s.dispatchMemGated() {
		t.Error("gated with no floor configured")
	}

	// Unknown probe → fails open.
	s.SetDispatchMemFloor(4096)
	s.memAvailMB = func() int { return -1 }
	if s.dispatchMemGated() {
		t.Error("gated on an Unknown probe; guardrail must fail open")
	}

	// Below floor → gates.
	s.memAvailMB = func() int { return 1024 }
	if !s.dispatchMemGated() {
		t.Error("not gated with available below floor")
	}
	// Still below → stays gated (idempotent re-check).
	if !s.dispatchMemGated() {
		t.Error("gate did not hold on re-check")
	}

	// Recovery → ungates.
	s.memAvailMB = func() int { return 60000 }
	if s.dispatchMemGated() {
		t.Error("still gated after memory recovered above floor")
	}

	// Exactly at the floor is NOT gated (floor is "defer when below").
	s.memAvailMB = func() int { return 4096 }
	if s.dispatchMemGated() {
		t.Error("gated at exactly the floor; boundary is strict-below")
	}
}
