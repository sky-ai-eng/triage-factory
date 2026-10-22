package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
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
