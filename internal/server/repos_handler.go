package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sky-ai-eng/todo-tinder/internal/auth"
	"github.com/sky-ai-eng/todo-tinder/internal/config"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/domain"
	ghclient "github.com/sky-ai-eng/todo-tinder/internal/github"
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
		ID          string  `json:"id"`
		Owner       string  `json:"owner"`
		Repo        string  `json:"repo"`
		Description string  `json:"description,omitempty"`
		HasReadme   bool    `json:"has_readme"`
		HasClaudeMd bool    `json:"has_claude_md"`
		HasAgentsMd bool    `json:"has_agents_md"`
		ProfileText string  `json:"profile_text,omitempty"`
		ProfiledAt  *string `json:"profiled_at,omitempty"`
	}

	result := make([]repoJSON, len(profiles))
	for i, p := range profiles {
		result[i] = repoJSON{
			ID:          p.ID,
			Owner:       p.Owner,
			Repo:        p.Repo,
			Description: p.Description,
			HasReadme:   p.HasReadme,
			HasClaudeMd: p.HasClaudeMd,
			HasAgentsMd: p.HasAgentsMd,
			ProfileText: p.ProfileText,
		}
		if p.ProfiledAt != nil {
			t := p.ProfiledAt.UTC().Format(time.RFC3339)
			result[i].ProfiledAt = &t
		}
	}

	writeJSON(w, http.StatusOK, result)
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

	// Trigger GitHub changed — re-profiles and restarts pollers
	if s.onGitHubChanged != nil {
		go s.onGitHubChanged()
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "repos": len(req.Repos)})
}
