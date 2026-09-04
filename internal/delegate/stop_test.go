package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
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
// An auto-fired conversation parked `open` has no goroutine defer to piggy-back
// on, so without the explicit drain the task's firing queue would stick until
// some other conversation terminated.
func TestStop_OpenAutoRun_DrainsQueue(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r1", "sess-1", "/tmp/wt-r1")
	if _, err := database.Exec(`UPDATE conversations SET status = 'open', trigger_type = 'event', creator_user_id = NULL WHERE id = 'r1'`); err != nil {
		t.Fatalf("park conversation: %v", err)
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
	seedConversation(t, database, "r-manual", "sess-2", "/tmp/wt-rm")
	// Manual is the seedConversation default but we set it explicitly for
	// clarity and pin to `open`.
	if _, err := database.Exec(`UPDATE conversations SET status = 'open', trigger_type = 'manual' WHERE id = 'r-manual'`); err != nil {
		t.Fatalf("park conversation: %v", err)
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
		t.Fatal("DrainTask called for manual conversation; should be filtered by trigger_type")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestStop_AlreadyTerminal_NoDrain confirms we don't double-drain
// a row that some other path already terminated. Without the
// "only on flipped == true" guard, a stale stop on a completed
// conversation would fire a redundant drain.
func TestStop_AlreadyTerminal_NoDrain(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-done", "sess-3", "/tmp/wt-rd")
	// Trigger_type='event' requires creator_user_id IS NULL per the
	// CHECK invariant. seedConversation defaults to manual +
	// sentinel creator; the UPDATE has to clear creator alongside.
	if _, err := database.Exec(`UPDATE conversations SET status = 'completed', trigger_type = 'event', creator_user_id = NULL WHERE id = 'r-done'`); err != nil {
		t.Fatalf("complete conversation: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	drainer := newFakeDrainer()
	s.SetQueueDrainer(drainer)

	if err := s.Stop(runmode.LocalDefaultOrgID, "r-done", ""); err == nil {
		t.Fatal("expected 'no active conversation' error on terminal row")
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
	seedConversation(t, database, "r-step", "sess-step", "/tmp/wt-rs")
	if _, err := database.Exec(`UPDATE conversations SET status = 'open' WHERE id = 'r-step'`); err != nil {
		t.Fatalf("park conversation: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.Stop(runmode.LocalDefaultOrgID, "r-step", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	var convStatus, bpStatus, stopReason string
	var cancelRequested bool
	if err := database.QueryRow(`SELECT status, COALESCE(park_reason, '') FROM conversations WHERE id = 'r-step'`).Scan(&convStatus, &stopReason); err != nil {
		t.Fatalf("read conversation status: %v", err)
	}
	if convStatus != "open" || stopReason != "user_cancelled" {
		t.Errorf("conversation = (%q, %q), want (open, user_cancelled) — a stop parks, it never writes a terminal of its own", convStatus, stopReason)
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

// TestStopConversationAndCancelBlueprint_OpenStep_FinalizesBlueprintRun pins the other
// half of the split: when the layer above the conversation has already ended
// — a closed or swiped task, an archived team — the teardown verb carries the
// blueprint terminal with it. Nothing will resume these conversations, so a
// frozen 'running' blueprint would hold a worktree and count as live work
// forever. seedConversation links the conversation to a 1-step blueprint_run
// "seedbpr-<conversationID>".
func TestStopConversationAndCancelBlueprint_OpenStep_FinalizesBlueprintRun(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-teardown", "sess-teardown", "/tmp/wt-rt")
	if _, err := database.Exec(`UPDATE conversations SET status = 'open' WHERE id = 'r-teardown'`); err != nil {
		t.Fatalf("park conversation: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.StopConversationAndCancelBlueprint(runmode.LocalDefaultOrgID, "r-teardown", runmode.LocalDefaultUserID, StopCauseTaskDispositioned); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	var convStatus, bpStatus string
	var cancelRequested bool
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = 'r-teardown'`).Scan(&convStatus); err != nil {
		t.Fatalf("read conversation status: %v", err)
	}
	if convStatus != "open" {
		t.Errorf("conversation status = %q, want open — even a teardown parks the conversation rather than writing a terminal on it", convStatus)
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

// TestCancelBlueprintRun_FinalizesRunAndParksSteps pins the layer the stop verb
// hands cancellation off TO. Now that stopping a conversation only freezes the
// plan, this is the only verb that can end one — a blueprint sitting 'running'
// with no queued step and no live claim has no other exit short of resuming
// the conversation or dispositioning the task.
//
// It lives beside the stop tests on purpose: the pair is the split. The
// conversation verb parks and freezes; the blueprint verb finalizes. Both park
// the step, and only one writes a blueprint terminal.
func TestCancelBlueprintRun_FinalizesRunAndParksSteps(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	const conversationID = "r-bp-cancel"
	seedConversation(t, database, conversationID, "sess-bp-cancel", "/tmp/wt-bp-cancel")
	brID := "seedbpr-" + conversationID

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	// No registered subprocess handle, so this takes CancelBlueprintRun's
	// paused-blueprint branch: park every active step itself, then finalize.
	if err := s.CancelBlueprintRun(runmode.LocalDefaultOrgID, brID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("CancelBlueprintRun: %v", err)
	}

	var bpStatus string
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id = ?`, brID).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run: %v", err)
	}
	if bpStatus != "cancelled" {
		t.Errorf("blueprint_run status = %q, want cancelled", bpStatus)
	}
	// The step's park reason names what actually happened to it: the plan
	// behind it was cancelled. `user_cancelled` would be a step nobody
	// cancelled claiming somebody did — the person cancelled the blueprint.
	var stepStatus, parkReason string
	if err := database.QueryRow(`SELECT status, COALESCE(park_reason, '') FROM conversations WHERE id = ?`, conversationID).Scan(&stepStatus, &parkReason); err != nil {
		t.Fatalf("read step conversation: %v", err)
	}
	if stepStatus != "open" || parkReason != string(domain.ParkReasonBlueprintCancelled) {
		t.Errorf("step = (%q, %q), want (open, blueprint_cancelled) — cancelling the plan parks its steps, it does not write terminals on them", stepStatus, parkReason)
	}
}

// TestCancelBlueprintRun_AlreadyTerminalIsNoOp: the blueprint verb is idempotent
// against a plan that already ended, so a second click cannot re-stamp a
// terminal over the one that recorded how the blueprint actually finished.
func TestCancelBlueprintRun_AlreadyTerminalIsNoOp(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	const conversationID = "r-bp-done"
	seedConversation(t, database, conversationID, "sess-bp-done", "/tmp/wt-bp-done")
	brID := "seedbpr-" + conversationID
	if _, err := database.Exec(`UPDATE blueprint_runs SET status = 'completed' WHERE id = ?`, brID); err != nil {
		t.Fatalf("complete blueprint: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if err := s.CancelBlueprintRun(runmode.LocalDefaultOrgID, brID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("CancelBlueprintRun on a terminal plan: %v", err)
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
			conversationID := "r-shape-" + tc.suffix
			seedConversation(t, database, conversationID, "sess-"+tc.suffix, "/tmp/wt-"+tc.suffix)
			brID := "seedbpr-" + conversationID
			if tc.plan != nil {
				if _, err := database.Exec(`UPDATE blueprint_runs SET step_plan = ?, current_step_index = ? WHERE id = ?`, tc.plan(t), tc.stepIdx, brID); err != nil {
					t.Fatalf("set step plan: %v", err)
				}
			}
			if _, err := database.Exec(`UPDATE conversations SET blueprint_step_index = ? WHERE id = ?`, tc.stepIdx, conversationID); err != nil {
				t.Fatalf("set step index: %v", err)
			}

			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
			if err := s.Stop(runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID); err != nil {
				t.Fatalf("stop: %v", err)
			}

			if got := storedStatus(t, database, conversationID); got != "open" {
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
		// seedConversation mints the entity/event/task chain; this conversation hangs off
		// the same task with no blueprint_run of its own.
		seedConversation(t, database, "r-anchor", "sess-anchor", "/tmp/wt-anchor")
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
// conversation queue and driven, which only happens while its blueprint is still
// 'running' and not cancel-requested — the exact state the old
// conversation-level cancel destroyed.
//
// The claim's own work fails fast here (the warm worktree holds no session
// transcript, so there is no agent subprocess); what is being pinned is that
// the resume was accepted, claimed, and delivered, not what the agent then
// did with it.
func TestStop_MidBlueprintStep_ResumesAndIsDriven(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	const conversationID = "r-resume-after-stop"
	// A real worktree, empty: the rehydrate warm-returns (a missing one is a
	// runtime failure, which hands the claim back rather than delivering) and
	// the claim then stops on the absent session transcript instead of
	// launching an agent.
	seedConversation(t, database, conversationID, "sess-resume", t.TempDir())
	brID := "seedbpr-" + conversationID
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

	if err := s.Stop(runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID, "carry on"); err != nil {
		t.Fatalf("resume after stop: %v (a stopped step must stay resumable)", err)
	}

	// Claims the globally-oldest queued row and drives it — it fatals if
	// nothing is claimable, which is the failure mode a terminal blueprint
	// produced.
	claimAndDispatch(t, s, database)

	if _, _, ok, err := s.pendingInput.Consume(context.Background(), runmode.LocalDefaultOrgID, conversationID); err != nil || ok {
		t.Errorf("pending input still queued after the claim (ok=%v err=%v); the resume was accepted but never driven", ok, err)
	}
}

// TestParkConversationOpen_StoppedKeepsTheWorkspace is the stop-costs-disk
// trade, pinned. The old cancel terminal removed the worktree on its way out,
// which threw the workspace away at exactly the moment a user who just killed
// a wedged conversation is most likely to want it back. Now the conversation
// parks `open`, the snapshot is written BEFORE the flip (so a resume that lands
// without the warm worktree can still rebuild it), and the warm tree is left
// for the blueprint's own cleanup.
func TestParkConversationOpen_StoppedKeepsTheWorkspace(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, taskID := setupAdvanceFixture(t, "cancel-park")
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s.SetStorage(blobs)

	// A workspace with something in it that only the snapshot can carry.
	wtPath := t.TempDir()
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.txt"), "work in progress")
	namespace := blueprintRunIDForConversation(t, database, conversationID)

	if fenced := s.parkConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: conversationID,
		taskID:         taskID,
		namespace:      namespace,
		claudeCwd:      wtPath,
		triggerType:    "event",
		reason:         db.ParkStopped("system_cancelled", "Cancelled by system"),
	}, ""); fenced {
		t.Fatal("parkConversationOpen reported a fence trip on an unfenced store")
	}

	var status, stopReason string
	var parked bool
	if err := database.QueryRow(
		`SELECT status, COALESCE(park_reason, ''), parked_at IS NOT NULL FROM conversations WHERE id = ?`, conversationID,
	).Scan(&status, &stopReason, &parked); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != "open" || stopReason != "system_cancelled" {
		t.Errorf("conversation = (%q, %q), want (open, system_cancelled)", status, stopReason)
	}
	if !parked {
		t.Error("parked_at unset; the snapshot-retention sweep keys the parked workspace off it")
	}

	// The snapshot exists…
	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, namespace))
	if err != nil {
		t.Fatalf("cancelled conversation wrote no workspace snapshot: %v", err)
	}
	_ = rc.Close()
	// …and the warm worktree was not torn down under it.
	if _, err := os.Stat(filepath.Join(wtPath, "_tfac", "notes.txt")); err != nil {
		t.Errorf("worktree removed by the cancel (%v); parking retains it and the TTL is the reaper", err)
	}
}

// TestStop_CrossPodNativeStop_KeepsTheWorkspaceAndStaysResumable drives the
// two halves of a split-mode stop in the order they really run.
//
// In the control/executor split, EVERY stop of a live native-runtime
// conversation takes the DB-only park: the process registry Spawner.stop
// consults first is per-pod, and control never holds the process. So control
// parks the row, releases the claim, and fires the cancel signal at the
// executor as best-effort hastening; the executor's own teardown lands
// seconds later, holding a claim that is already released. Every write it
// makes is refused — which leaves the unfenced workspace snapshot as the only
// thing it still has to contribute, and the only thing a follow-up needs.
//
// That is the sequence this test pins, because it used to be broken end to
// end: the engine classified a ctx kill observed inside a store write as a
// failure, the failure path's FIRST write is fenced, and it returned there
// before reaching any snapshot. Net state was a parked conversation with no
// workspace anywhere, and a follow-up a minute later answered 410 "this
// conversation's workspace has expired" for a workspace that had never been
// saved.
func TestStop_CrossPodNativeStop_KeepsTheWorkspaceAndStaysResumable(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, taskID := setupAdvanceFixture(t, "cross-pod-stop")
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s.SetStorage(blobs)
	markNative(t, database, conversationID)

	// The worktree lives on the executor. Control's copy of the path resolves
	// to nothing, exactly as it would on another machine — so recoverability
	// can only be answered by the snapshot.
	wtPath := t.TempDir()
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.txt"), "half-finished work")
	if _, err := database.Exec(
		`UPDATE conversations SET worktree_path = ? WHERE id = ?`,
		filepath.Join(t.TempDir(), "not-on-this-pod"), conversationID,
	); err != nil {
		t.Fatalf("point the row at an off-pod worktree: %v", err)
	}
	namespace := blueprintRunIDForConversation(t, database, conversationID)

	// 1. Control's half: park, release the claim, signal the executor. It
	// takes no snapshot — the workspace is on a machine it cannot reach.
	if err := s.Stop(runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := storedStatus(t, database, conversationID); got != "open" {
		t.Fatalf("after control's park, status = %q, want open", got)
	}

	// 2. The executor's half. Its engine reports the kill as a cancellation
	// however it observed it, and its teardown writes into a conversation
	// whose claim step 1 released. The refusal is injected rather than left
	// to the store's own fence so the park attempt can be counted — as
	// everywhere else in this package.
	fenced := &fencedConversationStore{ConversationStore: s.conversations}
	s.conversations = fenced
	gotFenced := s.recordNativeResult(context.Background(), runmode.LocalDefaultOrgID, conversationID,
		loadTask(t, s, taskID),
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: "claim-1", blueprintRunID: namespace},
		namespace, wtPath, "manual", runmode.LocalDefaultUserID, time.Now(),
		agentloop.Result{Kind: agentloop.ResultCancelled, Err: context.Canceled}, nil)
	s.conversations = fenced.ConversationStore
	if !gotFenced {
		t.Fatal("the executor's teardown did not report the fence trip; it would go on to react to a conversation it no longer owns")
	}
	if fenced.cancels != 1 {
		t.Errorf("fenced park attempted %d times, want 1", fenced.cancels)
	}

	// The blob is the whole point of the teardown, and it is all of it.
	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, namespace))
	if err != nil {
		t.Fatalf("the fenced teardown wrote no workspace snapshot: %v", err)
	}
	_ = rc.Close()
	if got := storedStatus(t, database, conversationID); got != "open" {
		t.Errorf("status = %q, want open — control's park stands and the fenced teardown records no terminal of its own", got)
	}
	// One row, and it is the stop's own note. A stop is not an agent_error,
	// so the fenced teardown writes no failure row — but the stop itself
	// records what it did, on the transcript, where the resumed model reads
	// it.
	var msgs int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, conversationID).Scan(&msgs); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgs != 1 {
		t.Errorf("messages = %d, want exactly the stop note — a stop is not an agent_error and writes no failure row", msgs)
	}
	var role, subtype, content string
	var isError bool
	if err := database.QueryRow(
		`SELECT role, subtype, content, is_error FROM messages WHERE conversation_id = ?`, conversationID,
	).Scan(&role, &subtype, &content, &isError); err != nil {
		t.Fatalf("read the stop note: %v", err)
	}
	if role != "user" || subtype != domain.MessageSubtypeStopNote || isError {
		t.Errorf("stop note = (role=%q, subtype=%q, is_error=%v), want (user, %s, false)",
			role, subtype, isError, domain.MessageSubtypeStopNote)
	}
	if content != stopNoteByUser {
		t.Errorf("stop note content = %q, want %q", content, stopNoteByUser)
	}

	// 3. The user's follow-up, a minute later. It has to be accepted off the
	// snapshot alone, and it has to be claimable.
	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID, "actually, try the other approach"); err != nil {
		t.Fatalf("follow-up after a cross-pod stop: %v (ErrWorkspaceExpired here is the bug this ticket exists for)", err)
	}
	claimed, err := s.conversationQueue.ClaimNextConversation(context.Background(), "test-executor", 1, db.ClaimPlacement{})
	if err != nil {
		t.Fatalf("claim next conversation: %v", err)
	}
	if claimed == nil || claimed.ID != conversationID {
		t.Fatalf("claimed = %v, want the stopped conversation %s — the follow-up was accepted but nothing will drive it", claimed, conversationID)
	}
}

// TestRecordNativeResult_GenuineFailureIsStillAFailure is the other side of
// the reclassification: routing a cancelled engagement to the park must not
// have softened what a real failure does. It still writes `failed`, it still
// records the cause on the transcript, and it still declines to snapshot a
// workspace nothing will resume into.
func TestRecordNativeResult_GenuineFailureIsStillAFailure(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, taskID := setupAdvanceFixture(t, "native-genuine-fail")
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s.SetStorage(blobs)

	wtPath := t.TempDir()
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.txt"), "work the failure discards")
	namespace := blueprintRunIDForConversation(t, database, conversationID)

	if fenced := s.recordNativeResult(context.Background(), runmode.LocalDefaultOrgID, conversationID,
		loadTask(t, s, taskID),
		runConfig{orgID: runmode.LocalDefaultOrgID, blueprintRunID: namespace},
		namespace, wtPath, "event", "", time.Now(),
		agentloop.Result{
			Kind:        agentloop.ResultFailed,
			FailureKind: domain.ConversationFailureAgentError,
			Err:         errors.New("tool host is unusable: broken pipe"),
		}, nil); fenced {
		t.Fatal("recordNativeResult reported a fence trip on an unfenced store")
	}

	if got := storedStatus(t, database, conversationID); got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
	var content string
	if err := database.QueryRow(
		`SELECT content FROM messages WHERE conversation_id = ? AND is_error = 1`, conversationID,
	).Scan(&content); err != nil {
		t.Fatalf("read the failure row: %v", err)
	}
	if !strings.Contains(content, "broken pipe") {
		t.Errorf("failure row = %q, want the underlying cause", content)
	}
	if ok, err := s.Storage().Exists(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, namespace)); err != nil {
		t.Fatalf("snapshot existence: %v", err)
	} else if ok {
		t.Error("a failed conversation snapshotted its workspace; the failure path keeps no workspace and the reaper never enumerates one")
	}
}

// TestStopConversationAndCancelBlueprint_AlreadyTerminal_LeavesBlueprintAlone: a stale
// teardown — one aimed at a conversation that already concluded — must stay a pure
// no-op. The blueprint-layer signal is the reason this needs its own guard:
// raising cancel_requested on a blueprint that has moved on to its next step
// would cancel work the caller never aimed at.
func TestStopConversationAndCancelBlueprint_AlreadyTerminal_LeavesBlueprintAlone(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-stale", "sess-stale", "/tmp/wt-stale")
	if _, err := database.Exec(`UPDATE conversations SET status = 'completed', outcome = 'continue' WHERE id = 'r-stale'`); err != nil {
		t.Fatalf("complete conversation: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.StopConversationAndCancelBlueprint(runmode.LocalDefaultOrgID, "r-stale", runmode.LocalDefaultUserID, StopCauseTaskDispositioned); err == nil {
		t.Fatal("expected 'no active conversation' on a concluded conversation")
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
