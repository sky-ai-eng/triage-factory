package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// handleGitHubRepos returns all repositories the authenticated user has access to.
func (s *Server) handleGitHubRepos(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject
	// Credentials + per-org base URL read through the app pool inside
	// WithTx so SecretStore decrypts under the user's claims and
	// org_settings_select RLS is enforced — same shape the rest of
	// the post-SKY-355 settings surface uses.
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub not configured"})
		return
	}
	baseURL := orgSet.GitHubBaseURL
	if baseURL == "" {
		baseURL = creds.GitHubURL
	}

	client := ghclient.NewClient(baseURL, creds.GitHubPAT)
	repos, err := client.ListUserRepos()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch repos: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, repos)
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

	if s.ghClient == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub not configured"})
		return
	}

	query := r.URL.Query().Get("q")
	branches, err := s.ghClient.ListBranches(owner, repo, query, 30)
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
