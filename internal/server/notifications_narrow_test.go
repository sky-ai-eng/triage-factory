package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// notificationFacetsWire is a facets node's answer as a client decodes it.
type notificationFacetsWire struct {
	ByPriority []httpx.FacetJSON `json:"by_priority"`
	ByAudience []httpx.FacetJSON `json:"by_audience"`
}

func postNotificationFacets(t *testing.T, s *Server, path string, body map[string]any) notificationFacetsWire {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, path, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s %v = %d: %s", path, body, rec.Code, rec.Body.String())
	}
	var out notificationFacetsWire
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode facets: %v (%s)", err, rec.Body.String())
	}
	if out.ByPriority == nil || out.ByAudience == nil {
		t.Fatalf("facets answered a null cut: %s", rec.Body.String())
	}
	return out
}

func notificationTitles(items []notificationItemWire) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Title
	}
	return out
}

// assertFieldFault asserts a 400 whose first fault is reason on field.
func assertFieldFault(t *testing.T, s *Server, path string, body map[string]any, reason, field string) {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, path, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST %s %v = %d, want 400: %s", path, body, rec.Code, rec.Body.String())
	}
	if items := errorItems(t, rec); items[0].Reason != reason || items[0].Field != field {
		t.Errorf("POST %s %v fault = %+v, want %s on %s", path, body, items[0], reason, field)
	}
}

// TestNotificationList_Search: search narrows both lists by title or body, as
// a literal, case-insensitive substring; it is capped, and a page token is
// bound to the needle it was minted under.
func TestNotificationList_Search(t *testing.T) {
	s, _ := newNotificationsServer(t)
	localNotification(t, s, "a 50%")
	localNotification(t, s, "b 50x")
	localNotification(t, s, "c")

	for _, path := range []string{meNotifications + "/list", orgNotificationsList()} {
		for _, c := range []struct {
			search string
			want   []string
		}{
			// A % in the needle is a percent sign, not a wildcard.
			{"50%", []string{"fixable a 50%"}},
			{"  50% ", []string{"fixable a 50%"}},
			{"50", []string{"fixable a 50%", "fixable b 50x"}},
			{"BODY C", []string{"fixable c"}},
			{"", []string{"fixable a 50%", "fixable b 50x", "fixable c"}},
			{"nothing matches", []string{}},
		} {
			got := listNotifications(t, s, path, map[string]any{"search": c.search, "sort_key": "title"})
			if titles := notificationTitles(got.Items); !slices.Equal(titles, c.want) || got.Total() != len(c.want) {
				t.Errorf("%s search %q = %v (total %d), want %v", path, c.search, titles, got.Total(), c.want)
			}
		}

		assertFieldFault(t, s, path, map[string]any{"search": strings.Repeat("é", httpx.MaxSearchRunes+1)}, httpx.ReasonInvalidField, "search")
		if got := listNotifications(t, s, path, map[string]any{"search": strings.Repeat("é", httpx.MaxSearchRunes)}); got.Total() != 0 {
			t.Errorf("%s: a needle at the cap = %d rows, want none and no fault", path, got.Total())
		}

		first := listNotifications(t, s, path, map[string]any{"search": "fixable", "page_size": 1})
		if first.NextPageToken == "" {
			t.Fatalf("%s: searched page 1 minted no token", path)
		}
		// One needle, spelled in other case, is the same query.
		if next := listNotifications(t, s, path, map[string]any{"search": "FIXABLE", "page_size": 1, "page_token": first.NextPageToken}); len(next.Items) != 1 {
			t.Errorf("%s: page 2 under the same needle = %+v", path, next.Items)
		}
		assertFault(t, doJSON(t, s, http.MethodPost, path, map[string]any{"search": "50", "page_size": 1, "page_token": first.NextPageToken}),
			http.StatusBadRequest, httpx.ReasonInvalidParam)
	}
}

