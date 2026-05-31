package server

import (
	"log"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// teamJSON is the wire shape the multi-team selectors enumerate. Slug is
// included for a stable, human-readable secondary label; the frontend
// renders name as the primary.
type teamJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// teamsResponse is GET /api/teams. Teams is the caller's teams in the
// active org (the count drives whether the frontend renders any team
// control — the ≥2 gate). LastActingTeamID is the caller's sticky default
// when it is still one of those teams (a stale default is omitted so the
// frontend never seeds to a team that isn't offered).
type teamsResponse struct {
	Teams           []teamJSON `json:"teams"`
	LastActingTeamID string     `json:"last_acting_team_id,omitempty"`
}

// handleTeamsList returns the caller's teams in the active org plus their
// sticky default. The single data source for both selectors: the per-page
// read filter and the write-time picker. Renders in both modes — local
// returns the one local team (so the frontend's ≥2 gate keeps every
// control hidden) without a mode branch here.
//
// GET /api/teams
func (s *Server) handleTeamsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var (
		teams     []domain.Team
		preferred string
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		teams, e = tx.Teams.ListForUser(r.Context(), orgID)
		if e != nil {
			return e
		}
		preferred, e = tx.Users.GetLastActingTeam(r.Context(), userID)
		return e
	}); err != nil {
		internalError(w, "teams", err)
		return
	}

	out := make([]teamJSON, len(teams))
	validPreferred := ""
	for i, t := range teams {
		out[i] = teamJSON{ID: t.ID, Name: t.Name, Slug: t.Slug}
		if t.ID == preferred {
			validPreferred = preferred
		}
	}
	writeJSON(w, http.StatusOK, teamsResponse{Teams: out, LastActingTeamID: validPreferred})
}

// handleTeamCreate is the org-admin "add team" affordance — the hosted-
// only path a solo user takes to grow past one team, at which point the
// count-gated selectors begin rendering. Multi-mode only (local is N=1);
// 404 in local matches the "feature absent" posture. Gated on org-admin;
// the new team enrolls the creator so it shows up in their team list.
//
// POST /api/teams  body: { "name": "<display name>", "slug"?: "<slug>" }
func (s *Server) handleTeamCreate(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		// Hosted-only: local mode has exactly one team by construction.
		http.NotFound(w, r)
		return
	}
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	isAdmin, err := s.userIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		internalError(w, "teams", err)
		return
	}
	if !isAdmin {
		// 404 not 403 — same non-disclosure posture as withOrg /
		// requireOrgAdmin: don't reveal the org to a non-admin.
		http.NotFound(w, r)
		return
	}

	var body struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		badRequest(w, "name is required")
		return
	}
	slug := strings.TrimSpace(body.Slug)
	if slug == "" {
		slug = slugify(name)
	} else {
		slug = slugify(slug)
	}
	if slug == "" {
		badRequest(w, "name must contain letters or numbers")
		return
	}

	var created domain.Team
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		created, e = tx.Teams.Create(r.Context(), orgID, name, slug, userID)
		return e
	}); err != nil {
		// UNIQUE (org_id, slug) collision → 409 with a generic message
		// (don't echo the index name). SQLite: "UNIQUE constraint";
		// Postgres: "duplicate key". The constraint is on the *slug*, not
		// the name, so two distinct names can collide ("Engineering" and
		// "Engineering!" both slugify to "engineering") — say so.
		if strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "duplicate key") {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "a team with that name or slug already exists",
			})
			return
		}
		internalError(w, "teams", err)
		return
	}

	// Materialize the shipped defaults for the new team — its
	// default-enabled bot membership + the shipped event handlers
	// (rules + triggers) scoped to it. Runs AFTER the team row commits
	// because the seeders route through the admin pool and refuse to run
	// inside the request's WithTx. Idempotent. Log-and-continue on
	// failure: the team exists and is usable; a missing-defaults team is
	// degraded (auto-delegation won't fire) but repairable by re-running
	// bootstrap, whereas failing the create after the row committed would
	// orphan it. (SKY-378 — D7 team-create bootstrap follow-through.)
	if err := db.BootstrapNewTeam(r.Context(), s.allStores, orgID, created.ID); err != nil {
		log.Printf("[teams] new team %s/%s created but bootstrap failed (shipped rules/bot may be missing): %v", orgID, created.ID, err)
	}

	writeJSON(w, http.StatusCreated, teamJSON{ID: created.ID, Name: created.Name, Slug: created.Slug})
}
