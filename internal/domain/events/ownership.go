package events

import (
	"sort"
	"strings"
)

// OwnershipModel classifies how an event type's routing participants + owner
// are resolved. It is per-type DATA, declared once at registration alongside
// the type's Metadata/Predicate — core types via their init()-time Register
// call (github.go, jira.go, system.go), an EE source's types the same way at
// install. internal/routing's resolveTeamRouting dispatches on it; routing
// does not decide it.
type OwnershipModel int

const (
	// OwnershipPool — handler-team grouping: every team with a matched handler is a
	// participant, the highest-priority team is the (eager) owner. For
	// unassigned/team-pool work like jira:issue:available, and the default for
	// any event type not otherwise classified (including one with no
	// registered schema at all — the permissive fallback a type would get if
	// it somehow reached resolveTeamRouting unclassified).
	OwnershipPool OwnershipModel = iota
	// OwnershipOwned — the owning-team ladder: the entity's owner (author-centric
	// github / assignee-centric jira). Non-owner teams reach it only via an
	// applies_to_unowned watch handler. The reach flag is meaningful ONLY here.
	OwnershipOwned
	// OwnershipRequestedParty — review_requested: routes to the requested reviewer's
	// team(s), scoped at emit time, with a handler-team fallback for legacy
	// events. Only github's native resolver supports this model — Register
	// panics if a non-github: event type declares it.
	OwnershipRequestedParty
	// OwnershipUnrouted — for a cataloged event type that never reaches
	// resolveTeamRouting at all (system:* sentinels: they carry no EntityID,
	// so routableEntity drops them before dispatch ever classifies them, and
	// RouterBound keeps them out of the durable queue in the first place).
	// Distinct from OwnershipPool, which is a real routing behavior
	// (handler-team grouping) — a system event doesn't participate in that,
	// it just doesn't route. dispatch's switch treats it the same as Pool if
	// it were ever reached (it structurally isn't), so this carries no
	// behavior, only an honest label.
	OwnershipUnrouted
)

// OwnershipModelFor returns eventType's declared ownership model, or
// OwnershipPool when the type has no registered schema — the historical
// default for an unclassified event type.
func OwnershipModelFor(eventType string) OwnershipModel {
	s, ok := Get(eventType)
	if !ok {
		return OwnershipPool
	}
	return s.Ownership
}

// TypesWithOwnership returns every registered event type whose source prefix
// matches sourcePrefix (e.g. "github:", "jira:") and whose declared
// OwnershipModel equals model, sorted for determinism. internal/routing
// derives its owning-team-ladder anchor sets (the tier-3 "most recent prior
// owned task" lookup) from this instead of a hand-maintained list — the set
// is exactly "every Owned type of this source," auditable against the
// Register calls in this package instead of a parallel enumeration.
func TypesWithOwnership(model OwnershipModel, sourcePrefix string) []string {
	mu.RLock()
	defer mu.RUnlock()
	var out []string
	for et, s := range registry {
		if s.Ownership == model && strings.HasPrefix(et, sourcePrefix) {
			out = append(out, et)
		}
	}
	sort.Strings(out)
	return out
}
