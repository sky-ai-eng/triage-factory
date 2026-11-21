package server

import (
	"context"
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
	Scope  domain.NotificationScope `json:"scope" doc:"Who the notification is addressed to: org, team, or user."`
	Role   *domain.NotificationRole `json:"role" doc:"Which people of the organization or team the notification is for, admins or members, or null on a user's notification."`
	TeamID *string                  `json:"team_id" doc:"The team's id when scope is team, otherwise null."`
	UserID *string                  `json:"user_id" doc:"The user's id when scope is user, otherwise null."`
}

// notificationJSON is the wire item: the row's display columns, its actions
// as this viewer may use them, and this viewer's marks. The key and params are
// ids for the system's use and never leave the server.
type notificationJSON struct {
	ID             string                             `json:"id" doc:"The notification's id."`
	Kind           string                             `json:"kind" doc:"The kind of notification, such as linear_team_unreadable."`
	Lifecycle      domain.NotificationLifecycle       `json:"lifecycle" doc:"condition for a notification about a problem, which resolves when the problem is fixed, or notice for one that expires and never resolves."`
	Priority       domain.NotificationPriority        `json:"priority" doc:"The notification's priority: urgent, high, or low."`
	Audience       notificationAudienceJSON           `json:"audience" doc:"Who the notification is for."`
	Title          string                             `json:"title" doc:"The notification's title."`
	Body           string                             `json:"body" doc:"The notification's text."`
	Dismissible    bool                               `json:"dismissible" doc:"Whether the notification may be dismissed; one about something the organization requires stays until it resolves."`
	FirstSeenAt    time.Time                          `json:"first_seen_at" doc:"When the notification was first raised, as an RFC 3339 timestamp."`
	LastSeenAt     time.Time                          `json:"last_seen_at" doc:"When the notification was last raised, as an RFC 3339 timestamp."`
	SeenCount      int                                `json:"seen_count" doc:"How many times the notification has been raised, the first time included."`
	ResolvedAt     *time.Time                         `json:"resolved_at" doc:"When the notification resolved, as an RFC 3339 timestamp, or null while it is open."`
	ResolvedReason *domain.NotificationResolvedReason `json:"resolved_reason" doc:"Why the notification resolved: cleared when the problem was fixed, source_off when its event source was turned off, or audience_removed when its audience was taken out of the problem's reach; null while it is open."`
	// Resolution says in a sentence why the row resolved; null while it is
	// open.
	Resolution *string             `json:"resolution" doc:"A sentence saying why the notification resolved, or null while it is open."`
	ExpiresAt  *time.Time          `json:"expires_at" doc:"When a notice expires, as an RFC 3339 timestamp, or null on a condition."`
	Actions    []notify.ActionView `json:"actions" doc:"What the caller can do from the notification."`
	Read       bool                `json:"read" doc:"Whether the caller has read the notification; a rise in its priority makes it unread again."`
	Dismissed  bool                `json:"dismissed" doc:"Whether the caller has dismissed the notification; a rise in its priority brings it back."`
	// Sent is whether the caller is in the row's audience: true on every
	// inbox row, and false on a team's row an org admin reads without being
	// sent it.
	Sent bool `json:"sent" doc:"Whether the caller is in the notification's audience: true on every notification in the caller's inbox, and false on a team's notification an organization admin reads without being sent it."`
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
			Resolution:     nilIfEmpty(v.Resolution),
			ExpiresAt:      v.ExpiresAt,
			Actions:        actions,
			Read:           v.Read,
			Dismissed:      v.Dismissed,
			Sent:           v.Sent,
		}
	}
	return out, nil
}

// notificationActionResponse is an invoke action's answer: what it did, and
// the row as the caller now sees it, null once the row has left what they
// may read.
type notificationActionResponse struct {
	Notification *notificationJSON `json:"notification" doc:"The notification as the caller now sees it, or null when the caller can no longer read it."`
	Outcome      string            `json:"outcome" doc:"A sentence saying what the action did."`
}

