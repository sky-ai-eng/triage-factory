package server

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/promptseed"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
)

// orgCreateResponse is the wire shape POST /api/orgs returns. The
// frontend doesn't read it on the happy path — it calls auth.refresh()
// and the membership-appears effect routes in — but echoing the IDs +
// slug keeps the response self-describing and parallels the local
// /api/setup/start shape.
type orgCreateResponse struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Slug   string `json:"slug"`
	TeamID string `json:"team_id"`
}

// handleOrgCreate is the multi-mode "Start your Factory" create-org
// action — the destination of the onboarding entry's CTA. It creates a
// net-new org with DEFAULT settings (the configure step is a follow-up
// ticket) through the shared bootstrap path, makes the caller its owner
// + Default-team admin, then points their session at the new org so the
// next /api/me reflects the membership and the frontend auto-routes in.
//
// POST /api/orgs  body: { "name": "<display name>" }
//
// Any authed user may create an org — the data model already allows a
// user to own multiple. Returns 403 when org creation is disabled on the
// instance (defense-in-depth behind the UI gate); 404 in local mode,
// which provisions via POST /api/setup/start instead.
func (s *Server) handleOrgCreate(w http.ResponseWriter, r *http.Request) {
	if s.authDeps == nil || runmode.Current() == runmode.ModeLocal {
		// Multi-mode only: local provisions its single tenant through
		// /api/setup/start. 404 matches the "feature absent" posture.
		notFound(w, "route")
		return
	}

	// Defense-in-depth behind the UI gate: the onboarding CTA is hidden
	// when org creation is prevented, but the endpoint must refuse too.
	if !runmode.OrgCreationEnabled() {
		forbidden(w, "org creation is disabled on this instance")
		return
	}

	claims := ClaimsFrom(r.Context())
	if claims == nil || claims.Subject == runmode.LocalDefaultUserID {
		// Sentinel-claim caller is the local-mode shim; nothing to do
		// here. 401 mirrors handleActiveOrgUpdate's gate.
		writeUnauth(w)
		return
	}
	sess := SessionFrom(r.Context())
	if sess == nil {
		// withSession sets this; absence is a route-wiring bug.
		orgsLog.Error("org create: no session in context, route missing withsession")
		writeUnauth(w)
		return
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		writeUnauth(w)
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if !httpx.DecodeJSONStrict(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "name is required", Field: "name",
		})
		return
	}
	// Cap the stored display name. The column is unbounded TEXT and the
	// slug derived below is length-capped separately, so without this a
	// pathologically long name would persist verbatim.
	if utf8.RuneCountInString(name) > 200 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonOutOfRange, Message: "name must be 200 characters or fewer", Field: "name",
		})
		return
	}
	slugBase := slugify(name)
	if slugBase == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "name must contain letters or numbers", Field: "name",
		})
		return
	}

	orgID, teamID, slug, err := s.provisionOrg(r.Context(), userID, name, slugBase)
	if err != nil {
		internalError(w, "orgs", err)
		return
	}

	// Materialize the shipped defaults: the org's agent, its template
	// (prompts + blueprints), the founder team's copies, and the team's
	// bot membership. Runs AFTER the provisioning tx commits because the
	// seeders route through the admin pool and refuse to run inside a
	// WithTx. Idempotent. Log-and-continue on failure: the org exists and
	// the founder is signed into it; a missing-defaults org is degraded
	// (auto-delegation won't fire) but repairable by re-running bootstrap,
	// whereas failing the create after the rows committed would orphan it.
	if err := db.BootstrapNewOrg(r.Context(), s.allStores, orgID.String(), teamID.String(), promptseed.Prompts(), promptseed.Blueprints()); err != nil {
		orgsLog.Warn("new org created but bootstrap failed, template/bot may be missing", "org", orgID, "team", teamID, "error", err)
	}

	// Point the session at the new org so the next /api/me carries it as
	// the active org and the app's org-scoped handlers resolve it.
	// Log-and-continue: the membership exists either way, so the
	// onboarding screen's membership-appears effect still routes in; the
	// founder can switch explicitly if this rare path failed.
	if err := s.authDeps.sessions.UpdateActiveOrgSystem(r.Context(), sess.ID, orgID); err != nil {
		orgsLog.Warn("set active org failed", "sid", sessions.LogID(sess.ID), "org", orgID, "error", err)
	}

	writeJSON(w, http.StatusCreated, orgCreateResponse{
		ID:     orgID.String(),
		Name:   name,
		Slug:   slug,
		TeamID: teamID.String(),
	})
}
