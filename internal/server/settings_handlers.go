package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
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
	userID := ClaimsFrom(r.Context()).Subject
	orgID := OrgIDFrom(r.Context())

	var resp userSettingsResponse
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		settings, err := tx.Users.GetSettings(r.Context(), userID)
		if err != nil {
			return fmt.Errorf("user settings: %w", err)
		}
		resp.UserSettings = settings

		// Identity is host-scoped (SKY-396): resolve the org's GitHub host
		// from org_settings, then look up the login for (user, host). An
		// absent row degrades to "" exactly as the old NULL column did.
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("org settings: %w", err)
		}
		ghUsername, err := tx.Users.GetGitHubLogin(r.Context(), userID, orgSet.GitHubBaseURL)
		if err != nil {
			return fmt.Errorf("github identity: %w", err)
		}
		resp.GitHubUsername = ghUsername

		// Jira identity is host-scoped too (SKY-397): look it up for the
		// org's Jira host (same org_settings already loaded above).
		jiraAccountID, _, err := tx.Users.GetJiraIdentity(r.Context(), userID, orgSet.JiraBaseURL)
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
	orgID := OrgIDFrom(r.Context())
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
	TeamSettings domain.TeamSettings   `json:"team_settings"`
	JiraProjects []jiraProjectSettings `json:"jira_projects"`
	// MemberCount + Role describe the caller's relationship to this team,
	// so the frontend can collapse to the flat N=1 layout and gate the
	// write-side fields without a second round trip. They live on the
	// team-scope response (not /api/me) because they're properties of the
	// team, not the user — switching teams refetches this endpoint and
	// gets the new team's count + the caller's role in it.
	MemberCount int    `json:"member_count"`
	Role        string `json:"role"`
}

func (s *Server) handleTeamSettingsGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, err := s.az.ResolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		authz.WriteResolveError(w, "settings/team", err)
		return
	}

	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
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

	count, role, err := s.az.TeamMemberCountAndRole(r.Context(), orgID, userID, teamID)
	if err != nil {
		internalError(w, "settings/team", err)
		return
	}
	resp.MemberCount = count
	resp.Role = role

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
	teamID, err := s.az.ResolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		authz.WriteResolveError(w, "settings/team", err)
		return
	}

	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	if !s.az.RequireTeamAdmin(w, r, orgID, userID, teamID) {
		return
	}

	var req teamSettingsUpdate
	if !decodeJSON(w, r, &req, "") {
		return
	}

	var (
		prevProjects    []jiraProjectConfig
		writtenProjects []jiraProjectConfig
		prevModel       string
		savedModel      string
		orgMaxTier      string
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamSet, err := tx.Teams.GetSettings(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("load team settings: %w", err)
		}
		prevModel = teamSet.DefaultModel
		if orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID); err == nil {
			orgMaxTier = orgSet.MaxLLMModelTier
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
		savedModel = teamSet.DefaultModel
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

	resp := map[string]string{"status": "saved"}
	// The team default doesn't override the org cap. If a newly-picked
	// default exceeds it, accept the save (the team owns its preference)
	// but tell them the effective model is the org's cap. Gate on an
	// actual model change so an unrelated save (e.g. editing projects,
	// which re-sends the current model) doesn't re-warn every time.
	if req.AIModel != "" && savedModel != prevModel {
		if eff, source := domain.EffectiveModel(savedModel, orgMaxTier); source == "org-cap" {
			resp["warning"] = fmt.Sprintf(
				"Team default of %s exceeds the org cap of %s. Effective model is %s.",
				savedModel, orgMaxTier, eff,
			)
		}
	}
	writeJSON(w, http.StatusOK, resp)
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
	// HasJiraCredential reports whether a usable Jira service credential is
	// stored for the org's auth-method marker — a Data Center PAT or a Cloud
	// email + API token — rather than the presence of a specific key, so a
	// Cloud org (which has no PAT) still reports true.
	HasJiraCredential  bool   `json:"has_jira_credential"`
	MaxLLMModelTier    string `json:"max_llm_model_tier,omitempty"`
	HasAnthropicAPIKey bool   `json:"has_anthropic_api_key"`
	HasBedrockCreds    bool   `json:"has_bedrock_credentials"`
	// MemberCount is the number of members in this org. Feeds the
	// frontend's N=1 collapse alongside the team member count. A property
	// of the org, so it rides the org-scope response rather than /api/me.
	MemberCount int `json:"member_count"`
}

