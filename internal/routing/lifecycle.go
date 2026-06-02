// Package routing handles event routing: task creation, auto-delegation,
// inline close checks, and entity lifecycle transitions. It replaces the
// old auto-delegate hook in internal/delegate/auto.go.
package routing

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EntityTerminatingEvents is the set of event types that trigger an entity
// lifecycle close (active → closed). When one of these fires, the entity
// transitions to closed and all its active tasks are cascade-closed with
// close_reason="entity_closed".
//
// The cascade (enumerate tasks → cancel their runs → close entity → batch-
// close tasks) is implemented as (*Router).closeEntity — the spawner
// dependency makes a free function awkward.
var EntityTerminatingEvents = map[string]bool{
	domain.EventGitHubPRMerged:     true,
	domain.EventGitHubPRClosed:     true,
	domain.EventJiraIssueCompleted: true,
}

// authorCentricGitHubEventTypes is every github:pr:* event type EXCEPT
// review_requested. These concern whoever OWNS the PR — its CI, conflict,
// review-feedback, and commit lifecycle — so they route to the entity's
// owning team via the owning-team ladder, not to whichever team's rule
// happened to match. review_requested is the lone exception: it is the
// requested reviewer's personal obligation and routes to their team(s),
// scoped at emit time. Enumerated (not a github: prefix test) so the set is
// auditable against internal/domain/events/github.go and so the prior-task
// ladder tier keys on exactly these types. merged/closed are included for
// completeness but never reach routing (they close the entity first) and
// never have tasks, so their presence in the prior-task lookup is a no-op.
var authorCentricGitHubEventTypes = []string{
	domain.EventGitHubPRCICheckFailed,
	domain.EventGitHubPRCICheckPassed,
	domain.EventGitHubPRReviewChangesRequested,
	domain.EventGitHubPRReviewApproved,
	domain.EventGitHubPRReviewCommented,
	domain.EventGitHubPRReviewDismissed,
	domain.EventGitHubPRReviewRequestRemoved,
	domain.EventGitHubPRLabelAdded,
	domain.EventGitHubPRLabelRemoved,
	domain.EventGitHubPROpened,
	domain.EventGitHubPRReadyForReview,
	domain.EventGitHubPRNewCommits,
	domain.EventGitHubPRConflicts,
	domain.EventGitHubPRMentioned,
	domain.EventGitHubPRMerged,
	domain.EventGitHubPRClosed,
}

var authorCentricGitHubEventSet = func() map[string]bool {
	m := make(map[string]bool, len(authorCentricGitHubEventTypes))
	for _, et := range authorCentricGitHubEventTypes {
		m[et] = true
	}
	return m
}()

// isAuthorCentricGitHubEvent reports whether an event routes via the
// owning-team ladder (true) rather than the requested-reviewer path
// (review_requested) or the default handler-team grouping (Jira, etc.).
func isAuthorCentricGitHubEvent(eventType string) bool {
	return authorCentricGitHubEventSet[eventType]
}
