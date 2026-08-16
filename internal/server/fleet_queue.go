package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fleetQueueReader is the narrow view the fleet queue endpoint needs of the
// run queue store: the per-org active/queued shares (+ cap) FleetQueueShares
// computes fleet-wide. A field interface (not a direct store reach) so the
// handler test can inject canned shares.
type fleetQueueReader interface {
	FleetQueueShares(ctx context.Context) ([]db.OrgQueueShare, error)
}

// fleetQueueShareDTO is one org's line in the fleet queue view: how many of
// its runs are executing (active), how many are still waiting (queued), its
// configured concurrency cap (omitted when unlimited), and whether it is at
// that cap right now (its queued runs are invisible to claims until an active
// run finishes).
type fleetQueueShareDTO struct {
	OrgID             string `json:"org_id"`
	Active            int    `json:"active"`
	Queued            int    `json:"queued"`
	MaxConcurrentRuns *int   `json:"max_concurrent_runs,omitempty"`
	AtCap             bool   `json:"at_cap"`
}

// handleFleetQueue surfaces one org's run-queue share against its concurrency
// cap: active vs queued runs, the configured cap, and whether the org is at cap
// (its queued runs invisible to claims until an active one finishes). This is
// the org-facing read-out of the per-org cap + fair-claim feature — an org
// admin (or the org owner) checking their own tenant's standing.
//
// Org-scoped by design: the FleetQueueShares store read is fleet-wide, but this
// endpoint returns only the caller's own ?org= row, so no tenant sees another's
// queue depth. It is org-admin gated exactly like the placement explainer.
//
// The DEPLOYMENT-WIDE operator queue view is a SEPARATE surface, not this one:
// the fleet console's operator backlog (fleet-wide oldest-waiting + per-org
// shares) lives in ee/fleet at GET /api/fleet/backlog, gated on the operator
// identity AND the FeatureFleet entitlement (the console is EE; this per-org
// cap read-out is org-facing operability). The two are different lenses on the
// queue — cap-fairness here, wait-latency there — deliberately kept apart.
//
// GET /api/fleet/queue?org=<uuid>
func (s *Server) handleFleetQueue(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return
	}
	if s.fleetQueue == nil {
		// This pod runs no queue reader — deployment shape, not an outage.
		writeNotConfigured(w, "the fleet queue reader is not configured on this deployment")
		return
	}

	orgID := strings.TrimSpace(r.URL.Query().Get("org"))
	if orgID == "" {
		badRequest(w, "org query parameter is required")
		return
	}
	// Match the placement explainer's path-org validation: reject a non-uuid
	// org in multi mode before the admin check (local mode has one synthetic
	// org).
	if runmode.Current() != runmode.ModeLocal {
		if _, err := uuid.Parse(orgID); err != nil {
			notFound(w, "org")
			return
		}
	}
	if !s.az.RequireOrgAdminRole(w, r, orgID, claims.Subject) {
		return
	}

	shares, err := s.fleetQueue.FleetQueueShares(r.Context())
	if err != nil {
		internalError(w, "fleet-queue", err)
		return
	}

	// Interim org scoping: return only the requested org's row. An org with no
	// active or queued runs is absent from the activity-driven FleetQueueShares
	// result, so report it as an all-zero share rather than 404 — a stable shape
	// for a caller polling a quiet org. The configured cap rides the shares row,
	// which only exists for orgs with queue activity, so a quiet org's
	// max_concurrent_runs reads as unset here (at_cap is trivially false at zero
	// active regardless); the cap read-back for an idle org belongs on the
	// settings surface, not this queue view.
	dto := fleetQueueShareDTO{OrgID: orgID}
	for _, sh := range shares {
		if sh.OrgID != orgID {
			continue
		}
		dto.Active = sh.Active
		dto.Queued = sh.Queued
		dto.MaxConcurrentRuns = sh.MaxConcurrentRuns
		dto.AtCap = sh.MaxConcurrentRuns != nil && sh.Active >= *sh.MaxConcurrentRuns
		break
	}
	writeJSON(w, http.StatusOK, dto)
}
