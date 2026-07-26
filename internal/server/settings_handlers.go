package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
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

		// Identity is host-scoped: resolve the org's GitHub host
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

		// Jira identity is host-scoped too: look it up for the
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
// This keeps the frontend functional before team pickers ship.
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
	// PermissionAbsentGraceMinSeconds / PermissionAbsentGraceMaxSeconds advertise
	// the honored bounds of the unattended-prompt grace window so the team
	// settings UI can render a slider whose range tracks the backend (the 1s
	// floor clampGrace enforces and the ceiling just below permTimeout()) instead
	// of hardcoding it.
	PermissionAbsentGraceMinSeconds int `json:"permission_absent_grace_min_seconds"`
	PermissionAbsentGraceMaxSeconds int `json:"permission_absent_grace_max_seconds"`
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
	resp.PermissionAbsentGraceMinSeconds = delegate.AbsentGraceMinSeconds
	resp.PermissionAbsentGraceMaxSeconds = delegate.AbsentGraceMaxSeconds

	writeJSON(w, http.StatusOK, resp)
}

type teamSettingsUpdate struct {
	AIModel                    string                 `json:"ai_model,omitempty"`
	AIAutoDelegate             *bool                  `json:"ai_auto_delegate_enabled,omitempty"`
	AIReprioritizeThreshold    *int                   `json:"ai_reprioritize_threshold,omitempty"`
	AIPreferenceUpdateInterval *int                   `json:"ai_preference_update_interval,omitempty"`
	JiraProjects               *[]jiraProjectSettings `json:"jira_projects,omitempty"`
	// BranchTemplate is the team's branch-name convention shown to delegated
	// agents as envelope guidance (TFAC-498), not enforced. Pointer so an
	// unrelated save that omits it leaves the stored value untouched; an empty
	// string coalesces to domain.DefaultBranchTemplate so a blank never persists.
	BranchTemplate *string `json:"branch_template,omitempty"`
	// ReviewPosture is how the team's delegated reviews reach GitHub
	// (TFAC-680) — one of domain.ValidReviewPostures. Pointer for the same
	// reason as BranchTemplate: an unrelated save that omits the key must not
	// clobber the stored posture. An empty string coalesces to
	// domain.DefaultReviewPosture; an unrecognized value is a 400.
	ReviewPosture *string `json:"review_posture,omitempty"`
	// Presence-gated absent auto-deny knobs (TFAC-392). Pointers so an
	// unrelated save (e.g. editing projects) that omits them leaves the
	// stored values untouched. Grace is in seconds on the wire (the UI input
	// is seconds); it's stored as ms and the spawner clamps it at run time.
	PermissionAbsentAutodenyEnabled *bool `json:"permission_absent_autodeny_enabled,omitempty"`
	PermissionAbsentGraceSeconds    *int  `json:"permission_absent_grace_seconds,omitempty"`
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
	// Block writes to an archived team (TFAC-448). The team-settings family gates
	// on user_is_team_admin, which doesn't carry the archived filter baked into
	// user_can_write_team, so the explicit gate is required here.
	if !s.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
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
		if req.BranchTemplate != nil {
			// Coalesce a blank to the default so an empty string never persists
			// (mirrors the model-cap "" → default convention). The literal
			// "<ticket-id>" stays verbatim — it's substituted at prompt-render time.
			bt := *req.BranchTemplate
			if bt == "" {
				bt = domain.DefaultBranchTemplate
			}
			teamSet.BranchTemplate = bt
		}
		if req.ReviewPosture != nil {
			// Blank coalesces to the default (same convention as the template
			// above); anything else must name a known posture — an unrecognized
			// value would silently degrade to "stage everything" at finalize
			// time, which is exactly the misconfiguration a team switching to
			// auto would never notice.
			rp := *req.ReviewPosture
			if rp == "" {
				rp = domain.DefaultReviewPosture
			}
			if !domain.ValidReviewPosture(rp) {
				badRequest(w, "review_posture: unknown value "+rp)
				return errAbortHandler
			}
			teamSet.ReviewPosture = rp
		}
		if req.PermissionAbsentAutodenyEnabled != nil {
			teamSet.PermissionAbsentAutodenyEnabled = *req.PermissionAbsentAutodenyEnabled
		}
		if req.PermissionAbsentGraceSeconds != nil {
			// Accept seconds from the UI; store ms. Clamp into the honored band
			// [AbsentGraceMinSeconds, AbsentGraceMaxSeconds] so a 0/negative input
			// can't disable the grace by collapsing it and an over-large one can't
			// pretend to exceed permTimeout(). The spawner re-clamps against the
			// live permTimeout() at run time, but a sane band here keeps the
			// persisted value honest and matches the UI slider's range.
			secs := *req.PermissionAbsentGraceSeconds
			if secs < delegate.AbsentGraceMinSeconds {
				secs = delegate.AbsentGraceMinSeconds
			}
			if secs > delegate.AbsentGraceMaxSeconds {
				secs = delegate.AbsentGraceMaxSeconds
			}
			teamSet.PermissionAbsentGraceMS = secs * 1000
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
			s.MarkJiraRestarted(r.Context(), orgID)
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
	// GitHubPATLogin is the @login the org's stored bot PAT authenticates as —
	// the credential's own identity, not the caller's. Not a secret (it's the
	// account name that shows up as the commit author on delegated work), and
	// it's the context that makes replacing the token from Settings feel safe:
	// you can see which account you're about to swap out. Empty (omitted) when
	// no PAT is bound, when the bind predates the login being recorded (it
	// self-heals on the next bind), or when the live token comes from the
	// environment — see GitHubPATEnvProvided.
	GitHubPATLogin string `json:"github_pat_login,omitempty"`
	// GitHubPATEnvProvided reports that the token TF actually authenticates
	// with is supplied by TRIAGE_FACTORY_GITHUB_BOT_PAT, not by the vault.
	// Local mode only (there is no env overlay in multi).
	//
	// The overlay is read-only and read-wins: a write lands in the keychain but
	// every subsequent read still returns the env value. So a credential the
	// environment supplies can be seen but not managed here, and a UI that
	// offered to replace it would be promising something it cannot deliver —
	// the operator would rotate, get a success, and keep polling with the old
	// token. Surfaces render this as settled rather than editable.
	GitHubPATEnvProvided bool   `json:"github_pat_env_provided,omitempty"`
	JiraBaseURL          string `json:"jira_base_url"`
	JiraPollInterval     string `json:"jira_poll_interval"`
	// HasJiraCredential reports whether a usable Jira service credential is
	// stored for the org's auth-method marker — a Data Center PAT or a Cloud
	// email + API token — rather than the presence of a specific key, so a
	// Cloud org (which has no PAT) still reports true.
	HasJiraCredential bool `json:"has_jira_credential"`
	// JiraCredentialEnvProvided is the Jira half of GitHubPATEnvProvided, and
	// covers the URL as well as the token: the resolver reads BOTH from the
	// overlaid secret, so an env-supplied host makes a rebind partly ineffective
	// even for a Cloud org whose email + API token aren't shadowed at all.
	// Either half being env-supplied is enough to make "replace this credential"
	// a promise Settings can't keep. Local mode only.
	JiraCredentialEnvProvided bool   `json:"jira_credential_env_provided,omitempty"`
	MaxLLMModelTier           string `json:"max_llm_model_tier,omitempty"`
	// MaxDailyCostUSD is the org-wide daily LLM spend cap (TFAC-477); 0 = no
	// cap. Always emitted (not omitempty) so the Settings form can render the
	// numeric input's current value, including an explicit "0 / no cap".
	MaxDailyCostUSD float64 `json:"max_daily_cost_usd"`
	// MaxConcurrentRuns is the org-wide concurrent-run ceiling; 0 = unlimited.
	// Always emitted (not omitempty) for the same reason as MaxDailyCostUSD —
	// the form renders the numeric input's current value, "0 / unlimited"
	// included.
	MaxConcurrentRuns  int  `json:"max_concurrent_runs"`
	HasAnthropicAPIKey bool `json:"has_anthropic_api_key"`
	HasBedrockCreds    bool `json:"has_bedrock_credentials"`
	// Bedrock non-secret config (TFAC-68). The credential itself never
	// leaves the vault — presence rides has_bedrock_credentials and the
	// method marker below; these three let the Settings form render the
	// current region / model / endpoint without a secrets round-trip.
	BedrockAuthMethod string `json:"bedrock_auth_method,omitempty"` // "role" | "bearer" | "access_keys"
	BedrockRegion     string `json:"bedrock_region,omitempty"`
	BedrockModelID    string `json:"bedrock_model_id,omitempty"`
	BedrockBaseURL    string `json:"bedrock_base_url,omitempty"`
	// Role-mode (TFAC-616) non-secret config: the customer role ARN and the
	// TF-generated External ID, so the settings form re-renders the role card
	// and the copyable trust-policy snippet without a round-trip to the
	// role-setup endpoint. Both empty unless the org is in role mode.
	BedrockRoleARN    string `json:"bedrock_role_arn,omitempty"`
	BedrockExternalID string `json:"bedrock_external_id,omitempty"`
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
	var ghPATLogin string
	var bedrockRegion, bedrockModelID, bedrockBaseURL, bedrockRoleARN, bedrockExternalID string

	// Which credentials the environment supplies, and therefore which ones this
	// deployment can only report rather than manage. Multi mode has no overlay,
	// so the question is local-only and both flags are false there.
	local := runmode.Current() == runmode.ModeLocal
	ghPATEnv := local && auth.EnvProvidesKey(integrations.KeyGitHubPAT)
	jiraCredEnv := local &&
		(auth.EnvProvidesKey(integrations.KeyJiraPAT) || auth.EnvProvidesKey(integrations.KeyJiraURL))

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var err error
		orgSet, err = tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return err
		}
		creds, _ = integrations.Load(r.Context(), tx.Secrets, orgID)
		// The login the org PAT authenticates as, recorded on the agents row by
		// every PAT bind. Only meaningful while the BOUND PAT is the credential —
		// an App org's bot login (<slug>[bot]) resolves live from the
		// registration, and an env-overlaid org authenticates as whoever the env
		// token belongs to while the agents row still describes the last token
		// bound through a route. Neither describes the live credential, and a
		// name that names the wrong account is worse than no name on a surface
		// whose whole job is "here's what you're about to replace". Best-effort
		// like the Bedrock reads below: a read failure degrades the form to
		// "connected" without a name, not a 500.
		if creds.GitHubPAT != "" && !ghPATEnv {
			if agent, aerr := tx.Agents.GetForOrg(r.Context(), orgID); aerr == nil && agent != nil {
				ghPATLogin = agent.GitHubOrgLogin
			}
		}
		// Bedrock non-secret config rides the same vault as the
		// credential; missing keys come back ("", nil). Best-effort like
		// the integrations.Load above — a vault hiccup degrades the form
		// to blank fields, not a 500. The role ARN + External ID are
		// non-secret too (role mode stores no credential at all).
		bedrockRegion, _ = tx.Secrets.Get(r.Context(), orgID, integrations.KeyAWSRegion)
		bedrockModelID, _ = tx.Secrets.Get(r.Context(), orgID, integrations.KeyBedrockModelID)
		bedrockBaseURL, _ = tx.Secrets.Get(r.Context(), orgID, integrations.KeyBedrockBaseURL)
		bedrockRoleARN, _ = tx.Secrets.Get(r.Context(), orgID, integrations.KeyAWSRoleARN)
		bedrockExternalID, _ = tx.Secrets.Get(r.Context(), orgID, integrations.KeyAWSExternalID)
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
		GitHubBaseURL:             ghBaseURL,
		GitHubPollInterval:        orgSet.GitHubPollInterval.String(),
		GitHubCloneProtocol:       defaultedCloneProtocolView(orgSet.GitHubCloneProtocol),
		HasGitHubPAT:              creds.GitHubPAT != "",
		GitHubPATLogin:            ghPATLogin,
		GitHubPATEnvProvided:      ghPATEnv,
		JiraBaseURL:               jiraBaseURL,
		JiraPollInterval:          orgSet.JiraPollInterval.String(),
		HasJiraCredential:         hasJiraCred,
		JiraCredentialEnvProvided: jiraCredEnv,
		MaxLLMModelTier:           orgSet.MaxLLMModelTier,
		MaxDailyCostUSD:           orgSet.MaxDailyCostUSD,
		MaxConcurrentRuns:         orgSet.MaxConcurrentRuns,
		HasAnthropicAPIKey:        orgSet.AnthropicAPIKeyRef != "",
		HasBedrockCreds:           orgSet.BedrockCredentialsRef != "",
		BedrockAuthMethod:         bedrockAuthMethodFromRef(orgSet.BedrockCredentialsRef),
		BedrockRegion:             bedrockRegion,
		BedrockModelID:            bedrockModelID,
		BedrockBaseURL:            bedrockBaseURL,
		BedrockRoleARN:            bedrockRoleARN,
		BedrockExternalID:         bedrockExternalID,
		MemberCount:               memberCount,
	})
}

