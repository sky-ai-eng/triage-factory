package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// workHandler serves durable work across every registered kind: the
// catalogue of kinds, each kind's depth, its rows, and the operator controls
// over them. The event queue is the one registered kind today; the next
// adopter registers a db.WorkKindHandle and inherits every route here.
//
// Why a row here is worth a surface at all: a parked row is work that is
// durably recorded and will never run on its own. Nothing re-drives it, so an
// operator is the only remaining recovery path, and an empty parked set has to
// be distinguishable from a queue that has stopped draining — which is what
// the depth node beside the list is for.
//
// Every route runs on the handle's Conn — the admin pool the kind's worker
// uses — with org_id bound by argument, so there is no RLS backstop: the
// per-kind access switch in authorize is the whole enforcement, and the
// handler test is the authorization test.
type workHandler struct {
	az    *authz.Checker
	kinds []db.WorkKindHandle
}

// find returns the registered kind by name, or nil.
func (h *workHandler) find(name string) db.WorkKindHandle {
	for _, k := range h.kinds {
		if k.Name() == name {
			return k
		}
	}
	return nil
}

// resolveOrg reads {org_id} from the path and the caller's claims. A
// malformed org id names nothing and is a 404 before any store is touched;
// no session is a 401.
//
// Local mode has exactly one org, and its membership gate admits every
// caller to every org id without reading anything, so the sentinel is
// checked here: the routes below run the package's reads and controls on
// the kind's connection with org_id bound by argument, and a well-formed id
// that is not the local org would otherwise answer an empty page or a zero
// count as if the org existed.
func (h *workHandler) resolveOrg(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	orgID = r.PathValue("org_id")
	parsed, err := uuid.Parse(orgID)
	if err != nil {
		notFound(w, "org")
		return "", "", false
	}
	if runmode.Current() == runmode.ModeLocal && parsed != uuid.MustParse(runmode.LocalDefaultOrgID) {
		notFound(w, "org")
		return "", "", false
	}
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return "", "", false
	}
	return orgID, claims.Subject, true
}

// authorize is the per-kind access gate: one arm per declared policy, and a
// default that refuses. A kind declaring a policy this switch does not know
// must not be reachable — a permissive default would turn a new policy into
// an open door — so the default answers 500 naming the policy rather than
// falling through to any gate.
func (h *workHandler) authorize(w http.ResponseWriter, r *http.Request, orgID, userID string, access db.WorkAccess) bool {
	switch access {
	case db.WorkAccessOrgAdmin:
		return h.az.RequireOrgAdminRole(w, r, orgID, userID)
	default:
		internalError(w, "work", fmt.Errorf("work kind declares access policy %d, which this handler has no gate for", access))
		return false
	}
}

// resolveKind is the front gate for every kind-addressed route: the org, the
// caller, the kind by its path segment, and the kind's own access policy.
func (h *workHandler) resolveKind(w http.ResponseWriter, r *http.Request) (orgID string, k db.WorkKindHandle, ok bool) {
	orgID, userID, ok := h.resolveOrg(w, r)
	if !ok {
		return "", nil, false
	}
	k = h.find(r.PathValue("kind"))
	if k == nil {
		notFound(w, "work kind")
		return "", nil, false
	}
	if !h.authorize(w, r, orgID, userID, k.Access()) {
		return "", nil, false
	}
	return orgID, k, true
}

// The catalogue's wire shape: a fixed small set, the shape /sources takes.
type workKindJSON struct {
	Kind      string            `json:"kind"`
	Label     string            `json:"label"`
	Controls  workControlsJSON  `json:"controls"`
	Objective workObjectiveJSON `json:"objective"`
}

type workControlsJSON struct {
	Redrive   bool `json:"redrive"`
	Supersede bool `json:"supersede"`
	Cancel    bool `json:"cancel"`
}

type workObjectiveJSON struct {
	OldestReadyAgeSeconds int64 `json:"oldest_ready_age_seconds"`
}

