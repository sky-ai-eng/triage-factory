// Package teams serves /api/teams: the caller's team list, the single read,
// the org-admin create and rename, and the archive/restore lifecycle.
package teams

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/promptseed"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

var teamsLog = logging.Component("teams")

// Handler serves /api/teams — the caller's team list and the org-admin
// "add team" affordance. It reads/writes through the transactional store
// runner, gates the create on az.UserIsOrgAdmin, and runs the post-commit
// new-team bootstrap (prompts/rules/bot defaults) against the full store
// bundle, which must run outside the request transaction.
type Handler struct {
	tx        db.TxRunner
	az        *authz.Checker
	allStores db.Stores
	// spawner is read through a getter so the archive force-stop cascade
	// (TFAC-448) always sees the current delegation spawner, which is wired
	// onto the server after construction (SetSpawner) and hot-swapped on
	// credential change. May be nil before startup finishes; the archive
	// handler guards.
	spawner func() *delegate.Spawner
	// memoryOwed rings the memory doorbell for a conversation the archive
	// cascade just ended. The relay callbacks live on the Server, so the single
	// construction site in the server's routes() hands this handler the one
	// method rather than a back-reference to the whole server — the same shape
	// the server's settingsHandler takes kickJira in.
	memoryOwed func(orgID, conversationID string)
}

