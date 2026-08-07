package delegate

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/hostmem"
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

// TestSpawnerExecutorIdentityIndependentPerInstance pins that spawner
// identity is per-instance state wired via SetExecutorID — unset (empty)
// at construction, and setting one spawner's identity never leaks into
// another. The empty default is deliberate: a missed wiring stays
// observable (heartbeat refuses to start, stamps bind NULL) instead of
// minting a plausible-looking random uuid.
func TestSpawnerExecutorIdentityIndependentPerInstance(t *testing.T) {
	a := NewSpawner(nil, db.Stores{}, nil, nil, "")
	b := NewSpawner(nil, db.Stores{}, nil, nil, "")
	if a.executorID != "" || b.executorID != "" {
		t.Fatal("expected empty executor ids before SetExecutorID wires them")
	}
	a.SetExecutorID("instance-a", 1)
	if b.executorID != "" {
		t.Error("wiring one spawner's identity leaked into another instance")
	}
	b.SetExecutorID("instance-b", 1)
	if a.executorID == b.executorID {
		t.Error("expected distinct executor ids per spawner instance")
	}
}

// TestStampExecutor_WritesExecutorID is the acceptance check for "a live run
// stamps executor_id": stampExecutor confirms the wired instance id on the
// conversation's active claim — and an unwired spawner (no SetExecutorID)
// leaves the conversation unclaimed, never claiming under a fabricated id.
func TestStampExecutor_WritesExecutorID(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-exec", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	// Unwired: the empty id releases rather than mints, so no active claim
	// may exist — an invented identity would show up here.
	s.stampExecutor(runmode.LocalDefaultOrgID, "r-exec", "")
	var active int
	if err := database.QueryRow(`SELECT COUNT(*) FROM claims WHERE conversation_id='r-exec' AND released_at IS NULL`).Scan(&active); err != nil {
		t.Fatalf("count active claims: %v", err)
	}
	if active != 0 {
		t.Errorf("active claims = %d, want 0 from an unwired spawner", active)
	}

	s.SetExecutorID("persistent-registry-id", 5)
	s.stampExecutor(runmode.LocalDefaultOrgID, "r-exec", "")
	var got string
	if err := database.QueryRow(`SELECT executor_id FROM claims WHERE conversation_id='r-exec' AND released_at IS NULL`).Scan(&got); err != nil {
		t.Fatalf("read active claim executor_id: %v", err)
	}
	if got != "persistent-registry-id" {
		t.Errorf("executor_id = %q, want the wired registry id", got)
	}
}

// TestStop_ActiveRun_RoutesThroughController verifies the live-run stop
// path: an active run (a registered cancel handle) is killed via the
// controller rather than the DB-only path.
func TestStop_ActiveRun_RoutesThroughController(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-active", "sess", "/tmp/wt") // status running
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	fired := make(chan struct{}, 1)
	s.cancels["r-active"] = func() { fired <- struct{}{} }

	if err := s.Stop(runmode.LocalDefaultOrgID, "r-active", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-fired:
	default:
		t.Error("expected Stop to fire the registered cancel func via the controller")
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
	s.memAvailMB = func() int { return hostmem.Unknown }
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

func TestDerivedRunCapacity(t *testing.T) {
	tests := []struct {
		name    string
		totalMB int
		want    int
	}{
		{"below reserve is zero", 8 * 1024, 0},
		{"exactly reserve is zero", DefaultPlatformReserveMB, 0},
		{"16GB host", 16 * 1024, 16},
		{"32GB host", 32 * 1024, 80},
		{"64GB host", 64 * 1024, 208},
		{"96GB hits the ceiling", 96 * 1024, MaxConcurrentRunsCeiling},
		{"huge host stays at ceiling", 512 * 1024, MaxConcurrentRunsCeiling},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DerivedRunCapacity(tt.totalMB); got != tt.want {
				t.Errorf("DerivedRunCapacity(%d) = %d, want %d", tt.totalMB, got, tt.want)
			}
		})
	}
}

