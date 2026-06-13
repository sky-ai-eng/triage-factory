package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// agentHandler serves the agent-run endpoints (status / messages / cancel /
// message / interrupt / permissions / runs). spawner is read through a
// getter so the handler always sees the current delegation spawner.
type agentHandler struct {
	tx      db.TxRunner
	ws      *websocket.Hub
	spawner func() *delegate.Spawner
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

// runVisible reports whether runID exists and is visible to the caller's org
// (and team, under RLS). The steering control ops resolve a run by id against
// in-memory state (the process registry, the permission broker), so this is the
// authorization gate that stops a known run id from one tenant being acted on by
// another: a non-existent or not-visible run reads as false → the caller 404s.
func (ag *agentHandler) runVisible(ctx context.Context, orgID, userID, runID string) (bool, error) {
	var exists bool
	err := ag.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		run, e := tx.AgentRuns.Get(ctx, orgID, runID)
		if e != nil {
			return e
		}
		exists = run != nil
		return nil
	})
	return exists, err
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
	exists, err := ag.runVisible(r.Context(), orgID, userID, runID)
	if err != nil {
		internalError(w, "agent", err)
		return
	}
	if !exists {
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
// the run's state. An expired workspace (ErrWorkspaceExpired) is 410 Gone: the
// run's saved state was reaped after the retention window, so retrying won't
// help — the client surfaces the clear error rather than a transient conflict.
// Everything else is a server-side 500.
func steerErrorStatus(err error) int {
	switch {
	case errors.Is(err, delegate.ErrWorkspaceExpired):
		return http.StatusGone
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
// "message"?:string,"updated_input"?:object}. The run is authorized under the
// caller's org (RLS) first — like the message/interrupt endpoints — so a run not
// visible to this org is 404; a request that isn't pending (already answered,
// timed out, or never existed) is also 404.
func (ag *agentHandler) handleAgentPermission(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
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

	// Authorize the run under the caller's org before touching the broker — the
	// broker's own org check is a backstop, not a team-level RLS gate.
	exists, err := ag.runVisible(r.Context(), orgID, userID, runID)
	if err != nil {
		internalError(w, "agent", err)
		return
	}
	if !exists {
		notFound(w, "run")
		return
	}

	err = spawner.ResolvePermission(orgID, runID, requestID, agentproc.PermissionDecision{
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