// New builds the teams handler. spawner is read on every archive, not once
// here; see Handler.spawner. Either func may be nil: the archive cascade then
// stops no process, or rings no doorbell.
func New(tx db.TxRunner, az *authz.Checker, allStores db.Stores, spawner func() *delegate.Spawner, memoryOwed func(orgID, conversationID string)) *Handler {
	return &Handler{tx: tx, az: az, allStores: allStores, spawner: spawner, memoryOwed: memoryOwed}
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
	ID          string `json:"id" doc:"The team's id."`
	Name        string `json:"name" doc:"The team's display name."`
	Slug        string `json:"slug" doc:"The team's slug, unique within its organization."`
	Role        string `json:"role,omitempty" doc:"The caller's role on the team: admin, member, or viewer. Absent when the caller is not on the team, on the archived teams list, and on the responses of an update or a restore."`
	Description string `json:"description" doc:"The team's description, empty when it has none."`
	// ArchivedAt is the soft-delete timestamp (RFC3339 UTC), present only on
	// an archived team. The active list filters archived teams out, so this is
	// empty there and populated on the archived list and the single read.
	ArchivedAt   string `json:"archived_at,omitempty" doc:"When the team was archived, as an RFC 3339 timestamp in UTC, present only on an archived team."`
	IsLastActing bool   `json:"is_last_acting,omitempty" doc:"Present and true on the team the caller last acted as, which a client offers first; at most one team in a response carries it."`
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

var opTeamsList = httpx.NewListOp[teamListRequest, teamJSON](httpx.OpSpec{
	Pattern:     "POST /api/teams/list",
	ID:          "list_teams",
	Summary:     "List the caller's teams",
	Description: "Returns one page of the active teams the caller belongs to in their current organization, each with the caller's role. In local mode it returns the one local team.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
func (th *Handler) handleTeamsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject

	req, ok := opTeamsList.Decode(w, r)
	if !ok {
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
	if err := th.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		teams, total, e = tx.Teams.ListForUser(r.Context(), orgID, db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		if e != nil {
			return e
		}
		lastActing, e = tx.Users.GetLastActingTeam(r.Context(), userID)
		return e
	}); err != nil {
		httpx.InternalError(w, "teams", err)
		return
	}

	out := make([]teamJSON, len(teams))
	for i, t := range teams {
		out[i] = teamToJSON(t, lastActing)
	}
	opTeamsList.Write(w, page, out, total)
}

// orgTeamsListKey is the org list's page-token fingerprint. It names the list
// and the org, so a token minted by the caller's own list, or by another
// org's, is refused here.
type orgTeamsListKey struct {
	List  string `json:"list"`
	OrgID string `json:"org_id"`
}

var opOrgTeamsList = httpx.NewListOp[teamListRequest, teamJSON](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/teams/list",
	ID:          "list_org_teams",
	Summary:     "List an organization's teams",
	Description: "Returns one page of every active team in the organization, including teams the caller is not on, with the caller's role on the teams they belong to.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleOrgTeamsList returns every active team in the org the path names, for
// a caller who has to name teams they may not be on: an org admin's views
// across the org's teams. Any member of the org may read it, as they may read
// any one of its teams, and each row is the caller's own list's row: their
// role on a team they are on, none on the rest.
//
// POST /api/orgs/{org_id}/teams/list
func (th *Handler) handleOrgTeamsList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := th.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	req, ok := opOrgTeamsList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(orgTeamsListKey{List: "org", OrgID: orgID}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		teams      []domain.Team
		total      int
		lastActing string
	)
	if err := th.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		teams, total, e = tx.Teams.ListForOrg(r.Context(), orgID, db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		if e != nil {
			return e
		}
		lastActing, e = tx.Users.GetLastActingTeam(r.Context(), userID)
		return e
	}); err != nil {
		httpx.InternalError(w, "teams", err)
		return
	}

	out := make([]teamJSON, len(teams))
	for i, t := range teams {
		out[i] = teamToJSON(t, lastActing)
	}
	opOrgTeamsList.Write(w, page, out, total)
}

var opTeamGet = httpx.NewJSONOp[httpx.NoBody, teamJSON](httpx.OpSpec{
	Pattern:     "GET /api/teams/{team_id}",
	ID:          "get_team",
	Summary:     "Get a team",
	Description: "Returns one of the caller's organization's teams, archived or not, with the caller's role on it when they belong to it.",
	PathParams:  map[string]string{"team_id": "The team's id, or default for the one team in local mode."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
func (th *Handler) handleTeamGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	teamID, ok := th.az.TeamIDFromPath(w, r, "teams", orgID, userID)
	if !ok {
		return
	}

	var (
		team       *domain.Team
		lastActing string
	)
	if err := th.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		team, e = tx.Teams.Get(r.Context(), orgID, teamID)
		if e != nil || team == nil {
			return e
		}
		lastActing, e = tx.Users.GetLastActingTeam(r.Context(), userID)
		return e
	}); err != nil {
		httpx.InternalError(w, "teams", err)
		return
	}
	if team == nil {
		httpx.NotFound(w, "team")
		return
	}
	opTeamGet.Write(w, teamToJSON(*team, lastActing))
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
	slug := domain.Slugify(name)
	if slug == "" {
		v.Invalid("name", "name must contain letters or numbers")
	}
	return slug
}

// teamUpdateRequest is the body of PATCH /api/teams/{team_id}. Both fields are
// pointers, so an explicit null reads as absent, and the description is
// cleared with an empty string.
type teamUpdateRequest struct {
	Name        *string `json:"name" doc:"The team's new display name, at most 100 characters, with at least one letter or number; the slug is derived from it again. Absent or null leaves the name unchanged."`
	Description *string `json:"description" doc:"The team's new description, at most 500 characters; an empty string clears it, and absent or null leaves it unchanged."`
}

var opTeamUpdate = httpx.NewJSONOp[teamUpdateRequest, teamJSON](httpx.OpSpec{
	Pattern:     "PATCH /api/teams/{team_id}",
	ID:          "update_team",
	Summary:     "Rename a team or change its description",
	Description: "Sets the team's name, its description, or both, and returns the team with the change applied. Renaming derives a new slug from the name. A team admin or an organization admin may call it, and an archived team is refused. Multi mode only.",
	PathParams:  map[string]string{"team_id": "The team's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleTeamUpdate renames a team and/or rewrites its description. Multi-mode
// only (local is N=1; 404 matches the create affordance's posture). Gated
// team-admin-or-org-admin: a team admin can edit their own team (the widened
// teams_update RLS), an org admin can edit any team in the org, and a plain
// member gets a 403. A cross-org team_id 404s via VerifyTeamInOrg before the
// role gate, so it can't leak the team's existence.
//
// Body is a partial PATCH: { "name"?: "<display name>", "description"?: "<blurb>" }.
// A present name must be non-empty and within the length cap, and its slug is
// re-derived (the same domain.Slugify Create uses) so name and slug stay in
// sync — there's no separate slug field. At least one field must be present.
//
// PATCH /api/teams/{team_id}  body: { "name"?: "...", "description"?: "..." }
func (th *Handler) handleTeamUpdate(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		// Multi-only: local mode has exactly one team by construction and
		// hides the whole team-management surface. A route that doesn't
		// exist in this deployment mode is a 404, not a 501.
		httpx.NotFound(w, "route")
		return
	}
	orgID, ok := httpx.RequireOrg(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject

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

	body, ok := opTeamUpdate.Decode(w, r)
	if !ok {
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
		opTeamUpdate.Write(w, teamToJSON(updated, ""))
	case errors.Is(err, db.ErrTeamNotFound):
		// Raced past VerifyTeamInOrg (deleted between gate and write) — 404.
		httpx.NotFound(w, "team")
	case httpx.IsUniqueViolation(err):
		// Re-deriving slug from the new name collided with a sibling team —
		// same 409 + generic message as Create.
		writeDuplicateTeamName(w)
	default:
		httpx.InternalError(w, "teams", err)
	}
}

// requireTeamAdminOrOrgAdmin confirms the caller may rename the team: a team
// admin (the widened teams_update RLS) OR an org admin. On not-allowed it writes
// 403 and returns false — unlike the roster's non-disclosure 404, the caller is
// already inside the org and the cross-org case 404'd at VerifyTeamInOrg, so a
// plain member learns only that they lack the role, not whether the team exists.
func (th *Handler) requireTeamAdminOrOrgAdmin(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	isTeamAdmin, err := th.az.UserIsTeamAdmin(r.Context(), userID, orgID, teamID)
	if err != nil {
		httpx.InternalError(w, "teams", err)
		return false
	}
	if isTeamAdmin {
		return true
	}
	isOrgAdmin, err := th.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		httpx.InternalError(w, "teams", err)
		return false
	}
	if !isOrgAdmin {
		httpx.Forbidden(w, "team admin or org admin role required")
		return false
	}
	return true
}

// teamCreateRequest is the body of POST /api/orgs/{org_id}/teams.
type teamCreateRequest struct {
	Name string `json:"name" doc:"The team's display name, at most 100 characters, with at least one letter or number."`
	Slug string `json:"slug" doc:"The team's slug: lowercase ASCII letters and digits in words joined by single hyphens, at most 48 characters, such as platform-eng. Absent or empty derives it from name."`
}

var opTeamCreate = httpx.NewJSONOp[teamCreateRequest, teamJSON](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/teams",
	ID:          "create_team",
	Summary:     "Create a team",
	Description: "Creates a team in the organization, gives it the shipped prompts, blueprints, and event handlers, makes the caller its admin, and returns it. Only an organization admin may call it. Multi mode only.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Status:      http.StatusCreated,
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintNo, Idempotent: httpx.HintNo, OpenWorld: httpx.HintNo},
})

// handleTeamCreate is the org-admin "add team" affordance — the multi-
// only path a solo user takes to grow past one team, at which point the
// count-gated selectors begin rendering. Multi-mode only (local is N=1);
// 404 in local matches the "feature absent" posture. Gated on org-admin;
// the new team enrolls the creator so it shows up in their team list.
//
// POST /api/orgs/{org_id}/teams  body: { "name": "<display name>", "slug"?: "<slug>" }
func (th *Handler) handleTeamCreate(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		// Multi-only: local mode has exactly one team by construction. A
		// route absent in this mode is a 404, not a 501.
		httpx.NotFound(w, "route")
		return
	}
	// A member who is not an admin gets 403 naming the role; a caller outside
	// the org gets 404.
	orgID, userID, ok := th.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}

	body, ok := opTeamCreate.Decode(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(body.Name)
	var v httpx.Validation
	derived := validateTeamName(&v, name)
	// An explicit slug overrides the derived one. It must already be in the
	// form Slugify produces, which is what the column holds: a value Slugify
	// would change is refused rather than rewritten, so the stored slug is
	// the one the caller sent.
	slug := derived
	if body.Slug != "" {
		slug = body.Slug
		if domain.Slugify(slug) != slug {
			v.Invalid("slug", fmt.Sprintf("slug must be lowercase letters and digits, in words joined by single hyphens, at most %d characters, such as platform-eng", domain.MaxSlugLen))
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
		if httpx.IsUniqueViolation(err) {
			writeDuplicateTeamName(w)
			return
		}
		httpx.InternalError(w, "teams", err)
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
	opTeamCreate.Write(w, teamToJSON(created, ""))
}

// Register mounts the teams routes.
func (th *Handler) Register(m httpx.Mounts) {
	// Teams. The list read is the data source for the per-page read filter and
	// the write-time picker (count-gated to ≥2 teams in the frontend); it
	// carries the last-acting-team the picker seeds from, maintained
	// server-side on each write — a recency signal, not a user preference, so
	// there is no endpoint to set it explicitly. POST /api/orgs/{org_id}/teams
	// is the org-admin "add team" affordance (multi-only; 404 in local).
	m.Op(opTeamsList, th.handleTeamsList)
	// Every active team in the org, for any of its members, with the caller's
	// role on the ones they are on: the names an org-wide view needs for the
	// teams the caller's own list leaves out.
	m.Op(opOrgTeamsList, th.handleOrgTeamsList)
	m.Op(opTeamCreate, th.handleTeamCreate)
	// The canonical single read, and the only reader of a team's description.
	// Any org member may read any team in their org; a cross-org id 404s under
	// RLS. {team_id} takes the literal "default" in local mode
	// (authz.ResolveTeamID).
	m.Op(opTeamGet, th.handleTeamGet)
	// PATCH /api/teams/{team_id} renames a team / edits its description
	// (multi-only; 404 in local). Gated team-admin-or-org-admin; a plain
	// member 403s, a cross-org team_id 404s (VerifyTeamInOrg).
	m.Op(opTeamUpdate, th.handleTeamUpdate)
	// Team archive/restore lifecycle, org-admin only, multi-mode. Archive
	// soft-deletes + force-stops the team's in-flight delegations and blocks
	// further writes; restore flips it back, and dead conversations stay dead.
	// The preview + archived-list back the confirm modal and the org-admin
	// restore surface.
	m.Op(opTeamArchivedList, th.handleTeamArchivedList)
	m.Op(opTeamArchivePreview, th.handleTeamArchivePreview)
	m.Op(opTeamArchive, th.handleTeamArchive)
	m.Op(opTeamRestore, th.handleTeamRestore)
}
