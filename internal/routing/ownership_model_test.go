package routing

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// TestOwnershipModelForEvent pins the explicit event→model classification —
// the single dispatch key resolveTeamRouting branches on. The precedence
// guard matters: review_requested is a github:pr:* type that must resolve to
// RequestedParty, NOT Owned, even though most other github:pr:* types are
// Owned. The entity-terminating events (merged/closed/completed) are
// owner-ladder events too, since they route rather than short-circuiting.
func TestOwnershipModelForEvent(t *testing.T) {
	cases := []struct {
		eventType string
		want      events.OwnershipModel
		why       string
	}{
		{domain.EventGitHubPRReviewRequested, events.OwnershipRequestedParty, "review_requested routes to the requested reviewer"},
		{domain.EventGitHubPRCICheckFailed, events.OwnershipOwned, "author-centric github → owning-team ladder"},
		{domain.EventGitHubPRConflicts, events.OwnershipOwned, "author-centric github → owning-team ladder"},
		{domain.EventJiraIssueAssigned, events.OwnershipOwned, "assignee-centric jira → owning-team ladder"},
		{domain.EventJiraIssueCommented, events.OwnershipOwned, "assignee-centric jira → owning-team ladder"},
		{domain.EventJiraIssueAvailable, events.OwnershipPool, "unassigned team-pool signal → handler-team grouping"},
		{domain.EventGitHubPRMerged, events.OwnershipOwned, "entity-terminating but routes: owning-team ladder"},
		{domain.EventGitHubPRClosed, events.OwnershipOwned, "entity-terminating but routes: owning-team ladder"},
		{domain.EventJiraIssueCompleted, events.OwnershipOwned, "entity-terminating but routes: assignee owning-team ladder"},
		{"some:unregistered:event", events.OwnershipPool, "unclassified types fall to the pool default"},
	}
	for _, c := range cases {
		if got := ownershipModelForEvent(c.eventType); got != c.want {
			t.Errorf("ownershipModelForEvent(%q) = %d, want %d (%s)", c.eventType, got, c.want, c.why)
		}
	}
}

// TestEventSupportsWatch pins the exported UI-facing classifier the
// /api/event-types handler calls — the watch toggle shows only for these. It
// equals "OwnershipOwned": review_requested (requested-party) and the pool events
// are false, while the entity-terminating events (merged/closed/completed) are
// TRUE since they route via the owning-team ladder — a non-owner team can
// watch "PR merged in a tracked repo → run my blueprint".
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
		{domain.EventGitHubPRMerged, true, "entity-terminating but routes via the owning-team ladder"},
		{domain.EventGitHubPRClosed, true, "entity-terminating but routes via the owning-team ladder"},
		{domain.EventJiraIssueCompleted, true, "entity-terminating but routes via the owning-team ladder"},
	}
	for _, c := range cases {
		if got := EventSupportsWatch(c.eventType); got != c.want {
			t.Errorf("EventSupportsWatch(%q) = %v, want %v (%s)", c.eventType, got, c.want, c.why)
		}
	}
}
