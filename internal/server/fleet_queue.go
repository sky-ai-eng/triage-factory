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

// handleFleetQueue surfaces per-org run-queue shares (spec §8.3's `queue`
// fleet surface: per-org active/queued shares) — the read the fleet dashboard
// consumes and the operator uses to see one tenant's burst against the fleet's
// capacity.
//
// The FleetQueueShares store read is fleet-wide (every org's share); the
// spec's full operator view is gated on the operator identity that lands with
// a later ticket. Until then this mirrors the placement explainer's interim
// exactly: org-admin gated on ?org=, returning only the caller's own org's
// share — so no tenant sees another's queue depth before the operator gate
// exists. When operator identity lands, an operator caller (no ?org=) gets the
// full list; the store method already returns it.
//
// GET /api/fleet/queue?org=<uuid>
func (s *Server) handleFleetQueue(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return
	}
	if s.fleetQueue == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "fleet queue reader not ready"})
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
	// active or queued runs is absent from the fleet-wide result — report it as
	// an all-zero share (with its cap, if any) rather than 404, so a caller
	// polling a quiet org gets a stable shape.
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
