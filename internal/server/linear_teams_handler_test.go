package server

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

func linearOrgPath(orgID string) string { return "/api/orgs/" + orgID + "/linear" }

var (
	linearTeamsListPath = linearOrgPath(runmode.LocalDefaultOrgID) + "/teams/list"
)

func linearTeamPath(teamID string) string {
	return linearOrgPath(runmode.LocalDefaultOrgID) + "/teams/" + teamID
}

func linearStatesListPath(teamID string) string { return linearTeamPath(teamID) + "/states/list" }

func linearStatePath(teamID, stateID string) string {
	return linearTeamPath(teamID) + "/states/" + stateID
}

func linearTeamKeysOf(items []linearTeamJSON) string {
	keys := make([]string, 0, len(items))
	for _, t := range items {
		keys = append(keys, t.Key)
	}
	return strings.Join(keys, ",")
}

// assertOneFault fails unless rec answered status with exactly one error, of
// this reason and naming this field ("" for a fault with no field).
func assertOneFault(t *testing.T, rec *httptest.ResponseRecorder, status int, reason, field string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, status, rec.Body.String())
	}
	items := decodeErrorItems(t, rec)
	if len(items) != 1 {
		t.Fatalf("errors = %d, want 1; body=%s", len(items), rec.Body.String())
	}
	if items[0].Reason != reason || items[0].Field != field {
		t.Errorf("error = %s on %q, want %s on %q (%s)", items[0].Reason, items[0].Field, reason, field, items[0].Message)
	}
}

// TestLinearTeamsList_TokenIsFingerprintedOnSearch: the search is applied
// upstream and pages through Linear's cursor with total_count null. A token
// minted for one search cannot page another's, and case alone does not make
// two searches.
func TestLinearTeamsList_TokenIsFingerprintedOnSearch(t *testing.T) {
	alphaBeta := linear.Team{ID: fixtureUUID("t2"), Key: "ABB", Name: "Alpha Beta", Private: true}
	s, _ := newServerWithLinearCatalog(t,
		linear.Team{ID: fixtureUUID("t1"), Key: "AAA", Name: "Alpha"},
		alphaBeta,
		linear.Team{ID: fixtureUUID("t3"), Key: "ZZZ", Name: "Zed"},
	)
	first := httpxtest.DecodeList[linearTeamJSON](t, doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"search": "a", "page_size": 1}))
	if got := linearTeamKeysOf(first.Items); got != "AAA" || first.NextPageToken == "" || first.TotalCount != nil {
		t.Fatalf("page 1 = %s (token %q, total %v), want AAA with a token and a null total", got, first.NextPageToken, first.TotalCount)
	}

	rec := doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"search": "alpha", "page_size": 1, "page_token": first.NextPageToken})
	assertOneFault(t, rec, http.StatusBadRequest, "INVALID_PARAM", "page_token")

	// ZZZ does not match, so the second and last page is ABB.
	second := httpxtest.DecodeList[linearTeamJSON](t, doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"search": "A", "page_size": 1, "page_token": first.NextPageToken}))
	want := linearTeamJSON{ID: alphaBeta.ID, Key: "ABB", Name: "Alpha Beta", Private: true}
	if len(second.Items) != 1 || second.Items[0] != want || second.NextPageToken != "" {
		t.Errorf("page 2 in another case = %+v (token %q), want only %+v", second.Items, second.NextPageToken, want)
	}
}