// orgSettingsUpdate is the body of POST /api/settings/org — the org's PURE
// CONFIG. No secrets: the GitHub PAT and the Jira service credential each live
// on their own resource (PUT/DELETE /api/orgs/{org_id}/github/access/pat and
// .../jira/access/credential, see org_credentials.go), so this route touches no
// vault key, makes no outbound call, and cannot revoke access as a side effect.
// The Anthropic key and the Bedrock set are likewise writable only through
// their own validated capture endpoints. A stray credential field in the
// request body is ignored by the decoder, not stored.
//
// Every field is a pointer with ONE meaning: nil = leave it alone, present =
// apply (including the zero value, which clears). The route used to mix three
// different "unset" encodings — nil-vs-empty for some fields, empty-means-
// untouched for others, zero-means-clear for the caps — which among other
// things left the poll intervals with no way to be cleared at all.
type orgSettingsUpdate struct {
	GitHubBaseURL       *string `json:"github_base_url"`
	GitHubPollInterval  *string `json:"github_poll_interval"`
	GitHubCloneProtocol *string `json:"github_clone_protocol"`
	JiraBaseURL         *string `json:"jira_base_url"`
	JiraPollInterval    *string `json:"jira_poll_interval"`
	MaxLLMModelTier     *string `json:"max_llm_model_tier"`
	// MaxDailyCostUSD is the org-wide daily LLM spend cap; 0 clears it.
	MaxDailyCostUSD *float64 `json:"max_daily_cost_usd"`
	// MaxConcurrentRuns is the org-wide concurrent-run ceiling; 0 clears it
	// back to unlimited.
	MaxConcurrentRuns *int `json:"max_concurrent_runs"`
}

