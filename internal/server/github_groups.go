package server

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
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

	// Import-and-choose: fetch the org's live GitHub teams and reconcile
	// deleted ones out of the mapping table before reading current rows,
	// so the editor never shows or persists a mapping to a team GitHub
	// no longer reports. Best-effort — a GitHub outage leaves the editor
	// working off the stored mappings with an empty candidate list.
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
// GET /orgs/{org}/teams per distinct configured-repo owner) and, for
// each org login it successfully reads, prunes mapping rows pointing at
// GitHub teams that no longer exist — the deletion-reconcile floor.
// Returns the flat candidate list. Never prunes an org login whose fetch
// failed (a transient error must not be read as "all its teams were
// deleted"). Returns nil (empty candidates) when GitHub isn't configured
// or context loading fails — the editor degrades to "edit existing
// mappings only."
func (s *Server) gitHubGroupCandidates(ctx context.Context, orgID, userID string) []gitHubGroupCandidateJSON {
	var (
		creds  auth.Credentials
		orgSet domain.OrgSettings
		repos  []domain.RepoProfile
	)
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		creds, _ = integrations.Load(ctx, tx.Secrets, orgID)
		orgSet, _ = tx.Orgs.GetSettings(ctx, orgID)
		var e error
		repos, e = tx.Repos.List(ctx, orgID)
		return e
	}); err != nil {
		log.Printf("[github-groups] load candidate context: %v", err)
		return nil
	}
	if creds.GitHubPAT == "" || creds.GitHubURL == "" {
		return nil
	}
	baseURL := orgSet.GitHubBaseURL
	if baseURL == "" {
		baseURL = creds.GitHubURL
	}
	client := ghclient.NewClient(baseURL, creds.GitHubPAT)

	out := []gitHubGroupCandidateJSON{}
	for _, owner := range distinctRepoOwners(repos) {
		teams, err := client.ListOrgTeams(owner)
		if err != nil {
			// Skip — the owner may be a user account (no teams) or one
			// the token can't read. Crucially, do NOT prune its mappings.
			log.Printf("[github-groups] list teams for GitHub org %s: %v", owner, err)
			continue
		}
		login := strings.ToLower(owner)
		slugs := make([]string, 0, len(teams))
		for _, t := range teams {
			if t.Slug == "" {
				continue
			}
			slugs = append(slugs, t.Slug)
			out = append(out, gitHubGroupCandidateJSON{
				OrgLogin: login,
				TeamSlug: strings.ToLower(t.Slug),
				Name:     t.Name,
			})
		}
		if n, err := s.githubGroups.PruneMissingSystem(ctx, orgID, owner, slugs); err != nil {
			log.Printf("[github-groups] reconcile prune for GitHub org %s: %v", owner, err)
		} else if n > 0 {
			log.Printf("[github-groups] reconcile: pruned %d stale mapping(s) for deleted GitHub teams under %s", n, owner)
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
