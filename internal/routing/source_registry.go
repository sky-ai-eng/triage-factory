package routing

import (
	"context"
	"fmt"

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
// The two hooks have OPPOSITE store-error postures, and the split is the same
// one core's own routing draws between the owning-team ladder and
// teamTracksEventRepo:
//
//   - ResolveOwner DECIDES the owner, so a failed read PROPAGATES: return the
//     error and the router replays the event rather than routing it on a
//     guess. Degrading to "nothing owns this" is indistinguishable from the
//     real no-owner answer, and downstream that either loses the occurrence
//     for good (nothing re-emits it) or hands it to an applies_to_unowned
//     watcher, whose fire then consolidates ownership onto that watcher.
//   - TracksScope only NARROWS a set someone else computed, so it must keep
//     failing OPEN (return the permissive result + log), mirroring
//     teamTracksEventRepo: a briefly-too-wide gate the next event corrects
//     beats dropping legitimate work on a transient DB blip.
type SourceHooks struct {
	// ResolveOwner answers who owns an Owned event's occurrence — the analogue
	// of authorCentricOwner's return contract (see its doc). The OwnerResolution
	// return is what keeps the two empty-looking answers apart: a resolved
	// no-owner must SAY so, an error means the owner could not be resolved and
	// the event is replayed.
	// Required unconditionally, even for a source whose event types never
	// declare OwnershipOwned: registration can't see what event types this
	// source will add in the future, and dispatch calls this hook whenever a
	// type's declared model is Owned — a nil here would be a drain-goroutine
	// panic on the first Owned event instead of a boot failure. A pool-only
	// source supplies a stub returning (Unowned(), nil).
	ResolveOwner func(ctx context.Context, orgID string, evt domain.Event, entityID string) (OwnerResolution, error)
	// TracksScope reports whether teamID tracks the event's scope (the
	// stage-1 team↔resource gate). Fails open on a store error (see the
	// posture split above). Required.
	TracksScope func(ctx context.Context, evt domain.Event, teamID string) bool
}

// OwnerResolution is one ResolveOwner answer. It exists to make the RESOLVED
// no-owner answer a positive statement instead of the zero value, because the
// zero value is also what a swallowed store error produces — the two used to
// be the same `("", nil)`, and that identity was the whole bug this seam had.
// Stating it costs one field and buys a mechanism: resolveSourceOwner rejects
// an empty resolution that didn't claim Unowned, so a hook that returns early
// on a failed read fails loudly on its first occurrence rather than quietly
// handing the occurrence to a watcher for the rest of the source's life.
//
// The flag's polarity is deliberate. Forgetting it is only possible on the
// no-owner path, where the consequence is a replay and a named error; the
// owned path cannot be broken by forgetting anything. A `Resolved bool` that
// every answer had to set would invert that — a forgotten flag would discard a
// real owner.
type OwnerResolution struct {
	// Owner is the single team that owns this occurrence, "" when the source
	// resolved a set with no unambiguous owner (the ownerLadderRouting tail
	// treats that the same way the built-in ladders' ambiguous tier does).
	Owner string
	// OwnerSet is every team with an ownership claim on the occurrence —
	// usually just Owner, wider when the source resolves an ambiguous set.
	OwnerSet []string
	// Unowned asserts the resolved no-owner answer: the source looked, and
	// nothing owns this occurrence. The event is recorded taskless and
	// consumed; only an applies_to_unowned watcher can still reach it. Set it
	// ONLY for a data state a replay would reproduce — a scope no team has
	// claimed, metadata this source cannot read. Never for a failed read.
	Unowned bool
}

// OwnedBy is the resolution for an occurrence owned by exactly one team.
func OwnedBy(team string) OwnerResolution {
	return OwnerResolution{Owner: team, OwnerSet: []string{team}}
}

// Unowned is the resolved no-owner answer — see OwnerResolution.Unowned. It is
// also the pool-only source's ResolveOwner stub.
func Unowned() OwnerResolution {
	return OwnerResolution{Unowned: true}
}

// resolveSourceOwner calls a registered source's ResolveOwner and enforces the
// half of the contract the signature cannot. An empty resolution that did not
// claim Unowned is the shape of a swallowed store error — the `if err != nil {
// log; return }` every resolver is tempted to write — so it is refused here
// instead of being read as "nobody owns this". The event replays and parks with
// the source named on it: a bug someone can find, rather than a mention that
// silently never happened or a watcher that silently answered another team's
// channel.
func resolveSourceOwner(ctx context.Context, hooks SourceHooks, source, orgID string, evt domain.Event, entityID string) (owner string, ownerSet []string, err error) {
	res, err := hooks.ResolveOwner(ctx, orgID, evt, entityID)
	if err != nil {
		return "", nil, err
	}
	if res.Owner == "" && len(res.OwnerSet) == 0 && !res.Unowned {
		routerLog.ErrorContext(ctx, "registered source resolved an empty owner without asserting Unowned; treating it as unresolved",
			"source", source, "event_type", evt.EventType, "entity_id", entityID)
		return "", nil, fmt.Errorf("source %q returned an empty OwnerResolution without Unowned: a resolved no-owner must set it, a failed read must return an error", source)
	}
	return res.Owner, res.OwnerSet, nil
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
