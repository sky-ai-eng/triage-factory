package tracker

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// A status change is keyed on the new status's id and carries both statuses'
// ids beside their names, so a status renamed while a task waits on it does
// not open a second task the next time the issue enters it, and a
// new_status_id_in filter matches the event.
func TestDiffJira_StatusChangeKeyedOnTheStatusID(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Status: "To Do", StatusID: "10000"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Status: "In Review", StatusID: "10003"}

	evt := findEvent(DiffJiraSnapshots(prev, curr, testEntityID, JiraEventProjects{}, testDoneStatuses, nil),
		domain.EventJiraIssueStatusChanged)
	if evt == nil {
		t.Fatal("no status_changed event")
	}
	if evt.DedupKey != "10003" {
		t.Errorf("dedup_key = %q, want the new status's id", evt.DedupKey)
	}
	meta := decodeMetadata[events.JiraIssueStatusChangedMetadata](t, *evt)
	if meta.OldStatusID != "10000" || meta.NewStatusID != "10003" || meta.OldStatus != "To Do" || meta.NewStatus != "In Review" {
		t.Errorf("statuses = %q (%q) → %q (%q), want To Do (10000) → In Review (10003)",
			meta.OldStatus, meta.OldStatusID, meta.NewStatus, meta.NewStatusID)
	}
	if !matchesPredicate(t, *evt, `{"new_status_id_in":["10003"],"old_status_id_in":["10000"]}`) {
		t.Errorf("the status ids did not match %s", evt.MetadataJSON)
	}

	renamed := curr
	renamed.Status = "Peer Review"
	again := findEvent(DiffJiraSnapshots(prev, renamed, testEntityID, JiraEventProjects{}, testDoneStatuses, nil),
		domain.EventJiraIssueStatusChanged)
	if again == nil || again.DedupKey != evt.DedupKey {
		t.Errorf("entering the renamed status keyed %v, want the same key as before the rename", again)
	}
}

// A read that carries no status or priority id keys the change on the name,
// the only identity it has.
func TestDiffJira_ChangeWithoutAnIDKeyedOnTheName(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Status: "To Do", Priority: "Low"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Status: "Done", Priority: "High"}

	evts := DiffJiraSnapshots(prev, curr, testEntityID, JiraEventProjects{}, nil, nil)
	if evt := findEvent(evts, domain.EventJiraIssueStatusChanged); evt == nil || evt.DedupKey != "Done" {
		t.Errorf("status_changed = %v, want it keyed on Done", evt)
	}
	if evt := findEvent(evts, domain.EventJiraIssuePriorityChanged); evt == nil || evt.DedupKey != "High" {
		t.Errorf("priority_changed = %v, want it keyed on High", evt)
	}
}

// Every event that names the issue's status carries its id: the events a rule
// filters with status_id_in, and completed's final status.
func TestDiffJira_EventsCarryTheStatusID(t *testing.T) {
	curr := domain.JiraSnapshot{Key: "SKY-1", Status: "To Do", StatusID: "10000", Labels: []string{}}
	evt := findEvent(DiffJiraSnapshots(domain.JiraSnapshot{}, curr, testEntityID, JiraEventProjects{}, testDoneStatuses, nil),
		domain.EventJiraIssueAvailable)
	if evt == nil {
		t.Fatal("no available event on first discovery")
	}
	if meta := decodeMetadata[events.JiraIssueAvailableMetadata](t, *evt); meta.StatusID != "10000" {
		t.Errorf("available status_id = %q, want 10000", meta.StatusID)
	}

	labelled := curr
	labelled.Labels = []string{"tf-agent"}
	evt = findEvent(DiffJiraSnapshots(curr, labelled, testEntityID, JiraEventProjects{}, testDoneStatuses, nil),
		domain.EventJiraIssueLabelAdded)
	if evt == nil {
		t.Fatal("no label_added event")
	}
	if meta := decodeMetadata[events.JiraIssueLabelMetadata](t, *evt); meta.StatusID != "10000" {
		t.Errorf("label_added status_id = %q, want 10000", meta.StatusID)
	}

	done := domain.JiraSnapshot{Key: "SKY-1", Status: "Done", StatusID: "10002"}
	doneSet := []domain.JiraStatusRef{{ID: "10002", Name: "Done"}}
	evt = findEvent(DiffJiraSnapshots(curr, done, testEntityID, JiraEventProjects{}, doneSet, nil),
		domain.EventJiraIssueCompleted)
	if evt == nil {
		t.Fatal("no completed event")
	}
	if meta := decodeMetadata[events.JiraIssueCompletedMetadata](t, *evt); meta.FinalStatusID != "10002" {
		t.Errorf("completed final_status_id = %q, want 10002", meta.FinalStatusID)
	}
}
