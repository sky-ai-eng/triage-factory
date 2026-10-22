package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

func linearLabelsListPath(teamID string) string { return linearTeamPath(teamID) + "/labels/list" }

// TestLinearLabelsList_PagesTheTeamsLabels walks a team's labels a page at a
// time, by name: each carries its group, and a group is marked as one even
// when the label naming it is on another page.
func TestLinearLabelsList_PagesTheTeamsLabels(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng)

	first := decodeList[linearLabelJSON](t, doJSON(t, s, http.MethodPost, linearLabelsListPath(linearTeamEng), map[string]any{"page_size": 2}))
	if first.TotalCount != nil || first.NextPageToken == "" {
		t.Fatalf("page 1 = %+v, want a null total and a next page", first)
	}
	second := decodeList[linearLabelJSON](t, doJSON(t, s, http.MethodPost, linearLabelsListPath(linearTeamEng),
		map[string]any{"page_size": 2, "page_token": first.NextPageToken}))
	got := append(first.Items, second.Items...)
	want := []linearLabelJSON{
		{ID: linearLabelBug, Name: "Bug"},
		{ID: linearLabelDifficulty, Name: "Difficulty", IsGroup: true},
		{ID: linearLabelHard, Name: "Hard", Group: &linearLabelRefJSON{ID: linearLabelDifficulty, Name: "Difficulty"}},
	}
	if len(got) != len(want) || second.NextPageToken != "" {
		t.Fatalf("labels = %+v (next %q), want %+v", got, second.NextPageToken, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.ID != w.ID || g.Name != w.Name || g.IsGroup != w.IsGroup || (g.Group == nil) != (w.Group == nil) ||
			(g.Group != nil && *g.Group != *w.Group) {
			t.Errorf("label %d = %+v (group %+v), want %+v (group %+v)", i, g, g.Group, w, w.Group)
		}
	}

	assertOneFault(t, doJSON(t, s, http.MethodPost, linearLabelsListPath(linearTeamGhost), map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearLabelsListPath(linearTeamEng), map[string]any{"page_size": 0}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertOneFault(t, doJSON(t, s, http.MethodPost, linearLabelsListPath("ENG"), map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")
}

// TestLinearLabelGet: one label through a team that can use it. A label of
// another team only is a 404 at this team's address.
func TestLinearLabelGet(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	opsOnly := linear.Label{ID: fixtureUUID("linear-label-ops-only"), Name: "Incident"}
	fake.SetLabels(linearTeamOps, opsOnly)

	rec := doJSON(t, s, http.MethodGet, linearTeamPath(linearTeamEng)+"/labels/"+linearLabelHard, nil)
	var got linearLabelJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.ID != linearLabelHard || got.Group == nil || got.Group.ID != linearLabelDifficulty {
		t.Errorf("GET label = %d %+v, want 200 Hard in Difficulty", rec.Code, got)
	}
	assertOneFault(t, doJSON(t, s, http.MethodGet, linearTeamPath(linearTeamEng)+"/labels/"+opsOnly.ID, nil),
		http.StatusNotFound, "NOT_FOUND", "")
	assertOneFault(t, doJSON(t, s, http.MethodGet, linearTeamPath(linearTeamEng)+"/labels/Bug", nil),
		http.StatusNotFound, "NOT_FOUND", "")
}
