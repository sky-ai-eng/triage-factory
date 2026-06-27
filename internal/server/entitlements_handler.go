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
// this deployment. It is a CORE route: it reads the entitlements checker's
// state and is deliberately NOT itself gated by any license, so a community
// build can learn it has nothing licensed (features: []) and an EE build learns
// what it does. Entitlements are process-global today — one TF_LICENSE verified
// at boot in ee.Install → entitlements.Register — so the answer is
// deployment-level: no org or role scoping, just an authenticated session.
//
// Gate: authenticated session only (mounted via s.api → withSession). Local
// mode's session shim seeds sentinel claims, so the local user gets the same
// reply — everything false unless a TF_LICENSE happens to be present.
func (s *Server) handleEntitlements(w http.ResponseWriter, r *http.Request) {
	if ClaimsFrom(r.Context()) == nil {
		writeUnauth(w)
		return
	}
	checker := entitlements.Active()
	features := make([]string, 0, len(entitlements.AllFeatures))
	for _, f := range entitlements.AllFeatures {
		if checker.Has(f) {
			features = append(features, string(f))
		}
	}
	writeJSON(w, http.StatusOK, entitlementsResponse{Features: features})
}
