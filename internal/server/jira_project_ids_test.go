package server

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// putJiraProjectElements sends elements to the default team's jira-projects
// PUT exactly as given.
func putJiraProjectElements(t *testing.T, s *Server, elements ...map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), map[string]any{"jira_projects": elements})
}

// A project renamed in Jira before the poll moved its registry row is still
// the tracked project: named by its registry id, its absent rules keep what is
// stored and Jira is not asked. Named by the Jira id its renamed catalog entry
// carries, it is refused as the tracked project it is.
func TestJiraProjectsPut_TrackedProjectKeepsItsRulesAcrossARenameThePollHasNotApplied(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("arming PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := teamJiraRules(t, s)
	if len(stored) != 1 || !stored[0].Armed() {
		t.Fatalf("rules = %+v, want SKY armed", stored)
	}
	fake.SetKeys("SKYL")
	calls := fake.Calls()

	if rec := putJiraProjectElements(t, s, map[string]any{"id": stored[0].JiraProjectID}); rec.Code != http.StatusOK {
		t.Fatalf("PUT naming the tracked project by id = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if fake.Calls() != calls {
		t.Errorf("a write naming only the tracked project reached Jira %d times", fake.Calls()-calls)
	}
	if after, _ := teamJiraRules(t, s); !reflect.DeepEqual(after, stored) {
		t.Errorf("rules after the write = %+v, want the stored %+v kept", after, stored)
	}

	rec := putJiraProjectElements(t, s, map[string]any{"jira_id": stored[0].ProjectExternalID})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT naming the tracked project by its Jira id = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Field != "jira_projects[0].jira_id" ||
		!strings.Contains(items[0].Message, stored[0].JiraProjectID) {
		t.Errorf("errors = %+v, want one on jira_projects[0].jira_id naming the tracked project's id", items)
	}
	if again, _ := teamJiraRules(t, s); !reflect.DeepEqual(again, stored) {
		t.Errorf("rules after the refused write = %+v, want them untouched", again)
	}
}

// An element names exactly one project by exactly one identifier, never a
// key, and an id names a project this team tracks: a registry row it does not
// track, or no row at all, is refused. None of these reaches Jira.
func TestJiraProjectsPut_RefusesAnElementThatNamesNoProjectOfTheTeams(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY", "OPS")
	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	_, site := teamJiraRules(t, s)
	untracked, err := s.allStores.JiraProjects.FindOrCreateSystem(t.Context(), runmode.LocalDefaultOrgID, site, fakeJiraProjectID("OPS"), "OPS", "OPS Project")
	if err != nil {
		t.Fatalf("register OPS: %v", err)
	}
	calls := fake.Calls()

	for name, tc := range map[string]struct {
		element       map[string]any
		reason, field string
	}{
		"untracked registry row": {map[string]any{"id": untracked.ID}, "INVALID_FIELD", "jira_projects[0].id"},
		"no such row":            {map[string]any{"id": "not-a-row"}, "INVALID_FIELD", "jira_projects[0].id"},
		"neither":                {map[string]any{"pickup": validPickup()}, "MISSING_FIELD", "jira_projects[0]"},
		"both":                   {map[string]any{"id": "x", "jira_id": fakeJiraProjectID("SKY")}, "INVALID_FIELD", "jira_projects[0]"},
		"blank id":               {map[string]any{"id": "", "jira_id": ""}, "MISSING_FIELD", "jira_projects[0]"},
		"a key":                  {map[string]any{"key": "SKY"}, "UNKNOWN_FIELD", "key"},
	} {
		t.Run(name, func(t *testing.T) {
			assertOneFault(t, putJiraProjectElements(t, s, tc.element), http.StatusBadRequest, tc.reason, tc.field)
			if got := teamJiraProjectKeys(t, s); !slices.Equal(got, []string{"SKY"}) {
				t.Errorf("stored keys = %v, want [SKY]", got)
			}
		})
	}
	if fake.Calls() != calls {
		t.Errorf("a refused element reached Jira %d times", fake.Calls()-calls)
	}
}

// A tracked project the poll has not resolved carries no Jira id, so adding it
// by Jira id cannot be told apart from it by the caller. Registering the added
// project would resolve the tracked row onto it and put one project in the set
// twice: the write is refused, naming the added element, and keeps nothing it
// registered.
func TestJiraProjectsPut_RefusesAnAddedProjectThatIsAnUnresolvedTrackedOne(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")
	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := teamJiraRules(t, s)
	execSQL(t, s.db, `UPDATE jira_projects SET external_id = NULL WHERE id = ?`, stored[0].JiraProjectID)

	rec := putJiraProjectElements(t, s,
		map[string]any{"id": stored[0].JiraProjectID},
		map[string]any{"jira_id": fakeJiraProjectID("SKY")},
	)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT naming one project twice = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Field != "jira_projects[1].jira_id" ||
		!strings.Contains(items[0].Message, "jira_projects[0]") {
		t.Errorf("errors = %+v, want one on jira_projects[1].jira_id naming jira_projects[0]", items)
	}
	if after, _ := teamJiraRules(t, s); after[0].ProjectExternalID != "" {
		t.Errorf("SKY's row external_id = %q, want it unresolved: the refused write keeps nothing", after[0].ProjectExternalID)
	}
}
