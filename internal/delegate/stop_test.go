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

// fakeWaker captures WakeFirings invocations so tests can assert the
// spawner's terminal-state hooks ring the firing worker's doorbell.
// Synchronized because a terminal may arrive from another goroutine.
type fakeWaker struct {
	mu     sync.Mutex
	calls  int
	called chan struct{}
}

func newFakeWaker() *fakeWaker {
	return &fakeWaker{called: make(chan struct{}, 8)}
}

func (f *fakeWaker) WakeFirings() {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	select {
	case f.called <- struct{}{}:
	default:
	}
}

func (f *fakeWaker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestStop_OpenAutoRun_WakesFirings pins the wake for a stop nobody holds.
// An auto-fired conversation parked `open` has no engagement to settle its
// stop, so the dispatcher's settlement does — and without its wake the task's
// queued firings would wait on the scan tick.
func TestStop_OpenAutoRun_WakesFirings(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r1", "sess-1", "/tmp/wt-r1")
	if _, err := database.Exec(`UPDATE conversations SET status = 'open', trigger_type = 'event', creator_user_id = NULL WHERE id = 'r1'`); err != nil {
		t.Fatalf("park conversation: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	waker := newFakeWaker()
	s.SetFiringWaker(waker)

	if err := s.Stop(runmode.LocalDefaultOrgID, "r1", ""); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if n := waker.count(); n != 0 {
		t.Fatalf("the request woke the firing worker %d times; only the settlement does", n)
	}
	s.settleUnclaimedStops(context.Background())

	select {
	case <-waker.called:
	case <-time.After(time.Second):
		t.Fatal("WakeFirings was never called")
	}
	if n := waker.count(); n != 1 {
		t.Fatalf("expected 1 wake, got %d", n)
	}
}

// TestStop_OpenManualRun_WakesFirings is the other half of the one-live-
// conversation-per-task rule: a manual conversation holds the task's firing
// gate exactly as an auto-fired one does, so its stop is the moment that gate
// opens and the queued firings behind it have to be claimed.
func TestStop_OpenManualRun_WakesFirings(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-manual", "sess-2", "/tmp/wt-rm")
	// Manual is the seedConversation default but we set it explicitly for
	// clarity and pin to `open`.
	if _, err := database.Exec(`UPDATE conversations SET status = 'open', trigger_type = 'manual' WHERE id = 'r-manual'`); err != nil {
		t.Fatalf("park conversation: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	waker := newFakeWaker()
	s.SetFiringWaker(waker)

	if err := s.Stop(runmode.LocalDefaultOrgID, "r-manual", ""); err != nil {
		t.Fatalf("stop: %v", err)
	}
	s.settleUnclaimedStops(context.Background())

	select {
	case <-waker.called:
	case <-time.After(time.Second):
		t.Fatal("WakeFirings was never called for a manual conversation's stop")
	}
	if n := waker.count(); n != 1 {
		t.Fatalf("expected 1 wake, got %d", n)
	}
}

// TestStop_AlreadyTerminal_NoWake confirms a stale stop on a row some other
// path already terminated rings nothing. Without the "only on flipped ==
// true" guard, it would wake the worker for a terminal that already did.
func TestStop_AlreadyTerminal_NoWake(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-done", "sess-3", "/tmp/wt-rd")
	// Trigger_type='event' requires creator_user_id IS NULL per the
	// CHECK invariant. seedConversation defaults to manual +
	// sentinel creator; the UPDATE has to clear creator alongside.
	if _, err := database.Exec(`UPDATE conversations SET status = 'completed', trigger_type = 'event', creator_user_id = NULL WHERE id = 'r-done'`); err != nil {
		t.Fatalf("complete conversation: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	waker := newFakeWaker()
	s.SetFiringWaker(waker)

	if err := s.Stop(runmode.LocalDefaultOrgID, "r-done", ""); err == nil {
		t.Fatal("expected 'no active conversation' error on terminal row")
	}

	select {
	case <-waker.called:
		t.Fatal("WakeFirings called on already-terminal row")
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
	s.settleUnclaimedStops(context.Background())

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
	s.settleUnclaimedStops(context.Background())

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

// TestCancelBlueprintRun_RecordsIntentAndTheSettlementCancels pins the layer
// the stop verb hands cancellation off TO. Now that stopping a conversation
// only freezes the plan, this is the only verb that can end one — a blueprint
// sitting 'running' with no queued step and no live claim has no other exit
// short of resuming the conversation or dispositioning the task.
//
// The verb itself writes requests only: the run's cancel signal and a stop
// intent on its step. The dispatcher's settlement, finding the step unheld,
// parks it and cancels the run in one transaction.
func TestCancelBlueprintRun_RecordsIntentAndTheSettlementCancels(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	const conversationID = "r-bp-cancel"
	seedConversation(t, database, conversationID, "sess-bp-cancel", "/tmp/wt-bp-cancel")
	if _, err := database.Exec(`UPDATE conversations SET status = NULL WHERE id = ?`, conversationID); err != nil {
		t.Fatalf("stage the step queued: %v", err)
	}
	brID := "seedbpr-" + conversationID

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.CancelBlueprintRun(runmode.LocalDefaultOrgID, brID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("CancelBlueprintRun: %v", err)
	}

	var bpStatus string
	var cancelRequested bool
	if err := database.QueryRow(`SELECT status, cancel_requested FROM blueprint_runs WHERE id = ?`, brID).Scan(&bpStatus, &cancelRequested); err != nil {
		t.Fatalf("read blueprint_run: %v", err)
	}
	if bpStatus != "running" || !cancelRequested {
		t.Errorf("blueprint_run after the request = (%q, cancel_requested=%v), want (running, true) — the verb writes no run status", bpStatus, cancelRequested)
	}
	if got := storedStatus(t, database, conversationID); got != "" {
		t.Errorf("step status after the request = %q, want none — the verb writes no conversation status", got)
	}
	var intent bool
	if err := database.QueryRow(`SELECT stop_requested_at IS NOT NULL FROM conversations WHERE id = ?`, conversationID).Scan(&intent); err != nil {
		t.Fatalf("read the intent: %v", err)
	}
	if !intent {
		t.Fatal("the step carries no stop intent; nothing would settle the cancel")
	}

	s.settleUnclaimedStops(context.Background())

	var abortReason string
	if err := database.QueryRow(`SELECT status, COALESCE(abort_reason, '') FROM blueprint_runs WHERE id = ?`, brID).Scan(&bpStatus, &abortReason); err != nil {
		t.Fatalf("read blueprint_run: %v", err)
	}
	if bpStatus != "cancelled" || abortReason != "user_cancelled" {
		t.Errorf("blueprint_run after settlement = (%q, %q), want (cancelled, user_cancelled)", bpStatus, abortReason)
	}
	var stepStatus, parkReason string
	if err := database.QueryRow(`SELECT status, COALESCE(park_reason, '') FROM conversations WHERE id = ?`, conversationID).Scan(&stepStatus, &parkReason); err != nil {
		t.Fatalf("read step conversation: %v", err)
	}
	if stepStatus != "open" || parkReason != string(domain.ParkReasonUserCancelled) {
		t.Errorf("step = (%q, %q), want (open, user_cancelled) — cancelling the plan parks its steps, it does not write terminals on them", stepStatus, parkReason)
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
			s.settleUnclaimedStops(context.Background())

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
		s.settleUnclaimedStops(context.Background())
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
	s.settleUnclaimedStops(context.Background())

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
	s, database, conversationID, _ := setupAdvanceFixture(t, "cancel-park")
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
		claimID:        holderClaimFor(t, s, runmode.LocalDefaultOrgID, conversationID),
		namespace:      namespace,
		claudeCwd:      wtPath,
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
// In the control/executor split, every stop of a live native-runtime
// conversation is a request on control and a settlement on the executor:
// control records the intent (the process registry it would cancel through is
// per-pod, and control never holds the process), and the executor's teardown
// — reached through the signal or its next renewal — parks the row through its
// own fence, snapshotting the workspace only it can reach.
//
// What this pins is that the teardown of a cancelled native engagement is a
// park, not a failure: a ctx kill observed inside a store write must not be
// read as agent_error, and the park must carry the workspace with it, or a
// follow-up a minute later answers 410 for a workspace that was never saved.
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
	claimID := markEngaged(t, database, conversationID)

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

	// 1. Control's half: the request. No status, no claim release, no
	// snapshot — the workspace is on a machine it cannot reach.
	if err := s.Stop(runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := storedStatus(t, database, conversationID); got != "" {
		t.Fatalf("after control's request, status = %q, want none — the request writes no status", got)
	}
	if !hasActiveClaim(t, database, conversationID) {
		t.Fatal("control's request released the executor's claim; only the holder settles")
	}

	// 2. The executor's half. Its engine reports the kill as a cancellation
	// however it observed it, and its teardown settles the stop.
	if fenced := s.recordNativeResult(context.Background(), runmode.LocalDefaultOrgID, conversationID,
		loadTask(t, s, taskID),
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: claimID, blueprintRunID: namespace},
		namespace, wtPath, "manual", runmode.LocalDefaultUserID, time.Now(),
		agentloop.Result{Kind: agentloop.ResultCancelled, Err: context.Canceled}, nil); fenced {
		t.Fatal("the executor's teardown reported a fence trip while holding the claim")
	}

	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, namespace))
	if err != nil {
		t.Fatalf("the teardown wrote no workspace snapshot: %v", err)
	}
	_ = rc.Close()
	var status, parkReason string
	var intent bool
	if err := database.QueryRow(
		`SELECT status, COALESCE(park_reason, ''), stop_requested_at IS NOT NULL FROM conversations WHERE id = ?`, conversationID,
	).Scan(&status, &parkReason, &intent); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != "open" || parkReason != string(domain.ParkReasonUserCancelled) || intent {
		t.Errorf("after the settlement = (%q, %q, intent %v), want (open, user_cancelled, cleared)", status, parkReason, intent)
	}
	if hasActiveClaim(t, database, conversationID) {
		t.Error("the settlement left the claim live; the park and the release are one transaction")
	}
	// One row, and it is the stop's own note. A stop is not an agent_error,
	// so the teardown writes no failure row.
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
		t.Fatalf("follow-up after a cross-pod stop: %v", err)
	}
	claimed, err := s.conversationQueue.ClaimNextConversation(context.Background(), "test-executor", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
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
		runConfig{orgID: runmode.LocalDefaultOrgID, blueprintRunID: namespace, claimID: holderClaimFor(t, s, runmode.LocalDefaultOrgID, conversationID)},
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

// TestStopCauseTaskRequeued_Note pins the sentence a returned-to-queue task
// writes onto the transcript of the run it stopped. Nothing resumes that
// conversation, so this note is the entire explanation a human reading its
// history gets — and a task on its way back to the queue is still open, which
// is why it cannot borrow the disposition wording.
func TestStopCauseTaskRequeued_Note(t *testing.T) {
	const want = "Run stopped: the task it was working on was returned to the queue."
	if got := StopCauseTaskRequeued.note(); got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
	if StopCauseTaskRequeued.note() == StopCauseTaskDispositioned.note() {
		t.Error("requeue and disposition write the same sentence; a reader cannot tell a task still on the docket from one swiped away")
	}
}

// TestStop_HolderParkRecordsWhoAsked: the engagement settles a stop through
// its own fenced park, and whatever reason its exit path passes, the row
// records the intent's actor — a system stop reads system_cancelled even
// though the engagement's cancelled-context arm names a user cancel.
func TestStop_HolderParkRecordsWhoAsked(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-holder", "sess-holder", "/tmp/wt-holder")
	claimID := markEngaged(t, database, "r-holder")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.Stop(runmode.LocalDefaultOrgID, "r-holder", ""); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := storedStatus(t, database, "r-holder"); got != "" {
		t.Fatalf("status after the request = %q, want none", got)
	}
	if fenced := s.markConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: "r-holder",
		claimID:        claimID,
		reason:         db.ParkStopped(domain.ParkReasonUserCancelled, ""),
	}); fenced {
		t.Fatal("the holder's park was refused while it held the claim")
	}
	var status, reason string
	var intent bool
	if err := database.QueryRow(
		`SELECT status, COALESCE(park_reason, ''), stop_requested_at IS NOT NULL FROM conversations WHERE id = 'r-holder'`,
	).Scan(&status, &reason, &intent); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != "open" || reason != string(domain.ParkReasonSystemCancelled) || intent {
		t.Errorf("after the holder's park = (%q, %q, intent %v), want (open, system_cancelled, cleared)", status, reason, intent)
	}
	if hasActiveClaim(t, database, "r-holder") {
		t.Error("the holder's park left its claim live")
	}
}

// TestMarkConversationOpen_RefusesAParkWithNoClaim: a park is the holder's
// write, so a caller that names no claim is a bug, and it writes nothing.
func TestMarkConversationOpen_RefusesAParkWithNoClaim(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-claimless", "sess-claimless", "/tmp/wt-claimless")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if fenced := s.markConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: "r-claimless",
		reason:         db.ParkIdle(),
	}); !fenced {
		t.Error("a claimless park reported success")
	}
	if got := storedStatus(t, database, "r-claimless"); got == "open" {
		t.Error("a claimless park wrote the row")
	}
}

// TestSettleUnclaimedStops_WakesOnlyAfterSettlingSomething: the firing
// worker's wake is the settlement's, and only a settlement that parked
// something has news for it.
func TestSettleUnclaimedStops_WakesOnlyAfterSettlingSomething(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-wake", "sess-wake", "/tmp/wt-wake")
	if _, err := database.Exec(`UPDATE conversations SET status = 'open' WHERE id = 'r-wake'`); err != nil {
		t.Fatalf("park conversation: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	waker := newFakeWaker()
	s.SetFiringWaker(waker)

	s.settleUnclaimedStops(context.Background())
	if n := waker.count(); n != 0 {
		t.Fatalf("an empty settlement woke the firing worker %d times", n)
	}
	if err := s.Stop(runmode.LocalDefaultOrgID, "r-wake", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	s.settleUnclaimedStops(context.Background())
	if n := waker.count(); n != 1 {
		t.Fatalf("wakes after settling one stop = %d, want 1", n)
	}
}

// TestReconcileConversationQueue_CountsAClaimDesyncAndRepairsNothing: a
// terminal conversation still holding a live claim is a shape no writer
// produces, so boot reports it and leaves it for someone to look at.
func TestReconcileConversationQueue_CountsAClaimDesyncAndRepairsNothing(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-desync", "sess-desync", "/tmp/wt-desync")
	claimID := markEngaged(t, database, "r-desync")
	if _, err := database.Exec(`UPDATE conversations SET status = 'completed' WHERE id = 'r-desync'`); err != nil {
		t.Fatalf("stage the desync: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	s.reconcileConversationQueue(context.Background())

	var released bool
	if err := database.QueryRow(`SELECT released_at IS NOT NULL FROM claims WHERE id = ?`, claimID).Scan(&released); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if released {
		t.Error("boot released the desynced claim; the checker counts and repairs nothing")
	}
	if got := storedStatus(t, database, "r-desync"); got != "completed" {
		t.Errorf("status = %q, want completed (untouched)", got)
	}
}
