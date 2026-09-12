package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRekeyWorkspaceBlobsToTask_MovesTheNewestRunAndDropsTheRest is the boot
// half of the re-key: the migration rewrote the lifecycle rows, and this moves
// the objects those rows describe. A task that delegated twice ends with one
// blob, at the task key, holding the newer run's bytes — the older run's is
// dropped, because its row is gone and a blob with no row is the one state the
// record exists to rule out.
func TestRekeyWorkspaceBlobsToTask_MovesTheNewestRunAndDropsTheRest(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	s, database, conversationID, taskID := setupAdvanceFixture(t, "rekey")
	wireBlobStore(t, s)

	// Two runs on one task, the fixture's own and a newer one, each with the
	// blob its park wrote under the old run key.
	olderRun := blueprintRunIDForConversation(t, database, conversationID)
	newerRun := seedConversationBlueprint(t, database, "rekey-newer", taskID)
	if _, err := database.Exec(
		`UPDATE blueprint_runs SET started_at = datetime('now', '-1 day') WHERE id = ?`, olderRun); err != nil {
		t.Fatalf("age the older run: %v", err)
	}
	putTestSnapshotBytes(t, s, olderRun, "the first delegation's workspace")
	putTestSnapshotBytes(t, s, newerRun, "the second delegation's workspace")

	s.RekeyWorkspaceBlobsToTask(ctx, org)

	if got := readSnapshotBlob(t, s, taskID); got != "the second delegation's workspace" {
		t.Errorf("blob at the task key = %q, want the newer run's bytes — a resume continues the most recent workspace", got)
	}
	for _, run := range []string{olderRun, newerRun} {
		ok, err := s.Storage().Exists(ctx, snapshotKey(org, run))
		if err != nil {
			t.Fatalf("Exists(%s): %v", run, err)
		}
		if ok {
			t.Errorf("run-keyed blob for %s survived; nothing reads that key any more, so it is pure leaked storage", run)
		}
	}
}

// TestRekeyWorkspaceBlobsToTask_IsIdempotent: the sweep runs on every boot, so
// a second pass must find nothing to do and leave the blob it moved alone. A
// key that names no blueprint run is already a task key.
func TestRekeyWorkspaceBlobsToTask_IsIdempotent(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	s, database, conversationID, taskID := setupAdvanceFixture(t, "rekey-twice")
	wireBlobStore(t, s)

	run := blueprintRunIDForConversation(t, database, conversationID)
	putTestSnapshotBytes(t, s, run, "the only workspace")

	s.RekeyWorkspaceBlobsToTask(ctx, org)
	s.RekeyWorkspaceBlobsToTask(ctx, org)

	if got := readSnapshotBlob(t, s, taskID); got != "the only workspace" {
		t.Errorf("blob at the task key after two passes = %q, want it untouched", got)
	}
	objects, err := s.Storage().List(ctx, org+"/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var keys []string
	for _, o := range objects {
		keys = append(keys, o.Key)
	}
	if len(keys) != 1 || keys[0] != snapshotKey(org, taskID) {
		t.Errorf("blobs after two passes = %v, want just %s", keys, snapshotKey(org, taskID))
	}
}

// TestRekeyWorkspaceBlobsToTask_FinishesAnInterruptedMove: a process that died
// between the copy and the delete left the blob at both keys. The task key
// holds the finished move, so the next pass must drop the source rather than
// copy over it — copying would put the older bytes back on top of the newer.
func TestRekeyWorkspaceBlobsToTask_FinishesAnInterruptedMove(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	s, database, conversationID, taskID := setupAdvanceFixture(t, "rekey-interrupted")
	wireBlobStore(t, s)

	run := blueprintRunIDForConversation(t, database, conversationID)
	putTestSnapshotBytes(t, s, run, "the source the crash left behind")
	putTestSnapshotBytes(t, s, taskID, "the copy that already landed")

	s.RekeyWorkspaceBlobsToTask(ctx, org)

	if got := readSnapshotBlob(t, s, taskID); got != "the copy that already landed" {
		t.Errorf("blob at the task key = %q, want the copy that already landed — the source must not overwrite it", got)
	}
	if ok, err := s.Storage().Exists(ctx, snapshotKey(org, run)); err != nil || ok {
		t.Errorf("source blob present = %v (err %v), want it dropped once the destination holds the move", ok, err)
	}
}

// TestSnapshotKeyID_RecognizesOnlyItsOwnKeys: the sweep hands whatever it finds
// under the org prefix to a blueprint-run lookup and then to a delete, so the
// shape check is what keeps another consumer's object out of both.
func TestSnapshotKeyID_RecognizesOnlyItsOwnKeys(t *testing.T) {
	const org = "org-1"
	for _, tc := range []struct {
		key      string
		wantID   string
		wantOK   bool
		whyNotOK string
	}{
		{key: snapshotKey(org, "key-1"), wantID: "key-1", wantOK: true},
		{key: "other-org/key-1/workspace.tar", whyNotOK: "another tenant's blob"},
		{key: org + "/key-1/something-else.bin", whyNotOK: "another consumer's object under our key"},
		{key: org + "/key-1/nested/workspace.tar", whyNotOK: "a deeper layout we do not write"},
		{key: org + "/workspace.tar", whyNotOK: "no key segment at all"},
	} {
		gotID, gotOK := snapshotKeyID(org, tc.key)
		if gotOK != tc.wantOK || gotID != tc.wantID {
			what := tc.whyNotOK
			if what == "" {
				what = "our own snapshot key"
			}
			t.Errorf("snapshotKeyID(%q) = (%q, %v), want (%q, %v) — %s", tc.key, gotID, gotOK, tc.wantID, tc.wantOK, what)
		}
	}
	// And the two spellings agree: every key snapshotKey composes is one
	// snapshotKeyID reads back.
	if id, ok := snapshotKeyID(org, snapshotKey(org, "round-trip")); !ok || id != "round-trip" {
		t.Errorf("round trip = (%q, %v), want (\"round-trip\", true)", id, ok)
	}
	if !strings.HasSuffix(snapshotKey(org, "x"), snapshotBlobLeaf) {
		t.Errorf("snapshotKey does not end in %q; the reader recognizes nothing the writer produces", snapshotBlobLeaf)
	}
}
