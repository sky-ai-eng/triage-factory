package dbtest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ReviewStoreFactory is what a per-backend test file hands to
// RunReviewStoreConformance. Returns:
//   - the wired ReviewStore impl,
//   - the orgID to pass to every call,
//   - a ReviewSeeder for fixtures the harness can't create through
//     the store itself (a run row that pending_reviews.run_id can
//     FK to, plus a setLegacyOriginal escape hatch for the
//     pre-SKY-205 legacy-row subtests).
type ReviewStoreFactory func(t *testing.T) (
	store db.ReviewStore,
	orgID string,
	seed ReviewSeeder,
)

// ReviewSeeder bags the raw-SQL helpers backend tests provide.
type ReviewSeeder struct {
	// Run inserts a runs row and returns its id. pending_reviews
	// has a nullable FK to runs(id); tests that exercise
	// DeleteByRunID + ByRunID need a real row to point at.
	Run func(t *testing.T) string

	// SetReviewOriginals overwrites original_review_body /
	// original_review_event directly. Used to set up the legacy-
	// row subtests where review_event is populated (a prior
	// submission) but original_review_event is NULL (the row was
	// created before SKY-205 added the column). The store has no
	// surface for this — it's a backfill/migration shape.
	SetReviewOriginals func(t *testing.T, reviewID string, body, event *string)

	// SetCommentOriginalNull writes NULL into original_body for
	// the given comment id. Used by the "legacy comment row"
	// subtest, again a migration-era shape with no store surface.
	SetCommentOriginalNull func(t *testing.T, commentID string)
}

