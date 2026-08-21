package poller

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/tracker"
)

// A team may WATCH a Jira project before mapping its workflow's statuses. Such
// a row carries no members, and the discovery JQL is built from members — so
// the merge that feeds the tracker drops it, and these pin that.

// jiraRef builds one status ref the way a rule armed through the API carries
// it: the id, which is the identity, plus the display name resolved for it.
func jiraRef(name string) domain.JiraStatusRef {
	return domain.JiraStatusRef{ID: "st-" + name, Name: name}
}

func jiraRefs(names ...string) []domain.JiraStatusRef {
	refs := make([]domain.JiraStatusRef, 0, len(names))
	for _, n := range names {
		refs = append(refs, jiraRef(n))
	}
	return refs
}

// armedRule is a fully-mapped rule for key, owned by team.
func armedRule(team, key string, pickup, done []string) domain.JiraProjectStatusRules {
	return domain.JiraProjectStatusRules{
		TeamID:              team,
		ProjectKey:          key,
		PickupMembers:       jiraRefs(pickup...),
		InProgressMembers:   jiraRefs("In Progress"),
		InProgressCanonical: jiraRef("In Progress"),
		DoneMembers:         jiraRefs(done...),
		DoneCanonical:       jiraRef(done[0]),
	}
}

func mergedKeys(rules tracker.JiraRules) string {
	keys := make([]string, len(rules))
	for i, r := range rules {
		keys[i] = r.Key
	}
	return strings.Join(keys, ",")
}

// TestToTrackerJiraRules_SkipsUnarmed: a watched-but-unmapped project is not a
// project the poller can ask Jira about, so it never reaches the merged view.
func TestToTrackerJiraRules_SkipsUnarmed(t *testing.T) {
	got := toTrackerJiraRules([]domain.JiraProjectStatusRules{
		armedRule("team-a", "SKY", []string{"To Do"}, []string{"Done"}),
		{TeamID: "team-a", ProjectKey: "OPS"},
	})
	if k := mergedKeys(got); k != "SKY" {
		t.Errorf("merged keys = %q, want SKY alone", k)
	}
}

// TestToTrackerJiraRules_SkipsPartiallyArmed — arming is all three rules. A
// project with pickup mapped and no write targets can be discovered but never
// claimed or completed, which is not a state to poll from.
func TestToTrackerJiraRules_SkipsPartiallyArmed(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule domain.JiraProjectStatusRules
	}{
		{"pickup only", domain.JiraProjectStatusRules{
			ProjectKey: "OPS", PickupMembers: jiraRefs("To Do"),
		}},
		{"no pickup", domain.JiraProjectStatusRules{
			ProjectKey:        "OPS",
			InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
			DoneMembers: jiraRefs("Done"), DoneCanonical: jiraRef("Done"),
		}},
		{"done unmapped", domain.JiraProjectStatusRules{
			ProjectKey: "OPS", PickupMembers: jiraRefs("To Do"),
			InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toTrackerJiraRules([]domain.JiraProjectStatusRules{tc.rule}); len(got) != 0 {
				t.Errorf("merged = %+v, want nothing — the project is not armed", got)
			}
		})
	}
}

// TestToTrackerJiraRules_UnarmedTeamDoesNotDiluteAnArmedOne is the multi-team
// case: two teams track SKY, one has mapped it and one has not. The project is
// polled on the armed team's rules, and the unarmed row changes nothing.
func TestToTrackerJiraRules_UnarmedTeamDoesNotDiluteAnArmedOne(t *testing.T) {
	got := toTrackerJiraRules([]domain.JiraProjectStatusRules{
		{TeamID: "team-a", ProjectKey: "SKY"},
		armedRule("team-b", "SKY", []string{"Backlog"}, []string{"Done"}),
	})
	if k := mergedKeys(got); k != "SKY" {
		t.Fatalf("merged keys = %q, want SKY", k)
	}
	rule := got.ForKey("SKY")
	if len(rule.PickupMembers) != 1 || rule.PickupMembers[0].Name != "Backlog" {
		t.Errorf("pickup = %v, want the armed team's members alone", rule.PickupMembers)
	}
	if len(rule.DoneMembers) != 1 || rule.DoneMembers[0].Name != "Done" {
		t.Errorf("done = %v, want the armed team's members alone", rule.DoneMembers)
	}
}

// TestToTrackerJiraRules_MergesArmedTeams keeps the merge the armed rows have
// always had: two teams' status sets union, first-seen order preserved.
func TestToTrackerJiraRules_MergesArmedTeams(t *testing.T) {
	got := toTrackerJiraRules([]domain.JiraProjectStatusRules{
		armedRule("team-a", "SKY", []string{"To Do", "Backlog"}, []string{"Done"}),
		armedRule("team-b", "SKY", []string{"Backlog", "Selected"}, []string{"Done", "Verified"}),
	})
	rule := got.ForKey("SKY")
	if rule == nil {
		t.Fatalf("merged = %+v, want SKY", got)
	}
	if want := "To Do,Backlog,Selected"; strings.Join(domain.JiraStatusNames(rule.PickupMembers), ",") != want {
		t.Errorf("pickup = %v, want the union %q in first-seen order", rule.PickupMembers, want)
	}
	if want := "Done,Verified"; strings.Join(domain.JiraStatusNames(rule.DoneMembers), ",") != want {
		t.Errorf("done = %v, want the union %q", rule.DoneMembers, want)
	}
}
