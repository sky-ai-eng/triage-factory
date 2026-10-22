package tracker

import (
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// An assignee who changes their display name is still the assignee. The
// account id is unchanged, so the issue was not reassigned and nothing is
// emitted.
func TestDiffJira_AssigneeDisplayNameChangeIsNotAReassignment(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Assignee: "Ada Lovelace", AssigneeAccountID: "557058:ada"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Assignee: "Ada King", AssigneeAccountID: "557058:ada"}

	evts := DiffJiraSnapshots(prev, curr, testEntityID, JiraEventProjects{}, testDoneStatuses, nil)
	if len(evts) != 0 {
		t.Errorf("a display name change emitted %v; the issue was not reassigned", eventTypes(evts))
	}
}

// Two people who share a display name are two assignees. Handing the issue
// from one to the other is a reassignment, and the event names the new one.
func TestDiffJira_ReassignmentBetweenNamesakes(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Assignee: "Sam Smith", AssigneeAccountID: "557058:sam-a"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Assignee: "Sam Smith", AssigneeAccountID: "557058:sam-b"}

	evt := findEvent(DiffJiraSnapshots(prev, curr, testEntityID, JiraEventProjects{}, testDoneStatuses, nil),
		domain.EventJiraIssueAssigned)
	if evt == nil {
		t.Fatal("no assigned event for a reassignment between two people with one display name")
	}
	if meta := decodeMetadata[events.JiraIssueAssignedMetadata](t, *evt); meta.AssigneeAccountID != "557058:sam-b" {
		t.Errorf("AssigneeAccountID = %q, want the new assignee's", meta.AssigneeAccountID)
	}
}

// A snapshot stored before account ids were recorded names its assignee by
// display name only. The first read that records the id compares on the name,
// so an unchanged assignee is not a reassignment and a changed one still is.
func TestDiffJira_AssigneeWithoutAccountIDComparesByName(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Assignee: "Ada Lovelace"}

	same := domain.JiraSnapshot{Key: "SKY-1", Assignee: "Ada Lovelace", AssigneeAccountID: "557058:ada"}
	if evts := DiffJiraSnapshots(prev, same, testEntityID, JiraEventProjects{}, testDoneStatuses, nil); len(evts) != 0 {
		t.Errorf("the account id arriving emitted %v", eventTypes(evts))
	}

	other := domain.JiraSnapshot{Key: "SKY-1", Assignee: "Grace Hopper", AssigneeAccountID: "557058:grace"}
	if findEvent(DiffJiraSnapshots(prev, other, testEntityID, JiraEventProjects{}, testDoneStatuses, nil),
		domain.EventJiraIssueAssigned) == nil {
		t.Error("no assigned event for a reassignment seen against a snapshot without an account id")
	}
}

// An assignee is present when either field is. One with an account id and no
// display name is still assigned, on first discovery and on unassignment.
func TestDiffJira_AssigneeIsPresentByIDOrName(t *testing.T) {
	idOnly := domain.JiraSnapshot{Key: "SKY-1", Status: "To Do", AssigneeAccountID: "557058:ada"}

	if evt := findEvent(DiffJiraSnapshots(domain.JiraSnapshot{}, idOnly, testEntityID, JiraEventProjects{}, testDoneStatuses, nil),
		domain.EventJiraIssueAssigned); evt == nil {
		t.Error("first discovery of an issue whose assignee has no display name was not assigned")
	}

	unassigned := domain.JiraSnapshot{Key: "SKY-1", Status: "To Do"}
	if findEvent(DiffJiraSnapshots(idOnly, unassigned, testEntityID, JiraEventProjects{}, testDoneStatuses, nil),
		domain.EventJiraIssueAvailable) == nil {
		t.Error("no available event when an assignee without a display name was removed")
	}
}

// A priority renamed in Jira keeps its id. The issue's priority did not
// change, so no priority_changed event fires for it.
func TestDiffJira_RenamedPriorityIsNotAChange(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Priority: "High", PriorityID: "2"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Priority: "P1", PriorityID: "2"}

	if evt := findEvent(DiffJiraSnapshots(prev, curr, testEntityID, JiraEventProjects{}, testDoneStatuses, nil),
		domain.EventJiraIssuePriorityChanged); evt != nil {
		t.Errorf("a priority rename emitted %s", evt.EventType)
	}
}

// A real priority change carries both priorities' ids beside their names, and
// the dedup key is the new priority's id, so a priority renamed while a task
// waits on it does not open a second one. The id arriving on a snapshot that
// predates it is not a change.
func TestDiffJira_PriorityChangeCarriesIDs(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Priority: "Low", PriorityID: "4"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Priority: "High", PriorityID: "2"}

	evt := findEvent(DiffJiraSnapshots(prev, curr, testEntityID, JiraEventProjects{}, testDoneStatuses, nil),
		domain.EventJiraIssuePriorityChanged)
	if evt == nil {
		t.Fatal("no priority_changed event")
	}
	if evt.DedupKey != "2" {
		t.Errorf("dedup_key = %q, want the new priority's id", evt.DedupKey)
	}
	meta := decodeMetadata[events.JiraIssuePriorityChangedMetadata](t, *evt)
	if meta.OldPriorityID != "4" || meta.NewPriorityID != "2" {
		t.Errorf("priority ids = %q → %q, want 4 → 2", meta.OldPriorityID, meta.NewPriorityID)
	}

	legacy := domain.JiraSnapshot{Key: "SKY-1", Priority: "High"}
	if evt := findEvent(DiffJiraSnapshots(legacy, curr, testEntityID, JiraEventProjects{}, testDoneStatuses, nil),
		domain.EventJiraIssuePriorityChanged); evt != nil {
		t.Error("the priority id arriving emitted priority_changed")
	}
}

// The ids of the issue's priority and issue type ride every event that names
// them.
func TestDiffJira_EventsCarryPriorityAndIssueTypeIDs(t *testing.T) {
	curr := domain.JiraSnapshot{
		Key: "SKY-1", Status: "To Do",
		Priority: "High", PriorityID: "2",
		IssueType: "Bug", IssueTypeID: "10004",
	}
	evt := findEvent(DiffJiraSnapshots(domain.JiraSnapshot{}, curr, testEntityID, JiraEventProjects{}, testDoneStatuses, nil),
		domain.EventJiraIssueAvailable)
	if evt == nil {
		t.Fatal("no available event on first discovery")
	}
	meta := decodeMetadata[events.JiraIssueAvailableMetadata](t, *evt)
	if meta.PriorityID != "2" || meta.IssueTypeID != "10004" {
		t.Errorf("priority_id = %q, issue_type_id = %q; want 2 and 10004", meta.PriorityID, meta.IssueTypeID)
	}
}

// The read records the priority's and the issue type's ids beside their names.
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
