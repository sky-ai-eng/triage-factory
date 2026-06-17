package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// handleGitHubRepos returns the repositories the Settings picker offers. The
// source is tier-aware:
//
//   - App org (own App registered + active + ≥1 installation): the union of
//     each installation's repositories (GET /installation/repositories). A
//     GitHub App installation token can't call GET /user/repos — it 4xxs — and
//     under an App the only repos worth offering are the ones the App is
//     installed on.
//   - PAT-only org (no App / no installations): the authenticated user's repos
//     (GET /user/repos) — identical to the pre-App behavior.
func (s *Server) handleGitHubRepos(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject

	// Decide App-vs-PAT here rather than widening the resolver (which
	// deliberately hides which tier resolved): this handler is the one consumer
	// that must branch, so it reads the App registration + installations
	// itself. Both reads use the System door (the orgID is already authorized
	// by middleware); a read error degrades to the PAT path rather than
	// blanking the picker.
	var (
		app      *domain.OrgGitHubApp
		insts    []domain.OrgGitHubAppInstallation
		instsErr error
	)
	if s.githubApps != nil {
		// Both reads degrade to the PAT path on error (per above) rather than
		// blanking the picker, but log either failure — these silent points are
		// what made the original dead-end untraceable (TFAC-324). instsErr is
		// also load-bearing below: a failed installations read leaves insts nil,
		// which must not masquerade as a positive "installed on zero accounts".
		var appErr error
		app, appErr = s.githubApps.GetForOrgSystem(r.Context(), orgID)
		if appErr != nil {
			reposLog.Warn("read app registration failed, falling back to pat path", "org", orgID, "error", appErr)
		}
		insts, instsErr = s.githubApps.ListInstallationsForOrgSystem(r.Context(), orgID)
		if instsErr != nil {
			reposLog.Warn("list installations failed, falling back to pat path", "org", orgID, "error", instsErr)
		}
	}

	var repos []ghclient.UserRepo
	if app != nil && app.Active && len(insts) > 0 {
		var err error
		repos, err = s.installationReposUnion(r.Context(), orgID, insts)
		if err != nil {
			if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				reposLog.Warn("app installed but resolver produced no credentials for any installation", "org", orgID, "error", err)
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub not configured"})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch repos: " + err.Error()})
			return
		}
	} else {
		// PAT path — identical to the pre-App behavior. Credentials + per-org
		// base URL read through the app pool inside WithTx so SecretStore
		// decrypts under the user's claims and org_settings_select RLS is
		// enforced — same shape the rest of the post-SKY-355 settings surface
		// uses.
		var (
			creds  auth.Credentials
			orgSet domain.OrgSettings
		)
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			creds, _ = integrations.Load(r.Context(), tx.Secrets, orgID)
			var lerr error
			orgSet, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
			return lerr
		}); err != nil {
			internalError(w, "repos", err)
			return
		}
		if creds.GitHubPAT == "" || creds.GitHubURL == "" {
			// An active App installed on zero accounts dead-ends here in the
			// PAT fallback — no installation token to use, no PAT to borrow.
			// Surface that as its own 400 the picker can key install guidance
			// off, instead of the generic "not configured" that reads as
			// "add a PAT". This is the first-run dead-end TFAC-324 unblocks.
			//
			// instsErr == nil is load-bearing: only a *successful* read of zero
			// installations means "not installed". A failed read also leaves
			// insts empty, and reporting that as "not installed" would mislead
			// on a transient store error — so it falls through to the generic
			// message instead.
			if app != nil && app.Active && instsErr == nil && len(insts) == 0 {
				reposLog.Warn("app registered and active but installed on zero accounts, and no pat configured", "org", orgID)
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub App is not installed on any account"})
				return
			}
			reposLog.Warn("github not configured, no usable app installation and no pat", "org", orgID)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub not configured"})
			return
		}
		baseURL := orgSet.GitHubBaseURL
		if baseURL == "" {
			baseURL = creds.GitHubURL
		}

		client := ghclient.NewClient(baseURL, creds.GitHubPAT)
		var err error
		repos, err = client.ListUserRepos()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch repos: " + err.Error()})
			return
		}
	}

	// Warm the reachable-repo enumeration cache (SKY-409) so the
	// immediate-next team-repos PUT validates the selection against this
	// in-memory set in ~µs instead of re-enumerating the org. We just paid
	// for the enumeration; the gate shouldn't pay for it again. Only the
	// lowercased slug set is needed for the membership check, so we keep
	// that rather than the full UserRepo slice. TTL-bounded and evicted on
	// SetOnGitHubChanged.
	//
	// NB: warmed from whatever source produced `repos` — the App
	// installation-repos union for App orgs, ListUserRepos for PAT-only orgs.
	// Either way the cached set is exactly what the picker just showed, so the
	// hot-path gate can't reject a repo the user could actually have selected.
	slugs := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		if repo.FullName != "" {
			slugs[strings.ToLower(repo.FullName)] = struct{}{}
		}
	}
	s.reachableRepoCachePut(orgID, userID, slugs)

	writeJSON(w, http.StatusOK, repos)
}

