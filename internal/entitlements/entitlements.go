// Package entitlements is the seam between the open-source core and the
// commercially-licensed Enterprise Edition (the ee/ subtree).
//
// The whole point of the seam is a one-way dependency rule: CORE CODE
// MAY IMPORT THIS PACKAGE, BUT NEVER IMPORTS ee/. Core asks "is this
// feature licensed?" by calling entitlements.Active().Has(FeatureX). The
// ee/ subtree (and only the ee/ subtree, wired in from package main —
// the one layer allowed to import ee/) registers the real, license-key-
// backed checker at startup via Register. If nothing registers — an
// unlicensed / pure-community build, or a build with no valid TF_LICENSE
// — Active() returns the community checker, which answers false for every
// enterprise feature. Enterprise code paths stay dark unless a license
// turns them on.
//
// This mirrors internal/runmode's design deliberately: a process-global
// initialized once near main(), read through a tiny RLock'd accessor.
// Keeping it global (rather than threading a checker through every
// handler constructor) keeps call sites honest — `entitlements.Active().
// Has(entitlements.FeatureSSO)` reads correctly anywhere.
//
// Source-availability is unaffected: every line of ee/ ships in this
// repo and is readable. The license gates the right to RUN enterprise
// features in production, not the right to read the code — exactly the
// boundary a security reviewer needs (the isolation substrate stays in
// core, free and auditable) and a paying enterprise expects (SSO, SCIM,
// sandbox-fleet administration, audit export are the paid tier).
package entitlements

import (
	"slices"
	"sync"
)

// Feature names a gated enterprise capability. The string values are the
// stable wire identifiers that appear in a signed license token's
// features list, so they must not change once issued in the wild.
type Feature string

const (
	// FeatureSSO gates SAML/OIDC single sign-on — the ee/sso route + login
	// extensions: GoTrue connection management, domain verification, and
	// enforcement. Multi-mode only.
	FeatureSSO Feature = "sso"

	// FeatureGovernance gates the Enterprise governance/audit surfaces:
	// per-team daily spend caps, the bot-activity audit feed, and the
	// access/credential change-log viewer. It is the first EE feature with
	// a real frontend surface — SSO only needed backend route-mounting plus a
	// 404-and-hide — which is why this feature also motivates the
	// /api/entitlements probe and the useEntitlements FE hook the governance
	// surfaces gate on.
	FeatureGovernance Feature = "governance"
)

// allFeatures is the registry of every gated feature. Unexported + returned by
// value through AllFeatures so a caller can't append to or blank out the
// registry and corrupt the probe.
var allFeatures = []Feature{FeatureSSO, FeatureGovernance}

// AllFeatures returns every gated feature, for the /api/entitlements probe to
// iterate and report the subset the active checker licenses. It returns a fresh
// copy each call, so a new gated Feature must be added to allFeatures to show up
// on the probe (and thus to the frontend's useEntitlements hook).
func AllFeatures() []Feature { return slices.Clone(allFeatures) }

// Checker answers whether a given enterprise feature is licensed for use
// in this process right now. Implementations must be safe for concurrent
// use and must fail closed (return false) on any doubt — an expired or
// malformed license is "not licensed", never "licensed".
type Checker interface {
	Has(Feature) bool
}

// community is the default checker for an unlicensed build: every
// enterprise feature is off. It carries no state.
type community struct{}

func (community) Has(Feature) bool { return false }

// Community returns the default, no-enterprise-features checker. Exposed
// so tests (and ee/ wiring that fails to load a license) can restore the
// baseline explicitly.
func Community() Checker { return community{} }

// Limit names a quota key (per-seat/tier billing, later). The type is
// defined here so provider implementations have a stable key type to code
// against; no constants exist yet — they land with billing.
type Limit string

// Entitlements is an immutable snapshot of what one org is entitled to. It
// GROWS BY FIELDS — a quota added later touches nothing structural. The
// zero value denies every feature and reports every limit as unset.
type Entitlements struct {
	features map[Feature]struct{}
	limits   map[Limit]int
}

// Has reports whether the snapshot grants f.
func (e Entitlements) Has(f Feature) bool { _, ok := e.features[f]; return ok }

// Limit returns the quota value for l, or (0, false) if l is absent — absent
// means unlimited/unset, not zero.
func (e Entitlements) Limit(l Limit) (int, bool) { v, ok := e.limits[l]; return v, ok }

// New builds an Entitlements snapshot from a feature list and a limits map.
// Both are copied into fresh maps so the returned value is a true immutable
// snapshot — a caller that mutates (or keeps mutating) the slice/map it
// passed in, or reuses one map across multiple For(orgID) results, can never
// reach back into a live Entitlements. That copy is also what makes
// concurrent Has/Limit reads across goroutines safe: nothing outside this
// package ever holds a reference to the backing maps. Exported so ee/
// providers can construct one — the struct's fields stay unexported so only
// this package can shape the invariants.
func New(features []Feature, limits map[Limit]int) Entitlements {
	fs := make(map[Feature]struct{}, len(features))
	for _, f := range features {
		fs[f] = struct{}{}
	}
	ls := make(map[Limit]int, len(limits))
	for k, v := range limits {
		ls[k] = v
	}
	return Entitlements{features: fs, limits: ls}
}

// Provider resolves the Entitlements snapshot for one org. Implementations
// GROW BY IMPLEMENTATIONS (Static, the ee license provider, later an ee
// Stripe provider) — per-org only, no deployment-scope accessor.
type Provider interface {
	For(orgID string) Entitlements
}

// staticProvider yields the same Entitlements for every org.
type staticProvider struct{ ent Entitlements }

func (s staticProvider) For(string) Entitlements { return s.ent }

// Static returns a Provider that grants features (no limits) to every org
// alike. Static() with no arguments is the community default: nothing
// entitled, for any org.
func Static(features ...Feature) Provider { return staticProvider{ent: New(features, nil)} }

var (
	mu       sync.RWMutex
	active   Checker  = community{} // LEGACY: only the SSO mount-gate reads this; removed by the SSO ticket
	provider Provider = Static()    // per-org default: everything off
)

// Active returns the registered checker, or the community checker if none
// has been registered. Safe to call from any goroutine, including during
// boot before ee.Install runs (callers get the permissive-to-no-one
// community default).
func Active() Checker {
	mu.RLock()
	defer mu.RUnlock()
	return active
}

// Register installs the process-wide checker. Called once, from package
// main via ee.Install, after a license token has been verified. A nil
// checker is ignored so a wiring bug can never blank out Active() into a
// nil-deref; the worst case is the community default stays in place.
func Register(c Checker) {
	if c == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	active = c
}

// RegisterProvider installs the process-wide per-org Provider. Called once,
// from package main via ee.Install, after a license token has been verified.
// A nil provider is ignored, mirroring Register's nil-guard.
func RegisterProvider(p Provider) {
	if p == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	provider = p
}

// For returns the entitlements snapshot for orgID from the registered
// Provider, or the Static (everything off) default if none has been
// registered. Safe to call from any goroutine, including during boot before
// ee.Install runs.
func For(orgID string) Entitlements {
	mu.RLock()
	p := provider
	mu.RUnlock()
	return p.For(orgID)
}

// Reset restores the community default. Intended for tests that register
// a stub checker and need to avoid leaking it into sibling tests.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	active = community{}
	provider = Static()
}