// TestLinearTeamsList_SearchIsLiteral: a % or an _ in the search is the
// character itself, even from a Linear that reads them as LIKE wildcards. A
// row Linear matched only through the wildcard is dropped from its page, a
// page left empty is replaced by the next one Linear serves, and no matching
// row is lost on the walk.
func TestLinearTeamsList_SearchIsLiteral(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t,
		linear.Team{ID: fixtureUUID("t-abc"), Key: "ABC", Name: "Abc"},
		linear.Team{ID: fixtureUUID("t-pct"), Key: "A%C", Name: "Percent"},
		linear.Team{ID: fixtureUUID("t-und"), Key: "A_C", Name: "Underscore"},
		linear.Team{ID: fixtureUUID("t-axc"), Key: "AXC", Name: "Axc"},
	)
	fake.SetLikeWildcards(true)
	if !fakeContains("ABC", "a%c", true) {
		t.Fatal("the fake does not read % as a wildcard, so this test proves nothing")
	}

	for search, want := range map[string]string{"a%c": "A%C", "a_c": "A_C", "%": "A%C", "_": "A_C"} {
		var got []string
		token, pages := "", 0
		for {
			body := map[string]any{"search": search, "page_size": 1}
			if token != "" {
				body["page_token"] = token
			}
			page := httpxtest.DecodeList[linearTeamJSON](t, doJSON(t, s, http.MethodPost, linearTeamsListPath, body))
			if key := linearTeamKeysOf(page.Items); key != "" {
				got = append(got, key)
			}
			pages++
			if token = page.NextPageToken; token == "" || pages > 10 {
				break
			}
		}
		if strings.Join(got, ",") != want {
			t.Errorf("search %q walked to %v, want only %s", search, got, want)
		}
	}

	// Linear's first page holds only ABC, so the page served is its second.
	before := fake.Calls()
	first := httpxtest.DecodeList[linearTeamJSON](t, doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"search": "a%c", "page_size": 1}))
	if got := linearTeamKeysOf(first.Items); got != "A%C" || first.NextPageToken == "" || fake.Calls()-before != 2 {
		t.Errorf("page 1 = %s (token %q) in %d reads, want A%%C and a token in 2", got, first.NextPageToken, fake.Calls()-before)
	}
}

// TestLinearTeamsList_SearchedReadsAreBounded: a request reads at most
// linearSearchedPageReads of Linear's pages past pages the literal re-check
// left empty, and past that answers 502 rather than end the listing early.
func TestLinearTeamsList_SearchedReadsAreBounded(t *testing.T) {
	teams := make([]linear.Team, 0, linearSearchedPageReads+1)
	for i := range linearSearchedPageReads {
		key := fmt.Sprintf("A%dC", i)
		teams = append(teams, linear.Team{ID: fixtureUUID("t-" + key), Key: key, Name: key})
	}
	teams = append(teams, linear.Team{ID: fixtureUUID("t-pct"), Key: "A%C", Name: "Percent"})
	s, fake := newServerWithLinearCatalog(t, teams...)
	fake.SetLikeWildcards(true)

	before := fake.Calls()
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{"search": "a%c", "page_size": 1}),
		http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "")
	if got := fake.Calls() - before; got != linearSearchedPageReads {
		t.Errorf("reads = %d, want %d", got, linearSearchedPageReads)
	}
	// Pages of two reach A%C on Linear's sixth page, inside the bound.
	page := httpxtest.DecodeList[linearTeamJSON](t, doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"search": "a%c", "page_size": 2}))
	if got := linearTeamKeysOf(page.Items); got != "A%C" || page.NextPageToken != "" {
		t.Errorf("pages of two = %s (token %q), want A%%C and no token", got, page.NextPageToken)
	}
}

// TestLinearTeamsList_RefusedBodies: count-only has no answer on a proxy list,
// a search past the cap is refused, and a field the route does not take, the
// old name of the search among them, is refused by name.
func TestLinearTeamsList_RefusedBodies(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)

	assertOneFault(t, doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{"q": "eng"}),
		http.StatusBadRequest, "UNKNOWN_FIELD", "q")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{"page_size": 0}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{"page_size": 201}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{"team": "x"}),
		http.StatusBadRequest, "UNKNOWN_FIELD", "team")
	if fake.Calls() != 0 {
		t.Errorf("a refused body reached Linear %d times", fake.Calls())
	}
	assertSearchCap(t, s, linearTeamsListPath, map[string]any{}, fake.Calls)
}

// TestLinearCatalog_UpstreamFailure: a failed Linear call is a 502 on every
// read, never an empty page or a 404.
func TestLinearCatalog_UpstreamFailure(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	fake.SetFailing(true)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"teams list":  doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{}),
		"team":        doJSON(t, s, http.MethodGet, linearTeamPath(linearTeamEng), nil),
		"states list": doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamEng), map[string]any{}),
		"state":       doJSON(t, s, http.MethodGet, linearStatePath(linearTeamEng, linearStateDone), nil),
		"users list":  doJSON(t, s, http.MethodPost, linearUsersListPath, map[string]any{}),
		"user":        doJSON(t, s, http.MethodGet, linearUserPath(linearUserAda), nil),
	} {
		t.Run(name, func(t *testing.T) {
			assertOneFault(t, rec, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "")
		})
	}
}

