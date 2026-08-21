package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestJiraStatusRules_SQLite_LegacyNameOnlyRow reads a row in the shape written
// before statuses were identified: members as a JSON array of bare names, and
// the canonical column holding the display name itself rather than a document.
//
// Those rows are live data, not a migration waiting to run — resolving an id
// means asking Jira, and nothing does that on a team's behalf outside a poll
// cycle — so they have to keep reading, and keep polling on the name fallback,
// until the team next saves that rule through the API.
func TestJiraStatusRules_SQLite_LegacyNameOnlyRow(t *testing.T) {
	conn := openSQLiteForTest(t)
	if _, err := conn.Exec(`
		INSERT INTO jira_project_status_rules (
			team_id, project_key,
			pickup_members, in_progress_members, in_progress_canonical,
			done_members, done_canonical
		) VALUES (?, 'SKY',
			'["To Do","Backlog"]',
			'["In Progress"]', 'In Progress',
			'["Done"]', 'Done')
	`, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	got, err := sqlite.New(conn).JiraStatusRules.ListForTeamSystem(t.Context(), runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rules = %+v, want the one legacy row", got)
	}
	rule := got[0]
	for _, ref := range append(append([]domain.JiraStatusRef{}, rule.PickupMembers...), rule.DoneMembers...) {
		if ref.ID != "" {
			t.Errorf("legacy member %+v carries an id nothing could have resolved", ref)
		}
	}
	if names := domain.JiraStatusNames(rule.PickupMembers); len(names) != 2 || names[0] != "To Do" || names[1] != "Backlog" {
		t.Errorf("legacy pickup = %v, want [To Do Backlog]", names)
	}
	if rule.InProgressCanonical != (domain.JiraStatusRef{Name: "In Progress"}) {
		t.Errorf("legacy in-progress canonical = %+v, want the bare name", rule.InProgressCanonical)
	}
	if rule.DoneCanonical != (domain.JiraStatusRef{Name: "Done"}) {
		t.Errorf("legacy done canonical = %+v, want the bare name", rule.DoneCanonical)
	}
	// Complete is complete: a legacy row polls exactly as an identified one does.
	if !rule.Armed() {
		t.Error("a complete legacy row should read as armed")
	}
	if !rule.DoneContains(domain.JiraStatusRef{Name: "Done"}) {
		t.Error("membership tests should match a legacy row on the name")
	}
}

// TestJiraStatusRules_SQLite_SavingALegacyRowFillsIDs: the row gains its ids the
// first time it is written back, which is the only thing that ever fills them.
func TestJiraStatusRules_SQLite_SavingALegacyRowFillsIDs(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlite.New(conn)
	if _, err := conn.Exec(`
		INSERT INTO jira_project_status_rules (
			team_id, project_key, pickup_members,
			in_progress_members, in_progress_canonical,
			done_members, done_canonical
		) VALUES (?, 'SKY', '["To Do"]', '["In Progress"]', 'In Progress', '["Done"]', 'Done')
	`, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	identified := []domain.JiraProjectStatusRules{{
		ProjectKey:          "SKY",
		PickupMembers:       []domain.JiraStatusRef{{ID: "10000", Name: "To Do"}},
		InProgressMembers:   []domain.JiraStatusRef{{ID: "10001", Name: "In Progress"}},
		InProgressCanonical: domain.JiraStatusRef{ID: "10001", Name: "In Progress"},
		DoneMembers:         []domain.JiraStatusRef{{ID: "10002", Name: "Done"}},
		DoneCanonical:       domain.JiraStatusRef{ID: "10002", Name: "Done"},
	}}
	if err := stores.JiraStatusRules.ReplaceForTeam(t.Context(), runmode.LocalDefaultTeamID, identified); err != nil {
		t.Fatalf("ReplaceForTeam: %v", err)
	}
	got, err := stores.JiraStatusRules.ListForTeamSystem(t.Context(), runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if len(got) != 1 || got[0].InProgressCanonical.ID != "10001" {
		t.Fatalf("rules = %+v, want the ids persisted", got)
	}
	if got[0].PickupMembers[0].ID != "10000" || got[0].PickupMembers[0].Name != "To Do" {
		t.Errorf("pickup member = %+v, want the id and the name", got[0].PickupMembers[0])
	}
}
