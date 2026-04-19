package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
)

// validateStatusRule enforces the shape invariants for a JiraStatusRule.
// hasWriteTarget distinguishes the two roles:
//   - true (InProgress/Done): TF transitions into this state, so Canonical
//     must be set whenever Members is non-empty, and must itself be a member.
//   - false (Pickup): TF only reads this state, it never writes tickets back
//     to it — Canonical must be empty. A non-empty Canonical is rejected even
//     if it happens to be in Members, because storing it would let stale
//     config drift through and later mislead readers ("why does Pickup have
//     a canonical?").
func validateStatusRule(name string, r config.JiraStatusRule, hasWriteTarget bool) error {
	if !hasWriteTarget {
		if r.Canonical != "" {
			return fmt.Errorf("jira %s: canonical must be empty — this rule has no write target", name)
		}
		return nil
	}
	if r.Canonical != "" && !slices.Contains(r.Members, r.Canonical) {
		return fmt.Errorf("jira %s: canonical status %q is not in members", name, r.Canonical)
	}
	if len(r.Members) > 0 && r.Canonical == "" {
		return fmt.Errorf("jira %s: canonical status is required when members are set", name)
	}
	return nil
}

// ruleEqual compares two status rules by value. Used by change detection to
// decide whether a Jira poller restart is needed. Nil-safe on the Members slice.
func ruleEqual(a, b config.JiraStatusRule) bool {
	return a.Canonical == b.Canonical && slices.Equal(a.Members, b.Members)
}

// settingsResponse combines config values with auth status so the frontend
// can render everything on one page.
type settingsResponse struct {
	GitHub githubSettings `json:"github"`
	Jira   jiraSettings   `json:"jira"`
	Server serverSettings `json:"server"`
	AI     aiSettings     `json:"ai"`
}

type githubSettings struct {
	Enabled      bool   `json:"enabled"`
	BaseURL      string `json:"base_url"`
	HasToken     bool   `json:"has_token"`
	PollInterval string `json:"poll_interval"`
}

type jiraSettings struct {
	Enabled      bool                  `json:"enabled"`
	BaseURL      string                `json:"base_url"`
	HasToken     bool                  `json:"has_token"`
	PollInterval string                `json:"poll_interval"`
	Projects     []string              `json:"projects"`
	Pickup       config.JiraStatusRule `json:"pickup"`
	InProgress   config.JiraStatusRule `json:"in_progress"`
	Done         config.JiraStatusRule `json:"done"`
}

type serverSettings struct {
	Port int `json:"port"`
}

