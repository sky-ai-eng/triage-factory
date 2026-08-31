package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/promptseed"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
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
	// spawner is read through a getter so the archive force-stop cascade
	// (TFAC-448) always sees the current delegation spawner, which is wired
	// onto the server after construction (SetSpawner) and hot-swapped on
	// credential change. May be nil before startup finishes; the archive
	// handler guards.
	spawner func() *delegate.Spawner
}

// teamJSON is the one wire shape for a team: the list rows, the single read,
// the archived list, and the PATCH response all serialize this. Slug is
// included for a stable, human-readable secondary label; the frontend renders
// name as the primary.
//
// Two of its fields are caller-relative and named as such rather than folded
// into a second envelope key:
//
//   - Role is the caller's membership role in the team ("admin" | "member" |
//     "viewer"). The settings surface renders the Team section only for users
//     who admin ≥1 team and filters its selector to those teams, so a
//     non-admin never sees fields that would 403 on save. Empty when the
//     caller is not on the team (an org admin reading a team they never
//     joined) or when the route has no membership to report.
//   - IsLastActing marks the caller's sticky default team. At most one row in
//     a result carries it, and a stale default carries it nowhere — so the
//     frontend never seeds a selector to a team that isn't offered.
type teamJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Role        string `json:"role,omitempty"`
	Description string `json:"description"`
	// ArchivedAt is the soft-delete timestamp (RFC3339 UTC), present only on
	// an archived team. The active list filters archived teams out, so this is
	// empty there and populated on the archived list and the single read.
	ArchivedAt   string `json:"archived_at,omitempty"`
	IsLastActing bool   `json:"is_last_acting,omitempty"`
}

// teamToJSON renders a stored team. lastActing is the caller's sticky default
// team id, which the single read and the active list pass through and the
// lifecycle routes leave empty.
func teamToJSON(t domain.Team, lastActing string) teamJSON {
	archivedAt := ""
	if t.DeletedAt != nil {
		archivedAt = t.DeletedAt.UTC().Format(time.RFC3339)
	}
	return teamJSON{
		ID:           t.ID,
		Name:         t.Name,
		Slug:         t.Slug,
		Role:         t.Role,
		Description:  t.Description,
		ArchivedAt:   archivedAt,
		IsLastActing: lastActing != "" && t.ID == lastActing,
	}
}

// writeDuplicateTeamName answers the (org_id, slug) collision both create and
// rename can hit. Generic by design — the index name is not the caller's
// business.
func writeDuplicateTeamName(w http.ResponseWriter) {
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
		Reason: httpx.ReasonDuplicateName, Message: "a team with that name or slug already exists", Field: "name",
	})
}

// teamListRequest is the body of POST /api/teams/list. The resource is "the
// teams I belong to", which has no filters of its own, so the body is the
// paging pair alone.
type teamListRequest struct {
	httpx.PageRequest
}

// handleTeamsList returns the caller's active teams in the org. The single
// data source for both selectors: the per-page read filter and the write-time
// picker. Renders in both modes — local returns the one local team (so the
// frontend's ≥2 gate keeps every control hidden) without a mode branch here.
//
// The caller's sticky default team rides on the row that is it
// (`is_last_acting`) rather than as a second top-level key: the envelope is
// the shared list shape, and a default that has been archived or left simply
// marks no row, which is the same "omit a stale default" the old
// `last_acting_team_id` gave.
//
// POST /api/teams/list
func (th *teamsHandler) handleTeamsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req teamListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		teams      []domain.Team
		total      int
		lastActing string
	)
	if err := th.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		teams, total, e = tx.Teams.ListForUser(r.Context(), orgID, db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
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
	for i, t := range teams {
		out[i] = teamToJSON(t, lastActing)
	}
	httpx.WriteList(w, page, out, total)
}

// handleTeamGet is the canonical single read. It is the only reader of a
// team's description — before it, the column was writable through PATCH and
// unreadable anywhere.
//
// Any member of the org may read any team in it: the org roster and the
// archived-teams surface both name teams the caller may not belong to, so
// hiding one here would deny the existence of a row they can already see. The
// team's own membership is reported as `role` (empty when the caller is not on
// it). A team in another org is invisible under RLS and answers 404, which is
// the disclosure rule's own answer rather than a handler predicate's.
//
// GET /api/teams/{team_id}
func (th *teamsHandler) handleTeamGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, ok := th.az.TeamIDFromPath(w, r, "teams", orgID, userID)
	if !ok {
		return
	}

	var (
		team       *domain.Team
		lastActing string
	)
	if err := th.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		team, e = tx.Teams.Get(r.Context(), orgID, teamID)
		if e != nil || team == nil {
			return e
		}
		lastActing, e = tx.Users.GetLastActingTeam(r.Context(), userID)
		return e
	}); err != nil {
		internalError(w, "teams", err)
		return
	}
	if team == nil {
		notFound(w, "team")
		return
	}
	writeJSON(w, http.StatusOK, teamToJSON(*team, lastActing))
}

// maxTeamNameLen caps a team name's length (in runes). A generous bound that
// keeps a pasted blob out of the column without constraining any real team
// name. Enforced at every door that writes the column — see validateTeamName.
const maxTeamNameLen = 100

// maxTeamDescriptionLen caps the description blurb (in runes). The column is
// unbounded TEXT, so this keeps a pasted document out of it — a blurb, not a
// wiki. Generous enough for a sentence or two of "what this team owns."
const maxTeamDescriptionLen = 500

