package entitlements

import (
	"slices"
	"strings"
)

// eventSourceGates maps an event source prefix (the segment before the first
// ':' in an event_type, e.g. "slack" in "slack:mention") to the Feature that
// must be held for an org to see or use events of that source. Guarded by the
// same mu as provider (entitlements.go) — unlike TFAC-523's routing source
// registry, this map is consulted from request handlers as well as init-time
// registration, so it needs the same lock discipline the provider already
// uses, not a registration-only one.
var eventSourceGates = map[string]Feature{}

// GateEventSource declares that every event type whose source prefix (the
// segment before the first ':') equals source requires feature f. Called from
// an ee package's install closure during startup; consulted per-request and
// per-event afterward via FeatureForEventType / EventTypeAllowed.
func GateEventSource(source string, f Feature) {
	mu.Lock()
	defer mu.Unlock()
	eventSourceGates[source] = f
}

// FeatureForEventType returns the feature gating eventType's source, ok=false
// when the source is ungated (github, jira, system — everything registered by
// core).
func FeatureForEventType(eventType string) (Feature, bool) {
	source := eventType
	if i := strings.IndexByte(eventType, ':'); i >= 0 {
		source = eventType[:i]
	}
	mu.RLock()
	defer mu.RUnlock()
	f, ok := eventSourceGates[source]
	return f, ok
}

// EventTypeAllowed reports whether orgID may see/use eventType: ungated types
// are always allowed; gated types require the org to hold the feature.
func EventTypeAllowed(orgID, eventType string) bool {
	f, gated := FeatureForEventType(eventType)
	if !gated {
		return true
	}
	return For(orgID).Has(f)
}

// RegisteredEventGateFeatures returns the distinct features referenced by
// event-source gates, sorted. It exists for the composition-root parity test
// (feature_parity_test.go at the repo root): a feature wired into a seam but
// missing from AllFeatures() gates correctly (Has is a plain map lookup) yet
// never surfaces on the /api/entitlements probe, leaving every render-gated
// surface dark for licensed orgs. Registration-time validation can't do this
// job — seam tests deliberately register synthetic features, and only the
// composition root sees exactly the real registrations.
func RegisteredEventGateFeatures() []Feature {
	mu.RLock()
	defer mu.RUnlock()
	seen := map[Feature]struct{}{}
	for _, f := range eventSourceGates {
		seen[f] = struct{}{}
	}
	out := make([]Feature, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	slices.Sort(out)
	return out
}
