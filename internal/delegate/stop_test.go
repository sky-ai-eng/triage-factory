package delegate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// fakeDrainer captures DrainTask invocations so tests can assert
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

// TestStop_OpenAutoRun_DrainsQueue pins the fix for the
// "stop without active goroutine never calls notifyDrainer" leak.
// An auto-fired run parked `open` has no goroutine defer to piggy-back on, so
// without the explicit drain the task's firing queue would stick until some
// other run terminated.
func TestStop_OpenAutoRun_DrainsQueue(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r1", "sess-1", "/tmp/wt-r1")
	if _, err := database.Exec(`UPDATE conversations SET status = 'open', trigger_type = 'event', creator_user_id = NULL WHERE id = 'r1'`); err != nil {
		t.Fatalf("park run: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	drainer := newFakeDrainer()
	s.SetQueueDrainer(drainer)

	if err := s.Stop(runmode.LocalDefaultOrgID, "r1", ""); err != nil {
		t.Fatalf("stop: %v", err)
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

// TestStop_OpenManualRun_NoDrain confirms the manual short-circuit still
// applies when a stop hits the no-goroutine path. notifyDrainer is the spot
// that filters trigger_type=manual, not the caller, so this is a regression
// guard against someone later adding the filter at the call site instead.
func TestStop_OpenManualRun_NoDrain(t *testing.T) {
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

	if err := s.Stop(runmode.LocalDefaultOrgID, "r-manual", ""); err != nil {
		t.Fatalf("stop: %v", err)
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

// TestStop_AlreadyTerminal_NoDrain confirms we don't double-drain
// a row that some other path already terminated. Without the
// "only on flipped == true" guard, a stale stop on a completed
// run would fire a redundant drain.
func TestStop_AlreadyTerminal_NoDrain(t *testing.T) {
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

	if err := s.Stop(runmode.LocalDefaultOrgID, "r-done", ""); err == nil {
		t.Fatal("expected 'no active run' error on terminal row")
	}

	select {
	case <-drainer.called:
		t.Fatal("DrainTask called on already-terminal row")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestStop_OpenStep_FreezesBlueprintRun is the stop verb's central claim: a
// conversation-level stop parks the conversation and touches nothing else. The
// blueprint stays 'running' with no queued step and no live claim — frozen, not
// finished — which is what leaves the parked step claimable again on resume.
//
// The old behavior raised cancel_requested here and finalized the blueprint
// 'cancelled', which made `open` mean two different things depending on a
// column no user can see: resumable under a running blueprint, dead forever
// under a cancelled one, decided by which button was pressed.
func TestStop_OpenStep_FreezesBlueprintRun(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-step", "sess-step", "/tmp/wt-rs")
	if _, err := database.Exec(`UPDATE conversations SET status = 'open' WHERE id = 'r-step'`); err != nil {
		t.Fatalf("park run: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.Stop(runmode.LocalDefaultOrgID, "r-step", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	var runStatus, bpStatus, stopReason string
	var cancelRequested bool
	if err := database.QueryRow(`SELECT status, COALESCE(stop_reason, '') FROM conversations WHERE id = 'r-step'`).Scan(&runStatus, &stopReason); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if runStatus != "open" || stopReason != "user_cancelled" {
		t.Errorf("run = (%q, %q), want (open, user_cancelled) — a stop parks, it never writes a terminal of its own", runStatus, stopReason)
	}
	if err := database.QueryRow(`SELECT status, cancel_requested FROM blueprint_runs WHERE id = 'seedbpr-r-step'`).Scan(&bpStatus, &cancelRequested); err != nil {
		t.Fatalf("read blueprint_run status: %v", err)
	}
	if bpStatus != "running" {
		t.Errorf("blueprint_run status = %q, want running — a stop freezes the plan, it does not finalize it", bpStatus)
	}
	if cancelRequested {
		t.Error("blueprint_run cancel_requested = true; no conversation-level operation may raise it — the claim gate would then refuse the parked step forever")
	}
}

// TestStopAndCancelBlueprint_OpenStep_FinalizesBlueprintRun pins the other
// half of the split: when the layer above the conversation has already ended
// — a closed or swiped task, an archived team — the teardown verb carries the
// blueprint terminal with it. Nothing will resume these conversations, so a
// frozen 'running' blueprint would hold a worktree and count as live work
// forever. seedRun links the run to a 1-step blueprint_run "seedbpr-<runID>".
func TestStopAndCancelBlueprint_OpenStep_FinalizesBlueprintRun(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-teardown", "sess-teardown", "/tmp/wt-rt")
	if _, err := database.Exec(`UPDATE conversations SET status = 'open' WHERE id = 'r-teardown'`); err != nil {
		t.Fatalf("park run: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.StopAndCancelBlueprint(runmode.LocalDefaultOrgID, "r-teardown", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	var runStatus, bpStatus string
	var cancelRequested bool
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = 'r-teardown'`).Scan(&runStatus); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if runStatus != "open" {
		t.Errorf("run status = %q, want open — even a teardown parks the conversation rather than writing a terminal on it", runStatus)
	}
	if err := database.QueryRow(`SELECT status, cancel_requested FROM blueprint_runs WHERE id = 'seedbpr-r-teardown'`).Scan(&bpStatus, &cancelRequested); err != nil {
		t.Fatalf("read blueprint_run status: %v", err)
	}
	if bpStatus != "cancelled" {
		t.Errorf("blueprint_run status = %q, want cancelled (a torn-down parked step must not strand the blueprint_run in 'running')", bpStatus)
	}
	if !cancelRequested {
		t.Error("blueprint_run cancel_requested = false; the signal is what stops the claim gate handing out the next step in the teardown window")
	}
}

// TestCancelBlueprint_FinalizesRunAndParksSteps pins the layer the stop verb
// hands cancellation off TO. Now that stopping a conversation only freezes the
// plan, this is the only verb that can end one — a blueprint sitting 'running'
// with no queued step and no live claim has no other exit short of resuming
// the conversation or dispositioning the task.
//
// It lives beside the stop tests on purpose: the pair is the split. The
// conversation verb parks and freezes; the blueprint verb finalizes. Both park
// the step, and only one writes a blueprint terminal.
func TestCancelBlueprint_FinalizesRunAndParksSteps(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	const runID = "r-bp-cancel"
	seedRun(t, database, runID, "sess-bp-cancel", "/tmp/wt-bp-cancel")
	brID := "seedbpr-" + runID

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	// No registered subprocess handle, so this takes CancelBlueprint's
	// paused-blueprint branch: park every active step itself, then finalize.
	if err := s.CancelBlueprint(runmode.LocalDefaultOrgID, brID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("CancelBlueprint: %v", err)
	}

	var bpStatus string
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id = ?`, brID).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run: %v", err)
	}
	if bpStatus != "cancelled" {
		t.Errorf("blueprint_run status = %q, want cancelled", bpStatus)
	}
	var stepStatus, stopReason string
	if err := database.QueryRow(`SELECT status, COALESCE(stop_reason, '') FROM conversations WHERE id = ?`, runID).Scan(&stepStatus, &stopReason); err != nil {
		t.Fatalf("read step conversation: %v", err)
	}
	if stepStatus != "open" || stopReason != "user_cancelled" {
		t.Errorf("step = (%q, %q), want (open, user_cancelled) — cancelling the plan parks its steps, it does not write terminals on them", stepStatus, stopReason)
	}
}

// TestCancelBlueprint_AlreadyTerminalIsNoOp: the blueprint verb is idempotent
// against a plan that already ended, so a second click cannot re-stamp a
// terminal over the one that recorded how the blueprint actually finished.
func TestCancelBlueprint_AlreadyTerminalIsNoOp(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	const runID = "r-bp-done"
	seedRun(t, database, runID, "sess-bp-done", "/tmp/wt-bp-done")
	brID := "seedbpr-" + runID
	if _, err := database.Exec(`UPDATE blueprint_runs SET status = 'completed' WHERE id = ?`, brID); err != nil {
		t.Fatalf("complete blueprint: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.CancelBlueprint(runmode.LocalDefaultOrgID, brID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("CancelBlueprint on a terminal plan: %v", err)
	}

	var bpStatus string
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id = ?`, brID).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run: %v", err)
	}
	if bpStatus != "completed" {
		t.Errorf("blueprint_run status = %q, want completed (unchanged)", bpStatus)
	}
}

// TestStop_UniformAcrossBlueprintShapes pins that stopping means the same
// thing wherever the conversation sits in a plan: the conversation parks
// `open` and the blueprint, if there is one, is left exactly as it was. The
// position in the plan is the thing most likely to grow a special case (a
// final step "has nothing left to do", a lone step "is the whole plan"), and
// every such case would be a second meaning of `open`.
func TestStop_UniformAcrossBlueprintShapes(t *testing.T) {
	twoStepPlan := func(t *testing.T) string {
		t.Helper()
		raw, err := json.Marshal([]domain.BlueprintPlanStep{
			{StepIndex: 0, PromptID: "test-prompt", PromptName: "One", PromptBody: "b", Source: "user"},
			{StepIndex: 1, PromptID: "test-prompt", PromptName: "Two", PromptBody: "b", Source: "user"},
		})
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}
		return string(raw)
	}

	for _, tc := range []struct {
		name    string
		suffix  string
		plan    func(*testing.T) string
		stepIdx int
	}{
		{name: "intermediate step", suffix: "mid", plan: twoStepPlan, stepIdx: 0},
		{name: "final step", suffix: "fin", plan: twoStepPlan, stepIdx: 1},
		{name: "single-step blueprint", suffix: "solo", plan: nil, stepIdx: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newDelegateTestDB(t)
			runID := "r-shape-" + tc.suffix
			seedRun(t, database, runID, "sess-"+tc.suffix, "/tmp/wt-"+tc.suffix)
			brID := "seedbpr-" + runID
			if tc.plan != nil {
				if _, err := database.Exec(`UPDATE blueprint_runs SET step_plan = ?, current_step_index = ? WHERE id = ?`, tc.plan(t), tc.stepIdx, brID); err != nil {
					t.Fatalf("set step plan: %v", err)
				}
			}
			if _, err := database.Exec(`UPDATE conversations SET blueprint_step_index = ? WHERE id = ?`, tc.stepIdx, runID); err != nil {
				t.Fatalf("set step index: %v", err)
			}

			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
			if err := s.Stop(runmode.LocalDefaultOrgID, runID, runmode.LocalDefaultUserID); err != nil {
				t.Fatalf("stop: %v", err)
			}

			if got := storedStatus(t, database, runID); got != "open" {
				t.Errorf("conversation status = %q, want open", got)
			}
			var bpStatus string
			var cancelRequested bool
			if err := database.QueryRow(`SELECT status, cancel_requested FROM blueprint_runs WHERE id = ?`, brID).Scan(&bpStatus, &cancelRequested); err != nil {
				t.Fatalf("read blueprint_run: %v", err)
			}
			if bpStatus != "running" || cancelRequested {
				t.Errorf("blueprint = (%q, cancel_requested=%v), want (running, false)", bpStatus, cancelRequested)
			}
		})
	}

	t.Run("no blueprint", func(t *testing.T) {
		database := newDelegateTestDB(t)
		// seedRun mints the entity/event/task chain; this conversation hangs off
		// the same task with no blueprint_run of its own.
		seedRun(t, database, "r-anchor", "sess-anchor", "/tmp/wt-anchor")
		var taskID string
		if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id = 'r-anchor'`).Scan(&taskID); err != nil {
			t.Fatalf("lookup task: %v", err)
		}
		dbtest.SeedConversation(t, database, domain.Conversation{
			ID: "r-bare", TaskID: taskID, PromptID: "test-prompt", Status: "running",
			Model: "claude-sonnet-4-6", SessionID: "sess-bare", WorktreePath: "/tmp/wt-bare",
		})

		s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
		if err := s.Stop(runmode.LocalDefaultOrgID, "r-bare", runmode.LocalDefaultUserID); err != nil {
			t.Fatalf("stop: %v", err)
		}
		if got := storedStatus(t, database, "r-bare"); got != "open" {
			t.Errorf("conversation status = %q, want open — a conversation with no plan stops the same way", got)
		}
	})
}

// TestStop_MidBlueprintStep_ResumesAndIsDriven is the acceptance test the
// whole change exists for: a stopped intermediate step is not merely marked
// resumable, it actually resumes. The parked row is claimed off the ordinary
// run queue and driven, which only happens while its blueprint is still
// 'running' and not cancel-requested — the exact state the old
// conversation-level cancel destroyed.
//
// The claim's own work fails fast here (no warm worktree, no snapshot, no
// agent subprocess); what is being pinned is that the resume was accepted,
// claimed, and delivered, not what the agent then did with it.
func TestStop_MidBlueprintStep_ResumesAndIsDriven(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	const runID = "r-resume-after-stop"
	// A worktree path that does not exist: with no blob store wired the
	// enqueue-time recoverability check cannot disprove recoverability and lets
	// the wake through, and the claim then fails fast instead of launching an
	// agent.
	seedRun(t, database, runID, "sess-resume", "/tmp/does-not-exist-resume-after-stop")
	brID := "seedbpr-" + runID
	plan, err := json.Marshal([]domain.BlueprintPlanStep{
		{StepIndex: 0, PromptID: "test-prompt", PromptName: "One", PromptBody: "b", Source: "user"},
		{StepIndex: 1, PromptID: "test-prompt", PromptName: "Two", PromptBody: "b", Source: "user"},
	})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if _, err := database.Exec(`UPDATE blueprint_runs SET step_plan = ? WHERE id = ?`, string(plan), brID); err != nil {
		t.Fatalf("set step plan: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.Stop(runmode.LocalDefaultOrgID, runID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, runID, runmode.LocalDefaultUserID, "carry on"); err != nil {
		t.Fatalf("resume after stop: %v (a stopped step must stay resumable)", err)
	}

	// Claims the globally-oldest queued row and drives it — it fatals if
	// nothing is claimable, which is the failure mode a terminal blueprint
	// produced.
	claimAndDispatch(t, s, database)

	if _, _, ok, err := s.pendingInput.Consume(context.Background(), runmode.LocalDefaultOrgID, runID); err != nil || ok {
		t.Errorf("pending input still queued after the claim (ok=%v err=%v); the resume was accepted but never driven", ok, err)
	}
}

// TestParkRunOpen_StoppedKeepsTheWorkspace is the stop-costs-disk
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

// TestStopAndCancelBlueprint_AlreadyTerminal_LeavesBlueprintAlone: a stale
// teardown — one aimed at a run that already concluded — must stay a pure
// no-op. The blueprint-layer signal is the reason this needs its own guard:
// raising cancel_requested on a blueprint that has moved on to its next step
// would cancel work the caller never aimed at.
func TestStopAndCancelBlueprint_AlreadyTerminal_LeavesBlueprintAlone(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-stale", "sess-stale", "/tmp/wt-stale")
	if _, err := database.Exec(`UPDATE conversations SET status = 'completed', outcome = 'continue' WHERE id = 'r-stale'`); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.StopAndCancelBlueprint(runmode.LocalDefaultOrgID, "r-stale", runmode.LocalDefaultUserID); err == nil {
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
		t.Errorf("blueprint = (%q, cancel_requested=%v), want (running, false) — a stale teardown must not stop the next step", status, cancelRequested)
	}
}
