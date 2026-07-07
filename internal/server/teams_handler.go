package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
)

// teamsHandler serves /api/teams — the caller's team list and the org-admin
// "add team" affordance. It reads/writes through the transactional store
// runner, gates the create on az.UserIsOrgAdmin, and runs the post-commit
// new-team bootstrap (prompts/rules/bot defaults) against the full store
// bundle, which must run outside the request transaction.
type teamsHandler struct {
	tx        db.TxRunner
	az        *authz.Checker
	allStores db.Stores
	// spawner / curator are read through getters so the archive force-stop
	// cascade (TFAC-448) always sees the current delegation spawner + curator
	// runtime, which are wired onto the server after construction (SetSpawner /
	// SetCurator) and hot-swapped on credential change. Either may be nil before
	// startup finishes; the archive handler guards.
	spawner func() *delegate.Spawner
	curator func() *curator.Curator
}

// teamJSON is the wire shape the multi-team selectors enumerate. Slug is
// included for a stable, human-readable secondary label; the frontend
// renders name as the primary. Role is the caller's membership role in the
// team ("admin" | "member" | "viewer") — the settings surface renders the
// Team section only for users who admin ≥1 team and filters its selector to
// those teams, so a non-admin never sees fields that would 403 on save.
type teamJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	Role string `json:"role"`
}

// teamsResponse is GET /api/teams. Teams is the caller's teams in the
// active org (the count drives whether the frontend renders any team
// control — the ≥2 gate). LastActingTeamID is the caller's sticky default
// when it is still one of those teams (a stale default is omitted so the
// frontend never seeds to a team that isn't offered).
type teamsResponse struct {
	Teams            []teamJSON `json:"teams"`
	LastActingTeamID string     `json:"last_acting_team_id,omitempty"`
}

// handleTeamsList returns the caller's teams in the active org plus their
// sticky default. The single data source for both selectors: the per-page
// read filter and the write-time picker. Renders in both modes — local
// returns the one local team (so the frontend's ≥2 gate keeps every
// control hidden) without a mode branch here.
//
// GET /api/teams
func (th *teamsHandler) handleTeamsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var (
		teams      []domain.Team
		lastActing string
	)
	if err := th.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		teams, e = tx.Teams.ListForUser(r.Context(), orgID)
		if e != nil {
			return e
		}
		lastActing, e = tx.Users.GetLastActingTeam(r.Context(), userID)
		return e
	}); err != nil {
		internalError(w, "teams", err)
		return
	}

	out := make([]teamJSON, len(teams))
	validLastActing := ""
	for i, t := range teams {
		out[i] = teamJSON{ID: t.ID, Name: t.Name, Slug: t.Slug, Role: t.Role}
		if t.ID == lastActing {
			validLastActing = lastActing
		}
	}
	writeJSON(w, http.StatusOK, teamsResponse{Teams: out, LastActingTeamID: validLastActing})
}

// maxTeamNameLen caps a team name's length (in runes) on rename. A generous
// bound that keeps a pasted blob out of the column without constraining any
// real team name; Create doesn't enforce it today, but rename is the explicit
// "edit this field" affordance so it validates.
const maxTeamNameLen = 100

// maxTeamDescriptionLen caps the description blurb (in runes). The column is
// unbounded TEXT, so this keeps a pasted document out of it — a blurb, not a
// wiki. Generous enough for a sentence or two of "what this team owns."
const maxTeamDescriptionLen = 500

// teamDetailJSON is the PATCH /api/teams/{team_id} response — the updated
// identity row plus its description (the field this endpoint manages). Role is
// omitted: the caller already knows their relationship to the team (they had to
// be team-admin-or-org-admin to reach the write), and the list endpoint carries
// role for the selectors.
type teamDetailJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
}

