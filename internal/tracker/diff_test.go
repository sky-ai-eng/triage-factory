package tracker

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
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

func TestDiffPR_FirstSeen_Closed(t *testing.T) {
	events := DiffPRSnapshots(domain.PRSnapshot{}, domain.PRSnapshot{
		Number: 42,
		State:  "CLOSED",
		Merged: false,
	}, "42", "alice")

	assertEventTypes(t, events, []string{domain.EventGitHubPRClosed})
}

func TestDiffPR_OpenToClosed(t *testing.T) {
	prev := domain.PRSnapshot{Number: 42, State: "OPEN", Merged: false}
	curr := domain.PRSnapshot{Number: 42, State: "CLOSED", Merged: false}

	events := DiffPRSnapshots(prev, curr, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRClosed})
}

func TestDiffPR_OpenToMerged_EmitsMergedNotClosed(t *testing.T) {
	// Merged PRs (Merged=true) should emit github:pr:merged, not github:pr:closed
	prev := domain.PRSnapshot{Number: 42, State: "OPEN", Merged: false}
	curr := domain.PRSnapshot{Number: 42, State: "MERGED", Merged: true}

	events := DiffPRSnapshots(prev, curr, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRMerged})
}

func TestDiffPR_CITransition(t *testing.T) {
	// Baseline: one check, still running.
	pending := domain.PRSnapshot{
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 1, Name: "test", Status: "in_progress"},
		},
	}

	// pending → same id completing as success: fires CIPassed
	events := DiffPRSnapshots(pending, domain.PRSnapshot{
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 1, Name: "test", Status: "completed", Conclusion: "success"},
		},
	}, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRCIPassed})

	// pending → same id completing as failure: fires CIFailed
	// (case 2 in the diff logic: existing ID, prior conclusion wasn't failing)
	events = DiffPRSnapshots(pending, domain.PRSnapshot{
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 1, Name: "test", Status: "completed", Conclusion: "failure"},
		},
	}, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRCIFailed})
	assertMetaContains(t, events[0], "primary_check_name", "test")
	assertMetaContains(t, events[0], "primary_conclusion", "failure")

	// no change → no events
	events = DiffPRSnapshots(pending, pending, "42", "")
	assertEventTypes(t, events, nil)

	// nil prev.CheckRuns (unknown prior state, e.g. old snapshot) → no fire
	// even if curr has a failing check. Guards against spurious events on
	// the first poll after this field landed.
	events = DiffPRSnapshots(
		domain.PRSnapshot{Number: 42, CheckRuns: nil},
		domain.PRSnapshot{
			Number: 42,
			CheckRuns: []domain.CheckRun{
				{ID: 99, Name: "test", Status: "completed", Conclusion: "failure"},
			},
		},
		"42", "",
	)
	assertEventTypes(t, events, nil)
}

// TestDiffPR_CIFailure_Scenario_B reproduces the "missed PENDING" bug that
// the old scalar-CIState logic could not detect:
//
//	FAILURE → fix pushed → poller sees FAILURE again on the new SHA without
//	having observed the intermediate PENDING → old aggregate stayed FAILURE
//	→ no event fired → auto-delegation stuck.
//
// With per-check-run identity, the retry produces a new check_run_id at the
// new SHA, and the diff logic fires on the new ID.
func TestDiffPR_CIFailure_Scenario_B_RetryAtNewSHA(t *testing.T) {
	prev := domain.PRSnapshot{
		Number:  42,
		HeadSHA: "abc123",
		CheckRuns: []domain.CheckRun{
			{ID: 100, Name: "test", Status: "completed", Conclusion: "failure"},
		},
	}
	curr := domain.PRSnapshot{
		Number:  42,
		HeadSHA: "def456",
		CheckRuns: []domain.CheckRun{
			{ID: 200, Name: "test", Status: "completed", Conclusion: "failure"},
		},
	}

	events := DiffPRSnapshots(prev, curr, "42", "")
	types := eventTypes(events)
	assertContains(t, types, domain.EventGitHubPRCIFailed)
	assertContains(t, types, domain.EventGitHubPRNewCommits)
}

// TestDiffPR_CIFailure_Scenario_C reproduces the "different check fails"
// bug that the old scalar-CIState logic could not detect:
//
//	Check A fails → agent fixes A → rerun → A passes, B newly fails →
//	aggregate rollup stays FAILURE → no transition → no event fired →
//	auto-delegation stuck.
//
// With per-check-run identity, B's new failing ID isn't in prev, so fires.
func TestDiffPR_CIFailure_Scenario_C_DifferentCheckFails(t *testing.T) {
	prev := domain.PRSnapshot{
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 100, Name: "unit", Status: "completed", Conclusion: "failure"},
			{ID: 101, Name: "integration", Status: "completed", Conclusion: "success"},
		},
	}
	curr := domain.PRSnapshot{
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 200, Name: "unit", Status: "completed", Conclusion: "success"},
			{ID: 201, Name: "integration", Status: "completed", Conclusion: "failure"},
		},
	}

	events := DiffPRSnapshots(prev, curr, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRCIFailed})
	assertMetaContains(t, events[0], "primary_check_name", "integration")
	assertMetaContains(t, events[0], "count", "1")
}

