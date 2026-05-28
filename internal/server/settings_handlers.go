package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// errAbortHandler is a sentinel used by handlers that write their own
// error response inside a WithTx closure and need the outer code to
// return without writing a second response.
var errAbortHandler = fmt.Errorf("handler already responded")

// --------------------------------------------------------------------
// /api/settings/user — any authenticated user
// --------------------------------------------------------------------

type userSettingsResponse struct {
	UserSettings   domain.UserSettings `json:"user_settings"`
	GitHubUsername string              `json:"github_username,omitempty"`
	JiraAccountID  string              `json:"jira_account_id,omitempty"`
}

func (s *Server) handleUserSettingsGet(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject

	var resp userSettingsResponse
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		settings, err := tx.Users.GetSettings(r.Context(), userID)
		if err != nil {
			return fmt.Errorf("user settings: %w", err)
		}
		resp.UserSettings = settings

		ghUsername, err := tx.Users.GetGitHubUsername(r.Context(), userID)
		if err != nil {
			return fmt.Errorf("github username: %w", err)
		}
		resp.GitHubUsername = ghUsername

		jiraAccountID, _, err := tx.Users.GetJiraIdentity(r.Context(), userID)
		if err != nil {
			return fmt.Errorf("jira identity: %w", err)
		}
		resp.JiraAccountID = jiraAccountID
		return nil
	}); err != nil {
		internalError(w, "settings/user", err)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

type userSettingsUpdate struct {
	UserSettings *domain.UserSettings `json:"user_settings,omitempty"`
}

func (s *Server) handleUserSettingsPost(w http.ResponseWriter, r *http.Request) {
	userID := ClaimsFrom(r.Context()).Subject
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	var req userSettingsUpdate
	if !decodeJSON(w, r, &req, "") {
		return
	}

	if req.UserSettings != nil {
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			return tx.Users.UpdateSettings(r.Context(), userID, *req.UserSettings)
		}); err != nil {
			internalError(w, "settings/user", err)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// --------------------------------------------------------------------
// /api/settings/team/{team_id} — team members (GET), team admin (POST)
//
// The path segment {team_id} accepts a UUID or the literal "default",
// which resolves to the org's default team via TeamsStore.GetDefaultForOrg.
// This keeps the frontend functional before team pickers ship (SKY-358).
// --------------------------------------------------------------------

type teamSettingsResponse struct {
	TeamSettings domain.TeamSettings  `json:"team_settings"`
	JiraProjects []jiraProjectSettings `json:"jira_projects"`
}

func (s *Server) handleTeamSettingsGet(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.resolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		writeResolveError(w, "settings/team", err)
		return
	}

	if !s.verifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}

	var resp teamSettingsResponse
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		settings, err := tx.Teams.GetSettings(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("team settings: %w", err)
		}
		resp.TeamSettings = settings

		rules, err := tx.JiraStatusRules.ListForTeam(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("jira rules: %w", err)
		}
		projects := rulesToProjectConfigsOrdered(rules, settings.JiraProjects)
		resp.JiraProjects = toJiraProjectSettings(projects)
		return nil
	}); err != nil {
		internalError(w, "settings/team", err)
		return
	}

	if resp.JiraProjects == nil {
		resp.JiraProjects = []jiraProjectSettings{}
	}

	writeJSON(w, http.StatusOK, resp)
}

type teamSettingsUpdate struct {
	AIModel                    string                 `json:"ai_model,omitempty"`
	AIAutoDelegate             *bool                  `json:"ai_auto_delegate_enabled,omitempty"`
	AIReprioritizeThreshold    *int                   `json:"ai_reprioritize_threshold,omitempty"`
	AIPreferenceUpdateInterval *int                   `json:"ai_preference_update_interval,omitempty"`
	JiraProjects               *[]jiraProjectSettings `json:"jira_projects,omitempty"`
}

