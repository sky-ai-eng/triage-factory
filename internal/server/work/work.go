// Package work serves durable work across every registered kind: the
// catalogue of kinds, each kind's depth, its rows, and the operator controls
// over them.
package work

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
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

var workLog = logging.Component("work")

// Handler serves durable work across every registered kind: the
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
type Handler struct {
	az    *authz.Checker
	kinds []db.WorkKindHandle
}

// find returns the registered kind by name, or nil.
func (h *Handler) find(name string) db.WorkKindHandle {
	for _, k := range h.kinds {
		if k.Name() == name {
			return k
		}
	}
	return nil
}

// resolveOrg reads {org_id} from the path, in canonical form, and the
// caller's claims. A malformed org id names nothing and is a 404 before any
// store is touched; no session is a 401. Whether the caller may address the
// org is authorize's question: the routes below run the package's reads and
// controls on the kind's connection with org_id bound by argument, so every
// one of them asks it before reading.
func (h *Handler) resolveOrg(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	parsed, err := uuid.Parse(r.PathValue("org_id"))
	if err != nil {
		httpx.NotFound(w, "org")
		return "", "", false
	}
	claims := httpx.ClaimsFrom(r.Context())
	if claims == nil {
		httpx.WriteUnauth(w)
		return "", "", false
	}
	return parsed.String(), claims.Subject, true
}

// authorize is the per-kind access gate: one arm per declared policy, and a
// default that refuses. A kind declaring a policy this switch does not know
// must not be reachable — a permissive default would turn a new policy into
// an open door — so the default answers 500 naming the policy rather than
// falling through to any gate.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, orgID, userID string, access db.WorkAccess) bool {
	switch access {
	case db.WorkAccessOrgAdmin:
		return h.az.RequireOrgAdminRole(w, r, orgID, userID)
	default:
		httpx.InternalError(w, "work", fmt.Errorf("work kind declares access policy %d, which this handler has no gate for", access))
		return false
	}
}

// resolveKind is the front gate for every kind-addressed route: the org, the
// caller, the kind by its path segment, and the kind's own access policy.
func (h *Handler) resolveKind(w http.ResponseWriter, r *http.Request) (orgID string, k db.WorkKindHandle, ok bool) {
	orgID, userID, ok := h.resolveOrg(w, r)
	if !ok {
		return "", nil, false
	}
	k = h.find(r.PathValue("kind"))
	if k == nil {
		httpx.NotFound(w, "work kind")
		return "", nil, false
	}
	if !h.authorize(w, r, orgID, userID, k.Access()) {
		return "", nil, false
	}
	return orgID, k, true
}

// The catalogue's wire shape: a fixed small set, the shape /sources takes.
type workCatalogueResponse struct {
	Kinds []workKindJSON `json:"kinds" doc:"Every kind of durable work the deployment registers."`
}

type workKindJSON struct {
	Kind      string            `json:"kind" doc:"The kind's name, which the kind's routes take in their path."`
	Label     string            `json:"label" doc:"The kind's name for a person to read."`
	Controls  workControlsJSON  `json:"controls" doc:"Which operator controls the kind offers."`
	Objective workObjectiveJSON `json:"objective" doc:"The kind's monitoring objective."`
}

type workControlsJSON struct {
	Redrive   bool `json:"redrive" doc:"Whether the kind's parked rows can be redriven."`
	Supersede bool `json:"supersede" doc:"Whether a parked row of the kind can be superseded by another row."`
	Cancel    bool `json:"cancel" doc:"Whether cancellation can be requested for the kind's rows."`
}

type workObjectiveJSON struct {
	OldestReadyAgeSeconds int64 `json:"oldest_ready_age_seconds" doc:"How old, in seconds from its first enqueue, the kind's oldest ready row may get before the kind's queue counts as behind."`
}

// The path parameters the work routes share.
const (
	workOrgParam  = "The organization's id."
	workKindParam = "The work kind's name, as the catalogue lists it, such as event_queue."
	workIDParam   = "The row's id, a positive integer."
)