// handleCatalogue serves GET /api/orgs/{org_id}/work: every registered kind
// with its controls and objective. The caller passes each distinct access
// policy the registry declares — one today — and, with nothing registered,
// the org-admin gate, so an empty registry still answers the way a populated
// one would to the same caller.
func (h *workHandler) handleCatalogue(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveOrg(w, r)
	if !ok {
		return
	}
	policies := map[db.WorkAccess]bool{}
	for _, k := range h.kinds {
		policies[k.Access()] = true
	}
	if len(policies) == 0 {
		policies[db.WorkAccessOrgAdmin] = true
	}
	for access := range policies {
		if !h.authorize(w, r, orgID, userID, access) {
			return
		}
	}

	kinds := make([]workKindJSON, 0, len(h.kinds))
	for _, k := range h.kinds {
		c := k.Controls()
		kinds = append(kinds, workKindJSON{
			Kind:      k.Name(),
			Label:     k.Label(),
			Controls:  workControlsJSON{Redrive: c.Redrive, Supersede: c.Supersede, Cancel: c.Cancel},
			Objective: workObjectiveJSON{OldestReadyAgeSeconds: int64(k.Objective().OldestReadyAge.Seconds())},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"kinds": kinds})
}

// workDepthJSON is the depth node: numbers about rows, on their own path.
type workDepthJSON struct {
	Ready                    int   `json:"ready"`
	Leased                   int   `json:"leased"`
	Parked                   int   `json:"parked"`
	Deferred                 int   `json:"deferred"`
	OldestReadyAgeSeconds    int64 `json:"oldest_ready_age_seconds"`
	OldestDeferredAgeSeconds int64 `json:"oldest_deferred_age_seconds"`
}

// handleDepth serves GET /api/orgs/{org_id}/work/{kind}/depth.
func (h *workHandler) handleDepth(w http.ResponseWriter, r *http.Request) {
	orgID, k, ok := h.resolveKind(w, r)
	if !ok {
		return
	}
	d, err := workitem.Measure(r.Context(), k.Conn(), k.Kind(), orgID)
	if err != nil {
		internalError(w, "work", err)
		return
	}
	writeJSON(w, http.StatusOK, workDepthJSON{
		Ready: d.Ready, Leased: d.Leased, Parked: d.Parked, Deferred: d.Deferred,
		OldestReadyAgeSeconds:    int64(d.OldestReadyAge.Seconds()),
		OldestDeferredAgeSeconds: int64(d.OldestDeferredAge.Seconds()),
	})
}

// workItemJSON is the wire shape of one row: the shared block, plus the
// kind's description of it when the kind could give one. Every timestamp is
// RFC 3339 UTC; nullable fields are omitted when null. lease is present only
// while the row is leased, cancel only when a request is recorded.
type workItemJSON struct {
	Kind            string           `json:"kind"`
	ID              int64            `json:"id"`
	Status          string           `json:"status"`
	Attempt         int              `json:"attempt"`
	MaxAttempts     int              `json:"max_attempts"`
	LastOutcome     string           `json:"last_outcome,omitempty"`
	LastError       string           `json:"last_error,omitempty"`
	UniqueKey       string           `json:"unique_key,omitempty"`
	FirstEnqueuedAt string           `json:"first_enqueued_at"`
	CreatedAt       string           `json:"created_at"`
	DoneAt          string           `json:"done_at,omitempty"`
	NextAttemptAt   string           `json:"next_attempt_at,omitempty"`
	SupersededBy    *int64           `json:"superseded_by,omitempty"`
	Lease           *workLeaseJSON   `json:"lease,omitempty"`
	Cancel          *workCancelJSON  `json:"cancel,omitempty"`
	Subject         *workSubjectJSON `json:"subject,omitempty"`
}

type workLeaseJSON struct {
	Generation int64  `json:"generation"`
	Owner      string `json:"owner"`
	Epoch      *int64 `json:"epoch,omitempty"`
	LeasedAt   string `json:"leased_at,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
}

type workCancelJSON struct {
	RequestedAt string `json:"requested_at"`
	By          string `json:"by"`
	Reason      string `json:"reason"`
}

type workSubjectJSON struct {
	Label  string            `json:"label"`
	Detail string            `json:"detail,omitempty"`
	Fields map[string]string `json:"fields"`
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func rfc3339Ptr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return rfc3339(*t)
}

func workItemToJSON(kind string, it workitem.Item, subject *db.WorkSubject) workItemJSON {
	out := workItemJSON{
		Kind:            kind,
		ID:              it.ID,
		Status:          it.Status,
		Attempt:         it.Attempt,
		MaxAttempts:     it.MaxAttempts,
		LastOutcome:     it.LastOutcome,
		LastError:       it.LastError,
		UniqueKey:       it.UniqueKey,
		FirstEnqueuedAt: rfc3339(it.FirstEnqueuedAt),
		CreatedAt:       rfc3339(it.CreatedAt),
		DoneAt:          rfc3339Ptr(it.DoneAt),
		NextAttemptAt:   rfc3339Ptr(it.NextAttemptAt),
		SupersededBy:    it.SupersededBy,
	}
	if it.Status == workitem.StatusLeased {
		out.Lease = &workLeaseJSON{
			Generation: it.LeaseGeneration,
			Owner:      it.LeaseOwner,
			Epoch:      it.LeaseEpoch,
			LeasedAt:   rfc3339Ptr(it.LeasedAt),
			ExpiresAt:  rfc3339Ptr(it.LeaseExpiresAt),
		}
	}
	if it.CancelRequestedAt != nil {
		out.Cancel = &workCancelJSON{
			RequestedAt: rfc3339(*it.CancelRequestedAt),
			By:          it.CancelRequestedBy,
			Reason:      it.CancelReason,
		}
	}
	if subject != nil {
		fields := subject.Fields
		if fields == nil {
			fields = map[string]string{}
		}
		out.Subject = &workSubjectJSON{Label: subject.Label, Detail: subject.Detail, Fields: fields}
	}
	return out
}

// describe asks the kind for the page's subjects once, and renders the block
// alone for any row it could not describe. A describe failure is a 500 rather
// than a page without subjects: an operator acting on a row needs to know
// what it is, and a silently bare page would hide that the join broke.
func describeItems(r *http.Request, k db.WorkKindHandle, orgID string, items []workitem.Item) ([]workItemJSON, error) {
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	subjects := map[int64]db.WorkSubject{}
	if len(ids) > 0 {
		var err error
		subjects, err = k.Describe(r.Context(), orgID, ids)
		if err != nil {
			return nil, err
		}
	}
	out := make([]workItemJSON, len(items))
	for i, it := range items {
		var subject *db.WorkSubject
		if s, ok := subjects[it.ID]; ok {
			subject = &s
		}
		out[i] = workItemToJSON(k.Name(), it, subject)
	}
	return out, nil
}

// workItemsListRequest is the body of POST …/items/list: an optional status
// and the paging pair. status absent is every status.
type workItemsListRequest struct {
	Status string `json:"status"`
	httpx.PageRequest
}

// workItemsFilter is the canonicalized filter set the page token is minted
// against, so page 2 of one status cannot be requested with page 1's token of
// another.
type workItemsFilter struct {
	Status string `json:"status"`
}

// handleItemsList serves POST /api/orgs/{org_id}/work/{kind}/items/list —
// the kind's rows for the org, newest first. total_count is the org's whole
// population under the filter, which is the number the panel's badge wants.
func (h *workHandler) handleItemsList(w http.ResponseWriter, r *http.Request) {
	orgID, k, ok := h.resolveKind(w, r)
	if !ok {
		return
	}
	var req workItemsListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	if req.Status != "" && !validWorkStatus(req.Status) {
		v.Invalid("status", "status must be one of "+strings.Join(workitem.ListStatuses, ", "))
	}
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(workItemsFilter{Status: req.Status}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	limit := page.Limit
	if page.CountOnly {
		limit = 0
	}
	items, total, err := workitem.List(r.Context(), k.Conn(), k.Kind(), orgID, req.Status, limit, page.Offset)
	if err != nil {
		internalError(w, "work", err)
		return
	}
	out, err := describeItems(r, k, orgID, items)
	if err != nil {
		internalError(w, "work", err)
		return
	}
	httpx.WriteList(w, page, out, total)
}

func validWorkStatus(s string) bool {
	for _, allowed := range workitem.ListStatuses {
		if s == allowed {
			return true
		}
	}
	return false
}

// parseItemID reads {id}. A row id is a positive integer; anything else is
// an INVALID_ID fault before the store, rather than whatever the driver would
// make of it.
func parseItemID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidID, Message: "id must be a positive integer",
		})
		return 0, false
	}
	return id, true
}

// handleItemGet serves GET /api/orgs/{org_id}/work/{kind}/items/{id}: one
// row in any status, in the shape the list serves it.
func (h *workHandler) handleItemGet(w http.ResponseWriter, r *http.Request) {
	orgID, k, ok := h.resolveKind(w, r)
	if !ok {
		return
	}
	id, ok := parseItemID(w, r)
	if !ok {
		return
	}
	it, err := workitem.Get(r.Context(), k.Conn(), k.Kind(), orgID, id)
	if err != nil {
		internalError(w, "work", err)
		return
	}
	if it == nil {
		notFound(w, "work item")
		return
	}
	out, err := describeItems(r, k, orgID, []workitem.Item{*it})
	if err != nil {
		internalError(w, "work", err)
		return
	}
	writeJSON(w, http.StatusOK, out[0])
}

// workIDsRequest is the redrive body: the row ids an operator selected.
type workIDsRequest struct {
	IDs []int64 `json:"ids"`
}

// validateIDs is the shared check on a selection: at least one id, and no
// more than the bound that keeps the per-id loop bounded.
func validateIDs(v *httpx.Validation, ids []int64) {
	if len(ids) == 0 {
		v.Missing("ids")
	}
	if len(ids) > db.MaxRedriveIDs {
		v.OutOfRange("ids", fmt.Sprintf("at most %d ids per call", db.MaxRedriveIDs))
	}
}

// refuseControl answers 422 for a control the kind does not offer. The kind
// is real and the routes beside this one answer for it, so it is not a 404;
// and no role reaches the control, so it is not a 403. No field is named:
// the fault is the kind, not a value in the body.
func refuseControl(w http.ResponseWriter, k db.WorkKindHandle, control string) {
	httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
		Reason:  httpx.ReasonInvalidField,
		Message: k.Label() + " does not offer " + control,
	})
}

// handleRedrive serves POST …/items/redrive: each named parked row returns
// to ready with a fresh budget.
//
// The count returned is rows actually moved. Ids that are no longer parked
// (already redriven, or driven to done by another path since the page loaded)
// are counted out rather than erroring: a stale selection is the normal case
// for a table an operator reads and then acts on, not a client bug.
func (h *workHandler) handleRedrive(w http.ResponseWriter, r *http.Request) {
	orgID, k, ok := h.resolveKind(w, r)
	if !ok {
		return
	}
	if !k.Controls().Redrive {
		refuseControl(w, k, "redrive")
		return
	}
	var req workIDsRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	validateIDs(&v, req.IDs)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	by := ClaimsFrom(r.Context()).Subject
	moved := 0
	for _, id := range req.IDs {
		switch err := workitem.Redrive(r.Context(), k.Conn(), k.Kind(), orgID, id, by); {
		case err == nil:
			moved++
		case errors.Is(err, workitem.ErrNotParked):
		default:
			workLog.ErrorContext(r.Context(), "redrive failed partway",
				"org", orgID, "kind", k.Name(), "requested", len(req.IDs), "moved", moved, "error", err)
			internalError(w, "work", err)
			return
		}
	}
	// Worth a line at info: a human deliberately re-driving parked work, and
	// the next thing in the log is that work running.
	workLog.InfoContext(r.Context(), "operator redrove parked work",
		"org", orgID, "kind", k.Name(), "requested", len(req.IDs), "redriven", moved)
	writeJSON(w, http.StatusOK, map[string]any{"redriven": moved})
}

// workCancelRequest is the cancel body: the ids and the reason the terminal
// record keeps.
type workCancelRequest struct {
	IDs    []int64 `json:"ids"`
	Reason string  `json:"reason"`
}

// maxCancelReasonLen bounds the reason a cancellation records.
const maxCancelReasonLen = 500

// handleCancel serves POST …/items/cancel. A request is recorded, not a
// status change: the row settles at its next claim or its holder's next
// renewal, which is what lets a request land safely against a row somebody
// is mid-way through executing. Ids that are settled or already requested
// are counted out.
func (h *workHandler) handleCancel(w http.ResponseWriter, r *http.Request) {
	orgID, k, ok := h.resolveKind(w, r)
	if !ok {
		return
	}
	if !k.Controls().Cancel {
		refuseControl(w, k, "cancel")
		return
	}
	var req workCancelRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	validateIDs(&v, req.IDs)
	reason := strings.TrimSpace(req.Reason)
	switch {
	case reason == "":
		v.Missing("reason")
	case len(reason) > maxCancelReasonLen:
		v.Invalid("reason", fmt.Sprintf("reason must be at most %d characters", maxCancelReasonLen))
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	by := ClaimsFrom(r.Context()).Subject
	requested := 0
	for _, id := range req.IDs {
		switch err := workitem.RequestCancel(r.Context(), k.Conn(), k.Kind(), orgID, id, by, reason); {
		case err == nil:
			requested++
		case errors.Is(err, workitem.ErrNotCancellable):
		default:
			workLog.ErrorContext(r.Context(), "cancel request failed partway",
				"org", orgID, "kind", k.Name(), "requested", requested, "error", err)
			internalError(w, "work", err)
			return
		}
	}
	workLog.InfoContext(r.Context(), "operator requested cancellation of work",
		"org", orgID, "kind", k.Name(), "named", len(req.IDs), "requested", requested)
	writeJSON(w, http.StatusOK, map[string]any{"requested": requested})
}

// workSupersedeRequest is the supersede body: the replacement row.
type workSupersedeRequest struct {
	SupersededBy int64 `json:"superseded_by"`
}

// handleSupersede serves POST …/items/{id}/supersede: the parked row is
// settled as cancelled in favour of the named replacement, releasing its
// unique key. It ships behind Controls().Supersede so the next adopter does
// not add a route; no registered kind offers it today, so every real kind
// answers 422 here. A row that is not parked is counted out like a redrive
// miss — the answer is the count, zero or one — because a stale selection is
// the normal case for an operator table.
func (h *workHandler) handleSupersede(w http.ResponseWriter, r *http.Request) {
	orgID, k, ok := h.resolveKind(w, r)
	if !ok {
		return
	}
	if !k.Controls().Supersede {
		refuseControl(w, k, "supersede")
		return
	}
	id, ok := parseItemID(w, r)
	if !ok {
		return
	}
	var req workSupersedeRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	switch {
	case req.SupersededBy <= 0:
		v.Invalid("superseded_by", "superseded_by must be a positive row id")
	case req.SupersededBy == id:
		v.Invalid("superseded_by", "a row cannot supersede itself")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	// The replacement must be one of this org's rows of this kind: the column
	// carries no foreign key, so a supersede pointing at nothing would settle
	// the parked row against a replacement that does not exist.
	replacement, err := workitem.Get(r.Context(), k.Conn(), k.Kind(), orgID, req.SupersededBy)
	if err != nil {
		internalError(w, "work", err)
		return
	}
	if replacement == nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "superseded_by names no row of this kind in this org", Field: "superseded_by",
		})
		return
	}

	by := ClaimsFrom(r.Context()).Subject
	superseded := 0
	switch err := workitem.Supersede(r.Context(), k.Conn(), k.Kind(), orgID, id, by, req.SupersededBy); {
	case err == nil:
		superseded = 1
	case errors.Is(err, workitem.ErrNotParked):
	default:
		internalError(w, "work", err)
		return
	}
	workLog.InfoContext(r.Context(), "operator superseded parked work",
		"org", orgID, "kind", k.Name(), "id", id, "superseded_by", req.SupersededBy, "superseded", superseded)
	writeJSON(w, http.StatusOK, map[string]any{"superseded": superseded})
}
