package tracker

import (
	"testing"

	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

func TestDiffPR_FirstSeen_ReviewRequested(t *testing.T) {
	events := DiffPRSnapshots(domain.PRSnapshot{}, domain.PRSnapshot{
		Number:         42,
		State:          "OPEN",
		Author:         "bob",
		ReviewRequests: []string{"alice"},
	}, "42", "alice")

	assertEventTypes(t, events, []string{domain.EventGitHubPRReviewRequested})
}

func TestDiffPR_FirstSeen_Authored(t *testing.T) {
	events := DiffPRSnapshots(domain.PRSnapshot{}, domain.PRSnapshot{
		Number: 42,
		State:  "OPEN",
		Author: "alice",
	}, "42", "alice")

	assertEventTypes(t, events, []string{domain.EventGitHubPROpened})
}

func TestDiffPR_FirstSeen_Mentioned(t *testing.T) {
	events := DiffPRSnapshots(domain.PRSnapshot{}, domain.PRSnapshot{
		Number: 42,
		State:  "OPEN",
		Author: "bob",
	}, "42", "alice")

	assertEventTypes(t, events, []string{domain.EventGitHubPRMentioned})
}

func TestDiffPR_FirstSeen_Merged(t *testing.T) {
	events := DiffPRSnapshots(domain.PRSnapshot{}, domain.PRSnapshot{
		Number: 42,
		State:  "MERGED",
		Merged: true,
	}, "42", "alice")

	assertEventTypes(t, events, []string{domain.EventGitHubPRMerged})
}

func TestDiffPR_CITransition(t *testing.T) {
	prev := domain.PRSnapshot{Number: 42, CIState: "PENDING"}

	// success
	events := DiffPRSnapshots(prev, domain.PRSnapshot{Number: 42, CIState: "SUCCESS"}, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRCIPassed})

	// failure
	events = DiffPRSnapshots(prev, domain.PRSnapshot{Number: 42, CIState: "FAILURE"}, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRCIFailed})

	// no change → no events
	events = DiffPRSnapshots(prev, domain.PRSnapshot{Number: 42, CIState: "PENDING"}, "42", "")
	assertEventTypes(t, events, nil)
}

func TestDiffPR_Merged(t *testing.T) {
	prev := domain.PRSnapshot{Number: 42, State: "OPEN", Merged: false}
	curr := domain.PRSnapshot{Number: 42, State: "MERGED", Merged: true}

	events := DiffPRSnapshots(prev, curr, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRMerged})
}

func TestDiffPR_DraftToReady(t *testing.T) {
	prev := domain.PRSnapshot{Number: 42, IsDraft: true}
	curr := domain.PRSnapshot{Number: 42, IsDraft: false}

	events := DiffPRSnapshots(prev, curr, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRReadyForReview})
}

func TestDiffPR_Conflicts(t *testing.T) {
	prev := domain.PRSnapshot{Number: 42, Mergeable: "MERGEABLE"}
	curr := domain.PRSnapshot{Number: 42, Mergeable: "CONFLICTING"}

	events := DiffPRSnapshots(prev, curr, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRConflicts})
}

func TestDiffPR_ReviewRequestAdded(t *testing.T) {
	prev := domain.PRSnapshot{Number: 42, ReviewRequests: []string{"alice"}}
	curr := domain.PRSnapshot{Number: 42, ReviewRequests: []string{"alice", "bob"}}

	events := DiffPRSnapshots(prev, curr, "42", "bob")
	assertEventTypes(t, events, []string{domain.EventGitHubPRReviewRequested})
	assertMetaContains(t, events[0], "requested_reviewer", "bob")
}

func TestDiffPR_ReviewRequestAdded_OtherUser(t *testing.T) {
	// Review requested for someone else — should NOT fire for us
	prev := domain.PRSnapshot{Number: 42, ReviewRequests: []string{"alice"}}
	curr := domain.PRSnapshot{Number: 42, ReviewRequests: []string{"alice", "bob"}}

	events := DiffPRSnapshots(prev, curr, "42", "charlie")
	assertEventTypes(t, events, nil)
}

func TestDiffPR_ReviewRequestReAdded(t *testing.T) {
	// Simulates: alice reviewed, request removed, then re-requested
	prev := domain.PRSnapshot{Number: 42, ReviewRequests: nil}
	curr := domain.PRSnapshot{Number: 42, ReviewRequests: []string{"alice"}}

	events := DiffPRSnapshots(prev, curr, "42", "alice")
	assertEventTypes(t, events, []string{domain.EventGitHubPRReviewRequested})
}

func TestDiffPR_ReviewApproved(t *testing.T) {
	prev := domain.PRSnapshot{
		Number:  42,
		Author:  "me",
		Reviews: []domain.ReviewState{{Author: "alice", State: "COMMENTED"}},
	}
	curr := domain.PRSnapshot{
		Number:  42,
		Author:  "me",
		Reviews: []domain.ReviewState{{Author: "alice", State: "APPROVED"}},
	}

	events := DiffPRSnapshots(prev, curr, "42", "me")
	assertEventTypes(t, events, []string{domain.EventGitHubPRApproved})
}

func TestDiffPR_ReviewApproved_NotAuthor(t *testing.T) {
	// Review state changed on a PR we don't own — no event
	prev := domain.PRSnapshot{
		Number:  42,
		Author:  "bob",
		Reviews: []domain.ReviewState{{Author: "alice", State: "COMMENTED"}},
	}
	curr := domain.PRSnapshot{
		Number:  42,
		Author:  "bob",
		Reviews: []domain.ReviewState{{Author: "alice", State: "APPROVED"}},
	}

	events := DiffPRSnapshots(prev, curr, "42", "me")
	assertEventTypes(t, events, nil)
}

