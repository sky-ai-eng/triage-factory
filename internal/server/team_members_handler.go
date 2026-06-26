package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
)

// teamMembersHandler serves /api/teams/{team_id}/members — the team roster
// (TFAC-444) and its add / change-role / remove(/leave) affordances. It mirrors
// orgMembersHandler's shape (transactional store runner + authz checker): read
// is any org member, mutate is team-admin-or-org-admin (or self for a leave).
// Every endpoint front-gates on VerifyTeamInOrg so a cross-org team_id 404s
// rather than leaking its existence.
//
// Multi-mode only: the whole surface 404s in local mode (N=1 keeps the synthetic
// single-member roster on GET /api/team/members). The last-admin invariant is
// enforced by the tf.guard_team_admins DB trigger, never re-implemented here;
// the store surfaces its SQLSTATE 23514 as db.ErrLastTeamAdminGuard, which the
// mutators translate into a friendly 409.
type teamMembersHandler struct {
	tx db.TxRunner
	az *authz.Checker
}

// teamRoles is the closed set of membership_role enum values the add/role
// handlers accept. Validating here keeps an invalid value from reaching the
// column (where it would surface as a driver error, not a clean 400) and pins
// the vocabulary the frontend adapter mirrors. 'viewer' is assignable now;
// read-only enforcement is a separate slice (TFAC-447) — until it lands a
// viewer behaves like a member.
var teamRoles = map[string]bool{"admin": true, "member": true, "viewer": true}

// teamRosterRow is one roster row. github_username / jira_account_id are null
// when the member holds no host-scoped identity binding on the org's host (the
// frontend renders that as a "Not connected" readiness badge). is_current_user
// lets the frontend swap the Remove control for Leave on the caller's own row.
type teamRosterRow struct {
	UserID         string  `json:"user_id"`
	DisplayName    string  `json:"display_name"`
	GitHubUsername *string `json:"github_username"`
	JiraAccountID  *string `json:"jira_account_id"`
	Role           string  `json:"role"`
	IsCurrentUser  bool    `json:"is_current_user"`
}

// teamRosterBotRow is the team's agent (team_agents) surfaced as an extra
// roster row — the bot is a workload identity, not a member, so it rides
// alongside Members rather than inside it. enabled/model/autonomy reflect the
// per-team team_agents overrides (falling back to the org agent's defaults).
// Null when the org has no bootstrapped agent.
type teamRosterBotRow struct {
	AgentID     string   `json:"agent_id"`
	DisplayName string   `json:"display_name"`
	Enabled     bool     `json:"enabled"`
	Model       string   `json:"model"`
	Autonomy    *float64 `json:"autonomy"`
}

type teamRosterResponse struct {
	Members []teamRosterRow   `json:"members"`
	Bot     *teamRosterBotRow `json:"bot"`
}

// handleTeamRosterList returns every member of the team with their role and
// identity readiness, plus the team bot row. Any org member may read (the
// memberships_select RLS gates on org access), so it gates on org membership +
// team-in-org, not admin.
//
// GET /api/teams/{team_id}/members
func (h *teamMembersHandler) handleTeamRosterList(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		http.NotFound(w, r)
		return
	}
	orgID, userID, teamID, ok := h.resolve(w, r)
	if !ok {
		return
	}

	var (
		members []domain.TeamMember
		agent   *domain.Agent
		ta      *domain.TeamAgent
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Identity is host-scoped: resolve the org's GitHub/Jira hosts from
		// org_settings, then list the team's members with their readiness.
		orgSet, e := tx.Orgs.GetSettings(r.Context(), orgID)
		if e != nil {
			return e
		}
		members, e = tx.Teams.ListMembers(r.Context(), teamID, orgSet.GitHubBaseURL, orgSet.JiraBaseURL)
		if e != nil {
			return e
		}
		// Bot row: the org's agent + this team's per-team override row. A
		// missing agent (un-bootstrapped) yields a null bot; the picker/roster
		// degrades to "no bot" rather than failing the whole fetch.
		agent, e = tx.Agents.GetForOrg(r.Context(), orgID)
		if e != nil {
			return e
		}
		if agent != nil {
			ta, e = tx.TeamAgents.GetForTeam(r.Context(), orgID, teamID, agent.ID)
		}
		return e
	}); err != nil {
		internalError(w, "team-members", err)
		return
	}

	out := make([]teamRosterRow, 0, len(members))
	for _, m := range members {
		out = append(out, teamRosterRow{
			UserID:         m.UserID,
			DisplayName:    m.DisplayName,
			GitHubUsername: m.GitHubUsername,
			JiraAccountID:  m.JiraAccountID,
			Role:           m.Role,
			IsCurrentUser:  m.UserID == userID,
		})
	}

	writeJSON(w, http.StatusOK, teamRosterResponse{Members: out, Bot: teamBotFromAgent(agent, ta)})
}

