package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/sky-ai-eng/todo-triage/internal/auth"
	"github.com/sky-ai-eng/todo-triage/internal/config"
	"github.com/sky-ai-eng/todo-triage/internal/db"
)

type setupRequest struct {
	GitHubURL string `json:"github_url"`
	GitHubPAT string `json:"github_pat"`
	JiraURL   string `json:"jira_url"`
	JiraPAT   string `json:"jira_pat"`
}

type setupResponse struct {
	GitHub *auth.GitHubUser `json:"github,omitempty"`
	Jira   *auth.JiraUser   `json:"jira,omitempty"`
}

func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.GitHubURL == "" || req.GitHubPAT == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub URL and token are required"})
		return
	}

	resp := setupResponse{}

	// Validate GitHub if provided
	if req.GitHubURL != "" && req.GitHubPAT != "" {
		ghUser, err := auth.ValidateGitHub(req.GitHubURL, req.GitHubPAT)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "GitHub: " + err.Error(),
				"field": "github",
			})
			return
		}
		resp.GitHub = ghUser
	}

	// Validate Jira if provided
	if req.JiraURL != "" && req.JiraPAT != "" {
		jiraUser, err := auth.ValidateJira(req.JiraURL, req.JiraPAT)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "Jira: " + err.Error(),
				"field": "jira",
			})
			return
		}
		resp.Jira = jiraUser
	}

	// Store credentials in keychain (include username if we validated GitHub)
	ghUsername := ""
	if resp.GitHub != nil {
		ghUsername = resp.GitHub.Login
	}
	if err := auth.Store(auth.Credentials{
		GitHubURL:      req.GitHubURL,
		GitHubPAT:      req.GitHubPAT,
		GitHubUsername:  ghUsername,
		JiraURL:        req.JiraURL,
		JiraPAT:        req.JiraPAT,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store credentials: " + err.Error()})
		return
	}

	// Persist base URLs in config so they survive without keychain access
	cfg, _ := config.Load()
	if req.GitHubURL != "" {
		cfg.GitHub.BaseURL = req.GitHubURL
	}
	if req.JiraURL != "" {
		cfg.Jira.BaseURL = req.JiraURL
	}
	if err := config.Save(cfg); err != nil {
		log.Printf("[auth] warning: failed to save config: %v", err)
	}

	// Setup always includes GitHub — trigger full restart
	if s.onGitHubChanged != nil {
		go s.onGitHubChanged()
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	creds, err := auth.Load()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"error":      err.Error(),
		})
		return
	}

	repoCount, _ := db.CountConfiguredRepos(s.db)

	// GitHub is mandatory — configured requires GitHub creds + at least one repo
	result := map[string]any{
		"configured":   creds.GitHubPAT != "" && creds.GitHubURL != "" && repoCount > 0,
		"github":       creds.GitHubPAT != "",
		"jira":         creds.JiraPAT != "",
		"github_repos": repoCount,
	}

	if creds.GitHubURL != "" {
		result["github_url"] = creds.GitHubURL
	}
	if creds.JiraURL != "" {
		result["jira_url"] = creds.JiraURL
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAuthDelete(w http.ResponseWriter, r *http.Request) {
	if err := auth.Clear(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}
