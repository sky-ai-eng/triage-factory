package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// agentHandler serves the agent-run endpoints (status / messages / cancel /
// takeover / release / respond / runs / held-takeovers). spawner is read
// through a getter so the handler always sees the current delegation spawner.
type agentHandler struct {
	tx          db.TxRunner
	ws          *websocket.Hub
	takeoverDir string
	spawner     func() *delegate.Spawner
}

func (ag *agentHandler) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")
	var run *domain.AgentRun
	var resp map[string]any
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		run, e = tx.AgentRuns.Get(r.Context(), orgID, runID)
		if e != nil {
			return e
		}
		if run == nil {
			return nil
		}
		resp = runResponse(r.Context(), tx.Reviews, tx.PendingPRs, orgID, run)
		return nil
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if run == nil {
		notFound(w, "run")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// runResponse projects an AgentRun into the wire shape the frontend
// consumes, augmented with `pending_kind` so the Board page can pick
// the right approval card variant ("Review" or "Open PR") when
// `status == "pending_approval"`. Two side tables can park a run in
// pending_approval (pending_reviews, pending_prs); the discriminator
// has to come from the server because the run row itself doesn't
// know which kind queued it.
//
// Cheap: at most two indexed lookups per call, both no-ops on the
// common case (terminal completed/failed/cancelled with no pending
// row). Both errors swallow + log — pending_kind is informational
// for the UI; an erroring lookup shouldn't fail the whole status
// fetch.
func runResponse(ctx context.Context, reviews db.ReviewStore, pendingPRs db.PendingPRStore, orgID string, run *domain.AgentRun) map[string]any {
	out := map[string]any{
		"ID":                   run.ID,
		"TaskID":               run.TaskID,
		"PromptID":             run.PromptID,
		"Status":               run.Status,
		"Model":                run.Model,
		"StartedAt":            run.StartedAt,
		"CompletedAt":          run.CompletedAt,
		"TotalCostUSD":         run.TotalCostUSD,
		"DurationMs":           run.DurationMs,
		"NumTurns":             run.NumTurns,
		"StopReason":           run.StopReason,
		"WorktreePath":         run.WorktreePath,
		"ResultSummary":        run.ResultSummary,
		"SessionID":            run.SessionID,
		"MemoryMissing":        run.MemoryMissing,
		"TriggerType":          run.TriggerType,
		"TriggerID":            run.TriggerID,
		"blueprint_run_id":     run.BlueprintRunID,
		"blueprint_step_index": run.BlueprintStepIndex,
	}
	// pending_kind only relevant when the run is parked.
	if run.Status == "pending_approval" {
		if review, err := reviews.ByRunID(ctx, orgID, run.ID); err == nil && review != nil {
			out["pending_kind"] = "review"
		} else if pr, err := pendingPRs.ByRunID(ctx, orgID, run.ID); err == nil && pr != nil {
			out["pending_kind"] = "pr"
		}
	}
	return out
}

func (ag *agentHandler) handleAgentMessages(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")
	var messages []domain.AgentMessage
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		messages, e = tx.AgentRuns.Messages(r.Context(), orgID, runID)
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if messages == nil {
		messages = []domain.AgentMessage{}
	}
	writeJSON(w, http.StatusOK, messages)
}

func (ag *agentHandler) handleAgentCancel(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")
	if ag.spawner() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}
	if err := ag.spawner().Cancel(orgID, runID, userID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (ag *agentHandler) handleAgentTakeover(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")
	if ag.spawner() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}

	baseDir, err := delegate.ResolveTakeoverDir(ag.takeoverDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("resolve takeover dir: %v", err)})
		return
	}

	// Note: Takeover does NOT take r.Context(). Once it commits
	// (sets the takenOver flag and SIGKILLs the agent) the operation
	// must run to completion or roll back cleanly; tying it to the
	// request context would let a client disconnect destroy the run.
	result, err := ag.spawner().Takeover(orgID, runID, baseDir, userID)
	if err != nil {
		writeJSON(w, takeoverErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"takeover_path":  result.TakeoverPath,
		"session_id":     result.SessionID,
		"resume_command": fmt.Sprintf("cd %s && claude --resume %s", shellQuote(result.TakeoverPath), shellQuote(result.SessionID)),
	})
}

