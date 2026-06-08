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
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
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
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
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
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
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
	a := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	b := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
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
	database := newTakeoverTestDB(t)
	seedRun(t, database, "r-exec", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6", "")

	s.stampExecutor(runmode.LocalDefaultOrg, "r-exec")

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
	database := newTakeoverTestDB(t)
	seedRun(t, database, "r-active", "sess", "/tmp/wt") // status running
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6", "")
	fired := make(chan struct{}, 1)
	s.cancels["r-active"] = func() { fired <- struct{}{} }

	if err := s.Cancel(runmode.LocalDefaultOrg, "r-active", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-fired:
	default:
		t.Error("expected Cancel to fire the registered cancel func via the controller")
	}
}
