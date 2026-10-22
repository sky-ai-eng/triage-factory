package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/placement"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// placementResolver is the narrow view the explainer needs of
// *placement.Resolver: compute the full candidate order for a key,
// even when the layer is advisory-off, so an operator can preview placement.
type placementResolver interface {
	Explain(ctx context.Context, orgID, repositoryID string) (placement.Plan, error)
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
// candidate order for the repository's key, the preferred set the enqueue
// stamp draws from, whether the layer is live or advisory-off, the
// aging/liveness windows the claim runs with, and any override in effect. This
// is the debuggability that made a mutable routing table tempting — delivered
// as a pure view over deterministic state (spec §6.1).
//
// repository_id is the registry row the key is (the value the rendezvous hash
// ranks on and the row any override belongs to); host and repo are that
// row's current host and name, display values read off the row.
type placementExplainDTO struct {
	Enabled      bool                    `json:"enabled"`
	OrgID        string                  `json:"org_id"`
	KeyKind      string                  `json:"key_kind"`
	RepositoryID string                  `json:"repository_id"`
	Host         string                  `json:"host"`
	Repo         string                  `json:"repo"`
	PreferredSet []string                `json:"preferred_set"`
	Candidates   []placementCandidateDTO `json:"candidates"`
	Override     *placementOverrideDTO   `json:"override,omitempty"`
	AgingSeconds float64                 `json:"aging_seconds"`
	LivenessSec  float64                 `json:"liveness_seconds"`
}

// handleFleetPlacement is the placement explainer (spec §6.1): for a
// repository it returns the computed rendezvous candidate order and why
// (weights, overrides, liveness) — the same order the claim honors. Org-admin
// gated on the ?org= query (a dedicated fleet operator identity is a later
// ticket); the answer names executor instance ids for the caller's own org's
// work, so org-admin is the right scope until then.
//
// ?repo= names the repository the way an operator knows it, owner/repo on the
// org's current GitHub host, and is resolved to its registry row here, once:
// the key and any override are that row's. A name no live row answers to is a
// 404, since no run is keyed on a repository TF holds no row for.
//
// GET /api/fleet/placement?org=<uuid>&repo=<owner/repo>[&kind=repo]
func (s *Server) handleFleetPlacement(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return
	}
	if s.placement == nil {
		// This pod runs no placement resolver — deployment shape, not a
		// transient outage.
		writeNotConfigured(w, "the placement resolver is not configured on this deployment")
		return
	}

	orgID := strings.TrimSpace(r.URL.Query().Get("org"))
	if orgID == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidParam, Message: "org query parameter is required", Field: "org",
		})
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
	if s.refuseSSOBarred(w, r, orgID) {
		return
	}

	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind == "" {
		kind = domain.PlacementKindRepo
	}
	if kind != domain.PlacementKindRepo {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidParam, Message: "kind must be 'repo'", Field: "kind",
		})
		return
	}
	slug := strings.TrimSpace(r.URL.Query().Get("repo"))
	if slug == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidParam, Message: "repo query parameter is required", Field: "repo",
		})
		return
	}
	owner, name, ok := strings.Cut(slug, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidParam, Message: "repo must be owner/repo", Field: "repo",
		})
		return
	}

	host, ok := s.requireOrgGitHubHost(r.Context(), w, orgID, "fleet-placement")
	if !ok {
		return
	}
	repo, err := s.repos.GetByRefSystem(r.Context(), orgID, domain.RepoRef{Host: host, Owner: owner, Repo: name})
	if err != nil {
		internalError(w, "fleet-placement", err)
		return
	}
	if repo == nil {
		notFound(w, "repository")
		return
	}
	plan, err := s.placement.Explain(r.Context(), orgID, repo.ID)
	if err != nil {
		internalError(w, "fleet-placement", err)
		return
	}

	dto := placementExplainDTO{
		Enabled:      plan.Enabled,
		OrgID:        plan.OrgID,
		KeyKind:      kind,
		RepositoryID: plan.RepositoryID,
		Host:         repo.Host,
		Repo:         repo.Slug(),
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
