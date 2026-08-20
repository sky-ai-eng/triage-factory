package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// WorkspaceSnapshotStore owns the workspace_snapshots table — one row per
// snapshot key recording whether that key's workspace blob is being written,
// was written, or failed, and which engagement owns the write.
//
// It exists because blob absence is ambiguous. `Get` returning not-found means
// any of: the persist is in flight, the persist failed, the conversation never
// had a workspace, or the retention reaper collected it. Only the first is
// worth waiting for and only the last two are really "gone", so a resume that
// reads the blob store alone has to refuse all four. This row is the
// disambiguation, and the writer's claim id on it is simultaneously the guard
// that keeps an older engagement's late upload from overwriting a successor's
// newer blob.
//
// Admin-pool `...System` variants only, same posture as ClaimCredentials: every
// caller is an executor-side teardown goroutine or a system sweep, none of them
// hold JWT claims, and no request handler reads the table. SQLite collapses the
// pools onto its one connection (N=1).
//
// Neither write returns the row it persisted, and that is deliberate rather
// than an oversight of the store-write standard. Handing a writer back the row
// it just wrote would say the one thing that must never be assumed here: that
// the writer still owns the key. The only legitimate read of that row is a
// FRESH one taken later, precisely to learn whether a successor displaced this
// writer in between — so the write returns the CAS outcome (did I still own
// it) rather than a picture of the row a caller could carry forward as truth.
type WorkspaceSnapshotStore interface {
	// BeginSnapshotSystem records that claimID owes a snapshot for this key:
	// state='pending', writer_claim_id=claimID, updated_at=now. An
	// unconditional upsert — a newer engagement starting its own snapshot
	// takes the key over, which is exactly what makes the completion CAS
	// below able to detect the older writer.
	//
	// Called before the capture starts, so that "a persist is owed" is
	// durable before a waiter could ever observe the conversation as
	// resumable-with-no-blob.
	BeginSnapshotSystem(ctx context.Context, orgID, blueprintRunID, claimID string) error

	// FinishSnapshotSystem closes out claimID's write: a CAS that flips
	// 'pending' to 'written' (ok) or 'failed' (not ok) only while
	// writer_claim_id is still claimID. matched=false means zero rows moved
	// — a successor re-owned the key (or the row is already terminal) — and
	// the caller treats that as superseded, not as an error.
	FinishSnapshotSystem(ctx context.Context, orgID, blueprintRunID, claimID string, ok bool) (matched bool, err error)

	// GetSnapshotStateSystem reads one key's lifecycle row, or (nil, nil)
	// when no snapshot lifecycle has ever started for it.
	GetSnapshotStateSystem(ctx context.Context, orgID, blueprintRunID string) (*domain.WorkspaceSnapshotState, error)

	// DeleteSnapshotStateSystem drops the key's lifecycle row. Paired with
	// the blob delete at terminal cleanup and in the retention reaper, so a
	// reaped snapshot reads as "no lifecycle" rather than as a `written` row
	// pointing at a blob that is gone. Idempotent — deleting a row that
	// isn't there is a no-op, not an error.
	DeleteSnapshotStateSystem(ctx context.Context, orgID, blueprintRunID string) error
}
