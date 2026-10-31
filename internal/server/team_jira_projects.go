package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// PUT /api/teams/{team_id}/jira-projects — the team's tracked Jira projects
// and the pickup / in-progress / in-review / done status rules for each.
//
// A replace-set write, exactly like the tracked repos and the GitHub-team
// mappings: the body IS the desired set, and a project absent from it is
// untracked. It is a route of its own rather than a field of the team-settings
// save, as its siblings are, so a settings save that touches the model does not
// rewrite the routing rules, and the two share no failure mode and no poller
// restart.
//
// Team admin, non-archived team — the same gate as the settings PATCH, because
// this is the same blast radius: the rules decide which tickets become tasks.
//
// **The body names Jira objects by identifier, never by label.** A project the
// team tracks is named by its jira_projects registry row id (the id the GET
// and this route's response serve on each entry), and a project being added by
// Jira's own project id (the jira_id the org's catalog serves on each
// candidate). A key names neither: a project's key changes when it is renamed
// in Jira, and a key the poll has not caught up with would name a stored
// project as a new one. A status is named by its id. Every identifier is
// checked against what the org's credential can actually see before anything
// is stored (see jira_project_gate.go). Status display names are not accepted
// at all: the server resolves them from the same fetch that validates the ids,
// so a stored name is always one Jira gave us and a caller cannot store a name
// that lies.
//
// Each element carries exactly one of the two identifiers, and they do not
// select different behaviour: an element is one project in the desired set,
// with the same rule fields whichever way it is named. They differ because the
// project is in a different state. A project not yet tracked may have no
// registry row, and a tracked one may have no Jira id yet (a row the poll has
// not resolved against Jira), so neither identifier names every project. One
// project has one address: a tracked project is refused under its Jira id.
//
// A project added here is registered under the id Jira answers with, so a
// later key rename moves the registry row's key and leaves this team's set
// intact.
//
// An absent rule keeps the stored one. That is what makes the replace-set
// survivable for rules a client cannot express — the name-only rows written
// before statuses were identified, which have no ids to send — and it is the
// wire form of the same grandfathering the gate applies to a stored project.
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

// jiraProjectWrite is one project in the desired set, named by exactly one of
// ID and JiraID. The four rules are pointers because absent and empty are
// different requests: absent keeps the rule as stored, an explicit empty
// clears it.
type jiraProjectWrite struct {
	// ID is the registry row id of a project this team tracks.
	ID string `json:"id"`
	// JiraID is Jira's project id of a project being added.
	JiraID     string               `json:"jira_id"`
	Pickup     *jiraPickupRuleWrite `json:"pickup"`
	InProgress *jiraStatusRuleWrite `json:"in_progress"`
	InReview   *jiraStatusRuleWrite `json:"in_review"`
	Done       *jiraStatusRuleWrite `json:"done"`
}

// jiraProjectIDRe is a Jira project id as both deployments write one: a
// decimal integer, carried as a string. It is checked before Jira is asked, so
// a value that cannot be a project id costs no round trip.
var jiraProjectIDRe = regexp.MustCompile(`^[0-9]+$`)

// teamJiraProjectsRequest is the desired set. Order is significant and
// preserved — the settings UI keeps projects in the order they were added, and
// the GET hands them back that way.
type teamJiraProjectsRequest struct {
	JiraProjects []jiraProjectWrite `json:"jira_projects"`
}

// teamJiraProjectsResponse echoes the set as stored, so a client renders the
// canonical form (each project under its registry id, key and name as Jira
// spells them today, status names likewise, empty rule members normalized to
// []) rather than what it happened to send.
type teamJiraProjectsResponse struct {
	JiraProjects []jiraProjectSettings `json:"jira_projects"`
}

// errJiraProjectNamedTwice is the write's refusal of an added project that
// registering resolved onto a project already in the set: a tracked row the
// poll had not resolved, under the key Jira gives the added one. The caller
// could not have seen it, since the tracked row carried no Jira id.
type errJiraProjectNamedTwice struct {
	index, other int
	key          string
}

