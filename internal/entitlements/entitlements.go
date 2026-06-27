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
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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

	// FeatureGovernance gates the Enterprise governance/audit surfaces
	// (TFAC-449): per-team daily spend caps, the bot-activity audit feed, and
	// the access/credential change-log viewer. It is the first EE feature with
	// a real frontend surface — SSO only needed backend route-mounting plus a
	// 404-and-hide — which is why this feature also motivates the
	// /api/entitlements probe and the useEntitlements FE hook the governance
	// surfaces gate on.
	FeatureGovernance Feature = "governance"
)

// AllFeatures lists every gated feature, for the /api/entitlements probe. The
// handler iterates it and reports the subset the active checker licenses, so a
// new gated Feature must be appended here to show up on the probe (and thus to
// the frontend's useEntitlements hook).
var AllFeatures = []Feature{FeatureSSO, FeatureGovernance}

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

var (
	mu     sync.RWMutex
	active Checker = community{}
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

// Available reports whether feature may be used in this deployment right now.
// It is the single gate every EE code path — backend handlers and the
// /api/entitlements probe — should consult, rather than calling Active().Has
// directly, because it encodes the open-core licensing policy in one place:
//
//   - Local mode is fully source-available and free. Every feature is on and no
//     checker is consulted, so purchasing EE while in local changes nothing.
//     Gating a feature behind a license in a build a developer runs on their own
//     machine is both wrong (local is free) and futile, so this never restricts
//     local — entitlements are a multi-mode concept.
//   - Multi mode consults the registered checker, which fails closed: a
//     deployment-wide license today (self-host EE), or per-org Stripe state in
//     future (the SaaS tenant model). When that per-org checker lands, this gate
//     gains an orgID; the deployment-license checker simply ignores it.
func Available(f Feature) bool {
	if runmode.Current() == runmode.ModeLocal {
		return true
	}
	return Active().Has(f)
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

// Reset restores the community default. Intended for tests that register
// a stub checker and need to avoid leaking it into sibling tests.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	active = community{}
}
