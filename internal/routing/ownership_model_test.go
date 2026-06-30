package routing

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestOwnershipModelForEvent pins the explicit event→model classification
// (TFAC-519) — the single dispatch key resolveTeamRouting branches on. The
// precedence guard matters: review_requested is a github:pr:* type that must
// resolve to RequestedParty, NOT Owned, even though the author-centric set is
// checked right after.
func TestOwnershipModelForEvent(t *testing.T) {
	cases := []struct {
		eventType string
		want      ownershipModel
		why       string
	}{
		{domain.EventGitHubPRReviewRequested, modelRequestedParty, "review_requested routes to the requested reviewer"},
		{domain.EventGitHubPRCICheckFailed, modelOwned, "author-centric github → owning-team ladder"},
		{domain.EventGitHubPRConflicts, modelOwned, "author-centric github → owning-team ladder"},
		{domain.EventJiraIssueAssigned, modelOwned, "assignee-centric jira → owning-team ladder"},
		{domain.EventJiraIssueCommented, modelOwned, "assignee-centric jira → owning-team ladder"},
		{domain.EventJiraIssueAvailable, modelPool, "unassigned team-pool signal → handler-team grouping"},
		{"some:unregistered:event", modelPool, "unclassified types fall to the pool default"},
	}
	for _, c := range cases {
		if got := ownershipModelForEvent(c.eventType); got != c.want {
			t.Errorf("ownershipModelForEvent(%q) = %d, want %d (%s)", c.eventType, got, c.want, c.why)
		}
	}
}

// TestEventSupportsWatch pins the exported UI-facing classifier the
// /api/event-types handler calls — the watch toggle shows only for these. It is
// stricter than "modelOwned": review_requested (precedence over the author-
// centric set) and the entity-terminating owned events (pr:merged / pr:closed,
// which never create a task) must all be false.
func TestEventSupportsWatch(t *testing.T) {
	cases := []struct {
		eventType string
		want      bool
		why       string
	}{
		{domain.EventGitHubPRCICheckFailed, true, "owner-ladder event → watch flag is meaningful"},
		{domain.EventJiraIssueAssigned, true, "assignee-centric jira → watch flag is meaningful"},
		{domain.EventGitHubPRReviewRequested, false, "requested-party routing, not owner-ladder"},
		{domain.EventJiraIssueAvailable, false, "pool routing — everyone matched is already a participant"},
		{domain.EventGitHubPRMerged, false, "entity-terminating: in the owned set for the ladder but never creates a task"},
		{domain.EventGitHubPRClosed, false, "entity-terminating: closes the entity before any task is created"},
	}
	for _, c := range cases {
		if got := EventSupportsWatch(c.eventType); got != c.want {
			t.Errorf("EventSupportsWatch(%q) = %v, want %v (%s)", c.eventType, got, c.want, c.why)
		}
	}
}