func (e errJiraProjectNamedTwice) Error() string {
	return fmt.Sprintf("jira_projects[%d] registers as the project at jira_projects[%d]", e.index, e.other)
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
	// every bad element so a caller fixing three projects learns about three.
	var v httpx.Validation
	seenIDs, seenJiraIDs := map[string]int{}, map[string]int{}
	for i, p := range req.JiraProjects {
		base := fmt.Sprintf("jira_projects[%d]", i)
		switch {
		case p.ID == "" && p.JiraID == "":
			v.Add(httpx.ErrorItem{
				Reason:  httpx.ReasonMissingField,
				Message: base + " names no project: send id for a project this team tracks, or jira_id for a project to add",
				Field:   base,
			})
		case p.ID != "" && p.JiraID != "":
			v.Invalid(base, base+" carries both id and jira_id; a project is named by exactly one of them")
		case p.ID != "":
			if j, dup := seenIDs[p.ID]; dup {
				v.Invalid(base+".id", fmt.Sprintf("project %s is already in this set at jira_projects[%d]", p.ID, j))
				continue
			}
			seenIDs[p.ID] = i
		default:
			if !jiraProjectIDRe.MatchString(p.JiraID) {
				v.Invalid(base+".jira_id", "jira_id "+p.JiraID+" is not a Jira project id")
				continue
			}
			if j, dup := seenJiraIDs[p.JiraID]; dup {
				v.Invalid(base+".jira_id", fmt.Sprintf("Jira project %s is already in this set at jira_projects[%d]", p.JiraID, j))
				continue
			}
			seenJiraIDs[p.JiraID] = i
		}
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// The pre-image: what this team has stored right now. It is what an absent
	// rule is carried from, what the gates grandfather, and what the poller
	// restart decision is compared against.
	// TODO(TFAC-1271): it is read outside the write transaction, so a write
	// that commits between this read and that transaction is reverted by the
	// rules carried from it, and neither caller is told.
	prev, site, err := s.storedJiraProjects(r.Context(), orgID, userID, teamID)
	if err != nil {
		internalError(w, "settings/team/jira-projects", err)
		return
	}

	// Each element against the stored set: an id must be a project this team
	// tracks, and a jira_id must not be one, so every element is either one
	// stored project or one project being added.
	wishes, ok := resolveJiraProjectWishes(&v, req.JiraProjects, prev)
	if !ok {
		v.Flush(w, http.StatusBadRequest)
		return
	}

	// Everything the body claims about Jira is checked here, against Jira:
	// projects being added must exist, and status ids in a rule that changed
	// must be in that project's workflow — the same fetch resolving each id to
	// its display name.
	next, ok := s.gateJiraProjects(w, r, orgID, site, wishes)
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
	// rules holds the rules themselves. Both are written in one transaction with
	// the registry rows of the projects being added, so the order can never name
	// a project whose rules didn't land, and a rule never names a project that
	// has no registry row.
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		at := make(map[string]int, len(next))
		for i := range next {
			if next[i].JiraProjectID != "" {
				at[next[i].JiraProjectID] = i
			}
		}
		for i := range next {
			if next[i].JiraProjectID != "" {
				continue
			}
			project, err := tx.JiraProjects.FindOrCreate(r.Context(), orgID, site,
				next[i].ProjectExternalID, next[i].ProjectKey, next[i].ProjectName)
			if err != nil {
				return fmt.Errorf("register jira project %s: %w", next[i].ProjectKey, err)
			}
			if j, dup := at[project.ID]; dup {
				return errJiraProjectNamedTwice{index: wishes[i].index, other: wishes[j].index, key: next[i].ProjectKey}
			}
			at[project.ID] = i
			next[i].JiraProjectID = project.ID
		}
		teamSet, err := tx.Teams.GetSettings(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("load team settings: %w", err)
		}
		teamSet.JiraProjects = jiraProjectIDsFromConfigs(next)
		if _, err := tx.Teams.UpdateSettings(r.Context(), teamID, teamSet); err != nil {
			return fmt.Errorf("save team settings: %w", err)
		}
		// With no Jira site there is nothing stored to replace: every read
		// confines itself to the site, and a set naming a project never gets
		// past the gate without one.
		if site == "" {
			return nil
		}
		if err := tx.JiraStatusRules.ReplaceForTeam(r.Context(), teamID, site, next); err != nil {
			return fmt.Errorf("save jira rules: %w", err)
		}
		return nil
	}); err != nil {
		var twice errJiraProjectNamedTwice
		if errors.As(err, &twice) {
			v.Invalid(fmt.Sprintf("jira_projects[%d].jira_id", twice.index),
				fmt.Sprintf("Jira project %s is the project already in this set at jira_projects[%d]", twice.key, twice.other))
			v.Flush(w, http.StatusBadRequest)
			return
		}
		// Every project's row was read on this site in this org above, so a
		// row the rules write cannot place moved since: the poll merged it
		// into the row its Jira id names. The caller's set is stale, not wrong.
		if errors.Is(err, db.ErrJiraRuleProjectNotOnSite) {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
				Reason:  httpx.ReasonConflict,
				Message: "a tracked Jira project changed while saving; read the team's projects again and retry",
			})
			return
		}
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

