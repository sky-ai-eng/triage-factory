package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/notify"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

type notificationTestParams struct {
	ID     string `json:"id"`
	TeamID string `json:"team_id,omitempty"`
}

func notificationTestKey(p notificationTestParams) string { return p.ID }

func notificationTestTeam(p notificationTestParams) string { return p.TeamID }

var (
	// fixableCondition carries a link and the "fix" invoke action, which the
	// tests below register handlers for.
	fixableCondition = notify.Condition(notify.ConditionSpec[notificationTestParams]{
		Name: "server_test.fixable", Priority: notify.Always(notify.High), Dismissible: true,
		Key: notificationTestKey, Audience: notify.OrgAdmins(),
		Render: func(p notificationTestParams, _ notify.Recipient) notify.Content {
			return notify.Content{Title: "fixable " + p.ID, Body: "body " + p.ID, Actions: []notify.Action{
				notify.Link("Open settings", notify.Routes.OrgSettings()),
				notify.Invoke("fix", "Fix it", notify.RequiresOrgAdmin, notify.Confirm("Fix it now?")),
			}}
		},
		Example: notificationTestParams{ID: "x"},
	})
	// requiredCondition is not dismissible, and its one action has no handler
	// in any process.
	requiredCondition = notify.Condition(notify.ConditionSpec[notificationTestParams]{
		Name: "server_test.required", Priority: notify.Always(notify.Urgent),
		Key: notificationTestKey, Audience: notify.OrgAdmins(),
		Render: func(p notificationTestParams, _ notify.Recipient) notify.Content {
			return notify.Content{Title: "required " + p.ID, Actions: []notify.Action{
				notify.Invoke("unhandled", "Do it", notify.RequiresOrgAdmin),
			}}
		},
		Example: notificationTestParams{ID: "x"},
	})
	// teamCondition is for the org's admins and the admins of the team its
	// params name; only the team's rows carry "untrack".
	teamCondition = notify.Condition(notify.ConditionSpec[notificationTestParams]{
		Name: "server_test.team", Priority: notify.Always(notify.Low), Dismissible: true,
		Key: notificationTestKey, Audience: notify.Audiences(notify.OrgAdmins(), notify.TeamAdmins(notificationTestTeam)),
		Render: func(p notificationTestParams, to notify.Recipient) notify.Content {
			c := notify.Content{Title: "team " + p.ID}
			if to.TeamID != "" {
				c.Actions = []notify.Action{notify.Invoke("untrack", "Stop tracking", notify.RequiresTeamAdmin)}
			}
			return c
		},
		Example: notificationTestParams{ID: "x", TeamID: "00000000-0000-0000-0000-0000000000aa"},
	})
)

// capturedPings records the notifications_updated envelopes a hub publishes.
type capturedPings struct {
	mu     sync.Mutex
	events []websocket.Event
}

func (c *capturedPings) Publish(evt websocket.Event) {
	if evt.Type != notify.EventType {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, evt)
}

// take returns the pings since the last take.
func (c *capturedPings) take() []websocket.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.events
	c.events = nil
	return out
}

// installNotify writes the verbs through s's stores and records the pings its
// hub sends.
func installNotify(t *testing.T, s *Server) *capturedPings {
	t.Helper()
	notifytest.Isolate(t)
	pings := &capturedPings{}
	s.ws.SetBackplane(pings)
	notify.Install(notify.Backend{
		Tx:              s.allStores.Tx,
		Notifications:   s.allStores.Notifications,
		Orgs:            s.allStores.Orgs,
		LinearTeamRules: s.allStores.LinearTeamRules,
		Hub:             s.ws,
		Local:           runmode.Current() == runmode.ModeLocal,
	})
	return pings
}

