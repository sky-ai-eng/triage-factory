package server

import (
	"fmt"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// PUT /api/teams/{team_id}/jira-projects — the team's tracked Jira projects
// and the pickup / in-progress / done status rules for each.
//
// A replace-set write, exactly like the tracked repos and the GitHub-team
// mappings: the body IS the desired set, and a key absent from it is untracked.
// It used to be a key inside the team-settings save, which made a child
// collection a field — so a settings save that touched the model also rewrote
// the routing rules, and the two had one shared failure mode and one shared
// poller restart. Its siblings already had their own routes; this is the third.
//
// Team admin, non-archived team — the same gate as the settings PATCH, because
// this is the same blast radius: the rules decide which tickets become tasks.
// --------------------------------------------------------------------

// teamJiraProjectsRequest is the desired set. Order is significant and
// preserved — the settings UI keeps projects in the order they were added, and
// the GET hands them back that way.
type teamJiraProjectsRequest struct {
	JiraProjects []jiraProjectSettings `json:"jira_projects"`
}

// teamJiraProjectsResponse echoes the set as stored, so a client renders the
// canonical form (keys uppercased, empty rule members normalized to []) rather
// than what it happened to send.
type teamJiraProjectsResponse struct {
	JiraProjects []jiraProjectSettings `json:"jira_projects"`
}

func (s *Server) handleTeamJiraProjectsPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, ok := s.az.TeamIDFromPath(w, r, "settings/team/jira-projects", orgID, userID)
	if !ok {
		return
	}
	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	if !s.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
		return
	}
	if !s.az.RequireTeamAdmin(w, r, orgID, userID, teamID) {
		return
	}

	var req teamJiraProjectsRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	// Validate the whole set before any store work, accumulating every bad row
	// so a caller fixing three projects learns about three.
	var v httpx.Validation
	seen := map[string]bool{}
	next := make([]jiraProjectConfig, 0, len(req.JiraProjects))
	for i, p := range req.JiraProjects {
		field := fmt.Sprintf("jira_projects[%d].key", i)
		key := normalizeJiraProjectKey(p.Key)
		switch {
		case key == "":
			v.Missing(field)
			continue
		case !jiraProjectKeyRe.MatchString(key):
			v.Invalid(field, "invalid Jira project key "+key)
			continue
		case seen[key]:
			v.Invalid(field, "duplicate project key "+key)
			continue
		}
		seen[key] = true
		normalized := jiraProjectConfig{Key: key, Pickup: p.Pickup, InProgress: p.InProgress, Done: p.Done}
		if err := validateProjectRules(normalized); err != nil {
			v.Invalid(fmt.Sprintf("jira_projects[%d]", i), err.Error())
			continue
		}
		next = append(next, normalized)
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// team_settings.jira_projects holds the display ORDER; jira_project_status_
	// rules holds the rules themselves. Both are written in one transaction, so
	// the order can never name a project whose rules didn't land.
	var prev []jiraProjectConfig
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamSet, err := tx.Teams.GetSettings(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("load team settings: %w", err)
		}
		rules, err := tx.JiraStatusRules.ListForTeam(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("load jira rules: %w", err)
		}
		prev = cloneJiraProjects(rulesToProjectConfigsOrdered(rules, teamSet.JiraProjects))

		teamSet.JiraProjects = projectKeysFromConfigs(next)
		if _, err := tx.Teams.UpdateSettings(r.Context(), teamID, teamSet); err != nil {
			return fmt.Errorf("save team settings: %w", err)
		}
		if err := tx.JiraStatusRules.ReplaceForTeam(r.Context(), teamID, projectConfigsToRules(next)); err != nil {
			return fmt.Errorf("save jira rules: %w", err)
		}
		return nil
	}); err != nil {
		internalError(w, "settings/team/jira-projects", err)
		return
	}

	// Re-due Jira polling only when the set actually moved. Re-PUTting an
	// identical set is a no-op, unlike the repos sibling where it doubles as the
	// re-profile trigger.
	if !jiraProjectsEqual(next, prev) && s.onJiraChanged != nil {
		s.MarkJiraRestarted(r.Context(), orgID)
		go s.onJiraChanged(orgID)
	}

	writeJSON(w, http.StatusOK, teamJiraProjectsResponse{JiraProjects: toJiraProjectSettings(next)})
}