// jiraProjectWish is one validated body element: the project it names, as far
// as the stored set knows it, plus the rules as the caller expressed them,
// still unresolved.
type jiraProjectWish struct {
	// index is the element's position in the request body, which is what the
	// error fields name — a caller reads its own payload, not the stored order.
	index int
	// stored is the team's stored row for a project named by its registry id;
	// nil for a project being added, which jiraID names.
	stored *domain.JiraProjectStatusRules
	jiraID string
	write  jiraProjectWrite
}

// resolveJiraProjectWishes places each shape-checked element against the
// team's stored set: an id names one of the stored projects, and a jira_id
// names a project that is not one of them. Faults are recorded on v, every
// one of them; ok is false when there is any.
func resolveJiraProjectWishes(v *httpx.Validation, writes []jiraProjectWrite, stored []domain.JiraProjectStatusRules) ([]jiraProjectWish, bool) {
	storedByID := make(map[string]*domain.JiraProjectStatusRules, len(stored))
	storedByJiraID := make(map[string]*domain.JiraProjectStatusRules, len(stored))
	for i := range stored {
		storedByID[stored[i].JiraProjectID] = &stored[i]
		if stored[i].ProjectExternalID != "" {
			storedByJiraID[stored[i].ProjectExternalID] = &stored[i]
		}
	}
	wishes := make([]jiraProjectWish, 0, len(writes))
	faulted := false
	for i, p := range writes {
		base := fmt.Sprintf("jira_projects[%d]", i)
		if p.ID != "" {
			prior, ok := storedByID[p.ID]
			if !ok {
				v.Invalid(base+".id", p.ID+" is not a Jira project this team tracks; a project being added is named by its jira_id")
				faulted = true
				continue
			}
			wishes = append(wishes, jiraProjectWish{index: i, stored: prior, write: p})
			continue
		}
		if prior, tracked := storedByJiraID[p.JiraID]; tracked {
			v.Invalid(base+".jira_id", fmt.Sprintf("Jira project %s is tracked by this team as %s; name it by its id %s", p.JiraID, prior.ProjectKey, prior.JiraProjectID))
			faulted = true
			continue
		}
		wishes = append(wishes, jiraProjectWish{index: i, jiraID: p.JiraID, write: p})
	}
	return wishes, !faulted
}

// storedJiraProjects reads the team's rules on the org's Jira site in their
// display order — the same shape and order the GET renders, so "already
// stored" means exactly what a client that just read the resource would
// resend — and the site, which the write saves under.
func (s *Server) storedJiraProjects(ctx context.Context, orgID, userID, teamID string) ([]domain.JiraProjectStatusRules, string, error) {
	var (
		out  []domain.JiraProjectStatusRules
		site string
	)
	err := s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		orgSet, err := tx.Orgs.GetSettings(ctx, orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		site = domain.EntityScope("jira", orgSet)
		teamSet, err := tx.Teams.GetSettings(ctx, teamID)
		if err != nil {
			return fmt.Errorf("load team settings: %w", err)
		}
		rules, err := tx.JiraStatusRules.ListForTeam(ctx, teamID, site)
		if err != nil {
			return fmt.Errorf("load jira rules: %w", err)
		}
		out = cloneJiraProjects(rulesToProjectConfigsOrdered(rules, teamSet.JiraProjects))
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, site, nil
}