// renderNotificationItem renders one view for the caller, answering a 500
// itself when that fails.
func renderNotificationItem(w http.ResponseWriter, r *http.Request, caller *notify.Caller, v domain.NotificationView) (notificationJSON, bool) {
	items, err := notificationItems(r, caller, []domain.NotificationView{v})
	if err != nil {
		internalError(w, "notifications", err)
		return notificationJSON{}, false
	}
	return items[0], true
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
// lists and their facets
// --------------------------------------------------------------------

// notificationFilterRequest is the filters both lists and both facets nodes
// take. An absent filter does not filter. A present one filters by exactly its
// value, so an empty list or an empty string is refused rather than read as
// "no filter", which would answer a wider question than the caller asked.
// search is the exception: as on every list, it is trimmed, and empty after
// the trim is absent.
//
//   - search: a case-insensitive literal substring of the title or the body.
//   - scope_in, role_in, team_id_in: the audience filters. A user row has no
//     role and no team, so role_in or team_id_in leaves it out. The
//     by_audience facet's values each name the filters that select them.
type notificationFilterRequest struct {
	Status     *string  `json:"status" doc:"open or resolved, to include only the notifications in that state; absent includes both."`
	Dismissed  *bool    `json:"dismissed" doc:"true to include only the notifications the caller has dismissed, false to include only those they have not; absent includes both."`
	Unread     *bool    `json:"unread" doc:"true to include only the notifications the caller has not read, false to include only those they have; absent includes both."`
	PriorityIn []string `json:"priority_in" doc:"Priorities to include, from urgent, high, and low; absent includes every priority, and an empty list is refused."`
	ScopeIn    []string `json:"scope_in" doc:"Audience scopes to include, from org, team, and user; absent includes every scope, and an empty list is refused."`
	RoleIn     []string `json:"role_in" doc:"Audience roles to include, from admins and members, which leaves out every user notification; absent includes every role, and an empty list is refused."`
	TeamIDIn   []string `json:"team_id_in" doc:"Ids of the teams whose notifications to include, which leaves out every org and user notification; absent includes every team, and an empty list is refused."`
	Search     string   `json:"search" doc:"Text the notification's title or body must contain, matched without regard to case."`
}

// notificationListRequest is the body of both lists, POST
// /api/me/notifications/list and POST /api/orgs/{org_id}/notifications/list.
//
//   - sort_key: db.NotificationListSortKeys. Absent is the default order,
//     priority then the latest escalation, and there is no "default" value
//     to send, so an empty string is refused. sort_dir is its direction,
//     the key's own (db.NotificationSortDefaultDir) when absent, and is
//     refused without a key: the default order has no direction to flip.
type notificationListRequest struct {
	notificationFilterRequest
	SortKey *string `json:"sort_key" doc:"Orders the result by priority, title, first_seen_at, last_seen_at, or resolved_at; absent orders it by priority, urgent first, and then by when each notification last rose in priority, latest first. An empty string is refused."`
	SortDir *string `json:"sort_dir" doc:"The direction of sort_key, asc or desc; absent is asc for priority and title and desc for the times. It is refused without sort_key, and an empty string is refused."`

	httpx.PageRequest
}

// notificationFacetsRequest is the body of both facets nodes, POST
// /api/me/notifications/facets and POST
// /api/orgs/{org_id}/notifications/facets: the list's filters, on the list's
// terms. The order and paging fields are declared so that sending one is
// refused by name rather than as an unknown field: they are real fields of
// the list, and a caller sending one has misread what this route answers.
// They are json.RawMessage so presence is what is detected.
type notificationFacetsRequest struct {
	notificationFilterRequest

	SortKey   json.RawMessage `json:"sort_key" spec:"-"`
	SortDir   json.RawMessage `json:"sort_dir" spec:"-"`
	PageSize  json.RawMessage `json:"page_size" spec:"-"`
	PageToken json.RawMessage `json:"page_token" spec:"-"`
}

// notificationListSortKeys is db.NotificationListSortKeys as the lists'
// sort vocabulary, each with the direction it takes without a sort_dir.
var notificationListSortKeys = func() []httpx.SortKey {
	keys := make([]httpx.SortKey, len(db.NotificationListSortKeys))
	for i, k := range db.NotificationListSortKeys {
		keys[i] = httpx.SortKey{Name: k, Default: db.NotificationSortDefaultDir(k)}
	}
	return keys
}()

// parseNotificationSort validates a list's sort_key and sort_dir. An explicit
// empty string is refused rather than read as absent: neither field has a
// value that names the default.
func parseNotificationSort(v *httpx.Validation, key, dir *string) (sortKey, sortDir string) {
	refused := false
	if key != nil && *key == "" {
		v.Invalid("sort_key", fmt.Sprintf("sort_key must be one of %s; leave it out for the default order",
			strings.Join(db.NotificationListSortKeys, ", ")))
		refused = true
	}
	if dir != nil && *dir == "" {
		v.Invalid("sort_dir", fmt.Sprintf("sort_dir must be one of %s; leave it out for the key's own direction",
			strings.Join(db.NotificationListSortDirs, ", ")))
		refused = true
	}
	if refused {
		return "", ""
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	order := httpx.Sort(v, deref(key), deref(dir), notificationListSortKeys)
	return order.Key, order.Dir
}

// notificationListFilterKey is the canonicalized filter set and order a page
// token is fingerprinted against. It names the list and the org too, since
// the inbox and the All-teams list share their filters but not their rows.
type notificationListFilterKey struct {
	List       string                        `json:"list"`
	OrgID      string                        `json:"org_id"`
	Status     domain.NotificationStatus     `json:"status"`
	Dismissed  *bool                         `json:"dismissed"`
	Unread     *bool                         `json:"unread"`
	PriorityIn []domain.NotificationPriority `json:"priority_in"`
	ScopeIn    []domain.NotificationScope    `json:"scope_in"`
	RoleIn     []domain.NotificationRole     `json:"role_in"`
	TeamIDIn   []string                      `json:"team_id_in"`
	Search     string                        `json:"search"`
	// The sort is in it because an offset addresses a position in an
	// ordering, so a token minted under one sort names different rows under
	// another.
	SortKey string `json:"sort_key"`
	SortDir string `json:"sort_dir"`
}

// parseNotificationFilter validates the filters the lists and the facets
// nodes share.
func parseNotificationFilter(v *httpx.Validation, in notificationFilterRequest) db.NotificationListFilter {
	f := db.NotificationListFilter{Dismissed: in.Dismissed, Unread: in.Unread}
	if in.Status != nil {
		switch st := domain.NotificationStatus(*in.Status); st {
		case domain.NotificationOpen, domain.NotificationResolved:
			f.Status = st
		default:
			v.Invalid("status", fmt.Sprintf("status must be %q or %q; leave it out to match both", domain.NotificationOpen, domain.NotificationResolved))
		}
	}
	f.PriorityIn = parseNotificationEnums(v, "priority_in", in.PriorityIn, domain.AllNotificationPriorities())
	f.ScopeIn = parseNotificationEnums(v, "scope_in", in.ScopeIn, domain.AllNotificationScopes())
	f.RoleIn = parseNotificationEnums(v, "role_in", in.RoleIn, domain.AllNotificationRoles())
	if in.TeamIDIn != nil {
		if len(in.TeamIDIn) == 0 {
			v.Invalid("team_id_in", "team_id_in must name at least one team; leave it out to match every team")
		}
		for _, raw := range in.TeamIDIn {
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
	f.Search = httpx.Search(v, in.Search)
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

// notificationOrgCaller resolves the /api/orgs/{org_id} routes' subject: an
// admin of the org the path names. The path spells the org as the caller
// typed it, and the rows carry its canonical form, which their actions are
// checked against; RequireOrgAdmin has parsed it.
func (s *Server) notificationOrgCaller(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	orgID, userID, ok = s.az.RequireOrgAdmin(w, r)
	if !ok {
		return "", "", false
	}
	return uuid.MustParse(orgID).String(), userID, true
}

// notificationListRead is one list's store read, as a method expression.
type notificationListRead = func(db.NotificationStore, context.Context, string, string, db.NotificationListFilter, db.ListOpts) ([]domain.NotificationView, int, error)

// notificationFacetsRead is one list's facets read, as a method expression.
type notificationFacetsRead = func(db.NotificationStore, context.Context, string, string, db.NotificationListFilter) (db.NotificationFacets, error)

var opNotificationList = httpx.NewListOp[notificationListRequest, notificationJSON](httpx.OpSpec{
	Pattern:     "POST /api/me/notifications/list",
	ID:          "list_my_notifications",
	Summary:     "List your notifications",
	Description: "Lists one page of the notifications the caller is sent in the organization their credential names, each with the caller's read and dismissed marks and the actions the caller may use.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	req, ok := opNotificationList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	f := parseNotificationListRequest(&v, req)
	page := httpx.ResolvePage(&v, req.PageRequest, notificationListFingerprint("inbox", orgID, f), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	items, total, ok := s.readNotificationPage(w, r, orgID, userID, f, page, db.NotificationStore.ListForViewer)
	if !ok {
		return
	}
	opNotificationList.Write(w, page, items, total)
}

var opOrgNotificationList = httpx.NewListOp[notificationListRequest, notificationJSON](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/notifications/list",
	ID:          "list_org_notifications",
	Summary:     "List an organization's notifications",
	Description: "Lists one page of every notification in the organization, team notifications included, each with the caller's own read and dismissed marks and the actions the caller may use.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleOrgNotificationList is the org admin's "All teams" view: every row in
// the org they may read, team rows included, with their own marks.
//
// POST /api/orgs/{org_id}/notifications/list
func (s *Server) handleOrgNotificationList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.notificationOrgCaller(w, r)
	if !ok {
		return
	}
	req, ok := opOrgNotificationList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	f := parseNotificationListRequest(&v, req)
	page := httpx.ResolvePage(&v, req.PageRequest, notificationListFingerprint("org", orgID, f), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	items, total, ok := s.readNotificationPage(w, r, orgID, userID, f, page, db.NotificationStore.ListForOrg)
	if !ok {
		return
	}
	opOrgNotificationList.Write(w, page, items, total)
}

// parseNotificationListRequest validates a list body's filters and order.
func parseNotificationListRequest(v *httpx.Validation, req notificationListRequest) db.NotificationListFilter {
	f := parseNotificationFilter(v, req.notificationFilterRequest)
	f.SortKey, f.SortDir = parseNotificationSort(v, req.SortKey, req.SortDir)
	return f
}

// notificationListFingerprint is the page fingerprint of list's read of f in
// orgID.
func notificationListFingerprint(list, orgID string, f db.NotificationListFilter) string {
	return httpx.FilterFingerprint(notificationListFilterKey{
		List: list, OrgID: orgID,
		Status: f.Status, Dismissed: f.Dismissed, Unread: f.Unread,
		PriorityIn: f.PriorityIn, ScopeIn: f.ScopeIn, RoleIn: f.RoleIn, TeamIDIn: f.TeamIDIn,
		Search: httpx.SearchKey(f.Search), SortKey: f.SortKey, SortDir: f.SortDir,
	})
}

// readNotificationPage reads one page through read and renders it for the
// caller, answering a 500 itself when either fails.
func (s *Server) readNotificationPage(w http.ResponseWriter, r *http.Request, orgID, userID string,
	f db.NotificationListFilter, page httpx.Page, read notificationListRead,
) ([]notificationJSON, int, bool) {
	var (
		views []domain.NotificationView
		total int
	)
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		views, total, e = read(tx.Notifications, r.Context(), orgID, userID, f,
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "notifications", err)
		return nil, 0, false
	}
	items, err := notificationItems(r, notify.NewCaller(s.az, orgID, userID), views)
	if err != nil {
		internalError(w, "notifications", err)
		return nil, 0, false
	}
	return items, total, true
}

// notificationFacetsResponse is the facets nodes' fixed schema of named cuts.
//
//   - by_priority: the priorities present, urgent first, counted under every
//     filter but priority_in.
//   - by_audience: the audiences present, counted under every filter but the
//     audience filters (scope_in, role_in, team_id_in), in the byte order of
//     their values. A value names the filters that list exactly its rows:
//     "user" is scope_in [user]; "org:admins" and "org:members" are scope_in
//     [org] with role_in [admins] or [members]; "team:<team id>" is
//     team_id_in [<team id>], whichever role the team's rows are for.
//
// A value no row carries is absent, so an empty cut is [].
type notificationFacetsResponse struct {
	ByPriority []httpx.FacetJSON `json:"by_priority" doc:"The priorities the matching notifications have, urgent first, each counted under every filter but priority_in."`
	ByAudience []httpx.FacetJSON `json:"by_audience" doc:"The audiences the matching notifications are for, each counted under every filter but scope_in, role_in, and team_id_in: user, org:admins, org:members, or team: followed by a team's id."`
}

// notificationFacetsOp is the type of both facets nodes' operations.
type notificationFacetsOp = httpx.JSONOp[notificationFacetsRequest, notificationFacetsResponse]

var opNotificationFacets = httpx.NewJSONOp[notificationFacetsRequest, notificationFacetsResponse](httpx.OpSpec{
	Pattern:     "POST /api/me/notifications/facets",
	ID:          "get_my_notification_facets",
	Summary:     "Count your notifications",
	Description: "Counts the notifications in the caller's inbox that the filters match, by priority and by audience, under the inbox list's filters. Each cut is counted without its own filters.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

var opOrgNotificationFacets = httpx.NewJSONOp[notificationFacetsRequest, notificationFacetsResponse](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/notifications/facets",
	ID:          "get_org_notification_facets",
	Summary:     "Count an organization's notifications",
	Description: "Counts the organization's notifications that the filters match, by priority and by audience, under the organization list's filters. Each cut is counted without its own filters.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleNotificationFacets is the inbox's synthetic sibling: how many of the
// rows its filters match sit at each priority and each audience, which a
// client's filter chips count from. It is a node rather than a field on the
// list because the answer is numbers about rows, and a client cannot count
// them from the list, which it holds one page of.
//
// POST /api/me/notifications/facets
func (s *Server) handleNotificationFacets(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := notificationCaller(w, r)
	if !ok {
		return
	}
	s.writeNotificationFacets(w, r, opNotificationFacets, orgID, userID, db.NotificationStore.FacetsForViewer)
}

// handleOrgNotificationFacets is the All-teams list's synthetic sibling, on
// the same terms as the inbox's.
//
// POST /api/orgs/{org_id}/notifications/facets
func (s *Server) handleOrgNotificationFacets(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.notificationOrgCaller(w, r)
	if !ok {
		return
	}
	s.writeNotificationFacets(w, r, opOrgNotificationFacets, orgID, userID, db.NotificationStore.FacetsForOrg)
}

// writeNotificationFacets decodes a facets body and answers the cuts read
// counts. It takes no paging: the cuts group closed vocabularies, so the
// answer is bounded by them rather than by a window.
func (s *Server) writeNotificationFacets(w http.ResponseWriter, r *http.Request, op *notificationFacetsOp, orgID, userID string, read notificationFacetsRead) {
	req, ok := op.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	const orders = "%s orders the list; the facets count rows and have no order to take"
	const pages = "%s is not accepted: the facets are one answer over closed vocabularies, not a page"
	for _, refused := range []struct {
		field  string
		sent   json.RawMessage
		reason string
	}{
		{"sort_key", req.SortKey, orders},
		{"sort_dir", req.SortDir, orders},
		{"page_size", req.PageSize, pages},
		{"page_token", req.PageToken, pages},
	} {
		if refused.sent != nil {
			v.Invalid(refused.field, fmt.Sprintf(refused.reason, refused.field))
		}
	}
	f := parseNotificationFilter(&v, req.notificationFilterRequest)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var facets db.NotificationFacets
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		facets, e = read(tx.Notifications, r.Context(), orgID, userID, f)
		return e
	}); err != nil {
		internalError(w, "notifications", err)
		return
	}
	op.Write(w, notificationFacetsResponse{
		ByPriority: httpx.Facets(facets.ByPriority),
		ByAudience: httpx.Facets(facets.ByAudience),
	})
}

// --------------------------------------------------------------------
// one row and its marks
// --------------------------------------------------------------------

var opNotificationGet = httpx.NewJSONOp[httpx.NoBody, notificationJSON](httpx.OpSpec{
	Pattern:     "GET /api/me/notifications/{id}",
	ID:          "get_notification",
	Summary:     "Get a notification",
	Description: "Returns a notification the caller may read, with the caller's read and dismissed marks and the actions the caller may use. For an organization admin that includes a team's notification the admin is not sent.",
	PathParams:  map[string]string{"id": "The notification's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	if item, ok := renderNotificationItem(w, r, notify.NewCaller(s.az, orgID, userID), *view); ok {
		opNotificationGet.Write(w, item)
	}
}

// notificationMarksPatchRequest is the body of PATCH
// /api/me/notifications/{id}. Absent keeps a mark, true stamps it now, and
// false or null clears it.
type notificationMarksPatchRequest struct {
	Read      httpx.Patch[bool] `json:"read" doc:"true marks the notification read now, and false or null marks it unread."`
	Dismissed httpx.Patch[bool] `json:"dismissed" doc:"true dismisses the notification now, and false or null brings it back; dismissing a notification that is not dismissible is refused."`
}

// patchMark reads one mark of a marks PATCH: nil when absent, and false when
// null, which Read returns for a clear.
func patchMark(v *httpx.Validation, mark httpx.Patch[bool], field string) *bool {
	on, st := mark.Read(v, field)
	if st == httpx.PatchAbsent {
		return nil
	}
	return &on
}

// notDismissibleMessage is the refusal to dismiss a row that is not
// dismissible, on the marks PATCH and the bulk dismiss alike.
const notDismissibleMessage = "this notification can't be dismissed: it stays until the condition is fixed"

var opNotificationMarksPatch = httpx.NewJSONOp[notificationMarksPatchRequest, notificationJSON](httpx.OpSpec{
	Pattern:     "PATCH /api/me/notifications/{id}",
	ID:          "update_notification",
	Summary:     "Update your marks on a notification",
	Description: "Sets or clears the caller's read and dismissed marks on a notification the caller may read, and returns the notification as the caller now sees it. The marks are the caller's own and change nothing for anyone else.",
	PathParams:  map[string]string{"id": "The notification's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	req, ok := opNotificationMarksPatch.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	patch := db.NotificationMarksPatch{
		Read:      patchMark(&v, req.Read, "read"),
		Dismissed: patchMark(&v, req.Dismissed, "dismissed"),
	}
	// A PATCH that names no mark wrote nothing, so it must not answer as a
	// write.
	if patch.Read == nil && patch.Dismissed == nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no marks to update: provide read, dismissed or both",
		})
		return
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
			Message: notDismissibleMessage,
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
	if item, ok := renderNotificationItem(w, r, notify.NewCaller(s.az, orgID, userID), *view); ok {
		opNotificationMarksPatch.Write(w, item)
	}
}

// maxNotificationIDs bounds a mark-read or dismiss call at the largest page a
// client can have rendered.
const maxNotificationIDs = httpx.MaxPageSize

// notificationIDsRequest is the body of POST /api/me/notifications/mark-read
// and POST /api/me/notifications/dismiss: the rows the client rendered.
type notificationIDsRequest struct {
	IDs []string `json:"ids" doc:"Ids of the notifications, from 1 to 200, each a UUID."`
}

// notificationIDsResponse names the rows the call wrote. An id the caller
// cannot read, or that no longer exists, is absent from it.
type notificationIDsResponse struct {
	IDs []string `json:"ids" doc:"The ids of the notifications whose marks the call wrote, sorted; an id the caller cannot read, or that no longer exists, is left out."`
}

// notificationIDsOp is the type of the bulk marks verbs' operations.
type notificationIDsOp = httpx.JSONOp[notificationIDsRequest, notificationIDsResponse]

// notificationIDsBody reads a bulk marks body: 1 to maxNotificationIDs ids,
// each a UUID, returned in their canonical spelling at the index the body
// gave them. Every malformed id is reported.
func notificationIDsBody(w http.ResponseWriter, r *http.Request, op *notificationIDsOp) ([]string, bool) {
	req, ok := op.Decode(w, r)
	if !ok {
		return nil, false
	}
	var v httpx.Validation
	switch {
	case len(req.IDs) == 0:
		v.Missing("ids")
	case len(req.IDs) > maxNotificationIDs:
		v.OutOfRange("ids", fmt.Sprintf("at most %d ids per call", maxNotificationIDs))
	}
	if v.Flush(w, http.StatusBadRequest) {
		return nil, false
	}
	ids := make([]string, len(req.IDs))
	for i, raw := range req.IDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			v.Invalid(fmt.Sprintf("ids[%d]", i), fmt.Sprintf("%q is not a valid notification id", raw))
			continue
		}
		ids[i] = id.String()
	}
	if v.Flush(w, http.StatusBadRequest) {
		return nil, false
	}
	return ids, true
}

// writeNotificationMarksWritten answers a bulk marks call with the sorted ids
// of the marks it wrote, and pings the caller's other tabs when there are any.
func (s *Server) writeNotificationMarksWritten(w http.ResponseWriter, op *notificationIDsOp, orgID, userID string, marks []domain.NotificationMarks) {
	out := notificationIDsResponse{IDs: make([]string, len(marks))}
	for i, m := range marks {
		out.IDs[i] = m.NotificationID
	}
	slices.Sort(out.IDs)
	if len(marks) > 0 {
		s.pingMarksChanged(orgID, userID)
	}
	op.Write(w, out)
}

var opNotificationMarkRead = httpx.NewJSONOp[notificationIDsRequest, notificationIDsResponse](httpx.OpSpec{
	Pattern:     "POST /api/me/notifications/mark-read",
	ID:          "mark_my_notifications_read",
	Summary:     "Mark notifications read",
	Description: "Marks the named notifications read for the caller and returns the ids it marked. A notification the caller cannot read is skipped.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	ids, ok := notificationIDsBody(w, r, opNotificationMarkRead)
	if !ok {
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
	s.writeNotificationMarksWritten(w, opNotificationMarkRead, orgID, userID, marks)
}

var opNotificationDismiss = httpx.NewJSONOp[notificationIDsRequest, notificationIDsResponse](httpx.OpSpec{
	Pattern:     "POST /api/me/notifications/dismiss",
	ID:          "dismiss_my_notifications",
	Summary:     "Dismiss notifications",
	Description: "Dismisses the named notifications for the caller and returns the ids it dismissed. A notification the caller cannot read is skipped, and when any named notification is not dismissible the call dismisses none of them.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleNotificationDismiss dismisses the rows the client selected. An id the
// caller cannot read is left out, since it went away between render and call.
// A readable row that is not dismissible refuses the whole call with a 422 on
// each ids[i] naming one, and nothing is written: every item says whether it
// is dismissible, so sending one is a bad call.
//
// POST /api/me/notifications/dismiss
func (s *Server) handleNotificationDismiss(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := notificationCaller(w, r)
	if !ok {
		return
	}
	ids, ok := notificationIDsBody(w, r, opNotificationDismiss)
	if !ok {
		return
	}
	var marks []domain.NotificationMarks
	err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		marks, e = tx.Notifications.Dismiss(r.Context(), orgID, userID, ids)
		return e
	})
	var refused *db.NotificationsNotDismissibleError
	switch {
	case errors.As(err, &refused):
		var v httpx.Validation
		for i, id := range ids {
			if slices.Contains(refused.IDs, id) {
				v.Invalid(fmt.Sprintf("ids[%d]", i), notDismissibleMessage)
			}
		}
		if !v.Flush(w, http.StatusUnprocessableEntity) {
			internalError(w, "notifications", err)
		}
		return
	case err != nil:
		internalError(w, "notifications", err)
		return
	}
	s.writeNotificationMarksWritten(w, opNotificationDismiss, orgID, userID, marks)
}

// --------------------------------------------------------------------
// invoke
// --------------------------------------------------------------------

var opNotificationAction = httpx.NewJSONOp[httpx.NoBody, notificationActionResponse](httpx.OpSpec{
	Pattern:     "POST /api/me/notifications/{id}/actions/{action_id}",
	ID:          "invoke_notification_action",
	Summary:     "Run a notification's action",
	Description: "Runs one of the notification's invoke actions as the caller, and returns a sentence saying what it did, with the notification as the caller now sees it. An action can change the organization's configuration or start work that calls outside services.",
	PathParams: map[string]string{
		"id":        "The notification's id.",
		"action_id": "The id of one of the notification's invoke actions.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintNo, OpenWorld: httpx.HintYes},
})

// handleNotificationAction runs a row's invoke action as the caller and
// answers what it did, with the row as they now see it: resolved when the
// action resolved the condition, and null when the row has since left what
// they may read. The route takes no body. A handler that refuses with an
// *httpx.Fault, as one writing through a route's own gate does, is answered
// with it; any other error from the handler is a 500.
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
	res, err := caller.RunAction(r.Context(), view.Notification, r.PathValue("action_id"))
	switch {
	case errors.Is(err, notify.ErrNoSuchAction):
		notFound(w, "notification action")
		return
	case errors.Is(err, notify.ErrActionForbidden):
		forbidden(w, "you don't hold the permission this action requires")
		return
	case errors.Is(err, notify.ErrConditionResolved):
		conflict(w, "the condition has resolved, so its actions no longer apply")
		return
	case httpx.WriteFault(w, err):
		return
	case err != nil:
		internalError(w, "notifications", err)
		return
	}

	if view, err = read(); err != nil {
		internalError(w, "notifications", err)
		return
	}
	out := notificationActionResponse{Outcome: res.Outcome}
	// A nil view is a row that has left what the caller may read since the
	// action ran: retention deleted it, or their access changed.
	if view != nil {
		items, err := notificationItems(r, caller, []domain.NotificationView{*view})
		if err != nil {
			internalError(w, "notifications", err)
			return
		}
		out.Notification = &items[0]
	}
	opNotificationAction.Write(w, out)
}

func (s *Server) registerNotificationRoutes() {
	// The caller's notification inbox, their marks on it, and the rows'
	// invoke actions. Viewer-relative: the org is the credential's, so an API
	// token reaches all of it. The list is the rows the caller is SENT; the
	// single read, the marks and the actions take any row they may READ,
	// which for an org admin includes every team's. mark-read and dismiss
	// are verbs because each is a multi-row transition over the ids the client
	// rendered. facets is the list's counts, a POST because its filters are a
	// body.
	s.op(opNotificationList, s.handleNotificationList)
	s.op(opNotificationFacets, s.handleNotificationFacets)
	s.op(opNotificationMarkRead, s.handleNotificationMarkRead)
	s.op(opNotificationDismiss, s.handleNotificationDismiss)
	s.op(opNotificationGet, s.handleNotificationGet)
	s.op(opNotificationMarksPatch, s.handleNotificationMarksPatch)
	s.op(opNotificationAction, s.handleNotificationAction)
	// The org admin's "All teams" view: every row in the org, team rows
	// included, which an org admin may read but is not sent. Authorized
	// against {org_id}; a member who is not an admin gets 403.
	s.op(opOrgNotificationList, s.handleOrgNotificationList)
	s.op(opOrgNotificationFacets, s.handleOrgNotificationFacets)
}
