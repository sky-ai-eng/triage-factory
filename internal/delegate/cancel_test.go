package delegate

import (
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeDrainer captures DrainEntity invocations so tests can assert
// the spawner's terminal-state hooks fire correctly. Synchronized
// because notifyDrainer dispatches the call in a goroutine.
type drainCall struct {
	orgID    string
	entityID string
}

type fakeDrainer struct {
	mu     sync.Mutex
	calls  []drainCall
	called chan struct{}
}

func newFakeDrainer() *fakeDrainer {
	return &fakeDrainer{called: make(chan struct{}, 8)}
}

func (f *fakeDrainer) DrainEntity(orgID, entityID string) {
	f.mu.Lock()
	f.calls = append(f.calls, drainCall{orgID: orgID, entityID: entityID})
	f.mu.Unlock()
	select {
	case f.called <- struct{}{}:
	default:
	}
}

func (f *fakeDrainer) callsCopy() []drainCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]drainCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestCancel_AwaitingInputAutoRun_DrainsQueue pins the fix for the
// "Cancel without active goroutine never calls notifyDrainer" leak.
// An auto-fired run parked in awaiting_input has no goroutine defer
// to piggy-back on, so without the explicit drain the per-entity
// firing queue would stick until some other run terminated. SKY-139.
func TestCancel_AwaitingInputAutoRun_DrainsQueue(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "r1", "sess-1", "/tmp/wt-r1")
	if _, err := database.Exec(`UPDATE runs SET status = 'awaiting_input', trigger_type = 'event', creator_user_id = NULL WHERE id = 'r1'`); err != nil {
		t.Fatalf("park run: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6", "")
	drainer := newFakeDrainer()
	s.SetQueueDrainer(drainer)

	if err := s.Cancel(runmode.LocalDefaultOrg, "r1", ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// notifyDrainer dispatches in a goroutine — wait briefly.
	select {
	case <-drainer.called:
	case <-time.After(time.Second):
		t.Fatal("DrainEntity was never called")
	}

	calls := drainer.callsCopy()
	if len(calls) != 1 {
		t.Fatalf("expected 1 drain call, got %d (%v)", len(calls), calls)
	}
	if calls[0].entityID == "" {
		t.Errorf("DrainEntity called with empty entityID")
	}
	if calls[0].orgID != runmode.LocalDefaultOrg {
		t.Errorf("DrainEntity orgID = %q, want %q", calls[0].orgID, runmode.LocalDefaultOrg)
	}
}

// TestCancel_AwaitingInputManualRun_NoDrain confirms the manual
// short-circuit still applies when Cancel hits the no-goroutine
// path. notifyDrainer is the spot that filters trigger_type=manual,
// not the caller, so this is a regression guard against someone
// later adding the filter at the call site instead.
func TestCancel_AwaitingInputManualRun_NoDrain(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "r-manual", "sess-2", "/tmp/wt-rm")
	// Manual is the seedRun default but we set it explicitly for
	// clarity and pin to awaiting_input.
	if _, err := database.Exec(`UPDATE runs SET status = 'awaiting_input', trigger_type = 'manual' WHERE id = 'r-manual'`); err != nil {
		t.Fatalf("park run: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6", "")
	drainer := newFakeDrainer()
	s.SetQueueDrainer(drainer)

	if err := s.Cancel(runmode.LocalDefaultOrg, "r-manual", ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Give the would-be drainer goroutine a window to fire if the
	// filter regresses. notifyDrainer's manual-filter is synchronous
	// so we won't see a real call here, but a sleep keeps the test
	// honest without making it slow.
	select {
	case <-drainer.called:
		t.Fatal("DrainEntity called for manual run; should be filtered by trigger_type")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestCancel_AlreadyTerminal_NoDrain confirms we don't double-drain
// a row that some other path already terminated. Without the
// "only on flipped == true" guard, a stale Cancel on a completed
// run would fire a redundant drain.
func TestCancel_AlreadyTerminal_NoDrain(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "r-done", "sess-3", "/tmp/wt-rd")
	// Trigger_type='event' requires creator_user_id IS NULL per the
	// SKY-261 v0.9 CHECK invariant. seedRun defaults to manual +
	// sentinel creator; the UPDATE has to clear creator alongside.
	if _, err := database.Exec(`UPDATE runs SET status = 'completed', trigger_type = 'event', creator_user_id = NULL WHERE id = 'r-done'`); err != nil {
		t.Fatalf("complete run: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6", "")
	drainer := newFakeDrainer()
	s.SetQueueDrainer(drainer)

	if err := s.Cancel(runmode.LocalDefaultOrg, "r-done", ""); err == nil {
		t.Fatal("expected 'no active run' error on terminal row")
	}

	select {
	case <-drainer.called:
		t.Fatal("DrainEntity called on already-terminal row")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestCancel_AwaitingInputStep_FinalizesBlueprintRun pins the fix for the
// orphaned-blueprint_run leak: cancelling a yield-parked step through the
// DB-only path (no live orchestrator goroutine) must also finalize the owning
// blueprint_run, not just the run row. Pre-fix the blueprint_run stuck in
// 'running' forever (and its snapshot orphaned). seedRun links the run to a
// 1-step blueprint_run "seedbpr-<runID>".
func TestCancel_AwaitingInputStep_FinalizesBlueprintRun(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "r-step", "sess-step", "/tmp/wt-rs")
	if _, err := database.Exec(`UPDATE runs SET status = 'awaiting_input' WHERE id = 'r-step'`); err != nil {
		t.Fatalf("park run: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6", "")

	// User-initiated cancel.
	if err := s.Cancel(runmode.LocalDefaultOrg, "r-step", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	var runStatus, bpStatus string
	if err := database.QueryRow(`SELECT status FROM runs WHERE id = 'r-step'`).Scan(&runStatus); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if runStatus != "cancelled" {
		t.Errorf("run status = %q, want cancelled", runStatus)
	}
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id = 'seedbpr-r-step'`).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run status: %v", err)
	}
	if bpStatus != "cancelled" {
		t.Errorf("blueprint_run status = %q, want cancelled (a cancelled parked step must not strand the blueprint_run in 'running')", bpStatus)
	}
}