func (s *Server) handleTeamSettingsPost(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.resolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		writeResolveError(w, "settings/team", err)
		return
	}

	if !s.verifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	if !s.requireTeamAdmin(w, r, orgID, userID, teamID) {
		return
	}

	var req teamSettingsUpdate
	if !decodeJSON(w, r, &req, "") {
		return
	}

	var (
		prevProjects    []jiraProjectConfig
		writtenProjects []jiraProjectConfig
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamSet, err := tx.Teams.GetSettings(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("load team settings: %w", err)
		}
		rules, err := tx.JiraStatusRules.ListForTeam(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("load jira rules: %w", err)
		}
		projects := rulesToProjectConfigsOrdered(rules, teamSet.JiraProjects)
		prevProjects = cloneJiraProjects(projects)

		if req.AIModel != "" {
			teamSet.DefaultModel = req.AIModel
		}
		if req.AIAutoDelegate != nil {
			teamSet.AutoDelegateEnabled = *req.AIAutoDelegate
		}
		if req.AIReprioritizeThreshold != nil {
			teamSet.AIReprioritizeThreshold = *req.AIReprioritizeThreshold
		}
		if req.AIPreferenceUpdateInterval != nil {
			teamSet.AIPreferenceUpdateInterval = *req.AIPreferenceUpdateInterval
		}

		if req.JiraProjects != nil {
			seen := map[string]bool{}
			next := make([]jiraProjectConfig, 0, len(*req.JiraProjects))
			for _, p := range *req.JiraProjects {
				key := normalizeJiraProjectKey(p.Key)
				if key == "" {
					badRequest(w, "jira_projects: project key must not be empty")
					return errAbortHandler
				}
				if !jiraProjectKeyRe.MatchString(key) {
					badRequest(w, "jira_projects: invalid project key "+key)
					return errAbortHandler
				}
				if seen[key] {
					badRequest(w, "jira_projects: duplicate project key "+key)
					return errAbortHandler
				}
				seen[key] = true
				normalized := jiraProjectConfig{Key: key, Pickup: p.Pickup, InProgress: p.InProgress, Done: p.Done}
				if err := validateProjectRules(normalized); err != nil {
					badRequest(w, err.Error())
					return errAbortHandler
				}
				next = append(next, normalized)
			}
			projects = next
		}

		writtenProjects = projects
		teamSet.JiraProjects = projectKeysFromConfigs(projects)
		if err := tx.Teams.UpdateSettings(r.Context(), teamID, teamSet); err != nil {
			return fmt.Errorf("save team settings: %w", err)
		}
		if err := tx.JiraStatusRules.ReplaceForTeam(r.Context(), teamID, projectConfigsToRules(projects)); err != nil {
			return fmt.Errorf("save jira rules: %w", err)
		}
		return nil
	}); err != nil {
		if err == errAbortHandler {
			return
		}
		internalError(w, "settings/team", err)
		return
	}

	if req.JiraProjects != nil && !jiraProjectsEqual(writtenProjects, prevProjects) {
		if s.onJiraChanged != nil {
			s.MarkJiraRestarted()
			go s.onJiraChanged(orgID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// --------------------------------------------------------------------
// /api/settings/org — org members (GET), org admin (POST)
// --------------------------------------------------------------------

type orgSettingsResponse struct {
	GitHubBaseURL       string `json:"github_base_url"`
	GitHubPollInterval  string `json:"github_poll_interval"`
	GitHubCloneProtocol string `json:"github_clone_protocol"`
	HasGitHubPAT        bool   `json:"has_github_pat"`
	JiraBaseURL         string `json:"jira_base_url"`
	JiraPollInterval    string `json:"jira_poll_interval"`
	HasJiraPAT          bool   `json:"has_jira_pat"`
	MaxLLMModelTier     string `json:"max_llm_model_tier,omitempty"`
	HasAnthropicAPIKey  bool   `json:"has_anthropic_api_key"`
	HasBedrockCreds     bool   `json:"has_bedrock_credentials"`
}

func (s *Server) handleOrgSettingsGet(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject

	var orgSet domain.OrgSettings
	var creds auth.Credentials
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var err error
		orgSet, err = tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return err
		}
		creds, _ = integrations.Load(r.Context(), tx.Secrets, orgID)
		return nil
	}); err != nil {
		internalError(w, "settings/org", err)
		return
	}

	writeJSON(w, http.StatusOK, orgSettingsResponse{
		GitHubBaseURL:       orgSet.GitHubBaseURL,
		GitHubPollInterval:  orgSet.GitHubPollInterval.String(),
		GitHubCloneProtocol: defaultedCloneProtocolView(orgSet.GitHubCloneProtocol),
		HasGitHubPAT:        creds.GitHubPAT != "",
		JiraBaseURL:         orgSet.JiraBaseURL,
		JiraPollInterval:    orgSet.JiraPollInterval.String(),
		HasJiraPAT:          creds.JiraPAT != "",
		MaxLLMModelTier:     orgSet.MaxLLMModelTier,
		HasAnthropicAPIKey:  orgSet.AnthropicAPIKeyRef != "",
		HasBedrockCreds:     orgSet.BedrockCredentialsRef != "",
	})
}

