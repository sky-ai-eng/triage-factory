package tracker

import (
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

// matchesPredicate runs predJSON against evt through the registered schema,
// the matcher the router evaluates task rules and triggers with.
func matchesPredicate(t *testing.T, evt domain.Event, predJSON string) bool {
	t.Helper()
	schema, ok := events.Get(evt.EventType)
	if !ok {
		t.Fatalf("no schema registered for %s", evt.EventType)
	}
	ok, err := schema.Match(predJSON, evt.MetadataJSON)
	if err != nil {
		t.Fatalf("match %s against %s: %v", predJSON, evt.EventType, err)
	}
	return ok
}

func allowlist(t *testing.T, field string, ids ...string) string {
	t.Helper()
	raw, err := json.Marshal(map[string][]string{field: ids})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestJiraReporter_ReachesReporterIn decodes a Cloud and a Data Center issue
// the way the tracker reads one, diffs it into each event that offers
// reporter_in, and checks the reporter lands in the metadata and the
// allowlist matches on it: the reporter's id and nobody else's.
func TestJiraReporter_ReachesReporterIn(t *testing.T) {
	fixtures := []struct {
		name     string
		reporter string // the issue's reporter field, as Jira answers it
		wantID   string
	}{
		{"cloud", `{"displayName":"Ada Lovelace","accountId":"557058:abc-ada"}`, "557058:abc-ada"},
		{"data center", `{"displayName":"Ada Lovelace","key":"JIRAUSER10200","name":"ada"}`, "JIRAUSER10200"},
	}
	cases := []struct {
		name      string
		eventType string
		first     bool // prev is the zero snapshot
		prev      func(*domain.JiraSnapshot)
		curr      func(*domain.JiraSnapshot)
	}{
		{name: "available on first discovery", eventType: domain.EventJiraIssueAvailable, first: true},
		{name: "assigned on first discovery", eventType: domain.EventJiraIssueAssigned, first: true,
			curr: func(s *domain.JiraSnapshot) { s.Assignee, s.AssigneeAccountID = "Bob", "bob-id" }},
		{name: "available on unassignment", eventType: domain.EventJiraIssueAvailable,
			prev: func(s *domain.JiraSnapshot) { s.Assignee, s.AssigneeAccountID = "Bob", "bob-id" }},
		{name: "assigned on assignment", eventType: domain.EventJiraIssueAssigned,
			curr: func(s *domain.JiraSnapshot) { s.Assignee, s.AssigneeAccountID = "Bob", "bob-id" }},
		{name: "became_atomic", eventType: domain.EventJiraIssueBecameAtomic,
			prev: func(s *domain.JiraSnapshot) { s.OpenSubtaskCount = 1 }},
	}
	for _, fx := range fixtures {
		raw := `{"id":"10001","key":"SKY-1","fields":{"summary":"Fix the thing",` +
			`"status":{"id":"1","name":"To Do"},"reporter":` + fx.reporter + `}}`
		var issue jiraclient.Issue
		if err := json.Unmarshal([]byte(raw), &issue); err != nil {
			t.Fatalf("%s fixture: %v", fx.name, err)
		}
		base := issueToState(issue, "https://jira.example.com", nil).Snap

		for _, tc := range cases {
			t.Run(fx.name+"/"+tc.name, func(t *testing.T) {
				prev, curr := base, base
				if tc.first {
					prev = domain.JiraSnapshot{}
				}
				if tc.prev != nil {
					tc.prev(&prev)
				}
				if tc.curr != nil {
					tc.curr(&curr)
				}
				evt := findEvent(DiffJiraSnapshots(prev, curr, testEntityID, testDoneStatuses, nil), tc.eventType)
				if evt == nil {
					t.Fatalf("no %s event", tc.eventType)
				}

				var meta struct {
					Reporter          string `json:"reporter"`
					ReporterAccountID string `json:"reporter_account_id"`
				}
				if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
					t.Fatal(err)
				}
				if meta.Reporter != "Ada Lovelace" || meta.ReporterAccountID != fx.wantID {
					t.Errorf("metadata reporter = (%q, %q), want (%q, %q)", meta.Reporter, meta.ReporterAccountID, "Ada Lovelace", fx.wantID)
				}
				if !matchesPredicate(t, *evt, allowlist(t, "reporter_in", fx.wantID)) {
					t.Errorf("reporter_in naming the reporter did not match")
				}
				if matchesPredicate(t, *evt, allowlist(t, "reporter_in", "557058:someone-else")) {
					t.Errorf("reporter_in naming someone else matched")
				}
			})
		}
	}
}

// TestJiraReporter_ChangeEmitsNothing: the reporter is carried, never diffed.
func TestJiraReporter_ChangeEmitsNothing(t *testing.T) {
	prev := domain.JiraSnapshot{ID: "10001", Key: "SKY-1", Status: "To Do", Reporter: "Ada", ReporterAccountID: "ada-id"}
	curr := prev
	curr.Reporter, curr.ReporterAccountID = "Bob", "bob-id"
	if evts := DiffJiraSnapshots(prev, curr, testEntityID, testDoneStatuses, nil); len(evts) != 0 {
		t.Errorf("reporter change emitted %v, want nothing", eventTypes(evts))
	}
}

// TestLinearCreator_OnEveryEvent converts an issue the way the tracker does,
// checks every event the diff emits carries the creator, and that creator_in
// matches on it wherever it is offered.
func TestLinearCreator_OnEveryEvent(t *testing.T) {
	issue := linear.Issue{
		ID: "uuid-1", Identifier: "ENG-1", Title: "Fix the thing",
		State:    linear.WorkflowState{ID: linTodo.ID, Name: linTodo.Name, Type: linTodo.Type},
		Assignee: &linear.User{ID: "u-alice", Name: "Alice"},
		Creator:  &linear.User{ID: "u-ada", Name: "Ada Lovelace"},
		Team:     linear.Team{ID: "team-eng", Key: "ENG"},
	}
	base := linearIssueToState(issue, linDoneSet).Snap
	if base.Creator != "Ada Lovelace" || base.CreatorUserID != "u-ada" {
		t.Fatalf("snapshot creator = (%q, %q), want (Ada Lovelace, u-ada)", base.Creator, base.CreatorUserID)
	}

	offersCreatorIn := map[string]bool{
		domain.EventLinearIssueAssigned:     true,
		domain.EventLinearIssueAvailable:    true,
		domain.EventLinearIssueBecameAtomic: true,
	}
	cases := []struct {
		name      string
		eventType string
		first     bool
		prev      func(*domain.LinearSnapshot)
		curr      func(*domain.LinearSnapshot)
	}{
		{name: "assigned on first discovery", eventType: domain.EventLinearIssueAssigned, first: true},
		{name: "available on first discovery", eventType: domain.EventLinearIssueAvailable, first: true,
			curr: func(s *domain.LinearSnapshot) { s.Assignee, s.AssigneeUserID = "", "" }},
		{name: "available on unassignment", eventType: domain.EventLinearIssueAvailable,
			curr: func(s *domain.LinearSnapshot) { s.Assignee, s.AssigneeUserID = "", "" }},
		{name: "became_atomic", eventType: domain.EventLinearIssueBecameAtomic,
			prev: func(s *domain.LinearSnapshot) { s.OpenChildCount = 1 }},
		{name: "status_changed", eventType: domain.EventLinearIssueStatusChanged,
			curr: func(s *domain.LinearSnapshot) { s.State = linProgress }},
		{name: "identifier_changed", eventType: domain.EventLinearIssueIdentifierChanged,
			curr: func(s *domain.LinearSnapshot) { s.Identifier = "OPS-1" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev, curr := base, base
			if tc.first {
				prev = domain.LinearSnapshot{}
			}
			if tc.prev != nil {
				tc.prev(&prev)
			}
			if tc.curr != nil {
				tc.curr(&curr)
			}
			evt := findEvent(DiffLinearSnapshots(prev, curr, testEntityID, linDoneSet, nil), tc.eventType)
			if evt == nil {
				t.Fatalf("no %s event", tc.eventType)
			}
			var meta events.LinearIssueIdentity
			if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
				t.Fatal(err)
			}
			if meta.Creator != "Ada Lovelace" || meta.CreatorUserID != "u-ada" {
				t.Errorf("metadata creator = (%q, %q), want (Ada Lovelace, u-ada)", meta.Creator, meta.CreatorUserID)
			}
			if !offersCreatorIn[tc.eventType] {
				return
			}
			if !matchesPredicate(t, *evt, allowlist(t, "creator_in", "u-ada")) {
				t.Errorf("creator_in naming the creator did not match")
			}
			if matchesPredicate(t, *evt, allowlist(t, "creator_in", "u-bob")) {
				t.Errorf("creator_in naming someone else matched")
			}
		})
	}
}

// TestLinearCreator_IntegrationCreated: an issue with no creator leaves both
// fields empty, so only a rule that does not filter on creator matches it.
func TestLinearCreator_IntegrationCreated(t *testing.T) {
	issue := linear.Issue{
		ID: "uuid-1", Identifier: "ENG-1",
		State: linear.WorkflowState{ID: linTodo.ID, Name: linTodo.Name, Type: linTodo.Type},
		Team:  linear.Team{ID: "team-eng", Key: "ENG"},
	}
	snap := linearIssueToState(issue, linDoneSet).Snap
	if snap.Creator != "" || snap.CreatorUserID != "" {
		t.Fatalf("snapshot creator = (%q, %q), want empty", snap.Creator, snap.CreatorUserID)
	}
	evt := findEvent(DiffLinearSnapshots(domain.LinearSnapshot{}, snap, testEntityID, linDoneSet, nil), domain.EventLinearIssueAvailable)
	if evt == nil {
		t.Fatal("no available event")
	}
	if matchesPredicate(t, *evt, allowlist(t, "creator_in", "u-ada")) {
		t.Error("creator_in matched an issue with no creator")
	}
	if !matchesPredicate(t, *evt, `{}`) {
		t.Error("an empty predicate did not match")
	}
}
