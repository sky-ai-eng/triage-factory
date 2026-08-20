package domain

import "time"

// Workspace-snapshot lifecycle states, the stored vocabulary of
// workspace_snapshots.state (CHECK-constrained in both dialects).
//
// The absence of a row is a fourth answer and the most common one: no
// snapshot lifecycle has ever started for that key, either because the
// conversation never got far enough to have a workspace or because the
// retention reaper collected the blob and its state together.
const (
	// WorkspaceSnapshotPending — an engagement has decided to snapshot and
	// the write is owed. The blob may not exist yet; writer_claim_id names
	// who is producing it.
	WorkspaceSnapshotPending = "pending"
	// WorkspaceSnapshotWritten — the blob landed under the snapshot key.
	WorkspaceSnapshotWritten = "written"
	// WorkspaceSnapshotFailed — the capture, archive, or upload errored. The
	// durable answer that lets a waiter stop waiting and fall back rather
	// than time out.
	WorkspaceSnapshotFailed = "failed"
)

// WorkspaceSnapshotState is one workspace_snapshots row: the lifecycle of the
// blob under one snapshot key — (OrgID, BlueprintRunID), because a blueprint's
// steps share one workspace tree and one blob.
//
// WriterClaimID is the engagement that owns the write. It answers two
// questions: whether waiting for a pending write is worthwhile (resolve the
// claim's executor and ask whether it is still alive), and whether a writer
// about to upload has been superseded by a newer engagement that took the key.
type WorkspaceSnapshotState struct {
	OrgID          string
	BlueprintRunID string
	State          string
	WriterClaimID  string
	UpdatedAt      time.Time
}
