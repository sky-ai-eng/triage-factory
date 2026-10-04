package delegate

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// takeOver releases the fixture's claim the way expiry handling does and mints
// the successor's, which is the newer of the two.
func (f checkpointFixture) takeOver(t *testing.T) (successor string) {
	t.Helper()
	if _, err := f.database.Exec(
		`UPDATE claims SET released_at = CURRENT_TIMESTAMP, outcome = 'reaped' WHERE id = ?`, f.claimID,
	); err != nil {
		t.Fatalf("release the old claim: %v", err)
	}
	successor = uuid.New().String()
	if _, err := f.database.Exec(`
		INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, claimed_at, lease_expires_at)
		VALUES (?, ?, ?, 'successor-executor', 1, ?, strftime('%Y-%m-%d %H:%M:%f','now','+300.000 seconds'))
	`, successor, runmode.LocalDefaultOrgID, f.conversationID, time.Now().UTC()); err != nil {
		t.Fatalf("mint the successor's claim: %v", err)
	}
	return successor
}

// TestCheckpoint_ALateBeginCannotTakeTheKeyFromASuccessor is the zombie
// checkpoint: an engagement whose claim was taken over reaches its record
// open after the successor opened its own. Its begin is refused and it writes
// nothing, and the successor's snapshot is the one the key holds.
//
// Were the late begin to take the record, the successor's upload would read
// itself as superseded and skip its Put, and the key would hold the older
// tree under the older writer, or no blob at all.
func TestCheckpoint_ALateBeginCannotTakeTheKeyFromASuccessor(t *testing.T) {
	f := newCheckpointFixture(t, "r-ckpt-late-begin")
	puts := &countingPutStorage{Storage: f.s.Storage()}
	f.s.SetStorage(puts)
	state := filepath.Join(f.tree, worktree.ScratchDir, "state.txt")
	zombie := f.claimID
	successor := f.takeOver(t)

	if !f.s.beginSnapshotState(context.Background(), runmode.LocalDefaultOrgID, f.keyID, successor).owned() {
		t.Fatal("the successor could not open the key's record")
	}

	writeFile(t, state, "as the zombie read it")
	pos := 5.0
	res := f.s.writeCheckpoint(context.Background(), snapshotWrite{
		orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, keyID: f.keyID, claimID: zombie,
		wtPath: f.tree, runtime: domain.ConversationRuntimeNative, reason: snapshotReasonCheckpoint, position: &pos,
	}, "", func() {})
	if res.outcome == checkpointWritten {
		t.Fatal("the zombie's checkpoint was written over a key its successor holds")
	}
	if got := puts.count(); got != 0 {
		t.Fatalf("uploads = %d, want none from the zombie", got)
	}
	assertSnapshotState(t, f.s, f.keyID, domain.WorkspaceSnapshotPending, successor)

	writeFile(t, state, "as the successor left it")
	if err := f.s.persistWorkspaceSnapshot(context.Background(), snapshotWrite{
		orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, keyID: f.keyID, claimID: successor,
		wtPath: f.tree, runtime: domain.ConversationRuntimeNative, reason: snapshotReasonPark,
	}, true); err != nil {
		t.Fatalf("successor persist: %v", err)
	}
	_, files := f.blob(t)
	if got := files["state.txt"]; got != "as the successor left it" {
		t.Fatalf("the key holds %q, want the successor's tree", got)
	}
	assertSnapshotState(t, f.s, f.keyID, domain.WorkspaceSnapshotWritten, successor)
}

// TestSnapshotWorkspace_ASupersededEndingWritesNothing: an ending whose begin
// is refused — its engagement was taken over and the successor already holds
// the key — writes no blob and no outcome, and is not an error.
func TestSnapshotWorkspace_ASupersededEndingWritesNothing(t *testing.T) {
	f := newCheckpointFixture(t, "r-ending-superseded")
	zombie := f.claimID
	successor := f.takeOver(t)
	if !f.s.beginSnapshotState(context.Background(), runmode.LocalDefaultOrgID, f.keyID, successor).owned() {
		t.Fatal("the successor could not open the key's record")
	}
	puts := &countingPutStorage{Storage: f.s.Storage()}
	f.s.SetStorage(puts)

	if err := f.s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conversationID, f.keyID, zombie, f.tree, "", domain.ConversationRuntimeNative); err != nil {
		t.Fatalf("a superseded snapshot is not a failure: %v", err)
	}
	if got := puts.count(); got != 0 {
		t.Fatalf("uploads = %d, want none from the superseded ending", got)
	}
	assertSnapshotState(t, f.s, f.keyID, domain.WorkspaceSnapshotPending, successor)
}