// handleTeamUpdate renames a team and/or rewrites its description. Multi-mode
// only (local is N=1; 404 matches the create affordance's posture). Gated
// team-admin-or-org-admin: a team admin can edit their own team (the widened
// teams_update RLS), an org admin can edit any team in the org, and a plain
// member gets a 403. A cross-org team_id 404s via VerifyTeamInOrg before the
// role gate, so it can't leak the team's existence.
//
// Body is a partial PATCH: { "name"?: "<display name>", "description"?: "<blurb>" }.
// A present name must be non-empty and within the length cap, and its slug is
// re-derived (the same slugify Create uses) so name and slug stay in sync —
// there's no separate slug field. At least one field must be present.
//
// PATCH /api/teams/{team_id}  body: { "name"?: "...", "description"?: "..." }
func (th *teamsHandler) handleTeamUpdate(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		// Hosted-only: local mode has exactly one team by construction and
		// hides the whole team-management surface.
		http.NotFound(w, r)
		return
	}
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	teamID := r.PathValue("team_id")
	if _, err := uuid.Parse(teamID); err != nil {
		http.NotFound(w, r)
		return
	}
	// Cross-org 404 before the role gate — non-disclosure of teams in other
	// orgs.
	if !th.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	// Block rename/description edits on an archived team — teams_update RLS gates
	// on team-admin-or-org-admin, which carries no archived filter (TFAC-448).
	if !th.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
		return
	}
	if !th.requireTeamAdminOrOrgAdmin(w, r, orgID, userID, teamID) {
		return
	}

	var body struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}

	var namePtr, slugPtr, descPtr *string
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			badRequest(w, "name cannot be empty")
			return
		}
		if len([]rune(name)) > maxTeamNameLen {
			badRequest(w, "name is too long")
			return
		}
		slug := slugify(name)
		if slug == "" {
			badRequest(w, "name must contain letters or numbers")
			return
		}
		namePtr, slugPtr = &name, &slug
	}
	if body.Description != nil {
		desc := strings.TrimSpace(*body.Description)
		if len([]rune(desc)) > maxTeamDescriptionLen {
			badRequest(w, "description is too long")
			return
		}
		descPtr = &desc
	}
	if namePtr == nil && descPtr == nil {
		badRequest(w, "nothing to update: provide name and/or description")
		return
	}

	var updated domain.Team
	err := th.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		updated, e = tx.Teams.Update(r.Context(), teamID, namePtr, slugPtr, descPtr)
		return e
	})
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, teamDetailJSON{
			ID: updated.ID, Name: updated.Name, Slug: updated.Slug, Description: updated.Description,
		})
	case errors.Is(err, db.ErrTeamNotFound):
		// Raced past VerifyTeamInOrg (deleted between gate and write) — 404.
		notFound(w, "team")
	case strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "duplicate key"):
		// Re-deriving slug from the new name collided with a sibling team —
		// same 409 + generic message as Create.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "a team with that name or slug already exists",
		})
	default:
		internalError(w, "teams", err)
	}
}

// requireTeamAdminOrOrgAdmin confirms the caller may rename the team: a team
// admin (the widened teams_update RLS) OR an org admin. On not-allowed it writes
// 403 and returns false — unlike the roster's non-disclosure 404, the caller is
// already inside the org and the cross-org case 404'd at VerifyTeamInOrg, so a
// plain member learns only that they lack the role, not whether the team exists.
func (th *teamsHandler) requireTeamAdminOrOrgAdmin(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	isTeamAdmin, err := th.az.UserIsTeamAdmin(r.Context(), userID, orgID, teamID)
	if err != nil {
		internalError(w, "teams", err)
		return false
	}
	if isTeamAdmin {
		return true
	}
	isOrgAdmin, err := th.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		internalError(w, "teams", err)
		return false
	}
	if !isOrgAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "team admin or org admin role required"})
		return false
	}
	return true
}

// handleTeamCreate is the org-admin "add team" affordance — the hosted-
// only path a solo user takes to grow past one team, at which point the
// count-gated selectors begin rendering. Multi-mode only (local is N=1);
// 404 in local matches the "feature absent" posture. Gated on org-admin;
// the new team enrolls the creator so it shows up in their team list.
//
// POST /api/teams  body: { "name": "<display name>", "slug"?: "<slug>" }
func (th *teamsHandler) handleTeamCreate(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		// Hosted-only: local mode has exactly one team by construction.
		http.NotFound(w, r)
		return
	}
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	isAdmin, err := th.az.UserIsOrgAdmin(r.Context(), userID, orgID)
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
	if err := th.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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

	// Materialize the defaults for the new team — its default-enabled bot
	// membership + its own copies of the prompts and event handlers (rules +
	// triggers), copied from the *org template* so the team inherits
	// the org's house rules, not just the TF-shipped set. Runs AFTER the team
	// row commits because the seeders route through the admin pool and refuse
	// to run inside the request's WithTx. Idempotent. Log-and-continue on
	// failure: the team exists and is usable; a missing-defaults team is
	// degraded (auto-delegation won't fire) but repairable by re-running
	// bootstrap, whereas failing the create after the row committed would
	// orphan it.
	if err := db.BootstrapNewTeam(r.Context(), th.allStores, orgID, created.ID); err != nil {
		teamsLog.Warn("new team created but bootstrap failed, prompts/rules/bot may be missing", "org", orgID, "team", created.ID, "error", err)
	}

	// The creator is enrolled as admin (Teams.Create), so stamp the role on
	// the response — the settings Team selector lists admin'd teams, and this
	// lets a freshly-created team surface there without waiting on a refetch.
	writeJSON(w, http.StatusCreated, teamJSON{ID: created.ID, Name: created.Name, Slug: created.Slug, Role: "admin"})
}
