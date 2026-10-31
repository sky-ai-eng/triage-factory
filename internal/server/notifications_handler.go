package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// The notifications API (specs/durable-notifications.md, HTTP API). The inbox
// is viewer-relative, so its routes live under /api/me and answer for the org
// the credential carries; the org admin's "All teams" view names its org in
// the path. Every row is read under the caller's claims, where RLS is the "may
// read" predicate, and the inbox list narrows that to "is sent".

// notificationAudienceJSON is who a row is for. Each id is null unless the
// scope names one, and role is null on a user row.
type notificationAudienceJSON struct {
	Scope  domain.NotificationScope `json:"scope"`
	Role   *domain.NotificationRole `json:"role"`
	TeamID *string                  `json:"team_id"`
	UserID *string                  `json:"user_id"`
}

// notificationJSON is the wire item: the row's display columns, its actions
// as this viewer may use them, and this viewer's marks. The key and params are
// ids for the system's use and never leave the server.
type notificationJSON struct {
	ID             string                             `json:"id"`
	Kind           string                             `json:"kind"`
	Lifecycle      domain.NotificationLifecycle       `json:"lifecycle"`
	Priority       domain.NotificationPriority        `json:"priority"`
	Audience       notificationAudienceJSON           `json:"audience"`
	Title          string                             `json:"title"`
	Body           string                             `json:"body"`
	Dismissible    bool                               `json:"dismissible"`
	FirstSeenAt    time.Time                          `json:"first_seen_at"`
	LastSeenAt     time.Time                          `json:"last_seen_at"`
	SeenCount      int                                `json:"seen_count"`
	ResolvedAt     *time.Time                         `json:"resolved_at"`
	ResolvedReason *domain.NotificationResolvedReason `json:"resolved_reason"`
	ExpiresAt      *time.Time                         `json:"expires_at"`
	Actions        []notify.ActionView                `json:"actions"`
	Read           bool                               `json:"read"`
	Dismissed      bool                               `json:"dismissed"`
}

// notificationItems renders views for the caller. Call it outside any
// transaction: an action's allowed asks the caller's permissions, and local
// mode's database has one connection.
func notificationItems(r *http.Request, caller *notify.Caller, views []domain.NotificationView) ([]notificationJSON, error) {
	out := make([]notificationJSON, len(views))
	for i, v := range views {
		actions, err := caller.Actions(r.Context(), v.Notification)
		if err != nil {
			return nil, fmt.Errorf("actions for notification %s: %w", v.ID, err)
		}
		a := v.Audience
		out[i] = notificationJSON{
			ID:        v.ID,
			Kind:      v.Kind,
			Lifecycle: v.Lifecycle,
			Priority:  v.Priority,
			Audience: notificationAudienceJSON{
				Scope: a.Scope, Role: nilIfEmpty(a.Role), TeamID: nilIfEmpty(a.TeamID), UserID: nilIfEmpty(a.UserID),
			},
			Title:          v.Title,
			Body:           v.Body,
			Dismissible:    v.Dismissible,
			FirstSeenAt:    v.FirstSeenAt,
			LastSeenAt:     v.LastSeenAt,
			SeenCount:      v.SeenCount,
			ResolvedAt:     v.ResolvedAt,
			ResolvedReason: nilIfEmpty(v.ResolvedReason),
			ExpiresAt:      v.ExpiresAt,
			Actions:        actions,
			Read:           v.Read,
			Dismissed:      v.Dismissed,
		}
	}
	return out, nil
}

// writeNotificationItem renders one view for the caller as a 200.
func writeNotificationItem(w http.ResponseWriter, r *http.Request, caller *notify.Caller, v domain.NotificationView) {
	items, err := notificationItems(r, caller, []domain.NotificationView{v})
	if err != nil {
		internalError(w, "notifications", err)
		return
	}
	writeJSON(w, http.StatusOK, items[0])
}

// notificationCaller resolves the /api/me routes' subject: the org the
// credential carries and the user it names.
func notificationCaller(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	orgID, ok = requireOrg(w, r)
	if !ok {
		return "", "", false
	}
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return "", "", false
	}
	return orgID, claims.Subject, true
}

// notificationIDOr404 reads {id} in its canonical spelling, so both dialects
// match the same row for it; a malformed id names nothing.
func notificationIDOr404(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		notFound(w, "notification")
		return "", false
	}
	return id.String(), true
}

// pingMarksChanged tells the caller's other tabs that their marks changed. It
// carries no payload and is addressed to the caller alone, since marks are
// per person.
func (s *Server) pingMarksChanged(orgID, userID string) {
	s.ws.Broadcast(websocket.Event{Type: notify.EventType, OrgID: orgID, UserID: userID})
}

