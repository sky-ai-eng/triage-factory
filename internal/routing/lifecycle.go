// Package routing handles event routing: task creation, auto-delegation,
// inline close checks, and entity lifecycle transitions. It replaces the
// old auto-delegate hook in internal/delegate/auto.go.
package routing

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// authorCentricGitHubEventTypes is every github:pr:* event type that ROUTES via
// the owning-team ladder — the events concerning whoever OWNS the PR (its CI,
// conflict, review-feedback, commit, and termination lifecycle), routed to the
// entity's owning team rather than to whichever team's rule happened to match.
//
// merged / closed are INCLUDED: a terminating event now closes the entity AND
// routes, so lifecycle automation (a post-merge blueprint, a close-time
// triage card) lands on the PR's owning team like any other author-centric
// event. The owner still resolves after the close phase has run — the
// prior-task ladder tier sees the just-closed tasks (no status filter) and
// the merged/closed metadata carries the author for the identity fallback.
//
// Only review_requested is EXCLUDED: the requested reviewer's personal
// obligation, routed to their team(s) and scoped at emit time
// (OwnershipRequestedParty), not the owning-team ladder.
//
// Derived from the events registry's per-type OwnershipModel declarations
// instead of hand-enumerated — the set is exactly "every github:pr:* type
// registered OwnershipOwned" (see internal/domain/events/github.go),
// auditable there rather than against a parallel list here. The prefix is
// "github:pr:", not just "github:", so a future non-PR github: event type
// (registered Owned) doesn't silently join this PR-specific anchor set.
// Sorted for determinism; the prior-task ladder tier and the terminal-close
// set (close_relations.go) key on membership, not order.
var authorCentricGitHubEventTypes = events.TypesWithOwnership(events.OwnershipOwned, "github:pr:")

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
// jira:issue:completed is INCLUDED: like the GitHub terminating events, it
// now closes the entity AND routes, so completion automation lands on the
// assignee's owning team. Only jira:issue:available is EXCLUDED — it is
// unassigned by definition (the team-pool / stock-discovery signal: "unclaimed
// work in a project this team tracks"); routing it through the assignee ladder
// would always drop (no assignee → no owner) and silently kill the discovery
// deck, so it stays in resolveTeamRouting's default branch, bounded by the
// team↔project tracking gate.
//
// Derived the same way as authorCentricGitHubEventTypes: every jira:issue:*
// type registered OwnershipOwned (see internal/domain/events/jira.go) —
// issue:available is declared OwnershipPool there, so it falls out of this
// set for free instead of needing an explicit exclusion. The prefix is
// "jira:issue:", not just "jira:", so a future non-issue jira: event type
// (registered Owned) doesn't silently join this issue-specific anchor set.
var assigneeCentricJiraEventTypes = events.TypesWithOwnership(events.OwnershipOwned, "jira:issue:")

var assigneeCentricJiraEventSet = func() map[string]bool {
	m := make(map[string]bool, len(assigneeCentricJiraEventTypes))
	for _, et := range assigneeCentricJiraEventTypes {
		m[et] = true
	}
	return m
}()

// ownershipModelForEvent classifies an event type into its ownership model —
// a lookup, not a decision. Every event type declares its model at
// registration (internal/domain/events.EventSchema.Ownership): core types at
// their init()-time Register call, a registered EE source's types the same
// way at install — one registration path for everyone, rather than a
// per-source hook consulted ahead of a built-in special case. An event type
// with no registered schema at all falls to OwnershipPool, the historical
// default.
func ownershipModelForEvent(eventType string) events.OwnershipModel {
	return events.OwnershipModelFor(eventType)
}

// EventSupportsWatch reports whether the applies_to_unowned ("watch") reach flag
// is meaningful for an event type — true ONLY for owner-ladder events
// (OwnershipOwned), where a non-owner team can opt into reaching the entity.
// Pool events already make every matched team a participant and requested-party
// routing is identity-scoped, so the flag is inert there. Entity-terminating
// events (pr:merged / pr:closed / issue:completed) are owner-ladder events now
// that they route, so the flag IS meaningful for them — a non-owner team can
// watch "PR merged in a tracked repo → run my blueprint". The server's
// /api/event-types handler surfaces this so the editors show the toggle exactly
// where it does something. The classification itself is per-type data owned by
// internal/domain/events; this just derives the UI-facing bool from it.
func EventSupportsWatch(eventType string) bool {
	return ownershipModelForEvent(eventType) == events.OwnershipOwned
}
