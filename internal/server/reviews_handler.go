package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// reviewsHandler serves the pending-review endpoints. ghResolver picks the
// right GitHub client (org App installation token → PAT) per repo at call
// time, so App-only orgs (no PAT) work identically to PAT orgs. spawner is
// read through a getter so the handler always sees the current value, (re)wired
// onto the server after construction.
type reviewsHandler struct {
	tx         db.TxRunner
	ws         *websocket.Hub
	agentRuns  db.AgentRunStore
	ghResolver ghclient.Resolver
	spawner    func() *delegate.Spawner
}

type pendingReviewJSON struct {
	ID          string                     `json:"id"`
	PRNumber    int                        `json:"pr_number"`
	Owner       string                     `json:"owner"`
	Repo        string                     `json:"repo"`
	CommitSHA   string                     `json:"commit_sha"`
	RunID       string                     `json:"run_id,omitempty"`
	ReviewBody  string                     `json:"review_body"`
	ReviewEvent string                     `json:"review_event"`
	Comments    []pendingReviewCommentJSON `json:"comments"`
}

type pendingReviewCommentJSON struct {
	ID        string `json:"id"`
	ReviewID  string `json:"review_id"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine *int   `json:"start_line,omitempty"`
	Body      string `json:"body"`
	// Severity is the finding level (domain.Severity* or ""); the diff
	// UI renders it as a native chip. The shields.io badge is added only
	// when posting to GitHub (handleReviewSubmit), never stored in Body.
	Severity string `json:"severity,omitempty"`
}

// handleReviewGet returns a pending review and its comments.
func (rh *reviewsHandler) handleReviewGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	reviewID := r.PathValue("id")

	var review *domain.PendingReview
	var comments []domain.PendingReviewComment
	if err := rh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		review, e = tx.Reviews.Get(r.Context(), orgID, reviewID)
		if e != nil {
			return e
		}
		if review == nil {
			return nil
		}
		comments, e = tx.Reviews.ListComments(r.Context(), orgID, reviewID)
		return e
	}); err != nil {
		internalError(w, "reviews", err)
		return
	}
	if review == nil {
		notFound(w, "review")
		return
	}

	result := pendingReviewJSON{
		ID:          review.ID,
		PRNumber:    review.PRNumber,
		Owner:       review.Owner,
		Repo:        review.Repo,
		CommitSHA:   review.CommitSHA,
		RunID:       review.RunID,
		ReviewBody:  review.ReviewBody,
		ReviewEvent: review.ReviewEvent,
		Comments:    make([]pendingReviewCommentJSON, len(comments)),
	}
	for i, c := range comments {
		result.Comments[i] = pendingReviewCommentJSON{
			ID:        c.ID,
			ReviewID:  c.ReviewID,
			Path:      c.Path,
			Line:      c.Line,
			StartLine: c.StartLine,
			Body:      c.Body,
			Severity:  c.Severity,
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// handleReviewSubmit posts a pending review to GitHub, then cleans up local state.
func (rh *reviewsHandler) handleReviewSubmit(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	reviewID := r.PathValue("id")

	var review *domain.PendingReview
	var comments []domain.PendingReviewComment
	if err := rh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		review, e = tx.Reviews.Get(r.Context(), orgID, reviewID)
		if e != nil {
			return e
		}
		if review == nil {
			return nil
		}
		if review.ReviewEvent == "" {
			return nil
		}
		// Load comments (potentially edited by the user)
		comments, e = tx.Reviews.ListComments(r.Context(), orgID, reviewID)
		return e
	}); err != nil {
		internalError(w, "reviews", err)
		return
	}
	if review == nil {
		notFound(w, "review")
		return
	}
	if review.ReviewEvent == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "review has not been submitted by the agent yet"})
		return
	}

	// Resolve the GitHub client for this review's repo (org App installation
	// token → PAT). The review row carries Owner/Repo, so resolve per-repo so a
	// selective App install that doesn't cover this repo falls through to the
	// PAT instead of minting a token that 403s on the submit. Posting as the App
	// bot (App-mode) vs the PAT owner (PAT-mode) is intentional — agent outputs
	// carry the org's bot identity.
	gh, err := rh.ghResolver.ClientForRepo(r.Context(), orgID, review.Owner, review.Repo)
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			log.Printf("[reviews] org %s: GitHub not configured for %s/%s: %v", orgID, review.Owner, review.Repo, err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub credentials not configured"})
			return
		}
		internalError(w, "reviews", err)
		return
	}

	ghComments := make([]ghclient.SubmitReviewComment, len(comments))
	for i, c := range comments {
		ghComments[i] = ghclient.SubmitReviewComment{
			Path:      c.Path,
			Line:      c.Line,
			StartLine: c.StartLine,
			// Prepend the severity badge at GitHub-post time only. The
			// stored body stays badge-free (UI edits don't fight the
			// markdown; the diff UI renders a native chip from Severity).
			Body: domain.SeverityBadgeMarkdown(c.Severity) + c.Body,
		}
	}

	// Build the final review body with header + footer using actual run data
	body := review.ReviewBody + agentmeta.Build(rh.agentRuns, orgID, review.RunID, "review")

	// Submit to GitHub
	ghReviewID, actualEvent, err := gh.SubmitReview(
		review.Owner, review.Repo, review.PRNumber,
		review.CommitSHA, review.ReviewEvent, body, ghComments,
	)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}

	// Persist the human's verdict to run_memory.human_content while
	// the originals are still in scope (DeletePendingReview below
	// removes them). SKY-205. Skipped for non-agent reviews
	// (review.RunID empty — standalone CLI path) since there's no
	// run_memory row to attach to. Logged-not-failed: the GitHub
	// submit already succeeded, so a memory-write failure shouldn't
	// surface as a 5xx and confuse the user about what landed.
	if review.RunID != "" {
		humanContent := FormatHumanFeedback(buildHumanFeedbackInput(review, comments, actualEvent))
		if err := rh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			return tx.TaskMemory.UpdateRunMemoryHumanContent(r.Context(), orgID, review.RunID, humanContent)
		}); err != nil {
			log.Printf("[reviews] warning: failed to record human verdict for run %s: %v", review.RunID, err)
		}
	}

	// Post-submit cleanup runs detached from r.Context(): the GitHub
	// review is already posted, so a client disconnect must not leave
	// the pending row + run/task in a half-cleaned state.
	//
	// Each logical step is its own small tx so a failure in the
	// run/task bookkeeping doesn't roll back the pending_reviews
	// delete. Reviews have no MarkSubmitted-style guard, so a
	// rolled-back delete leaves the row visible and lets the user
	// retry from the UI — which would re-post the same review to
	// GitHub. Splitting the txs keeps the delete durable once GitHub
	// has accepted the submit, regardless of downstream bookkeeping
	// outcomes.
	cleanupCtx := context.WithoutCancel(r.Context())

	// Step 1: clear the pending review. Must commit on its own to
	// prevent a double-submit on retry (no guard equivalent to
	// pending_prs.submitted_at exists on this path).
	if err := rh.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
		return tx.Reviews.Delete(cleanupCtx, orgID, reviewID)
	}); err != nil {
		log.Printf("[reviews] warning: failed to clean up review %s: %v", reviewID, err)
	}

	// Step 2: run/task bookkeeping. Independent of the delete above.
	var blueprintRun *domain.BlueprintRun
	if review.RunID != "" {
		if err := rh.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
			if _, err := tx.AgentRuns.MarkCompletedIfPendingApproval(cleanupCtx, orgID, review.RunID); err != nil {
				return fmt.Errorf("mark run completed: %w", err)
			}
			// Skip the blanket task-mark-done for blueprint steps;
			// terminateBlueprint owns task closure so the blueprint_runs
			// row finalizes first. A blueprint lookup error leaves the
			// task open for human follow-up rather than racing
			// terminateBlueprint.
			cr, _, blueprintLookupErr := tx.Blueprints.GetRunForRun(cleanupCtx, orgID, review.RunID)
			if blueprintLookupErr != nil {
				log.Printf("[reviews] warning: blueprint lookup failed for run %s; skipping task closure: %v", review.RunID, blueprintLookupErr)
				return nil
			}
			blueprintRun = cr
			if cr != nil {
				return nil
			}
			run, runErr := tx.AgentRuns.Get(cleanupCtx, orgID, review.RunID)
			if runErr != nil || run == nil {
				log.Printf("[reviews] warning: read run %s for task closure: %v", review.RunID, runErr)
				return nil
			}
			if err := tx.Tasks.Close(cleanupCtx, orgID, run.TaskID, "run_completed", ""); err != nil {
				log.Printf("[reviews] warning: failed to close task for run %s: %v", review.RunID, err)
			}
			return nil
		}); err != nil {
			log.Printf("[reviews] warning: post-submit run bookkeeping for %s: %v", review.RunID, err)
		}

		rh.ws.Broadcast(websocket.Event{
			Type:  "agent_run_update",
			OrgID: orgID,
			RunID: review.RunID,
			Data:  map[string]string{"status": "completed"},
		})
		spawner := rh.spawner()
		if blueprintRun != nil && spawner != nil {
			spawner.ResumeBlueprintAfterApproval(orgID, review.RunID, userID)
		} else if spawner != nil {
			// Standalone run: approval is its terminal, so drop the durable
			// workspace snapshot taken when it parked in pending_approval. The
			// blueprint mirror of this cleanup lives in terminateBlueprint.
			spawner.DiscardWorkspaceSnapshot(orgID, review.RunID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"github_review_id": ghReviewID,
		"event":            actualEvent,
		"comments_posted":  len(ghComments),
	})
}

// handleRunReview looks up the pending review associated with an agent run.
func (rh *reviewsHandler) handleRunReview(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")

	var review *domain.PendingReview
	var comments []domain.PendingReviewComment
	if err := rh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		review, e = tx.Reviews.ByRunID(r.Context(), orgID, runID)
		if e != nil {
			return e
		}
		if review == nil {
			return nil
		}
		// Delegate to the full review GET which includes comments
		comments, e = tx.Reviews.ListComments(r.Context(), orgID, review.ID)
		return e
	}); err != nil {
		internalError(w, "reviews", err)
		return
	}
	if review == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no pending review for this run"})
		return
	}

	result := pendingReviewJSON{
		ID:          review.ID,
		PRNumber:    review.PRNumber,
		Owner:       review.Owner,
		Repo:        review.Repo,
		CommitSHA:   review.CommitSHA,
		RunID:       review.RunID,
		ReviewBody:  review.ReviewBody,
		ReviewEvent: review.ReviewEvent,
		Comments:    make([]pendingReviewCommentJSON, len(comments)),
	}
	for i, c := range comments {
		result.Comments[i] = pendingReviewCommentJSON{
			ID:        c.ID,
			ReviewID:  c.ReviewID,
			Path:      c.Path,
			Line:      c.Line,
			StartLine: c.StartLine,
			Body:      c.Body,
			Severity:  c.Severity,
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// handleReviewUpdate updates the review body and/or event type.
func (rh *reviewsHandler) handleReviewUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	reviewID := r.PathValue("id")

	var req struct {
		ReviewBody  *string `json:"review_body"`
		ReviewEvent *string `json:"review_event"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	var review *domain.PendingReview
	if err := rh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		review, e = tx.Reviews.Get(r.Context(), orgID, reviewID)
		if e != nil {
			return e
		}
		if review == nil {
			return nil
		}
		body := review.ReviewBody
		event := review.ReviewEvent
		if req.ReviewBody != nil {
			body = *req.ReviewBody
		}
		if req.ReviewEvent != nil {
			event = *req.ReviewEvent
		}
		return tx.Reviews.SetSubmission(r.Context(), orgID, reviewID, body, event)
	}); err != nil {
		internalError(w, "reviews", err)
		return
	}
	if review == nil {
		notFound(w, "review")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReviewCommentUpdate edits the body of a pending review comment.
func (rh *reviewsHandler) handleReviewCommentUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	commentID := r.PathValue("commentId")

	var req struct {
		Body string `json:"body"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required"})
		return
	}

	if err := rh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Reviews.UpdateComment(r.Context(), orgID, commentID, req.Body)
	}); err != nil {
		if errors.Is(err, db.ErrPendingReviewCommentNotFound) {
			notFound(w, "review comment")
			return
		}
		internalError(w, "reviews", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReviewCommentDelete removes a pending review comment.
func (rh *reviewsHandler) handleReviewCommentDelete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	commentID := r.PathValue("commentId")

	if err := rh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Reviews.DeleteComment(r.Context(), orgID, commentID)
	}); err != nil {
		if errors.Is(err, db.ErrPendingReviewCommentNotFound) {
			notFound(w, "review comment")
			return
		}
		internalError(w, "reviews", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReviewDiff proxies the PR diff from GitHub for the review's PR.
func (rh *reviewsHandler) handleReviewDiff(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	reviewID := r.PathValue("id")

	var review *domain.PendingReview
	if err := rh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		review, e = tx.Reviews.Get(r.Context(), orgID, reviewID)
		return e
	}); err != nil {
		internalError(w, "reviews", err)
		return
	}
	if review == nil {
		notFound(w, "review")
		return
	}

	// Resolve per-repo (org App installation token → PAT) after loading the
	// row, which carries Owner/Repo — App-only orgs (no PAT) resolve a client
	// here instead of 503-ing on a nil global client.
	gh, err := rh.ghResolver.ClientForRepo(r.Context(), orgID, review.Owner, review.Repo)
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			log.Printf("[reviews] org %s: GitHub not configured for %s/%s: %v", orgID, review.Owner, review.Repo, err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub credentials not configured"})
			return
		}
		internalError(w, "reviews", err)
		return
	}

	file := r.URL.Query().Get("file")
	diff, err := gh.GetPRDiff(review.Owner, review.Repo, review.PRNumber, file)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(diff))
}