// notificationItemWire is the item as a client decodes it.
type notificationItemWire struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Lifecycle string `json:"lifecycle"`
	Priority  string `json:"priority"`
	Audience  struct {
		Scope  string  `json:"scope"`
		Role   *string `json:"role"`
		TeamID *string `json:"team_id"`
		UserID *string `json:"user_id"`
	} `json:"audience"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	Dismissible    bool    `json:"dismissible"`
	FirstSeenAt    string  `json:"first_seen_at"`
	LastSeenAt     string  `json:"last_seen_at"`
	SeenCount      int     `json:"seen_count"`
	ResolvedAt     *string `json:"resolved_at"`
	ResolvedReason *string `json:"resolved_reason"`
	ExpiresAt      *string `json:"expires_at"`
	Actions        []struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Label   string `json:"label"`
		Href    string `json:"href"`
		Confirm string `json:"confirm"`
		Allowed *bool  `json:"allowed"`
	} `json:"actions"`
	Read      bool `json:"read"`
	Dismissed bool `json:"dismissed"`
}

func decodeNotificationItem(t *testing.T, body []byte) notificationItemWire {
	t.Helper()
	var out notificationItemWire
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode notification item: %v (%s)", err, body)
	}
	return out
}

func listNotifications(t *testing.T, s *Server, path string, body map[string]any) httpxtest.ListEnvelope[notificationItemWire] {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	return httpxtest.DecodeList[notificationItemWire](t, doJSON(t, s, http.MethodPost, path, body))
}

const meNotifications = "/api/me/notifications"

func orgNotificationsList() string {
	return "/api/orgs/" + runmode.LocalDefaultOrgID + "/notifications/list"
}

// localNotification raises fixableCondition for id and returns its row's id.
func localNotification(t *testing.T, s *Server, id string) string {
	t.Helper()
	fixableCondition.Set(context.Background(), runmode.LocalDefaultOrgID, notificationTestParams{ID: id}, true)
	for _, it := range listNotifications(t, s, meNotifications+"/list", nil).Items {
		if it.Title == "fixable "+id {
			return it.ID
		}
	}
	t.Fatalf("no row for fixable %s after Set", id)
	return ""
}

func newNotificationsServer(t *testing.T) (*Server, *capturedPings) {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	return s, installNotify(t, s)
}

// TestNotificationList_Item pins the wire item and the list mechanics: the
// shape the spec gives, actions rendered for the viewer, the count-only read
// the badge takes, and paging.
func TestNotificationList_Item(t *testing.T) {
	s, _ := newNotificationsServer(t)
	localNotification(t, s, "a")
	localNotification(t, s, "b")

	all := listNotifications(t, s, meNotifications+"/list", nil)
	if all.Total() != 2 || len(all.Items) != 2 {
		t.Fatalf("inbox = %d items of %d, want 2 of 2", len(all.Items), all.Total())
	}
	it := all.Items[0]
	if it.Kind != fixableCondition.Name() || it.Lifecycle != "condition" || it.Priority != "high" || !it.Dismissible || it.SeenCount != 1 {
		t.Errorf("item = %+v", it)
	}
	if it.Audience.Scope != "user" || it.Audience.Role != nil || it.Audience.TeamID != nil ||
		it.Audience.UserID == nil || *it.Audience.UserID != runmode.LocalDefaultUserID {
		t.Errorf("audience = %+v; want the local user's row", it.Audience)
	}
	if it.FirstSeenAt == "" || it.LastSeenAt == "" || it.ResolvedAt != nil || it.ResolvedReason != nil || it.ExpiresAt != nil {
		t.Errorf("timestamps = first %q last %q resolved %v/%v expires %v", it.FirstSeenAt, it.LastSeenAt, it.ResolvedAt, it.ResolvedReason, it.ExpiresAt)
	}
	if it.Read || it.Dismissed {
		t.Errorf("a fresh row reads read=%v dismissed=%v", it.Read, it.Dismissed)
	}
	if len(it.Actions) != 2 {
		t.Fatalf("actions = %+v, want the link and the invoke", it.Actions)
	}
	if link := it.Actions[0]; link.Type != "link" || link.ID != "org_settings" || link.Href != "/settings" || link.Allowed != nil {
		t.Errorf("link = %+v", link)
	}
	if inv := it.Actions[1]; inv.Type != "invoke" || inv.ID != "fix" || inv.Confirm != "Fix it now?" || inv.Allowed == nil {
		t.Errorf("invoke = %+v", inv)
	}

	count := listNotifications(t, s, meNotifications+"/list", map[string]any{"page_size": 0})
	if count.Total() != 2 || len(count.Items) != 0 || count.NextPageToken != "" {
		t.Errorf("count-only = %d items, total %d, token %q; want no items and total 2", len(count.Items), count.Total(), count.NextPageToken)
	}

	first := listNotifications(t, s, meNotifications+"/list", map[string]any{"page_size": 1})
	if len(first.Items) != 1 || first.NextPageToken == "" {
		t.Fatalf("page 1 = %d items, token %q", len(first.Items), first.NextPageToken)
	}
	second := listNotifications(t, s, meNotifications+"/list", map[string]any{"page_size": 1, "page_token": first.NextPageToken})
	if len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID || second.NextPageToken != "" {
		t.Errorf("page 2 = %+v, token %q; want the other row and no further page", second.Items, second.NextPageToken)
	}
	// A token is bound to the filters it was minted for.
	rec := doJSON(t, s, http.MethodPost, meNotifications+"/list", map[string]any{"page_size": 1, "page_token": first.NextPageToken, "unread": true})
	assertFault(t, rec, http.StatusBadRequest, httpx.ReasonInvalidParam)
}

// TestNotificationList_Filters: each filter narrows, an absent one does not,
// and a malformed one is refused with its field named rather than dropped.
func TestNotificationList_Filters(t *testing.T) {
	s, _ := newNotificationsServer(t)
	read := localNotification(t, s, "a")
	localNotification(t, s, "b")
	requiredCondition.Set(context.Background(), runmode.LocalDefaultOrgID, notificationTestParams{ID: "c"}, true)
	fixableCondition.Set(context.Background(), runmode.LocalDefaultOrgID, notificationTestParams{ID: "b"}, false)
	if rec := doJSON(t, s, http.MethodPatch, meNotifications+"/"+read, map[string]any{"read": true}); rec.Code != http.StatusOK {
		t.Fatalf("mark read = %d: %s", rec.Code, rec.Body.String())
	}

	for _, c := range []struct {
		name string
		body map[string]any
		want []string
	}{
		{"no filter", map[string]any{}, []string{"required c", "fixable b", "fixable a"}},
		{"open", map[string]any{"status": "open"}, []string{"required c", "fixable a"}},
		{"resolved", map[string]any{"status": "resolved"}, []string{"fixable b"}},
		{"unread", map[string]any{"unread": true}, []string{"required c", "fixable b"}},
		{"read", map[string]any{"unread": false}, []string{"fixable a"}},
		{"undismissed", map[string]any{"dismissed": false}, []string{"required c", "fixable b", "fixable a"}},
		{"urgent", map[string]any{"priority_in": []string{"urgent"}}, []string{"required c"}},
		{"urgent or high, twice", map[string]any{"priority_in": []string{"high", "urgent", "high"}}, []string{"required c", "fixable b", "fixable a"}},
		{"team scope", map[string]any{"scope_in": []string{"team"}}, nil},
		{"user scope", map[string]any{"scope_in": []string{"user"}, "status": "open"}, []string{"required c", "fixable a"}},
		{"null is absent", map[string]any{"status": nil, "priority_in": nil}, []string{"required c", "fixable b", "fixable a"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := listNotifications(t, s, meNotifications+"/list", c.body)
			var titles []string
			for _, it := range got.Items {
				titles = append(titles, it.Title)
			}
			if strings.Join(titles, ",") != strings.Join(c.want, ",") || got.Total() != len(c.want) {
				t.Errorf("titles = %v (total %d), want %v", titles, got.Total(), c.want)
			}
		})
	}

	for _, c := range []struct {
		name   string
		body   map[string]any
		reason string
		field  string
	}{
		{"empty status", map[string]any{"status": ""}, httpx.ReasonInvalidField, "status"},
		{"unknown status", map[string]any{"status": "closed"}, httpx.ReasonInvalidField, "status"},
		{"empty priority_in", map[string]any{"priority_in": []string{}}, httpx.ReasonInvalidField, "priority_in"},
		{"unknown priority", map[string]any{"priority_in": []string{"high", "medium"}}, httpx.ReasonInvalidField, "priority_in"},
		{"unknown scope", map[string]any{"scope_in": []string{"everyone"}}, httpx.ReasonInvalidField, "scope_in"},
		{"wrong type", map[string]any{"unread": "yes"}, httpx.ReasonInvalidField, "unread"},
		{"unknown field", map[string]any{"team_id_in": []string{runmode.LocalDefaultTeamID}}, httpx.ReasonUnknownField, "team_id_in"},
		{"page too large", map[string]any{"page_size": httpx.MaxPageSize + 1}, httpx.ReasonOutOfRange, "page_size"},
	} {
		t.Run("refuses "+c.name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPost, meNotifications+"/list", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			items := errorItems(t, rec)
			if items[0].Reason != c.reason || items[0].Field != c.field {
				t.Errorf("fault = %+v, want %s on %s", items[0], c.reason, c.field)
			}
		})
	}

	// Every bad field is reported at once.
	rec := doJSON(t, s, http.MethodPost, meNotifications+"/list", map[string]any{"status": "x", "scope_in": []string{"y"}})
	if items := errorItems(t, rec); len(items) != 2 {
		t.Errorf("two bad fields reported %d faults: %s", len(items), rec.Body.String())
	}
}

// TestNotificationGet: a readable row by id, and 404 for an id that names
// nothing the caller may read.
func TestNotificationGet(t *testing.T) {
	s, _ := newNotificationsServer(t)
	id := localNotification(t, s, "a")

	rec := doJSON(t, s, http.MethodGet, meNotifications+"/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
	}
	if it := decodeNotificationItem(t, rec.Body.Bytes()); it.ID != id || it.Title != "fixable a" || len(it.Actions) != 2 {
		t.Errorf("GET item = %+v", it)
	}
	// Spelled in capitals, the id still names the row.
	if rec := doJSON(t, s, http.MethodGet, meNotifications+"/"+strings.ToUpper(id), nil); rec.Code != http.StatusOK {
		t.Errorf("GET by uppercase id = %d: %s", rec.Code, rec.Body.String())
	}
	for _, missing := range []string{fixtureUUID("no such notification"), "not-a-uuid"} {
		assertFault(t, doJSON(t, s, http.MethodGet, meNotifications+"/"+missing, nil), http.StatusNotFound, httpx.ReasonNotFound)
	}
}

// TestNotificationMarksPatch: absent keeps, true stamps, false and null clear;
// a body naming no mark is refused, and dismissing a row that is not
// dismissible is a 422 on dismissed. The caller's other tabs are pinged.
func TestNotificationMarksPatch(t *testing.T) {
	s, pings := newNotificationsServer(t)
	id := localNotification(t, s, "a")
	pings.take()

	patch := func(body any) notificationItemWire {
		t.Helper()
		rec := doJSON(t, s, http.MethodPatch, meNotifications+"/"+id, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH %v = %d: %s", body, rec.Code, rec.Body.String())
		}
		return decodeNotificationItem(t, rec.Body.Bytes())
	}
	if it := patch(map[string]any{"read": true}); !it.Read || it.Dismissed || it.ID != id || len(it.Actions) != 2 {
		t.Errorf("after read=true: %+v", it)
	}
	got := pings.take()
	if len(got) != 1 || got[0].UserID != runmode.LocalDefaultUserID || got[0].OrgID != runmode.LocalDefaultOrgID {
		t.Errorf("pings after a marks write = %+v; want one to the caller", got)
	}
	if it := patch(map[string]any{"dismissed": true}); !it.Read || !it.Dismissed {
		t.Errorf("after dismissed=true (read absent): %+v; read must be kept", it)
	}
	if it := patch(map[string]any{"read": false}); it.Read || !it.Dismissed {
		t.Errorf("after read=false: %+v", it)
	}
	if it := patch(map[string]any{"dismissed": nil}); it.Dismissed {
		t.Errorf("after dismissed=null: %+v; null clears", it)
	}
	if got := listNotifications(t, s, meNotifications+"/list", map[string]any{"dismissed": false, "unread": true}); got.Total() != 1 {
		t.Errorf("unread undismissed after clearing both = %d, want the row", got.Total())
	}
	pings.take()

	assertFault(t, doJSON(t, s, http.MethodPatch, meNotifications+"/"+id, map[string]any{}), http.StatusBadRequest, httpx.ReasonMissingField)
	assertFault(t, doJSON(t, s, http.MethodPatch, meNotifications+"/"+id, map[string]any{"read": "yes"}), http.StatusBadRequest, httpx.ReasonInvalidField)
	assertFault(t, doJSON(t, s, http.MethodPatch, meNotifications+"/"+id, map[string]any{"seen": true}), http.StatusBadRequest, httpx.ReasonUnknownField)
	assertFault(t, doJSON(t, s, http.MethodPatch, meNotifications+"/"+fixtureUUID("nope"), map[string]any{"read": true}), http.StatusNotFound, httpx.ReasonNotFound)
	if got := pings.take(); len(got) != 0 {
		t.Errorf("refused PATCHes pinged %+v", got)
	}

	requiredCondition.Set(context.Background(), runmode.LocalDefaultOrgID, notificationTestParams{ID: "r"}, true)
	required := listNotifications(t, s, meNotifications+"/list", map[string]any{"priority_in": []string{"urgent"}}).Items[0]
	rec := doJSON(t, s, http.MethodPatch, meNotifications+"/"+required.ID, map[string]any{"read": true, "dismissed": true})
	assertFault(t, rec, http.StatusUnprocessableEntity, httpx.ReasonInvalidField)
	if f := errorItems(t, rec)[0].Field; f != "dismissed" {
		t.Errorf("non-dismissible fault field = %q, want dismissed", f)
	}
	// The refusal wrote nothing, read included.
	if got := listNotifications(t, s, meNotifications+"/list", map[string]any{"priority_in": []string{"urgent"}}).Items[0]; got.Read {
		t.Error("a refused PATCH stamped read")
	}
	// Clearing a dismissal it never had is not a dismissal.
	if rec := doJSON(t, s, http.MethodPatch, meNotifications+"/"+required.ID, map[string]any{"dismissed": false}); rec.Code != http.StatusOK {
		t.Errorf("dismissed=false on a non-dismissible row = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestNotificationMarkRead marks the ids the client rendered and answers
// which it marked; an id the caller cannot read is left out rather than
// failing the rest.
func TestNotificationMarkRead(t *testing.T) {
	s, pings := newNotificationsServer(t)
	a := localNotification(t, s, "a")
	b := localNotification(t, s, "b")
	localNotification(t, s, "c")
	pings.take()

	rec := doJSON(t, s, http.MethodPost, meNotifications+"/mark-read", map[string]any{
		"ids": []string{b, strings.ToUpper(a), fixtureUUID("gone"), a},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("mark-read = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{a, b}
	if a > b {
		want = []string{b, a}
	}
	if strings.Join(out.IDs, ",") != strings.Join(want, ",") {
		t.Errorf("marked = %v, want %v", out.IDs, want)
	}
	if got := listNotifications(t, s, meNotifications+"/list", map[string]any{"unread": true}); got.Total() != 1 || got.Items[0].Title != "fixable c" {
		t.Errorf("unread after mark-read = %+v", got.Items)
	}
	if got := pings.take(); len(got) != 1 || got[0].UserID != runmode.LocalDefaultUserID {
		t.Errorf("pings = %+v; want one to the caller", got)
	}

	tooMany := make([]string, maxMarkReadIDs+1)
	for i := range tooMany {
		tooMany[i] = a
	}
	for _, c := range []struct {
		name   string
		body   map[string]any
		reason string
		field  string
	}{
		{"absent", map[string]any{}, httpx.ReasonMissingField, "ids"},
		{"empty", map[string]any{"ids": []string{}}, httpx.ReasonMissingField, "ids"},
		{"malformed", map[string]any{"ids": []string{a, "x"}}, httpx.ReasonInvalidField, "ids[1]"},
		{"too many", map[string]any{"ids": tooMany}, httpx.ReasonOutOfRange, "ids"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPost, meNotifications+"/mark-read", c.body)
			assertFault(t, rec, http.StatusBadRequest, c.reason)
			if f := errorItems(t, rec)[0].Field; f != c.field {
				t.Errorf("field = %q, want %q", f, c.field)
			}
		})
	}

	// Nothing readable is a 200 that marked nothing, and pings nobody.
	rec = doJSON(t, s, http.MethodPost, meNotifications+"/mark-read", map[string]any{"ids": []string{fixtureUUID("gone")}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ids":[]`) {
		t.Errorf("mark-read of nothing readable = %d %s", rec.Code, rec.Body.String())
	}
	if got := pings.take(); len(got) != 0 {
		t.Errorf("marking nothing pinged %+v", got)
	}
}

