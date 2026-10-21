package server

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// teamJiraRules reads the default team's stored rules on the org's Jira site,
// with the site, straight from the store.
func teamJiraRules(t *testing.T, s *Server) ([]domain.JiraProjectStatusRules, string) {
	t.Helper()
	orgSet, err := s.allStores.Orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	site := domain.EntityScope("jira", orgSet)
	rules, err := s.allStores.JiraStatusRules.ListForTeamSystem(t.Context(), runmode.LocalDefaultTeamID, site)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	return rules, site
}

// TestJiraProjectsPut_RegistersTheProjectByItsJiraID: an added project is
// stored as a reference to the project's registry row, keyed by the Jira id
// the caller named it by, under the key and name Jira gives for it, and the
// team's display order names that row.
func TestJiraProjectsPut_RegistersTheProjectByItsJiraID(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")
	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rules, _ := teamJiraRules(t, s)
	if len(rules) != 1 {
		t.Fatalf("rules = %+v, want the one SKY rule", rules)
	}
	if rules[0].ProjectExternalID != fakeJiraProjectID("SKY") || rules[0].ProjectKey != "SKY" || rules[0].ProjectName != "SKY Project" {
		t.Errorf("rule's project = (%q, %q, %q), want SKY registered under Jira's id and name",
			rules[0].ProjectExternalID, rules[0].ProjectKey, rules[0].ProjectName)
	}
	settings, err := s.allStores.Teams.GetSettingsSystem(t.Context(), runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	if !slices.Equal(settings.JiraProjects, []string{rules[0].JiraProjectID}) {
		t.Errorf("team project order = %v, want the registry row %s", settings.JiraProjects, rules[0].JiraProjectID)
	}
}

// TestJiraProjectsPut_SurvivesAProjectKeyRename: after the poll moves the
// registry row onto the key Jira renamed the project to, the team's set reads
// under the new key with its rules intact, and resending exactly what the
// client read (the project's registry id) asks Jira nothing — the rename
// touched no team's set.
func TestJiraProjectsPut_SurvivesAProjectKeyRename(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	before, _ := teamJiraRules(t, s)
	if len(before) != 1 {
		t.Fatalf("rules = %+v, want one", before)
	}
	if _, err := s.allStores.JiraProjects.RefreshSystem(t.Context(), runmode.LocalDefaultOrgID,
		before[0].JiraProjectID, before[0].ProjectExternalID, "SKYL", "Skyline"); err != nil {
		t.Fatalf("RefreshSystem: %v", err)
	}

	if got := teamJiraProjectKeys(t, s); len(got) != 1 || got[0] != "SKYL" {
		t.Errorf("stored keys after the rename = %v, want [SKYL]", got)
	}
	after, _ := teamJiraRules(t, s)
	if len(after) != 1 || after[0].JiraProjectID != before[0].JiraProjectID ||
		!slices.Equal(after[0].DoneMembers, before[0].DoneMembers) {
		t.Errorf("rules after the rename = %+v, want the same rule under SKYL", after)
	}

	calls := fake.Calls()
	if rec := putJiraProjects(t, s, "SKYL"); rec.Code != http.StatusOK {
		t.Fatalf("resending the renamed set = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if fake.Calls() != calls {
		t.Errorf("resending the stored set reached Jira %d times, want 0", fake.Calls()-calls)
	}
	resent, _ := teamJiraRules(t, s)
	if len(resent) != 1 || resent[0].JiraProjectID != before[0].JiraProjectID {
		t.Errorf("rules after the resend = %+v, want the one rule on the same registry row", resent)
	}
}

// TestJiraProjectsPut_RefusesATrackedProjectByItsJiraID: a stored project the
// registry still holds under a stale key, and the catalog's entry for it under
// its current key, are one Jira project. Naming it by its Jira id as though it
// were being added would track it twice, so the write is refused with the
// stored project named, and the id it is addressed by.
func TestJiraProjectsPut_RefusesATrackedProjectByItsJiraID(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")
	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := teamJiraRules(t, s)
	if _, err := s.allStores.JiraProjects.RefreshSystem(t.Context(), runmode.LocalDefaultOrgID,
		stored[0].JiraProjectID, stored[0].ProjectExternalID, "OLD", ""); err != nil {
		t.Fatalf("RefreshSystem: %v", err)
	}

	rec := putJiraProjects(t, s, "OLD", "SKY")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT naming one project twice = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	items := decodeErrorItems(t, rec)
	if len(items) != 1 || items[0].Field != "jira_projects[1].jira_id" ||
		!strings.Contains(items[0].Message, "OLD") || !strings.Contains(items[0].Message, stored[0].JiraProjectID) {
		t.Errorf("errors = %+v, want one on jira_projects[1].jira_id naming the stored OLD by its id", items)
	}
	if got := teamJiraProjectKeys(t, s); len(got) != 1 || got[0] != "OLD" {
		t.Errorf("stored keys after the refused write = %v, want [OLD]", got)
	}
}

// TestJiraProjectsPut_ConflictsWithAMergeMidSave: while a save that read the
// team's set is asking Jira about an added project, the poll merges a stored
// project's unresolved registry row into the row that carries its Jira id. The
// save names a row that no longer exists, so it is refused as a conflict to
// read again, and nothing it carried is written.
func TestJiraProjectsPut_ConflictsWithAMergeMidSave(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY", "OPS")
	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	stored, site := teamJiraRules(t, s)
	if _, err := s.db.Exec(`UPDATE jira_projects SET external_id = NULL WHERE id = ?`, stored[0].JiraProjectID); err != nil {
		t.Fatalf("unresolve SKY: %v", err)
	}
	if _, err := s.allStores.JiraProjects.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, site, "777", "SKYNEW", "Sky"); err != nil {
		t.Fatalf("FindOrCreate: %v", err)
	}
	var merge sync.Once
	fake.OnCall(func() {
		merge.Do(func() {
			if _, err := s.allStores.JiraProjects.RefreshSystem(t.Context(), runmode.LocalDefaultOrgID,
				stored[0].JiraProjectID, "777", "SKYNEW", "Sky"); err != nil {
				t.Errorf("RefreshSystem: %v", err)
			}
		})
	})

	rec := putJiraProjects(t, s, "SKY", "OPS")
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT racing a merge = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Reason != httpx.ReasonConflict {
		t.Errorf("errors = %+v, want one CONFLICT", items)
	}
	if got := teamJiraProjectKeys(t, s); !slices.Equal(got, []string{"SKYNEW"}) {
		t.Errorf("stored keys after the refused write = %v, want the merged [SKYNEW] alone", got)
	}
}
