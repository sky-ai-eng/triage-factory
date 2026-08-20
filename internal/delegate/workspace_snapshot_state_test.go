package delegate

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// TestSnapshotWorkspace_SuccessRecordsWritten: a snapshot that lands leaves a
// durable `written` under the engagement that wrote it — the state a resume
// reads to know the blob it is about to fetch is whole rather than in flight.
func TestSnapshotWorkspace_SuccessRecordsWritten(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "snapstate-written")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, "_tfac", "notes.txt"), "work in progress")

	const claimID = "claim-writer"
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, claimID, wt, "", domain.ConversationRuntimeNative); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	assertSnapshotPresent(t, s, bpr, true)
	assertSnapshotState(t, s, bpr, domain.WorkspaceSnapshotWritten, claimID)
}

// TestSnapshotWorkspace_PutFailureRecordsFailed: the upload fails, and the
// failure is durable. Without the row a resume can only see "no blob", which it
// has to treat as maybe-still-coming; with it, the resume stops waiting and
// falls back immediately.
func TestSnapshotWorkspace_PutFailureRecordsFailed(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "snapstate-failed")
	wireBlobStore(t, s)
	putErr := errors.New("object store unreachable")
	s.SetStorage(failingPutStorage{Storage: s.Storage(), err: putErr})
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, "_tfac", "notes.txt"), "work in progress")

	const claimID = "claim-doomed"
	err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, claimID, wt, "", domain.ConversationRuntimeNative)
	if !errors.Is(err, putErr) {
		t.Fatalf("snapshotWorkspace error = %v, want the Put failure", err)
	}
	assertSnapshotState(t, s, bpr, domain.WorkspaceSnapshotFailed, claimID)
}

// TestSnapshotWorkspace_SupersededWriterSkipsPut is the stale-clobber guard: a
// successor takes the key over while this teardown is still capturing, and this
// teardown must not overwrite the newer blob with its older one. The successor's
// bytes are asserted intact — an unguarded Put would have replaced them, and
// with them the state a resume would rehydrate.
func TestSnapshotWorkspace_SupersededWriterSkipsPut(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "snapstate-superseded")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	const (
		oldClaim       = "claim-displaced"
		successorClaim = "claim-successor"
		successorBlob  = "the successor's newer snapshot"
	)
	// The takeover happens at the one instant that makes this a race rather
	// than a check: after the displaced engagement has recorded its own
	// pending, and before it reaches the upload.
	real := s.workspaceSnapshots
	s.workspaceSnapshots = takeoverSnapshotStore{WorkspaceSnapshotStore: real, afterBegin: func() {
		ctx := context.Background()
		if err := real.BeginSnapshotSystem(ctx, runmode.LocalDefaultOrgID, bpr, successorClaim); err != nil {
			t.Errorf("successor begin: %v", err)
		}
		putTestSnapshotBytes(t, s, bpr, successorBlob)
	}}

	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, "_tfac", "notes.txt"), "the displaced engagement's older work")

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, oldClaim, wt, "", domain.ConversationRuntimeNative); err != nil {
		t.Fatalf("a superseded snapshot is not a failure: %v", err)
	}

	if got := readSnapshotBlob(t, s, bpr); got != successorBlob {
		t.Errorf("blob = %q, want the successor's bytes %q — the displaced engagement overwrote a newer snapshot", got, successorBlob)
	}
	// The row stays the successor's, still pending: the displaced writer's CAS
	// matches nothing, so it cannot close out someone else's write either.
	assertSnapshotState(t, s, bpr, domain.WorkspaceSnapshotPending, successorClaim)
}

// TestSnapshotWorkspace_NoClaimRecordsNothing: a claimless caller (a manual
// path with no engagement, a fixture) still snapshots, and records no
// lifecycle — there is nothing to fence a CAS on, and no row is the honest
// answer rather than one nobody owns.
func TestSnapshotWorkspace_NoClaimRecordsNothing(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "snapstate-claimless")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, "_tfac", "notes.txt"), "work in progress")

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, bpr, "", wt, "", domain.ConversationRuntimeNative); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}
	assertSnapshotPresent(t, s, bpr, true)
	if got := snapshotState(t, s, bpr); got != nil {
		t.Errorf("state = %+v, want none for a claimless write", got)
	}
}