func (s *Server) handleOrgSettingsGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
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

	// Fall back to SecretStore URLs when org_settings is empty — covers
	// env-overlay/legacy installs where the URL lives only in the
	// credential bundle.
	ghBaseURL := orgSet.GitHubBaseURL
	if ghBaseURL == "" {
		ghBaseURL = creds.GitHubURL
	}
	jiraBaseURL := orgSet.JiraBaseURL
	if jiraBaseURL == "" {
		jiraBaseURL = creds.JiraURL
	}

	memberCount, err := s.az.OrgMemberCount(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "settings/org", err)
		return
	}

	// Marker-based "is Jira connected" (matches the integrations-status signal),
	// so a Cloud org with no PAT still reports a stored credential.
	_, hasJiraCred := integrations.JiraSystemConfig(creds)

	writeJSON(w, http.StatusOK, orgSettingsResponse{
		GitHubBaseURL:       ghBaseURL,
		GitHubPollInterval:  orgSet.GitHubPollInterval.String(),
		GitHubCloneProtocol: defaultedCloneProtocolView(orgSet.GitHubCloneProtocol),
		HasGitHubPAT:        creds.GitHubPAT != "",
		JiraBaseURL:         jiraBaseURL,
		JiraPollInterval:    orgSet.JiraPollInterval.String(),
		HasJiraCredential:   hasJiraCred,
		MaxLLMModelTier:     orgSet.MaxLLMModelTier,
		HasAnthropicAPIKey:  orgSet.AnthropicAPIKeyRef != "",
		HasBedrockCreds:     orgSet.BedrockCredentialsRef != "",
		MemberCount:         memberCount,
	})
}

type orgSettingsUpdate struct {
	GitHubBaseURL       *string `json:"github_base_url"`
	GitHubPAT           *string `json:"github_pat"`
	GitHubPollInterval  string  `json:"github_poll_interval,omitempty"`
	GitHubCloneProtocol string  `json:"github_clone_protocol,omitempty"`
	JiraBaseURL         *string `json:"jira_base_url"`
	JiraPAT             *string `json:"jira_pat"`
	JiraPollInterval    string  `json:"jira_poll_interval,omitempty"`
	MaxLLMModelTier     *string `json:"max_llm_model_tier"`
	AnthropicAPIKey     *string `json:"anthropic_api_key"`
}