// TestExecutorReserve_RescuesSmallPodCapacity pins the executor-role
// platform reserve (TFAC-582): an ~8 GB executor pod derives 0 advisory
// capacity under the all-in-one 12 GB reserve, but a real, non-zero capacity
// under the much smaller executor reserve — the whole reason the smaller
// reserve exists.
func TestExecutorReserve_RescuesSmallPodCapacity(t *testing.T) {
	const smallPodMB = 8 * 1024

	if DerivedRunCapacityWithReserve(smallPodMB, DefaultPlatformReserveMB) != 0 {
		t.Fatalf("precondition: an %dMB host under the all-in-one reserve should derive 0", smallPodMB)
	}
	got := DerivedRunCapacityWithReserve(smallPodMB, DefaultExecutorPlatformReserveMB)
	want := (smallPodMB - DefaultExecutorPlatformReserveMB) / DefaultRunMemoryBudgetMB
	if got != want {
		t.Errorf("DerivedRunCapacityWithReserve(%d, executor reserve) = %d, want %d", smallPodMB, got, want)
	}
	if got == 0 {
		t.Error("the executor reserve must derive a non-zero capacity on a normal executor pod")
	}
	if DefaultExecutorPlatformReserveMB >= DefaultPlatformReserveMB {
		t.Error("the executor reserve must be smaller than the all-in-one reserve")
	}
}

func TestParsePlatformReserveMB(t *testing.T) {
	cases := []struct {
		raw     string
		def     int
		want    int
		wantErr bool
	}{
		{"", DefaultExecutorPlatformReserveMB, DefaultExecutorPlatformReserveMB, false},
		{"4096", DefaultExecutorPlatformReserveMB, 4096, false},
		{"0", DefaultPlatformReserveMB, 0, false},
		{"-1", DefaultPlatformReserveMB, DefaultPlatformReserveMB, true},
		{"nope", DefaultPlatformReserveMB, DefaultPlatformReserveMB, true},
	}
	for _, tc := range cases {
		got, err := ParsePlatformReserveMB(tc.raw, tc.def)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParsePlatformReserveMB(%q) err = %v, wantErr %v", tc.raw, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("ParsePlatformReserveMB(%q, %d) = %d, want %d", tc.raw, tc.def, got, tc.want)
		}
	}
}

// TestNoteCapSaturationTransitions pins the saturation episode's state
// machine: a blocked acquire with an empty queue stays quiet (full
// utilization is not a backlog), a blocked acquire with queued work opens
// the episode exactly once, and an immediate acquire closes it.
func TestNoteCapSaturationTransitions(t *testing.T) {
	s, database, _, _, run0 := reactorFixture(t, "capsat", 1, "completed", "finish")
	ctx := context.Background()

	s.noteCapAcquireBlocked(ctx, 4)
	if s.capSaturated.Load() {
		t.Error("blocked acquire with an empty queue must not open a saturation episode")
	}

	if _, err := database.Exec(`UPDATE conversations SET status = NULL WHERE id = ?`, run0); err != nil {
		t.Fatalf("force queued: %v", err)
	}
	s.noteCapAcquireBlocked(ctx, 4)
	if !s.capSaturated.Load() {
		t.Error("blocked acquire with a queued run must open a saturation episode")
	}
	// Re-entry while already saturated is a no-op, not a second episode.
	s.noteCapAcquireBlocked(ctx, 4)
	if !s.capSaturated.Load() {
		t.Error("episode must stay open across repeated blocked acquires")
	}

	s.noteCapAcquireImmediate(4)
	if s.capSaturated.Load() {
		t.Error("an immediate acquire must close the saturation episode")
	}
}

// TestNoteCapSaturation_NilRunQueueSafe guards test/partial-wiring setups: a
// spawner with no RunQueueStore must not panic when the dispatcher helpers
// fire.
func TestNoteCapSaturation_NilRunQueueSafe(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.noteCapAcquireBlocked(context.Background(), 4)
	if s.capSaturated.Load() {
		t.Error("nil run queue must never open a saturation episode")
	}
	s.noteCapAcquireImmediate(4)
}
