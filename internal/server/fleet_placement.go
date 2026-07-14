package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/placement"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// placementResolver is the narrow view the explainer needs of
// *placement.Resolver: compute the full candidate order for a key,
// even when the layer is advisory-off, so an operator can preview placement.
type placementResolver interface {
	Explain(ctx context.Context, orgID, keyKind, keyValue string) (placement.Plan, error)
}

// SetPlacementResolver wires the rendezvous resolver that backs GET
// /api/fleet/placement. Called once at startup on serving roles;
// nil until then, so the endpoint 503s rather than panicking on a request
// that races boot.
func (s *Server) SetPlacementResolver(r placementResolver) { s.placement = r }

// placementCandidateDTO is one instance's line in the explainer output: its
// rendezvous score, capacity weight, whether it is eligible/preferred, and a
// short reason for any special standing (dead/draining/gated/pinned).
type placementCandidateDTO struct {
	InstanceID string  `json:"instance_id"`
	Weight     int     `json:"weight"`
	Score      float64 `json:"score"`
	Eligible   bool    `json:"eligible"`
	Preferred  bool    `json:"preferred"`
	Reason     string  `json:"reason,omitempty"`
}

type placementOverrideDTO struct {
	PinnedInstanceID string `json:"pinned_instance_id,omitempty"`
	Replicas         int    `json:"replicas,omitempty"`
}

// placementExplainDTO is the GET /api/fleet/placement response: the computed
// candidate order for the key, the preferred set the enqueue stamp draws
// from, whether the layer is live or advisory-off, the aging/liveness windows
// the claim runs with, and any override in effect. This is the debuggability
// that made a mutable routing table tempting — delivered as a pure view over
// deterministic state (spec §6.1).
type placementExplainDTO struct {
	Enabled      bool                    `json:"enabled"`
	OrgID        string                  `json:"org_id"`
	KeyKind      string                  `json:"key_kind"`
	KeyValue     string                  `json:"key_value"`
	PreferredSet []string                `json:"preferred_set"`
	Candidates   []placementCandidateDTO `json:"candidates"`
	Override     *placementOverrideDTO   `json:"override,omitempty"`
	AgingSeconds float64                 `json:"aging_seconds"`
	LivenessSec  float64                 `json:"liveness_seconds"`
}

// handleFleetPlacement is the placement explainer (spec §6.1): for a key it
// returns the computed rendezvous candidate order and why (weights, overrides,
// liveness) — the same order the claim honors. Org-admin gated on the ?org=
// query (a dedicated fleet operator identity is a later ticket);
// the answer names executor instance ids for the caller's own org's work, so
// org-admin is the right scope until then.
//
// GET /api/fleet/placement?org=<uuid>&repo=<owner/repo>[&kind=repo|project]
func (s *Server) handleFleetPlacement(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return
	}
	if s.placement == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "placement resolver not ready"})
		return
	}

	orgID := strings.TrimSpace(r.URL.Query().Get("org"))
	if orgID == "" {
		badRequest(w, "org query parameter is required")
		return
	}
	// Match RequireOrgAdmin's path-org validation: reject a non-uuid org in
	// multi mode before the admin check (local mode has one synthetic org).
	if runmode.Current() != runmode.ModeLocal {
		if _, err := uuid.Parse(orgID); err != nil {
			notFound(w, "org")
			return
		}
	}
	if !s.az.RequireOrgAdminRole(w, r, orgID, claims.Subject) {
		return
	}

	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind == "" {
		kind = domain.PlacementKindRepo
	}
	if kind != domain.PlacementKindRepo && kind != domain.PlacementKindProject {
		badRequest(w, "kind must be 'repo' or 'project'")
		return
	}
	// Read the key value from ?repo= for a repo key or ?project= for a project
	// key — either query-parameter NAME is accepted so the URL reads naturally
	// for each kind (repo=owner/repo, or project=<id>). The value itself is the
	// owner/repo string or the project id.
	keyValue := strings.TrimSpace(r.URL.Query().Get("repo"))
	if keyValue == "" {
		keyValue = strings.TrimSpace(r.URL.Query().Get("project"))
	}
	if keyValue == "" {
		badRequest(w, "repo (or project) query parameter is required")
		return
	}

	plan, err := s.placement.Explain(r.Context(), orgID, kind, keyValue)
	if err != nil {
		internalError(w, "fleet-placement", err)
		return
	}

	dto := placementExplainDTO{
		Enabled:      plan.Enabled,
		OrgID:        plan.OrgID,
		KeyKind:      plan.KeyKind,
		KeyValue:     plan.KeyValue,
		PreferredSet: plan.PreferredSet,
		AgingSeconds: plan.Aging.Seconds(),
		LivenessSec:  plan.Liveness.Seconds(),
	}
	if dto.PreferredSet == nil {
		dto.PreferredSet = []string{}
	}
	for _, c := range plan.Candidates {
		dto.Candidates = append(dto.Candidates, placementCandidateDTO{
			InstanceID: c.InstanceID,
			Weight:     c.Weight,
			Score:      c.Score,
			Eligible:   c.Eligible,
			Preferred:  c.Preferred,
			Reason:     c.Reason,
		})
	}
	if dto.Candidates == nil {
		dto.Candidates = []placementCandidateDTO{}
	}
	if plan.Override != nil {
		dto.Override = &placementOverrideDTO{
			PinnedInstanceID: plan.Override.PinnedInstanceID,
			Replicas:         plan.Override.Replicas,
		}
	}
	writeJSON(w, http.StatusOK, dto)
}
