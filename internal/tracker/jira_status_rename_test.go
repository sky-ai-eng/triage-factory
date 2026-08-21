package tracker

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// A status renamed in Jira is not a transition. The ticket did not move, so
// nothing downstream should behave as though it did: no status-change event,
// no task minted from one, and no spurious completion. Before status ids were
// recorded this was indistinguishable from a real move, and one rename emitted
// an event for every issue sitting in the renamed status.
func TestDiffJira_RenamedStatusIsNotATransition(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Status: "In Review", StatusID: "10005"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Status: "In Review (QA)", StatusID: "10005"}

	evts := DiffJiraSnapshots(prev, curr, testEntityID, testDoneStatuses)
	if evt := findEvent(evts, domain.EventJiraIssueStatusChanged); evt != nil {
		t.Errorf("a rename emitted %s (%q → %q); the issue never moved",
			evt.EventType, prev.Status, curr.Status)
	}
}

// The id decides only when both snapshots carry one. A snapshot captured
// before the field existed has none, so the comparison falls back to the name
// and a real move is still seen.
func TestDiffJira_StatusChangeSeenWithoutIDs(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Status: "To Do"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Status: "In Review"}

	if findEvent(DiffJiraSnapshots(prev, curr, testEntityID, testDoneStatuses),
		domain.EventJiraIssueStatusChanged) == nil {
		t.Error("no status_changed event for a real move between id-less snapshots")
	}
}

// The upgrade cycle: the prior snapshot predates status ids, the fresh one has
// one. That is not a transition either — the name is unchanged, and the id
// arriving is a detail of what we now record, not something the ticket did.
func TestDiffJira_IDArrivingIsNotATransition(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Status: "In Review"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Status: "In Review", StatusID: "10005"}

	if evt := findEvent(DiffJiraSnapshots(prev, curr, testEntityID, testDoneStatuses),
		domain.EventJiraIssueStatusChanged); evt != nil {
		t.Error("the first poll after the status id landed emitted a status change")
	}
}

// Terminal classification follows the same rule, which is what keeps a rename
// from silently stopping auto-close: the rule stores the id it was armed with,
// and the snapshot reports the new spelling of the same status.
func TestDiffJira_TerminalMatchesRenamedDoneStatus(t *testing.T) {
	done := []domain.JiraStatusRef{{ID: "10100", Name: "Done"}}
	prev := domain.JiraSnapshot{Key: "SKY-1", Status: "In Progress", StatusID: "10005"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Status: "Shipped", StatusID: "10100"}

	if findEvent(DiffJiraSnapshots(prev, curr, testEntityID, done),
		domain.EventJiraIssueCompleted) == nil {
		t.Error("no completed event: the Done status was renamed, and the id match should have caught it")
	}
}

// The name fallback is not a transitional state — a rule seeded from the
// headless env vars carries no ids at all, permanently.
func TestDiffJira_TerminalMatchesNameOnlyRule(t *testing.T) {
	done := []domain.JiraStatusRef{{Name: "Done"}}
	prev := domain.JiraSnapshot{Key: "SKY-1", Status: "In Progress", StatusID: "10005"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Status: "Done", StatusID: "10100"}

	if findEvent(DiffJiraSnapshots(prev, curr, testEntityID, done),
		domain.EventJiraIssueCompleted) == nil {
		t.Error("no completed event for a name-only rule; headless deployments have nothing else")
	}
}