// installationReposUnion lists each installation's repositories through the
// credential resolver (one tier-1 App-token client per installation account)
// and returns their union, deduped by lowercased full name and sorted stably
// by full name. This is the App-org replacement for ListUserRepos: an
// installation token can't call GET /user/repos, and the union is exactly the
// set of repos the org's App can act on.
//
// Per-installation failures are isolated — a bad mint or list on one account
// must not blank the whole picker, so they're logged and skipped while the
// rest still contribute. ErrNoGitHubCredentials is only surfaced when the
// resolver produced no client for any installation (genuine "not configured").
// But if clients resolved yet *every* list attempt failed and the union is
// empty, the failure is surfaced rather than returned as an empty list — an
// empty result there is a fetch error masquerading as "no repos" (and would
// also warm the reachable-repo cache with an empty set).
func (s *Server) installationReposUnion(ctx context.Context, orgID string, insts []domain.OrgGitHubAppInstallation) ([]ghclient.UserRepo, error) {
	if s.ghResolver == nil {
		return nil, ghclient.ErrNoGitHubCredentials
	}

	byName := make(map[string]ghclient.UserRepo)
	resolvedAny := false
	var lastListErr error
	for _, inst := range insts {
		client, err := s.ghResolver.ClientFor(ctx, orgID, inst.AccountLogin)
		if err != nil {
			if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
				continue
			}
			// Real DB/vault/RLS failure — propagate so the handler can 502
			// rather than silently returning a partial union.
			return nil, err
		}
		resolvedAny = true

		repos, err := client.ListInstallationRepos()
		if err != nil {
			reposLog.Warn("list installation repos failed, skipping installation", "org", orgID, "account", inst.AccountLogin, "error", err)
			lastListErr = err
			continue
		}
		for _, repo := range repos {
			if repo.FullName == "" {
				continue
			}
			key := strings.ToLower(repo.FullName)
			if _, ok := byName[key]; !ok {
				byName[key] = repo
			}
		}
	}
	if !resolvedAny {
		return nil, ghclient.ErrNoGitHubCredentials
	}
	// Clients resolved but produced nothing, and at least one list errored:
	// surface the fetch failure instead of an empty (and misleading) picker.
	// A genuinely empty union with no errors (App installed on zero repos) is
	// a legitimate empty result and falls through.
	if len(byName) == 0 && lastListErr != nil {
		return nil, fmt.Errorf("list installation repositories for org %s: %w", orgID, lastListErr)
	}

	out := make([]ghclient.UserRepo, 0, len(byName))
	for _, repo := range byName {
		out = append(out, repo)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].FullName) < strings.ToLower(out[j].FullName)
	})
	return out, nil
}

