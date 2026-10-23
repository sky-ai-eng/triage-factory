package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

var linearTeamVisibilityPath = linearOrgPath(runmode.LocalDefaultOrgID) + "/access/team-visibility"

// trackLinearTeamsNamed makes teams the local team's whole tracked set in the
// test workspace, stored under the key and name given.
func trackLinearTeamsNamed(t *testing.T, s *Server, teams ...linear.Team) {
	t.Helper()
	rules := make([]domain.LinearTeamRules, 0, len(teams))
	for _, team := range teams {
		rules = append(rules, domain.LinearTeamRules{LinearTeamID: team.ID, LinearTeamKey: team.Key, LinearTeamName: team.Name})
	}
	if _, err := s.allStores.LinearTeamRules.ReplaceForTeam(t.Context(), runmode.LocalDefaultTeamID, linearTestWorkspaceID, rules); err != nil {
		t.Fatalf("track linear teams: %v", err)
	}
}

func getLinearTeamVisibility(t *testing.T, s *Server) linearTeamVisibility {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, linearTeamVisibilityPath, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET team-visibility = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got linearTeamVisibility
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// TestLinearTeamVisibility_Cuts: a tracked team the credential reads and is
// private is in private, under Linear's current key and name; one it cannot
// read is in unreadable, under the key and name stored when it was tracked; a
// readable public team is in neither, and an untracked one is not asked about.
func TestLinearTeamVisibility_Cuts(t *testing.T) {
	gone := linear.Team{ID: linearTeamGhost, Key: "GHO", Name: "Ghosts", Private: true}
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng, linear.Team{ID: linearTeamOps, Key: "OPZ", Name: "Ops", Private: true})
	trackLinearTeamsNamed(t, s, linearFixtureOps, gone)

	got := getLinearTeamVisibility(t, s)
	if want := []linearTrackedTeamJSON{{ID: linearTeamOps, Key: "OPZ", Name: "Ops"}}; !equalTrackedTeams(got.Private, want) {
		t.Errorf("private = %+v, want %+v", got.Private, want)
	}
	if want := []linearTrackedTeamJSON{{ID: linearTeamGhost, Key: "GHO", Name: "Ghosts"}}; !equalTrackedTeams(got.Unreadable, want) {
		t.Errorf("unreadable = %+v, want %+v", got.Unreadable, want)
	}
}

// TestLinearTeamVisibility_EmptyCutsAreLists: with nothing tracked, both cuts
// are empty lists rather than null, and Linear is not asked.
func TestLinearTeamVisibility_EmptyCutsAreLists(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	rec := doJSON(t, s, http.MethodGet, linearTeamVisibilityPath, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET team-visibility = %d; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != "{\"private\":[],\"unreadable\":[]}\n" {
		t.Errorf("body = %s, want both cuts as empty lists", body)
	}
	if fake.Calls() != 0 {
		t.Errorf("Linear asked %d times with nothing tracked", fake.Calls())
	}
}

// TestLinearTeamVisibility_Failures: no credential is a 409 naming the fix, and
// a failed Linear read is a 502, never a report that every team is unreadable.
func TestLinearTeamVisibility_Failures(t *testing.T) {
	s, _ := newServerWithUnconnectedLinear(t, linearFixtureEng)
	assertOneFault(t, doJSON(t, s, http.MethodGet, linearTeamVisibilityPath, nil), http.StatusConflict, "NOT_CONFIGURED", "")

	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	trackLinearTeamsNamed(t, s, linearFixtureEng)
	fake.SetFailing(true)
	assertOneFault(t, doJSON(t, s, http.MethodGet, linearTeamVisibilityPath, nil), http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "")
}

func equalTrackedTeams(a, b []linearTrackedTeamJSON) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
