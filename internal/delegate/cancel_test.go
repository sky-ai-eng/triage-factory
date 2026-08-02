package delegate

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// fakeDrainer captures DrainEntity invocations so tests can assert
// the spawner's terminal-state hooks fire correctly. Synchronized
// because notifyDrainer dispatches the call in a goroutine.
type drainCall struct {
	orgID  string
	taskID string
}

type fakeDrainer struct {
	mu     sync.Mutex
	calls  []drainCall
	called chan struct{}
}

func newFakeDrainer() *fakeDrainer {
	return &fakeDrainer{called: make(chan struct{}, 8)}
}

func (f *fakeDrainer) DrainTask(orgID, taskID string) {
	f.mu.Lock()
	f.calls = append(f.calls, drainCall{orgID: orgID, taskID: taskID})
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

// TestCancel_OpenAutoRun_DrainsQueue pins the fix for the
// "Cancel without active goroutine never calls notifyDrainer" leak.
// An auto-fired run parked `open` has no goroutine defer to piggy-back on, so
// without the explicit drain the per-entity firing queue would stick until some
// other run terminated.
func TestCancel_OpenAutoRun_DrainsQueue(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r1", "sess-1", "/tmp/wt-r1")
	if _, err := database.Exec(`UPDATE conversations SET status = 'open', trigger_type = 'event', creator_user_id = NULL WHERE id = 'r1'`); err != nil {
		t.Fatalf("park run: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	drainer := newFakeDrainer()
	s.SetQueueDrainer(drainer)

	if err := s.Cancel(runmode.LocalDefaultOrgID, "r1", ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// notifyDrainer dispatches in a goroutine — wait briefly.
	select {
	case <-drainer.called:
	case <-time.After(time.Second):
		t.Fatal("DrainTask was never called")
	}

	calls := drainer.callsCopy()
	if len(calls) != 1 {
		t.Fatalf("expected 1 drain call, got %d (%v)", len(calls), calls)
	}
	if calls[0].taskID == "" {
		t.Errorf("DrainTask called with empty taskID")
	}
	if calls[0].orgID != runmode.LocalDefaultOrgID {
		t.Errorf("DrainTask orgID = %q, want %q", calls[0].orgID, runmode.LocalDefaultOrgID)
	}
}

// TestCancel_OpenManualRun_NoDrain confirms the manual short-circuit still
// applies when Cancel hits the no-goroutine path. notifyDrainer is the spot
// that filters trigger_type=manual, not the caller, so this is a regression
// guard against someone later adding the filter at the call site instead.
func TestCancel_OpenManualRun_NoDrain(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-manual", "sess-2", "/tmp/wt-rm")
	// Manual is the seedRun default but we set it explicitly for
	// clarity and pin to `open`.
	if _, err := database.Exec(`UPDATE conversations SET status = 'open', trigger_type = 'manual' WHERE id = 'r-manual'`); err != nil {
		t.Fatalf("park run: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	drainer := newFakeDrainer()
	s.SetQueueDrainer(drainer)

	if err := s.Cancel(runmode.LocalDefaultOrgID, "r-manual", ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Give the would-be drainer goroutine a window to fire if the
	// filter regresses. notifyDrainer's manual-filter is synchronous
	// so we won't see a real call here, but a sleep keeps the test
	// honest without making it slow.
	select {
	case <-drainer.called:
		t.Fatal("DrainTask called for manual run; should be filtered by trigger_type")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestCancel_AlreadyTerminal_NoDrain confirms we don't double-drain
// a row that some other path already terminated. Without the
// "only on flipped == true" guard, a stale Cancel on a completed
// run would fire a redundant drain.
func TestCancel_AlreadyTerminal_NoDrain(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-done", "sess-3", "/tmp/wt-rd")
	// Trigger_type='event' requires creator_user_id IS NULL per the
	// CHECK invariant. seedRun defaults to manual +
	// sentinel creator; the UPDATE has to clear creator alongside.
	if _, err := database.Exec(`UPDATE conversations SET status = 'completed', trigger_type = 'event', creator_user_id = NULL WHERE id = 'r-done'`); err != nil {
		t.Fatalf("complete run: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	drainer := newFakeDrainer()
	s.SetQueueDrainer(drainer)

	if err := s.Cancel(runmode.LocalDefaultOrgID, "r-done", ""); err == nil {
		t.Fatal("expected 'no active run' error on terminal row")
	}

	select {
	case <-drainer.called:
		t.Fatal("DrainTask called on already-terminal row")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestCancel_OpenStep_FinalizesBlueprintRun pins the fix for the
// orphaned-blueprint_run leak: cancelling an open-parked step through the
// DB-only path (no live orchestrator goroutine) must also finalize the owning
// blueprint_run, not just the run row. Pre-fix the blueprint_run stuck in
// 'running' forever. seedRun links the run to a 1-step blueprint_run
// "seedbpr-<runID>".
//
// It also pins where the cancellation is spelled: the blueprint takes the
// 'cancelled' terminal and raises cancel_requested, while the conversation
// stays parked `open` with its workspace.
func TestCancel_OpenStep_FinalizesBlueprintRun(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-step", "sess-step", "/tmp/wt-rs")
	if _, err := database.Exec(`UPDATE conversations SET status = 'open' WHERE id = 'r-step'`); err != nil {
		t.Fatalf("park run: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	// User-initiated cancel.
	if err := s.Cancel(runmode.LocalDefaultOrgID, "r-step", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	var runStatus, bpStatus, stopReason string
	var cancelRequested bool
	if err := database.QueryRow(`SELECT status, COALESCE(stop_reason, '') FROM conversations WHERE id = 'r-step'`).Scan(&runStatus, &stopReason); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if runStatus != "open" || stopReason != "user_cancelled" {
		t.Errorf("run = (%q, %q), want (open, user_cancelled) — a cancel parks, it never writes a terminal of its own", runStatus, stopReason)
	}
	if err := database.QueryRow(`SELECT status, cancel_requested FROM blueprint_runs WHERE id = 'seedbpr-r-step'`).Scan(&bpStatus, &cancelRequested); err != nil {
		t.Fatalf("read blueprint_run status: %v", err)
	}
	if bpStatus != "cancelled" {
		t.Errorf("blueprint_run status = %q, want cancelled (a cancelled parked step must not strand the blueprint_run in 'running')", bpStatus)
	}
	if !cancelRequested {
		t.Error("blueprint_run cancel_requested = false; the signal is what tells a parked step apart from a killed one")
	}
}

// TestParkRunOpen_StoppedKeepsTheWorkspace is the cancel-costs-disk
// trade, pinned. The old cancel terminal removed the worktree on its way out,
// which threw the workspace away at exactly the moment a user who just killed
// a wedged run is most likely to want it back. Now the run parks `open`, the
// snapshot is written BEFORE the flip (so a resume that lands without the warm
// worktree can still rebuild it), and the warm tree is left for the
// blueprint's own cleanup.
func TestParkRunOpen_StoppedKeepsTheWorkspace(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, runID, taskID := setupAdvanceFixture(t, "cancel-park")
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s.SetStorage(blobs)

	// A workspace with something in it that only the snapshot can carry.
	wtPath := t.TempDir()
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.txt"), "work in progress")
	namespace := blueprintRunIDForRun(t, database, runID)

	if fenced := s.parkRunOpen(liveParkContext{
		orgID:       runmode.LocalDefaultOrgID,
		runID:       runID,
		taskID:      taskID,
		namespace:   namespace,
		claudeCwd:   wtPath,
		triggerType: "event",
		reason:      db.ParkStopped("system_cancelled", "Cancelled by system"),
	}, ""); fenced {
		t.Fatal("parkRunOpen reported a fence trip on an unfenced store")
	}

	var status, stopReason string
	var parked bool
	if err := database.QueryRow(
		`SELECT status, COALESCE(stop_reason, ''), parked_at IS NOT NULL FROM conversations WHERE id = ?`, runID,
	).Scan(&status, &stopReason, &parked); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "open" || stopReason != "system_cancelled" {
		t.Errorf("run = (%q, %q), want (open, system_cancelled)", status, stopReason)
	}
	if !parked {
		t.Error("parked_at unset; the snapshot-retention sweep keys the parked workspace off it")
	}

	// The snapshot exists…
	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, namespace))
	if err != nil {
		t.Fatalf("cancelled run wrote no workspace snapshot: %v", err)
	}
	_ = rc.Close()
	// …and the warm worktree was not torn down under it.
	if _, err := os.Stat(filepath.Join(wtPath, "_tfac", "notes.txt")); err != nil {
		t.Errorf("worktree removed by the cancel (%v); parking retains it and the TTL is the reaper", err)
	}
}

// TestCancel_AlreadyTerminal_LeavesBlueprintAlone: a stale cancel — a click on
// a run that already concluded — must stay a pure no-op. The blueprint-layer
// signal is the reason this needs its own guard: raising cancel_requested on a
// blueprint that has moved on to its next step would cancel work the user never
// aimed at.
func TestCancel_AlreadyTerminal_LeavesBlueprintAlone(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-stale", "sess-stale", "/tmp/wt-stale")
	if _, err := database.Exec(`UPDATE conversations SET status = 'completed', outcome = 'continue' WHERE id = 'r-stale'`); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.Cancel(runmode.LocalDefaultOrgID, "r-stale", runmode.LocalDefaultUserID); err == nil {
		t.Fatal("expected 'no active run' on a concluded run")
	}

	var status string
	var cancelRequested bool
	if err := database.QueryRow(
		`SELECT status, cancel_requested FROM blueprint_runs WHERE id = 'seedbpr-r-stale'`,
	).Scan(&status, &cancelRequested); err != nil {
		t.Fatalf("read blueprint_run: %v", err)
	}
	if status != "running" || cancelRequested {
		t.Errorf("blueprint = (%q, cancel_requested=%v), want (running, false) — a stale cancel must not stop the next step", status, cancelRequested)
	}
}
