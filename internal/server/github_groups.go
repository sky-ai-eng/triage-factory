package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// --------------------------------------------------------------------
// /api/settings/team/{team_id}/github-groups — the GitHub twin of the
// per-team Jira project rules. GET (any team member) returns the team's
// current GitHub-team mappings plus the org's live GitHub teams as
// import-and-choose candidates; PUT (team admin) replace-sets the
// mappings. The {team_id} segment accepts a UUID or the literal
// "default", same as the sibling team-settings routes.
// --------------------------------------------------------------------

// gitHubGroupJSON is the wire shape for one stored mapping row.
type gitHubGroupJSON struct {
	OrgLogin string `json:"org_login"`
	TeamSlug string `json:"team_slug"`
}

// gitHubGroupCandidateJSON is one live GitHub team the admin can assign
// to this TF team. Name rides along for display only — TF stores slugs,
// never names or membership.
type gitHubGroupCandidateJSON struct {
	OrgLogin string `json:"org_login"`
	TeamSlug string `json:"team_slug"`
	Name     string `json:"name"`
}

type teamGitHubGroupsResponse struct {
	Groups     []gitHubGroupJSON          `json:"groups"`
	Candidates []gitHubGroupCandidateJSON `json:"candidates"`
	// Role is the caller's role in this team ("admin"/"member"/""), so
	// the editor can disable Save for non-admins without a second call.
	Role string `json:"role"`
}

func (s *Server) handleTeamGitHubGroupsGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.resolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		writeResolveError(w, "settings/team/github-groups", err)
		return
	}
	if !s.verifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}

	// Import-and-choose: fetch the org's live GitHub teams as candidates
	// (read-only). The deletion-reconcile prune runs in the GitHub poll
	// cycle, not here, so this GET never mutates — stale mappings just
	// show flagged "not found" until the next cycle prunes them.
	candidates := s.gitHubGroupCandidates(r.Context(), orgID, userID)

	var groups []domain.TeamGitHubGroup
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		groups, e = tx.TeamGitHubGroups.ListForTeam(r.Context(), teamID)
		return e
	}); err != nil {
		internalError(w, "settings/team/github-groups", err)
		return
	}

	_, role, err := s.teamMemberCountAndRole(r.Context(), orgID, userID, teamID)
	if err != nil {
		internalError(w, "settings/team/github-groups", err)
		return
	}

	resp := teamGitHubGroupsResponse{
		Groups:     toGitHubGroupJSON(groups),
		Candidates: candidates,
		Role:       role,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleTeamGitHubGroupsPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.resolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		writeResolveError(w, "settings/team/github-groups", err)
		return
	}
	if !s.verifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	if !s.requireTeamAdmin(w, r, orgID, userID, teamID) {
		return
	}

	var req struct {
		Groups []gitHubGroupJSON `json:"groups"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	groups := make([]domain.TeamGitHubGroup, 0, len(req.Groups))
	for _, g := range req.Groups {
		groups = append(groups, domain.TeamGitHubGroup{OrgLogin: g.OrgLogin, TeamSlug: g.TeamSlug})
	}
	// Validate up front so a half-specified group surfaces as a 400 rather
	// than an opaque 500 from inside the tx. SetForTeam re-normalizes
	// idempotently.
	if _, err := domain.NormalizeTeamGitHubGroups(groups); err != nil {
		badRequest(w, err.Error())
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.TeamGitHubGroups.SetForTeam(r.Context(), teamID, groups)
	}); err != nil {
		internalError(w, "settings/team/github-groups", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// gitHubGroupCandidates fetches the org's live GitHub teams (one
// GET /orgs/{org}/teams per distinct configured-repo owner) and returns
// them as the flat candidate list the editor presents. Read-only — the
// deletion-reconcile prune runs in the GitHub poll cycle
// (poller.Manager.reconcileGitHubGroups), not here, so this GET stays a
// pure read. Stale mappings (a GitHub team deleted since the last poll)
// still surface in the editor flagged "not found" until the next cycle
// prunes them, and an admin can remove one immediately via the PUT.
//
// Credentials resolve through s.ghResolver.ClientFor, which mints an App
// installation token for the owner when one is installed (tier 1) and
// falls back to the org PAT (tier 3) — so App-only workspaces (no PAT)
// can still import their teams. Owners with no resolvable credential, or
// that the token can't read (a user account, a private org the token
// isn't in), are skipped. Returns nil when nothing is configured / the
// repo list can't be read — the editor degrades to "edit existing
// mappings only."
func (s *Server) gitHubGroupCandidates(ctx context.Context, orgID, userID string) []gitHubGroupCandidateJSON {
	var repos []domain.RepoProfile
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		repos, e = tx.Repos.List(ctx, orgID)
		return e
	}); err != nil {
		log.Printf("[github-groups] load configured repos: %v", err)
		return nil
	}

	out := []gitHubGroupCandidateJSON{}
	for _, owner := range distinctRepoOwners(repos) {
		client, err := s.ghResolver.ClientFor(ctx, orgID, owner)
		if err != nil {
			// ErrNoGitHubCredentials just means this owner has no App
			// install and the org has no PAT — expected, skip quietly.
			if !errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				log.Printf("[github-groups] resolve GitHub client for %s: %v", owner, err)
			}
			continue
		}
		teams, err := client.ListOrgTeams(owner)
		if err != nil {
			// The owner may be a user account (no teams) or one the org
			// credential can't read. Skip — candidates only.
			log.Printf("[github-groups] list teams for GitHub org %s: %v", owner, err)
			continue
		}
		login := strings.ToLower(owner)
		for _, t := range teams {
			if t.Slug == "" {
				continue
			}
			out = append(out, gitHubGroupCandidateJSON{
				OrgLogin: login,
				TeamSlug: strings.ToLower(t.Slug),
				Name:     t.Name,
			})
		}
	}
	return out
}

// distinctRepoOwners returns the configured repos' owners, lowercased,
// de-duplicated, in first-seen order. These are the GitHub orgs whose
// teams the editor offers as candidates.
func distinctRepoOwners(repos []domain.RepoProfile) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, p := range repos {
		owner := strings.ToLower(strings.TrimSpace(p.Owner))
		if owner == "" || seen[owner] {
			continue
		}
		seen[owner] = true
		out = append(out, owner)
	}
	return out
}

func toGitHubGroupJSON(groups []domain.TeamGitHubGroup) []gitHubGroupJSON {
	out := make([]gitHubGroupJSON, 0, len(groups))
	for _, g := range groups {
		out = append(out, gitHubGroupJSON{OrgLogin: g.OrgLogin, TeamSlug: g.TeamSlug})
	}
	return out
}
