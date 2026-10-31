package tracker

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// The assignee, status and priority are identified by id, and by name only
// where a side has none. A case with no event wants no events at all; one
// with an event checks the first of its type. Keying a change on the new
// status's or priority's id means one renamed while a task waits on it does
// not open a second task.
func TestDiffJira_IDs(t *testing.T) {
	type snap = domain.JiraSnapshot
	type meta = map[string]string
	todo := snap{Status: "To Do", StatusID: "10000"}
	cases := []struct {
		name       string
		prev, curr snap // a zero prev is first discovery
		done       []domain.JiraStatusRef
		event      string
		dedupKey   string
		meta       meta
		pred       string
	}{
		{name: "assignee's display name changed", prev: snap{Assignee: "Ada Lovelace", AssigneeAccountID: "557058:ada"},
			curr: snap{Assignee: "Ada King", AssigneeAccountID: "557058:ada"}},
		{name: "reassigned between namesakes", prev: snap{Assignee: "Sam Smith", AssigneeAccountID: "557058:sam-a"},
			curr:  snap{Assignee: "Sam Smith", AssigneeAccountID: "557058:sam-b"},
			event: domain.EventJiraIssueAssigned, meta: meta{"assignee_account_id": "557058:sam-b"}},
		{name: "assignee's account id arriving", prev: snap{Assignee: "Ada Lovelace"},
			curr: snap{Assignee: "Ada Lovelace", AssigneeAccountID: "557058:ada"}},
		{name: "reassigned against a snapshot without an account id", prev: snap{Assignee: "Ada Lovelace"},
			curr: snap{Assignee: "Grace Hopper", AssigneeAccountID: "557058:grace"}, event: domain.EventJiraIssueAssigned},
		{name: "discovered with an assignee that has no display name",
			curr: snap{Status: "To Do", AssigneeAccountID: "557058:ada"}, event: domain.EventJiraIssueAssigned},
		{name: "unassigned from an assignee that had no display name", prev: snap{Status: "To Do", AssigneeAccountID: "557058:ada"},
			curr: snap{Status: "To Do"}, event: domain.EventJiraIssueAvailable},
		{name: "priority renamed", prev: snap{Priority: "High", PriorityID: "2"}, curr: snap{Priority: "P1", PriorityID: "2"}},
		{name: "priority's id arriving", prev: snap{Priority: "High"}, curr: snap{Priority: "High", PriorityID: "2"}},
		{name: "priority changed", prev: snap{Priority: "Low", PriorityID: "4"}, curr: snap{Priority: "High", PriorityID: "2"},
			event: domain.EventJiraIssuePriorityChanged, dedupKey: "2", meta: meta{"old_priority_id": "4", "new_priority_id": "2"}},
		{name: "priority changed without ids", prev: snap{Priority: "Low"}, curr: snap{Priority: "High"},
			event: domain.EventJiraIssuePriorityChanged, dedupKey: "High"},
		{name: "status changed", prev: todo, curr: snap{Status: "In Review", StatusID: "10003"},
			event: domain.EventJiraIssueStatusChanged, dedupKey: "10003",
			meta: meta{"old_status": "To Do", "old_status_id": "10000", "new_status": "In Review", "new_status_id": "10003"},
			pred: `{"new_status_id_in":["10003"],"old_status_id_in":["10000"]}`},
		{name: "entered a renamed status", prev: todo, curr: snap{Status: "Peer Review", StatusID: "10003"},
			event: domain.EventJiraIssueStatusChanged, dedupKey: "10003"},
		{name: "status changed without ids", prev: snap{Status: "To Do"}, curr: snap{Status: "Done"},
			event: domain.EventJiraIssueStatusChanged, dedupKey: "Done"},
		{name: "discovered", curr: snap{Status: "To Do", StatusID: "10000", Priority: "High", PriorityID: "2", IssueType: "Bug", IssueTypeID: "10004"},
			event: domain.EventJiraIssueAvailable, meta: meta{"status_id": "10000", "priority_id": "2", "issue_type_id": "10004"}},
		{name: "labelled", prev: snap{Status: "To Do", StatusID: "10000", Labels: []string{}},
			curr:  snap{Status: "To Do", StatusID: "10000", Labels: []string{"tf-agent"}},
			event: domain.EventJiraIssueLabelAdded, meta: meta{"status_id": "10000"}},
		{name: "completed", prev: todo, curr: snap{Status: "Done", StatusID: "10002"}, done: []domain.JiraStatusRef{{ID: "10002", Name: "Done"}},
			event: domain.EventJiraIssueCompleted, meta: meta{"final_status_id": "10002"}},
	}
	for _, c := range cases {
		if !reflect.DeepEqual(c.prev, snap{}) {
			c.prev.Key = "SKY-1"
		}
		c.curr.Key = "SKY-1"
		evts := DiffJiraSnapshots(c.prev, c.curr, testEntityID, JiraEventProjects{}, c.done, nil)
		if c.event == "" {
			if len(evts) != 0 {
				t.Errorf("%s: emitted %v, want nothing", c.name, eventTypes(evts))
			}
			continue
		}
		evt := findEvent(evts, c.event)
		if evt == nil {
			t.Errorf("%s: no %s in %v", c.name, c.event, eventTypes(evts))
			continue
		}
		if c.dedupKey != "" && evt.DedupKey != c.dedupKey {
			t.Errorf("%s: dedup_key = %q, want %q", c.name, evt.DedupKey, c.dedupKey)
		}
		got := decodeMetadata[map[string]any](t, *evt)
		for k, v := range c.meta {
			if got[k] != v {
				t.Errorf("%s: %s = %v, want %q", c.name, k, got[k], v)
			}
		}
		if c.pred != "" && !matchesPredicate(t, *evt, c.pred) {
			t.Errorf("%s: %s did not match %s", c.name, c.pred, evt.MetadataJSON)
		}
	}
}

func TestIssueToState_RecordsPriorityAndIssueTypeIDs(t *testing.T) {
	var issue jiraclient.Issue
	if err := json.Unmarshal([]byte(`{"id":"10001","key":"SKY-1","fields":{
		"summary":"s","priority":{"id":"2","name":"High"},"issuetype":{"id":"10004","name":"Bug"}}}`), &issue); err != nil {
		t.Fatal(err)
	}
	snap := issueToState(issue, "https://jira.corp.com", nil).Snap
	if snap.Priority != "High" || snap.PriorityID != "2" {
		t.Errorf("priority = %q (%q), want High (2)", snap.Priority, snap.PriorityID)
	}
	if snap.IssueType != "Bug" || snap.IssueTypeID != "10004" {
		t.Errorf("issue type = %q (%q), want Bug (10004)", snap.IssueType, snap.IssueTypeID)
	}
}