// --------------------------------------------------------------------
// lists
// --------------------------------------------------------------------

// notificationListRequest is the body of POST /api/me/notifications/list. An
// absent field does not filter. A present one filters by exactly its value,
// so an empty list or an empty string is refused rather than read as "no
// filter", which would answer a wider question than the caller asked.
//
// The filters are spelled out here and in notificationOrgListRequest rather
// than embedded from one struct: encoding/json names a type fault inside an
// embedded struct by the struct's Go name, which would reach the error's
// field.
// TODO(TFAC-1255): embed one shared filter struct once the decode error
// names the JSON field.
type notificationListRequest struct {
	Status     *string  `json:"status"`
	Dismissed  *bool    `json:"dismissed"`
	Unread     *bool    `json:"unread"`
	PriorityIn []string `json:"priority_in"`
	ScopeIn    []string `json:"scope_in"`

	httpx.PageRequest
}

// notificationOrgListRequest is the body of POST
// /api/orgs/{org_id}/notifications/list: the inbox's filters plus the teams
// whose rows to show.
type notificationOrgListRequest struct {
	Status     *string  `json:"status"`
	Dismissed  *bool    `json:"dismissed"`
	Unread     *bool    `json:"unread"`
	PriorityIn []string `json:"priority_in"`
	ScopeIn    []string `json:"scope_in"`
	TeamIDIn   []string `json:"team_id_in"`

	httpx.PageRequest
}

// notificationListFilterKey is the canonicalized filter set a page token is
// fingerprinted against. It names the list and the org too, since the inbox
// and the All-teams list share their filters but not their rows.
type notificationListFilterKey struct {
	List       string                        `json:"list"`
	OrgID      string                        `json:"org_id"`
	Status     domain.NotificationStatus     `json:"status"`
	Dismissed  *bool                         `json:"dismissed"`
	Unread     *bool                         `json:"unread"`
	PriorityIn []domain.NotificationPriority `json:"priority_in"`
	ScopeIn    []domain.NotificationScope    `json:"scope_in"`
	TeamIDIn   []string                      `json:"team_id_in"`
}

// parseNotificationFilter validates the filters both lists share.
func parseNotificationFilter(v *httpx.Validation, status *string, dismissed, unread *bool, priorityIn, scopeIn []string) db.NotificationListFilter {
	f := db.NotificationListFilter{Dismissed: dismissed, Unread: unread}
	if status != nil {
		switch st := domain.NotificationStatus(*status); st {
		case domain.NotificationOpen, domain.NotificationResolved:
			f.Status = st
		default:
			v.Invalid("status", fmt.Sprintf("status must be %q or %q; leave it out to match both", domain.NotificationOpen, domain.NotificationResolved))
		}
	}
	f.PriorityIn = parseNotificationEnums(v, "priority_in", priorityIn, domain.AllNotificationPriorities())
	f.ScopeIn = parseNotificationEnums(v, "scope_in", scopeIn, domain.AllNotificationScopes())
	return f
}

