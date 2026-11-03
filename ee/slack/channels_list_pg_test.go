package slack

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/ttlcache"
)

// listRig is a team with a connected workspace listing every channel in
// channels (id -> name), the bot a member of each, and a tracked set.
type listRig struct {
	*slackChannelsRig
	orgID, owner, teamID string
}

func newListRig(t *testing.T, slug string, channels map[string]string, tracked ...string) listRig {
	t.Helper()
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, slug)
	r.seedWorkspace(orgID, owner, "T0LIST1", "xoxb-list")
	listed := make([]map[string]any, 0, len(channels))
	for id, name := range channels {
		listed = append(listed, map[string]any{"id": id, "name": name, "is_private": false})
	}
	r.fake.setChannels("xoxb-list", listed, listed)
	if len(tracked) > 0 {
		rec := httptest.NewRecorder()
		r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
			map[string]any{"channel_ids": tracked}))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT status = %d (body=%s)", rec.Code, rec.Body.String())
		}
	}
	return listRig{slackChannelsRig: r, orgID: orgID, owner: owner, teamID: teamID}
}

// list posts body to the list route as the team's owner.
func (l listRig) list(body map[string]any) *httptest.ResponseRecorder {
	l.t.Helper()
	rec := httptest.NewRecorder()
	l.hdl.handleList(rec, l.listReq(l.owner, l.orgID, l.teamID, body))
	return rec
}

// listOK posts body and decodes a 200.
func (l listRig) listOK(body map[string]any) channelsListResponse {
	l.t.Helper()
	rec := l.list(body)
	if rec.Code != http.StatusOK {
		l.t.Fatalf("list %v: status = %d, want 200 (body=%s)", body, rec.Code, rec.Body.String())
	}
	return decodeChannelList(l.t, rec)
}

func itemChannelIDs(resp channelsListResponse) []string {
	return channelIDs(resp.Items)
}

// faultFields maps each fault's field to its reason.
func faultFields(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	out := map[string]string{}
	for _, e := range decodeErrors(t, rec) {
		out[e.Field] = e.Reason
	}
	return out
}

var listChannelSet = map[string]string{
	"C1": "eng",
	"C2": "eng-oncall",
	"C3": "ops",
	"C4": "100%-done",
	"C5": "100x-done",
}

func TestChannelsList_SearchTrackedIDsAndSort(t *testing.T) {
	l := newListRig(t, "chan-list-narrow", listChannelSet, "C1")

	all := l.listOK(map[string]any{})
	if all.TotalCount != 5 {
		t.Fatalf("total_count = %d, want 5", all.TotalCount)
	}
	// The default order is name, then channel id.
	if got, want := itemChannelIDs(all), []string{"C4", "C5", "C1", "C2", "C3"}; !slices.Equal(got, want) {
		t.Errorf("default order = %v, want %v", got, want)
	}
	if all.Role != "admin" || all.Warnings == nil {
		t.Errorf("role = %q, warnings = %v; want admin and []", all.Role, all.Warnings)
	}

	cases := []struct {
		name string
		body map[string]any
		want []string
	}{
		// Search covers the name, case-insensitively, and the channel id.
		{"search name", map[string]any{"search": "ENG"}, []string{"C1", "C2"}},
		{"search channel id", map[string]any{"search": "c3"}, []string{"C3"}},
		// A percent sign is the character, not a wildcard.
		{"search percent", map[string]any{"search": "100%"}, []string{"C4"}},
		{"tracked", map[string]any{"tracked": true}, []string{"C1"}},
		{"untracked", map[string]any{"tracked": false}, []string{"C4", "C5", "C2", "C3"}},
		{"search and tracked", map[string]any{"search": "eng", "tracked": false}, []string{"C2"}},
		{"name desc", map[string]any{"sort_key": "name", "sort_dir": "desc"}, []string{"C3", "C2", "C1", "C5", "C4"}},
		// Watched-with-a-primary first, then unwatched by name.
		{"primary", map[string]any{"sort_key": "primary"}, []string{"C1", "C4", "C5", "C2", "C3"}},
	}
	for _, tc := range cases {
		got := l.listOK(tc.body)
		if ids := itemChannelIDs(got); !slices.Equal(ids, tc.want) {
			t.Errorf("%s: items = %v, want %v", tc.name, ids, tc.want)
		}
		if got.TotalCount != len(tc.want) {
			t.Errorf("%s: total_count = %d, want %d", tc.name, got.TotalCount, len(tc.want))
		}
	}

	// ids names registry rows: the tracked channel has one, a live channel
	// TF has never registered has none.
	var engRow string
	for _, c := range all.Items {
		if c.ChannelID == "C1" {
			engRow = c.ID
		}
	}
	if engRow == "" {
		t.Fatalf("tracked C1 carries no registry id: %+v", all.Items)
	}
	byID := l.listOK(map[string]any{"ids": []string{strings.ToUpper(engRow)}})
	if got := itemChannelIDs(byID); !slices.Equal(got, []string{"C1"}) {
		t.Errorf("ids [%s] = %v, want [C1]", engRow, got)
	}
}

