package server

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The team's Jira project write names a tracked project by its registry id and
// a project being added by Jira's id, never by key. These pin that addressing:
// what each identifier may name, and that a key change in Jira cannot make a
// tracked project read as a new one.

// putJiraProjectElements sends elements to the default team's jira-projects
// PUT exactly as given.
func putJiraProjectElements(t *testing.T, s *Server, elements ...map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), map[string]any{"jira_projects": elements})
}

// A project renamed in Jira, before the poll has moved its registry row, is
// still the tracked project: the client names it by its registry id, so its
// absent rules keep what is stored and Jira is not asked. Named by the Jira id
// its renamed catalog entry carries, it is refused as the tracked project it
// is, rather than written as a new one over the team's rules.
func TestJiraProjectsPut_TrackedProjectKeepsItsRulesAcrossARenameThePollHasNotApplied(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("arming PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := teamJiraRules(t, s)
	if len(stored) != 1 || !stored[0].Armed() {
		t.Fatalf("rules = %+v, want SKY armed", stored)
	}
	// Jira renames SKY to SKYL; the registry row still reads SKY.
	fake.SetKeys("SKYL")
	calls := fake.Calls()

	rec := putJiraProjectElements(t, s, map[string]any{"id": stored[0].JiraProjectID})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT naming the tracked project by id = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if fake.Calls() != calls {
		t.Errorf("a write naming only the tracked project reached Jira %d times", fake.Calls()-calls)
	}
	after, _ := teamJiraRules(t, s)
	if len(after) != 1 || after[0].JiraProjectID != stored[0].JiraProjectID || !after[0].Armed() ||
		!slices.Equal(after[0].PickupMembers, stored[0].PickupMembers) ||
		after[0].InProgressCanonical != stored[0].InProgressCanonical ||
		after[0].DoneCanonical != stored[0].DoneCanonical {
		t.Errorf("rules after the write = %+v, want the stored rules kept", after)
	}

	rec = putJiraProjectElements(t, s, map[string]any{"jira_id": stored[0].ProjectExternalID})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT naming the tracked project by its Jira id = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Field != "jira_projects[0].jira_id" ||
		!strings.Contains(items[0].Message, stored[0].JiraProjectID) {
		t.Errorf("errors = %+v, want one on jira_projects[0].jira_id naming the tracked project's id", items)
	}
	if again, _ := teamJiraRules(t, s); len(again) != 1 || !again[0].Armed() {
		t.Errorf("rules after the refused write = %+v, want SKY still armed", again)
	}
}

// An id names a project this team tracks and nothing else: a registry row the
// team does not track, or no row at all, is refused, naming the field.
func TestJiraProjectsPut_IDMustNameATrackedProject(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY", "OPS")
	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	_, site := teamJiraRules(t, s)
	other, err := s.allStores.JiraProjects.FindOrCreateSystem(t.Context(), runmode.LocalDefaultOrgID, site, fakeJiraProjectID("OPS"), "OPS", "OPS Project")
	if err != nil {
		t.Fatalf("register OPS: %v", err)
	}

	for name, id := range map[string]string{"untracked registry row": other.ID, "no such row": "not-a-row"} {
		t.Run(name, func(t *testing.T) {
			rec := putJiraProjectElements(t, s, map[string]any{"id": id})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Reason != "INVALID_FIELD" || items[0].Field != "jira_projects[0].id" {
				t.Errorf("errors = %+v, want one INVALID_FIELD on jira_projects[0].id", items)
			}
			if got := teamJiraProjectKeys(t, s); !slices.Equal(got, []string{"SKY"}) {
				t.Errorf("stored keys = %v, want [SKY]", got)
			}
		})
	}
}

// Each element names exactly one project by exactly one identifier, and a key
// is not one of them.
func TestJiraProjectsPut_ElementNamesOneProjectByOneIdentifier(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")

	cases := map[string]struct {
		element map[string]any
		reason  string
		field   string
	}{
		"neither":  {map[string]any{"pickup": validPickup()}, "MISSING_FIELD", "jira_projects[0]"},
		"both":     {map[string]any{"id": "x", "jira_id": fakeJiraProjectID("SKY")}, "INVALID_FIELD", "jira_projects[0]"},
		"a key":    {map[string]any{"key": "SKY"}, "UNKNOWN_FIELD", ""},
		"blank id": {map[string]any{"id": "", "jira_id": ""}, "MISSING_FIELD", "jira_projects[0]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := putJiraProjectElements(t, s, tc.element)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			items := decodeErrorItems(t, rec)
			if len(items) != 1 || items[0].Reason != tc.reason || (tc.field != "" && items[0].Field != tc.field) {
				t.Errorf("errors = %+v, want one %s on %q", items, tc.reason, tc.field)
			}
		})
	}
	if fake.Calls() != 0 {
		t.Errorf("a malformed element reached Jira %d times", fake.Calls())
	}
}

// A tracked project the poll has not resolved against Jira carries no Jira id,
// so the caller cannot tell that the project it adds by Jira id is that one.
// Registering the added project resolves the tracked row onto it, which would
// put one project in the set twice: the write is refused, naming the added
// element, and nothing it registered is kept.
func TestJiraProjectsPut_RefusesAnAddedProjectThatIsAnUnresolvedTrackedOne(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")
	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := teamJiraRules(t, s)
	if _, err := s.db.Exec(`UPDATE jira_projects SET external_id = NULL WHERE id = ?`, stored[0].JiraProjectID); err != nil {
		t.Fatalf("unresolve SKY: %v", err)
	}

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
	var externalID *string
	if err := s.db.QueryRow(`SELECT external_id FROM jira_projects WHERE id = ?`, stored[0].JiraProjectID).Scan(&externalID); err != nil {
		t.Fatalf("read SKY's row: %v", err)
	}
	if externalID != nil {
		t.Errorf("SKY's row external_id = %q, want it unresolved: the refused write keeps nothing", *externalID)
	}
}
