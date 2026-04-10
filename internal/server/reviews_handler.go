package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sky-ai-eng/todo-triage/internal/db"
	ghclient "github.com/sky-ai-eng/todo-triage/internal/github"
	"github.com/sky-ai-eng/todo-triage/pkg/websocket"
)

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
}

// handleReviewGet returns a pending review and its comments.
func (s *Server) handleReviewGet(w http.ResponseWriter, r *http.Request) {
	reviewID := r.PathValue("id")

	review, err := db.GetPendingReview(s.db, reviewID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if review == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "review not found"})
		return
	}

	comments, err := db.ListPendingReviewComments(s.db, reviewID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// handleReviewSubmit posts a pending review to GitHub, then cleans up local state.
func (s *Server) handleReviewSubmit(w http.ResponseWriter, r *http.Request) {
	reviewID := r.PathValue("id")

	if s.ghClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub credentials not configured"})
		return
	}

	review, err := db.GetPendingReview(s.db, reviewID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if review == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "review not found"})
		return
	}
	if review.ReviewEvent == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "review has not been submitted by the agent yet"})
		return
	}

	// Load comments (potentially edited by the user)
	comments, err := db.ListPendingReviewComments(s.db, reviewID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	ghComments := make([]ghclient.SubmitReviewComment, len(comments))
	for i, c := range comments {
		ghComments[i] = ghclient.SubmitReviewComment{
			Path:      c.Path,
			Line:      c.Line,
			StartLine: c.StartLine,
			Body:      c.Body,
		}
	}

	// Build the final review body with header + footer using actual run data
	body := buildFinalReviewBody(s.db, review.RunID, review.ReviewBody)

	// Submit to GitHub
	ghReviewID, actualEvent, err := s.ghClient.SubmitReview(
		review.Owner, review.Repo, review.PRNumber,
		review.CommitSHA, review.ReviewEvent, body, ghComments,
	)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}

	// Clean up local state
	if err := db.DeletePendingReview(s.db, reviewID); err != nil {
		log.Printf("[reviews] warning: failed to clean up review %s: %v", reviewID, err)
	}

	// If this review was associated with an agent run, update the run status
	if review.RunID != "" {
		if _, err := s.db.Exec(`UPDATE agent_runs SET status = 'completed' WHERE id = ? AND status = 'pending_approval'`, review.RunID); err != nil {
			log.Printf("[reviews] warning: failed to update run %s status: %v", review.RunID, err)
		}
		// Also mark the task as done
		if _, err := s.db.Exec(`UPDATE tasks SET status = 'done' WHERE id = (SELECT task_id FROM agent_runs WHERE id = ?)`, review.RunID); err != nil {
			log.Printf("[reviews] warning: failed to update task status for run %s: %v", review.RunID, err)
		}
		s.ws.Broadcast(websocket.Event{
			Type:  "agent_run_update",
			RunID: review.RunID,
			Data:  map[string]string{"status": "completed"},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"github_review_id": ghReviewID,
		"event":            actualEvent,
		"comments_posted":  len(ghComments),
	})
}

// handleRunReview looks up the pending review associated with an agent run.
func (s *Server) handleRunReview(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")

	review, err := db.PendingReviewByRunID(s.db, runID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if review == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no pending review for this run"})
		return
	}

	// Delegate to the full review GET which includes comments
	comments, err := db.ListPendingReviewComments(s.db, review.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// handleReviewUpdate updates the review body and/or event type.
func (s *Server) handleReviewUpdate(w http.ResponseWriter, r *http.Request) {
	reviewID := r.PathValue("id")

	var req struct {
		ReviewBody  *string `json:"review_body"`
		ReviewEvent *string `json:"review_event"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	review, err := db.GetPendingReview(s.db, reviewID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if review == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "review not found"})
		return
	}

	body := review.ReviewBody
	event := review.ReviewEvent
	if req.ReviewBody != nil {
		body = *req.ReviewBody
	}
	if req.ReviewEvent != nil {
		event = *req.ReviewEvent
	}

	if err := db.SetPendingReviewSubmission(s.db, reviewID, body, event); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReviewCommentUpdate edits the body of a pending review comment.
func (s *Server) handleReviewCommentUpdate(w http.ResponseWriter, r *http.Request) {
	commentID := r.PathValue("commentId")

	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required"})
		return
	}

	if err := db.UpdatePendingReviewComment(s.db, commentID, req.Body); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReviewCommentDelete removes a pending review comment.
func (s *Server) handleReviewCommentDelete(w http.ResponseWriter, r *http.Request) {
	commentID := r.PathValue("commentId")

	if err := db.DeletePendingReviewComment(s.db, commentID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReviewDiff proxies the PR diff from GitHub for the review's PR.
func (s *Server) handleReviewDiff(w http.ResponseWriter, r *http.Request) {
	reviewID := r.PathValue("id")

	if s.ghClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub credentials not configured"})
		return
	}

	review, err := db.GetPendingReview(s.db, reviewID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if review == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "review not found"})
		return
	}

	file := r.URL.Query().Get("file")
	diff, err := s.ghClient.GetPRDiff(review.Owner, review.Repo, review.PRNumber, file)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(diff))
}

// buildFinalReviewBody wraps the agent's raw review body with a header and
// a metadata footer using actual run data (real cost, not estimated).
func buildFinalReviewBody(database *sql.DB, runID, rawBody string) string {
	if runID == "" {
		return rawBody + "\n\n---\n*This review was partially generated by AI.*"
	}

	run, err := db.GetAgentRun(database, runID)
	if err != nil || run == nil {
		return rawBody + "\n\n---\n*This review was partially generated by AI.*"
	}

	// Use actual cost from the completed run
	cost := 0.0
	if run.TotalCostUSD != nil {
		cost = *run.TotalCostUSD
	}

	// Pretty-print model name
	model := run.Model
	switch {
	case strings.Contains(model, "opus"):
		model = "Claude Opus"
	case strings.Contains(model, "sonnet"):
		model = "Claude Sonnet"
	case strings.Contains(model, "haiku"):
		model = "Claude Haiku"
	}

	// Duration from the completed run
	elapsed := "?"
	if run.DurationMs != nil {
		elapsed = prettyDuration(*run.DurationMs)
	} else if run.CompletedAt != nil {
		elapsed = prettyDuration(int(run.CompletedAt.Sub(run.StartedAt).Milliseconds()))
	}

	footer := fmt.Sprintf("\n\n---\n*This review was partially generated by AI.* Time: %s | Model: %s | Cost: $%.3f", elapsed, model, cost)

	return rawBody + footer
}

func prettyDuration(ms int) string {
	d := time.Duration(ms) * time.Millisecond
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	s = s % 60
	if m < 60 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := m / 60
	m = m % 60
	return fmt.Sprintf("%dh %dm", h, m)
}
