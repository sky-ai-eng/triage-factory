package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// TestLinearTeamsList_ProxyPagingRoundTrip walks the team list the way a
// client does, first page then the token it was handed, and pins the proxy
// contract: the rows page through Linear's own cursor, and total_count is null.
func TestLinearTeamsList_ProxyPagingRoundTrip(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t,
		linear.Team{ID: fixtureUUID("t1"), Key: "AAA", Name: "Alpha"},
		linear.Team{ID: fixtureUUID("t2"), Key: "BBB", Name: "Beta"},
		linear.Team{ID: fixtureUUID("t3"), Key: "CCC", Name: "Gamma", Private: true},
	)

	first := decodeList[linearTeamJSON](t, doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"page_size": 2}))
	if got := linearTeamKeysOf(first.Items); got != "AAA,BBB" {
		t.Fatalf("page 1 = %s, want AAA,BBB", got)
	}
	if first.TotalCount != nil {
		t.Errorf("total_count = %d, want null — a proxy list cannot count itself", *first.TotalCount)
	}
	if first.NextPageToken == "" {
		t.Fatal("page 1 carried no next_page_token with a team still to serve")
	}

	second := decodeList[linearTeamJSON](t, doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"page_size": 2, "page_token": first.NextPageToken}))
	if len(second.Items) != 1 {
		t.Fatalf("page 2 = %+v, want the one remaining team", second.Items)
	}
	if want := (linearTeamJSON{ID: fixtureUUID("t3"), Key: "CCC", Name: "Gamma", Private: true}); second.Items[0] != want {
		t.Errorf("page 2 item = %+v, want %+v", second.Items[0], want)
	}
	if second.NextPageToken != "" {
		t.Errorf("last page minted a next_page_token (%q)", second.NextPageToken)
	}
}

// TestLinearTeamsList_FiltersOnQ: the search box is applied upstream, against
// the key and the name.
func TestLinearTeamsList_FiltersOnQ(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps,
		linear.Team{ID: fixtureUUID("t-design"), Key: "DES", Name: "Design Ops"})

	page := decodeList[linearTeamJSON](t, doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"q": "ops"}))
	if got := linearTeamKeysOf(page.Items); got != "OPS,DES" {
		t.Errorf("filtered page = %s, want OPS,DES", got)
	}
}

// TestLinearTeamsList_TokenIsFingerprintedOnQ: a token minted for one search
// cannot page another's results, and case alone does not make two searches.
func TestLinearTeamsList_TokenIsFingerprintedOnQ(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t,
		linear.Team{ID: fixtureUUID("t1"), Key: "AAA", Name: "Alpha"},
		linear.Team{ID: fixtureUUID("t2"), Key: "ABB", Name: "Alpha Beta"},
	)
	first := decodeList[linearTeamJSON](t, doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"q": "a", "page_size": 1}))
	if first.NextPageToken == "" {
		t.Fatal("page 1 carried no next_page_token")
	}

	rec := doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"q": "alpha", "page_size": 1, "page_token": first.NextPageToken})
	assertOneFault(t, rec, http.StatusBadRequest, "INVALID_PARAM", "page_token")

	rec = doJSON(t, s, http.MethodPost, linearTeamsListPath,
		map[string]any{"q": "A", "page_size": 1, "page_token": first.NextPageToken})
	if rec.Code != http.StatusOK {
		t.Errorf("same search in another case = %d, want 200; body=%s", rec.Code, rec.Body.String())
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

// TestLinearCatalog_NotConnected: with no service credential every read is a
// 409 naming the fix, never an empty list or a 404 for a team that may exist.
func TestLinearCatalog_NotConnected(t *testing.T) {
	s, _ := newServerWithUnconnectedLinear(t, linearFixtureEng)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"teams list":  doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{}),
		"team":        doJSON(t, s, http.MethodGet, linearTeamPath(linearTeamEng), nil),
		"states list": doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamEng), map[string]any{}),
		"state":       doJSON(t, s, http.MethodGet, linearStatePath(linearTeamEng, linearStateDone), nil),
	} {
		t.Run(name, func(t *testing.T) {
			assertOneFault(t, rec, http.StatusConflict, "NOT_CONFIGURED", "")
		})
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

// TestLinearStatesList_PagesTheTeamsWorkflow walks a team's states a page at
// a time. The pages together are the whole workflow, in Linear's order with
// each state's position alongside, and total_count is null.
func TestLinearStatesList_PagesTheTeamsWorkflow(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng)

	var got []linearStateJSON
	token := ""
	for pages := 0; ; pages++ {
		if pages > len(linearFixtureStates) {
			t.Fatal("the states list never stopped minting tokens")
		}
		body := map[string]any{"page_size": 3}
		if token != "" {
			body["page_token"] = token
		}
		page := decodeList[linearStateJSON](t, doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamEng), body))
		if len(page.Items) > 3 {
			t.Fatalf("page of %d, want at most 3", len(page.Items))
		}
		if page.TotalCount != nil {
			t.Errorf("total_count = %d, want null", *page.TotalCount)
		}
		got = append(got, page.Items...)
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	want := make([]linearStateJSON, 0, len(linearFixtureStates))
	for _, st := range linearFixtureStates {
		want = append(want, toLinearStateJSON(st))
	}
	if len(got) != len(want) {
		t.Fatalf("states = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("state %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestLinearStatesList_TokenIsBoundToTheTeam: a token wraps a cursor into one
// team's workflow, so it cannot page another team's.
func TestLinearStatesList_TokenIsBoundToTheTeam(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	first := decodeList[linearStateJSON](t, doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamEng),
		map[string]any{"page_size": 2}))
	if first.NextPageToken == "" {
		t.Fatal("page 1 carried no next_page_token")
	}
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamOps),
		map[string]any{"page_size": 2, "page_token": first.NextPageToken}),
		http.StatusBadRequest, "INVALID_PARAM", "page_token")
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
