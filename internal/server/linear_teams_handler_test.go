package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

const linearTeamsListPath = "/api/linear/teams/list"

func linearStatesPath(query string) string { return "/api/linear/states" + query }

func linearTeamKeysOf(items []linearTeamJSON) string {
	keys := make([]string, 0, len(items))
	for _, t := range items {
		keys = append(keys, t.Key)
	}
	return strings.Join(keys, ",")
}

// assertLinearFault fails unless rec answered status with exactly one error, of
// this reason and naming this field ("" for a fault with no field).
func assertLinearFault(t *testing.T, rec *httptest.ResponseRecorder, status int, reason, field string) {
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

// TestLinearTeamsList_ProxyPagingRoundTrip walks the picker list the way a
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
	assertLinearFault(t, rec, http.StatusBadRequest, "INVALID_PARAM", "page_token")

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

	assertLinearFault(t, doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{"page_size": 0}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertLinearFault(t, doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{"page_size": 201}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertLinearFault(t, doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{"team": "x"}),
		http.StatusBadRequest, "UNKNOWN_FIELD", "team")
	if fake.Calls() != 0 {
		t.Errorf("a refused body reached Linear %d times", fake.Calls())
	}
}

// TestLinearTeamsList_NotConnected: no service credential is a 409 naming the
// fix, not an empty list.
func TestLinearTeamsList_NotConnected(t *testing.T) {
	s, _ := newServerWithUnconnectedLinear(t, linearFixtureEng)
	rec := doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{})
	assertLinearFault(t, rec, http.StatusConflict, "NOT_CONFIGURED", "")
}

// TestLinearTeamsList_UpstreamFailure is a 502, never an empty page.
func TestLinearTeamsList_UpstreamFailure(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	fake.SetFailing(true)
	assertLinearFault(t, doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{}),
		http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "")
}

// TestLinearStates_SortedByPosition: one team's states, in board order, each
// with the type the picker pre-arms from.
func TestLinearStates_SortedByPosition(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng)

	rec := doJSON(t, s, http.MethodGet, linearStatesPath("?team="+linearTeamEng), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET states = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got []linearStateJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var names []string
	for _, st := range got {
		names = append(names, st.Name)
	}
	if strings.Join(names, ",") != "Triage,Backlog,Todo,In Progress,In Review,Done,Canceled" {
		t.Errorf("states = %v, want board order", names)
	}
	if got[0] != (linearStateJSON{ID: linearStateTriage, Name: "Triage", Type: "triage", Position: 0}) {
		t.Errorf("first state = %+v", got[0])
	}
}

// TestLinearStates_TeamParamIsStrict: the team is required, a Linear id, and
// named exactly once, and the read takes nothing else.
func TestLinearStates_TeamParamIsStrict(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	for name, query := range map[string]string{
		"missing":          "",
		"empty":            "?team=",
		"not a uuid":       "?team=ENG",
		"upper-case uuid":  "?team=" + strings.ToUpper(linearTeamEng),
		"two teams":        "?team=" + linearTeamEng + "&team=" + linearTeamOps,
		"unknown param":    "?team=" + linearTeamEng + "&project=ENG",
		"only the unknown": "?project=ENG",
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodGet, linearStatesPath(query), nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("GET %q = %d, want 400; body=%s", query, rec.Code, rec.Body.String())
			}
			for _, item := range decodeErrorItems(t, rec) {
				if item.Reason != "INVALID_PARAM" {
					t.Errorf("reason = %q, want INVALID_PARAM", item.Reason)
				}
				if item.Field != "team" && item.Field != "project" {
					t.Errorf("field = %q, want the parameter named", item.Field)
				}
			}
		})
	}
	if fake.Calls() != 0 {
		t.Errorf("a refused read reached Linear %d times", fake.Calls())
	}
}

// TestLinearStates_NotConnected mirrors the list's 409.
func TestLinearStates_NotConnected(t *testing.T) {
	s, _ := newServerWithUnconnectedLinear(t, linearFixtureEng)
	assertLinearFault(t, doJSON(t, s, http.MethodGet, linearStatesPath("?team="+linearTeamEng), nil),
		http.StatusConflict, "NOT_CONFIGURED", "")
}