var opWorkCatalogue = httpx.NewJSONOp[httpx.NoBody, workCatalogueResponse](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/work",
	ID:          "get_work_catalogue",
	Summary:     "List the kinds of durable work",
	Description: "Returns every kind of durable work the deployment registers, with the operator controls each kind offers and its monitoring objective.",
	PathParams:  map[string]string{"org_id": workOrgParam},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleCatalogue serves GET /api/orgs/{org_id}/work: every registered kind
// with its controls and objective. The caller passes each distinct access
// policy the registry declares — one today — and, with nothing registered,
// the org-admin gate, so an empty registry still answers the way a populated
// one would to the same caller.
func (h *Handler) handleCatalogue(w http.ResponseWriter, r *http.Request) {
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
	opWorkCatalogue.Write(w, workCatalogueResponse{Kinds: kinds})
}

// workDepthJSON is the depth node: numbers about rows, on their own path.
type workDepthJSON struct {
	Ready                    int   `json:"ready" doc:"How many of the organization's rows of the kind are ready and can be claimed now."`
	Leased                   int   `json:"leased" doc:"How many rows a worker holds."`
	Parked                   int   `json:"parked" doc:"How many rows are parked, which no worker runs again until an operator redrives or supersedes them."`
	Deferred                 int   `json:"deferred" doc:"How many rows are ready but cannot be claimed yet, such as a row whose retry time has not come."`
	OldestReadyAgeSeconds    int64 `json:"oldest_ready_age_seconds" doc:"The age in seconds, from its first enqueue, of the oldest row counted in ready, or 0 when there is none."`
	OldestDeferredAgeSeconds int64 `json:"oldest_deferred_age_seconds" doc:"The age in seconds, from its first enqueue, of the oldest row counted in deferred, or 0 when there is none."`
}

var opWorkDepth = httpx.NewJSONOp[httpx.NoBody, workDepthJSON](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/work/{kind}/depth",
	ID:          "get_work_depth",
	Summary:     "Get a work kind's queue depth",
	Description: "Returns how many of the organization's rows of the kind are ready, leased, parked, and deferred, and the ages of the oldest ready and deferred rows.",
	PathParams:  map[string]string{"org_id": workOrgParam, "kind": workKindParam},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleDepth serves GET /api/orgs/{org_id}/work/{kind}/depth.
func (h *Handler) handleDepth(w http.ResponseWriter, r *http.Request) {
	orgID, k, ok := h.resolveKind(w, r)
	if !ok {
		return
	}
	d, err := workitem.Measure(r.Context(), k.Conn(), k.Kind(), orgID)
	if err != nil {
		httpx.InternalError(w, "work", err)
		return
	}
	opWorkDepth.Write(w, workDepthJSON{
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
	Kind            string           `json:"kind" doc:"The name of the row's kind."`
	ID              int64            `json:"id" doc:"The row's id."`
	Status          string           `json:"status" doc:"The row's status: ready, leased, done, parked, or cancelled."`
	Attempt         int              `json:"attempt" doc:"How many attempts have been made on the row since it was enqueued or last redriven."`
	MaxAttempts     int              `json:"max_attempts" doc:"How many attempts the row may have before it is parked."`
	LastOutcome     string           `json:"last_outcome,omitempty" doc:"The outcome the row last recorded, such as transient, permanent, budget_exhausted, done, or redriven, absent when none is recorded."`
	LastError       string           `json:"last_error,omitempty" doc:"The error the row last recorded, absent when none is recorded."`
	UniqueKey       string           `json:"unique_key,omitempty" doc:"The key that keeps a second row for the same work from being enqueued, absent when the row has none."`
	FirstEnqueuedAt string           `json:"first_enqueued_at" doc:"When the row's work was first enqueued, as an RFC 3339 timestamp, which a redrive does not change."`
	CreatedAt       string           `json:"created_at" doc:"When the row was created, as an RFC 3339 timestamp."`
	DoneAt          string           `json:"done_at,omitempty" doc:"When the row was last done, cancelled, or parked, as an RFC 3339 timestamp, absent when none of those has happened since it was enqueued or last redriven."`
	NextAttemptAt   string           `json:"next_attempt_at,omitempty" doc:"The earliest time the row's next attempt may start, as an RFC 3339 timestamp, absent when no retry time is set."`
	SupersededBy    *int64           `json:"superseded_by,omitempty" doc:"The id of the row that replaced this one, absent unless the row was superseded."`
	Lease           *workLeaseJSON   `json:"lease,omitempty" doc:"The worker's hold on the row, present only while the row is leased."`
	Cancel          *workCancelJSON  `json:"cancel,omitempty" doc:"The cancellation requested for the row, present only when a request is recorded."`
	Subject         *workSubjectJSON `json:"subject,omitempty" doc:"What the row is about, in its kind's words, absent when the kind could not describe the row."`
}

type workLeaseJSON struct {
	Generation int64  `json:"generation" doc:"The lease's generation, a number that rises each time the row is claimed, redriven, or parked."`
	Owner      string `json:"owner" doc:"The id of the Triage Factory process that holds the row."`
	Epoch      *int64 `json:"epoch,omitempty" doc:"The boot count of the process that holds the row, absent when none is recorded."`
	LeasedAt   string `json:"leased_at,omitempty" doc:"When the row was claimed, as an RFC 3339 timestamp, absent when none is recorded."`
	ExpiresAt  string `json:"expires_at,omitempty" doc:"When the lease expires unless its holder renews it, as an RFC 3339 timestamp, absent when none is recorded."`
}

type workCancelJSON struct {
	RequestedAt string `json:"requested_at" doc:"When cancellation was requested, as an RFC 3339 timestamp."`
	By          string `json:"by" doc:"The id of the person who requested it."`
	Reason      string `json:"reason" doc:"The reason the person gave."`
}

type workSubjectJSON struct {
	Label  string            `json:"label" doc:"A short name for what the row is about, such as owner/repo#18 for a pull request or PROJ-123 for a Jira issue."`
	Detail string            `json:"detail,omitempty" doc:"A longer description, such as the pull request's or issue's title, absent when it is unknown."`
	Fields map[string]string `json:"fields" doc:"Further facts about the row by name, such as event_type and entity_id."`
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
	Status string `json:"status" doc:"Only rows with this status: ready, leased, done, parked, cancelled, or deferred, which is the ready rows that cannot be claimed yet; absent lists every status."`
	httpx.PageRequest
}

var opWorkItemsList = httpx.NewListOp[workItemsListRequest, workItemJSON](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/work/{kind}/items/list",
	ID:          "list_work_items",
	Summary:     "List a work kind's rows",
	Description: "Returns one page of the organization's rows of the kind, newest first, optionally narrowed to one status. Each row carries its kind's description of what it is about.",
	PathParams:  map[string]string{"org_id": workOrgParam, "kind": workKindParam},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// workItemsFilter is the canonicalized filter set the page token is minted
// against, so page 2 of one status cannot be requested with page 1's token of
// another.
type workItemsFilter struct {
	Status string `json:"status"`
}

// handleItemsList serves POST /api/orgs/{org_id}/work/{kind}/items/list —
// the kind's rows for the org, newest first. total_count is the org's whole
// population under the filter, which is the number the panel's badge wants.
func (h *Handler) handleItemsList(w http.ResponseWriter, r *http.Request) {
	orgID, k, ok := h.resolveKind(w, r)
	if !ok {
		return
	}
	req, ok := opWorkItemsList.Decode(w, r)
	if !ok {
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
		httpx.InternalError(w, "work", err)
		return
	}
	out, err := describeItems(r, k, orgID, items)
	if err != nil {
		httpx.InternalError(w, "work", err)
		return
	}
	opWorkItemsList.Write(w, page, out, total)
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

var opWorkItem = httpx.NewJSONOp[httpx.NoBody, workItemJSON](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/work/{kind}/items/{id}",
	ID:          "get_work_item",
	Summary:     "Get a work row",
	Description: "Returns one of the organization's rows of the kind, in any status, in the shape the list serves it.",
	PathParams:  map[string]string{"org_id": workOrgParam, "kind": workKindParam, "id": workIDParam},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleItemGet serves GET /api/orgs/{org_id}/work/{kind}/items/{id}: one
// row in any status, in the shape the list serves it.
func (h *Handler) handleItemGet(w http.ResponseWriter, r *http.Request) {
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
		httpx.InternalError(w, "work", err)
		return
	}
	if it == nil {
		httpx.NotFound(w, "work item")
		return
	}
	out, err := describeItems(r, k, orgID, []workitem.Item{*it})
	if err != nil {
		httpx.InternalError(w, "work", err)
		return
	}
	opWorkItem.Write(w, out[0])
}

// workIDsRequest is the redrive body: the row ids an operator selected.
type workIDsRequest struct {
	IDs []int64 `json:"ids" doc:"The ids of the parked rows to redrive, at least one and at most 500."`
}

// workRedriveResponse is the redrive answer: how many rows moved.
type workRedriveResponse struct {
	Redriven int `json:"redriven" doc:"How many of the named rows returned to ready; a row that was no longer parked is not counted."`
}

var opWorkRedrive = httpx.NewJSONOp[workIDsRequest, workRedriveResponse](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/work/{kind}/items/redrive",
	ID:          "redrive_work_items",
	Summary:     "Redrive parked work rows",
	Description: "Returns each named parked row to ready with a fresh attempt budget, and answers how many moved. A named row that is no longer parked is skipped, and a kind whose controls do not include redrive refuses the call.",
	PathParams:  map[string]string{"org_id": workOrgParam, "kind": workKindParam},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
func (h *Handler) handleRedrive(w http.ResponseWriter, r *http.Request) {
	orgID, k, ok := h.resolveKind(w, r)
	if !ok {
		return
	}
	if !k.Controls().Redrive {
		refuseControl(w, k, "redrive")
		return
	}
	req, ok := opWorkRedrive.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	validateIDs(&v, req.IDs)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	by := httpx.ClaimsFrom(r.Context()).Subject
	moved := 0
	for _, id := range req.IDs {
		switch err := workitem.Redrive(r.Context(), k.Conn(), k.Kind(), orgID, id, by); {
		case err == nil:
			moved++
		case errors.Is(err, workitem.ErrNotParked):
		default:
			workLog.ErrorContext(r.Context(), "redrive failed partway",
				"org", orgID, "kind", k.Name(), "requested", len(req.IDs), "moved", moved, "error", err)
			httpx.InternalError(w, "work", err)
			return
		}
	}
	// Worth a line at info: a human deliberately re-driving parked work, and
	// the next thing in the log is that work running.
	workLog.InfoContext(r.Context(), "operator redrove parked work",
		"org", orgID, "kind", k.Name(), "requested", len(req.IDs), "redriven", moved)
	opWorkRedrive.Write(w, workRedriveResponse{Redriven: moved})
}

// workCancelRequest is the cancel body: the ids and the reason the terminal
// record keeps.
type workCancelRequest struct {
	IDs    []int64 `json:"ids" doc:"The ids of the rows to request cancellation for, at least one and at most 500."`
	Reason string  `json:"reason" doc:"Why the rows are being cancelled, which each row records; it is required and is at most 500 characters once surrounding whitespace is trimmed."`
}

// workCancelResponse is the cancel answer: how many requests were recorded.
type workCancelResponse struct {
	Requested int `json:"requested" doc:"How many of the named rows had a cancellation request recorded; a row that has settled or already has a request is not counted."`
}

var opWorkCancel = httpx.NewJSONOp[workCancelRequest, workCancelResponse](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/work/{kind}/items/cancel",
	ID:          "cancel_work_items",
	Summary:     "Request cancellation of work rows",
	Description: "Records a cancellation request on each named row, which takes effect when the row is next claimed or its holder next renews its lease, and answers how many requests were recorded. A named row that has settled or already has a request is skipped, and a kind whose controls do not include cancel refuses the call.",
	PathParams:  map[string]string{"org_id": workOrgParam, "kind": workKindParam},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// maxCancelReasonLen bounds the reason a cancellation records.
const maxCancelReasonLen = 500

// handleCancel serves POST …/items/cancel. A request is recorded, not a
// status change: the row settles at its next claim or its holder's next
// renewal, which is what lets a request land safely against a row somebody
// is mid-way through executing. Ids that are settled or already requested
// are counted out.
func (h *Handler) handleCancel(w http.ResponseWriter, r *http.Request) {
	orgID, k, ok := h.resolveKind(w, r)
	if !ok {
		return
	}
	if !k.Controls().Cancel {
		refuseControl(w, k, "cancel")
		return
	}
	req, ok := opWorkCancel.Decode(w, r)
	if !ok {
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

	by := httpx.ClaimsFrom(r.Context()).Subject
	requested := 0
	for _, id := range req.IDs {
		switch err := workitem.RequestCancel(r.Context(), k.Conn(), k.Kind(), orgID, id, by, reason); {
		case err == nil:
			requested++
		case errors.Is(err, workitem.ErrNotCancellable):
		default:
			workLog.ErrorContext(r.Context(), "cancel request failed partway",
				"org", orgID, "kind", k.Name(), "requested", requested, "error", err)
			httpx.InternalError(w, "work", err)
			return
		}
	}
	workLog.InfoContext(r.Context(), "operator requested cancellation of work",
		"org", orgID, "kind", k.Name(), "named", len(req.IDs), "requested", requested)
	opWorkCancel.Write(w, workCancelResponse{Requested: requested})
}

// workSupersedeRequest is the supersede body: the replacement row.
type workSupersedeRequest struct {
	SupersededBy int64 `json:"superseded_by" doc:"The id of the organization's row of the same kind that replaces this one."`
}

// workSupersedeResponse is the supersede answer: the count, zero or one.
type workSupersedeResponse struct {
	Superseded int `json:"superseded" doc:"1 when the row was settled as superseded, or 0 when it was no longer parked."`
}

var opWorkSupersede = httpx.NewJSONOp[workSupersedeRequest, workSupersedeResponse](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/work/{kind}/items/{id}/supersede",
	ID:          "supersede_work_item",
	Summary:     "Supersede a parked work row",
	Description: "Settles the parked row as cancelled in favour of the named replacement row, which releases the row's unique key, and answers whether it did. A row that is no longer parked is left as it is, and a kind whose controls do not include supersede refuses the call.",
	PathParams:  map[string]string{"org_id": workOrgParam, "kind": workKindParam, "id": workIDParam},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleSupersede serves POST …/items/{id}/supersede: the parked row is
// settled as cancelled in favour of the named replacement, releasing its
// unique key. It ships behind Controls().Supersede so the next adopter does
// not add a route; no registered kind offers it today, so every real kind
// answers 422 here. A row that is not parked is counted out like a redrive
// miss — the answer is the count, zero or one — because a stale selection is
// the normal case for an operator table.
func (h *Handler) handleSupersede(w http.ResponseWriter, r *http.Request) {
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
	req, ok := opWorkSupersede.Decode(w, r)
	if !ok {
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
		httpx.InternalError(w, "work", err)
		return
	}
	if replacement == nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "superseded_by names no row of this kind in this org", Field: "superseded_by",
		})
		return
	}

	by := httpx.ClaimsFrom(r.Context()).Subject
	superseded := 0
	switch err := workitem.Supersede(r.Context(), k.Conn(), k.Kind(), orgID, id, by, req.SupersededBy); {
	case err == nil:
		superseded = 1
	case errors.Is(err, workitem.ErrNotParked):
	default:
		httpx.InternalError(w, "work", err)
		return
	}
	workLog.InfoContext(r.Context(), "operator superseded parked work",
		"org", orgID, "kind", k.Name(), "id", id, "superseded_by", req.SupersededBy, "superseded", superseded)
	opWorkSupersede.Write(w, workSupersedeResponse{Superseded: superseded})
}

// New builds the work handler over the registered kinds.
func New(az *authz.Checker, kinds []db.WorkKindHandle) *Handler {
	return &Handler{az: az, kinds: kinds}
}

// Register mounts the work routes.
func (h *Handler) Register(m httpx.Mounts) {
	// Durable work across every registered kind — the depth of each queue
	// and the operator controls over its rows. Gated per kind's declared
	// access policy inside the handler; every kind runs on the admin pool
	// with org_id bound by argument, so the handler's predicate is the whole
	// enforcement. See work.go.
	m.Op(opWorkCatalogue, h.handleCatalogue)
	m.Op(opWorkDepth, h.handleDepth)
	m.Op(opWorkItemsList, h.handleItemsList)
	m.Op(opWorkItem, h.handleItemGet)
	m.Op(opWorkRedrive, h.handleRedrive)
	m.Op(opWorkCancel, h.handleCancel)
	m.Op(opWorkSupersede, h.handleSupersede)
}
