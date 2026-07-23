package delegate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// TestProcessCompletion_DraftPRWritesNoSnapshot: a completed step that queued a
// draft PR no longer parks or snapshots. The artifact is an async
// sidecar — the step completes with its real outcome (continue) and is not
// resumable through a parked status, so no workspace snapshot is written.
func TestProcessCompletion_DraftPRWritesNoSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, runID, taskID := setupAdvanceFixture(t, "draftpr-nosnap")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForRun(t, database, runID)

	seedDraftPRArtifact(t, s, runID)

	task := loadTask(t, s, taskID)
	parked := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, runID, bpr, task,
		res(`{"outcome":"continue","summary":"opened a PR"}`), t.TempDir(), "", "event", "", 0)

	if parked {
		t.Fatal("processCompletion(draft PR) = true; want parked=false (a draft PR is a sidecar; the step never parks)")
	}
	if run := loadRun(t, s, runID); run.Status != "completed" || run.Outcome != "continue" {
		t.Fatalf("run = {status:%q outcome:%q}, want {completed continue}", run.Status, run.Outcome)
	}
	assertSnapshotPresent(t, s, bpr, false)
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
	parked := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, runID, bpr, task,
		res(`{"outcome":"abort","summary":"looked into it","reason":"needs a human to rotate the token"}`),
		t.TempDir(), "", "event", "", 0)

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
	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, runID, bpr, task,
		res(`{"outcome":"finish","summary":"shipped it"}`), t.TempDir(), "", "event", "", 0)

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

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "", time.Now(),
		runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusAborted, "needs a human", nil, true)

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

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "", time.Now(),
		runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusCompleted, "", nil, true)

	assertSnapshotPresent(t, s, bpr, false)
}

// TestReapExpiredSnapshots_DropsExpiredKeepsFresh is the end-to-end retention
// sweep: a completed+abort run parked past the TTL has its snapshot reaped, while
// a freshly-parked one within the TTL survives.
func TestReapExpiredSnapshots_DropsExpiredKeepsFresh(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, oldRun, _ := setupAdvanceFixture(t, "reap-old")
	wireBlobStore(t, s)

	oldBpr := blueprintRunIDForRun(t, database, oldRun)
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort', completed_at=datetime('now','-20 days') WHERE id=?`, oldRun); err != nil {
		t.Fatalf("age old run: %v", err)
	}
	putTestSnapshot(t, s, oldBpr)

	seedRun(t, database, "r-fresh", "sess-fresh", "/tmp/wt-fresh")
	freshBpr := blueprintRunIDForRun(t, database, "r-fresh")
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort', completed_at=datetime('now') WHERE id='r-fresh'`); err != nil {
		t.Fatalf("complete fresh run: %v", err)
	}
	putTestSnapshot(t, s, freshBpr)

	s.ReapExpiredSnapshots(context.Background())

	assertSnapshotPresent(t, s, oldBpr, false)  // parked past the TTL → reaped
	assertSnapshotPresent(t, s, freshBpr, true) // within the TTL → kept
}

// TestListReapableSnapshotKeys_ExcludesFinishAndInTTL pins the query rules: a
// past-TTL completed+abort key is eligible, a past-TTL finish run is excluded
// (not resumable), and a within-TTL open run is not yet eligible.
func TestListReapableSnapshotKeys_ExcludesFinishAndInTTL(t *testing.T) {
	s, database, abortRun, _ := setupAdvanceFixture(t, "reap-rules")
	abortBpr := blueprintRunIDForRun(t, database, abortRun)
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort', completed_at=datetime('now','-30 days') WHERE id=?`, abortRun); err != nil {
		t.Fatalf("age abort run: %v", err)
	}

	seedRun(t, database, "r-fin2", "s", "/tmp/wt")
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='finish', completed_at=datetime('now','-30 days') WHERE id='r-fin2'`); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	seedRun(t, database, "r-open2", "s", "/tmp/wt")
	if _, err := database.Exec(`UPDATE conversations SET status='open', completed_at=NULL, started_at=datetime('now') WHERE id='r-open2'`); err != nil {
		t.Fatalf("open run: %v", err)
	}

	cutoff := time.Now().Add(-14 * 24 * time.Hour)
	keys, err := s.agentRuns.ListReapableSnapshotKeysSystem(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("ListReapableSnapshotKeysSystem: %v", err)
	}
	if len(keys) != 1 || keys[0].BlueprintRunID != abortBpr {
		t.Fatalf("reapable keys = %+v, want exactly the old abort key %s (finish excluded, fresh open excluded)", keys, abortBpr)
	}
	if keys[0].OrgID != runmode.LocalDefaultOrgID {
		t.Errorf("key org = %q, want %q", keys[0].OrgID, runmode.LocalDefaultOrgID)
	}
}

// TestListReapableSnapshotKeys_SharedBlueprintNeedsAllPastTTL pins the shared-key
// rule: blueprint steps share one snapshot blob, so a blueprint whose runs span
// the TTL boundary (one old, one fresh) is NOT reaped until every resumable run
// is past the TTL.
func TestListReapableSnapshotKeys_SharedBlueprintNeedsAllPastTTL(t *testing.T) {
	s, database, run1, taskID := setupAdvanceFixture(t, "reap-shared")
	bpr := blueprintRunIDForRun(t, database, run1)
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort', completed_at=datetime('now','-30 days') WHERE id=?`, run1); err != nil {
		t.Fatalf("age step 1: %v", err)
	}
	// A second step on the SAME blueprint_run, also completed+abort but fresh.
	addStepRun(t, database, bpr, taskID, "run2-shared", 1, "running")
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort', completed_at=datetime('now') WHERE id='run2-shared'`); err != nil {
		t.Fatalf("complete step 2: %v", err)
	}

	cutoff := time.Now().Add(-14 * 24 * time.Hour)
	keys, err := s.agentRuns.ListReapableSnapshotKeysSystem(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("ListReapableSnapshotKeysSystem: %v", err)
	}
	for _, k := range keys {
		if k.BlueprintRunID == bpr {
			t.Errorf("shared blueprint %s reaped while a step is still within the TTL", bpr)
		}
	}
}

