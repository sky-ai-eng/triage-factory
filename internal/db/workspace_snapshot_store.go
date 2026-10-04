package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrSnapshotSuperseded is BeginSnapshotSystem's refusal of a writer whose
// successor already holds the key. The caller writes nothing: no blob and no
// lifecycle outcome, both of which are the successor's.
var ErrSnapshotSuperseded = errors.New("db: a newer engagement holds this workspace snapshot key")

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
// claimID is a precondition, not a validated input: callers pass a real claim
// id, and the guard for "this engagement has no claim" lives at the caller
// (delegate's snapshot writer skips the record entirely rather than writing a
// row nobody owns). The dialects do not agree on what an empty or malformed one
// does — Postgres casts it to uuid and rejects, SQLite stores the string as
// given — so a caller that hands these methods an empty id gets a different
// answer per backend, and neither answer is one to build on.
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
	// state='pending', writer_claim_id=claimID, updated_at=now. A newer
	// engagement starting its own snapshot takes the key over, which is
	// exactly what makes the completion CAS below able to detect the older
	// writer.
	//
	// An older engagement never takes it back. The begin is refused with
	// ErrSnapshotSuperseded, and the row left as it is, when the key's
	// current writer is a claim minted after claimID: that writer holds a
	// newer tree, and an older begin landing late would re-own the key, make
	// the newer writer's own upload read itself as superseded, and leave the
	// key with no blob at all. Newer is by claim mint time, whichever
	// conversation on the task the claim belongs to, because every step of
	// a blueprint shares the key. Two claims minted in the same instant are
	// still ordered, so of any two one is the newer; otherwise each could
	// take the key back from the other. Whether claimID is still live does not
	// enter into it: a park releases its claim before it snapshots, and that
	// snapshot is the newest tree there is until a later claim exists. A
	// writer whose claim row is not found supersedes nothing and is
	// superseded by nothing.
	//
	// Called before the capture starts, so that "a persist is owed" is
	// durable before a waiter could ever observe the conversation as
	// resumable-with-no-blob.
	//
	// A begin that lands clears the covered position: it describes the blob
	// being replaced.
	BeginSnapshotSystem(ctx context.Context, orgID, taskID, claimID string) error

	// CoverSnapshotSystem records that claimID's written blob also reflects
	// the transcript up to position: a checkpoint captured the tree, found its
	// fingerprint equal to the blob's, and skipped the upload, which leaves
	// the position in the blob's manifest older than what the blob covers.
	// fingerprint is the tree the position describes; a restore applies the
	// position only to a blob whose manifest carries the same fingerprint and
	// writer, so it is never read against a different tree.
	//
	// A CAS on writer_claim_id = claimID and state 'written': that is what
	// makes the blob under the key the one this claim last wrote, the one its
	// fingerprint was compared against. matched=false means another writer
	// holds the key or this claim's latest write did not finish, and nothing
	// is recorded.
	CoverSnapshotSystem(ctx context.Context, orgID, taskID, claimID, fingerprint string, position float64) (matched bool, err error)

	// FinishSnapshotSystem closes out claimID's write: a CAS that flips
	// 'pending' to 'written' (ok) or 'failed' (not ok) only while
	// writer_claim_id is still claimID. matched=false means zero rows moved
	// — a successor re-owned the key (or the row is already terminal) — and
	// the caller treats that as superseded, not as an error.
	FinishSnapshotSystem(ctx context.Context, orgID, taskID, claimID string, ok bool) (matched bool, err error)

	// GetSnapshotStateSystem reads one key's lifecycle row, or (nil, nil)
	// when no snapshot lifecycle has ever started for it.
	GetSnapshotStateSystem(ctx context.Context, orgID, taskID string) (*domain.WorkspaceSnapshotState, error)

	// DeleteSnapshotStateSystem drops the key's lifecycle row. Paired with
	// the blob delete at terminal cleanup and in the retention reaper, so a
	// reaped snapshot reads as "no lifecycle" rather than as a `written` row
	// pointing at a blob that is gone. Idempotent — deleting a row that
	// isn't there is a no-op, not an error.
	DeleteSnapshotStateSystem(ctx context.Context, orgID, taskID string) error
}