// handleRepoProfiles returns all configured repo profiles from the DB.
func (s *Server) handleRepoProfiles(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var profiles []domain.RepoProfile
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		profiles, e = tx.Repos.List(r.Context(), orgID)
		return e
	}); err != nil {
		internalError(w, "repos", err)
		return
	}

	type repoJSON struct {
		ID             string  `json:"id"`
		Owner          string  `json:"owner"`
		Repo           string  `json:"repo"`
		Description    string  `json:"description,omitempty"`
		HasReadme      bool    `json:"has_readme"`
		HasClaudeMd    bool    `json:"has_claude_md"`
		HasAgentsMd    bool    `json:"has_agents_md"`
		ProfileText    string  `json:"profile_text,omitempty"`
		DefaultBranch  string  `json:"default_branch,omitempty"`
		BaseBranch     string  `json:"base_branch,omitempty"`
		ProfiledAt     *string `json:"profiled_at,omitempty"`
		CloneStatus    string  `json:"clone_status,omitempty"`
		CloneError     string  `json:"clone_error,omitempty"`
		CloneErrorKind string  `json:"clone_error_kind,omitempty"`
	}

	result := make([]repoJSON, len(profiles))
	for i, p := range profiles {
		result[i] = repoJSON{
			ID:             p.ID,
			Owner:          p.Owner,
			Repo:           p.Repo,
			Description:    p.Description,
			HasReadme:      p.HasReadme,
			HasClaudeMd:    p.HasClaudeMd,
			HasAgentsMd:    p.HasAgentsMd,
			ProfileText:    p.ProfileText,
			DefaultBranch:  p.DefaultBranch,
			BaseBranch:     p.BaseBranch,
			CloneStatus:    p.CloneStatus,
			CloneError:     p.CloneError,
			CloneErrorKind: p.CloneErrorKind,
		}
		if p.ProfiledAt != nil {
			t := p.ProfiledAt.UTC().Format(time.RFC3339)
			result[i].ProfiledAt = &t
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// handleRepoUpdate updates per-repo settings like base_branch.
func (s *Server) handleRepoUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	owner := r.PathValue("owner")
	repo := r.PathValue("repo")
	if owner == "" || repo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing owner/repo"})
		return
	}
	repoID := owner + "/" + repo

	// Use json.RawMessage to distinguish null (clear) from omitted (no change).
	// *string can't tell them apart — both decode to nil.
	var req struct {
		BaseBranch json.RawMessage `json:"base_branch,omitempty"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	if req.BaseBranch != nil {
		var branch string
		if string(req.BaseBranch) == "null" {
			branch = "" // explicit null → clear
		} else if err := json.Unmarshal(req.BaseBranch, &branch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid base_branch value"})
			return
		}
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			return tx.Repos.UpdateBaseBranch(r.Context(), orgID, repoID, branch)
		}); err != nil {
			internalError(w, "repos", err)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// handleRepoBranches returns branches for a repo, with optional search filtering.
func (s *Server) handleRepoBranches(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repo := r.PathValue("repo")
	if owner == "" || repo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing owner/repo"})
		return
	}

	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}

	// Resolve per-repo (org App installation token → PAT) so App-only orgs
	// (no PAT) list branches through the installation client instead of 400-ing
	// on a nil global client. owner/repo are path values; a selective App
	// install that doesn't cover this repo falls through to the PAT.
	client, err := s.ghResolver.ClientForRepo(r.Context(), orgID, owner, repo)
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			reposLog.Warn("github not configured", "org", orgID, "owner", owner, "repo", repo, "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub not configured"})
			return
		}
		internalError(w, "repos", err)
		return
	}

	query := r.URL.Query().Get("q")
	branches, err := client.ListBranches(owner, repo, query, 30)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch branches: " + err.Error()})
		return
	}

	// Return just the names for simplicity
	names := make([]string, len(branches))
	for i, b := range branches {
		names[i] = b.Name
	}
	writeJSON(w, http.StatusOK, names)
}

// Repo *tracking* selection is per-team (SKY-375): writes go through
// PUT /api/settings/team/{id}/repos (handleTeamReposPut), which writes
// team_github_repos and reconciles the org-wide repo_profiles union. The
// old org-global POST /api/repos was removed in favor of that single,
// team-admin-gated entry point; GET /api/repos (the union profiles) and
// PATCH /api/repos/{owner}/{repo} (base_branch) remain.
