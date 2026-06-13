package delegate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// TestProcessCompletion_PendingApprovalWritesSnapshot: every pending_approval
// flip now writes a workspace snapshot — not just abort-outcome ones — so a user
// can cold-resume a parked run before approving the queued artifact. Here a
// continue outcome is coerced finish by the queued PR, and the finish-coerced
// flip still snapshots.
func TestProcessCompletion_PendingApprovalWritesSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, runID, taskID := setupAdvanceFixture(t, "pa-snap")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForRun(t, database, runID)

	if err := s.pendingPRs.CreateSystem(context.Background(), runmode.LocalDefaultOrg, domain.PendingPR{
		ID: "pp-" + runID, RunID: runID, Owner: "o", Repo: "r",
		HeadBranch: "h", HeadSHA: "sha", BaseBranch: "main", Title: "queued PR",
	}); err != nil {
		t.Fatalf("seed pending PR: %v", err)
	}

	task := loadTask(t, s, taskID)
	parked := s.processCompletion(context.Background(), runmode.LocalDefaultOrg, runID, bpr, task,
		res(`{"outcome":"continue","summary":"opened a PR"}`), t.TempDir(), "", "event", "")

	if !parked {
		t.Fatal("processCompletion(pending flip) = false; want parked=true")
	}
	if run := loadRun(t, s, runID); run.Status != "pending_approval" || run.Outcome != "finish" {
		t.Fatalf("run = {status:%q outcome:%q}, want {pending_approval finish}", run.Status, run.Outcome)
	}
	assertSnapshotPresent(t, s, bpr, true)
}

// TestProcessCompletion_PlainAbortWritesSnapshot: a plain abort (completed +
// outcome=abort, no queued artifact) snapshots its workspace at the terminal
// write but returns parked=false — the worktree is torn down by the cleanup
// defers and cold rehydrate from the snapshot is the resume path.
func TestProcessCompletion_PlainAbortWritesSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, runID, taskID := setupAdvanceFixture(t, "ab-snap")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForRun(t, database, runID)

	task := loadTask(t, s, taskID)
	parked := s.processCompletion(context.Background(), runmode.LocalDefaultOrg, runID, bpr, task,
		res(`{"outcome":"abort","summary":"looked into it","reason":"needs a human to rotate the token"}`),
		t.TempDir(), "", "event", "")

	if parked {
		t.Error("processCompletion(plain abort) = true; want parked=false (worktree torn down, snapshot is the resume path)")
	}
	if run := loadRun(t, s, runID); run.Status != "completed" || run.Outcome != "abort" {
		t.Fatalf("run = {status:%q outcome:%q}, want {completed abort}", run.Status, run.Outcome)
	}
	assertSnapshotPresent(t, s, bpr, true)
}

// TestProcessCompletion_FinishNoPendingWritesNoSnapshot is the contrast: a clean
// finish with no queued artifact is not resumable, so it writes no snapshot.
func TestProcessCompletion_FinishNoPendingWritesNoSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, runID, taskID := setupAdvanceFixture(t, "fin-nosnap")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForRun(t, database, runID)

	task := loadTask(t, s, taskID)
	s.processCompletion(context.Background(), runmode.LocalDefaultOrg, runID, bpr, task,
		res(`{"outcome":"finish","summary":"shipped it"}`), t.TempDir(), "", "event", "")

	assertSnapshotPresent(t, s, bpr, false)
}

// TestTerminateBlueprint_AbortRetainsSnapshot: an aborted blueprint keeps its
// workspace snapshot (its completed+abort step is message-resumable; the TTL
// sweep reaps it later), where a clean finish discards it at terminate time.
func TestTerminateBlueprint_AbortRetainsSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, runID, taskID := setupAdvanceFixture(t, "term-abort-keep")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForRun(t, database, runID)
	putTestSnapshot(t, s, bpr)

	s.terminateBlueprint(runmode.LocalDefaultOrg, bpr, taskID, "event", "", time.Now(),
		runConfig{orgID: runmode.LocalDefaultOrg}, domain.BlueprintRunStatusAborted, "needs a human", nil, true)

	assertSnapshotPresent(t, s, bpr, true)
}

// TestTerminateBlueprint_FinishDiscardsSnapshot: a clean finish is not resumable,
// so terminateBlueprint drops its snapshot immediately.
func TestTerminateBlueprint_FinishDiscardsSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, runID, taskID := setupAdvanceFixture(t, "term-finish-drop")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForRun(t, database, runID)
	putTestSnapshot(t, s, bpr)

	s.terminateBlueprint(runmode.LocalDefaultOrg, bpr, taskID, "event", "", time.Now(),
		runConfig{orgID: runmode.LocalDefaultOrg}, domain.BlueprintRunStatusCompleted, "", nil, true)

	assertSnapshotPresent(t, s, bpr, false)
}

// --- helpers ---------------------------------------------------------------

func wireBlobStore(t *testing.T, s *Spawner) {
	t.Helper()
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s.SetStorage(blobs)
}

func putTestSnapshot(t *testing.T, s *Spawner, keyID string) {
	t.Helper()
	if err := s.Storage().Put(context.Background(), snapshotKey(runmode.LocalDefaultOrg, keyID), strings.NewReader("snapshot")); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
}

func assertSnapshotPresent(t *testing.T, s *Spawner, keyID string, want bool) {
	t.Helper()
	ok, err := s.Storage().Exists(context.Background(), snapshotKey(runmode.LocalDefaultOrg, keyID))
	if err != nil {
		t.Fatalf("Exists(%s): %v", keyID, err)
	}
	if ok != want {
		t.Errorf("snapshot for %s present = %v, want %v", keyID, ok, want)
	}
}
