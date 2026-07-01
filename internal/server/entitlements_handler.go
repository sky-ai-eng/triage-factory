package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// entitlementsResponse is the FE-facing shape of GET /api/entitlements: the
// subset of gated Enterprise features licensed for THIS deployment right now —
// e.g. ["governance"], or [] in a community / unlicensed build. Mirrored in the
// frontend by the useEntitlements hook, the reusable seam every EE FE surface
// gates on (render nothing until loaded && has("<feature>")).
type entitlementsResponse struct {
	// Features is always a non-nil slice so the JSON encodes as [] (never
	// null) for the unlicensed default — the frontend treats it as a set.
	Features []string `json:"features"`
}

// handleEntitlements reports which gated Enterprise features are licensed for
// the session's active org. It is a CORE route: it reports the entitlements
// provider's state and is deliberately NOT itself license-gated, so any build
// can learn what it has. It reports entitlements.For(orgID).Has uniformly —
// the check is mode-agnostic on purpose: a feature's EE-ness must not depend
// on local vs multi mode. In any unlicensed build (the Static/community
// default) it returns [].
//
// "Local is free" is a property of the FEATURE SET, never a runmode carve-out
// in the gate: the EE features are cross-team / org-wide lenses (governance) or
// inherently multi-tenant (SSO), and the within-your-boundary core surfaces a
// single-user / single-team install actually uses don't gate on entitlements at
// all. So an empty EE set in local is not a withheld feature — there's nothing
// an N=1 user is denied, and a license can't change that because those EE
// surfaces are multi-tenant concepts a local install doesn't present.
//
// The read is session-scoped: it resolves for the session's active org, so a
// future SaaS provider (Stripe) that grants features per-org needs no change
// to this handler's URL or shape.
//
// Gate: authenticated session only (mounted via s.api → withSession). Local
// mode's session shim seeds sentinel claims and stamps runmode.LocalDefaultOrgID,
// so the local user passes the gate and resolves an org normally.
func (s *Server) handleEntitlements(w http.ResponseWriter, r *http.Request) {
	// Defense-in-depth: the route is mounted withSession (s.api), which already
	// rejects an unauthenticated request, so in normal operation claims are
	// always seeded here. This guard keeps the handler safe to call directly and
	// makes the "never report feature state to an anonymous caller" contract
	// explicit at the handler boundary.
	if ClaimsFrom(r.Context()) == nil {
		writeUnauth(w)
		return
	}
	orgID := OrgIDFrom(r.Context())
	ent := entitlements.For(orgID)
	all := entitlements.AllFeatures()
	features := make([]string, 0, len(all))
	for _, f := range all {
		if ent.Has(f) {
			features = append(features, string(f))
		}
	}
	writeJSON(w, http.StatusOK, entitlementsResponse{Features: features})
}