func (s *Server) handleOrgSettingsPost(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	if !s.az.RequireOrgAdminRole(w, r, orgID, userID) {
		return
	}

	var req orgSettingsUpdate
	if !decodeJSON(w, r, &req, "") {
		return
	}

	// XOR guard (TFAC-328): GitHub access is strictly App XOR PAT per org. An
	// org that has a registered GitHub App (staged or active) switches
	// credentials through the dedicated switch flow, never by dropping a PAT
	// into the settings field. Reject setting a non-empty PAT while an App is
	// registered; clearing the field (empty string / nil) stays allowed so
	// disconnect + the switch flow's own teardown aren't blocked.
	if req.GitHubPAT != nil && *req.GitHubPAT != "" {
		app, err := s.githubApps.GetForOrgSystem(r.Context(), orgID)
		if err != nil {
			internalError(w, "settings/org", err)
			return
		}
		if app != nil {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "this workspace uses a GitHub App — use the switch flow",
				"field": "github_pat",
			})
			return
		}
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
		// Multi-mode is HTTPS-only: refuse an ssh write rather than
		// persist a value the effective resolver (and the clone path) will
		// ignore. The UI hides the control in multi mode; this rejects a
		// direct API call.
		if req.GitHubCloneProtocol == "ssh" && runmode.Current() != runmode.ModeLocal {
			badRequest(w, "ssh clone protocol is not available in this deployment; use https")
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
	// Max model tier: nil = don't touch, "" = clear the cap, value = set.
	if req.MaxLLMModelTier != nil {
		tier := *req.MaxLLMModelTier
		if tier != "" && domain.ParseTier(tier) == domain.TierUnknown {
			badRequest(w, "max_llm_model_tier must be haiku, sonnet, or opus")
			return
		}
		orgSet.MaxLLMModelTier = tier
	}

	// GitHub PAT (PAT_1, the org bot credential): nil = don't touch, "" =
	// clear, non-empty = validate + set. We validate to reject a bad token,
	// but we do NOT bind the caller's GitHub identity here — saving the org
	// credential is an access concern, not an identity one. Per-user identity
	// (PAT_2) is captured only by the dedicated identity surface (the setup
	// wizard's User step / the Connect gate page → POST .../identity/github*),
	// so access and identity stay independent even when the same token is used.
	if req.GitHubPAT != nil {
		if *req.GitHubPAT == "" {
			creds.GitHubPAT = ""
		} else {
			url := creds.GitHubURL
			if url == "" {
				badRequest(w, "GitHub URL is required before setting a PAT")
				return
			}
			if _, err := auth.ValidateGitHub(r.Context(), url, *req.GitHubPAT); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"error": "GitHub: " + err.Error(),
					"field": "github_pat",
				})
				return
			}
			creds.GitHubPAT = *req.GitHubPAT
		}
	}

	// Jira PAT (PAT_1, the org bot credential): nil = don't touch, "" = clear,
	// non-empty = validate + set. We validate to reject a bad token, but we do
	// NOT bind the caller's Jira identity here — saving the org credential is an
	// access concern, not an identity one. Per-user Jira access (the stored
	// credential + derived identity) is captured only by the dedicated bind
	// surface (POST .../identity/jira/pat), so access and identity stay
	// independent even when the same token is used.
	if req.JiraPAT != nil {
		if *req.JiraPAT == "" {
			creds.JiraPAT = ""
		} else {
			url := creds.JiraURL
			if url == "" {
				badRequest(w, "Jira URL is required before setting a PAT")
				return
			}
			if _, err := auth.ValidateJira(r.Context(), jira.DataCenterPAT(url, *req.JiraPAT)); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"error": "Jira: " + err.Error(),
					"field": "jira_pat",
				})
				return
			}
			creds.JiraPAT = *req.JiraPAT
		}
	}

	// SSH preflight: gate the transition into SSH mode. Local-mode only —
	// PreflightSSH writes the container's ~/.ssh/known_hosts and probes the
	// operator's ssh-agent, neither of which belongs in a hosted
	// runtime. In multi mode the ssh write is already rejected above, so
	// orgSet.GitHubCloneProtocol can't be "ssh" here; the explicit mode gate
	// makes the no-SSH-in-multi guarantee provable at this call site too.
	if runmode.Current() == runmode.ModeLocal &&
		orgSet.GitHubCloneProtocol == "ssh" && prevOrgSet.GitHubCloneProtocol != "ssh" {
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
		// field. integrations.Save skips empty strings, so zeroing a
		// creds field without an explicit clear would leave the old
		// value in the store.
		// These clears touch ONLY the org access credentials (PAT_1). Per-user
		// identity is deliberately never swept on an org disconnect — it's owned
		// by its own capture surface (see the per-branch notes below).
		if req.GitHubBaseURL != nil && *req.GitHubBaseURL == "" {
			if err := integrations.ClearGitHub(r.Context(), tx.Secrets, orgID); err != nil {
				return fmt.Errorf("clear GitHub secrets: %w", err)
			}
			creds.GitHubURL = ""
			creds.GitHubPAT = ""
			// Per-user GitHub identity (PAT_2) is deliberately left intact:
			// it's owned by the dedicated identity surface, not the org
			// credential. Disconnecting the org's GitHub access doesn't unmake
			// the fact that this user is @login on that host — a leftover row is
			// harmless (runtime tolerates absent/stale identity) and still valid
			// if GitHub is reconnected to the same host. Identity is cleared
			// only by its own surface, never as a side effect of an org-access
			// change.
		} else if req.GitHubPAT != nil && *req.GitHubPAT == "" {
			if _, err := tx.Secrets.Delete(r.Context(), orgID, integrations.KeyGitHubPAT); err != nil {
				return fmt.Errorf("clear GitHub PAT: %w", err)
			}
		}
		if req.JiraBaseURL != nil && *req.JiraBaseURL == "" {
			if err := integrations.ClearJira(r.Context(), tx.Secrets, orgID); err != nil {
				return fmt.Errorf("clear Jira secrets: %w", err)
			}
			creds.JiraURL = ""
			creds.JiraPAT = ""
			// Per-user Jira access (the user's own stored credential + derived
			// identity) is deliberately left intact: it's owned by the dedicated
			// bind surface, not the org credential, and is custodied under a
			// separate per-user secret key. Disconnecting the org's Jira access
			// doesn't unmake the user's own binding — it's cleared only by its own
			// surface, never as a side effect of an org-access change.
		} else if req.JiraPAT != nil && *req.JiraPAT == "" {
			if _, err := tx.Secrets.Delete(r.Context(), orgID, integrations.KeyJiraPAT); err != nil {
				return fmt.Errorf("clear Jira PAT: %w", err)
			}
		}
		if err := integrations.Save(r.Context(), tx.Secrets, orgID, creds); err != nil {
			return fmt.Errorf("save credentials: %w", err)
		}
		if req.AnthropicAPIKey != nil {
			if *req.AnthropicAPIKey == "" {
				if _, err := tx.Secrets.Delete(r.Context(), orgID, "anthropic_api_key"); err != nil {
					return fmt.Errorf("clear Anthropic key: %w", err)
				}
				orgSet.AnthropicAPIKeyRef = ""
			} else {
				if err := tx.Secrets.Put(r.Context(), orgID, "anthropic_api_key", *req.AnthropicAPIKey, "Org's Anthropic API key"); err != nil {
					return fmt.Errorf("save Anthropic key: %w", err)
				}
				orgSet.AnthropicAPIKeyRef = "anthropic_api_key"
			}
		}
		return tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet)
	}); err != nil {
		internalError(w, "settings/org", err)
		return
	}

	ghChanged := orgSet.GitHubBaseURL != prevOrgSet.GitHubBaseURL ||
		orgSet.GitHubPollInterval != prevOrgSet.GitHubPollInterval ||
		orgSet.GitHubCloneProtocol != prevOrgSet.GitHubCloneProtocol ||
		req.GitHubPAT != nil

	jiraChanged := orgSet.JiraBaseURL != prevOrgSet.JiraBaseURL ||
		orgSet.JiraPollInterval != prevOrgSet.JiraPollInterval ||
		req.JiraPAT != nil

	if ghChanged && s.onGitHubChanged != nil {
		s.MarkJiraRestarted()
		go s.onGitHubChanged(orgID)
	} else if jiraChanged && s.onJiraChanged != nil {
		s.MarkJiraRestarted()
		go s.onJiraChanged(orgID)
	}

	resp := map[string]string{"status": "saved"}
	// Lowering the cap doesn't block the save — the admin has authority —
	// but if the default team already prefers a higher tier, surface that
	// its effective model just dropped. Gate on an actual cap change: the
	// frontend re-sends max_llm_model_tier on every org save, so without
	// this an unrelated save would re-warn each time the default team
	// sits above an unchanged cap. Single-team-per-org today, so we check
	// the default team; broadens to a team list when multi-team lands.
	if orgSet.MaxLLMModelTier != "" && orgSet.MaxLLMModelTier != prevOrgSet.MaxLLMModelTier {
		if w := s.capDowngradeWarning(r.Context(), orgID, userID, orgSet.MaxLLMModelTier); w != "" {
			resp["warning"] = w
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// capDowngradeWarning returns a non-empty message when the org's default
// team prefers a model above the given cap — i.e. the cap clamps it. Empty
// when no clamp applies or the lookup fails (best-effort UX, never blocks
// the save).
func (s *Server) capDowngradeWarning(ctx context.Context, orgID, userID, maxTier string) string {
	var teamDefault string
	err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		teamID, e := tx.Teams.GetDefaultForOrg(ctx, orgID)
		if e != nil || teamID == "" {
			return e
		}
		teamSet, e := tx.Teams.GetSettings(ctx, teamID)
		if e != nil {
			return e
		}
		teamDefault = teamSet.DefaultModel
		return nil
	})
	if err != nil || teamDefault == "" {
		return ""
	}
	if eff, source := domain.EffectiveModel(teamDefault, maxTier); source == "org-cap" {
		return fmt.Sprintf(
			"The default team prefers %s, which exceeds the new cap of %s. Its effective model is now %s.",
			teamDefault, maxTier, eff,
		)
	}
	return ""
}

// --------------------------------------------------------------------
// Helpers: duration parsing
// (the authorization checks, team-ID resolution, and resolve-error
// rendering live in the authz package)
// --------------------------------------------------------------------

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
