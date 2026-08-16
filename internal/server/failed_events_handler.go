package server

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// failedEventsHandler serves the parked-event operator surface: the list of
// event_queue rows the router gave up on, and the requeue that puts them
// back. Until this existed the only access was psql.
//
// Why the pair is worth a UI at all: a 'failed' row is an event that is
// durably recorded but whose routing obligations — task mint/bump, owner
// resolution, trigger firing, close processing — exhausted the attempt
// budget and will never run. Nothing re-drives it. The tracker's snapshot
// advanced when the event was minted, so the transition that produced it
// does not re-emit, and the row is retained forever precisely because a
// human is the only remaining recovery path.
//
// Shape mirrors usageHandler's org-ops read (the reference for this
// pattern): an admin-pool store with org_id bound by argument, gated by the
// HTTP org-admin check. EventQueueStore is system-service wired and has no
// TxStores arm, so there is no RLS backstop here — az.RequireOrgAdminRole is
// the whole enforcement, exactly as it is for the fleet/usage-ops reads.
type failedEventsHandler struct {
	az *authz.Checker
	// queue is the admin-pool EventQueueStore. Only the two org-scoped
	// operator methods are called from here; the drain worker owns the rest.
	queue db.EventQueueStore
}

// failedEventJSON is the wire shape of one parked row. Age is left to the
// client to derive from enqueued_at rather than precomputed, so a panel left
// open doesn't display a frozen "3m ago".
type failedEventJSON struct {
	ID             int64  `json:"id"`
	EventType      string `json:"event_type"`
	EntityID       string `json:"entity_id,omitempty"`
	EntitySource   string `json:"entity_source,omitempty"`
	EntitySourceID string `json:"entity_source_id,omitempty"`
	EntityTitle    string `json:"entity_title,omitempty"`
	Attempts       int    `json:"attempts"`
	LastError      string `json:"last_error,omitempty"`
	EnqueuedAt     string `json:"enqueued_at"`
}

// handleFailedEventsList serves GET /api/events/failed — the org's parked
// rows, newest first. ?limit= is strict: the default applies only when the
// param is absent, and a non-integer, non-positive, or over-cap value is
// rejected rather than clamped — a clamp reports a truncated page as if it
// were the requested one.
func (h *failedEventsHandler) handleFailedEventsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.resolveAdmin(w, r)
	if !ok {
		return
	}

	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason: httpx.ReasonInvalidParam, Message: "limit must be a whole number", Field: "limit",
			})
			return
		}
		if n <= 0 || n > db.MaxFailedEventsLimit {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason:  httpx.ReasonOutOfRange,
				Message: fmt.Sprintf("limit must be between 1 and %d", db.MaxFailedEventsLimit),
				Field:   "limit",
			})
			return
		}
		limit = n
	}

	rows, err := h.queue.ListFailedEvents(r.Context(), orgID, limit)
	if err != nil {
		internalError(w, "failed-events", err)
		return
	}

	out := make([]failedEventJSON, len(rows))
	for i, e := range rows {
		out[i] = failedEventJSON{
			ID:             e.ID,
			EventType:      e.EventType,
			EntityID:       e.EntityID,
			EntitySource:   e.EntitySource,
			EntitySourceID: e.EntitySourceID,
			EntityTitle:    e.EntityTitle,
			Attempts:       e.Attempts,
			LastError:      e.LastError,
			EnqueuedAt:     e.EnqueuedAt.UTC().Format(time.RFC3339),
		}
	}
	// count is the length of THIS page, not a total — the panel's badge shows
	// what the operator can act on right now, and the page ceiling is far
	// above any healthy deployment's parked population.
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "count": len(out)})
}

// failedEventsRequeueRequest is the requeue body: the queue ids an operator
// selected. Numbers, not strings — event_queue.id is a bigint identity, the
// same type every other queue method takes.
type failedEventsRequeueRequest struct {
	IDs []int64 `json:"ids"`
}

// handleFailedEventsRequeue serves POST /api/events/failed/requeue.
//
// Requeueing is safe to expose to an operator because it changes nothing
// about how the event is processed: the row re-enters the exact at-least-once
// path every retry already uses. Task creation lands under the tasks dedup
// index, trigger firing behind the (triggering_event_id, trigger_id) replay
// fence and the one-active-run index. So a double-requeue, or a requeue of an
// event whose task has since arrived by other means, converges to no-ops —
// the failure mode is a redundant pass, never a duplicate task or run.
//
// The count returned is rows actually moved. Ids that are no longer 'failed'
// (already requeued, or driven to done by another path since the page loaded)
// are counted out rather than erroring: a stale selection is the normal case
// for a table an operator reads and then acts on, not a client bug.
func (h *failedEventsHandler) handleFailedEventsRequeue(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.resolveAdmin(w, r)
	if !ok {
		return
	}

	var req failedEventsRequeueRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "ids must contain at least one queue id", Field: "ids",
		})
		return
	}
	if len(req.IDs) > db.MaxFailedEventsLimit {
		// A selection can't exceed a page, and the page is capped — so this
		// only rejects a hand-rolled request, and rejecting it keeps the
		// statement's bind list bounded.
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonOutOfRange,
			Message: fmt.Sprintf("at most %d ids per requeue", db.MaxFailedEventsLimit),
			Field:   "ids",
		})
		return
	}

	n, err := h.queue.RequeueFailedEvents(r.Context(), orgID, req.IDs)
	if err != nil {
		internalError(w, "failed-events", err)
		return
	}
	// Worth a line at info: it is a human deliberately re-driving dropped
	// work, and the next thing in the log is that work routing.
	failedEventsLog.InfoContext(r.Context(), "operator requeued parked event rows",
		"org", orgID, "requested", len(req.IDs), "requeued", n)
	writeJSON(w, http.StatusOK, map[string]any{"requeued": n})
}

// resolveAdmin is the shared front gate for both endpoints: an org context, a
// session, and the org-admin role. Local mode (N=1, admin of everything)
// passes RequireOrgAdminRole's short-circuit without a store call; multi mode
// answers 403 for a member of the org who isn't an admin — an honest 403
// rather than a 404, because the caller can plainly see the org this is
// scoped to.
func (h *failedEventsHandler) resolveAdmin(w http.ResponseWriter, r *http.Request) (orgID string, ok bool) {
	orgID, ok = requireOrg(w, r)
	if !ok {
		return "", false
	}
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return "", false
	}
	if !h.az.RequireOrgAdminRole(w, r, orgID, claims.Subject) {
		return "", false
	}
	return orgID, true
}
