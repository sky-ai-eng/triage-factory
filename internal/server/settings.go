package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/sky-ai-eng/todo-tinder/internal/auth"
	"github.com/sky-ai-eng/todo-tinder/internal/config"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/jira"
)

// settingsResponse combines config values with auth status so the frontend
// can render everything on one page.
type settingsResponse struct {
	GitHub   githubSettings `json:"github"`
	Jira     jiraSettings   `json:"jira"`
	Server   serverSettings `json:"server"`
	AI       aiSettings     `json:"ai"`
}

type githubSettings struct {
	Enabled      bool   `json:"enabled"`
	BaseURL      string `json:"base_url"`
	HasToken     bool   `json:"has_token"`
	PollInterval string `json:"poll_interval"`
}

type jiraSettings struct {
	Enabled          bool     `json:"enabled"`
	BaseURL          string   `json:"base_url"`
	HasToken         bool     `json:"has_token"`
	PollInterval     string   `json:"poll_interval"`
	Projects         []string `json:"projects"`
	PickupStatuses   []string `json:"pickup_statuses"`
	InProgressStatus string   `json:"in_progress_status"`
}

type serverSettings struct {
	Port int `json:"port"`
}

type aiSettings struct {
	Model                    string `json:"model"`
	ReprioritizeThreshold    int    `json:"reprioritize_threshold"`
	PreferenceUpdateInterval int    `json:"preference_update_interval"`
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config: " + err.Error()})
		return
	}
	creds, _ := auth.Load() // auth errors are non-fatal (keychain may be empty)

	resp := settingsResponse{
		GitHub: githubSettings{
			Enabled:      creds.GitHubPAT != "",
			BaseURL:      cfg.GitHub.BaseURL,
			HasToken:     creds.GitHubPAT != "",
			PollInterval: cfg.GitHub.PollInterval.String(),
		},
		Jira: jiraSettings{
			Enabled:          creds.JiraPAT != "",
			BaseURL:          cfg.Jira.BaseURL,
			HasToken:         creds.JiraPAT != "",
			PollInterval:     cfg.Jira.PollInterval.String(),
			Projects:         cfg.Jira.Projects,
			PickupStatuses:   cfg.Jira.PickupStatuses,
			InProgressStatus: cfg.Jira.InProgressStatus,
		},
		Server: serverSettings{
			Port: cfg.Server.Port,
		},
		AI: aiSettings{
			Model:                    cfg.AI.Model,
			ReprioritizeThreshold:    cfg.AI.ReprioritizeThreshold,
			PreferenceUpdateInterval: cfg.AI.PreferenceUpdateInterval,
		},
	}

	if resp.Jira.Projects == nil {
		resp.Jira.Projects = []string{}
	}
	if resp.Jira.PickupStatuses == nil {
		resp.Jira.PickupStatuses = []string{}
	}

	writeJSON(w, http.StatusOK, resp)
}

type settingsUpdateRequest struct {
	// Connections — only validate/store if token is non-empty
	GitHubEnabled bool   `json:"github_enabled"`
	GitHubURL     string `json:"github_url"`
	GitHubPAT     string `json:"github_pat"` // empty means "keep existing"
	JiraEnabled   bool   `json:"jira_enabled"`
	JiraURL       string `json:"jira_url"`
	JiraPAT       string `json:"jira_pat"` // empty means "keep existing"

	// Config
	GitHubPollInterval   string   `json:"github_poll_interval"`
	JiraPollInterval     string   `json:"jira_poll_interval"`
	JiraProjects         []string `json:"jira_projects"`
	JiraPickupStatuses   []string `json:"jira_pickup_statuses"`
	JiraInProgressStatus string   `json:"jira_in_progress_status"`
	AIModel              string   `json:"ai_model"`
	ServerPort           int      `json:"server_port"`
}