type aiSettings struct {
	Model                    string `json:"model"`
	ReprioritizeThreshold    int    `json:"reprioritize_threshold"`
	PreferenceUpdateInterval int    `json:"preference_update_interval"`
	AutoDelegateEnabled      bool   `json:"auto_delegate_enabled"`
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
			Enabled:      creds.JiraPAT != "",
			BaseURL:      cfg.Jira.BaseURL,
			HasToken:     creds.JiraPAT != "",
			PollInterval: cfg.Jira.PollInterval.String(),
			Projects:     cfg.Jira.Projects,
			Pickup:       cfg.Jira.Pickup,
			InProgress:   cfg.Jira.InProgress,
			Done:         cfg.Jira.Done,
		},
		Server: serverSettings{
			Port: cfg.Server.Port,
		},
		AI: aiSettings{
			Model:                    cfg.AI.Model,
			ReprioritizeThreshold:    cfg.AI.ReprioritizeThreshold,
			PreferenceUpdateInterval: cfg.AI.PreferenceUpdateInterval,
			AutoDelegateEnabled:      cfg.AI.AutoDelegateEnabled,
		},
	}

	if resp.Jira.Projects == nil {
		resp.Jira.Projects = []string{}
	}
	if resp.Jira.Pickup.Members == nil {
		resp.Jira.Pickup.Members = []string{}
	}
	if resp.Jira.InProgress.Members == nil {
		resp.Jira.InProgress.Members = []string{}
	}
	if resp.Jira.Done.Members == nil {
		resp.Jira.Done.Members = []string{}
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
	GitHubPollInterval string   `json:"github_poll_interval"`
	JiraPollInterval   string   `json:"jira_poll_interval"`
	JiraProjects       []string `json:"jira_projects"`
	// JiraPickup/InProgress/Done are pointers so the request can distinguish
	// "don't touch" (nil) from "clear" ({Members:[], Canonical:""}). Empty
	// members + non-empty canonical is a validation error.
	JiraPickup     *config.JiraStatusRule `json:"jira_pickup,omitempty"`
	JiraInProgress *config.JiraStatusRule `json:"jira_in_progress,omitempty"`
	JiraDone       *config.JiraStatusRule `json:"jira_done,omitempty"`
	AIModel        string                 `json:"ai_model"`
	AIAutoDelegate *bool                  `json:"ai_auto_delegate_enabled"` // pointer to distinguish absent from false
	ServerPort     int                    `json:"server_port"`
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
	prevJiraPickup := cfg.Jira.Pickup
	prevJiraInProgress := cfg.Jira.InProgress
	prevJiraDone := cfg.Jira.Done
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
		// Disconnect is a soft gesture — clear credentials and stop polling,
		// but keep entities/tasks/runs/memory intact. Reconnecting the same
		// account resumes where we left off. Full wipe is a separate
		// destructive action (not implemented in v1).
		creds.GitHubURL = ""
		creds.GitHubPAT = ""
		creds.GitHubUsername = ""
		cfg.GitHub.BaseURL = ""
		if err := auth.ClearGitHub(); err != nil {
			log.Printf("[settings] failed to clear GitHub keychain entry: %v", err)
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
			jiraUser, err := auth.ValidateJira(url, req.JiraPAT)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"error": "Jira: " + err.Error(),
					"field": "jira",
				})
				return
			}
			creds.JiraPAT = req.JiraPAT
			creds.JiraDisplayName = jiraUser.DisplayName
		}
	} else {
		// Soft disconnect — keep entities/tasks/runs/memory intact.
		creds.JiraURL = ""
		creds.JiraPAT = ""
		creds.JiraDisplayName = ""
		cfg.Jira.BaseURL = ""
		if err := auth.ClearJira(); err != nil {
			log.Printf("[settings] failed to clear Jira keychain entry: %v", err)
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
	// Validate rule shapes before touching cfg — reject the whole request if
	// any rule has canonical outside members. Pickup has no canonical to check.
	if req.JiraInProgress != nil {
		if err := validateStatusRule("in_progress", *req.JiraInProgress, true); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		cfg.Jira.InProgress = *req.JiraInProgress
	}
	if req.JiraDone != nil {
		if err := validateStatusRule("done", *req.JiraDone, true); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		cfg.Jira.Done = *req.JiraDone
	}
	if req.JiraPickup != nil {
		if err := validateStatusRule("pickup", *req.JiraPickup, false); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		cfg.Jira.Pickup = *req.JiraPickup
	}
	if req.AIModel != "" {
		cfg.AI.Model = req.AIModel
	}
	if req.AIAutoDelegate != nil {
		cfg.AI.AutoDelegateEnabled = *req.AIAutoDelegate
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
		!ruleEqual(cfg.Jira.Pickup, prevJiraPickup) ||
		!ruleEqual(cfg.Jira.InProgress, prevJiraInProgress) ||
		!ruleEqual(cfg.Jira.Done, prevJiraDone) ||
		cfg.Jira.PollInterval != prevJiraPollInterval

	// Mark Jira restarted synchronously before launching the async callback so
	// jiraPollReady flips false before this request returns. Otherwise the
	// frontend can race ahead and hit /api/jira/stock while the old state is
	// still reported as ready.
	if ghChanged && s.onGitHubChanged != nil {
		// GitHub change triggers full restart (includes Jira poller restart)
		s.MarkJiraRestarted()
		go s.onGitHubChanged()
	} else if jiraChanged && s.onJiraChanged != nil {
		// Jira-only change — just restart Jira poller
		s.MarkJiraRestarted()
		go s.onJiraChanged()
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleJiraConnect validates and stores Jira credentials without saving
// the rest of the settings. This powers the two-stage settings flow: connect
// first, then configure projects and statuses.
func (s *Server) handleJiraConnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
		PAT string `json:"pat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.URL == "" || req.PAT == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url and pat are required"})
		return
	}

	jiraUser, err := auth.ValidateJira(req.URL, req.PAT)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}

	// Load existing state before writing anything (fail early)
	creds, err := auth.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load credentials: " + err.Error()})
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load config: " + err.Error()})
		return
	}

	// Persist credentials and config
	creds.JiraURL = req.URL
	creds.JiraPAT = req.PAT
	creds.JiraDisplayName = jiraUser.DisplayName
	cfg.Jira.BaseURL = req.URL

	if err := auth.Store(creds); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store credentials: " + err.Error()})
		return
	}
	if err := config.Save(cfg); err != nil {
		// Roll back keychain to avoid creds/config desync
		creds.JiraURL = ""
		creds.JiraPAT = ""
		auth.Store(creds) //nolint:errcheck
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save config: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":       "connected",
		"display_name": jiraUser.DisplayName,
	})
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

	// Intersect statuses across all projects — only return statuses that
	// exist in every project. A union would let users pick a status that
	// fails on some projects (TransitionTo can't find the transition).
	var counts map[string]int            // status name → number of projects it appears in
	var canonical map[string]jira.Status // status name → first-seen Status object
	for i, proj := range projects {
		projectStatuses, err := client.ProjectStatuses(proj)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch statuses for " + proj + ": " + err.Error()})
			return
		}
		if i == 0 {
			counts = make(map[string]int, len(projectStatuses))
			canonical = make(map[string]jira.Status, len(projectStatuses))
		}
		seen := map[string]bool{}
		for _, st := range projectStatuses {
			if !seen[st.Name] {
				seen[st.Name] = true
				counts[st.Name]++
				if _, ok := canonical[st.Name]; !ok {
					canonical[st.Name] = st
				}
			}
		}
	}

	var statuses []jira.Status
	for name, count := range counts {
		if count == len(projects) {
			statuses = append(statuses, canonical[name])
		}
	}
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].Name < statuses[j].Name
	})

	writeJSON(w, http.StatusOK, statuses)
}
