package dbtest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// JiraProjectFixture is what a backend hands RunJiraProjectConformance: the
// registry, the rules that reference it, the teams store for the display
// order a merge rewrites, and an org holding two teams. Both teams start with
// no rules.
type JiraProjectFixture struct {
	Projects db.JiraProjectStore
	Rules    db.JiraStatusRulesStore
	Teams    db.TeamsStore
	Entities db.EntityStore

	OrgID       string
	TeamID      string
	OtherTeamID string

	// SeedUnresolved inserts a registry row with no Jira project id — the
	// state only the SQLite migration leaves a row in, which no store method
	// writes — and returns its id.
	SeedUnresolved func(t *testing.T, site, key string) string

	// SeedIssue inserts an active Jira issue entity keyed key on site that
	// references the registry row jiraProjectID, and returns its id. Raw,
	// because the only rows that reference an unresolved registry row are the
	// ones the SQLite migration wrote.
	SeedIssue func(t *testing.T, site, key, jiraProjectID string) string
}

// JiraProjectFactory builds a fresh fixture per subtest.
type JiraProjectFactory func(t *testing.T) JiraProjectFixture

// The Jira sites the suite registers projects on.
const (
	jiraSite      = "https://acme.atlassian.net"
	jiraOtherSite = "https://jira.other.example"
)

// armedJiraRule is a fully mapped rule for the registry row projectID.
func armedJiraRule(projectID string) domain.JiraProjectStatusRules {
	return domain.JiraProjectStatusRules{
		JiraProjectID:       projectID,
		PickupMembers:       jiraRefs("To Do"),
		InProgressMembers:   jiraRefs("In Progress"),
		InProgressCanonical: jiraRef("In Progress"),
		InReviewMembers:     []domain.JiraStatusRef{},
		DoneMembers:         jiraRefs("Done"),
		DoneCanonical:       jiraRef("Done"),
	}
}

