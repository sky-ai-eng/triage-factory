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
// Ownership classification is NOT a hook here — a registered source declares
// each event type's OwnershipModel the same way core does, via
// internal/domain/events.Register at its own init()/install time. SourceHooks
// is purely resolution BEHAVIOR for the Owned model: which team owns a given
// occurrence (ResolveOwner) and whether a team tracks the event's scope
// (TracksScope).
//
// Hooks must fail OPEN on store errors (return the permissive result + log),
// mirroring teamTracksEventRepo: dropping legitimate work on a transient DB
// blip is worse than a briefly-wide gate.
type SourceHooks struct {
	// ResolveOwner resolves (owner, ownerSet) for an Owned event — the
	// analogue of authorCentricOwner's return contract (see its doc):
	// owner=="" + ownerSet==nil means nothing resolved.
	// Required unconditionally, even for a source whose event types never
	// declare OwnershipOwned: registration can't see what event types this
	// source will add in the future, and dispatch calls this hook whenever a
	// type's declared model is Owned — a nil here would be a drain-goroutine
	// panic on the first Owned event instead of a boot failure. A pool-only
	// source supplies a stub returning ("", nil).
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

// reservedSourcePrefixes are the prefixes RegisterSource refuses. github and
// jira route natively — their resolvers are methods on the Router using its
// stores, and hooks are consulted BEFORE the native paths at every wire
// point, so a registration here would silently shadow heavily-tested
// built-in behavior. system and webhook are bus-only by design (coalesced
// signals / raw deliveries) and must never become router-bound.
var reservedSourcePrefixes = map[string]bool{
	"github": true, "jira": true, "system": true, "webhook": true,
}

// routedPrefixes is the set of event-source prefixes the router consumes —
// the membership RouterBound reports and internal/ingest gates its durable
// outbox enqueue on. Seeded with the built-in github/jira sources (routed
// natively, no hooks); RegisterSource adds each registered source's prefix.
// Same startup-write / steady-state-read contract as sourceRegistry.
var routedPrefixes = builtinRoutedPrefixes()

func builtinRoutedPrefixes() map[string]bool {
	return map[string]bool{"github": true, "jira": true}
}

// RegisterSource registers hooks for an event-source prefix — the segment
// before the first ':' in an event type (e.g. "slack" for "slack:message").
// Registration also marks the prefix router-bound (see RouterBound). Panics
// on an empty, reserved, or already-registered source, or nil required
// hooks (wiring bug, fail at boot). The duplicate check mirrors
// events.Register: registrations are compile-time config, so a second
// registration for the same prefix (double-install, two sources colliding
// on a name) must fail loudly, never silently swap hooks.
func RegisterSource(source string, hooks SourceHooks) {
	if source == "" {
		panic("routing.RegisterSource: source must not be empty")
	}
	if reservedSourcePrefixes[source] {
		panic("routing.RegisterSource: " + source + " is a built-in source and cannot be re-registered")
	}
	if hooks.ResolveOwner == nil || hooks.TracksScope == nil {
		panic("routing.RegisterSource: ResolveOwner and TracksScope hooks are required")
	}
	if _, exists := sourceRegistry[source]; exists {
		panic("routing.RegisterSource: " + source + " is already registered")
	}
	sourceRegistry[source] = hooks
	routedPrefixes[source] = true
}

// RouterBound reports whether the router consumes eventType — true for the
// built-in github:/jira: sources and for every registered source. This is
// the durability boundary: internal/ingest enqueues router-bound events
// into the durable outbox and leaves everything else (system:*, webhook:*)
// bus-only.
func RouterBound(eventType string) bool {
	return routedPrefixes[eventSourcePrefix(eventType)]
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

// ResetSources clears the registry and restores the built-in routed set
// (tests only).
func ResetSources() {
	sourceRegistry = map[string]SourceHooks{}
	routedPrefixes = builtinRoutedPrefixes()
}
