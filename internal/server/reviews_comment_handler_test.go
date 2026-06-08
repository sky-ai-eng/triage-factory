package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedReviewComment creates a pending review plus one comment on the
// local-default org and returns (reviewID, commentID) so the comment
// handler tests can drive PUT/DELETE against a real row.
func seedReviewComment(t *testing.T, s *Server) (string, string) {
	t.Helper()
	ctx := context.Background()
	stores := sqlitestore.New(s.db)
	reviewID := uuid.New().String()
	if err := stores.Reviews.Create(ctx, runmode.LocalDefaultOrgID, domain.PendingReview{
		ID: reviewID, PRNumber: 1, Owner: "o", Repo: "r", CommitSHA: "sha",
	}); err != nil {
		t.Fatalf("seed review: %v", err)
	}
	commentID := uuid.New().String()
	if err := stores.Reviews.AddComment(ctx, runmode.LocalDefaultOrgID, domain.PendingReviewComment{
		ID: commentID, ReviewID: reviewID, Path: "f.go", Line: 1, Body: "draft",
	}); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	return reviewID, commentID
}

// TestReviewCommentUpdate_404VsSuccess pins the documented contract that
// the reviews handler turns a missing comment into a 404 while a real
// edit returns 200. Before the ErrPendingReviewCommentNotFound split a
// missing row and a server error both surfaced as 404 with the raw error
// leaked into the response body.
func TestReviewCommentUpdate_404VsSuccess(t *testing.T) {
	s := newTestServer(t)
	reviewID, commentID := seedReviewComment(t, s)

	rec := doJSON(t, s, "PUT", "/api/reviews/"+reviewID+"/comments/"+commentID, map[string]string{"body": "edited"})
	if rec.Code != http.StatusOK {
		t.Fatalf("update existing comment: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, s, "PUT", "/api/reviews/"+reviewID+"/comments/"+uuid.New().String(), map[string]string{"body": "edited"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update missing comment: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestReviewCommentDelete_404VsSuccess is the delete-side mirror: an
// existing comment deletes with 200, and deleting the now-gone id again
// returns 404 rather than a 500.
func TestReviewCommentDelete_404VsSuccess(t *testing.T) {
	s := newTestServer(t)
	reviewID, commentID := seedReviewComment(t, s)

	rec := doJSON(t, s, "DELETE", "/api/reviews/"+reviewID+"/comments/"+commentID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete existing comment: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, s, "DELETE", "/api/reviews/"+reviewID+"/comments/"+commentID, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete already-removed comment: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