// shellQuote wraps a path in single quotes for safe shell pasting,
// escaping any embedded single quotes the standard way ('"'"'). Used so
// the resume_command we hand back to the UI is paste-safe even when the
// takeover dir contains spaces or apostrophes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// handleAgentRelease tears down a held takeover (worktree dir, bare
// repo's per-PR config, projects-dir entry) so the next delegated run
// against the same PR can fetch into the branch ref again.
//
// Status mapping:
//   - 200: released, banner row will disappear via WS broadcast
//   - 409: nothing held (wrong status, or already released)
//   - 5xx: filesystem/git/DB failure during teardown — row stays held
//     so a retry can finish the job
func (ag *agentHandler) handleAgentRelease(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")
	if ag.spawner() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}
	if err := ag.spawner().Release(orgID, runID, userID); err != nil {
		writeJSON(w, releaseErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
}

// releaseErrorStatus maps Release() errors to HTTP status codes.
// ErrReleaseNothingHeld is 409 (the precondition shifted under the
// caller — wrong status, or someone already released the row), and
// everything else is 500 since it's filesystem/git/DB.
func releaseErrorStatus(err error) int {
	if errors.Is(err, delegate.ErrReleaseNothingHeld) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// handleHeldTakeovers lists every taken-over run whose takeover
// worktree_path is still recorded in the database. Drives the Board's
// "Held takeovers" banner.
// Released takeovers (status='taken_over' AND empty worktree_path) are
// already filtered out by the underlying query.
//
// Each row carries everything the banner needs to (a) display the
// takeover, (b) re-show the resume command via TakeoverModal, and (c)
// fire the release endpoint by run id. The resume_command is rebuilt
// server-side using the same shellQuote() rule the takeover endpoint
// uses, so the banner's modal renders an identical paste-safe command.
func (ag *agentHandler) handleHeldTakeovers(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var runs []domain.TakenOverRun
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		runs, e = tx.AgentRuns.ListTakenOverForResume(r.Context(), orgID)
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		out = append(out, map[string]any{
			"run_id":         run.RunID,
			"session_id":     run.SessionID,
			"takeover_path":  run.WorktreePath,
			"task_title":     run.TaskTitle,
			"source_id":      run.SourceID,
			"taken_over_at":  run.CompletedAt,
			"resume_command": fmt.Sprintf("cd %s && claude --resume %s", shellQuote(run.WorktreePath), shellQuote(run.SessionID)),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// takeoverErrorStatus maps a Takeover() error to an HTTP status code.
// Validation failures (no session id, no worktree, run not active) are
// 400 — the client asked for something the run state doesn't support.
// Conflicts (already in progress, race-loss) are 409 — the resource
// state shifted in a way the client should re-check. Everything else
// is 500 — filesystem, git subprocess, DB and other internal failures
// are server-side and shouldn't be misclassified as bad client input.
func takeoverErrorStatus(err error) int {
	switch {
	case errors.Is(err, delegate.ErrTakeoverInvalidState):
		return http.StatusBadRequest
	case errors.Is(err, delegate.ErrTakeoverInProgress),
		errors.Is(err, delegate.ErrTakeoverRaceLost):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func (ag *agentHandler) handleAgentRuns(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	taskID := r.URL.Query().Get("task_id")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id query parameter required"})
		return
	}
	var runs []domain.AgentRun
	var out []map[string]any
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		runs, e = tx.AgentRuns.ListForTask(r.Context(), orgID, taskID)
		if e != nil {
			return e
		}
		if runs == nil {
			runs = []domain.AgentRun{}
		}
		// Project each run through runResponse so pending_kind rides
		// alongside on the list endpoint too. Board's useWebSocket calls
		// this on every status transition; without the discriminator on
		// the list response the Open-PR vs Review button choice would
		// flicker on first paint and only settle after the per-run fetch.
		out = make([]map[string]any, len(runs))
		for i := range runs {
			out[i] = runResponse(r.Context(), tx.Reviews, tx.PendingPRs, orgID, &runs[i])
		}
		return nil
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAgentRespond accepts the user's answer to an open yield and
// resumes the run. SKY-139.
//
// Request body shape:
//
//	{
//	  "type": "confirmation"|"choice"|"prompt",
//	  "accepted": bool,            // confirmation
//	  "selected": ["id1","id2"],   // choice
//	  "value": "free text"          // prompt
//	}
//
// Validation:
//   - run exists and is in awaiting_input
//   - response.type matches the open yield_request's type
//   - choice responses with multi=false carry exactly one selected id;
//     multi=true carries 0+ ids drawn from the request's options
//
// Response: 200 with {run_id, status} on success. The actual resume
// runs in a background goroutine; the client refreshes via the
// existing run-update WS broadcast.
func (ag *agentHandler) handleAgentRespond(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")
	if ag.spawner() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}

	var resp domain.YieldResponse
	if !decodeJSON(w, r, &resp, "") {
		return
	}

	var run *domain.AgentRun
	var req *domain.YieldRequest
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		run, e = tx.AgentRuns.Get(r.Context(), orgID, runID)
		if e != nil {
			return e
		}
		if run == nil {
			return nil
		}
		req, e = tx.AgentRuns.LatestYieldRequest(r.Context(), orgID, runID)
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if run == nil {
		notFound(w, "run")
		return
	}
	if run.Status != "awaiting_input" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run is not awaiting input (status=" + run.Status + ")"})
		return
	}
	if req == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no open yield request for this run"})
		return
	}

	if resp.Type != req.Type {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("response type %q does not match request type %q", resp.Type, req.Type)})
		return
	}
	if errMsg := validateYieldResponse(req, &resp); errMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
		return
	}

	// Record the response message before handing off to the spawner.
	// If ResumeAfterYield refuses (concurrent cancel raced us, or the
	// run is no longer resumable for some other reason), the response
	// row stays in the transcript for completeness — the racing path
	// took the run to a terminal state and the message is the
	// historical record of what the user submitted.
	displayContent := domain.RenderYieldResponseForDisplay(req, &resp)
	var msg *domain.AgentMessage
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		msg, e = tx.AgentRuns.InsertYieldResponse(r.Context(), orgID, runID, &resp, displayContent)
		return e
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "record response: " + err.Error()})
		return
	}
	ag.ws.Broadcast(websocket.Event{Type: "agent_message", OrgID: orgID, RunID: runID, Data: msg})

	// Hand off to the spawner. Status flip from awaiting_input to
	// running happens INSIDE ResumeAfterYield, AFTER the cancel
	// handle is registered — that ordering closes the cancel race
	// where a Cancel() arriving between flip and registration would
	// silently mark the run cancelled while the resume goroutine
	// still continues the Claude session.
	agentText := domain.RenderYieldResponseForAgent(req, &resp)
	if err := ag.spawner().ResumeAfterYield(orgID, runID, agentText, userID); err != nil {
		if errors.Is(err, delegate.ErrYieldNotResumable) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "resume: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"run_id": runID, "status": "running"})
}