// validateTeamName is the single team-name rule, so create and rename accept
// exactly the same names and refuse the rest with the same reason and field —
// a cap enforced at one door and not its sibling is a name that can be created
// and then never renamed. Takes an already-trimmed name, records every fault on
// v, and returns the derived slug (empty when the name yields none).
func validateTeamName(v *httpx.Validation, name string) string {
	if name == "" {
		v.Missing("name")
		return ""
	}
	if len([]rune(name)) > maxTeamNameLen {
		v.OutOfRange("name", fmt.Sprintf("name must be at most %d characters", maxTeamNameLen))
	}
	slug := slugify(name)
	if slug == "" {
		v.Invalid("name", "name must contain letters or numbers")
	}
	return slug
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
		// Multi-only: local mode has exactly one team by construction and
		// hides the whole team-management surface. A route that doesn't
		// exist in this deployment mode is a 404, not a 501.
		notFound(w, "route")
		return
	}
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	teamID, ok := th.az.TeamIDFromPath(w, r, "teams", orgID, userID)
	if !ok {
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
	if !httpx.DecodeJSONStrict(w, r, &body) {
		return
	}

	var namePtr, slugPtr, descPtr *string
	var v httpx.Validation
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		slug := validateTeamName(&v, name)
		namePtr, slugPtr = &name, &slug
	}
	if body.Description != nil {
		desc := strings.TrimSpace(*body.Description)
		if len([]rune(desc)) > maxTeamDescriptionLen {
			v.OutOfRange("description", fmt.Sprintf("description must be at most %d characters", maxTeamDescriptionLen))
		}
		descPtr = &desc
	}
	if namePtr == nil && descPtr == nil {
		v.Add(httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidBody,
			Message: "nothing to update: provide name and/or description",
		})
	}
	if v.Flush(w, http.StatusBadRequest) {
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
		// The list row shape, with role left empty: the write carries no
		// membership lookup, and the caller already knows their relationship
		// to a team they just had the role to edit.
		writeJSON(w, http.StatusOK, teamToJSON(updated, ""))
	case errors.Is(err, db.ErrTeamNotFound):
		// Raced past VerifyTeamInOrg (deleted between gate and write) — 404.
		notFound(w, "team")
	case isUniqueViolation(err):
		// Re-deriving slug from the new name collided with a sibling team —
		// same 409 + generic message as Create.
		writeDuplicateTeamName(w)
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
		forbidden(w, "team admin or org admin role required")
		return false
	}
	return true
}

// handleTeamCreate is the org-admin "add team" affordance — the multi-
// only path a solo user takes to grow past one team, at which point the
// count-gated selectors begin rendering. Multi-mode only (local is N=1);
// 404 in local matches the "feature absent" posture. Gated on org-admin;
// the new team enrolls the creator so it shows up in their team list.
//
// POST /api/teams  body: { "name": "<display name>", "slug"?: "<slug>" }
func (th *teamsHandler) handleTeamCreate(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		// Multi-only: local mode has exactly one team by construction. A
		// route absent in this mode is a 404, not a 501.
		notFound(w, "route")
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
		// The caller is a member of the org (requireOrg resolved it from
		// their own session), so the org is visible to them and the denial
		// names the role they lack. A 404 here would report their own
		// workspace missing.
		forbidden(w, "org admin role required")
		return
	}

	var body struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if !httpx.DecodeJSONStrict(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	var v httpx.Validation
	derived := validateTeamName(&v, name)
	// An explicit slug overrides the derived one; it goes through the same
	// slugify, so a value that survives it is a value the column can hold.
	slug := derived
	if raw := strings.TrimSpace(body.Slug); raw != "" {
		slug = slugify(raw)
		if slug == "" {
			v.Invalid("slug", "slug must contain letters or numbers")
		}
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var created domain.Team
	if err := th.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		created, e = tx.Teams.Create(r.Context(), orgID, name, slug, userID)
		return e
	}); err != nil {
		// UNIQUE (org_id, slug) collision → 409 with a generic message
		// (don't echo the index name); the kernel's shim owns the
		// per-driver strings. The constraint is on the *slug*, not the
		// name, so two distinct names can collide ("Engineering" and
		// "Engineering!" both slugify to "engineering") — say so.
		if isUniqueViolation(err) {
			writeDuplicateTeamName(w)
			return
		}
		internalError(w, "teams", err)
		return
	}

	// Materialize the defaults for the new team — its default-enabled bot
	// membership + its own copies of the prompts, blueprints, and event
	// handlers (rules + triggers), seeded directly from the TF-shipped set.
	// Runs AFTER the team row commits because the seeders route through the
	// admin pool and refuse to run inside the request's WithTx. Idempotent.
	// Log-and-continue on failure: the team exists and is usable; a
	// missing-defaults team is degraded (auto-delegation won't fire) but
	// repairable by re-running bootstrap, whereas failing the create after
	// the row committed would orphan it.
	if err := db.BootstrapNewTeam(r.Context(), th.allStores, orgID, created.ID, promptseed.Prompts(), promptseed.Blueprints()); err != nil {
		teamsLog.Warn("new team created but bootstrap failed, prompts/rules/bot may be missing", "org", orgID, "team", created.ID, "error", err)
	}

	// The creator is enrolled as admin (Teams.Create), so stamp the role on
	// the response — the settings Team selector lists admin'd teams, and this
	// lets a freshly-created team surface there without waiting on a refetch.
	created.Role = "admin"
	writeJSON(w, http.StatusCreated, teamToJSON(created, ""))
}