// TestNotificationAction runs an invoke action through the dispatcher: the
// handler's effect, the row resolving when it reports the condition fixed, a
// handler's own refusal, and the route's 404s, 409 and body check.
func TestNotificationAction(t *testing.T) {
	s, _ := newNotificationsServer(t)
	var calls []notify.ActionCall[notificationTestParams]
	resolve, refusal := false, error(nil)
	fixableCondition.HandleAction("fix", func(_ context.Context, call notify.ActionCall[notificationTestParams]) (bool, error) {
		calls = append(calls, call)
		return resolve, refusal
	})
	id := localNotification(t, s, "a")
	invoke := func(id, action string, body any) (int, []byte) {
		t.Helper()
		rec := doJSON(t, s, http.MethodPost, meNotifications+"/"+id+"/actions/"+action, body)
		return rec.Code, rec.Body.Bytes()
	}

	// Not fixed: the handler ran with the row's params, the row stays open.
	code, body := invoke(id, "fix", nil)
	if code != http.StatusOK {
		t.Fatalf("invoke = %d: %s", code, body)
	}
	if len(calls) != 1 || calls[0].Params.ID != "a" || calls[0].OrgID != runmode.LocalDefaultOrgID || calls[0].UserID != runmode.LocalDefaultUserID {
		t.Errorf("handler calls = %+v", calls)
	}
	if it := decodeNotificationItem(t, body); it.ResolvedAt != nil || it.Actions[1].Allowed == nil || !*it.Actions[1].Allowed {
		t.Errorf("after an unfixed invoke: %+v", it)
	}

	// An empty object is no body; a handler's error is a 500.
	refusal = errors.New("store down")
	if code, _ := invoke(id, "fix", map[string]any{}); code != http.StatusInternalServerError {
		t.Errorf("handler error = %d, want 500", code)
	}
	// A handler's own refusal is answered as it refused, wrapped or not.
	refusal = fmt.Errorf("fix: %w", httpx.NewFault(http.StatusForbidden, httpx.ReasonTeamArchived, "team is archived"))
	assertFault(t, doJSON(t, s, http.MethodPost, meNotifications+"/"+id+"/actions/fix", nil), http.StatusForbidden, httpx.ReasonTeamArchived)
	refusal = nil

	// The route takes no body: anything with a field is refused, and the
	// handler never runs.
	before := len(calls)
	rec := doJSON(t, s, http.MethodPost, meNotifications+"/"+id+"/actions/fix", map[string]any{"confirm": true})
	assertFault(t, rec, http.StatusBadRequest, httpx.ReasonUnknownField)
	if len(calls) != before {
		t.Error("a refused body ran the handler")
	}

	// Fixed: the row resolves, and its action is no longer offered.
	resolve = true
	code, body = invoke(id, "fix", nil)
	if code != http.StatusOK {
		t.Fatalf("fixing invoke = %d: %s", code, body)
	}
	it := decodeNotificationItem(t, body)
	if it.ResolvedAt == nil || it.ResolvedReason == nil || *it.ResolvedReason != "cleared" {
		t.Errorf("after a fixing invoke: resolved %v/%v", it.ResolvedAt, it.ResolvedReason)
	}
	if a := it.Actions[1]; a.Allowed == nil || *a.Allowed {
		t.Errorf("a resolved row's invoke allowed = %v, want false", a.Allowed)
	}
	assertFault(t, doJSON(t, s, http.MethodPost, meNotifications+"/"+id+"/actions/fix", nil), http.StatusConflict, httpx.ReasonConflict)

	open := localNotification(t, s, "b")
	assertFault(t, doJSON(t, s, http.MethodPost, meNotifications+"/"+open+"/actions/nope", nil), http.StatusNotFound, httpx.ReasonNotFound)
	// The link is an action, but not one the route runs.
	assertFault(t, doJSON(t, s, http.MethodPost, meNotifications+"/"+open+"/actions/org_settings", nil), http.StatusNotFound, httpx.ReasonNotFound)
	assertFault(t, doJSON(t, s, http.MethodPost, meNotifications+"/"+fixtureUUID("nope")+"/actions/fix", nil), http.StatusNotFound, httpx.ReasonNotFound)

	// An action no process handles is not offered, and not run.
	requiredCondition.Set(context.Background(), runmode.LocalDefaultOrgID, notificationTestParams{ID: "r"}, true)
	required := listNotifications(t, s, meNotifications+"/list", map[string]any{"priority_in": []string{"urgent"}}).Items[0]
	if a := required.Actions[0]; a.Allowed == nil || *a.Allowed {
		t.Errorf("unhandled action allowed = %v, want false", a.Allowed)
	}
	assertFault(t, doJSON(t, s, http.MethodPost, meNotifications+"/"+required.ID+"/actions/unhandled", nil), http.StatusNotFound, httpx.ReasonNotFound)
}