func TestChannelsList_Paging(t *testing.T) {
	l := newListRig(t, "chan-list-paging", listChannelSet)

	var walked []string
	body := map[string]any{"page_size": 2}
	for range 4 {
		page := l.listOK(body)
		if page.TotalCount != 5 {
			t.Fatalf("total_count = %d, want 5 on every page", page.TotalCount)
		}
		walked = append(walked, itemChannelIDs(page)...)
		if page.NextPageToken == "" {
			break
		}
		body = map[string]any{"page_size": 2, "page_token": page.NextPageToken}
	}
	if want := []string{"C4", "C5", "C1", "C2", "C3"}; !slices.Equal(walked, want) {
		t.Errorf("walked = %v, want %v", walked, want)
	}

	count := l.listOK(map[string]any{"page_size": 0, "search": "eng"})
	if len(count.Items) != 0 || count.TotalCount != 2 || count.NextPageToken != "" {
		t.Errorf("count-only = %d items, total %d, token %q; want 0, 2, none", len(count.Items), count.TotalCount, count.NextPageToken)
	}
}

func TestChannelsList_RefusesBadFields(t *testing.T) {
	l := newListRig(t, "chan-list-bad", listChannelSet)

	cases := []struct {
		name   string
		body   map[string]any
		field  string
		reason string
	}{
		{"unknown sort_key", map[string]any{"sort_key": "created_at"}, "sort_key", httpx.ReasonInvalidField},
		{"unknown sort_dir", map[string]any{"sort_key": "name", "sort_dir": "up"}, "sort_dir", httpx.ReasonInvalidField},
		{"lone sort_dir", map[string]any{"sort_dir": "asc"}, "sort_dir", httpx.ReasonInvalidField},
		{"tracked not a bool", map[string]any{"tracked": "yes"}, "tracked", httpx.ReasonInvalidField},
		{"ids not registry ids", map[string]any{"ids": []string{"C1"}}, "ids[0]", httpx.ReasonInvalidField},
		{"search too long", map[string]any{"search": strings.Repeat("é", httpx.MaxSearchRunes+1)}, "search", httpx.ReasonInvalidField},
		{"page_size too big", map[string]any{"page_size": httpx.MaxPageSize + 1}, "page_size", httpx.ReasonOutOfRange},
		// The search field is spelled search on every list route; another
		// spelling is refused rather than ignored.
		{"q", map[string]any{"q": "eng"}, "q", httpx.ReasonUnknownField},
	}
	for _, tc := range cases {
		faults := faultFields(t, l.list(tc.body))
		if faults[tc.field] != tc.reason {
			t.Errorf("%s: faults = %v, want %s on %s", tc.name, faults, tc.reason, tc.field)
		}
	}

	// Every faulting field is reported, not just the first.
	faults := faultFields(t, l.list(map[string]any{"sort_key": "nope", "ids": []string{"x"}, "search": strings.Repeat("a", 201)}))
	for _, field := range []string{"sort_key", "ids[0]", "search"} {
		if faults[field] == "" {
			t.Errorf("combined faults = %v, want one on %s", faults, field)
		}
	}

	// The cap is inclusive.
	if rec := l.list(map[string]any{"search": strings.Repeat("é", httpx.MaxSearchRunes)}); rec.Code != http.StatusOK {
		t.Errorf("a %d-rune search: status = %d, want 200 (body=%s)", httpx.MaxSearchRunes, rec.Code, rec.Body.String())
	}
}

// A page token addresses a position in one query's ordering, so it is refused
// once any narrowing field or the order changes — and accepted under a
// spelling of the same search that matches the same rows.
func TestChannelsList_TokenRefusedAcrossNarrowingChange(t *testing.T) {
	l := newListRig(t, "chan-list-token", listChannelSet, "C1")

	first := l.listOK(map[string]any{"search": "eng", "page_size": 1})
	if first.NextPageToken == "" {
		t.Fatalf("first page of two has no next_page_token: %+v", first)
	}
	token := first.NextPageToken

	same := l.listOK(map[string]any{"search": " ENG ", "page_size": 1, "page_token": token})
	if got := itemChannelIDs(same); !slices.Equal(got, []string{"C2"}) {
		t.Errorf("page 2 under the same search = %v, want [C2]", got)
	}

	for _, changed := range []map[string]any{
		{"search": "ops"},
		{"search": "eng", "tracked": true},
		{"search": "eng", "ids": []string{rowEng}},
		{"search": "eng", "sort_key": "name"},
		{"search": "eng", "sort_key": "name", "sort_dir": "desc"},
	} {
		changed["page_size"] = 1
		changed["page_token"] = token
		faults := faultFields(t, l.list(changed))
		if faults["page_token"] != httpx.ReasonInvalidParam {
			t.Errorf("token under %v: faults = %v, want INVALID_PARAM on page_token", changed, faults)
		}
	}
}