// TestLinearCatalog_MalformedOrgIsNotFound: an org id that is not one is a 404
// that never reaches Linear. Membership itself is N=1 in local mode; the
// Postgres test covers a caller outside the org.
func TestLinearCatalog_MalformedOrgIsNotFound(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	org := linearOrgPath("not-an-org")
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"teams list":  doJSON(t, s, http.MethodPost, org+"/teams/list", map[string]any{}),
		"team":        doJSON(t, s, http.MethodGet, org+"/teams/"+linearTeamEng, nil),
		"states list": doJSON(t, s, http.MethodPost, org+"/teams/"+linearTeamEng+"/states/list", map[string]any{}),
		"state":       doJSON(t, s, http.MethodGet, org+"/teams/"+linearTeamEng+"/states/"+linearStateDone, nil),
		"users list":  doJSON(t, s, http.MethodPost, org+"/users/list", map[string]any{}),
		"user":        doJSON(t, s, http.MethodGet, org+"/users/"+linearUserAda, nil),
	} {
		t.Run(name, func(t *testing.T) {
			assertOneFault(t, rec, http.StatusNotFound, "NOT_FOUND", "")
		})
	}
	if fake.Calls() != 0 {
		t.Errorf("a malformed org reached Linear %d times", fake.Calls())
	}
}

// TestLinearTeamGet: one team by id; one Linear cannot show this credential,
// and an id that is not a Linear id at all, are both a 404.
func TestLinearTeamGet(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)

	rec := doJSON(t, s, http.MethodGet, linearTeamPath(linearTeamOps), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET team = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got linearTeamJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := (linearTeamJSON{ID: linearTeamOps, Key: "OPS", Name: "Operations", Private: true}); got != want {
		t.Errorf("team = %+v, want %+v", got, want)
	}

	assertOneFault(t, doJSON(t, s, http.MethodGet, linearTeamPath(linearTeamGhost), nil),
		http.StatusNotFound, "NOT_FOUND", "")
	before := fake.Calls()
	for _, id := range []string{"ENG", strings.ToUpper(linearTeamEng)} {
		assertOneFault(t, doJSON(t, s, http.MethodGet, linearTeamPath(id), nil),
			http.StatusNotFound, "NOT_FOUND", "")
	}
	if fake.Calls() != before {
		t.Error("a path id that is not a Linear id reached Linear")
	}
}

// TestLinearStatesList_TokenIsBoundToTheTeam: a team's whole workflow pages in
// board order, though Linear serves it in another, with each state's position,
// total_count null, under a token that wraps an offset into that team's
// workflow and cannot page another's.
func TestLinearStatesList_TokenIsBoundToTheTeam(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	first := httpxtest.DecodeList[linearStateJSON](t, doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamEng),
		map[string]any{"page_size": 4}))
	if first.NextPageToken == "" {
		t.Fatal("page 1 carried no next_page_token")
	}
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamOps),
		map[string]any{"page_size": 4, "page_token": first.NextPageToken}),
		http.StatusBadRequest, "INVALID_PARAM", "page_token")

	second := httpxtest.DecodeList[linearStateJSON](t, doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamEng),
		map[string]any{"page_size": 4, "page_token": first.NextPageToken}))
	if second.NextPageToken != "" || first.TotalCount != nil || second.TotalCount != nil {
		t.Errorf("pages = %+v then %+v, want two pages and a null total", first, second)
	}
	want := make([]linearStateJSON, 0, len(linearFixtureStates))
	for _, st := range linearFixtureStates {
		want = append(want, toLinearStateJSON(st))
	}
	slices.SortFunc(want, func(a, b linearStateJSON) int { return cmp.Compare(a.Position, b.Position) })
	if got := append(first.Items, second.Items...); !slices.Equal(got, want) {
		t.Errorf("states = %+v, want %+v", got, want)
	}
}

func linearStateName(st linearStateJSON) string { return st.Name }