// validateYieldResponse enforces type-specific shape rules. Returns
// "" on success, an error message otherwise.
func validateYieldResponse(req *domain.YieldRequest, resp *domain.YieldResponse) string {
	switch resp.Type {
	case domain.YieldTypeConfirmation:
		// Require an explicit accepted: a request body of
		// `{"type":"confirmation"}` would otherwise decode to a
		// silent rejection, which we don't want anyone to be able
		// to do by accident. The pointer-typed field lets us tell
		// "missing" apart from "explicit false".
		if resp.Accepted == nil {
			return "confirmation response missing required `accepted` field"
		}
		return ""
	case domain.YieldTypeChoice:
		if !req.Multi && len(resp.Selected) != 1 {
			return fmt.Sprintf("single-choice yield requires exactly one selection, got %d", len(resp.Selected))
		}
		valid := make(map[string]struct{}, len(req.Options))
		for _, o := range req.Options {
			valid[o.ID] = struct{}{}
		}
		for _, id := range resp.Selected {
			if _, ok := valid[id]; !ok {
				return "selected id not in request options: " + id
			}
		}
		return ""
	case domain.YieldTypePrompt:
		// Mirror the frontend's submit-disabled-on-empty behavior:
		// the modal won't let a user submit an empty prompt, but
		// nothing stops a direct API call from doing so. Reject here
		// so the agent never sees an ambiguous "the user submitted
		// an empty response" follow-up.
		if strings.TrimSpace(resp.Value) == "" {
			return "prompt response value cannot be empty"
		}
		return ""
	}
	return "unknown yield type: " + resp.Type
}

// WSHub returns the websocket hub for use by the delegation spawner.
func (s *Server) WSHub() *websocket.Hub {
	return s.ws
}