// RunReviewStoreConformance covers the review-store contract every
// backend impl must hold:
//
//   - Create / Get / ByRunID round-trip. Get returns (nil, nil) on
//     miss; ByRunID returns nil for rows whose review_event is
//     still empty.
//   - AddComment captures original_body == body (write-once).
//   - UpdateComment mutates body only; original_body unchanged.
//   - DeleteComment / UpdateComment surface "not found" wrapped
//     errors.
//   - SetSubmission write-once originals (first call captures;
//     later calls don't overwrite).
//   - LockSubmission gate: first call captures originals; second
//     returns ErrPendingReviewAlreadySubmitted; bogus reviewID
//     returns a different "not found" error so the agent's tool
//     result can distinguish.
//   - LockSubmission treats legacy SKY-204-era rows (review_event
//     populated but original_review_event NULL) as already
//     submitted — gates on review_event not original_review_event.
//   - Delete / DeleteByRunID tear down review + cascaded comments.
//   - IsCommentID returns true for known comment ids; false on
//     anything else.
//   - Get + ListComments preserve nullability of original_*: nil
//     for legacy rows, non-nil pointer to "" for snapshot-of-empty.
func RunReviewStoreConformance(t *testing.T, mk ReviewStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Create_then_Get_round_trips", func(t *testing.T) {
		s, orgID, _ := mk(t)
		id := uuid.New().String()
		if err := s.Create(ctx, orgID, domain.PendingReview{
			ID: id, PRNumber: 7, Owner: "sky-ai-eng", Repo: "triage-factory",
			CommitSHA: "abc123", DiffLines: `{"f.go":[1,2]}`, DiffHunks: `{"f.go":[[1,2]]}`,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := s.Get(ctx, orgID, id)
		if err != nil || got == nil {
			t.Fatalf("Get: got=%v err=%v", got, err)
		}
		if got.PRNumber != 7 || got.Owner != "sky-ai-eng" || got.CommitSHA != "abc123" {
			t.Errorf("round-trip mismatch: %+v", got)
		}
		if got.OriginalReviewBody != nil || got.OriginalReviewEvent != nil {
			t.Errorf("fresh row should have nil originals, got %v / %v",
				ptrOrNil(got.OriginalReviewBody), ptrOrNil(got.OriginalReviewEvent))
		}
	})

	t.Run("Get_returns_nil_on_miss", func(t *testing.T) {
		s, orgID, _ := mk(t)
		got, err := s.Get(ctx, orgID, uuid.New().String())
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != nil {
			t.Errorf("Get on missing id should be nil, got %+v", got)
		}
	})

	t.Run("ByRunID_only_returns_locked_reviews", func(t *testing.T) {
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		id := uuid.New().String()
		if err := s.Create(ctx, orgID, domain.PendingReview{
			ID: id, PRNumber: 8, Owner: "o", Repo: "r", CommitSHA: "sha", RunID: runID,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}

		// Fresh review — review_event still empty. ByRunID returns nil.
		got, err := s.ByRunID(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("ByRunID (unlocked): %v", err)
		}
		if got != nil {
			t.Errorf("ByRunID should return nil before review_event is set, got %+v", got)
		}

		// Lock submission — review_event populated. ByRunID returns the row.
		if err := s.LockSubmission(ctx, orgID, id, "body", "COMMENT"); err != nil {
			t.Fatalf("LockSubmission: %v", err)
		}
		got, err = s.ByRunID(ctx, orgID, runID)
		if err != nil || got == nil {
			t.Fatalf("ByRunID (locked): got=%v err=%v", got, err)
		}
		if got.ReviewEvent != "COMMENT" {
			t.Errorf("ReviewEvent = %q, want COMMENT", got.ReviewEvent)
		}
	})

	t.Run("AddComment_snapshots_original_body", func(t *testing.T) {
		s, orgID, _ := mk(t)
		revID := uuid.New().String()
		mustCreateReview(ctx, t, s, orgID, revID)

		commentID := uuid.New().String()
		if err := s.AddComment(ctx, orgID, domain.PendingReviewComment{
			ID: commentID, ReviewID: revID, Path: "foo.go", Line: 10, Body: "agent draft",
		}); err != nil {
			t.Fatalf("AddComment: %v", err)
		}
		got, err := s.ListComments(ctx, orgID, revID)
		if err != nil {
			t.Fatalf("ListComments: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 comment, got %d", len(got))
		}
		if got[0].Body != "agent draft" {
			t.Errorf("Body = %q", got[0].Body)
		}
		if got[0].OriginalBody == nil || *got[0].OriginalBody != "agent draft" {
			t.Errorf("OriginalBody should mirror Body on insert, got %v", ptrOrNil(got[0].OriginalBody))
		}
	})

	t.Run("UpdateComment_preserves_original_body", func(t *testing.T) {
		s, orgID, _ := mk(t)
		revID := uuid.New().String()
		mustCreateReview(ctx, t, s, orgID, revID)
		commentID := uuid.New().String()
		if err := s.AddComment(ctx, orgID, domain.PendingReviewComment{
			ID: commentID, ReviewID: revID, Path: "f.go", Line: 1, Body: "draft",
		}); err != nil {
			t.Fatalf("AddComment: %v", err)
		}
		if err := s.UpdateComment(ctx, orgID, commentID, "user edit"); err != nil {
			t.Fatalf("UpdateComment: %v", err)
		}
		got, _ := s.ListComments(ctx, orgID, revID)
		if got[0].Body != "user edit" {
			t.Errorf("Body = %q, want %q", got[0].Body, "user edit")
		}
		if got[0].OriginalBody == nil || *got[0].OriginalBody != "draft" {
			t.Errorf("OriginalBody should remain %q, got %v", "draft", ptrOrNil(got[0].OriginalBody))
		}
	})

	t.Run("UpdateComment_and_DeleteComment_not_found", func(t *testing.T) {
		s, orgID, _ := mk(t)
		// Both must wrap ErrPendingReviewCommentNotFound so the reviews
		// handler can errors.Is it into a 404 while a real DB/RLS failure
		// falls through to a redacted 500. The substring check stays too:
		// the agent CLI surfaces the message text directly.
		err := s.UpdateComment(ctx, orgID, uuid.New().String(), "anything")
		if !errors.Is(err, db.ErrPendingReviewCommentNotFound) || !strings.Contains(err.Error(), "not found") {
			t.Errorf("UpdateComment missing: err = %v, want wrapped ErrPendingReviewCommentNotFound", err)
		}
		err = s.DeleteComment(ctx, orgID, uuid.New().String())
		if !errors.Is(err, db.ErrPendingReviewCommentNotFound) || !strings.Contains(err.Error(), "not found") {
			t.Errorf("DeleteComment missing: err = %v, want wrapped ErrPendingReviewCommentNotFound", err)
		}
	})

	t.Run("SetSubmission_originals_are_write_once", func(t *testing.T) {
		s, orgID, _ := mk(t)
		revID := uuid.New().String()
		mustCreateReview(ctx, t, s, orgID, revID)

		if err := s.SetSubmission(ctx, orgID, revID, "draft body", "COMMENT"); err != nil {
			t.Fatalf("first SetSubmission: %v", err)
		}
		if err := s.SetSubmission(ctx, orgID, revID, "user edit", "APPROVE"); err != nil {
			t.Fatalf("second SetSubmission: %v", err)
		}

		got, err := s.Get(ctx, orgID, revID)
		if err != nil || got == nil {
			t.Fatalf("Get: %v / %v", got, err)
		}
		if got.ReviewBody != "user edit" || got.ReviewEvent != "APPROVE" {
			t.Errorf("second SetSubmission should overwrite body/event: got body=%q event=%q",
				got.ReviewBody, got.ReviewEvent)
		}
		if got.OriginalReviewBody == nil || *got.OriginalReviewBody != "draft body" {
			t.Errorf("OriginalReviewBody should remain %q, got %v",
				"draft body", ptrOrNil(got.OriginalReviewBody))
		}
		if got.OriginalReviewEvent == nil || *got.OriginalReviewEvent != "COMMENT" {
			t.Errorf("OriginalReviewEvent should remain %q, got %v",
				"COMMENT", ptrOrNil(got.OriginalReviewEvent))
		}
	})

	t.Run("LockSubmission_first_call_captures_then_seals", func(t *testing.T) {
		s, orgID, _ := mk(t)
		revID := uuid.New().String()
		mustCreateReview(ctx, t, s, orgID, revID)

		if err := s.LockSubmission(ctx, orgID, revID, "first body", "COMMENT"); err != nil {
			t.Fatalf("first LockSubmission: %v", err)
		}
		// Second call must hit the gate and return the sentinel.
		err := s.LockSubmission(ctx, orgID, revID, "second body", "APPROVE")
		if !errors.Is(err, db.ErrPendingReviewAlreadySubmitted) {
			t.Errorf("second LockSubmission: err = %v, want ErrPendingReviewAlreadySubmitted", err)
		}

		// The first body / event must still be there — the second call
		// didn't overwrite.
		got, _ := s.Get(ctx, orgID, revID)
		if got.ReviewBody != "first body" || got.ReviewEvent != "COMMENT" {
			t.Errorf("LockSubmission second call should not overwrite: body=%q event=%q",
				got.ReviewBody, got.ReviewEvent)
		}
		if got.OriginalReviewBody == nil || *got.OriginalReviewBody != "first body" {
			t.Errorf("OriginalReviewBody captured = %v", ptrOrNil(got.OriginalReviewBody))
		}
	})

	t.Run("LockSubmission_bogus_id_distinct_from_already_submitted", func(t *testing.T) {
		s, orgID, _ := mk(t)
		err := s.LockSubmission(ctx, orgID, uuid.New().String(), "body", "COMMENT")
		if errors.Is(err, db.ErrPendingReviewAlreadySubmitted) {
			t.Errorf("bogus reviewID should not return ErrPendingReviewAlreadySubmitted, got %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("bogus reviewID: err = %v, want 'not found'", err)
		}
	})

	t.Run("LockSubmission_legacy_SKY204_row_treated_as_submitted", func(t *testing.T) {
		s, orgID, seed := mk(t)
		revID := uuid.New().String()
		mustCreateReview(ctx, t, s, orgID, revID)

		// Land a body + event without going through Lock (mirrors the
		// SKY-204-era path that populated original_review_body via
		// COALESCE but didn't exist yet for original_review_event).
		if err := s.SetSubmission(ctx, orgID, revID, "legacy body", "COMMENT"); err != nil {
			t.Fatalf("SetSubmission: %v", err)
		}
		// Forcibly null out original_review_event the way a migrated
		// legacy row would look.
		seed.SetReviewOriginals(t, revID, nil, nil)
		// Re-stamp body/event via SetSubmission so the row mimics
		// "SKY-204 wrote body + event but originals are NULL".
		if err := s.SetSubmission(ctx, orgID, revID, "legacy body", "COMMENT"); err != nil {
			t.Fatalf("re-set: %v", err)
		}
		// SetSubmission's COALESCE will now repopulate originals — null
		// them out a second time to match the legacy shape exactly.
		seed.SetReviewOriginals(t, revID, nil, nil)

		// Even with original_review_event NULL, LockSubmission must
		// gate on review_event (which is populated) and refuse.
		err := s.LockSubmission(ctx, orgID, revID, "agent retry body", "APPROVE")
		if !errors.Is(err, db.ErrPendingReviewAlreadySubmitted) {
			t.Errorf("legacy row LockSubmission: err = %v, want ErrPendingReviewAlreadySubmitted", err)
		}
	})

	t.Run("Get_preserves_legacy_nil_originals", func(t *testing.T) {
		s, orgID, seed := mk(t)
		revID := uuid.New().String()
		mustCreateReview(ctx, t, s, orgID, revID)
		if err := s.SetSubmission(ctx, orgID, revID, "body", "COMMENT"); err != nil {
			t.Fatalf("SetSubmission: %v", err)
		}
		seed.SetReviewOriginals(t, revID, nil, nil)

		got, _ := s.Get(ctx, orgID, revID)
		if got.OriginalReviewBody != nil || got.OriginalReviewEvent != nil {
			t.Errorf("legacy row Get: originals should be nil, got body=%v event=%v",
				ptrOrNil(got.OriginalReviewBody), ptrOrNil(got.OriginalReviewEvent))
		}
	})

	t.Run("Get_preserves_empty_string_snapshot_as_non_nil", func(t *testing.T) {
		s, orgID, seed := mk(t)
		revID := uuid.New().String()
		mustCreateReview(ctx, t, s, orgID, revID)
		// Snapshot of legitimately empty body via the migration
		// escape hatch — SetSubmission's COALESCE wouldn't capture
		// a "" without first leaving the column NULL.
		empty := ""
		evt := "COMMENT"
		seed.SetReviewOriginals(t, revID, &empty, &evt)

		got, _ := s.Get(ctx, orgID, revID)
		if got.OriginalReviewBody == nil {
			t.Errorf("non-nil pointer to '' should round-trip, got nil")
		} else if *got.OriginalReviewBody != "" {
			t.Errorf("OriginalReviewBody = %q, want ''", *got.OriginalReviewBody)
		}
	})

	t.Run("ListComments_legacy_original_body_is_nil", func(t *testing.T) {
		s, orgID, seed := mk(t)
		revID := uuid.New().String()
		mustCreateReview(ctx, t, s, orgID, revID)
		commentID := uuid.New().String()
		if err := s.AddComment(ctx, orgID, domain.PendingReviewComment{
			ID: commentID, ReviewID: revID, Path: "f.go", Line: 1, Body: "draft",
		}); err != nil {
			t.Fatalf("AddComment: %v", err)
		}
		seed.SetCommentOriginalNull(t, commentID)

		got, _ := s.ListComments(ctx, orgID, revID)
		if len(got) != 1 {
			t.Fatalf("len comments = %d, want 1", len(got))
		}
		if got[0].OriginalBody != nil {
			t.Errorf("legacy comment OriginalBody should be nil, got %v", ptrOrNil(got[0].OriginalBody))
		}
	})

	t.Run("Delete_cascades_to_comments", func(t *testing.T) {
		s, orgID, _ := mk(t)
		revID := uuid.New().String()
		mustCreateReview(ctx, t, s, orgID, revID)
		c1 := uuid.New().String()
		if err := s.AddComment(ctx, orgID, domain.PendingReviewComment{
			ID: c1, ReviewID: revID, Path: "f.go", Line: 1, Body: "x",
		}); err != nil {
			t.Fatalf("AddComment: %v", err)
		}
		if err := s.Delete(ctx, orgID, revID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		got, _ := s.Get(ctx, orgID, revID)
		if got != nil {
			t.Errorf("review survived Delete: %+v", got)
		}
		if s.IsCommentID(ctx, orgID, c1) {
			t.Errorf("comment %s survived parent Delete", c1)
		}
	})

	t.Run("DeleteByRunID_cascades_and_no_op_on_unknown_run", func(t *testing.T) {
		s, orgID, seed := mk(t)
		runID := seed.Run(t)
		revID := uuid.New().String()
		if err := s.Create(ctx, orgID, domain.PendingReview{
			ID: revID, PRNumber: 1, Owner: "o", Repo: "r", CommitSHA: "sha", RunID: runID,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		commentID := uuid.New().String()
		if err := s.AddComment(ctx, orgID, domain.PendingReviewComment{
			ID: commentID, ReviewID: revID, Path: "f.go", Line: 1, Body: "x",
		}); err != nil {
			t.Fatalf("AddComment: %v", err)
		}

		if err := s.DeleteByRunID(ctx, orgID, runID); err != nil {
			t.Fatalf("DeleteByRunID: %v", err)
		}
		if got, _ := s.Get(ctx, orgID, revID); got != nil {
			t.Errorf("review survived DeleteByRunID")
		}
		if s.IsCommentID(ctx, orgID, commentID) {
			t.Errorf("comment survived DeleteByRunID")
		}

		// Unknown run id — no-op, no error.
		if err := s.DeleteByRunID(ctx, orgID, seed.Run(t)); err != nil {
			t.Errorf("DeleteByRunID on run with no review should be no-op, got %v", err)
		}
	})

	t.Run("IsCommentID_distinguishes_known_from_unknown", func(t *testing.T) {
		s, orgID, _ := mk(t)
		revID := uuid.New().String()
		mustCreateReview(ctx, t, s, orgID, revID)
		commentID := uuid.New().String()
		if err := s.AddComment(ctx, orgID, domain.PendingReviewComment{
			ID: commentID, ReviewID: revID, Path: "f.go", Line: 1, Body: "x",
		}); err != nil {
			t.Fatalf("AddComment: %v", err)
		}
		if !s.IsCommentID(ctx, orgID, commentID) {
			t.Errorf("IsCommentID should return true for known comment")
		}
		if s.IsCommentID(ctx, orgID, uuid.New().String()) {
			t.Errorf("IsCommentID should return false for unknown comment")
		}
	})
}

// mustCreateReview is a thin wrapper around ReviewStore.Create that
// fails the test on insert error. Most subtests only care about the
// review existing, not its full field surface, so this collapses the
// boilerplate.
func mustCreateReview(ctx context.Context, t *testing.T, s db.ReviewStore, orgID, reviewID string) {
	t.Helper()
	if err := s.Create(ctx, orgID, domain.PendingReview{
		ID: reviewID, PRNumber: 1, Owner: "o", Repo: "r", CommitSHA: "sha",
		DiffLines: "", DiffHunks: "",
	}); err != nil {
		t.Fatalf("mustCreateReview: %v", err)
	}
}

func ptrOrNil(p *string) any {
	if p == nil {
		return "<nil>"
	}
	return *p
}
