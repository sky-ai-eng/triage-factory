package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// PUT /api/teams/{team_id}/jira-projects — the team's tracked Jira projects
// and the pickup / in-progress / in-review / done status rules for each.
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
//
// **The body names Jira objects by identifier, never by label.** A project
// arrives as its key and a status as its id, and both are checked against what
// the org's credential can actually see before anything is stored (see
// jira_project_gate.go). Status display names are not accepted at all: the
// server resolves them from the same fetch that validates the ids, so a stored
// name is always one Jira gave us and a caller cannot store a name that lies.
//
// An absent rule keeps the stored one. That is what makes the replace-set
// survivable for rules a client cannot express — the name-only rows written
// before statuses were identified, which have no ids to send — and it is the
// wire form of the same grandfathering the gate applies to keys.
// --------------------------------------------------------------------

// jiraPickupRuleWrite is the pickup rule's write shape: members and nothing
// else. A distinct type from the write-target rules rather than a shared one
// with a validated-empty canonical, so a caller that sends canonical_id here is
// refused by the strict decode with the field named — TF never transitions a
// ticket back into pickup, so there is no write target to name.
type jiraPickupRuleWrite struct {
	MemberIDs []string `json:"member_ids"`
}

// jiraStatusRuleWrite is a write-target rule's shape: the statuses that count
// as being in this state, and the one TF transitions a ticket into. Both are
// status IDS. An empty pair clears the rule, which for in_progress and done is
// a project watched but not armed, and for in_review — which arms nothing — is
// simply a team that does not name a review status.
type jiraStatusRuleWrite struct {
	MemberIDs   []string `json:"member_ids"`
	CanonicalID string   `json:"canonical_id"`
}

// jiraProjectWrite is one project in the desired set. The four rules are
// pointers because absent and empty are different requests: absent keeps the
// rule as stored, an explicit empty clears it.
type jiraProjectWrite struct {
	Key        string               `json:"key"`
	Pickup     *jiraPickupRuleWrite `json:"pickup"`
	InProgress *jiraStatusRuleWrite `json:"in_progress"`
	InReview   *jiraStatusRuleWrite `json:"in_review"`
	Done       *jiraStatusRuleWrite `json:"done"`
}

// teamJiraProjectsRequest is the desired set. Order is significant and
// preserved — the settings UI keeps projects in the order they were added, and
// the GET hands them back that way.
type teamJiraProjectsRequest struct {
	JiraProjects []jiraProjectWrite `json:"jira_projects"`
}

// teamJiraProjectsResponse echoes the set as stored, so a client renders the
// canonical form (keys uppercased, status names as Jira spells them today,
// empty rule members normalized to []) rather than what it happened to send.
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

	// Validate the shape of the whole set before any store work, accumulating
	// every bad row so a caller fixing three projects learns about three.
	var v httpx.Validation
	wishes := make([]jiraProjectWish, 0, len(req.JiraProjects))
	seen := map[string]bool{}
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
		wishes = append(wishes, jiraProjectWish{index: i, key: key, write: p})
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// The pre-image: what this team has stored right now. It is what an absent
	// rule is carried from, what the gates grandfather, and what the poller
	// restart decision is compared against.
	prev, err := s.storedJiraProjects(r.Context(), orgID, userID, teamID)
	if err != nil {
		internalError(w, "settings/team/jira-projects", err)
		return
	}

	// Everything the body claims about Jira is checked here, against Jira:
	// project keys being added must exist, and status ids in a rule that
	// changed must be in that project's workflow — the same fetch resolving
	// each id to its display name.
	next, ok := s.gateJiraProjects(w, r, orgID, userID, wishes, prev)
	if !ok {
		return
	}

	// The gate answers one row per wish, in order, or it has already written a
	// 400 — so the body position each row came from is its own index, which is
	// what the fault names.
	for i, p := range next {
		if err := validateProjectRules(p); err != nil {
			v.Invalid(fmt.Sprintf("jira_projects[%d]", wishes[i].index), err.Error())
		}
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// team_settings.jira_projects holds the display ORDER; jira_project_status_
	// rules holds the rules themselves. Both are written in one transaction, so
	// the order can never name a project whose rules didn't land.
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamSet, err := tx.Teams.GetSettings(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("load team settings: %w", err)
		}
		teamSet.JiraProjects = projectKeysFromConfigs(next)
		if _, err := tx.Teams.UpdateSettings(r.Context(), teamID, teamSet); err != nil {
			return fmt.Errorf("save team settings: %w", err)
		}
		if err := tx.JiraStatusRules.ReplaceForTeam(r.Context(), teamID, next); err != nil {
			return fmt.Errorf("save jira rules: %w", err)
		}
		return nil
	}); err != nil {
		internalError(w, "settings/team/jira-projects", err)
		return
	}

	// Re-due Jira polling only when the ARMED configuration moved. Re-PUTting
	// an identical set is a no-op, unlike the repos sibling where it doubles as
	// the re-profile trigger — and so is watching a project without mapping it,
	// because an unarmed project contributes nothing to the discovery JQL and a
	// restart would re-due a poll with nothing new to ask. Mapping in_review is
	// the same nothing for the same reason: it reaches neither the discovery
	// JQL nor any classification, so jiraProjectsEqual deliberately does not
	// compare it.
	if !jiraProjectsEqual(armedJiraProjects(next), armedJiraProjects(prev)) && s.onJiraChanged != nil {
		s.MarkJiraRestarted(r.Context(), orgID)
		go s.onJiraChanged(orgID)
	}

	writeJSON(w, http.StatusOK, teamJiraProjectsResponse{JiraProjects: toJiraProjectSettings(next)})
}

// jiraProjectWish is one validated body element: the canonical key plus the
// rules as the caller expressed them, still unresolved.
type jiraProjectWish struct {
	// index is the element's position in the request body, which is what the
	// error fields name — a caller reads its own payload, not the stored order.
	index int
	key   string
	write jiraProjectWrite
}

// storedJiraProjects reads the team's rules in their display order — the same
// shape and order the GET renders, so "already stored" means exactly what a
// client that just read the resource would resend.
func (s *Server) storedJiraProjects(ctx context.Context, orgID, userID, teamID string) ([]domain.JiraProjectStatusRules, error) {
	var out []domain.JiraProjectStatusRules
	err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		teamSet, err := tx.Teams.GetSettings(ctx, teamID)
		if err != nil {
			return fmt.Errorf("load team settings: %w", err)
		}
		rules, err := tx.JiraStatusRules.ListForTeam(ctx, teamID)
		if err != nil {
			return fmt.Errorf("load jira rules: %w", err)
		}
		out = cloneJiraProjects(rulesToProjectConfigsOrdered(rules, teamSet.JiraProjects))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
