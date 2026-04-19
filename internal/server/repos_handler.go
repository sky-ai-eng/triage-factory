package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// handleGitHubRepos returns all repositories the authenticated user has access to.
func (s *Server) handleGitHubRepos(w http.ResponseWriter, r *http.Request) {
	creds, _ := auth.Load()
	if creds.GitHubPAT == "" || creds.GitHubURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub not configured"})
		return
	}

	cfg, _ := config.Load()
	baseURL := cfg.GitHub.BaseURL
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
	profiles, err := db.GetAllRepoProfiles(s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if profiles == nil {
		profiles = []domain.RepoProfile{}
	}

	type repoJSON struct {
		ID            string  `json:"id"`
		Owner         string  `json:"owner"`
		Repo          string  `json:"repo"`
		Description   string  `json:"description,omitempty"`
		HasReadme     bool    `json:"has_readme"`
		HasClaudeMd   bool    `json:"has_claude_md"`
		HasAgentsMd   bool    `json:"has_agents_md"`
		ProfileText   string  `json:"profile_text,omitempty"`
		DefaultBranch string  `json:"default_branch,omitempty"`
		BaseBranch    string  `json:"base_branch,omitempty"`
		ProfiledAt    *string `json:"profiled_at,omitempty"`
	}

	result := make([]repoJSON, len(profiles))
	for i, p := range profiles {
		result[i] = repoJSON{
			ID:            p.ID,
			Owner:         p.Owner,
			Repo:          p.Repo,
			Description:   p.Description,
			HasReadme:     p.HasReadme,
			HasClaudeMd:   p.HasClaudeMd,
			HasAgentsMd:   p.HasAgentsMd,
			ProfileText:   p.ProfileText,
			DefaultBranch: p.DefaultBranch,
			BaseBranch:    p.BaseBranch,
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
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
		if err := db.UpdateRepoBaseBranch(s.db, repoID, branch); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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

// handleReposSave updates the configured repos in the DB and triggers re-profiling.
func (s *Server) handleReposSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repos []string `json:"repos"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if len(req.Repos) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one repo is required"})
		return
	}

	if err := db.SetConfiguredRepos(s.db, req.Repos); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Trigger GitHub changed — re-profiles and restarts pollers (including
	// Jira). Mark Jira restarted synchronously so jiraPollReady flips false
	// before the async callback starts.
	if s.onGitHubChanged != nil {
		s.MarkJiraRestarted()
		go s.onGitHubChanged()
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "repos": len(req.Repos)})
}
