package routing

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SourceHooks is the routing behavior a registered event source (an ee/
// package, e.g. Slack) plugs into core's routing chokepoints. Core never
// imports ee/ (lint-enforced boundary), so this registry is the inversion
// seam: an ee package registers hooks once at install time, and dispatch-time
// code (lifecycle.go, dispatch.go, team_routing.go) reads them back without
// knowing the concrete source.
//
// Hooks must fail OPEN on store errors (return the permissive result + log),
// mirroring teamTracksEventRepo (team_routing.go:77): dropping legitimate
// work on a transient DB blip is worse than a briefly-wide gate.
type SourceHooks struct {
	// Ownership classifies an event type from this source. Required.
	// Returning OwnershipRequestedParty is unsupported for registered sources
	// (the requested-party resolver is GitHub-specific): dispatch logs an
	// error and treats it as OwnershipPool.
	Ownership func(eventType string) OwnershipModel
	// ResolveOwner resolves (owner, ownerSet) for an Owned event — the
	// analogue of authorCentricOwner's return contract (see its doc,
	// team_routing.go:236): owner=="" + ownerSet==nil means nothing resolved.
	// Required if Ownership ever returns OwnershipOwned.
	ResolveOwner func(ctx context.Context, orgID string, evt domain.Event, entityID string) (owner string, ownerSet []string)
	// TracksScope reports whether teamID tracks the event's scope (the
	// stage-1 team↔resource gate). Required.
	TracksScope func(ctx context.Context, evt domain.Event, teamID string) bool
}

// sourceRegistry is the process-global map of registered event-source
// prefixes to their routing hooks. Written once during single-threaded
// startup (an ee package's install closure runs inside routes(), before
// pollers/bus delivery start — goroutine creation gives the happens-before),
// read thereafter from the eventbus goroutine. Same no-mutex contract as
// registeredExtensions (internal/server/extension.go:112) — see there for the
// rationale.
var sourceRegistry = map[string]SourceHooks{}

// RegisterSource registers hooks for an event-source prefix — the segment
// before the first ':' in an event type (e.g. "slack" for "slack:mention").
// Panics on empty source or nil required hooks (wiring bug, fail at boot).
func RegisterSource(source string, hooks SourceHooks) {
	if source == "" {
		panic("routing.RegisterSource: source must not be empty")
	}
	if hooks.Ownership == nil || hooks.TracksScope == nil {
		panic("routing.RegisterSource: Ownership and TracksScope hooks are required")
	}
	sourceRegistry[source] = hooks
}

// SourceRegistered reports whether eventType's source prefix has registered
// hooks. Exported for internal/ingest's routerBound (Part 2).
func SourceRegistered(eventType string) bool {
	_, ok := sourceRegistry[eventSourcePrefix(eventType)]
	return ok
}

// sourceHooksFor returns the registered hooks for eventType's source prefix,
// ok=false if unregistered.
func sourceHooksFor(eventType string) (SourceHooks, bool) {
	hooks, ok := sourceRegistry[eventSourcePrefix(eventType)]
	return hooks, ok
}

// eventSourcePrefix returns the segment of eventType before the first ':'.
func eventSourcePrefix(eventType string) string {
	for i := 0; i < len(eventType); i++ {
		if eventType[i] == ':' {
			return eventType[:i]
		}
	}
	return eventType
}

// ResetSources clears the registry (tests only).
func ResetSources() {
	sourceRegistry = map[string]SourceHooks{}
}
