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

// assigneeCentricJiraEventTypes is every jira:issue:* event that concerns
// whoever IS ASSIGNED the issue — assignment, atomic-discovery, status,
// priority, and comment activity. These route to the assignee's owning team
// via the owning-team ladder, not to whichever team's rule happened to match.
// Two jira:issue:* types are deliberately EXCLUDED and stay on handler-team
// routing:
//
//   - jira:issue:available — unassigned by definition (the team-pool /
//     stock-discovery signal: "unclaimed work in a project this team
//     tracks"). Routing it through the assignee ladder would always drop
//     (no assignee → no owner) and silently kill the discovery deck, so it
//     stays in resolveTeamRouting's default branch, bounded by the
//     team↔project tracking gate.
//   - jira:issue:completed — entity-terminating (EntityTerminatingEvents);
//     it closes the entity before task creation, so it never creates a task
//     and never reaches the ladder.
//
// Enumerated (not a jira: prefix test) so the set is auditable against
// internal/domain/event.go and so the prior-task ladder tier keys on exactly
// these types.
var assigneeCentricJiraEventTypes = []string{
	domain.EventJiraIssueAssigned,
	domain.EventJiraIssueBecameAtomic,
	domain.EventJiraIssueStatusChanged,
	domain.EventJiraIssuePriorityChanged,
	domain.EventJiraIssueCommented,
}

var assigneeCentricJiraEventSet = func() map[string]bool {
	m := make(map[string]bool, len(assigneeCentricJiraEventTypes))
	for _, et := range assigneeCentricJiraEventTypes {
		m[et] = true
	}
	return m
}()

// isAssigneeCentricJiraEvent reports whether an event routes via the
// owning-team ladder keyed on the issue's assignee (true) rather than the
// default handler-team grouping (jira:issue:available, etc.).
func isAssigneeCentricJiraEvent(eventType string) bool {
	return assigneeCentricJiraEventSet[eventType]
}

// ownershipModel classifies how an event's routing participants + owner are
// resolved (TFAC-519). It is the single, explicit dispatch key for
// resolveTeamRouting — replacing the prior implicit "compute the handler-team
// default, then overwrite it in three special-case branches" shape. Every event
// type maps to exactly one model.
type ownershipModel int

const (
	// modelPool — handler-team grouping: every team with a matched handler is a
	// participant, the highest-priority team is the (eager) owner. For
	// unassigned/team-pool work like jira:issue:available, and the default for
	// any event not otherwise classified.
	modelPool ownershipModel = iota
	// modelOwned — the owning-team ladder: the entity's owner (author-centric
	// github / assignee-centric jira). Non-owner teams reach it only via an
	// applies_to_unowned watch handler. The reach flag is meaningful ONLY here.
	modelOwned
	// modelRequestedParty — review_requested: routes to the requested reviewer's
	// team(s), scoped at emit time, with a handler-team fallback for legacy
	// events / unwired stores.
	modelRequestedParty
)

// ownershipModelForEvent classifies an event type into its ownership model.
// This is the one place the owner-ladder / requested-party / pool distinction is
// made; an unclassified event type falls to modelPool (the historical default).
// review_requested is checked first because it is a github:pr:* type that is
// deliberately NOT in authorCentricGitHubEventSet.
func ownershipModelForEvent(eventType string) ownershipModel {
	switch {
	case eventType == domain.EventGitHubPRReviewRequested:
		return modelRequestedParty
	case isAuthorCentricGitHubEvent(eventType), isAssigneeCentricJiraEvent(eventType):
		return modelOwned
	default:
		return modelPool
	}
}
