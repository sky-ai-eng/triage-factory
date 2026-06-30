package routing

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestOwnershipModelForEvent pins the explicit event→model classification
// (TFAC-519) — the single dispatch key resolveTeamRouting branches on. The
// precedence guard matters: review_requested is a github:pr:* type that must
// resolve to RequestedParty, NOT Owned, even though the author-centric set is
// checked right after. The entity-terminating events (merged/closed/completed)
// are owner-ladder events now that they route (TFAC-520).
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
		{domain.EventGitHubPRMerged, modelOwned, "entity-terminating but routes (TFAC-520): owning-team ladder"},
		{domain.EventGitHubPRClosed, modelOwned, "entity-terminating but routes (TFAC-520): owning-team ladder"},
		{domain.EventJiraIssueCompleted, modelOwned, "entity-terminating but routes (TFAC-520): assignee owning-team ladder"},
		{"some:unregistered:event", modelPool, "unclassified types fall to the pool default"},
	}
	for _, c := range cases {
		if got := ownershipModelForEvent(c.eventType); got != c.want {
			t.Errorf("ownershipModelForEvent(%q) = %d, want %d (%s)", c.eventType, got, c.want, c.why)
		}
	}
}

// TestEventSupportsWatch pins the exported UI-facing classifier the
// /api/event-types handler calls — the watch toggle shows only for these. It
// equals "modelOwned": review_requested (requested-party) and the pool events
// are false, while the entity-terminating events (merged/closed/completed) are
// TRUE now that they route via the owning-team ladder (TFAC-520) — a non-owner
// team can watch "PR merged in a tracked repo → run my blueprint".
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
		{domain.EventGitHubPRMerged, true, "entity-terminating but routes (TFAC-520) via the owning-team ladder"},
		{domain.EventGitHubPRClosed, true, "entity-terminating but routes (TFAC-520) via the owning-team ladder"},
		{domain.EventJiraIssueCompleted, true, "entity-terminating but routes (TFAC-520) via the owning-team ladder"},
	}
	for _, c := range cases {
		if got := EventSupportsWatch(c.eventType); got != c.want {
			t.Errorf("EventSupportsWatch(%q) = %v, want %v (%s)", c.eventType, got, c.want, c.why)
		}
	}
}
