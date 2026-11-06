package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

func linearLabelsPath(teamID string) string { return linearTeamPath(teamID) + "/labels" }

// A team's labels page by name, each carrying its group, and a group is marked
// as one even when the label naming it is on another page.
func TestLinearLabelsList_PagesTheTeamsLabels(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng)

	got, _ := readTwoPages[linearLabelJSON](t, s, linearLabelsPath(linearTeamEng)+"/list")
	want := []linearLabelJSON{
		{ID: linearLabelBug, Name: "Bug"},
		{ID: linearLabelDifficulty, Name: "Difficulty", IsGroup: true},
		{ID: linearLabelHard, Name: "Hard", Group: &linearLabelRefJSON{ID: linearLabelDifficulty, Name: "Difficulty"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %+v, want %+v", got, want)
	}

	assertOneFault(t, doJSON(t, s, http.MethodPost, linearLabelsPath(linearTeamGhost)+"/list", map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearLabelsPath(linearTeamEng)+"/list", map[string]any{"page_size": 0}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearLabelsPath("ENG")+"/list", map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")
}

func linearLabelName(l linearLabelJSON) string {
	if l.Group != nil {
		return l.Group.Name + " / " + l.Name
	}
	return l.Name
}

// The search narrows by a label's name and its group's, and is_group to the
// groups or to the labels an issue can carry, both before the window: a group
// stays a group when the labels in it are narrowed away. A % or an _ is the
// character itself, and a token continues only its own narrowing.
func TestLinearLabelsList_Narrowing(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	path := linearLabelsPath(linearTeamEng) + "/list"
	for _, c := range []struct {
		body map[string]any
		want []string
	}{
		{map[string]any{"is_group": true}, []string{"Difficulty"}},
		{map[string]any{"is_group": false}, []string{"Bug", "Difficulty / Hard"}},
		{map[string]any{"search": "DIFF"}, []string{"Difficulty", "Difficulty / Hard"}},
		{map[string]any{"search": "diff", "is_group": false}, []string{"Difficulty / Hard"}},
		{map[string]any{"search": "diff", "is_group": true}, []string{"Difficulty"}},
		{map[string]any{"search": "bug", "is_group": true}, []string{}},
		{map[string]any{"is_group": nil}, []string{"Bug", "Difficulty", "Difficulty / Hard"}},
	} {
		if got := listNames(t, s, path, c.body, linearLabelName); !slices.Equal(got, c.want) {
			t.Errorf("%v = %q, want %q", c.body, got, c.want)
		}
	}

	first := httpxtest.DecodeList[linearLabelJSON](t, doJSON(t, s, http.MethodPost, path, map[string]any{"is_group": false, "page_size": 1}))
	if first.NextPageToken == "" {
		t.Fatal("page 1 carried no next_page_token")
	}
	for _, body := range []map[string]any{{}, {"is_group": true}, {"is_group": false, "search": "hard"}} {
		body["page_size"], body["page_token"] = 1, first.NextPageToken
		assertOneFault(t, doJSON(t, s, http.MethodPost, path, body), http.StatusBadRequest, "INVALID_PARAM", "page_token")
	}
	got := listNames(t, s, path, map[string]any{"is_group": false, "page_size": 1, "page_token": first.NextPageToken}, linearLabelName)
	if !slices.Equal(got, []string{"Difficulty / Hard"}) {
		t.Errorf("page 2 = %q, want the label in the group", got)
	}
	assertTokenBoundToSearch(t, s, path, "diff", "hard", linearLabelName, []string{"Difficulty / Hard"})

	fake.SetLabels(linearTeamEng,
		linear.Label{ID: fixtureUUID("l-pct"), Name: "100%"}, linear.Label{ID: fixtureUUID("l-zero"), Name: "1000"},
		linear.Label{ID: fixtureUUID("l-und"), Name: "needs_review"}, linear.Label{ID: fixtureUUID("l-dash"), Name: "needs-review"})
	assertSearches(t, s, path, linearLabelName, map[string][]string{
		"100%":         {"100%"},
		"needs_review": {"needs_review"},
	})
	assertSearchCap(t, s, path, map[string]any{"is_group": false}, fake.Calls)
}

// is_group takes a boolean and nothing else, and the old name of the search is
// not a field; neither reaches Linear.
func TestLinearLabelsList_RefusedBodies(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	path := linearLabelsPath(linearTeamEng) + "/list"
	for _, v := range []any{"true", 1, []any{true}} {
		assertOneFault(t, doJSON(t, s, http.MethodPost, path, map[string]any{"is_group": v}),
			http.StatusBadRequest, "INVALID_FIELD", "is_group")
	}
	assertOneFault(t, doJSON(t, s, http.MethodPost, path, map[string]any{"q": "bug"}),
		http.StatusBadRequest, "UNKNOWN_FIELD", "q")
	if fake.Calls() != 0 {
		t.Errorf("a refused body reached Linear %d times", fake.Calls())
	}
}

// One label through a team that can use it. A label of another team only is a
// 404 at this team's address.
func TestLinearLabelGet(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	opsOnly := linear.Label{ID: fixtureUUID("linear-label-ops-only"), Name: "Incident"}
	fake.SetLabels(linearTeamOps, opsOnly)

	rec := doJSON(t, s, http.MethodGet, linearLabelsPath(linearTeamEng)+"/"+linearLabelHard, nil)
	var got linearLabelJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.ID != linearLabelHard || got.Group == nil || got.Group.ID != linearLabelDifficulty {
		t.Errorf("GET label = %d %+v, want 200 Hard in Difficulty", rec.Code, got)
	}
	for _, id := range []string{opsOnly.ID, "Bug"} {
		assertOneFault(t, doJSON(t, s, http.MethodGet, linearLabelsPath(linearTeamEng)+"/"+id, nil), http.StatusNotFound, "NOT_FOUND", "")
	}
}