// parseNotificationEnums reads an _in filter over a closed vocabulary, sorted
// and deduplicated so two spellings of one set are one query. It reports the
// first unknown value only, so the response does not grow with the body.
func parseNotificationEnums[T ~string](v *httpx.Validation, field string, raw []string, vocabulary []T) []T {
	if raw == nil {
		return nil
	}
	if len(raw) == 0 {
		v.Invalid(field, field+" must name at least one value; leave it out to match every value")
		return nil
	}
	names := make([]string, len(vocabulary))
	for i, x := range vocabulary {
		names[i] = string(x)
	}
	var out []T
	for _, s := range raw {
		if !slices.Contains(vocabulary, T(s)) {
			v.Invalid(field, fmt.Sprintf("%s: %q is not one of %s", field, s, strings.Join(names, ", ")))
			return nil
		}
		out = append(out, T(s))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// handleNotificationList is the caller's inbox: the rows they are sent, which
// for an org admin excludes the teams' rows they may read but are not sent.
// page_size 0 is the badge's count-only read.
//
// POST /api/me/notifications/list
func (s *Server) handleNotificationList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := notificationCaller(w, r)
	if !ok {
		return
	}
	var req notificationListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	f := parseNotificationFilter(&v, req.Status, req.Dismissed, req.Unread, req.PriorityIn, req.ScopeIn)
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(notificationListFilterKey{
		List: "inbox", OrgID: orgID,
		Status: f.Status, Dismissed: f.Dismissed, Unread: f.Unread, PriorityIn: f.PriorityIn, ScopeIn: f.ScopeIn,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		views []domain.NotificationView
		total int
	)
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		views, total, e = tx.Notifications.ListForViewer(r.Context(), orgID, userID, f,
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "notifications", err)
		return
	}
	items, err := notificationItems(r, notify.NewCaller(s.az, orgID, userID), views)
	if err != nil {
		internalError(w, "notifications", err)
		return
	}
	httpx.WriteList(w, page, items, total)
}

// handleOrgNotificationList is the org admin's "All teams" view: every row in
// the org they may read, team rows included, with their own marks.
//
// POST /api/orgs/{org_id}/notifications/list
func (s *Server) handleOrgNotificationList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	// The path spells the org as the caller typed it, and the rows carry its
	// canonical form, which their actions are checked against. RequireOrgAdmin
	// has parsed it.
	orgID = uuid.MustParse(orgID).String()
	var req notificationOrgListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	f := db.NotificationOrgListFilter{
		NotificationListFilter: parseNotificationFilter(&v, req.Status, req.Dismissed, req.Unread, req.PriorityIn, req.ScopeIn),
	}
	if req.TeamIDIn != nil {
		if len(req.TeamIDIn) == 0 {
			v.Invalid("team_id_in", "team_id_in must name at least one team; leave it out to match every team")
		}
		for _, raw := range req.TeamIDIn {
			id, err := uuid.Parse(raw)
			if err != nil {
				v.Invalid("team_id_in", fmt.Sprintf("team_id_in: %q is not a valid team id", raw))
				f.TeamIDIn = nil
				break
			}
			f.TeamIDIn = append(f.TeamIDIn, id.String())
		}
		slices.Sort(f.TeamIDIn)
		f.TeamIDIn = slices.Compact(f.TeamIDIn)
	}
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(notificationListFilterKey{
		List: "org", OrgID: orgID,
		Status: f.Status, Dismissed: f.Dismissed, Unread: f.Unread, PriorityIn: f.PriorityIn, ScopeIn: f.ScopeIn,
		TeamIDIn: f.TeamIDIn,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		views []domain.NotificationView
		total int
	)
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		views, total, e = tx.Notifications.ListForOrg(r.Context(), orgID, userID, f,
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "notifications", err)
		return
	}
	items, err := notificationItems(r, notify.NewCaller(s.az, orgID, userID), views)
	if err != nil {
		internalError(w, "notifications", err)
		return
	}
	httpx.WriteList(w, page, items, total)
}

// --------------------------------------------------------------------
// one row and its marks
// --------------------------------------------------------------------

// handleNotificationGet returns a row the caller may read, with their marks.
// An org admin may read a team's row here that their inbox does not list.
//
// GET /api/me/notifications/{id}
func (s *Server) handleNotificationGet(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := notificationCaller(w, r)
	if !ok {
		return
	}
	id, ok := notificationIDOr404(w, r)
	if !ok {
		return
	}
	var view *domain.NotificationView
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		view, e = tx.Notifications.GetForViewer(r.Context(), orgID, userID, id)
		return e
	}); err != nil {
		internalError(w, "notifications", err)
		return
	}
	if view == nil {
		notFound(w, "notification")
		return
	}
	writeNotificationItem(w, r, notify.NewCaller(s.az, orgID, userID), *view)
}

// notificationMarksPatchRequest is the body of PATCH
// /api/me/notifications/{id}. Absent keeps a mark, true stamps it now, and
// false or null clears it.
type notificationMarksPatchRequest struct {
	Read      json.RawMessage `json:"read"`
	Dismissed json.RawMessage `json:"dismissed"`
}

// patchMark reads one mark of a marks PATCH: nil when absent, and false when
// null, which PatchBool returns for a clear.
func patchMark(v *httpx.Validation, raw json.RawMessage, field string) *bool {
	on, st := httpx.PatchBool(v, raw, field)
	if st == httpx.PatchAbsent {
		return nil
	}
	return &on
}

// handleNotificationMarksPatch writes the caller's own marks on a row they may
// read and returns the row as they now see it. Dismissing a row that is not
// dismissible is refused: it stays until the system resolves it.
//
// PATCH /api/me/notifications/{id}
func (s *Server) handleNotificationMarksPatch(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := notificationCaller(w, r)
	if !ok {
		return
	}
	id, ok := notificationIDOr404(w, r)
	if !ok {
		return
	}
	var req notificationMarksPatchRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if !httpx.PatchNamed(req.Read, req.Dismissed) {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no marks to update: provide read, dismissed or both",
		})
		return
	}
	var v httpx.Validation
	patch := db.NotificationMarksPatch{
		Read:      patchMark(&v, req.Read, "read"),
		Dismissed: patchMark(&v, req.Dismissed, "dismissed"),
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var view *domain.NotificationView
	err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if _, e := tx.Notifications.SetMarks(r.Context(), orgID, userID, id, patch); e != nil {
			return e
		}
		var e error
		view, e = tx.Notifications.GetForViewer(r.Context(), orgID, userID, id)
		return e
	})
	switch {
	case errors.Is(err, db.ErrNoSuchNotification):
		notFound(w, "notification")
		return
	case errors.Is(err, db.ErrNotificationNotDismissible):
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: "this notification can't be dismissed: it stays until the condition is fixed",
			Field:   "dismissed",
		})
		return
	case err != nil:
		internalError(w, "notifications", err)
		return
	case view == nil:
		// A notice that expired between the write and the read is gone.
		notFound(w, "notification")
		return
	}
	s.pingMarksChanged(orgID, userID)
	writeNotificationItem(w, r, notify.NewCaller(s.az, orgID, userID), *view)
}