func TestChannelsFacets_CountsTheMergedSet(t *testing.T) {
	l := newListRig(t, "chan-facets", listChannelSet, "C1")

	read := func(body map[string]any) channelsFacetsResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		l.hdl.handleFacets(rec, l.facetsReq(l.owner, l.orgID, l.teamID, body))
		if rec.Code != http.StatusOK {
			t.Fatalf("facets %v: status = %d, want 200 (body=%s)", body, rec.Code, rec.Body.String())
		}
		return decodeBody[channelsFacetsResponse](t, rec)
	}
	facet := func(value string, count int) httpx.FacetJSON { return httpx.FacetJSON{Value: value, Count: count} }

	got := read(map[string]any{})
	if want := []httpx.FacetJSON{facet("true", 1), facet("false", 4)}; !slices.Equal(got.Tracked, want) {
		t.Errorf("tracked = %+v, want %+v", got.Tracked, want)
	}
	if want := []httpx.FacetJSON{facet(channelPrimaryThisTeam, 1), facet(channelPrimaryUnwatched, 4)}; !slices.Equal(got.Primary, want) {
		t.Errorf("primary = %+v, want %+v", got.Primary, want)
	}

	// The tracked cut is computed without the tracked filter.
	narrowed := read(map[string]any{"tracked": true, "search": "eng"})
	if want := []httpx.FacetJSON{facet("true", 1), facet("false", 1)}; !slices.Equal(narrowed.Tracked, want) {
		t.Errorf("tracked under tracked=true = %+v, want %+v", narrowed.Tracked, want)
	}
	if want := []httpx.FacetJSON{facet(channelPrimaryThisTeam, 1)}; !slices.Equal(narrowed.Primary, want) {
		t.Errorf("primary under tracked=true = %+v, want %+v", narrowed.Primary, want)
	}

	// A count has no page and no order.
	for _, body := range []map[string]any{{"page_size": 10}, {"sort_key": "name"}} {
		rec := httptest.NewRecorder()
		l.hdl.handleFacets(rec, l.facetsReq(l.owner, l.orgID, l.teamID, body))
		for field := range body {
			if faults := faultFields(t, rec); faults[field] != httpx.ReasonUnknownField {
				t.Errorf("facets %v: faults = %v, want UNKNOWN_FIELD on %s", body, faults, field)
			}
		}
	}

	// The list's gate: a non-member of the team is a 404.
	outsider := pgtest.SeedUser(t, l.h, "chan-facets-outsider")
	pgtest.MustExec(t, l.h.AdminDB, `INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, outsider, l.orgID)
	rec := httptest.NewRecorder()
	l.hdl.handleFacets(rec, l.facetsReq(outsider, l.orgID, l.teamID, map[string]any{}))
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-member facets: status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestChannelsList_EnumeratesSlackOncePerWindow pins the cache: the list's
// pages, a search and the facets answer from one enumeration of the
// workspace, and a PUT, whose auto-join changes what Slack answers, makes the
// next read enumerate again.
func TestChannelsList_EnumeratesSlackOncePerWindow(t *testing.T) {
	l := newListRig(t, "chan-cache", listChannelSet)
	l.hdl.candidates = ttlcache.New[slackCandidateSet](time.Minute)
	calls := func() int {
		l.fake.mu.Lock()
		defer l.fake.mu.Unlock()
		return l.fake.listCalls
	}

	before := calls()
	first := l.listOK(map[string]any{"page_size": 2})
	l.listOK(map[string]any{"page_size": 2, "page_token": first.NextPageToken})
	l.listOK(map[string]any{"search": "dep"})
	rec := httptest.NewRecorder()
	l.hdl.handleFacets(rec, l.facetsReq(l.owner, l.orgID, l.teamID, map[string]any{}))
	if rec.Code != http.StatusOK {
		t.Fatalf("facets status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if got := calls() - before; got != 1 {
		t.Fatalf("conversations.list calls for two pages, a search and the facets = %d, want 1", got)
	}

	rec = httptest.NewRecorder()
	l.hdl.handlePut(rec, l.req(http.MethodPut, "/api/slack/teams/"+l.teamID+"/channels", l.owner, l.orgID, l.teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	afterPut := calls()
	if afterPut-before < 3 {
		t.Fatalf("conversations.list calls through the PUT = %d, want the gate's live read and a fresh one for its answer", afterPut-before)
	}
	l.listOK(map[string]any{})
	if got := calls() - afterPut; got != 0 {
		t.Fatalf("conversations.list calls for a list right after the PUT = %d, want 0: the PUT's answer refilled the cache", got)
	}
}
