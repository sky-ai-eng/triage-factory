package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
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
	if slug, reject := s.rejectUnreachableRepo(r.Context(), orgID, userID, repos); reject {
		badRequest(w, "repo "+slug+" is not reachable by this org's GitHub credentials — install the GitHub App on it or grant the PAT access first")
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

// rejectUnreachableRepo is the write-time guard that keeps a team from
// tracking a repo this deployment's GitHub credentials can't reach (a
// typo'd slug, a stale client, a hand-crafted curl). It enumerates the
// reachable set and returns the first input repo absent from it.
//
// It is deliberately fail-OPEN on the enumeration itself: if we can't
// build a complete, authoritative view (no credentials, a GitHub outage,
// a partial fetch), it returns reject=false and the write proceeds. That
// is not laxness — a user can't induce a GitHub outage, so every garbage
// input submittable under normal conditions is still hard-rejected,
// while we avoid coupling our write availability to GitHub's uptime. The
// residual (a write accepted while GitHub was unreachable) is caught by
// the poller, which no-ops on a repo it can't reach. When the reachable
// set is later persisted (candidate-sourcing work), swap the source in
// reachableRepoSet for a local read and this can go fully fail-closed.
func (s *Server) rejectUnreachableRepo(ctx context.Context, orgID, userID string, repos []domain.TeamGitHubRepo) (string, bool) {
	reachable, checked := s.reachableRepoSet(ctx, orgID, userID)
	return firstUnreachableRepo(reachable, checked, repos)
}

// firstUnreachableRepo is the pure policy: given the reachable set and
// whether it's authoritative (checked), return the first repo not in it.
// checked=false → never reject (fail open). Matching is case-insensitive
// because GitHub repo identifiers are; the set is keyed lowercase.
func firstUnreachableRepo(reachable map[string]struct{}, checked bool, repos []domain.TeamGitHubRepo) (string, bool) {
	if !checked {
		return "", false
	}
	for _, r := range repos {
		if _, ok := reachable[strings.ToLower(r.Slug())]; !ok {
			return r.Slug(), true
		}
	}
	return "", false
}

// reachableRepoSet enumerates every "owner/repo" the org's GitHub
// credentials can reach — the union of the org PAT's repos
// (ListUserRepos) and each App installation's repos
// (ListInstallationRepos) — returning the lowercased slug set plus a
// `checked` flag.
//
// checked is true only when at least one source was attempted AND every
// attempted fetch succeeded: a complete view safe to reject against. Any
// fetch error, or no credentials at all, yields checked=false so callers
// fail open. Validating against this union (rather than only the PAT
// repos the picker currently shows) is deliberately the *broadest*
// reachable set, so we never reject a repo the picker could legitimately
// have offered.
func (s *Server) reachableRepoSet(ctx context.Context, orgID, userID string) (map[string]struct{}, bool) {
	set := map[string]struct{}{}
	attempted := 0
	clean := true

	add := func(repos []ghclient.UserRepo) {
		for _, repo := range repos {
			if repo.FullName != "" {
				set[strings.ToLower(repo.FullName)] = struct{}{}
			}
		}
	}

	// Org PAT source (mirrors handleGitHubRepos). Credentials + the
	// per-org base URL are read through the app pool inside WithTx so the
	// SecretStore decrypts under the caller's claims.
	var (
		creds  auth.Credentials
		orgSet domain.OrgSettings
	)
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		creds, _ = integrations.Load(ctx, tx.Secrets, orgID)
		var e error
		orgSet, e = tx.Orgs.GetSettings(ctx, orgID)
		return e
	}); err != nil {
		clean = false
	}
	if creds.GitHubPAT != "" {
		baseURL := orgSet.GitHubBaseURL
		if baseURL == "" {
			baseURL = creds.GitHubURL
		}
		if baseURL != "" {
			attempted++
			repos, err := ghclient.NewClient(baseURL, creds.GitHubPAT).ListUserRepos()
			if err != nil {
				clean = false
			} else {
				add(repos)
			}
		}
	}

	// App installation sources, one per installed account. Each
	// installation token can reach a distinct repo set; their union is
	// the App-mode reachable universe.
	if s.ghResolver != nil && s.githubApps != nil {
		insts, err := s.githubApps.ListInstallationsForOrgSystem(ctx, orgID)
		if err != nil {
			clean = false
		}
		for _, inst := range insts {
			attempted++
			client, err := s.ghResolver.ClientFor(ctx, orgID, inst.AccountLogin)
			if err != nil {
				clean = false
				continue
			}
			repos, err := client.ListInstallationRepos()
			if err != nil {
				clean = false
				continue
			}
			add(repos)
		}
	}

	return set, attempted > 0 && clean
}