// maxMarkReadIDs bounds a mark-read call at the largest page a client can
// have rendered.
const maxMarkReadIDs = httpx.MaxPageSize

// notificationMarkReadRequest is the body of POST
// /api/me/notifications/mark-read: the rows the client rendered.
type notificationMarkReadRequest struct {
	IDs []string `json:"ids"`
}

// notificationMarkReadResponse names the rows the call marked read. An id the
// caller cannot read, or that no longer exists, is absent from it.
type notificationMarkReadResponse struct {
	IDs []string `json:"ids"`
}

// handleNotificationMarkRead marks read the rows the client rendered. It takes
// ids rather than a filter so that a row which arrived after the page rendered
// stays unread.
//
// POST /api/me/notifications/mark-read
func (s *Server) handleNotificationMarkRead(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := notificationCaller(w, r)
	if !ok {
		return
	}
	var req notificationMarkReadRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	switch {
	case len(req.IDs) == 0:
		v.Missing("ids")
	case len(req.IDs) > maxMarkReadIDs:
		v.OutOfRange("ids", fmt.Sprintf("at most %d ids per call", maxMarkReadIDs))
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	ids := make([]string, 0, len(req.IDs))
	for i, raw := range req.IDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			v.Invalid(fmt.Sprintf("ids[%d]", i), fmt.Sprintf("%q is not a valid notification id", raw))
			continue
		}
		ids = append(ids, id.String())
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var marks []domain.NotificationMarks
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		marks, e = tx.Notifications.MarkRead(r.Context(), orgID, userID, ids)
		return e
	}); err != nil {
		internalError(w, "notifications", err)
		return
	}
	out := notificationMarkReadResponse{IDs: make([]string, len(marks))}
	for i, m := range marks {
		out.IDs[i] = m.NotificationID
	}
	slices.Sort(out.IDs)
	if len(marks) > 0 {
		s.pingMarksChanged(orgID, userID)
	}
	writeJSON(w, http.StatusOK, out)
}

// --------------------------------------------------------------------
// invoke
// --------------------------------------------------------------------

// handleNotificationAction runs a row's invoke action as the caller and
// returns the row as they now see it, resolved when the action fixed the
// condition. The route takes no body. An error from the handler is a 500.
//
// POST /api/me/notifications/{id}/actions/{action_id}
func (s *Server) handleNotificationAction(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := notificationCaller(w, r)
	if !ok {
		return
	}
	id, ok := notificationIDOr404(w, r)
	if !ok {
		return
	}
	body, ok := httpx.BodyBytes(w, r)
	if !ok {
		return
	}
	if len(bytes.TrimSpace(body)) > 0 && !httpx.DecodeJSONStrictBytes(w, body, &struct{}{}) {
		return
	}

	read := func() (*domain.NotificationView, error) {
		var view *domain.NotificationView
		err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			view, e = tx.Notifications.GetForViewer(r.Context(), orgID, userID, id)
			return e
		})
		return view, err
	}
	view, err := read()
	if err != nil {
		internalError(w, "notifications", err)
		return
	}
	if view == nil {
		notFound(w, "notification")
		return
	}

	caller := notify.NewCaller(s.az, orgID, userID)
	switch err := caller.RunAction(r.Context(), view.Notification, r.PathValue("action_id")); {
	case errors.Is(err, notify.ErrNoSuchAction):
		notFound(w, "notification action")
		return
	case errors.Is(err, notify.ErrActionForbidden):
		forbidden(w, "you don't hold the permission this action requires")
		return
	case errors.Is(err, notify.ErrConditionResolved):
		conflict(w, "the condition has resolved, so its actions no longer apply")
		return
	case err != nil:
		internalError(w, "notifications", err)
		return
	}

	if view, err = read(); err != nil {
		internalError(w, "notifications", err)
		return
	}
	if view == nil {
		// The action ran, and the row has since left what the caller may
		// read: retention deleted it, or their access changed.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeNotificationItem(w, r, caller, *view)
}