// TestMarkOpenResuming_StampsAndClearsParkedAt pins the park-timestamp
// lifecycle: MarkOpen stamps parked_at (so retention keys off the last park),
// the resume-by-enqueue flip clears it (the run is no longer parked).
func TestMarkOpenResuming_StampsAndClearsParkedAt(t *testing.T) {
	s, database, run, _ := setupAdvanceFixture(t, "parked-stamp")
	ctx := context.Background()

	if _, err := s.agentRuns.MarkOpen(ctx, runmode.LocalDefaultOrgID, run); err != nil {
		t.Fatalf("MarkOpen: %v", err)
	}
	if !parkedAtSet(t, database, run) {
		t.Error("MarkOpen did not stamp parked_at")
	}

	if _, err := s.agentRuns.MarkQueuedForResume(ctx, runmode.LocalDefaultOrgID, run); err != nil {
		t.Fatalf("MarkQueuedForResume: %v", err)
	}
	if parkedAtSet(t, database, run) {
		t.Error("MarkQueuedForResume did not clear parked_at")
	}
}

// TestListReapableSnapshotKeys_OpenRunKeysOffParkedAt is the fix for the
// retention-timestamp concern: a long-lived open run started weeks ago but
// re-parked recently must NOT be reaped (retention keys off the last park via
// parked_at, not the never-resetting started_at); once the park itself ages past
// the TTL it becomes reapable.
func TestListReapableSnapshotKeys_OpenRunKeysOffParkedAt(t *testing.T) {
	s, database, run, _ := setupAdvanceFixture(t, "reap-parked")
	bpr := blueprintRunIDForRun(t, database, run)
	cutoff := time.Now().Add(-14 * 24 * time.Hour)

	// Started 30 days ago, last re-parked just now → must survive.
	if _, err := database.Exec(`UPDATE conversations SET status='open', started_at=datetime('now','-30 days'), parked_at=datetime('now') WHERE id=?`, run); err != nil {
		t.Fatalf("seed recently-parked open run: %v", err)
	}
	if keysContain(reapKeys(t, s, cutoff), bpr) {
		t.Errorf("recently re-parked open run %s reaped on its old started_at; retention must key off parked_at", bpr)
	}

	// Age the park past the TTL → now reapable.
	if _, err := database.Exec(`UPDATE conversations SET parked_at=datetime('now','-30 days') WHERE id=?`, run); err != nil {
		t.Fatalf("age park: %v", err)
	}
	if !keysContain(reapKeys(t, s, cutoff), bpr) {
		t.Errorf("open run parked 30 days ago not reaped; want reapable past the TTL")
	}
}

// --- helpers ---------------------------------------------------------------

func parkedAtSet(t *testing.T, database *sql.DB, runID string) bool {
	t.Helper()
	var pa sql.NullString
	if err := database.QueryRow(`SELECT parked_at FROM conversations WHERE id=?`, runID).Scan(&pa); err != nil {
		t.Fatalf("read parked_at: %v", err)
	}
	return pa.Valid
}

func reapKeys(t *testing.T, s *Spawner, cutoff time.Time) []domain.SnapshotReapKey {
	t.Helper()
	keys, err := s.agentRuns.ListReapableSnapshotKeysSystem(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("ListReapableSnapshotKeysSystem: %v", err)
	}
	return keys
}

func keysContain(keys []domain.SnapshotReapKey, blueprintRunID string) bool {
	for _, k := range keys {
		if k.BlueprintRunID == blueprintRunID {
			return true
		}
	}
	return false
}

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
	if err := s.Storage().Put(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, keyID), strings.NewReader("snapshot")); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
}

func assertSnapshotPresent(t *testing.T, s *Spawner, keyID string, want bool) {
	t.Helper()
	ok, err := s.Storage().Exists(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, keyID))
	if err != nil {
		t.Fatalf("Exists(%s): %v", keyID, err)
	}
	if ok != want {
		t.Errorf("snapshot for %s present = %v, want %v", keyID, ok, want)
	}
}
