package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// --------------------------------------------------------------------
// /api/settings/team/{team_id}/repos — the per-team GitHub repo
// *tracking* selection (the source of truth that repo_profiles is the
// org-wide UNION of). GET (any team member) returns the team's tracked
// repo slugs; PUT (team admin) replace-sets them and reconciles
// repo_profiles. The {team_id} segment accepts a UUID or the literal
// "default", same as the sibling team-settings routes. The candidate
// list (the repos the user's token can see) is fetched separately via
// GET /api/github/repos, mirroring how the existing repo picker sources
// its options.
// --------------------------------------------------------------------

type teamReposResponse struct {
	// Repos are the team's tracked repos as "owner/repo" slugs.
	Repos []string `json:"repos"`
	// Role is the caller's role in this team ("admin"/"member"/""), so
	// the picker can disable Save for non-admins without a second call.
	Role string `json:"role"`
}

func (s *Server) handleTeamReposGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.resolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		writeResolveError(w, "settings/team/repos", err)
		return
	}
	if !s.verifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}

	_, role, err := s.teamMemberCountAndRole(r.Context(), orgID, userID, teamID)
	if err != nil {
		internalError(w, "settings/team/repos", err)
		return
	}

	var repos []domain.TeamGitHubRepo
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		repos, e = tx.TeamGitHubRepos.ListForTeam(r.Context(), teamID)
		return e
	}); err != nil {
		internalError(w, "settings/team/repos", err)
		return
	}

	writeJSON(w, http.StatusOK, teamReposResponse{
		Repos: repoSlugs(repos),
		Role:  role,
	})
}

func (s *Server) handleTeamReposPut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.resolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		writeResolveError(w, "settings/team/repos", err)
		return
	}
	if !s.verifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	if !s.requireTeamAdmin(w, r, orgID, userID, teamID) {
		return
	}

	var req struct {
		Repos []string `json:"repos"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	// Split + validate up front so a malformed slug surfaces as a 400
	// rather than an opaque 500 from inside the tx. ReplaceForTeam
	// re-normalizes idempotently.
	repos, err := domain.TeamGitHubReposFromSlugs(req.Repos)
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.TeamGitHubRepos.ReplaceForTeam(r.Context(), teamID, repos)
	}); err != nil {
		internalError(w, "settings/team/repos", err)
		return
	}

	// Repo set changed → re-profile + restart pollers (the union the
	// poller reads from repo_profiles was just reconciled).
	if s.onGitHubChanged != nil {
		s.MarkJiraRestarted()
		go s.onGitHubChanged(orgID)
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "repos": len(repos)})
}

// repoSlugs renders tracked repos as "owner/repo" slugs for the wire.
func repoSlugs(repos []domain.TeamGitHubRepo) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.Slug())
	}
	return out
}
