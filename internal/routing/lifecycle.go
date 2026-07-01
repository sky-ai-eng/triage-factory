// Package routing handles event routing: task creation, auto-delegation,
// inline close checks, and entity lifecycle transitions. It replaces the
// old auto-delegate hook in internal/delegate/auto.go.
package routing

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// authorCentricGitHubEventTypes is every github:pr:* event type that ROUTES via
// the owning-team ladder — the events concerning whoever OWNS the PR (its CI,
// conflict, review-feedback, commit, and termination lifecycle), routed to the
// entity's owning team rather than to whichever team's rule happened to match.
//
// merged / closed are INCLUDED (TFAC-520): a terminating event now closes the
// entity AND routes, so lifecycle automation (a post-merge blueprint, a
// close-time triage card) lands on the PR's owning team like any other
// author-centric event. The owner still resolves after the close phase has run
// — the prior-task ladder tier sees the just-closed tasks (no status filter)
// and the merged/closed metadata carries the author for the identity fallback.
//
// Only review_requested is EXCLUDED: the requested reviewer's personal
// obligation, routed to their team(s) and scoped at emit time
// (OwnershipRequestedParty), not the owning-team ladder.
//
// Enumerated (not a github: prefix test) so the set is auditable against
// internal/domain/events/github.go (= those events minus review_requested) and
// so the prior-task ladder tier keys on exactly these types.
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
// priority, comment, and completion activity. These route to the assignee's
// owning team via the owning-team ladder, not to whichever team's rule happened
// to match.
//
// jira:issue:completed is INCLUDED (TFAC-520): like the GitHub terminating
// events, it now closes the entity AND routes, so completion automation lands
// on the assignee's owning team. Only jira:issue:available is EXCLUDED — it is
// unassigned by definition (the team-pool / stock-discovery signal: "unclaimed
// work in a project this team tracks"); routing it through the assignee ladder
// would always drop (no assignee → no owner) and silently kill the discovery
// deck, so it stays in resolveTeamRouting's default branch, bounded by the
// team↔project tracking gate.
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
	domain.EventJiraIssueCompleted,
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

// OwnershipModel classifies how an event's routing participants + owner are
// resolved (TFAC-519). It is the single, explicit dispatch key for
// resolveTeamRouting — replacing the prior implicit "compute the handler-team
// default, then overwrite it in three special-case branches" shape. Every event
// type maps to exactly one model.
type OwnershipModel int

const (
	// OwnershipPool — handler-team grouping: every team with a matched handler is a
	// participant, the highest-priority team is the (eager) owner. For
	// unassigned/team-pool work like jira:issue:available, and the default for
	// any event not otherwise classified.
	OwnershipPool OwnershipModel = iota
	// OwnershipOwned — the owning-team ladder: the entity's owner (author-centric
	// github / assignee-centric jira). Non-owner teams reach it only via an
	// applies_to_unowned watch handler. The reach flag is meaningful ONLY here.
	OwnershipOwned
	// OwnershipRequestedParty — review_requested: routes to the requested reviewer's
	// team(s), scoped at emit time, with a handler-team fallback for legacy
	// events / unwired stores.
	OwnershipRequestedParty
)

// ownershipModelForEvent classifies an event type into its ownership model.
// This is the one place the owner-ladder / requested-party / pool distinction is
// made; an unclassified event type falls to OwnershipPool (the historical default).
// A registered event source (e.g. ee/slack) is consulted before the built-in
// switch — built-in github:/jira:/system: sources are never migrated into the
// registry (see source_registry.go), so this only ever fires for an
// out-of-core source. OwnershipRequestedParty is unsupported for registered
// sources (the requested-party resolver is GitHub-specific): logged as an
// error and downgraded to OwnershipPool. review_requested is checked first in
// the built-in switch because it is a github:pr:* type that is deliberately
// NOT in authorCentricGitHubEventSet.
func ownershipModelForEvent(eventType string) OwnershipModel {
	if hooks, ok := sourceHooksFor(eventType); ok {
		model := hooks.Ownership(eventType)
		if model == OwnershipRequestedParty {
			routerLog.Error("registered source returned OwnershipRequestedParty, unsupported; treating as OwnershipPool", "event_type", eventType)
			return OwnershipPool
		}
		return model
	}
	switch {
	case eventType == domain.EventGitHubPRReviewRequested:
		return OwnershipRequestedParty
	case isAuthorCentricGitHubEvent(eventType), isAssigneeCentricJiraEvent(eventType):
		return OwnershipOwned
	default:
		return OwnershipPool
	}
}

// EventSupportsWatch reports whether the applies_to_unowned ("watch") reach flag
// is meaningful for an event type (TFAC-517/519) — true ONLY for owner-ladder
// events (OwnershipOwned), where a non-owner team can opt into reaching the entity.
// Pool events already make every matched team a participant and requested-party
// routing is identity-scoped, so the flag is inert there. Entity-terminating
// events (pr:merged / pr:closed / issue:completed) are owner-ladder events now
// that they route (TFAC-520), so the flag IS meaningful for them — a non-owner
// team can watch "PR merged in a tracked repo → run my blueprint". The server's
// /api/event-types handler surfaces this so the editors show the toggle exactly
// where it does something. Exported because the classification
// (ownershipModelForEvent) is routing's to own.
func EventSupportsWatch(eventType string) bool {
	return ownershipModelForEvent(eventType) == OwnershipOwned
}
