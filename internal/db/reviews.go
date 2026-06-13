package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=ReviewStore --output=./mocks --case=underscore --with-expecter

// ErrPendingReviewAlreadySubmitted is returned by ReviewStore.LockSubmission
// when the agent has already locked a pending review (its original_*
// columns are populated, meaning a prior submit-review call captured
// the agent's draft). It exists to give cmd/exec/gh/pr.go a clear
// sentinel for SKY-212's "block second submit" gate without leaking
// SQL specifics through the call stack.
var ErrPendingReviewAlreadySubmitted = errors.New("pending review already submitted to local approval queue")

// ErrPendingReviewCommentNotFound is returned by ReviewStore.UpdateComment
// and DeleteComment when no comment row matches the given id. The reviews
// handler matches on it to return 404 for a stale edit/delete while letting
// a real DB/RLS failure fall through to a redacted 500 — without it both
// outcomes would collapse into the same misleading 404.
var ErrPendingReviewCommentNotFound = errors.New("pending review comment not found")

// ReviewStore owns the pending_reviews + pending_review_comments
// tables — the agent-prepared GitHub review that sits in
// `pending_approval` until the user accepts or discards it.
//
// Audiences:
//
//   - Delegate spawner (internal/delegate/run.go) — DeleteByRunID on
//     run cleanup paths so a discarded / cancelled run doesn't strand
//     a review row.
//   - Reviews handler (internal/server/reviews_handler.go) — full
//     CRUD around the user-facing approve / edit / cancel flows.
//   - Server tasks handler (internal/server/tasks.go) — Delete on
//     swipe-dismiss of a pending_approval task.
//   - Server agent handler (internal/server/agent.go) — IsCommentID
//     for the inline-comment-in-transcript detection.
//   - cmd/exec/gh/pr.go (SKY-212) — LockSubmission to gate against
//     double-submit by the agent.
//
// Wired against the app pool in Postgres (RLS-active). The
// pending_reviews_all policy gates on (org_id, user_has_org_access,
// run reachability); pending_review_comments_all gates by parent
// review existence so the org check inherits transitively. Defense-
// in-depth `org_id` is included in every WHERE/INSERT regardless.
//
// SQLite has one connection; assertLocalOrg pins orgID to
// runmode.LocalDefaultOrgID.
//
// Return convention: Get / ByRunID return (nil, nil) when no row
// matches — a missing review is a normal outcome on the read paths,
// not an error. UpdateComment / DeleteComment surface "not found"
// as a wrapped error so the HTTP handler can distinguish a stale
// edit from a server error.
type ReviewStore interface {
	// --- Reviews ---

	// Create inserts a pending review row. ID is caller-supplied.
	Create(ctx context.Context, orgID string, r domain.PendingReview) error

	// Get returns a pending review by ID, or (nil, nil) on miss.
	// Preserves the original_review_* nullability so callers can
	// distinguish legacy rows (nil pointer) from snapshot-of-empty
	// (non-nil pointer to ""). See domain.PendingReview's comment.
	Get(ctx context.Context, orgID, reviewID string) (*domain.PendingReview, error)

	// ByRunID returns the pending review attached to a given agent
	// run **only** when the review has a deferred submission
	// (review_event populated). Returns (nil, nil) otherwise — used
	// by the approval flow to find the review to submit.
	ByRunID(ctx context.Context, orgID, runID string) (*domain.PendingReview, error)

	// Delete tears down a review and all its comments. Single
	// transaction in SQLite (no ON DELETE CASCADE on the comments
	// FK); single statement in Postgres where the FK cascades.
	Delete(ctx context.Context, orgID, reviewID string) error

	// DeleteByRunID does the same teardown keyed on the run that
	// produced the review. Used by the spawner's discard cleanup
	// (SKY-206) so a transient failure in a separate lookup-by-id
	// doesn't strand the review row. No-op on a run with no pending
	// review; safe to call unconditionally. NOTE: pending_reviews.run_id
	// is ON DELETE SET NULL, so this must be called BEFORE the run row
	// is deleted — once the run is gone the FK has nulled run_id and the
	// review survives detached (intentional), no longer matched here.
	DeleteByRunID(ctx context.Context, orgID, runID string) error

	// SetSubmission stores the deferred review body + event,
	// marking the review as ready for user approval rather than
	// immediate GitHub submission. original_review_* are populated
	// via COALESCE: the first call captures the agent's draft;
	// later calls (from the user-edit handler) leave the snapshot
	// untouched. Used by both the agent's initial submit and the
	// user's edit-before-approve flow.
	SetSubmission(ctx context.Context, orgID, reviewID, body, event string) error

	// LockSubmission is the agent-side variant of SetSubmission
	// used by cmd/exec/gh's prSubmitReview. Captures the agent's
	// draft AND seals the review against further agent submissions
	// in one atomic UPDATE. Returns
	// ErrPendingReviewAlreadySubmitted when the gate refuses (the
	// agent already locked this review); returns a wrapped
	// "not found" error when the review id doesn't exist (SKY-212
	// distinguishes the two so the agent's tool result is
	// unambiguous).
	LockSubmission(ctx context.Context, orgID, reviewID, body, event string) error

	// --- Comments ---

	// AddComment inserts a pending review comment, snapshotting
	// body into the write-once original_body column at the same
	// time so subsequent edits via UpdateComment mutate body only.
	AddComment(ctx context.Context, orgID string, c domain.PendingReviewComment) error

	// UpdateComment changes the body of an existing comment.
	// Returns a wrapped "not found" error when no row matches —
	// the handler turns that into a 404.
	UpdateComment(ctx context.Context, orgID, commentID, body string) error

	// DeleteComment removes a single comment row. Same "not found"
	// contract as UpdateComment.
	DeleteComment(ctx context.Context, orgID, commentID string) error

	// ListComments returns every comment attached to a review,
	// ordered by insertion. Preserves OriginalBody nullability the
	// same way Get preserves OriginalReviewBody.
	ListComments(ctx context.Context, orgID, reviewID string) ([]domain.PendingReviewComment, error)

	// IsCommentID is a cheap presence probe used by the agent
	// transcript renderer to detect inline-comment IDs that the
	// agent emits. Returns false on errors (best-effort).
	IsCommentID(ctx context.Context, orgID, commentID string) bool

	// ByRunIDSystem mirrors ByRunID but routes through the admin
	// pool in Postgres. The delegate spawner's processCompletion
	// reads the pending review attached to a run from a goroutine
	// that has detached from any request context, so it has no
	// JWT-claims in scope. Behavior matches the non-System variant;
	// the only difference is which Postgres pool the statement runs
	// on; SQLite has one connection and the two variants collapse.
	ByRunIDSystem(ctx context.Context, orgID, runID string) (*domain.PendingReview, error)

	// --- Admin-pool variants for the cmd/exec event-triggered branch (SKY-302) ---
	//
	// The agent CLI subcommand `triagefactory exec gh ...` runs in
	// the agent's subprocess and routes its writes via either
	// SyntheticClaimsWithTx (manual runs, with the run's
	// creator_user_id) or these `...System` admin-pool methods
	// (event-triggered runs, where no user identity exists).
	// Behavior matches the non-System variants verbatim; only the
	// Postgres pool changes. SQLite collapses both since it has one
	// connection.
	CreateSystem(ctx context.Context, orgID string, r domain.PendingReview) error
	GetSystem(ctx context.Context, orgID, reviewID string) (*domain.PendingReview, error)
	DeleteSystem(ctx context.Context, orgID, reviewID string) error
	LockSubmissionSystem(ctx context.Context, orgID, reviewID, body, event string) error
	AddCommentSystem(ctx context.Context, orgID string, c domain.PendingReviewComment) error
	UpdateCommentSystem(ctx context.Context, orgID, commentID, body string) error
	DeleteCommentSystem(ctx context.Context, orgID, commentID string) error
	ListCommentsSystem(ctx context.Context, orgID, reviewID string) ([]domain.PendingReviewComment, error)
}
