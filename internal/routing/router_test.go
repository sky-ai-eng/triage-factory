package routing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// --- matchPredicate unit tests ----------------------------------------------
// These validate the predicate-matching logic the router uses to evaluate
// task_rules and prompt_triggers against events. The type-erased matcher
// is already tested in the events package; these tests exercise the router's
// specific calling convention (empty predJSON = match-all, unknown event
// type = no match, etc.).

func TestMatchPredicate_EmptyPredicate_MatchesAll(t *testing.T) {
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:    "aidan",
		CheckName: "build",
		Repo:      "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)

	matched, err := matchPredicate(domain.EventGitHubPRCICheckFailed, "", string(metaJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !matched {
		t.Error("empty predicate should match all events")
	}
}

func TestMatchPredicate_AuthorIn_Match(t *testing.T) {
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:    "aidan",
		CheckName: "build",
	}
	metaJSON, _ := json.Marshal(meta)

	matched, err := matchPredicate(domain.EventGitHubPRCICheckFailed, `{"author_in":["aidan"]}`, string(metaJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !matched {
		t.Error("expected match when Author is in author_in allowlist")
	}
}

func TestMatchPredicate_AuthorIn_NoMatch(t *testing.T) {
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:    "someone-else",
		CheckName: "build",
	}
	metaJSON, _ := json.Marshal(meta)

	matched, err := matchPredicate(domain.EventGitHubPRCICheckFailed, `{"author_in":["aidan"]}`, string(metaJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matched {
		t.Error("expected no match when Author isn't in author_in allowlist")
	}
}

// author_in is case-insensitive — GitHub logins are case-insensitive on the
// platform side too. The matcher uses strings.EqualFold at compare time.
func TestMatchPredicate_AuthorIn_CaseInsensitive(t *testing.T) {
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:    "AidanAllchin",
		CheckName: "build",
	}
	metaJSON, _ := json.Marshal(meta)

	matched, _ := matchPredicate(domain.EventGitHubPRCICheckFailed,
		`{"author_in":["aidanallchin"]}`, string(metaJSON))
	if !matched {
		t.Error("expected case-insensitive author_in match")
	}
}

func TestMatchPredicate_MultiField_AND(t *testing.T) {
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:    "aidan",
		CheckName: "test",
		Repo:      "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)

	// Both fields match.
	matched, _ := matchPredicate(domain.EventGitHubPRCICheckFailed,
		`{"author_in":["aidan"],"check_name":"test"}`, string(metaJSON))
	if !matched {
		t.Error("expected match when both fields pass")
	}

	// One field fails.
	matched, _ = matchPredicate(domain.EventGitHubPRCICheckFailed,
		`{"author_in":["aidan"],"check_name":"build"}`, string(metaJSON))
	if matched {
		t.Error("expected no match when one field fails (AND semantics)")
	}
}

func TestMatchPredicate_HasLabel(t *testing.T) {
	meta := events.GitHubPRNewCommitsMetadata{
		Author: "aidan",
		Labels: []string{"wip", "self-review"},
		Repo:   "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)

	// Label present.
	matched, _ := matchPredicate(domain.EventGitHubPRNewCommits,
		`{"has_label":"self-review"}`, string(metaJSON))
	if !matched {
		t.Error("expected match when HasLabel=self-review and label is present")
	}

	// Label absent.
	matched, _ = matchPredicate(domain.EventGitHubPRNewCommits,
		`{"has_label":"urgent"}`, string(metaJSON))
	if matched {
		t.Error("expected no match when HasLabel=urgent but label is absent")
	}
}

func TestMatchPredicate_LabelName_OnLabelEvent(t *testing.T) {
	meta := events.GitHubPRLabelAddedMetadata{
		Author:    "aidan",
		LabelName: "self-review",
		Labels:    []string{"self-review"},
		Repo:      "owner/repo",
	}
	metaJSON, _ := json.Marshal(meta)

	matched, _ := matchPredicate(domain.EventGitHubPRLabelAdded,
		`{"label_name":"self-review","author_in":["aidan"]}`, string(metaJSON))
	if !matched {
		t.Error("expected match on label_name + author_in")
	}

	matched, _ = matchPredicate(domain.EventGitHubPRLabelAdded,
		`{"label_name":"urgent"}`, string(metaJSON))
	if matched {
		t.Error("expected no match when label_name doesn't match")
	}
}

func TestMatchPredicate_UnknownEventType_NoMatch(t *testing.T) {
	matched, err := matchPredicate("github:pr:does_not_exist", `{"author_in":["aidan"]}`, `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matched {
		t.Error("unknown event type should not match")
	}
}

// Jira predicates moved from *_is_self booleans to *_in
// allowlists of Atlassian account IDs, mirroring the GitHub shape.
// The matcher resolves an event's actor via the metadata's
// assignee_account_id and looks for case-insensitive membership in the
// rule's assignee_in slice.
func TestMatchPredicate_JiraAssigneeIn(t *testing.T) {
	const myAccountID = "557058:abc-aidan"
	meta := events.JiraIssueAssignedMetadata{
		Assignee:          "Aidan Allchin",
		AssigneeAccountID: myAccountID,
		IssueKey:          "SKY-123",
		Project:           "SKY",
	}
	metaJSON, _ := json.Marshal(meta)

	pred := `{"assignee_in":["` + myAccountID + `"]}`
	matched, _ := matchPredicate(domain.EventJiraIssueAssigned, pred, string(metaJSON))
	if !matched {
		t.Error("expected match when assignee_account_id is in assignee_in allowlist")
	}

	// Reassigned to someone else — metadata carries a different account ID,
	// which isn't in the allowlist.
	meta.AssigneeAccountID = "557058:xyz-bob"
	meta.Assignee = "Bob"
	metaJSON, _ = json.Marshal(meta)
	matched, _ = matchPredicate(domain.EventJiraIssueAssigned, pred, string(metaJSON))
	if matched {
		t.Error("expected no match when assignee_account_id is not in allowlist")
	}

	// Empty allowlist = match-all (same convention as author_in).
	meta.AssigneeAccountID = myAccountID
	metaJSON, _ = json.Marshal(meta)
	matched, _ = matchPredicate(domain.EventJiraIssueAssigned,
		`{"assignee_in":[]}`, string(metaJSON))
	if !matched {
		t.Error("expected match when assignee_in is empty (no filter)")
	}
}

// Self-review scenario: combine author_in + reviewer_in + has_label so a
// trigger only fires when the user reviews their own PR with a specific
// label set.
func TestMatchPredicate_ReviewerIn_SelfReview(t *testing.T) {
	meta := events.GitHubPRReviewCommentedMetadata{
		Author:   "aidan",
		Reviewer: "aidan",
		Labels:   []string{"self-review"},
	}
	metaJSON, _ := json.Marshal(meta)

	matched, _ := matchPredicate(domain.EventGitHubPRReviewCommented,
		`{"reviewer_in":["aidan"],"author_in":["aidan"],"has_label":"self-review"}`,
		string(metaJSON))
	if !matched {
		t.Error("expected match for self-review scenario predicate")
	}

	// External reviewer.
	meta.Reviewer = "alice"
	metaJSON, _ = json.Marshal(meta)
	matched, _ = matchPredicate(domain.EventGitHubPRReviewCommented,
		`{"reviewer_in":["aidan"],"author_in":["aidan"],"has_label":"self-review"}`,
		string(metaJSON))
	if matched {
		t.Error("expected no match when reviewer isn't in reviewer_in allowlist")
	}
}

// --- EntityTerminatingEvents ------------------------------------------------

func TestEntityTerminatingEvents(t *testing.T) {
	terminators := []string{
		domain.EventGitHubPRMerged,
		domain.EventGitHubPRClosed,
		domain.EventGitHubPRUnreachable,
		domain.EventJiraIssueCompleted,
		domain.EventJiraIssueUnreachable,
		domain.EventLinearIssueCompleted,
		domain.EventLinearIssueUnreachable,
	}
	for _, et := range terminators {
		if !EntityTerminatingEvents[et] {
			t.Errorf("expected %q to be entity-terminating", et)
		}
	}

	nonTerminators := []string{
		domain.EventGitHubPRCICheckFailed,
		domain.EventGitHubPRNewCommits,
		domain.EventGitHubPRReviewRequested,
		domain.EventGitHubPRReviewRequestRemoved,
		domain.EventJiraIssueAssigned,
		domain.EventLinearIssueAssigned,
		domain.EventLinearIssueBecameAtomic,
	}
	for _, et := range nonTerminators {
		if EntityTerminatingEvents[et] {
			t.Errorf("expected %q to NOT be entity-terminating", et)
		}
	}
}

// TestMatchHandlers_MatchesThroughTheCurrentView is the event-time twin: an
// event recorded under a replaced id matches a filter naming its replacement,
// and a view that cannot answer is a failed match, never "nothing matched".
func TestMatchHandlers_MatchesThroughTheCurrentView(t *testing.T) {
	var viewErr error
	events.RegisterCurrentView(domain.EventGitHubPRCICheckFailed, func(_ context.Context, _, meta string) (string, error) {
		if viewErr != nil {
			return "", viewErr
		}
		return strings.ReplaceAll(meta, `"author":"old-login"`, `"author":"aidan"`), nil
	})
	t.Cleanup(func() { events.ResetCurrentView(domain.EventGitHubPRCICheckFailed) })

	database := newTestDB(t)
	createTestPrompt(t, database, domain.Prompt{ID: "p5", Name: "Test5", Body: "Do", Source: "user"})
	pred := `{"author_in":["aidan"]}`
	createTriggerForTestRouting(t, database, domain.EventHandler{
		ID: "t-view-event", Kind: domain.EventHandlerKindTrigger,
		BlueprintID: "p5", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventGitHubPRCICheckFailed, BreakerThreshold: intPtr(4),
		MinAutonomySuitability: floatPtr(0), Enabled: true,
		ScopePredicateJSON: &pred,
	})
	r := reDeriveRouter(t, database, nil)

	metaJSON, _ := json.Marshal(events.GitHubPRCICheckFailedMetadata{Author: "old-login", CheckName: "build", Repo: "owner/repo"})
	evt := domain.Event{OrgID: runmode.LocalDefaultOrgID, EventType: domain.EventGitHubPRCICheckFailed, MetadataJSON: string(metaJSON)}

	_, triggers, err := r.matchHandlers(context.Background(), runmode.LocalDefaultOrgID, evt, map[string]bool{})
	if err != nil || len(triggers) != 1 || triggers[0].ID != "t-view-event" {
		t.Errorf("matchHandlers = %+v, %v; want t-view-event matched through the view", triggers, err)
	}

	viewErr = errors.New("db down")
	if _, _, err := r.matchHandlers(context.Background(), runmode.LocalDefaultOrgID, evt, map[string]bool{}); err == nil {
		t.Error("matchHandlers with a failing view = nil error; want the view's error")
	}
}