// handleTeamMemberAdd enrolls an existing org member on the team with a role.
// Team-admin-or-org-admin only (404 otherwise, non-disclosure). The target must
// already be an org member — the picker only offers org members, so a non-member
// is the defensive 422 rather than minting a team membership for someone with no
// org access.
//
// POST /api/teams/{team_id}/members  body: { "user_id": "<uuid>", "role": "<membership_role>" }
func (h *teamMembersHandler) handleTeamMemberAdd(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		http.NotFound(w, r)
		return
	}
	orgID, userID, teamID, ok := h.resolve(w, r)
	if !ok {
		return
	}
	if !h.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
		return
	}
	if !h.requireTeamManager(w, r, orgID, userID, teamID) {
		return
	}

	var body struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}
	target := strings.TrimSpace(body.UserID)
	if _, err := uuid.Parse(target); err != nil {
		badRequest(w, "user_id must be a valid user id")
		return
	}
	role := strings.TrimSpace(body.Role)
	if !teamRoles[role] {
		badRequest(w, "role must be one of admin, member, viewer")
		return
	}

	// Defensive backstop: the target must already be an org member (else 422,
	// matching the transfer-owner handler's non-member posture). RLS gates the
	// caller's privilege but not the target's org access.
	isMember, err := h.az.UserHasOrgAccess(r.Context(), target, orgID)
	if err != nil {
		internalError(w, "team-members", err)
		return
	}
	if !isMember {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "the user must be a member of this org",
		})
		return
	}

	err = h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.Teams.AddMember(r.Context(), teamID, target, role); err != nil {
			return err
		}
		// TFAC-471: audit the team add in the same tx (rolls back with it).
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID:  userID,
			Action:       domain.AccessActionTeamMemberAdded,
			TargetUserID: target,
			TeamID:       teamID,
			DetailJSON:   accessDetailNewRole(role),
		})
	})
	switch {
	case err == nil:
		w.WriteHeader(http.StatusCreated)
	case errors.Is(err, db.ErrTeamMemberExists):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "that user is already on this team",
		})
	default:
		internalError(w, "team-members", err)
	}
}

// handleTeamMemberRoleChange changes a member's team role. Team-admin-or-org-
// admin only (404 otherwise). Demoting the team's last admin is rejected by the
// guard trigger and surfaced as a 409 rather than a 500.
//
// PATCH /api/teams/{team_id}/members/{user_id}  body: { "role": "<membership_role>" }
func (h *teamMembersHandler) handleTeamMemberRoleChange(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		http.NotFound(w, r)
		return
	}
	orgID, userID, teamID, ok := h.resolve(w, r)
	if !ok {
		return
	}
	if !h.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
		return
	}
	if !h.requireTeamManager(w, r, orgID, userID, teamID) {
		return
	}
	target, ok := teamMemberTargetID(w, r)
	if !ok {
		return
	}

	var body struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}
	role := strings.TrimSpace(body.Role)
	if !teamRoles[role] {
		badRequest(w, "role must be one of admin, member, viewer")
		return
	}

	err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.Teams.ChangeMemberRole(r.Context(), teamID, target, role); err != nil {
			return err
		}
		// TFAC-471: audit the team role change in the same tx (rolls back with it).
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID:  userID,
			Action:       domain.AccessActionTeamRoleChanged,
			TargetUserID: target,
			TeamID:       teamID,
			DetailJSON:   accessDetailNewRole(role),
		})
	})
	if !writeTeamMemberMutationResult(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTeamMemberRemove removes a member from the team. A team/org admin may
// remove anyone; any member may remove themselves (a self-leave) — the split
// memberships_delete RLS already permits. Removing the team's last admin is
// rejected by the guard trigger and surfaced as a 409.
//
// DELETE /api/teams/{team_id}/members/{user_id}
func (h *teamMembersHandler) handleTeamMemberRemove(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		http.NotFound(w, r)
		return
	}
	orgID, userID, teamID, ok := h.resolve(w, r)
	if !ok {
		return
	}
	// An archived team is frozen — block roster writes (including self-leave):
	// the team is being torn down, and memberships_* RLS gates on
	// user_is_team_admin, which carries no archived filter (TFAC-448).
	if !h.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
		return
	}
	target, ok := teamMemberTargetID(w, r)
	if !ok {
		return
	}
	// Removing someone *else* requires manage rights; a self-leave does not.
	if target != userID {
		if !h.requireTeamManager(w, r, orgID, userID, teamID) {
			return
		}
	}

	err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.Teams.RemoveMember(r.Context(), teamID, target); err != nil {
			return err
		}
		// TFAC-471: audit the team removal in the same tx (rolls back with it).
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID:  userID,
			Action:       domain.AccessActionTeamMemberRemoved,
			TargetUserID: target,
			TeamID:       teamID,
		})
	})
	if !writeTeamMemberMutationResult(w, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolve runs the shared front gate for every team-roster endpoint: the active
// org + caller identity, a valid {team_id}, and the team-in-org check (404
// cross-org). Returns (orgID, userID, teamID, true) on success; writes the error
// and returns ok=false otherwise. Only reached in multi mode (each handler 404s
// local first).
func (h *teamMembersHandler) resolve(w http.ResponseWriter, r *http.Request) (orgID, userID, teamID string, ok bool) {
	orgID, ok = requireOrg(w, r)
	if !ok {
		return "", "", "", false
	}
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return "", "", "", false
	}
	userID = claims.Subject
	teamID = r.PathValue("team_id")
	if _, err := uuid.Parse(teamID); err != nil {
		http.NotFound(w, r)
		return "", "", "", false
	}
	if !h.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return "", "", "", false
	}
	return orgID, userID, teamID, true
}

