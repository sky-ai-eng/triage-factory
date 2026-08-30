package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/promptseed"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
)

// orgCreateResponse is the wire shape POST /api/orgs returns. The
// frontend doesn't read it on the happy path — it calls auth.refresh()
// and the membership-appears effect routes in — but echoing the IDs +
// slug keeps the response self-describing.
//
// AlreadyProvisioned is the local arm's answer to "did this call do the
// work, or find it already done" — the route is idempotent there (one
// workspace per install), so a caller that cares needs it said. Multi
// mode always creates a net-new org, so it is always false; the field is
// present in both arms rather than omitempty so the route answers one
// shape in every state.
type orgCreateResponse struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Slug               string `json:"slug"`
	TeamID             string `json:"team_id"`
	AlreadyProvisioned bool   `json:"already_provisioned"`
}

// orgJSON is the org's canonical read shape, matching the rows /api/me carries
// in its `orgs` list — the same three facts about the same object, so a client
// reading one org and a client reading its own list parse one thing.
//
// Role is the CALLER's org role, named as the caller-relative annotation it is.
type orgJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// handleOrgGet is the canonical single read for an org. Member-visible: any
// member of the org may read it, and a non-member (or a nonexistent org) gets
// 404 from RequireOrgMember rather than a 403 that would confirm the org
// exists.
//
// GET /api/orgs/{org_id}
func (s *Server) handleOrgGet(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var (
		org  *domain.Org
		role string
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		org, e = tx.Orgs.GetOrg(r.Context(), orgID)
		if e != nil || org == nil {
			return e
		}
		role, e = tx.OrgMemberships.RoleFor(r.Context(), orgID, userID)
		return e
	}); err != nil {
		internalError(w, "orgs", err)
		return
	}
	if org == nil {
		// RequireOrgMember already answered for a non-member, so this is the
		// narrow race where the org vanished between the gate and the read.
		notFound(w, "org")
		return
	}
	writeJSON(w, http.StatusOK, orgJSON{ID: org.ID, Name: org.Name, Role: role})
}

// handleOrgCreate is the "Start your Factory" create-workspace action —
// the destination of the onboarding entry's CTA in both modes. What it
// does is create the tenant a user works in; the modes differ only in
// how much of one there is to create.
//
// Multi mode creates a net-new org with DEFAULT settings (the configure
// step is a follow-up ticket) through the shared bootstrap path, makes
// the caller its owner + Default-team admin, then points their session
// at the new org so the next /api/me reflects the membership and the
// frontend auto-routes in.
//
// Local mode has exactly one org — the runmode.LocalDefault* sentinel —
// so there is nothing for a body to name and the arm takes none: it runs
// the shared BootstrapLocalOrg chain (createLocalOrg) and answers the
// same shape, with already_provisioned telling the caller whether this
// call did the work.
//
// POST /api/orgs  body: { "name": "<display name>" } (multi mode)
//
// Any authed user may create an org — the data model already allows a
// user to own multiple. Returns 403 when org creation is disabled on the
// instance (defense-in-depth behind the UI gate); local is never gated,
// since refusing there would leave the install with no workspace at all.
func (s *Server) handleOrgCreate(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		s.createLocalOrg(w, r)
		return
	}
	if s.authDeps == nil {
		// Multi mode without auth wiring can't own a session to point at
		// the new org. 404 matches the "feature absent" posture.
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
		// Creating an org points the caller's session cursor at the new one, so
		// this verb needs a session and a token-authed caller gets 401 — not a
		// wiring fault, hence the conditional ERROR.
		if httpx.TokenAuthFrom(r.Context()) == nil {
			orgsLog.Error("org create: no session in context, route missing withsession")
		}
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

// createLocalOrg is handleOrgCreate's local-mode arm: provision the single
// synthetic tenant — the runmode.LocalDefault* org / user / team rows plus
// the shipped agent, prompts, blueprints and handlers — and answer with the
// org it resolved to.
//
// Idempotent, and idempotent in a specific way: it no-ops once the tenant is
// *fully* provisioned, which is the non-resurrection guarantee. A user who
// deleted a shipped prompt and hits this route again keeps it deleted. See
// ensureLocalOrgProvisioned for what "fully" means and why the partial state
// is safe to re-run.
//
// The request body is not read. Local's org identity is the sentinel — its
// name and slug are seeded rows, not a caller's choice — so there is nothing
// a body could say, and a name sent by a multi-mode-shaped client is ignored
// rather than half-applied.
//
// 201 when this call provisioned the tenant, 200 when it found one already
// there: the same create-or-find distinction POST /api/tasks draws.
func (s *Server) createLocalOrg(w http.ResponseWriter, r *http.Request) {
	alreadyProvisioned, err := s.ensureLocalOrgProvisioned(r.Context())
	if err != nil {
		internalError(w, "orgs", fmt.Errorf("provision local workspace: %w", err))
		return
	}

	// Read back the org the sentinel names rather than restating the seeded
	// name and slug here — one source of truth for what the row says.
	org, err := s.orgs.GetOrgSystem(r.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		internalError(w, "orgs", fmt.Errorf("read local org: %w", err))
		return
	}
	if org == nil {
		// ensureLocalOrgProvisioned just committed the row (or found it), so
		// an absent org here is a provisioning fault, not a 404.
		internalError(w, "orgs", errors.New("local org missing after provision"))
		return
	}

	status := http.StatusCreated
	if alreadyProvisioned {
		status = http.StatusOK
	}
	writeJSON(w, status, orgCreateResponse{
		ID:                 org.ID,
		Name:               org.Name,
		Slug:               org.Slug,
		TeamID:             runmode.LocalDefaultTeamID,
		AlreadyProvisioned: alreadyProvisioned,
	})
}
