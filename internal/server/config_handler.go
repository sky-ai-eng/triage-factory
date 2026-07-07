package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// configResponse is the FE-facing shape exposed by GET /api/config.
//
// Single consumer + single purpose: AuthGate (D8) reads
// deployment_mode at boot to choose between the local keychain-capture
// flow and the multi-mode cookie auth flow. The call is unauthenticated
// — it has to work before login, hence the pre-auth allowlist mount in
// routes(). Per-user identity (github_username, jira_*) used to live
// here for the predicate editor; that data moved to /api/me, which now
// returns the same fields in both modes.
//
// Don't conflate with /api/settings (user-mutable preferences),
// /api/me (the caller's identity + org list), or /api/team/members
// (mutable team roster).
type configResponse struct {
	DeploymentMode string `json:"deployment_mode"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, configResponse{
		DeploymentMode: string(runmode.Current()),
	})
}

// handleHealth is the liveness probe target. Returns 200 once the
// server is accepting requests; platform restart logic uses this to
// decide when a Machine/container has come up. Deliberately does NOT
// reach into the DB or integrations — a flapping Postgres shouldn't
// recycle the whole TF process via the platform's auto-restart.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// teamMembersResponse is the roster shown to Variant B (multi-person team)
// of the predicate editor AND to the per-card assignee picker.
// Each row carries display_name + github_username + is_current_user so
// the FE can pre-rank the dropdown and highlight "you" among teammates.
//
// Bot is a sibling field rather than a member row because the bot is
// not a user — it occupies the agent claim slot, not the user claim
// slot. Frontend renders it as a distinct entry (right under "Me")
// when non-null. Null when the agent isn't bootstrapped or
// team_agents.enabled is false for the caller's team — same gate the
// delegate handlers enforce, surfaced to the picker so disabled bots
// don't appear as a (would-409) option.
type teamMembersResponse struct {
	Members []teamMemberRow `json:"members"`
	Bot     *teamBotRow     `json:"bot"`
}

type teamBotRow struct {
	AgentID     string `json:"agent_id"`
	DisplayName string `json:"display_name"`
}

type teamMemberRow struct {
	UserID         string  `json:"user_id"`
	DisplayName    string  `json:"display_name"`
	GitHubUsername *string `json:"github_username"` // null when member hasn't captured identity
	JiraAccountID  *string `json:"jira_account_id"` // null when member hasn't connected Jira
	IsCurrentUser  bool    `json:"is_current_user"`
}

func (s *Server) handleTeamMembers(w http.ResponseWriter, r *http.Request) {
	userID := ClaimsFrom(r.Context()).Subject
	orgID := OrgIDFrom(r.Context())

	// Multi mode: the caller's default team, sourced from the same
	// Teams.ListMembers the team-roster endpoint (GET
	// /api/teams/{team_id}/members) uses — one source of truth for the team
	// roster (TFAC-444). This is the predicate-editor / assignee-picker view
	// (roles dropped here; the picker only needs identity readiness + the bot),
	// scoped to the org's default team until a per-call team selector lands
	// (the page-shell slice). The roleful management view is the new endpoint.
	if runmode.Current() != runmode.ModeLocal {
		s.handleTeamMembersMulti(w, r, orgID, userID)
		return
	}

	// Local N=1: the synthetic single-member roster. There's exactly one
	// (implicit) user, so we render their identity directly rather than
	// querying memberships — the SQLite roster methods are multi-only stubs.
	var username, displayName, jiraAccount string
	_ = s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Identity is host-scoped: resolve the org's GitHub
		// host from org_settings, then look up the login for (user, host).
		orgSet, _ := tx.Orgs.GetSettings(r.Context(), orgID)
		username, _ = tx.Users.GetGitHubLogin(r.Context(), userID, orgSet.GitHubBaseURL)
		displayName, _ = tx.Users.GetDisplayName(r.Context(), userID)
		jiraAccount, _, _ = tx.Users.GetJiraIdentity(r.Context(), userID, orgSet.JiraBaseURL)
		return nil
	})
	var login, jiraID *string
	if username != "" {
		login = &username
	}
	if jiraAccount != "" {
		jiraID = &jiraAccount
	}

	// Bot resolution mirrors the swipe-delegate / factory-delegate
	// gate so the picker shows exactly the options the delegate
	// handlers would accept. Errors here are non-fatal — the picker
	// degrades to "no bot" rather than failing the roster fetch.
	var bot *teamBotRow
	if agent, enabled, err := s.agentEnabledForOrg(r.Context(), orgID, userID); err == nil && enabled && agent != nil {
		bot = &teamBotRow{
			AgentID:     agent.ID,
			DisplayName: agent.DisplayName,
		}
	}

	writeJSON(w, http.StatusOK, teamMembersResponse{
		Members: []teamMemberRow{
			{
				UserID:         userID,
				DisplayName:    displayName,
				GitHubUsername: login,
				JiraAccountID:  jiraID,
				IsCurrentUser:  true,
			},
		},
		Bot: bot,
	})
}

// handleTeamMembersMulti serves the multi-mode /api/team/members: the caller's
// default-team roster (identity readiness + the team bot), mapped to the
// roleless picker shape. It reads through Teams.ListMembers — the same store
// method the roleful GET /api/teams/{team_id}/members endpoint uses — so the
// two team-roster reads can't diverge. The team is the org's default for now
// (no per-call team param on this legacy picker route); the active-team
// refinement belongs to the page-shell slice.
func (s *Server) handleTeamMembersMulti(w http.ResponseWriter, r *http.Request, orgID, userID string) {
	var (
		members []domain.TeamMember
		teamID  string
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		teamID, e = tx.Teams.GetDefaultForOrg(r.Context(), orgID)
		if e != nil || teamID == "" {
			// Teamless org is a bootstrap bug; degrade to an empty roster
			// rather than erroring the predicate editor.
			return e
		}
		orgSet, e := tx.Orgs.GetSettings(r.Context(), orgID)
		if e != nil {
			return e
		}
		members, e = tx.Teams.ListMembers(r.Context(), teamID, orgSet.GitHubBaseURL, orgSet.JiraBaseURL)
		return e
	}); err != nil {
		internalError(w, "team-members", err)
		return
	}

	out := make([]teamMemberRow, 0, len(members))
	for _, m := range members {
		out = append(out, teamMemberRow{
			UserID:         m.UserID,
			DisplayName:    m.DisplayName,
			GitHubUsername: m.GitHubUsername,
			JiraAccountID:  m.JiraAccountID,
			IsCurrentUser:  m.UserID == userID,
		})
	}

	// Bot scoped to the same default team, mirroring the delegate gate so the
	// picker only offers a bot the delegate handlers would accept. Skip the
	// lookup entirely for a teamless org (the bootstrap-bug degraded state
	// above): agentEnabledForTeam("") already resolves to disabled, so the
	// guard just avoids a needless agent-lookup round-trip and makes the intent
	// explicit.
	var bot *teamBotRow
	if teamID != "" {
		if agent, enabled, err := s.agentEnabledForTeam(r.Context(), orgID, userID, teamID); err == nil && enabled && agent != nil {
			bot = &teamBotRow{AgentID: agent.ID, DisplayName: agent.DisplayName}
		}
	}

	writeJSON(w, http.StatusOK, teamMembersResponse{Members: out, Bot: bot})
}