func (s *Server) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	var req settingsUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// Load existing state — snapshot for change detection
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config: " + err.Error()})
		return
	}
	creds, _ := auth.Load() // auth errors are non-fatal

	// Snapshot pre-change values for diffing
	prevGHURL := creds.GitHubURL
	prevGHPAT := creds.GitHubPAT
	prevGHPollInterval := cfg.GitHub.PollInterval
	prevJiraURL := creds.JiraURL
	prevJiraPAT := creds.JiraPAT
	prevJiraProjects := cfg.Jira.Projects
	prevJiraPickupStatuses := cfg.Jira.PickupStatuses
	prevJiraInProgressStatus := cfg.Jira.InProgressStatus
	prevJiraPollInterval := cfg.Jira.PollInterval

	// --- Handle GitHub ---
	if req.GitHubEnabled {
		if req.GitHubURL != "" {
			cfg.GitHub.BaseURL = req.GitHubURL
			creds.GitHubURL = req.GitHubURL
		}
		// New token provided — validate it
		if req.GitHubPAT != "" {
			url := req.GitHubURL
			if url == "" {
				url = creds.GitHubURL
			}
			if url == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "GitHub URL is required"})
				return
			}
			ghUser, err := auth.ValidateGitHub(url, req.GitHubPAT)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"error": "GitHub: " + err.Error(),
					"field": "github",
				})
				return
			}
			creds.GitHubPAT = req.GitHubPAT
			creds.GitHubUsername = ghUser.Login
		}
		// Backfill username if we have a PAT but no stored username (e.g. upgrade)
		if creds.GitHubPAT != "" && creds.GitHubUsername == "" {
			url := creds.GitHubURL
			if url == "" {
				url = cfg.GitHub.BaseURL
			}
			if url != "" {
				if ghUser, err := auth.ValidateGitHub(url, creds.GitHubPAT); err == nil {
					creds.GitHubUsername = ghUser.Login
				}
			}
		}
	} else {
		// Disabled — clear GitHub credentials, keychain entries, and tracked data
		creds.GitHubURL = ""
		creds.GitHubPAT = ""
		creds.GitHubUsername = ""
		cfg.GitHub.BaseURL = ""
		auth.ClearGitHub()
		if err := db.ClearTrackedItems(s.db, "github"); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "failed to clear GitHub tracked items: " + err.Error(),
				"field": "github",
			})
			return
		}
	}

	// --- Handle Jira ---
	if req.JiraEnabled {
		if req.JiraURL != "" {
			cfg.Jira.BaseURL = req.JiraURL
			creds.JiraURL = req.JiraURL
		}
		if req.JiraPAT != "" {
			url := req.JiraURL
			if url == "" {
				url = creds.JiraURL
			}
			if url == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Jira URL is required"})
				return
			}
			_, err := auth.ValidateJira(url, req.JiraPAT)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"error": "Jira: " + err.Error(),
					"field": "jira",
				})
				return
			}
			creds.JiraPAT = req.JiraPAT
		}
	} else {
		creds.JiraURL = ""
		creds.JiraPAT = ""
		cfg.Jira.BaseURL = ""
		auth.ClearJira()
		if err := db.ClearTrackedItems(s.db, "jira"); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "failed to clear Jira tracked items: " + err.Error(),
				"field": "jira",
			})
			return
		}
	}

	// --- Update config fields ---
	if req.GitHubPollInterval != "" {
		if d, err := time.ParseDuration(req.GitHubPollInterval); err == nil && d >= 10*time.Second {
			cfg.GitHub.PollInterval = d
		}
	}
	if req.JiraPollInterval != "" {
		if d, err := time.ParseDuration(req.JiraPollInterval); err == nil && d >= 10*time.Second {
			cfg.Jira.PollInterval = d
		}
	}
	if req.JiraProjects != nil {
		cfg.Jira.Projects = req.JiraProjects
	}
	if req.JiraPickupStatuses != nil {
		cfg.Jira.PickupStatuses = req.JiraPickupStatuses
	}
	if req.JiraInProgressStatus != "" {
		cfg.Jira.InProgressStatus = req.JiraInProgressStatus
	}
	if req.AIModel != "" {
		cfg.AI.Model = req.AIModel
	}
	if req.ServerPort > 0 {
		cfg.Server.Port = req.ServerPort
	}

	// Persist
	if err := auth.Store(creds); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store credentials: " + err.Error()})
		return
	}
	if err := config.Save(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save config: " + err.Error()})
		return
	}

	// Detect what changed and fire the appropriate callback
	ghChanged := creds.GitHubURL != prevGHURL ||
		creds.GitHubPAT != prevGHPAT ||
		cfg.GitHub.PollInterval != prevGHPollInterval

	jiraChanged := creds.JiraURL != prevJiraURL ||
		creds.JiraPAT != prevJiraPAT ||
		!slices.Equal(cfg.Jira.Projects, prevJiraProjects) ||
		!slices.Equal(cfg.Jira.PickupStatuses, prevJiraPickupStatuses) ||
		cfg.Jira.InProgressStatus != prevJiraInProgressStatus ||
		cfg.Jira.PollInterval != prevJiraPollInterval

	if ghChanged && s.onGitHubChanged != nil {
		// GitHub change triggers full restart (includes Jira poller restart)
		go s.onGitHubChanged()
	} else if jiraChanged && s.onJiraChanged != nil {
		// Jira-only change — just restart Jira poller
		go s.onJiraChanged()
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleJiraStatuses returns available statuses for given Jira projects.
// Query params: ?project=PROJ1&project=PROJ2 (or uses configured projects if omitted).
func (s *Server) handleJiraStatuses(w http.ResponseWriter, r *http.Request) {
	creds, _ := auth.Load()
	if creds.JiraPAT == "" || creds.JiraURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Jira not configured"})
		return
	}

	projects := r.URL.Query()["project"]
	if len(projects) == 0 {
		cfg, _ := config.Load()
		projects = cfg.Jira.Projects
	}
	if len(projects) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no projects specified"})
		return
	}

	client := jira.NewClient(creds.JiraURL, creds.JiraPAT)

	// Collect statuses across all projects, deduplicated
	seen := map[string]bool{}
	var statuses []jira.Status
	for _, proj := range projects {
		projectStatuses, err := client.ProjectStatuses(proj)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch statuses for " + proj + ": " + err.Error()})
			return
		}
		for _, st := range projectStatuses {
			if !seen[st.Name] {
				seen[st.Name] = true
				statuses = append(statuses, st)
			}
		}
	}

	writeJSON(w, http.StatusOK, statuses)
}
