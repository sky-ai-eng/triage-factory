package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// entitlementsResponse is the FE-facing shape of GET
// /api/orgs/{org_id}/entitlements: the subset of gated Enterprise features
// licensed for that org right now — e.g. ["governance"], or [] in a community
// / unlicensed build. Mirrored in the frontend by the useEntitlements hook, the
// reusable seam every EE FE surface gates on (render nothing until loaded &&
// has("<feature>")).
type entitlementsResponse struct {
	// Features is always a non-nil slice so the JSON encodes as [] (never
	// null) for the unlicensed default — the frontend treats it as a set.
	Features []string `json:"features" doc:"The Enterprise features licensed for the organization, such as governance; empty when none is."`
}

var opEntitlements = httpx.NewJSONOp[httpx.NoBody, entitlementsResponse](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/entitlements",
	ID:          "list_entitlements",
	Summary:     "List an organization's licensed Enterprise features",
	Description: "Returns the Enterprise features licensed for the organization, as one unpaged list. A build with no licence answers an empty list.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleEntitlements reports which gated Enterprise features are licensed for
// the org the path names. It is a CORE route: it reports the entitlements
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
// The answer is the org's, which is why the org is in the path: a provider
// that grants features per org answers each org differently, and a caller in
// several orgs reads each one's without moving a session cursor.
//
// Gate: any member of the org. Local mode's sentinel org passes it.
func (s *Server) handleEntitlements(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	ent := entitlements.For(orgID)
	all := entitlements.AllFeatures()
	features := make([]string, 0, len(all))
	for _, f := range all {
		if ent.Has(f) {
			features = append(features, string(f))
		}
	}
	opEntitlements.Write(w, entitlementsResponse{Features: features})
}