type orgSettingsUpdate struct {
	GitHubBaseURL       *string `json:"github_base_url"`
	GitHubPAT           string  `json:"github_pat,omitempty"`
	GitHubPollInterval  string  `json:"github_poll_interval,omitempty"`
	GitHubCloneProtocol string  `json:"github_clone_protocol,omitempty"`
	JiraBaseURL         *string `json:"jira_base_url"`
	JiraPAT             string  `json:"jira_pat,omitempty"`
	JiraPollInterval    string  `json:"jira_poll_interval,omitempty"`
	MaxLLMModelTier     string  `json:"max_llm_model_tier,omitempty"`
	AnthropicAPIKey     string  `json:"anthropic_api_key,omitempty"`
}

func (s *Server) handleOrgSettingsPost(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	if !s.requireOrgAdminRole(w, r, orgID, userID) {
		return
	}

	var req orgSettingsUpdate
	if !decodeJSON(w, r, &req, "") {
		return
	}

	var prevOrgSet domain.OrgSettings
	var creds auth.Credentials
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var err error
		prevOrgSet, err = tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return err
		}
		creds, _ = integrations.Load(r.Context(), tx.Secrets, orgID)
		return nil
	}); err != nil {
		internalError(w, "settings/org", err)
		return
	}

	orgSet := prevOrgSet

	if req.GitHubBaseURL != nil {
		orgSet.GitHubBaseURL = *req.GitHubBaseURL
		creds.GitHubURL = *req.GitHubBaseURL
	}
	if req.GitHubPollInterval != "" {
		if d, err := parseMinDuration(req.GitHubPollInterval, 10); err == nil {
			orgSet.GitHubPollInterval = d
		}
	}
	if req.GitHubCloneProtocol != "" {
		if req.GitHubCloneProtocol != "ssh" && req.GitHubCloneProtocol != "https" {
			badRequest(w, "github_clone_protocol must be 'ssh' or 'https'")
			return
		}
		orgSet.GitHubCloneProtocol = req.GitHubCloneProtocol
	}
	if req.JiraBaseURL != nil {
		orgSet.JiraBaseURL = *req.JiraBaseURL
		creds.JiraURL = *req.JiraBaseURL
	}
	if req.JiraPollInterval != "" {
		if d, err := parseMinDuration(req.JiraPollInterval, 10); err == nil {
			orgSet.JiraPollInterval = d
		}
	}
	if req.MaxLLMModelTier != "" {
		orgSet.MaxLLMModelTier = req.MaxLLMModelTier
	}

	// GitHub PAT: validate, persist identity bridge, store credential.
	if req.GitHubPAT != "" {
		url := creds.GitHubURL
		if url == "" {
			badRequest(w, "GitHub URL is required before setting a PAT")
			return
		}
		ghUser, err := auth.ValidateGitHub(url, req.GitHubPAT)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "GitHub: " + err.Error(),
				"field": "github_pat",
			})
			return
		}
		creds.GitHubPAT = req.GitHubPAT
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			return tx.Users.SetGitHubUsername(r.Context(), userID, ghUser.Login)
		}); err != nil {
			log.Printf("[settings/org] failed to persist users.github_username: %v", err)
		}
	}

	// Jira PAT: validate, persist identity bridge, store credential.
	if req.JiraPAT != "" {
		url := creds.JiraURL
		if url == "" {
			badRequest(w, "Jira URL is required before setting a PAT")
			return
		}
		jiraUser, err := auth.ValidateJira(url, req.JiraPAT)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "Jira: " + err.Error(),
				"field": "jira_pat",
			})
			return
		}
		creds.JiraPAT = req.JiraPAT
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			return tx.Users.SetJiraIdentity(r.Context(), userID, jiraUser.StableID(), jiraUser.DisplayName)
		}); err != nil {
			log.Printf("[settings/org] failed to persist users.jira_identity: %v", err)
		}
	}

	// SSH preflight: gate the transition into SSH mode.
	if orgSet.GitHubCloneProtocol == "ssh" && prevOrgSet.GitHubCloneProtocol != "ssh" {
		sshHost := worktree.SSHHostFromBaseURL(creds.GitHubURL)
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		err := worktree.PreflightSSH(ctx, sshHost)
		cancel()
		if err != nil {
			log.Printf("[settings/org] blocked SSH switch against %s: %v", sshHost, err)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error":  fmt.Sprintf("SSH preflight against %s failed — fix your SSH setup or keep HTTPS. %s", sshHost, err.Error()),
				"field":  "github_clone_protocol",
				"stderr": err.Error(),
			})
			return
		}
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Clear SecretStore entries when the caller explicitly empties a
		// URL. integrations.Save skips empty strings, so a bare Save
		// after zeroing creds.GitHubURL would leave the old URL in the
		// store while org_settings.github_base_url is already "".
		if req.GitHubBaseURL != nil && *req.GitHubBaseURL == "" {
			if err := integrations.ClearGitHub(r.Context(), tx.Secrets, orgID); err != nil {
				return fmt.Errorf("clear GitHub secrets: %w", err)
			}
		}
		if req.JiraBaseURL != nil && *req.JiraBaseURL == "" {
			if err := integrations.ClearJira(r.Context(), tx.Secrets, orgID); err != nil {
				return fmt.Errorf("clear Jira secrets: %w", err)
			}
		}
		if err := integrations.Save(r.Context(), tx.Secrets, orgID, creds); err != nil {
			return fmt.Errorf("save credentials: %w", err)
		}
		if req.AnthropicAPIKey != "" {
			if err := tx.Secrets.Put(r.Context(), orgID, "anthropic_api_key", req.AnthropicAPIKey, "Org's Anthropic API key"); err != nil {
				return fmt.Errorf("save Anthropic key: %w", err)
			}
			orgSet.AnthropicAPIKeyRef = "anthropic_api_key"
		}
		return tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet)
	}); err != nil {
		internalError(w, "settings/org", err)
		return
	}

	ghChanged := orgSet.GitHubBaseURL != prevOrgSet.GitHubBaseURL ||
		orgSet.GitHubPollInterval != prevOrgSet.GitHubPollInterval ||
		orgSet.GitHubCloneProtocol != prevOrgSet.GitHubCloneProtocol ||
		req.GitHubPAT != ""

	jiraChanged := orgSet.JiraBaseURL != prevOrgSet.JiraBaseURL ||
		orgSet.JiraPollInterval != prevOrgSet.JiraPollInterval ||
		req.JiraPAT != ""

	if ghChanged && s.onGitHubChanged != nil {
		s.MarkJiraRestarted()
		go s.onGitHubChanged(orgID)
	} else if jiraChanged && s.onJiraChanged != nil {
		s.MarkJiraRestarted()
		go s.onJiraChanged(orgID)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// --------------------------------------------------------------------
// Helpers: team/org role checks, team ID resolution, duration parsing
// --------------------------------------------------------------------

type resolveError struct {
	notFound bool
	err      error
}

func (e *resolveError) Error() string { return e.err.Error() }

// resolveTeamID converts a raw {team_id} path value to a concrete team
// UUID. The literal "default" resolves to the org's default team so the
// frontend can call /api/settings/team/default before team pickers ship.
// Non-"default" values are validated as UUIDs.
func (s *Server) resolveTeamID(ctx context.Context, orgID, userID, raw string) (string, error) {
	if raw != "default" {
		if _, err := uuid.Parse(raw); err != nil {
			return "", &resolveError{notFound: true, err: fmt.Errorf("invalid team_id")}
		}
		return raw, nil
	}
	var teamID string
	err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		teamID, e = tx.Teams.GetDefaultForOrg(ctx, orgID)
		return e
	})
	if err != nil {
		return "", &resolveError{notFound: false, err: err}
	}
	if teamID == "" {
		return "", &resolveError{notFound: true, err: fmt.Errorf("org %s has no default team", orgID)}
	}
	return teamID, nil
}