// handleOrgSettingsPost saves the org's configuration. Org-admin only.
//
// Base URLs stay here rather than moving onto the credential resources: the
// GitHub App path has to set a host with no credential in sight (the manifest
// is built against the stored host, before any App exists), so the host is
// genuinely config. Clearing one no longer destroys the matching credential the
// way it used to — disconnecting is an explicit DELETE on the credential.
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

	var prevOrgSet domain.OrgSettings
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var err error
		prevOrgSet, err = tx.Orgs.GetSettings(r.Context(), orgID)
		return err
	}); err != nil {
		internalError(w, "settings/org", err)
		return
	}

	orgSet := prevOrgSet

	if req.GitHubBaseURL != nil {
		// Blanking the host while an App registration exists is REFUSED. The
		// resolver's base lookup falls org_settings → the github_url secret →
		// github.com, so an empty column silently re-points a GHES org's App at
		// github.com: wrong host, no error, nothing in any log.
		//
		// Refused rather than skipped, which is where this differs from the PAT
		// unbind's identical hazard. There, clearing the host is a side effect of
		// unbinding a token, so quietly keeping it is right. Here the clear IS
		// the request, and answering "saved" for work we declined to do is the
		// parse-and-drop bug in another costume.
		//
		// Re-targeting to a different NON-empty host stays allowed: that's a real
		// move during a GHES domain change, and whatever breaks is at least the
		// value the admin typed rather than a default they never chose.
		//
		// GitHub-only: jira.CanonicalHost returns ok=false on a blank base URL,
		// so the Jira surfaces fail loudly instead of resolving somewhere wrong.
		if *req.GitHubBaseURL == "" {
			app, err := s.githubApps.GetForOrgSystem(r.Context(), orgID)
			if err != nil {
				internalError(w, "settings/org", err)
				return
			}
			if app != nil {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "this workspace's GitHub App is registered against this host — remove the App before clearing it",
					"field": "github_base_url",
				})
				return
			}
		}
		orgSet.GitHubBaseURL = *req.GitHubBaseURL
	}
	if req.JiraBaseURL != nil {
		orgSet.JiraBaseURL = *req.JiraBaseURL
	}
	// A malformed duration is rejected rather than silently ignored — the old
	// parse-and-drop behavior meant a typo'd interval reported "saved" while
	// keeping the previous value.
	if req.GitHubPollInterval != nil {
		d, err := parseOrgPollInterval(*req.GitHubPollInterval, domain.DefaultOrgSettings().GitHubPollInterval)
		if err != nil {
			badRequestField(w, err.Error(), "github_poll_interval")
			return
		}
		orgSet.GitHubPollInterval = d
	}
	if req.JiraPollInterval != nil {
		d, err := parseOrgPollInterval(*req.JiraPollInterval, domain.DefaultOrgSettings().JiraPollInterval)
		if err != nil {
			badRequestField(w, err.Error(), "jira_poll_interval")
			return
		}
		orgSet.JiraPollInterval = d
	}
	if req.GitHubCloneProtocol != nil {
		proto := *req.GitHubCloneProtocol
		if proto != "ssh" && proto != "https" {
			badRequest(w, "github_clone_protocol must be 'ssh' or 'https'")
			return
		}
		// Multi-mode is HTTPS-only: refuse an ssh write rather than persist a
		// value the effective resolver (and the clone path) will ignore. The UI
		// hides the control in multi mode; this rejects a direct API call.
		if proto == "ssh" && runmode.Current() != runmode.ModeLocal {
			badRequest(w, "ssh clone protocol is not available in this deployment; use https")
			return
		}
		orgSet.GitHubCloneProtocol = proto
	}
	if req.MaxLLMModelTier != nil {
		tier := *req.MaxLLMModelTier
		if tier != "" && domain.ParseTier(tier) == domain.TierUnknown {
			badRequest(w, "max_llm_model_tier must be haiku, sonnet, or opus")
			return
		}
		orgSet.MaxLLMModelTier = tier
	}
	// Reject a negative cap — it would either block every run (if the trip is
	// >=) or be silently inert, neither a meaningful input.
	if req.MaxDailyCostUSD != nil {
		if *req.MaxDailyCostUSD < 0 {
			badRequest(w, "max_daily_cost_usd must be >= 0")
			return
		}
		orgSet.MaxDailyCostUSD = *req.MaxDailyCostUSD
	}
	// A negative concurrency limit reads as "unlimited" to the claim path, so
	// it can't mean anything; the ceiling keeps a validated value inside the
	// Postgres int4 column rather than 500ing on "integer out of range".
	if req.MaxConcurrentRuns != nil {
		if *req.MaxConcurrentRuns < 0 {
			badRequest(w, "max_concurrent_runs must be >= 0")
			return
		}
		if *req.MaxConcurrentRuns > domain.MaxConcurrentRunsCeiling {
			badRequest(w, fmt.Sprintf("max_concurrent_runs must be at most %d", domain.MaxConcurrentRunsCeiling))
			return
		}
		orgSet.MaxConcurrentRuns = *req.MaxConcurrentRuns
	}

	// SSH preflight: gate the transition INTO SSH mode. Local-mode only —
	// PreflightSSH writes the container's ~/.ssh/known_hosts and probes the
	// operator's ssh-agent, neither of which belongs in a hosted runtime. In
	// multi mode the ssh write is already rejected above, so the explicit mode
	// gate makes the no-SSH-in-multi guarantee provable at this call site too.
	if runmode.Current() == runmode.ModeLocal &&
		orgSet.GitHubCloneProtocol == "ssh" && prevOrgSet.GitHubCloneProtocol != "ssh" {
		sshHost := worktree.SSHHostFromBaseURL(orgSet.GitHubBaseURL)
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		err := worktree.PreflightSSH(ctx, sshHost)
		cancel()
		if err != nil {
			settingsOrgLog.Warn("blocked ssh switch", "ssh_host", sshHost, "error", err)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error":  fmt.Sprintf("SSH preflight against %s failed — fix your SSH setup or keep HTTPS. %s", sshHost, err.Error()),
				"field":  "github_clone_protocol",
				"stderr": err.Error(),
			})
			return
		}
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet)
	}); err != nil {
		internalError(w, "settings/org", err)
		return
	}

	// Re-due polling only for what this route can still change. Credential
	// rotations kick their own restart from the credential routes.
	ghChanged := orgSet.GitHubBaseURL != prevOrgSet.GitHubBaseURL ||
		orgSet.GitHubPollInterval != prevOrgSet.GitHubPollInterval ||
		orgSet.GitHubCloneProtocol != prevOrgSet.GitHubCloneProtocol
	jiraChanged := orgSet.JiraBaseURL != prevOrgSet.JiraBaseURL ||
		orgSet.JiraPollInterval != prevOrgSet.JiraPollInterval

	if ghChanged && s.onGitHubChanged != nil {
		s.MarkJiraRestarted(r.Context(), orgID)
		go s.onGitHubChanged(orgID)
	} else if jiraChanged && s.onJiraChanged != nil {
		s.MarkJiraRestarted(r.Context(), orgID)
		go s.onJiraChanged(orgID)
	}

	resp := map[string]string{"status": "saved"}
	// Lowering the cap doesn't block the save — the admin has authority — but if
	// the default team already prefers a higher tier, surface that its effective
	// model just dropped. Gate on an actual cap change: the frontend re-sends
	// max_llm_model_tier on every org save, so without this an unrelated save
	// would re-warn each time the default team sits above an unchanged cap.
	// Single-team-per-org today, so we check the default team; broadens to a
	// team list when multi-team lands.
	if orgSet.MaxLLMModelTier != "" && orgSet.MaxLLMModelTier != prevOrgSet.MaxLLMModelTier {
		if w := s.capDowngradeWarning(r.Context(), orgID, userID, orgSet.MaxLLMModelTier); w != "" {
			resp["warning"] = w
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseOrgPollInterval resolves a poll-interval field: an empty string clears
// the override back to that poller's shipped default, any other value must
// parse as a duration of at least the floor. `def` is the caller's own default
// so the two pollers can diverge without this helper picking a side.
func parseOrgPollInterval(raw string, def time.Duration) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return def, nil
	}
	d, err := parseMinDuration(raw, orgPollIntervalMinMinutes)
	if err != nil {
		return 0, fmt.Errorf("poll interval must be a duration of at least %dm (e.g. \"15m\")", orgPollIntervalMinMinutes)
	}
	return d, nil
}

// orgPollIntervalMinMinutes is the floor for an org poll interval. Anything
// tighter risks GitHub/Jira rate limits across a fleet of orgs.
const orgPollIntervalMinMinutes = 10

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