// TestParkConversationOpen_FencedTeardownRecordsWrittenState is the cross-pod
// stop shape: control parked the conversation and released the claim, so this
// engagement's status flip is refused by the fence — and its snapshot, the one
// thing the teardown still owes, is recorded exactly as an unfenced park's is.
// The state write keys on the claim id, not on winning the flip.
func TestParkConversationOpen_FencedTeardownRecordsWrittenState(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "snapstate-fenced")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	s.conversations = &fencedConversationStore{ConversationStore: s.conversations}

	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, "_tfac", "notes.txt"), "half-finished work")

	const claimID = "claim-fenced"
	fenced := s.parkConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: conversationID,
		claudeCwd:      wt,
		namespace:      bpr,
		triggerType:    "event",
		claimID:        claimID,
		reason:         db.ParkStopped("user_cancelled", "Cancelled by user"),
	}, "")
	if !fenced {
		t.Fatal("the teardown did not report the fence trip")
	}
	assertSnapshotPresent(t, s, bpr, true)
	assertSnapshotState(t, s, bpr, domain.WorkspaceSnapshotWritten, claimID)
}

// TestReapExpiredSnapshots_DropsStateWithTheBlob: the retention sweep takes the
// lifecycle row with the blob. Leaving the row behind would manufacture the one
// reading nothing can honor — `written`, pointing at a blob that is gone.
func TestReapExpiredSnapshots_DropsStateWithTheBlob(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	ctx := context.Background()
	s, database, conversationID, _ := setupAdvanceFixture(t, "snapstate-reap")
	wireBlobStore(t, s)
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	if _, err := database.Exec(
		`UPDATE conversations SET status='completed', outcome='abort', completed_at=datetime('now','-20 days') WHERE id=?`,
		conversationID); err != nil {
		t.Fatalf("age the conversation: %v", err)
	}
	putTestSnapshot(t, s, bpr)
	if err := s.workspaceSnapshots.BeginSnapshotSystem(ctx, runmode.LocalDefaultOrgID, bpr, "claim-reaped"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.workspaceSnapshots.FinishSnapshotSystem(ctx, runmode.LocalDefaultOrgID, bpr, "claim-reaped", true); err != nil {
		t.Fatalf("finish: %v", err)
	}

	s.ReapExpiredSnapshots(ctx)

	assertSnapshotPresent(t, s, bpr, false)
	if got := snapshotState(t, s, bpr); got != nil {
		t.Errorf("state survived the reap: %+v", got)
	}
}

// failingPutStorage is a blob store whose uploads always fail, so a test can
// drive the snapshot's error path without corrupting a real backend.
type failingPutStorage struct {
	storage.Storage
	err error
}

func (f failingPutStorage) Put(context.Context, string, io.Reader) error { return f.err }

// takeoverSnapshotStore runs afterBegin the instant a writer records its
// pending state, which is where a successor's takeover has to land for the
// pre-upload guard to be the thing under test.
type takeoverSnapshotStore struct {
	db.WorkspaceSnapshotStore
	afterBegin func()
}

func (t takeoverSnapshotStore) BeginSnapshotSystem(ctx context.Context, orgID, blueprintRunID, claimID string) error {
	if err := t.WorkspaceSnapshotStore.BeginSnapshotSystem(ctx, orgID, blueprintRunID, claimID); err != nil {
		return err
	}
	if t.afterBegin != nil {
		t.afterBegin()
	}
	return nil
}

func snapshotState(t *testing.T, s *Spawner, keyID string) *domain.WorkspaceSnapshotState {
	t.Helper()
	got, err := s.workspaceSnapshots.GetSnapshotStateSystem(context.Background(), runmode.LocalDefaultOrgID, keyID)
	if err != nil {
		t.Fatalf("read snapshot state for %s: %v", keyID, err)
	}
	return got
}

func assertSnapshotState(t *testing.T, s *Spawner, keyID, wantState, wantWriter string) {
	t.Helper()
	got := snapshotState(t, s, keyID)
	if got == nil {
		t.Fatalf("no snapshot state for %s, want %s under %s", keyID, wantState, wantWriter)
	}
	if got.State != wantState || got.WriterClaimID != wantWriter {
		t.Errorf("snapshot state = {%s, %s}, want {%s, %s}", got.State, got.WriterClaimID, wantState, wantWriter)
	}
}

func putTestSnapshotBytes(t *testing.T, s *Spawner, keyID, body string) {
	t.Helper()
	if err := s.Storage().Put(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, keyID), strings.NewReader(body)); err != nil {
		t.Fatalf("put snapshot bytes for %s: %v", keyID, err)
	}
}

func readSnapshotBlob(t *testing.T, s *Spawner, keyID string) string {
	t.Helper()
	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, keyID))
	if err != nil {
		t.Fatalf("get snapshot %s: %v", keyID, err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read snapshot %s: %v", keyID, err)
	}
	return string(body)
}