// verifyTeamInOrg confirms that team_id belongs to the active org.
// Returns 404 (not 403) to avoid leaking team existence across orgs.
func (s *Server) verifyTeamInOrg(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	if runmode.Current() == runmode.ModeLocal {
		return true
	}
	var belongs bool
	err := db.WithTx(r.Context(), s.db, db.Claims{Sub: userID},
		func(tx *sql.Tx) error {
			return tx.QueryRowContext(r.Context(),
				`SELECT tf.team_in_current_org($1::uuid)`, teamID,
			).Scan(&belongs)
		},
	)
	if err != nil {
		log.Printf("[settings] team-in-org check %s/%s: %v", teamID, orgID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if !belongs {
		http.NotFound(w, r)
		return false
	}
	return true
}

// requireTeamAdmin checks the user is an admin of the given team.
// Returns 403 on non-admin.
func (s *Server) requireTeamAdmin(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	if runmode.Current() == runmode.ModeLocal {
		return true
	}
	var isAdmin bool
	err := db.WithTx(r.Context(), s.db, db.Claims{Sub: userID},
		func(tx *sql.Tx) error {
			return tx.QueryRowContext(r.Context(),
				`SELECT tf.user_is_team_admin($1::uuid)`, teamID,
			).Scan(&isAdmin)
		},
	)
	if err != nil {
		log.Printf("[settings] team-admin check %s/%s/%s: %v", userID, orgID, teamID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if !isAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "team admin role required"})
		return false
	}
	return true
}

// requireOrgAdminRole checks the user is an admin of the given org.
// Returns 403 on non-admin.
func (s *Server) requireOrgAdminRole(w http.ResponseWriter, r *http.Request, orgID, userID string) bool {
	if runmode.Current() == runmode.ModeLocal {
		return true
	}
	isAdmin, err := s.userIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		log.Printf("[settings] org-admin check %s/%s: %v", userID, orgID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if !isAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "org admin role required"})
		return false
	}
	return true
}

func writeResolveError(w http.ResponseWriter, scope string, err error) {
	var re *resolveError
	if errors.As(err, &re) && re.notFound {
		notFound(w, "team")
		return
	}
	internalError(w, scope, err)
}

func parseMinDuration(s string, minSeconds int) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < time.Duration(minSeconds)*time.Second {
		return 0, fmt.Errorf("duration %s below minimum %ds", s, minSeconds)
	}
	return d, nil
}
