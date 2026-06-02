package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// pendingPRJSON is the JSON shape the pending-PR overlay consumes.
// Mirrors pendingReviewJSON but for PR queue state.
type pendingPRJSON struct {
	ID          string `json:"id"`
	RunID       string `json:"run_id,omitempty"`
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	HeadBranch  string `json:"head_branch"`
	HeadSHA     string `json:"head_sha"`
	BaseBranch  string `json:"base_branch"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	Draft       bool   `json:"draft"`
	Locked      bool   `json:"locked"`
	SubmittedAt string `json:"submitted_at,omitempty"`
}

// handlePendingPRGet returns a single pending PR.
func (s *Server) handlePendingPRGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var pr *domain.PendingPR
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		pr, e = tx.PendingPRs.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "pending-prs", err)
		return
	}
	if pr == nil {
		notFound(w, "pending PR")
		return
	}

	writeJSON(w, http.StatusOK, pendingPRToJSON(pr))
}

// handleRunPendingPR is the run-keyed lookup the frontend uses to find
// "the pending PR for this delegated run." Mirrors handleRunReview.
func (s *Server) handleRunPendingPR(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")

	var pr *domain.PendingPR
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		pr, e = tx.PendingPRs.ByRunID(r.Context(), orgID, runID)
		return e
	}); err != nil {
		internalError(w, "pending-prs", err)
		return
	}
	if pr == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no pending PR for this run"})
		return
	}

	writeJSON(w, http.StatusOK, pendingPRToJSON(pr))
}

// handlePendingPRUpdate edits the human-visible title and body of a
// queued pending PR. Originals stay frozen via the COALESCE in
// UpdatePendingPRTitleBody so the human-feedback diff at submit time
// has a stable baseline.
func (s *Server) handlePendingPRUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var req struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	var pr *domain.PendingPR
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		pr, e = tx.PendingPRs.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "pending-prs", err)
		return
	}
	if pr == nil {
		notFound(w, "pending PR")
		return
	}

	title := pr.Title
	body := pr.Body
	if req.Title != nil {
		title = *req.Title
	}
	if req.Body != nil {
		body = *req.Body
	}
	// Trim before the empty check: GitHub rejects whitespace-only
	// titles ("Title is required") at submit time anyway, and silently
	// letting "   " through this PATCH means the user only finds out
	// after clicking Open PR. Fail fast and write the trimmed value
	// so we don't carry leading/trailing whitespace into the PR.
	title = strings.TrimSpace(title)
	if title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title cannot be empty or whitespace-only"})
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.PendingPRs.UpdateTitleBody(r.Context(), orgID, id, title, body)
	}); err != nil {
		if errors.Is(err, db.ErrPendingPRSubmitted) {
			// 409 Conflict so the overlay can show the user a clean
			// "submission in flight, your edit was dropped" message
			// instead of a green "saved" toast over a no-op.
			writeJSON(w, http.StatusConflict, map[string]string{"error": "this PR is already being submitted — your edit didn't apply"})
			return
		}
		internalError(w, "pending-prs", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePendingPRDiff returns the unified diff between the queued
// PR's base and head branches, computed against the bare clone we
// already maintain. The `git fetch origin <head>:<head>` first syncs
// the bare's local ref to whatever the agent pushed (the bare
// doesn't auto-update on push). Diff is capped at livePRDiffMaxBytes.
func (s *Server) handlePendingPRDiff(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var pr *domain.PendingPR
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		pr, e = tx.PendingPRs.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "pending-prs", err)
		return
	}
	if pr == nil {
		notFound(w, "pending PR")
		return
	}

	diff, truncationNote, err := livePRDiff(r.Context(), pr.Owner, pr.Repo, pr.BaseBranch, pr.HeadBranch)
	if err != nil {
		// Server-side log with full row context so a 502 shows up
		// in console logs as something diagnosable instead of a
		// silent error. The pre-fix version returned the error in
		// the response body but never logged anything, which made
		// the "no errors in the backend" complaint accurate even
		// when the failure was right there in `err`.
		log.Printf("[pending-prs] diff failed for id=%s run=%s repo=%s/%s base=%s head=%s head_sha=%s: %v",
			pr.ID, pr.RunID, pr.Owner, pr.Repo, pr.BaseBranch, pr.HeadBranch, pr.HeadSHA, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "diff failed: " + err.Error()})
		return
	}

	// X-Diff-Truncated lets the frontend render a banner above the
	// file list when the cap kicked in. Without it parseDiff would
	// silently yield no files for a too-big-to-render diff and the
	// overlay would say "No diff available" — exactly the
	// confusing-when-it-matters case the cap is meant to handle.
	if truncationNote != "" {
		w.Header().Set("X-Diff-Truncated", truncationNote)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(diff))
}

// handlePendingPRSubmit is the user-clicked-Open-PR endpoint:
//
//  1. Concurrent-submit guard (MarkPendingPRSubmitted) — two browser
//     tabs both clicking can't both call CreatePR.
//  2. Build final body with agentmeta footer.
//  3. CreatePR on GitHub. On failure, release the guard so the user
//     can retry; do NOT delete the row.
//  4. On success, capture human verdict in run_memory.human_content,
//     delete the pending row, flip the run to completed, mark the
//     task done, broadcast the WS event.
func (s *Server) handlePendingPRSubmit(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	if s.ghClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub credentials not configured"})
		return
	}

	// Pointer so we can distinguish "client sent draft=false" from
	// "client didn't send draft." A missing field falls back to the
	// row's persisted value (the agent's queue-time --draft hint).
	var req struct {
		Draft *bool `json:"draft"`
	}
	if r.ContentLength > 0 {
		if !decodeJSON(w, r, &req, "") {
			return
		}
	}

	var pr *domain.PendingPR
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		pr, e = tx.PendingPRs.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "pending-prs", err)
		return
	}
	if pr == nil {
		notFound(w, "pending PR")
		return
	}

	// Concurrent-submit guard. Whichever caller wins this UPDATE goes
	// on to call GitHub; the loser sees 409 and can retry once the
	// winner finishes (which will release the guard on failure or
	// delete the row on success).
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.PendingPRs.MarkSubmitted(r.Context(), orgID, id)
	}); err != nil {
		if errors.Is(err, db.ErrPendingPRSubmitInFlight) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "another submit is in flight or has already completed"})
			return
		}
		internalError(w, "pending-prs", err)
		return
	}

	// Build the final body with footer using actual run cost data.
	finalBody := pr.Body + agentmeta.Build(s.agentRuns, orgID, pr.RunID, "PR")

	// Default draft to the row's persisted value (agent's hint). The
	// overlay's checkbox initializes from the same field, so a user
	// who clicks Open PR without touching the checkbox lands on the
	// agent's intent. A user who flipped the checkbox sends an
	// explicit `draft` in the request body.
	draft := pr.Draft
	if req.Draft != nil {
		draft = *req.Draft
	}
	number, htmlURL, err := s.ghClient.CreatePR(pr.Owner, pr.Repo, pr.HeadBranch, pr.BaseBranch, pr.Title, finalBody, draft)
	if err != nil {
		// Release the guard so the user can retry. Pending row stays
		// in place — they may want to edit title/body or push more
		// commits and retry without re-queueing. WithoutCancel keeps
		// the ctx alive past a browser cancel so the guard always
		// gets cleared (otherwise the row sticks in "in flight"
		// state and requires a manual fix). WithTx attaches the
		// request user's claims so the UPDATE passes RLS in
		// multi-mode.
		clearCtx := context.WithoutCancel(r.Context())
		if clearErr := s.tx.WithTx(clearCtx, orgID, userID, func(tx db.TxStores) error {
			return tx.PendingPRs.ClearSubmitted(clearCtx, orgID, id)
		}); clearErr != nil {
			log.Printf("[pending-prs] failed to release submit guard for %s after CreatePR failure: %v", id, clearErr)
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}

	// Post-success cleanup runs detached from r.Context(): the GitHub
	// PR is already open, so a client disconnect must not leave the
	// pending row + run/task in a half-cleaned state.
	//
	// Each logical step gets its OWN small tx wrap so a failure in
	// one doesn't roll back the others. The MarkSubmitted guard set
	// above is in a separate (already-committed) write — bundling the
	// post-success cleanup into a single all-or-nothing tx would
	// leave that guard stuck with the pending row visible on any
	// bookkeeping failure, locking the user out of retry even though
	// GitHub already has the PR.
	cleanupCtx := context.WithoutCancel(r.Context())

	// Step 1: best-effort human verdict capture. The format mirrors
	// the SKY-205 submit-time block so the next retry of this ticket
	// sees what the human changed vs the agent's draft.
	if pr.RunID != "" {
		humanContent := FormatHumanFeedbackPR(pr, pr.Title, pr.Body)
		if err := s.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
			return tx.TaskMemory.UpdateRunMemoryHumanContent(cleanupCtx, orgID, pr.RunID, humanContent)
		}); err != nil {
			log.Printf("[pending-prs] warning: failed to record human verdict for run %s: %v", pr.RunID, err)
		}
	}

	// Step 2: clear the pending PR row. The whole point of this
	// post-success cleanup is to leave the queue consistent with
	// GitHub state — if the delete itself fails, the row stays
	// visible (and the MarkSubmitted guard returns "in-flight" on
	// retry), which is the same failure mode as before SKY-300.
	if err := s.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
		return tx.PendingPRs.Delete(cleanupCtx, orgID, id)
	}); err != nil {
		log.Printf("[pending-prs] warning: failed to clean up pending PR %s after submit: %v", id, err)
	}

	// Step 3: run/task bookkeeping. Independent of the row delete
	// above — a failure here is best-effort logging; the user's
	// GitHub PR exists and the pending row is gone (or kept under
	// step 2's failure path).
	var blueprintRun *domain.BlueprintRun
	if pr.RunID != "" {
		if err := s.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
			if _, err := tx.AgentRuns.MarkCompletedIfPendingApproval(cleanupCtx, orgID, pr.RunID); err != nil {
				return fmt.Errorf("mark run completed: %w", err)
			}
			// Skip the blanket task-mark-done for blueprint steps;
			// terminateBlueprint owns task closure so the blueprint_runs
			// row finalizes first. A blueprint lookup error leaves the
			// task open for human follow-up rather than racing
			// terminateBlueprint.
			cr, _, blueprintLookupErr := tx.Blueprints.GetRunForRun(cleanupCtx, orgID, pr.RunID)
			if blueprintLookupErr != nil {
				log.Printf("[pending-prs] warning: blueprint lookup failed for run %s; skipping task closure: %v", pr.RunID, blueprintLookupErr)
				return nil
			}
			blueprintRun = cr
			if cr != nil {
				return nil
			}
			// Stand-alone run: resolve the run's task_id and flip
			// the task to done.
			run, runErr := tx.AgentRuns.Get(cleanupCtx, orgID, pr.RunID)
			if runErr != nil || run == nil {
				log.Printf("[pending-prs] warning: read run %s for task closure: %v", pr.RunID, runErr)
				return nil
			}
			if err := tx.Tasks.Close(cleanupCtx, orgID, run.TaskID, "run_completed", ""); err != nil {
				log.Printf("[pending-prs] warning: failed to close task for run %s: %v", pr.RunID, err)
			}
			return nil
		}); err != nil {
			log.Printf("[pending-prs] warning: post-submit run bookkeeping for %s: %v", pr.RunID, err)
		}

		s.ws.Broadcast(websocket.Event{
			Type:  "agent_run_update",
			OrgID: orgID,
			RunID: pr.RunID,
			Data:  map[string]string{"status": "completed"},
		})
		if blueprintRun != nil && s.spawner != nil {
			s.spawner.ResumeBlueprintAfterApproval(orgID, pr.RunID, userID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"number":   number,
		"html_url": htmlURL,
	})
}

// pendingPRToJSON projects the domain row into the wire shape.
// Pulled out so both the by-id and by-run handlers stay symmetric.
func pendingPRToJSON(pr *domain.PendingPR) pendingPRJSON {
	out := pendingPRJSON{
		ID:         pr.ID,
		RunID:      pr.RunID,
		Owner:      pr.Owner,
		Repo:       pr.Repo,
		HeadBranch: pr.HeadBranch,
		HeadSHA:    pr.HeadSHA,
		BaseBranch: pr.BaseBranch,
		Title:      pr.Title,
		Body:       pr.Body,
		Draft:      pr.Draft,
		Locked:     pr.Locked,
	}
	if pr.SubmittedAt != nil {
		out.SubmittedAt = pr.SubmittedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return out
}
