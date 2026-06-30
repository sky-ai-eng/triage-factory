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
