package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// curatorHandler serves the per-project curator chat endpoints. runtime is read
// through a getter so the handler always sees the current curator runtime,
// which is wired onto the server after construction.
//
// All DB access goes through tx (WithTx → tx.Curator), never a raw
// *sql.DB: in multi mode the raw handle is the ADMIN pool, and the
// curator RLS policies are deliberately self-only
// (creator_user_id = tf.current_user_id()) — history/cancel/reset act
// on the requesting user's own rows (TFAC-109).
type curatorHandler struct {
	tx      db.TxRunner
	ws      *websocket.Hub
	runtime func() *curator.Curator
}

// Curator chat endpoints. Three operations the Projects
// page needs:
//
//   - POST .../messages   queue a user turn → 202 + {request_id}
//   - GET  .../messages   chat history (requests + their messages)
//   - DELETE .../in-flight cancel the active turn → 204
//
// All three are scoped to a single project. The runtime in
// internal/curator owns concurrency: cross-project messages run in
// parallel; same-project messages queue serially behind one CC
// subprocess at a time.

type curatorSendRequest struct {
	Content string `json:"content"`
}

type curatorSendResponse struct {
	RequestID string `json:"request_id"`
}

// curatorRequestJSON is the wire shape for a Curator chat turn. Combines
// the request row (status + accounting) with its agent-side message
// stream so the frontend can render the entire conversation in one
// query rather than fetching messages per-request.
type curatorRequestJSON struct {
	domain.CuratorRequest
	Messages []domain.CuratorMessage `json:"messages"`
}

func (ch *curatorHandler) handleCuratorSend(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	cur := ch.runtime()
	if cur == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "curator runtime not started"})
		return
	}
	projectID := r.PathValue("id")
	var project *domain.Project
	if err := ch.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, projectID)
		return e
	}); err != nil {
		internalError(w, "curator", err)
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}

	var req curatorSendRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content is required"})
		return
	}

	// Pass the requesting user's identity explicitly so the per-project
	// goroutine attributes every per-turn write accordingly. Both org
	// and user come from the request context — sentinel in local mode
	// via the shim, real values in multi mode.
	requestID, err := cur.SendMessage(r.Context(), projectID, orgID, userID, content)
	if err != nil {
		internalError(w, "curator", err)
		return
	}
	writeJSON(w, http.StatusAccepted, curatorSendResponse{RequestID: requestID})
}

func (ch *curatorHandler) handleCuratorHistory(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	projectID := r.PathValue("id")
	var (
		project           *domain.Project
		requests          []domain.CuratorRequest
		messagesByRequest map[string][]domain.CuratorMessage
	)
	if err := ch.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, projectID)
		if e != nil || project == nil {
			return e
		}
		requests, e = tx.Curator.ListRequestsByProject(r.Context(), orgID, projectID)
		if e != nil {
			return e
		}
		// Batch the message fetch into one IN-list query keyed by every
		// request id rather than looping per-request — a project with a
		// long chat history would otherwise pay an N+1 round-trip per
		// page load.
		requestIDs := make([]string, len(requests))
		for i, req := range requests {
			requestIDs[i] = req.ID
		}
		messagesByRequest, e = tx.Curator.ListMessagesByRequestIDs(r.Context(), orgID, requestIDs)
		return e
	}); err != nil {
		internalError(w, "curator", err)
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}

	out := make([]curatorRequestJSON, 0, len(requests))
	for _, req := range requests {
		messages := messagesByRequest[req.ID]
		if messages == nil {
			messages = []domain.CuratorMessage{}
		}
		out = append(out, curatorRequestJSON{
			CuratorRequest: req,
			Messages:       messages,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCuratorCancel terminates the active in-flight turn. Returns
// 404 if there's no queued or running request for the project — the
// frontend uses that to clear stale "cancelling…" state when the
// agent finished between the user's click and the request landing.
func (ch *curatorHandler) handleCuratorCancel(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	cur := ch.runtime()
	if cur == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "curator runtime not started"})
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	projectID := r.PathValue("id")
	var (
		project  *domain.Project
		inFlight *domain.CuratorRequest
	)
	if err := ch.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, projectID)
		if e != nil || project == nil {
			return e
		}
		// Self-only under Postgres RLS: a user locates (and cancels)
		// their own in-flight turn, not a teammate's.
		inFlight, e = tx.Curator.InFlightRequestForProject(r.Context(), orgID, projectID)
		return e
	}); err != nil {
		internalError(w, "curator", err)
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}
	if inFlight == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no in-flight curator request"})
		return
	}

	// Two-step: tell the runtime to fire the cancel ctx (kills the
	// CC subprocess if one is running), then flip the row at the
	// DB level so a queued (not-yet-running) request is also handled.
	// The runtime's session goroutine writes the same terminal
	// status when it observes ctx.Err(); the status filter in
	// MarkRequestCancelledIfActive makes the second write a no-op.
	cur.Cancel(projectID)
	if err := ch.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		_, e := tx.Curator.MarkRequestCancelledIfActive(r.Context(), orgID, inFlight.ID, "user cancelled")
		return e
	}); err != nil {
		internalError(w, "curator", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCuratorReset wipes a project's curator session so the next
// message starts a brand-new Claude Code session. Useful when the
// allowlist or the envelope template change — `--resume` binds those
// flags to the original session, so existing sessions don't pick up
// new permissions until reset. Also handy for nuking a confused
// conversation without deleting the whole project.
//
// 409 with a clear hint if there's an in-flight turn — caller should
// cancel first. The DB op + the WS broadcast are decoupled because a
// failed broadcast (e.g. hub panicked) shouldn't roll back the wipe.
func (ch *curatorHandler) handleCuratorReset(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	projectID := r.PathValue("id")
	var project *domain.Project
	if err := ch.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, projectID)
		return e
	}); err != nil {
		internalError(w, "curator", err)
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}

	if err := ch.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Curator.ResetForProject(r.Context(), orgID, projectID)
	}); err != nil {
		if errors.Is(err, db.ErrCuratorInFlight) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "in-flight curator request — cancel it before resetting",
			})
			return
		}
		internalError(w, "curator", err)
		return
	}

	if ch.ws != nil {
		ch.ws.Broadcast(websocket.Event{
			Type:      "curator_reset",
			OrgID:     orgID,
			ProjectID: projectID,
		})
	}
	w.WriteHeader(http.StatusNoContent)
}
