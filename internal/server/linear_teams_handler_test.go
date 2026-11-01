package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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

// TestLinearTeamsList_TokenIsFingerprintedOnQ: the search is applied upstream
// and pages through Linear's cursor with total_count null. A token minted for
// one search cannot page another's, and case alone does not make two searches.
func TestLinearTeamsList_TokenIsFingerprintedOnQ(t *testing.T) {
	alphaBeta := linear.Team{ID: fixtureUUID("t2"), Key: "ABB", Name: "Alpha Beta", Private: true}
	s, _ := newServerWithLinearCatalog(t,
		linear.Team{ID: fixtureUUID("t1"), Key: "AAA", Name: "Alpha"},
		alphaBeta,
		linear.Team{ID: fixtureUUID("t3"), Key: "ZZZ", Name: "Zed"},
	)
	first := decodeList[linearTeamJSON](t, doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"q": "a", "page_size": 1}))
	if got := linearTeamKeysOf(first.Items); got != "AAA" || first.NextPageToken == "" || first.TotalCount != nil {
		t.Fatalf("page 1 = %s (token %q, total %v), want AAA with a token and a null total", got, first.NextPageToken, first.TotalCount)
	}

	rec := doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"q": "alpha", "page_size": 1, "page_token": first.NextPageToken})
	assertOneFault(t, rec, http.StatusBadRequest, "INVALID_PARAM", "page_token")

	// ZZZ does not match, so the second and last page is ABB.
	second := decodeList[linearTeamJSON](t, doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"q": "A", "page_size": 1, "page_token": first.NextPageToken}))
	want := linearTeamJSON{ID: alphaBeta.ID, Key: "ABB", Name: "Alpha Beta", Private: true}
	if len(second.Items) != 1 || second.Items[0] != want || second.NextPageToken != "" {
		t.Errorf("page 2 in another case = %+v (token %q), want only %+v", second.Items, second.NextPageToken, want)
	}
}

// TestLinearTeamsList_RefusedBodies: count-only has no answer on a proxy list,
// and a field the route does not take is refused by name.
func TestLinearTeamsList_RefusedBodies(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)

	assertOneFault(t, doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{"page_size": 0}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{"page_size": 201}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{"team": "x"}),
		http.StatusBadRequest, "UNKNOWN_FIELD", "team")
	if fake.Calls() != 0 {
		t.Errorf("a refused body reached Linear %d times", fake.Calls())
	}
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
// Linear's order with each state's position, total_count null, under a token
// that wraps a cursor into that team's workflow and cannot page another's.
func TestLinearStatesList_TokenIsBoundToTheTeam(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	first := decodeList[linearStateJSON](t, doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamEng),
		map[string]any{"page_size": 4}))
	if first.NextPageToken == "" {
		t.Fatal("page 1 carried no next_page_token")
	}
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamOps),
		map[string]any{"page_size": 4, "page_token": first.NextPageToken}),
		http.StatusBadRequest, "INVALID_PARAM", "page_token")

	second := decodeList[linearStateJSON](t, doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamEng),
		map[string]any{"page_size": 4, "page_token": first.NextPageToken}))
	if second.NextPageToken != "" || first.TotalCount != nil || second.TotalCount != nil {
		t.Errorf("pages = %+v then %+v, want two pages and a null total", first, second)
	}
	want := make([]linearStateJSON, 0, len(linearFixtureStates))
	for _, st := range linearFixtureStates {
		want = append(want, toLinearStateJSON(st))
	}
	if got := append(first.Items, second.Items...); !slices.Equal(got, want) {
		t.Errorf("states = %+v, want %+v", got, want)
	}
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