// TestDiffPR_CIPassed_PermissiveConclusions verifies that the aggregate
// CIPassed transition fires for check-run conclusions outside the narrow
// success/skipped/neutral set — specifically "stale" (which GitHub emits
// after a rebase and treats as non-blocking), empty conclusion on a
// completed run, and any future enum values. Regression guard for a real
// bug where CIStatusFromCheckRuns only recognized three conclusions,
// causing stable "stale" PRs to never report the aggregate success state.
func TestDiffPR_CIPassed_PermissiveConclusions(t *testing.T) {
	prev := domain.PRSnapshot{
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 1, Name: "test", Status: "in_progress"},
		},
	}

	// stale → success-like, fires CIPassed
	events := DiffPRSnapshots(prev, domain.PRSnapshot{
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 1, Name: "test", Status: "completed", Conclusion: "stale"},
		},
	}, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRCIPassed})

	// empty conclusion on a completed run → also success-like
	events = DiffPRSnapshots(prev, domain.PRSnapshot{
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 1, Name: "test", Status: "completed", Conclusion: ""},
		},
	}, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRCIPassed})

	// Mixed: one stale + one success → still passes
	events = DiffPRSnapshots(prev, domain.PRSnapshot{
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 1, Name: "test", Status: "completed", Conclusion: "success"},
			{ID: 2, Name: "lint", Status: "completed", Conclusion: "stale"},
		},
	}, "42", "")
	assertContains(t, eventTypes(events), domain.EventGitHubPRCIPassed)
}

// Stable CI — same IDs, same conclusions poll-to-poll — fires nothing.
func TestDiffPR_CIFailure_StableCI_NoEvent(t *testing.T) {
	snap := domain.PRSnapshot{
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 100, Name: "test", Status: "completed", Conclusion: "failure"},
		},
	}
	events := DiffPRSnapshots(snap, snap, "42", "")
	assertEventTypes(t, events, nil)
}

// Multiple checks newly fail in the same poll — fire once with a count and
// the full list in metadata, not one event per failing check.
func TestDiffPR_CIFailure_MultipleNewFailures_OneEvent(t *testing.T) {
	prev := domain.PRSnapshot{
		Number:    42,
		CheckRuns: []domain.CheckRun{},
	}
	curr := domain.PRSnapshot{
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 10, Name: "build", Status: "completed", Conclusion: "failure"},
			{ID: 11, Name: "lint", Status: "completed", Conclusion: "failure"},
			{ID: 12, Name: "test", Status: "completed", Conclusion: "failure"},
		},
	}

	events := DiffPRSnapshots(prev, curr, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRCIFailed})
	assertMetaContains(t, events[0], "count", "3")
	assertMetaContains(t, events[0], "failing_checks", "build")
	assertMetaContains(t, events[0], "failing_checks", "lint")
	assertMetaContains(t, events[0], "failing_checks", "test")
}

func TestDiffPR_NewCommits(t *testing.T) {
	// head_sha changed → fires with before/after metadata
	prev := domain.PRSnapshot{Number: 42, HeadSHA: "abc123"}
	curr := domain.PRSnapshot{Number: 42, HeadSHA: "def456"}

	events := DiffPRSnapshots(prev, curr, "42", "")
	assertEventTypes(t, events, []string{domain.EventGitHubPRNewCommits})
	assertMetaContains(t, events[0], "new", "def456")
	assertMetaContains(t, events[0], "prev", "abc123")

	// head_sha unchanged → no event
	events = DiffPRSnapshots(prev, prev, "42", "")
	assertEventTypes(t, events, nil)

	// prev empty (first poll populating the field) → no spurious event
	events = DiffPRSnapshots(
		domain.PRSnapshot{Number: 42, HeadSHA: ""},
		domain.PRSnapshot{Number: 42, HeadSHA: "abc123"},
		"42", "",
	)
	assertEventTypes(t, events, nil)

	// curr empty (refresh lost the field) → no spurious event
	events = DiffPRSnapshots(
		domain.PRSnapshot{Number: 42, HeadSHA: "abc123"},
		domain.PRSnapshot{Number: 42, HeadSHA: ""},
		"42", "",
	)
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
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 1, Name: "test", Status: "in_progress"},
		},
		Mergeable: "UNKNOWN",
		IsDraft:   true,
	}
	curr := domain.PRSnapshot{
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 1, Name: "test", Status: "completed", Conclusion: "success"},
		},
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
		Number: 42,
		CheckRuns: []domain.CheckRun{
			{ID: 1, Name: "test", Status: "completed", Conclusion: "success"},
		},
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
	if event.MetadataJSON == "" {
		t.Fatalf("event metadata is empty")
	}
	// Simple substring check — metadata is JSON
	if !contains(event.MetadataJSON, value) {
		t.Errorf("expected metadata to contain %s=%s, got %s", key, value, event.MetadataJSON)
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
