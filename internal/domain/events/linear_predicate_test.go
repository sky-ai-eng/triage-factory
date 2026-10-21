package events

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestLinearPredicates_ScopeByTeamID pins that every Linear predicate scopes
// to a Linear team by id, so a team key rename cannot leave a filter matching
// nothing, and that none still offers the key.
func TestLinearPredicates_ScopeByTeamID(t *testing.T) {
	n := 0
	for eventType, s := range All() {
		if !strings.HasPrefix(eventType, "linear:") {
			continue
		}
		n++
		fields := map[string]string{}
		for _, f := range s.Fields {
			fields[f.Name] = f.Type
		}
		if fields["linear_team_id_in"] != "string_list" {
			t.Errorf("%s has no linear_team_id_in list: fields %v", eventType, fields)
		}
		for name := range fields {
			if strings.Contains(name, "team_key") {
				t.Errorf("%s still offers %s", eventType, name)
			}
		}
	}
	if n != 13 {
		t.Errorf("checked %d linear event types, want 13", n)
	}
}

// TestLinearMatcher_TeamIDIn runs the type-erased matcher: linear_team_id_in
// matches the event's linear_team_id exactly, and the team's key, the value a
// caller might reach for, matches nothing.
func TestLinearMatcher_TeamIDIn(t *testing.T) {
	s, _ := Get(domain.EventLinearIssueAssigned)
	metaJSON, _ := json.Marshal(LinearIssueAssignedMetadata{
		LinearIssueIdentity: LinearIssueIdentity{IssueIdentifier: "ENG-1", LinearTeamID: "team-eng", LinearTeamKey: "ENG"},
	})
	cases := []struct {
		name, predicate string
		want            bool
	}{
		{"empty list is no filter", `{"linear_team_id_in":[]}`, true},
		{"the event's team among others", `{"linear_team_id_in":["team-ops","team-eng"]}`, true},
		{"another team", `{"linear_team_id_in":["team-ops"]}`, false},
		{"the key matches nothing", `{"linear_team_id_in":["ENG"]}`, false},
		{"ids are not case-folded", `{"linear_team_id_in":["TEAM-ENG"]}`, false},
	}
	for _, tc := range cases {
		got, err := s.Match(tc.predicate, string(metaJSON))
		if err != nil {
			t.Fatalf("%s: Match error: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: Match = %v, want %v", tc.name, got, tc.want)
		}
	}

	if _, err := s.Match(`{"linear_team_key":"ENG"}`, string(metaJSON)); err == nil {
		t.Error("a stored linear_team_key matched without error; want it refused rather than read as match-all")
	}
	if _, err := ValidatePredicateJSON(domain.EventLinearIssueAssigned, `{"linear_team_key":"ENG"}`); err == nil {
		t.Error("ValidatePredicateJSON accepted linear_team_key")
	}
}

// TestLinearIdentifierChangedPredicate pins the two team filters of the one
// Linear event about two teams: linear_team_id_in on the team the issue is in
// now, old_linear_team_id_in on the one it left.
func TestLinearIdentifierChangedPredicate(t *testing.T) {
	meta := LinearIssueIdentifierChangedMetadata{
		LinearIssueIdentity: LinearIssueIdentity{IssueIdentifier: "OPS-7", LinearTeamID: "team-ops", LinearTeamKey: "OPS"},
		OldIdentifier:       "ENG-7", OldLinearTeamID: "team-eng", OldLinearTeamKey: "ENG",
	}
	cases := []struct {
		name string
		pred LinearIssueIdentifierChangedPredicate
		want bool
	}{
		{"empty predicate", LinearIssueIdentifierChangedPredicate{}, true},
		{"the team it is in now", LinearIssueIdentifierChangedPredicate{LinearTeamIDIn: []string{"team-ops"}}, true},
		{"the team it left, as the current team", LinearIssueIdentifierChangedPredicate{LinearTeamIDIn: []string{"team-eng"}}, false},
		{"the team it left", LinearIssueIdentifierChangedPredicate{OldLinearTeamIDIn: []string{"team-eng"}}, true},
		{"the team it is in now, as the team it left", LinearIssueIdentifierChangedPredicate{OldLinearTeamIDIn: []string{"team-ops"}}, false},
		{"both", LinearIssueIdentifierChangedPredicate{LinearTeamIDIn: []string{"team-ops"}, OldLinearTeamIDIn: []string{"team-eng"}}, true},
	}
	for _, tc := range cases {
		if got := tc.pred.Matches(meta); got != tc.want {
			t.Errorf("%s: Matches() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestLinearMatcher_NewParentID runs the type-erased matcher for
// parent_changed: new_parent_id matches the new parent's UUID exactly, and
// its identifier, which a team move or key rename changes, matches nothing.
// The identifier field it replaces is refused rather than read as match-all.
func TestLinearMatcher_NewParentID(t *testing.T) {
	s, _ := Get(domain.EventLinearIssueParentChanged)
	metaJSON, _ := json.Marshal(LinearIssueParentChangedMetadata{
		LinearIssueIdentity: LinearIssueIdentity{IssueIdentifier: "ENG-1", LinearTeamID: "team-eng"},
		OldParentID:         "uuid-old", NewParentID: "uuid-new",
		OldParent: "ENG-9", NewParent: "OPS-4",
	})
	cases := []struct {
		name, predicate string
		want            bool
	}{
		{"no filter", `{}`, true},
		{"the new parent's id", `{"new_parent_id":"uuid-new"}`, true},
		{"the old parent's id", `{"new_parent_id":"uuid-old"}`, false},
		{"the new parent's identifier matches nothing", `{"new_parent_id":"OPS-4"}`, false},
		{"ids are not case-folded", `{"new_parent_id":"UUID-NEW"}`, false},
	}
	for _, tc := range cases {
		got, err := s.Match(tc.predicate, string(metaJSON))
		if err != nil {
			t.Fatalf("%s: Match error: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: Match = %v, want %v", tc.name, got, tc.want)
		}
	}

	if _, err := s.Match(`{"new_parent":"OPS-4"}`, string(metaJSON)); err == nil {
		t.Error("a stored new_parent matched without error; want it refused rather than read as match-all")
	}
	if _, err := ValidatePredicateJSON(domain.EventLinearIssueParentChanged, `{"new_parent":"OPS-4"}`); err == nil {
		t.Error("ValidatePredicateJSON accepted new_parent")
	}
}