func TestDiffPR_ChangesRequested(t *testing.T) {
	prev := domain.PRSnapshot{
		Number:  42,
		Author:  "me",
		Reviews: []domain.ReviewState{{Author: "alice", State: "APPROVED"}},
	}
	curr := domain.PRSnapshot{
		Number:  42,
		Author:  "me",
		Reviews: []domain.ReviewState{{Author: "alice", State: "CHANGES_REQUESTED"}},
	}

	events := DiffPRSnapshots(prev, curr, "42", "me")
	assertEventTypes(t, events, []string{domain.EventGitHubPRChangesReq})
}

func TestDiffPR_MultipleEvents(t *testing.T) {
	prev := domain.PRSnapshot{
		Number:    42,
		CIState:   "PENDING",
		Mergeable: "UNKNOWN",
		IsDraft:   true,
	}
	curr := domain.PRSnapshot{
		Number:    42,
		CIState:   "SUCCESS",
		Mergeable: "CONFLICTING",
		IsDraft:   false,
	}

	events := DiffPRSnapshots(prev, curr, "42", "")
	types := eventTypes(events)

	assertContains(t, types, domain.EventGitHubPRCIPassed)
	assertContains(t, types, domain.EventGitHubPRConflicts)
	assertContains(t, types, domain.EventGitHubPRReadyForReview)
}

func TestDiffPR_NoChange(t *testing.T) {
	snap := domain.PRSnapshot{
		Number:    42,
		CIState:   "SUCCESS",
		Mergeable: "MERGEABLE",
		Reviews:   []domain.ReviewState{{Author: "alice", State: "APPROVED"}},
	}

	events := DiffPRSnapshots(snap, snap, "42", "")
	assertEventTypes(t, events, nil)
}

func TestDiffJira_FirstSeen(t *testing.T) {
	events := DiffJiraSnapshots(domain.JiraSnapshot{}, domain.JiraSnapshot{
		Key: "SKY-1", Status: "To Do",
	}, "SKY-1")
	assertEventTypes(t, events, []string{domain.EventJiraIssueAvailable})

	events = DiffJiraSnapshots(domain.JiraSnapshot{}, domain.JiraSnapshot{
		Key: "SKY-2", Status: "In Progress", Assignee: "alice",
	}, "SKY-2")
	assertEventTypes(t, events, []string{domain.EventJiraIssueAssigned})
}

func TestDiffJira_StatusChange(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Status: "To Do"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Status: "In Progress"}

	events := DiffJiraSnapshots(prev, curr, "SKY-1")
	assertEventTypes(t, events, []string{domain.EventJiraIssueStatusChanged})
}

func TestDiffJira_Completed(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Status: "In Progress"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Status: "Done"}

	events := DiffJiraSnapshots(prev, curr, "SKY-1")
	types := eventTypes(events)
	assertContains(t, types, domain.EventJiraIssueStatusChanged)
	assertContains(t, types, domain.EventJiraIssueCompleted)
}

func TestDiffJira_Unassigned(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Status: "To Do", Assignee: "alice"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Status: "To Do", Assignee: ""}

	events := DiffJiraSnapshots(prev, curr, "SKY-1")
	assertEventTypes(t, events, []string{domain.EventJiraIssueAvailable})
}

func TestDiffJira_PriorityChange(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", Priority: "Medium"}
	curr := domain.JiraSnapshot{Key: "SKY-1", Priority: "High"}

	events := DiffJiraSnapshots(prev, curr, "SKY-1")
	assertEventTypes(t, events, []string{domain.EventJiraIssuePriorityChanged})
}

func TestDiffJira_NewComment(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", CommentCount: 2}
	curr := domain.JiraSnapshot{Key: "SKY-1", CommentCount: 3}

	events := DiffJiraSnapshots(prev, curr, "SKY-1")
	assertEventTypes(t, events, []string{domain.EventJiraIssueCommented})
}

func TestDiffJira_MultipleNewComments(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", CommentCount: 1}
	curr := domain.JiraSnapshot{Key: "SKY-1", CommentCount: 4}

	// Should fire once, not three times — it's "new comments detected", not per-comment
	events := DiffJiraSnapshots(prev, curr, "SKY-1")
	assertEventTypes(t, events, []string{domain.EventJiraIssueCommented})
}

func TestDiffJira_NoNewComments(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "SKY-1", CommentCount: 3}
	curr := domain.JiraSnapshot{Key: "SKY-1", CommentCount: 3}

	events := DiffJiraSnapshots(prev, curr, "SKY-1")
	assertEventTypes(t, events, nil)
}

// --- test helpers ---

func eventTypes(events []domain.Event) []string {
	var types []string
	for _, e := range events {
		types = append(types, e.EventType)
	}
	return types
}

func assertEventTypes(t *testing.T, events []domain.Event, expected []string) {
	t.Helper()
	got := eventTypes(events)
	if len(got) != len(expected) {
		t.Fatalf("expected %d events %v, got %d: %v", len(expected), expected, len(got), got)
	}
	for i := range expected {
		if got[i] != expected[i] {
			t.Errorf("event[%d]: expected %s, got %s", i, expected[i], got[i])
		}
	}
}

func assertContains(t *testing.T, types []string, expected string) {
	t.Helper()
	for _, typ := range types {
		if typ == expected {
			return
		}
	}
	t.Errorf("expected events to contain %s, got %v", expected, types)
}

func assertMetaContains(t *testing.T, event domain.Event, key, value string) {
	t.Helper()
	if event.Metadata == "" {
		t.Fatalf("event metadata is empty")
	}
	// Simple substring check — metadata is JSON
	if !contains(event.Metadata, value) {
		t.Errorf("expected metadata to contain %s=%s, got %s", key, value, event.Metadata)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