// TestOrgNotificationList_Local: local mode's one user is its org's admin, so
// the All-teams list answers, and its team filter is parsed strictly.
func TestOrgNotificationList_Local(t *testing.T) {
	s, _ := newNotificationsServer(t)
	localNotification(t, s, "a")

	localNotification(t, s, "b")

	all := listNotifications(t, s, orgNotificationsList(), map[string]any{"page_size": 1})
	if all.Total() != 2 || all.NextPageToken == "" {
		t.Fatalf("org list = %d rows, token %q; want 2 and a next page", all.Total(), all.NextPageToken)
	}
	// The two lists share their filters, not their rows, so neither takes
	// the other's page token.
	assertFault(t, doJSON(t, s, http.MethodPost, meNotifications+"/list", map[string]any{"page_size": 1, "page_token": all.NextPageToken}),
		http.StatusBadRequest, httpx.ReasonInvalidParam)
	inbox := listNotifications(t, s, meNotifications+"/list", map[string]any{"page_size": 1})
	assertFault(t, doJSON(t, s, http.MethodPost, orgNotificationsList(), map[string]any{"page_size": 1, "page_token": inbox.NextPageToken}),
		http.StatusBadRequest, httpx.ReasonInvalidParam)
	// Local rows are the user's, so a team narrow matches none of them.
	if got := listNotifications(t, s, orgNotificationsList(), map[string]any{"team_id_in": []string{runmode.LocalDefaultTeamID}}); got.Total() != 0 {
		t.Errorf("org list narrowed to a team = %d rows, want 0", got.Total())
	}
	for _, body := range []map[string]any{
		{"team_id_in": []string{}},
		{"team_id_in": []string{runmode.LocalDefaultTeamID, "default"}},
	} {
		rec := doJSON(t, s, http.MethodPost, orgNotificationsList(), body)
		assertFault(t, rec, http.StatusBadRequest, httpx.ReasonInvalidField)
		if f := errorItems(t, rec)[0].Field; f != "team_id_in" {
			t.Errorf("field = %q, want team_id_in", f)
		}
	}
}
