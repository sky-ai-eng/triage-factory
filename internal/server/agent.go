package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
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
	spawner := ag.spawner()
	if spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}
	if err := spawner.Cancel(orgID, runID, userID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// handleAgentMessage records a free-form user message on a run, broadcasts it
// to watchers, then routes it: a live run is steered in place, an `open` run is
// woken via resume. The run is read in the same tx that records the message, so
// a run not visible to the caller's org reads as 404 (the authz gate before any
// control op) and an existing run's message is recorded before routing — the
// transcript stays optimistic (the user's words show immediately). A run that
// can take no message (terminal / no live process) is 409.
func (ag *agentHandler) handleAgentMessage(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")

	var body struct {
		Text string `json:"text"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		badRequest(w, "text is required")
		return
	}

	spawner := ag.spawner()
	if spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}

	// Read + record in one tx: the Get authorizes against the run under the
	// caller's org (RLS), so a missing / cross-org run is a 404 before SendMessage
	// reaches the registry; an existing run gets the user message persisted (with
	// the row id carried back onto msg.ID for the broadcast's client dedup).
	msg := &domain.AgentMessage{RunID: runID, Role: "user", Subtype: "text", Content: body.Text}
	var runExists bool
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		run, e := tx.AgentRuns.Get(r.Context(), orgID, runID)
		if e != nil {
			return e
		}
		if run == nil {
			return nil
		}
		runExists = true
		id, e := tx.AgentRuns.InsertMessage(r.Context(), orgID, msg)
		if e != nil {
			return e
		}
		msg.ID = int(id)
		return nil
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if !runExists {
		notFound(w, "run")
		return
	}
	ag.ws.Broadcast(websocket.Event{
		Type:  "agent_message",
		OrgID: orgID,
		RunID: runID,
		Data:  msg,
	})

	if err := spawner.SendMessage(r.Context(), orgID, runID, userID, body.Text); err != nil {
		writeJSON(w, steerErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// handleAgentInterrupt stops a live run's current turn, leaving the process
// alive for further input. The run is authorized under the caller's org first
// (404 if not visible) — the process registry is keyed by run id alone, so this
// gate keeps a known run id from interrupting another tenant's run. An existing
// run with no live process is 409 — nothing to interrupt.
func (ag *agentHandler) handleAgentInterrupt(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")
	spawner := ag.spawner()
	if spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}
	var runExists bool
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		run, e := tx.AgentRuns.Get(r.Context(), orgID, runID)
		if e != nil {
			return e
		}
		runExists = run != nil
		return nil
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if !runExists {
		notFound(w, "run")
		return
	}
	if err := spawner.Interrupt(r.Context(), runID); err != nil {
		writeJSON(w, steerErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "interrupted"})
}

// steerErrorStatus maps a SendMessage / Interrupt error to an HTTP status. A
// run that can't take the op right now — no live process (ErrNoLiveProcess),
// not steerable (ErrRunNotSteerable), or a lost resume race
// (ErrRunNotResumable) — is 409 Conflict so the client refreshes and re-reads
// the run's state. Everything else is a server-side 500.
func steerErrorStatus(err error) int {
	switch {
	case errors.Is(err, delegate.ErrNoLiveProcess),
		errors.Is(err, delegate.ErrRunNotSteerable),
		errors.Is(err, delegate.ErrRunNotResumable):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// handleAgentPermission answers a pending tool-permission prompt a run surfaced
// via a `permission_request` WS event. Body: {"behavior":"allow"|"deny",
// "message"?:string,"updated_input"?:object}. A request that isn't pending —
// already answered, timed out, or never existed — is 404.
func (ag *agentHandler) handleAgentPermission(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	runID := r.PathValue("runID")
	requestID := r.PathValue("requestID")

	var body struct {
		Behavior     string         `json:"behavior"`
		Message      string         `json:"message"`
		UpdatedInput map[string]any `json:"updated_input"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}
	if body.Behavior != "allow" && body.Behavior != "deny" {
		badRequest(w, `behavior must be "allow" or "deny"`)
		return
	}

	spawner := ag.spawner()
	if spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}

	err := spawner.ResolvePermission(orgID, runID, requestID, agentproc.PermissionDecision{
		Behavior:     body.Behavior,
		Message:      body.Message,
		UpdatedInput: body.UpdatedInput,
	})
	switch {
	case errors.Is(err, delegate.ErrNoPendingPermission):
		notFound(w, "permission request")
	case err != nil:
		internalError(w, "agent", err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
	}
}

func (ag *agentHandler) handleAgentTakeover(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")
	spawner := ag.spawner()
	if spawner == nil {
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
	result, err := spawner.Takeover(orgID, runID, baseDir, userID)
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
	spawner := ag.spawner()
	if spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}
	if err := spawner.Release(orgID, runID, userID); err != nil {
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

// WSHub returns the websocket hub for use by the delegation spawner.
func (s *Server) WSHub() *websocket.Hub {
	return s.ws
}
