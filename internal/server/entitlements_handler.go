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

// handleEntitlements reports which gated Enterprise features are available in
// this deployment. It is a CORE route: it reads the entitlements state and is
// deliberately NOT itself gated by any license, so any build can learn what it
// has. It reports through entitlements.Available, which encodes the open-core
// policy:
//
//   - Local mode is fully source-available and free → every feature is
//     reported, so the frontend renders EE surfaces and buying EE in local
//     changes nothing. (entitlements are a multi-mode concept.)
//   - Multi mode reports the licensed subset — a deployment-wide license today
//     (self-host EE), per-org Stripe state in future. The answer is already
//     scoped to the session, so when per-org lands this reports for the
//     session's active org with no change to the URL or response shape.
//
// Gate: authenticated session only (mounted via s.api → withSession). Local
// mode's session shim seeds sentinel claims, so the local user passes the gate.
func (s *Server) handleEntitlements(w http.ResponseWriter, r *http.Request) {
	if ClaimsFrom(r.Context()) == nil {
		writeUnauth(w)
		return
	}
	features := make([]string, 0, len(entitlements.AllFeatures))
	for _, f := range entitlements.AllFeatures {
		if entitlements.Available(f) {
			features = append(features, string(f))
		}
	}
	writeJSON(w, http.StatusOK, entitlementsResponse{Features: features})
}