// TestNotificationList_Audience: role_in and team_id_in on both lists. Local
// rows are the user's, which has no role and no team, so a valid value of
// either matches nothing; an invalid one is refused with its field named.
func TestNotificationList_Audience(t *testing.T) {
	s, _ := newNotificationsServer(t)
	localNotification(t, s, "a")
	localNotification(t, s, "b")

	for _, path := range []string{meNotifications + "/list", orgNotificationsList()} {
		for _, body := range []map[string]any{
			{"role_in": []string{"admins"}},
			{"role_in": []string{"members", "admins", "members"}},
			{"team_id_in": []string{runmode.LocalDefaultTeamID}},
		} {
			if got := listNotifications(t, s, path, body); got.Total() != 0 {
				t.Errorf("%s %v = %d rows, want none of the user's", path, body, got.Total())
			}
		}
		if got := listNotifications(t, s, path, map[string]any{"scope_in": []string{"user"}}); got.Total() != 2 {
			t.Errorf("%s scope_in user = %d rows, want both", path, got.Total())
		}
		assertFieldFault(t, s, path, map[string]any{"role_in": []string{"owners"}}, httpx.ReasonInvalidField, "role_in")
		assertFieldFault(t, s, path, map[string]any{"role_in": []string{}}, httpx.ReasonInvalidField, "role_in")
		assertFieldFault(t, s, path, map[string]any{"team_id_in": []string{"infra"}}, httpx.ReasonInvalidField, "team_id_in")
		assertFieldFault(t, s, path, map[string]any{"team_id_in": []string{}}, httpx.ReasonInvalidField, "team_id_in")

		page := listNotifications(t, s, path, map[string]any{"page_size": 1})
		for _, changed := range []map[string]any{
			{"role_in": []string{"admins"}},
			{"team_id_in": []string{runmode.LocalDefaultTeamID}},
		} {
			changed["page_size"], changed["page_token"] = 1, page.NextPageToken
			assertFault(t, doJSON(t, s, http.MethodPost, path, changed), http.StatusBadRequest, httpx.ReasonInvalidParam)
		}
	}
}

// TestNotificationList_SortEveryKey: every sort_key orders both lists over
// the whole set, each key's own direction applies without a sort_dir, and a
// page token is bound to the order it was minted under.
func TestNotificationList_SortEveryKey(t *testing.T) {
	s, _ := newNotificationsServer(t)
	localNotification(t, s, "b")
	localNotification(t, s, "a")
	requiredCondition.Set(context.Background(), runmode.LocalDefaultOrgID, notificationTestParams{ID: "c"}, true)

	for _, path := range []string{meNotifications + "/list", orgNotificationsList()} {
		order := func(body map[string]any) []string {
			t.Helper()
			return notificationTitles(listNotifications(t, s, path, body).Items)
		}
		for _, c := range []struct {
			body map[string]any
			want []string
		}{
			// The default order is priority, urgent first, then the newest.
			{map[string]any{}, []string{"required c", "fixable a", "fixable b"}},
			{map[string]any{"sort_key": "priority"}, []string{"required c", "fixable a", "fixable b"}},
			// desc is asc reversed, the latest escalation's place included.
			{map[string]any{"sort_key": "priority", "sort_dir": "desc"}, []string{"fixable b", "fixable a", "required c"}},
			{map[string]any{"sort_key": "title"}, []string{"fixable a", "fixable b", "required c"}},
			{map[string]any{"sort_key": "title", "sort_dir": "desc"}, []string{"required c", "fixable b", "fixable a"}},
			{map[string]any{"sort_key": "first_seen_at"}, []string{"required c", "fixable a", "fixable b"}},
			{map[string]any{"sort_key": "first_seen_at", "sort_dir": "asc"}, []string{"fixable b", "fixable a", "required c"}},
			{map[string]any{"sort_key": "last_seen_at", "sort_dir": "asc"}, []string{"fixable b", "fixable a", "required c"}},
		} {
			if got := order(c.body); !slices.Equal(got, c.want) {
				t.Errorf("%s %v = %v, want %v", path, c.body, got, c.want)
			}
		}

		assertFieldFault(t, s, path, map[string]any{"sort_key": "seen_count"}, httpx.ReasonInvalidField, "sort_key")
		assertFieldFault(t, s, path, map[string]any{"sort_key": "title", "sort_dir": "up"}, httpx.ReasonInvalidField, "sort_dir")
		assertFieldFault(t, s, path, map[string]any{"sort_dir": "asc"}, httpx.ReasonInvalidField, "sort_dir")

		first := listNotifications(t, s, path, map[string]any{"sort_key": "title", "page_size": 1})
		if first.NextPageToken == "" {
			t.Fatalf("%s: sorted page 1 minted no token", path)
		}
		// The key's default direction, spelled out, is the same order.
		if next := listNotifications(t, s, path, map[string]any{"sort_key": "title", "sort_dir": "asc", "page_size": 1, "page_token": first.NextPageToken}); len(next.Items) != 1 || next.Items[0].Title != "fixable b" {
			t.Errorf("%s: page 2 by title = %+v, want fixable b", path, next.Items)
		}
		for _, changed := range []map[string]any{
			{"sort_key": "title", "sort_dir": "desc"},
			{"sort_key": "first_seen_at"},
			{},
		} {
			changed["page_size"], changed["page_token"] = 1, first.NextPageToken
			assertFault(t, doJSON(t, s, http.MethodPost, path, changed), http.StatusBadRequest, httpx.ReasonInvalidParam)
		}
	}

	// b resolves before a; c stays open.
	fixableCondition.Set(context.Background(), runmode.LocalDefaultOrgID, notificationTestParams{ID: "b"}, false)
	fixableCondition.Set(context.Background(), runmode.LocalDefaultOrgID, notificationTestParams{ID: "a"}, false)
	for _, path := range []string{meNotifications + "/list", orgNotificationsList()} {
		for _, c := range []struct {
			body map[string]any
			want []string
		}{
			{map[string]any{"sort_key": "resolved_at"}, []string{"fixable a", "fixable b", "required c"}},
			{map[string]any{"sort_key": "resolved_at", "sort_dir": "asc"}, []string{"fixable b", "fixable a", "required c"}},
			// The empty state's last resolved row.
			{map[string]any{"status": "resolved", "sort_key": "resolved_at", "sort_dir": "desc", "page_size": 1}, []string{"fixable a"}},
		} {
			if got := notificationTitles(listNotifications(t, s, path, c.body).Items); !slices.Equal(got, c.want) {
				t.Errorf("%s %v = %v, want %v", path, c.body, got, c.want)
			}
		}
	}
}

