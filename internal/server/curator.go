package server

import (
	"database/sql"
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
type curatorHandler struct {
	db      *sql.DB
	tx      db.TxRunner
	ws      *websocket.Hub
	runtime func() *curator.Curator
}

// Curator chat endpoints (SKY-216). Three operations the Projects
// page (SKY-217) needs:
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
	if ch.runtime() == nil {
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
	requestID, err := ch.runtime().SendMessage(r.Context(), projectID, orgID, userID, content)
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

	requests, err := db.ListCuratorRequestsByProject(ch.db, projectID)
	if err != nil {
		internalError(w, "curator", err)
		return
	}

	// Batch the message fetch into one IN-list query keyed by every
	// request id rather than looping per-request — a project with a
	// long chat history would otherwise pay an N+1 round-trip per
	// page load.
	requestIDs := make([]string, len(requests))
	for i, req := range requests {
		requestIDs[i] = req.ID
	}
	messagesByRequest, err := db.ListCuratorMessagesByRequestIDs(ch.db, requestIDs)
	if err != nil {
		internalError(w, "curator", err)
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
	if ch.runtime() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "curator runtime not started"})
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

	inFlight, err := db.InFlightCuratorRequestForProject(ch.db, projectID)
	if err != nil {
		internalError(w, "curator", err)
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
	// MarkCuratorRequestCancelledIfActive makes the second write
	// a no-op.
	ch.runtime().Cancel(projectID)
	if _, err := db.MarkCuratorRequestCancelledIfActive(ch.db, inFlight.ID, "user cancelled"); err != nil {
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

	if err := db.ResetCuratorForProject(ch.db, projectID); err != nil {
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