// RunJiraProjectConformance is the shared suite for db.JiraProjectStore and
// the registry half of db.JiraStatusRulesStore: a project found by Jira's id
// whatever key it is created or refreshed under, an unresolved row resolved in
// place or merged into the row carrying its id, a key rename reaching every
// rule read, and every rule read and write confined to the site it is given.
func RunJiraProjectConformance(t *testing.T, factory JiraProjectFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("FindOrCreate_ReturnsTheStoredRow", func(t *testing.T) {
		f := factory(t)
		created, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraSite, "10001", "SKY", "Skyline")
		if err != nil {
			t.Fatalf("FindOrCreate (create): %v", err)
		}
		want := domain.JiraProject{ID: created.ID, OrgID: f.OrgID, Site: jiraSite, ExternalID: "10001", Key: "SKY", Name: "Skyline"}
		if created != want {
			t.Errorf("FindOrCreate created %+v, want %+v", created, want)
		}
		AssertWriteReturnedStoredRow(t, "FindOrCreate (create)", created, func() (*domain.JiraProject, error) {
			return f.Projects.GetSystem(ctx, f.OrgID, created.ID)
		})

		// The same project under a new key is the same row, renamed.
		renamed, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraSite, "10001", "SKYL", "Skyline 2")
		if err != nil {
			t.Fatalf("FindOrCreate (rename): %v", err)
		}
		if renamed.ID != created.ID || renamed.Key != "SKYL" || renamed.Name != "Skyline 2" {
			t.Errorf("FindOrCreate under a new key = %+v, want row %s renamed to SKYL", renamed, created.ID)
		}
		AssertWriteReturnedStoredRow(t, "FindOrCreate (rename)", renamed, func() (*domain.JiraProject, error) {
			return f.Projects.GetSystem(ctx, f.OrgID, created.ID)
		})

		// The same id on another site is another project.
		elsewhere, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraOtherSite, "10001", "SKY", "Elsewhere")
		if err != nil {
			t.Fatalf("FindOrCreate (other site): %v", err)
		}
		if elsewhere.ID == created.ID || elsewhere.Site != jiraOtherSite {
			t.Errorf("FindOrCreate on another site = %+v, want a row of its own on %s", elsewhere, jiraOtherSite)
		}
	})

	t.Run("FindOrCreate_RefusesAMissingArgument", func(t *testing.T) {
		f := factory(t)
		for name, args := range map[string][5]string{
			"org":  {"", jiraSite, "1", "SKY", "n"},
			"site": {f.OrgID, "", "1", "SKY", "n"},
			"id":   {f.OrgID, jiraSite, "", "SKY", "n"},
			"key":  {f.OrgID, jiraSite, "1", "", "n"},
		} {
			if _, err := f.Projects.FindOrCreate(ctx, args[0], args[1], args[2], args[3], args[4]); err == nil {
				t.Errorf("FindOrCreate with no %s was accepted", name)
			}
		}
	})

	t.Run("FindOrCreate_ResolvesAnUnresolvedRowUnderTheKey", func(t *testing.T) {
		f := factory(t)
		unresolved := f.SeedUnresolved(t, jiraSite, "OPS")
		got, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraSite, "20002", "OPS", "Operations")
		if err != nil {
			t.Fatalf("FindOrCreate: %v", err)
		}
		if got.ID != unresolved || got.ExternalID != "20002" || got.Name != "Operations" {
			t.Errorf("FindOrCreate = %+v, want row %s resolved to 20002", got, unresolved)
		}
		AssertWriteReturnedStoredRow(t, "FindOrCreate (resolve)", got, func() (*domain.JiraProject, error) {
			return f.Projects.GetSystem(ctx, f.OrgID, unresolved)
		})
	})

	t.Run("FindOrCreate_LeavesAnUnresolvedRowWhenTheIDIsTaken", func(t *testing.T) {
		f := factory(t)
		resolved, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraSite, "30003", "NEW", "")
		if err != nil {
			t.Fatalf("FindOrCreate (seed): %v", err)
		}
		unresolved := f.SeedUnresolved(t, jiraSite, "OLD")
		got, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraSite, "30003", "OLD", "")
		if err != nil {
			t.Fatalf("FindOrCreate: %v", err)
		}
		if got.ID != resolved.ID {
			t.Errorf("FindOrCreate = %+v, want the row already carrying the id (%s)", got, resolved.ID)
		}
		left, err := f.Projects.GetSystem(ctx, f.OrgID, unresolved)
		if err != nil {
			t.Fatalf("GetSystem: %v", err)
		}
		if left == nil || left.ExternalID != "" {
			t.Errorf("unresolved row after FindOrCreate = %+v, want it left for the poll to merge", left)
		}
	})

	t.Run("RefreshSystem_RenameReachesEveryRuleRead", func(t *testing.T) {
		f := factory(t)
		p, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraSite, "40004", "OLD", "Project")
		if err != nil {
			t.Fatalf("FindOrCreate: %v", err)
		}
		if err := f.Rules.ReplaceForTeam(ctx, f.TeamID, jiraSite, []domain.JiraProjectStatusRules{armedJiraRule(p.ID)}); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		renamed, err := f.Projects.RefreshSystem(ctx, f.OrgID, p.ID, "40004", "NEW", "Project")
		if err != nil {
			t.Fatalf("RefreshSystem: %v", err)
		}
		if renamed.ID != p.ID || renamed.Key != "NEW" {
			t.Errorf("RefreshSystem = %+v, want row %s keyed NEW", renamed, p.ID)
		}
		AssertWriteReturnedStoredRow(t, "RefreshSystem (rename)", renamed, func() (*domain.JiraProject, error) {
			return f.Projects.GetSystem(ctx, f.OrgID, p.ID)
		})

		for name, list := range map[string]func() ([]domain.JiraProjectStatusRules, error){
			"ListForTeam": func() ([]domain.JiraProjectStatusRules, error) { return f.Rules.ListForTeam(ctx, f.TeamID, jiraSite) },
			"ListForTeamSystem": func() ([]domain.JiraProjectStatusRules, error) {
				return f.Rules.ListForTeamSystem(ctx, f.TeamID, jiraSite)
			},
			"ListForOrgSystem": func() ([]domain.JiraProjectStatusRules, error) {
				return f.Rules.ListForOrgSystem(ctx, f.OrgID, jiraSite)
			},
		} {
			got, err := list()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(got) != 1 || got[0].JiraProjectID != p.ID || got[0].ProjectKey != "NEW" || got[0].ProjectExternalID != "40004" {
				t.Errorf("%s after the rename = %+v, want the rule under NEW", name, got)
			}
		}
		if got, err := f.Rules.TracksJiraProjectSystem(ctx, f.TeamID, jiraSite, p.ID); err != nil || !got {
			t.Errorf("TracksJiraProjectSystem after the rename = %v, %v; want the row still tracked", got, err)
		}
	})

	t.Run("RefreshSystem_ResolvesAnUnresolvedRowInPlace", func(t *testing.T) {
		f := factory(t)
		id := f.SeedUnresolved(t, jiraSite, "OPS")
		got, err := f.Projects.RefreshSystem(ctx, f.OrgID, id, "50005", "OPS", "Operations")
		if err != nil {
			t.Fatalf("RefreshSystem: %v", err)
		}
		want := domain.JiraProject{ID: id, OrgID: f.OrgID, Site: jiraSite, ExternalID: "50005", Key: "OPS", Name: "Operations"}
		if got != want {
			t.Errorf("RefreshSystem = %+v, want %+v", got, want)
		}
		AssertWriteReturnedStoredRow(t, "RefreshSystem (resolve)", got, func() (*domain.JiraProject, error) {
			return f.Projects.GetSystem(ctx, f.OrgID, id)
		})
	})

	t.Run("RefreshSystem_MergesAnUnresolvedRowIntoTheRowCarryingItsID", func(t *testing.T) {
		f := factory(t)
		winner, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraSite, "60006", "NEW", "")
		if err != nil {
			t.Fatalf("FindOrCreate: %v", err)
		}
		loser := f.SeedUnresolved(t, jiraSite, "OLD")
		issue := f.SeedIssue(t, jiraSite, "OLD-1", loser)
		other, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraSite, "60007", "ENG", "")
		if err != nil {
			t.Fatalf("FindOrCreate (other): %v", err)
		}

		// The team tracks the project under both rows, with different rules
		// on each; the other team tracks it only under the unresolved one.
		winnerRule := armedJiraRule(winner.ID)
		loserRule := armedJiraRule(loser)
		loserRule.PickupMembers = jiraRefs("Backlog")
		if err := f.Rules.ReplaceForTeam(ctx, f.TeamID, jiraSite, []domain.JiraProjectStatusRules{loserRule, winnerRule, armedJiraRule(other.ID)}); err != nil {
			t.Fatalf("ReplaceForTeam (team): %v", err)
		}
		if err := f.Rules.ReplaceForTeam(ctx, f.OtherTeamID, jiraSite, []domain.JiraProjectStatusRules{loserRule}); err != nil {
			t.Fatalf("ReplaceForTeam (other team): %v", err)
		}
		setOrder := func(teamID string, order []string) {
			t.Helper()
			if _, err := f.Teams.SetJiraProjectOrder(ctx, teamID, order); err != nil {
				t.Fatalf("SetJiraProjectOrder: %v", err)
			}
		}
		setOrder(f.TeamID, []string{loser, other.ID, winner.ID})
		setOrder(f.OtherTeamID, []string{loser})

		got, err := f.Projects.RefreshSystem(ctx, f.OrgID, loser, "60006", "NEW", "Renamed")
		if err != nil {
			t.Fatalf("RefreshSystem: %v", err)
		}
		if got.ID != winner.ID || got.Key != "NEW" || got.Name != "Renamed" {
			t.Errorf("RefreshSystem = %+v, want the surviving row %s", got, winner.ID)
		}
		AssertWriteReturnedStoredRow(t, "RefreshSystem (merge)", got, func() (*domain.JiraProject, error) {
			return f.Projects.GetSystem(ctx, f.OrgID, winner.ID)
		})
		if gone, err := f.Projects.GetSystem(ctx, f.OrgID, loser); err != nil || gone != nil {
			t.Errorf("merged row after RefreshSystem = %+v, %v; want it deleted", gone, err)
		}
		// The issue that referenced the merged row references the survivor.
		if e, err := f.Entities.GetSystem(ctx, f.OrgID, issue); err != nil || e == nil || e.JiraProjectID != winner.ID {
			t.Errorf("issue after the merge = %+v, %v; want it on the surviving row %s", e, err, winner.ID)
		}

		// The team keeps the rule it already had on the surviving row.
		team, err := f.Rules.ListForTeamSystem(ctx, f.TeamID, jiraSite)
		if err != nil {
			t.Fatalf("ListForTeamSystem (team): %v", err)
		}
		if len(team) != 2 {
			t.Fatalf("team rules after the merge = %+v, want ENG and NEW", team)
		}
		for _, r := range team {
			if r.JiraProjectID == winner.ID && !reflect.DeepEqual(r.PickupMembers, winnerRule.PickupMembers) {
				t.Errorf("team's rule on the surviving row = %+v, want the one it held there", r)
			}
		}
		// The other team's rule moved onto the surviving row.
		moved, err := f.Rules.ListForTeamSystem(ctx, f.OtherTeamID, jiraSite)
		if err != nil {
			t.Fatalf("ListForTeamSystem (other team): %v", err)
		}
		if len(moved) != 1 || moved[0].JiraProjectID != winner.ID || !reflect.DeepEqual(moved[0].PickupMembers, loserRule.PickupMembers) {
			t.Errorf("other team's rules after the merge = %+v, want its rule moved to %s", moved, winner.ID)
		}

		for teamID, want := range map[string][]string{
			f.TeamID:      {other.ID, winner.ID},
			f.OtherTeamID: {winner.ID},
		} {
			ts, err := f.Teams.GetSettingsSystem(ctx, teamID)
			if err != nil {
				t.Fatalf("GetSettingsSystem: %v", err)
			}
			if !reflect.DeepEqual(ts.JiraProjects, want) {
				t.Errorf("team %s project order after the merge = %v, want %v", teamID, ts.JiraProjects, want)
			}
		}
	})

	t.Run("FindOrCreateSystem_WritesOnlyWhatChanged", func(t *testing.T) {
		f := factory(t)
		created, err := f.Projects.FindOrCreateSystem(ctx, f.OrgID, jiraSite, "11011", "SKY", "Skyline")
		if err != nil {
			t.Fatalf("FindOrCreateSystem (create): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "FindOrCreateSystem (create)", created, func() (*domain.JiraProject, error) {
			return f.Projects.GetSystem(ctx, f.OrgID, created.ID)
		})
		// An empty name keeps the stored one; a new key renames the row.
		same, err := f.Projects.FindOrCreateSystem(ctx, f.OrgID, jiraSite, "11011", "SKY", "")
		if err != nil || same != created {
			t.Errorf("FindOrCreateSystem with nothing new = %+v, %v; want %+v", same, err, created)
		}
		renamed, err := f.Projects.FindOrCreateSystem(ctx, f.OrgID, jiraSite, "11011", "SKYL", "")
		if err != nil || renamed.ID != created.ID || renamed.Key != "SKYL" || renamed.Name != "Skyline" {
			t.Errorf("FindOrCreateSystem under a new key = %+v, %v; want row %s renamed to SKYL with its name kept", renamed, err, created.ID)
		}
		AssertWriteReturnedStoredRow(t, "FindOrCreateSystem (rename)", renamed, func() (*domain.JiraProject, error) {
			return f.Projects.GetSystem(ctx, f.OrgID, created.ID)
		})
	})

	t.Run("AJiraIssueRegistersItsProjectThroughTheSameDoor", func(t *testing.T) {
		f := factory(t)
		// A project the registry has a row for under its key alone is resolved
		// by the first issue read in it.
		unresolved := f.SeedUnresolved(t, jiraSite, "OPS")
		ops, _, err := f.Entities.FindOrCreateJiraSystem(ctx, f.OrgID, jiraSite, "OPS-1", "90001",
			domain.JiraProjectRef{ID: "12012", Key: "OPS", Name: "Operations"}, "t", "")
		if err != nil {
			t.Fatalf("FindOrCreateJiraSystem (OPS): %v", err)
		}
		if ops.JiraProjectID != unresolved {
			t.Errorf("OPS-1 references %q, want the unresolved OPS row %s", ops.JiraProjectID, unresolved)
		}
		if p, err := f.Projects.GetSystem(ctx, f.OrgID, unresolved); err != nil || p == nil || p.ExternalID != "12012" || p.Name != "Operations" {
			t.Errorf("OPS row after its issue registered = %+v, %v; want it resolved to 12012", p, err)
		}

		// A project the registry has no row for gets one, which tracks
		// nothing.
		eng, _, err := f.Entities.FindOrCreateJiraSystem(ctx, f.OrgID, jiraSite, "ENG-1", "90002",
			domain.JiraProjectRef{ID: "13013", Key: "ENG"}, "t", "")
		if err != nil {
			t.Fatalf("FindOrCreateJiraSystem (ENG): %v", err)
		}
		p, err := f.Projects.GetSystem(ctx, f.OrgID, eng.JiraProjectID)
		if err != nil || p == nil || p.Site != jiraSite || p.ExternalID != "13013" || p.Key != "ENG" {
			t.Errorf("ENG-1's registry row = %+v, %v; want ENG under 13013 on %s", p, err, jiraSite)
		}
		if tracks, err := f.Rules.TracksJiraProjectSystem(ctx, f.TeamID, jiraSite, eng.JiraProjectID); err != nil || tracks {
			t.Errorf("TracksJiraProjectSystem on a registered project = %v, %v; want it untracked", tracks, err)
		}
	})

	t.Run("RefreshSystem_RefusesAnotherProjectOrAnUnknownRow", func(t *testing.T) {
		f := factory(t)
		p, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraSite, "70007", "SKY", "")
		if err != nil {
			t.Fatalf("FindOrCreate: %v", err)
		}
		if _, err := f.Projects.RefreshSystem(ctx, f.OrgID, p.ID, "70008", "SKY", ""); !errors.Is(err, db.ErrJiraProjectIdentityConflict) {
			t.Errorf("RefreshSystem with another project id: err = %v, want ErrJiraProjectIdentityConflict", err)
		}
		if _, err := f.Projects.RefreshSystem(ctx, f.OrgID, "00000000-0000-4000-8000-00000000dead", "70007", "SKY", ""); !errors.Is(err, db.ErrNoSuchJiraProject) {
			t.Errorf("RefreshSystem of an unknown row: err = %v, want ErrNoSuchJiraProject", err)
		}
	})

	t.Run("Rules_AreConfinedToTheirSite", func(t *testing.T) {
		f := factory(t)
		here, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraSite, "80008", "SKY", "")
		if err != nil {
			t.Fatalf("FindOrCreate: %v", err)
		}
		there, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraOtherSite, "90009", "SKY", "")
		if err != nil {
			t.Fatalf("FindOrCreate (other site): %v", err)
		}
		if err := f.Rules.ReplaceForTeam(ctx, f.TeamID, jiraSite, []domain.JiraProjectStatusRules{armedJiraRule(here.ID)}); err != nil {
			t.Fatalf("ReplaceForTeam (site): %v", err)
		}
		if err := f.Rules.ReplaceForTeam(ctx, f.TeamID, jiraOtherSite, []domain.JiraProjectStatusRules{armedJiraRule(there.ID)}); err != nil {
			t.Fatalf("ReplaceForTeam (other site): %v", err)
		}
		for site, want := range map[string]string{jiraSite: here.ID, jiraOtherSite: there.ID} {
			got, err := f.Rules.ListForTeam(ctx, f.TeamID, site)
			if err != nil {
				t.Fatalf("ListForTeam(%s): %v", site, err)
			}
			if len(got) != 1 || got[0].JiraProjectID != want {
				t.Errorf("ListForTeam(%s) = %+v, want only the rule for %s", site, got, want)
			}
		}

		// Clearing one site's set leaves the other site's rules stored.
		if err := f.Rules.ReplaceForTeam(ctx, f.TeamID, jiraOtherSite, nil); err != nil {
			t.Fatalf("ReplaceForTeam (clear other site): %v", err)
		}
		got, err := f.Rules.ListForTeam(ctx, f.TeamID, jiraSite)
		if err != nil {
			t.Fatalf("ListForTeam: %v", err)
		}
		if len(got) != 1 || got[0].JiraProjectID != here.ID {
			t.Errorf("rules on %s after clearing %s = %+v, want them untouched", jiraSite, jiraOtherSite, got)
		}

		// No site reads as nothing.
		none, err := f.Rules.ListForOrgSystem(ctx, f.OrgID, "")
		if err != nil {
			t.Fatalf("ListForOrgSystem(no site): %v", err)
		}
		if len(none) != 0 {
			t.Errorf("ListForOrgSystem with no site = %+v, want nothing", none)
		}
	})

	t.Run("ReplaceForTeam_RefusesARowItCouldNotRead", func(t *testing.T) {
		f := factory(t)
		here, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraSite, "11111", "SKY", "")
		if err != nil {
			t.Fatalf("FindOrCreate: %v", err)
		}
		there, err := f.Projects.FindOrCreate(ctx, f.OrgID, jiraOtherSite, "22222", "ENG", "")
		if err != nil {
			t.Fatalf("FindOrCreate (other site): %v", err)
		}
		if err := f.Rules.ReplaceForTeam(ctx, f.TeamID, jiraSite, []domain.JiraProjectStatusRules{armedJiraRule(here.ID)}); err != nil {
			t.Fatalf("ReplaceForTeam (seed): %v", err)
		}

		err = f.Rules.ReplaceForTeam(ctx, f.TeamID, jiraSite, []domain.JiraProjectStatusRules{armedJiraRule(there.ID)})
		if !errors.Is(err, db.ErrJiraRuleProjectNotOnSite) {
			t.Errorf("ReplaceForTeam naming another site's project: err = %v, want ErrJiraRuleProjectNotOnSite", err)
		}
		for name, rules := range map[string][]domain.JiraProjectStatusRules{
			"no project": {armedJiraRule("")},
			"twice":      {armedJiraRule(here.ID), armedJiraRule(here.ID)},
		} {
			if err := f.Rules.ReplaceForTeam(ctx, f.TeamID, jiraSite, rules); err == nil {
				t.Errorf("ReplaceForTeam with a rule naming %s was accepted", name)
			}
		}
		if err := f.Rules.ReplaceForTeam(ctx, f.TeamID, "", nil); err == nil {
			t.Error("ReplaceForTeam with no site was accepted")
		}

		// Every refused write left the stored set as it was.
		got, err := f.Rules.ListForTeam(ctx, f.TeamID, jiraSite)
		if err != nil {
			t.Fatalf("ListForTeam: %v", err)
		}
		if len(got) != 1 || got[0].JiraProjectID != here.ID {
			t.Errorf("rules after the refused writes = %+v, want the seeded one", got)
		}
	})
}