// requireTeamManager confirms the caller may mutate the team's roster: a team
// admin OR an org admin (the union memberships_insert/_update/_delete RLS gate
// on). On not-allowed it writes 404 (non-disclosure, matching the org roster's
// posture) and returns false; an error renders a 500.
func (h *teamMembersHandler) requireTeamManager(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	isTeamAdmin, err := h.az.UserIsTeamAdmin(r.Context(), userID, orgID, teamID)
	if err != nil {
		internalError(w, "team-members", err)
		return false
	}
	if isTeamAdmin {
		return true
	}
	isOrgAdmin, err := h.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		internalError(w, "team-members", err)
		return false
	}
	if !isOrgAdmin {
		http.NotFound(w, r)
		return false
	}
	return true
}

// teamBotFromAgent renders the team's agent + per-team override row as the bot
// roster row. The org's agent supplies the defaults; the team_agents row (when
// present) overrides enabled + model + autonomy. A nil agent (un-bootstrapped
// org) yields no bot row; a nil team_agents row leaves the bot disabled for
// this team with the agent's default model/autonomy.
func teamBotFromAgent(agent *domain.Agent, ta *domain.TeamAgent) *teamRosterBotRow {
	if agent == nil {
		return nil
	}
	bot := &teamRosterBotRow{
		AgentID:     agent.ID,
		DisplayName: agent.DisplayName,
		Model:       agent.DefaultModel,
		Autonomy:    agent.DefaultAutonomySuitability,
	}
	if ta != nil {
		bot.Enabled = ta.Enabled
		if ta.PerTeamModel != "" {
			bot.Model = ta.PerTeamModel
		}
		if ta.PerTeamAutonomySuitability != nil {
			bot.Autonomy = ta.PerTeamAutonomySuitability
		}
	}
	return bot
}

// teamMemberTargetID extracts and validates the {user_id} path value. A
// malformed UUID is a 404 (same response as "no such member") so a bad path
// can't surface as a 500 from the uuid cast in the store query.
func teamMemberTargetID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := r.PathValue("user_id")
	if _, err := uuid.Parse(raw); err != nil {
		http.NotFound(w, r)
		return "", false
	}
	return raw, true
}

// writeTeamMemberMutationResult renders the shared error mapping for the
// PATCH/DELETE mutators and reports whether the caller should proceed to write
// its success status. The last-admin guard (db.ErrLastTeamAdminGuard) is a 409;
// a missing target (db.ErrTeamMemberNotFound) is a 404; anything else is a
// redacted 500. Returns true only on nil err.
func writeTeamMemberMutationResult(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, db.ErrLastTeamAdminGuard):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "can't remove or demote the last admin — assign another admin first",
		})
	case errors.Is(err, db.ErrTeamMemberNotFound):
		notFound(w, "member")
	default:
		internalError(w, "team-members", err)
	}
	return false
}