// TestLinearStatesList_OrderAndSearch: two states at one position are ordered
// by id, the search narrows by name before the window and keeps board order, a
// % or an _ in it is the character itself, and a token continues only its own
// search.
func TestLinearStatesList_OrderAndSearch(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	path := linearStatesListPath(linearTeamEng)
	assertSearches(t, s, path, linearStateName, map[string][]string{
		"in":    {"In Progress", "In Review"},
		"O":     {"Backlog", "Todo", "In Progress", "Done"},
		"":      {"Triage", "Backlog", "Todo", "In Progress", "In Review", "Done", "Canceled"},
		"Ghost": {},
	})
	assertTokenBoundToSearch(t, s, path, "o", "in", linearStateName, []string{"Todo"})

	// Two states at one position, served with the greater id first: the tie
	// falls to the id, whatever order Linear serves them in.
	tieA, tieB := fixtureUUID("linear-state-tie-a"), fixtureUUID("linear-state-tie-b")
	first, second := tieA, tieB
	if tieB < tieA {
		first, second = tieB, tieA
	}
	fake.SetStates(linearTeamOps,
		linear.WorkflowState{ID: second, Name: "Second", Type: "started", Position: 1},
		linear.WorkflowState{ID: fixtureUUID("linear-state-pct"), Name: "50% Done", Type: "started", Position: 2},
		linear.WorkflowState{ID: fixtureUUID("linear-state-500"), Name: "500 Done", Type: "started", Position: 3},
		linear.WorkflowState{ID: fixtureUUID("linear-state-und"), Name: "On_Hold", Type: "started", Position: 4},
		linear.WorkflowState{ID: fixtureUUID("linear-state-onx"), Name: "OnXHold", Type: "started", Position: 5},
		linear.WorkflowState{ID: first, Name: "First", Type: "started", Position: 1},
	)
	assertSearches(t, s, linearStatesListPath(linearTeamOps), linearStateName, map[string][]string{
		"":        {"First", "Second", "50% Done", "500 Done", "On_Hold", "OnXHold"},
		"50%":     {"50% Done"},
		"on_hold": {"On_Hold"},
	})
	assertSearchCap(t, s, path, map[string]any{}, fake.Calls)
}

// TestLinearStatesList_Refusals: a team Linear cannot show is a 404, as is a
// path id that is not a Linear id; the body is strict and count-only has no
// answer.
func TestLinearStatesList_Refusals(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)

	assertOneFault(t, doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamGhost), map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")

	before := fake.Calls()
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearStatesListPath("ENG"), map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamEng), map[string]any{"page_size": 0}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamEng), map[string]any{"q": "done"}),
		http.StatusBadRequest, "UNKNOWN_FIELD", "q")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamEng), map[string]any{"search": 7}),
		http.StatusBadRequest, "INVALID_FIELD", "search")
	if fake.Calls() != before {
		t.Errorf("a refused request reached Linear %d times", fake.Calls()-before)
	}
}

// TestLinearStateGet: one state through the team that owns it. A state of
// another team's workflow is a 404 at this team's address, as is one Linear
// does not know.
func TestLinearStateGet(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	opsOnly := linear.WorkflowState{ID: fixtureUUID("linear-state-ops-only"), Name: "Queued", Type: "unstarted", Position: 1}
	fake.SetStates(linearTeamOps, opsOnly)

	rec := doJSON(t, s, http.MethodGet, linearStatePath(linearTeamEng, linearStateDoing), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET state = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got linearStateJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := (linearStateJSON{ID: linearStateDoing, Name: "In Progress", Type: "started", Position: 3}); got != want {
		t.Errorf("state = %+v, want %+v", got, want)
	}

	if rec := doJSON(t, s, http.MethodGet, linearStatePath(linearTeamOps, opsOnly.ID), nil); rec.Code != http.StatusOK {
		t.Errorf("GET the state at its own team = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertOneFault(t, doJSON(t, s, http.MethodGet, linearStatePath(linearTeamEng, opsOnly.ID), nil),
		http.StatusNotFound, "NOT_FOUND", "")
	assertOneFault(t, doJSON(t, s, http.MethodGet, linearStatePath(linearTeamEng, linearStateUnknown), nil),
		http.StatusNotFound, "NOT_FOUND", "")
	assertOneFault(t, doJSON(t, s, http.MethodGet, linearStatePath(linearTeamEng, "Done"), nil),
		http.StatusNotFound, "NOT_FOUND", "")
}

// TestLinearCatalog_NotConnected: with no service credential every read is a
// 409 naming the fix, never an empty list or a 404 for a team that may exist.
func TestLinearCatalog_NotConnected(t *testing.T) {
	s, _ := newServerWithUnconnectedLinear(t, linearFixtureEng)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"teams list":  doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{}),
		"team":        doJSON(t, s, http.MethodGet, linearTeamPath(linearTeamEng), nil),
		"states list": doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamEng), map[string]any{}),
		"state":       doJSON(t, s, http.MethodGet, linearStatePath(linearTeamEng, linearStateDone), nil),
		"users list":  doJSON(t, s, http.MethodPost, linearUsersListPath, map[string]any{}),
		"user":        doJSON(t, s, http.MethodGet, linearUserPath(linearUserAda), nil),
	} {
		t.Run(name, func(t *testing.T) {
			assertOneFault(t, rec, http.StatusConflict, "NOT_CONFIGURED", "")
		})
	}
}