// TestNotificationFacets: both nodes count the list's rows under its filters,
// each cut without its own; they take the list's filters on its terms and
// refuse its order and paging fields by name.
func TestNotificationFacets(t *testing.T) {
	s, _ := newNotificationsServer(t)
	localNotification(t, s, "a")
	localNotification(t, s, "b 50%")
	requiredCondition.Set(context.Background(), runmode.LocalDefaultOrgID, notificationTestParams{ID: "c"}, true)
	fixableCondition.Set(context.Background(), runmode.LocalDefaultOrgID, notificationTestParams{ID: "a"}, false)

	facetPaths := []string{meNotifications + "/facets", "/api/orgs/" + runmode.LocalDefaultOrgID + "/notifications/facets"}
	for _, path := range facetPaths {
		check := func(name string, got, want []httpx.FacetJSON) {
			t.Helper()
			if !slices.Equal(got, want) {
				t.Errorf("%s %s = %+v, want %+v", path, name, got, want)
			}
		}
		all := postNotificationFacets(t, s, path, map[string]any{})
		check("by_priority", all.ByPriority, []httpx.FacetJSON{{Value: "urgent", Count: 1}, {Value: "high", Count: 2}})
		check("by_audience", all.ByAudience, []httpx.FacetJSON{{Value: "user", Count: 3}})

		open := postNotificationFacets(t, s, path, map[string]any{"status": "open", "priority_in": []string{"urgent"}})
		check("open by_priority, priority_in ignored", open.ByPriority, []httpx.FacetJSON{{Value: "urgent", Count: 1}, {Value: "high", Count: 1}})
		check("open by_audience under priority_in", open.ByAudience, []httpx.FacetJSON{{Value: "user", Count: 1}})

		narrowed := postNotificationFacets(t, s, path, map[string]any{"scope_in": []string{"user"}, "role_in": []string{"admins"}, "search": "50%"})
		check("by_priority under the audience filters", narrowed.ByPriority, []httpx.FacetJSON{})
		check("by_audience, audience filters ignored", narrowed.ByAudience, []httpx.FacetJSON{{Value: "user", Count: 1}})

		none := postNotificationFacets(t, s, path, map[string]any{"search": "nothing like it"})
		check("empty by_priority", none.ByPriority, []httpx.FacetJSON{})
		check("empty by_audience", none.ByAudience, []httpx.FacetJSON{})

		for _, field := range []string{"sort_key", "sort_dir", "page_size", "page_token"} {
			assertFieldFault(t, s, path, map[string]any{field: nil}, httpx.ReasonInvalidField, field)
		}
		assertFieldFault(t, s, path, map[string]any{"priority_in": []string{"medium"}}, httpx.ReasonInvalidField, "priority_in")
		assertFieldFault(t, s, path, map[string]any{"role_in": []string{"owners"}}, httpx.ReasonInvalidField, "role_in")
		assertFieldFault(t, s, path, map[string]any{"team_id_in": []string{"infra"}}, httpx.ReasonInvalidField, "team_id_in")
		assertFieldFault(t, s, path, map[string]any{"search": strings.Repeat("x", httpx.MaxSearchRunes+1)}, httpx.ReasonInvalidField, "search")
		assertFieldFault(t, s, path, map[string]any{"q": "x"}, httpx.ReasonUnknownField, "q")
	}

	// A chip's count is what its filters list.
	if got := listNotifications(t, s, meNotifications+"/list", map[string]any{"status": "open", "priority_in": []string{"high"}}); got.Total() != 1 {
		t.Errorf("open high lists %d rows; its facet counts 1", got.Total())
	}
}
