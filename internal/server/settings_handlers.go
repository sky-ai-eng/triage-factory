package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/modelaccess"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// --------------------------------------------------------------------
// /api/me/settings — the caller's own settings row, GET and PATCH.
//
// Under /api/me because the subject is the caller: the handlers read and
// write user_settings for the session principal, and no caller may address
// another's, so there is no id to put in the path.
//
// The response carries the settings row alone. A user's GitHub / Jira
// identities are the same rows GET /api/orgs/{org_id}/{github,jira}/identity
// answers — one fact belongs in one place, and the identity reads are the
// place, since they are the ones that also say which host it is keyed under.
// --------------------------------------------------------------------

type userSettingsResponse struct {
	UserSettings domain.UserSettings `json:"user_settings"`
}

// handleMeSettingsGet answers the caller's settings resource.
//
// GET /api/me/settings
func (s *Server) handleMeSettingsGet(w http.ResponseWriter, r *http.Request) {
	userID := ClaimsFrom(r.Context()).Subject
	orgID := OrgIDFrom(r.Context())

	resp, ok := s.readUserSettings(w, r, orgID, userID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// readUserSettings reads the settings resource, shared by the GET and the
// PATCH's read-back so the write answers exactly what a follow-up read would.
func (s *Server) readUserSettings(w http.ResponseWriter, r *http.Request, orgID, userID string) (userSettingsResponse, bool) {
	var resp userSettingsResponse
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		settings, err := tx.Users.GetSettings(r.Context(), userID)
		if err != nil {
			return fmt.Errorf("user settings: %w", err)
		}
		resp.UserSettings = settings
		return nil
	}); err != nil {
		internalError(w, "me/settings", err)
		return resp, false
	}
	return resp, true
}

// userSettingsPatch is the PATCH body. user_settings absent keeps the stored
// row; present replaces the fields it carries. v1's domain.UserSettings has no
// fields yet, so strict decoding makes any key inside it a named 400 rather
// than a value quietly dropped.
type userSettingsPatch struct {
	UserSettings *domain.UserSettings `json:"user_settings,omitempty"`
}

// handleMeSettingsPatch applies a partial update to the caller's settings row
// and answers the settings resource as a follow-up GET would return it — so a
// body that describes no change answers the state, not the word "saved".
//
// PATCH /api/me/settings
func (s *Server) handleMeSettingsPatch(w http.ResponseWriter, r *http.Request) {
	userID := ClaimsFrom(r.Context()).Subject
	orgID := OrgIDFrom(r.Context())
	var req userSettingsPatch
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	if req.UserSettings != nil {
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			return tx.Users.UpdateSettings(r.Context(), userID, *req.UserSettings)
		}); err != nil {
			internalError(w, "me/settings", err)
			return
		}
	}

	// Read back rather than echo the request: UpdateSettings is exempt from the
	// returned-row rule while domain.UserSettings is empty (see UsersStore), so
	// there is no persisted row to hand back from the write itself.
	resp, ok := s.readUserSettings(w, r, orgID, userID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// --------------------------------------------------------------------
// /api/teams/{team_id}/settings — team members (GET), team admin (PATCH)
//
// It lives under the team resource rather than at /api/settings/team/{id}
// because the path segment is what the authorization check is about: the
// caller names a team and the handler authorizes them against it. The
// segment takes the one team grammar (a uuid, or the literal "default" in
// local mode) through authz.TeamIDFromPath, like every other {team_id} route.
//
// The Jira project rules are NOT part of the PATCH body. They're a child
// collection with their own replace-set write (PUT
// /api/teams/{team_id}/jira-projects), matching the tracked-repo and
// github-group siblings; they still ride the composite GET, which is a
// deliberate convenience for the settings page rather than an oversight.
// --------------------------------------------------------------------

type teamSettingsResponse struct {
	TeamSettings domain.TeamSettings   `json:"team_settings"`
	JiraProjects []jiraProjectSettings `json:"jira_projects"`
	// Warning is advisory prose about a save that SUCCEEDED — today, that the
	// org's model cap clamps the default the team just picked. Only the PATCH
	// response ever carries it; the GET leaves it empty and omitempty drops it,
	// so the read and the write answer one shape.
	Warning string `json:"warning,omitempty"`
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
	teamID, ok := s.az.TeamIDFromPath(w, r, "settings/team", orgID, userID)
	if !ok {
		return
	}

	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}

	resp, ok := s.readTeamSettings(w, r, orgID, userID, teamID, nil)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// readTeamSettings assembles the team-settings resource: the settings row, the
// team's Jira project rules, and the caller-relative annotations the settings
// page collapses its layout on. Shared by the GET and by the PATCH's read-back,
// so the write answers with exactly what a follow-up read would return.
// known is the settings row a caller already holds — the one a save's write
// just returned. When non-nil this skips the settings read and composes the
// rest of the response around it, so a PATCH never re-reads the row its own
// write produced. The GET route passes nil.
func (s *Server) readTeamSettings(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string, known *domain.TeamSettings) (teamSettingsResponse, bool) {
	var resp teamSettingsResponse
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var settings domain.TeamSettings
		if known != nil {
			settings = *known
		} else {
			var err error
			if settings, err = tx.Teams.GetSettings(r.Context(), teamID); err != nil {
				return fmt.Errorf("team settings: %w", err)
			}
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
		return resp, false
	}

	if resp.JiraProjects == nil {
		resp.JiraProjects = []jiraProjectSettings{}
	}

	count, role, err := s.az.TeamMemberCountAndRole(r.Context(), orgID, userID, teamID)
	if err != nil {
		internalError(w, "settings/team", err)
		return resp, false
	}
	resp.MemberCount = count
	resp.Role = role
	resp.PermissionAbsentGraceMinSeconds = delegate.AbsentGraceMinSeconds
	resp.PermissionAbsentGraceMaxSeconds = delegate.AbsentGraceMaxSeconds
	return resp, true
}

// teamSettingsPatch is the body of PATCH /api/teams/{team_id}/settings.
//
// Every field is json.RawMessage under ONE clearing convention: absent keeps
// the stored value, an explicit null clears it (ai_model) or resets it to the
// shipped default (everything else), and any other value is applied. The route
// used to speak three conventions at once — pointer-nil-keeps for most fields,
// ""-keeps for ai_model, present-but-empty-resets for the three string enums —
// so "how do I clear this" had a different answer per field, and for ai_model
// no answer at all.
//
// jira_projects is deliberately absent. The team's Jira rules are a child
// collection with their own replace-set write (PUT
// /api/teams/{team_id}/jira-projects), like the tracked repos and the
// github-group mappings; strict decoding turns a stale caller that still sends
// them here into a 400 naming the field rather than a silent half-save.
type teamSettingsPatch struct {
	AIModel                         json.RawMessage `json:"ai_model"`
	AIAutoDelegate                  json.RawMessage `json:"ai_auto_delegate_enabled"`
	AutoModeEnabled                 json.RawMessage `json:"auto_mode_enabled"`
	AIReprioritizeThreshold         json.RawMessage `json:"ai_reprioritize_threshold"`
	AIPreferenceUpdateInterval      json.RawMessage `json:"ai_preference_update_interval"`
	BranchTemplate                  json.RawMessage `json:"branch_template"`
	ReviewPosture                   json.RawMessage `json:"review_posture"`
	BaseBranchPushPolicy            json.RawMessage `json:"base_branch_push_policy"`
	PermissionAbsentAutodenyEnabled json.RawMessage `json:"permission_absent_autodeny_enabled"`
	PermissionAbsentGraceSeconds    json.RawMessage `json:"permission_absent_grace_seconds"`
}

// handleTeamSettingsPatch applies a partial update to the team's settings row.
// Team admin, non-archived team. Answers with the settings resource as a
// follow-up GET would return it, so a client's post-save state is exact.
//
// PATCH /api/teams/{team_id}/settings
func (s *Server) handleTeamSettingsPatch(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, ok := s.az.TeamIDFromPath(w, r, "settings/team", orgID, userID)
	if !ok {
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

	var req teamSettingsPatch
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	apply, ok := resolveTeamSettingsPatch(w, req)
	if !ok {
		return
	}

	var (
		prevModel  string
		savedModel string
		orgMaxTier string
		saved      domain.TeamSettings
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamSet, err := tx.Teams.GetSettings(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("load team settings: %w", err)
		}
		prevModel = teamSet.DefaultModel
		// The org row feeds two different things, and they tolerate a read
		// failure differently. The max-tier cap only renders an advisory line
		// after the save, so a failed read there costs a sentence. The provider
		// check below decides whether the write happens at all, so the same
		// failure has to stop it — see the abort inside.
		orgSet, orgErr := tx.Orgs.GetSettings(r.Context(), orgID)
		if orgErr == nil {
			orgMaxTier = orgSet.MaxLLMModelTier
		}
		apply(&teamSet)
		savedModel = teamSet.DefaultModel
		// A default this team's next run would refuse is not a default worth
		// storing, so the provider check happens here — inside the transaction,
		// where both the org's connected providers and the team's restriction
		// are already loaded. Only on a change: a save that re-sends a model
		// stored before a credential was disconnected must not be blocked by
		// something this caller did not do (that one is the dispatch's to
		// refuse).
		if savedModel != "" && savedModel != prevModel {
			// A failed read is the check's input missing, not the check
			// passing. Saving through it would persist a default the next
			// dispatch refuses, and the caller would be told the save
			// succeeded — so the write is abandoned and the caller retries.
			// A missing org_settings row is not this case: the store answers
			// that with the schema defaults, which resolve to an org holding
			// no credential and therefore refuse nothing.
			if orgErr != nil {
				return fmt.Errorf("load org settings: %w", orgErr)
			}
			if e := modelaccess.ForOrg(orgSet).Check(savedModel, teamSet.AllowedProviders); e != nil {
				return e
			}
		}
		if saved, err = tx.Teams.UpdateSettings(r.Context(), teamID, teamSet); err != nil {
			return fmt.Errorf("save team settings: %w", err)
		}
		return nil
	}); err != nil {
		if writeModelAccessError(w, err, "ai_model") {
			return
		}
		internalError(w, "settings/team", err)
		return
	}

	resp, ok := s.readTeamSettings(w, r, orgID, userID, teamID, &saved)
	if !ok {
		return
	}
	// The team default doesn't override the org cap. If a newly-picked default
	// exceeds it, accept the save (the team owns its preference) but say that
	// the effective model is the org's cap. Gated on an actual change so a save
	// that re-sends the current model doesn't re-warn every time.
	if savedModel != "" && savedModel != prevModel {
		if eff, source := domain.EffectiveModel(savedModel, orgMaxTier); source == "org-cap" {
			resp.Warning = fmt.Sprintf(
				"Team default of %s exceeds the org cap of %s. Effective model is %s.",
				savedModel, orgMaxTier, eff,
			)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveTeamSettingsPatch validates the body ONCE and returns the mutation it
// describes, so the transaction below applies a decided plan rather than
// re-deriving it against a row (and re-reporting the same faults).
//
// The two validation passes carry two statuses because they are two different
// faults. Shape and vocabulary are protocol-level — "you didn't say it right" —
// and answer 400; a well-formed number outside the band its field honors is
// semantic — "you said it right but it can't be done to this data" — and
// answers 422. Each pass accumulates every failure in its class before
// flushing, so a body with three bad fields reports three.
//
// On any failure the response is already written and ok is false.
func resolveTeamSettingsPatch(w http.ResponseWriter, req teamSettingsPatch) (apply func(*domain.TeamSettings), ok bool) {
	if !httpx.PatchNamed(
		req.AIModel, req.AIAutoDelegate, req.AutoModeEnabled, req.AIReprioritizeThreshold,
		req.AIPreferenceUpdateInterval, req.BranchTemplate, req.ReviewPosture,
		req.BaseBranchPushPolicy, req.PermissionAbsentAutodenyEnabled,
		req.PermissionAbsentGraceSeconds,
	) {
		// A PATCH that names no field wrote nothing, so it must not answer as
		// though it did — the one response a client can't tell from a real save.
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no fields to update: name at least one settings field (null clears it)",
		})
		return nil, false
	}

	var (
		shape    httpx.Validation
		ranges   httpx.Validation
		mutators []func(*domain.TeamSettings)
		defaults = domain.DefaultTeamSettings()
	)
	set := func(f func(*domain.TeamSettings)) { mutators = append(mutators, f) }

	// ai_model must name a model this deployment offers. The picker draws its
	// options from the same catalog, so the closed set is the one the UI shows
	// — and a stored value that is dispatched verbatim has no room for a
	// spelling nothing can invoke. Null clears the team's preference.
	if v, st := httpx.PatchString(&shape, req.AIModel, "ai_model"); st != httpx.PatchAbsent {
		switch {
		case st == httpx.PatchClear:
			set(func(t *domain.TeamSettings) { t.DefaultModel = "" })
		case st == httpx.PatchSet && strings.TrimSpace(v) == "":
			shape.Invalid("ai_model", "ai_model must name a model, or be null to inherit the org default")
		case st == httpx.PatchSet && !modelcatalog.Offers(strings.TrimSpace(v)):
			shape.Invalid("ai_model", "ai_model must name a model this deployment offers: "+strings.Join(modelcatalog.Keys(), ", "))
		case st == httpx.PatchSet:
			set(func(t *domain.TeamSettings) { t.DefaultModel = strings.TrimSpace(v) })
		}
	}
	if v, st := httpx.PatchBool(&shape, req.AIAutoDelegate, "ai_auto_delegate_enabled"); st != httpx.PatchAbsent {
		next := defaults.AutoDelegateEnabled
		if st == httpx.PatchSet {
			next = v
		}
		set(func(t *domain.TeamSettings) { t.AutoDelegateEnabled = next })
	}
	if v, st := httpx.PatchBool(&shape, req.AutoModeEnabled, "auto_mode_enabled"); st != httpx.PatchAbsent {
		next := defaults.AutoModeEnabled
		if st == httpx.PatchSet {
			next = v
		}
		set(func(t *domain.TeamSettings) { t.AutoModeEnabled = next })
	}
	if v, st := httpx.PatchInt(&shape, req.AIReprioritizeThreshold, "ai_reprioritize_threshold"); st != httpx.PatchAbsent {
		next := defaults.AIReprioritizeThreshold
		if st == httpx.PatchSet {
			if v <= 0 {
				ranges.OutOfRange("ai_reprioritize_threshold", "ai_reprioritize_threshold must be greater than 0")
			}
			next = v
		}
		set(func(t *domain.TeamSettings) { t.AIReprioritizeThreshold = next })
	}
	if v, st := httpx.PatchInt(&shape, req.AIPreferenceUpdateInterval, "ai_preference_update_interval"); st != httpx.PatchAbsent {
		next := defaults.AIPreferenceUpdateInterval
		if st == httpx.PatchSet {
			if v <= 0 {
				ranges.OutOfRange("ai_preference_update_interval", "ai_preference_update_interval must be greater than 0")
			}
			next = v
		}
		set(func(t *domain.TeamSettings) { t.AIPreferenceUpdateInterval = next })
	}
	// The literal "<ticket-id>" stays verbatim — it's substituted at
	// prompt-render time, not here.
	if v, st := httpx.PatchString(&shape, req.BranchTemplate, "branch_template"); st != httpx.PatchAbsent {
		next := defaults.BranchTemplate
		if st == httpx.PatchSet {
			if strings.TrimSpace(v) == "" {
				shape.Invalid("branch_template", "branch_template must not be blank; send null to reset it to the default")
			}
			next = v
		}
		set(func(t *domain.TeamSettings) { t.BranchTemplate = next })
	}
	// An unrecognized posture would silently degrade to "stage everything" at
	// finalize time — exactly the misconfiguration a team switching to auto
	// would never notice.
	if v, st := httpx.PatchString(&shape, req.ReviewPosture, "review_posture"); st != httpx.PatchAbsent {
		next := defaults.ReviewPosture
		if st == httpx.PatchSet {
			if !domain.ValidReviewPosture(v) {
				shape.Invalid("review_posture", "review_posture must be one of: "+strings.Join(domain.ValidReviewPostures, ", "))
			}
			next = v
		}
		set(func(t *domain.TeamSettings) { t.ReviewPosture = next })
	}
	// pushpolicy reads anything it doesn't recognize as the strictest policy, so
	// a typo would silently look like "never" forever.
	if v, st := httpx.PatchString(&shape, req.BaseBranchPushPolicy, "base_branch_push_policy"); st != httpx.PatchAbsent {
		next := defaults.BaseBranchPushPolicy
		if st == httpx.PatchSet {
			if !domain.ValidBaseBranchPushPolicy(v) {
				shape.Invalid("base_branch_push_policy", "base_branch_push_policy must be one of: "+strings.Join(domain.ValidBaseBranchPushPolicies, ", "))
			}
			next = v
		}
		set(func(t *domain.TeamSettings) { t.BaseBranchPushPolicy = next })
	}
	if v, st := httpx.PatchBool(&shape, req.PermissionAbsentAutodenyEnabled, "permission_absent_autodeny_enabled"); st != httpx.PatchAbsent {
		next := defaults.PermissionAbsentAutodenyEnabled
		if st == httpx.PatchSet {
			next = v
		}
		set(func(t *domain.TeamSettings) { t.PermissionAbsentAutodenyEnabled = next })
	}
	// Seconds on the wire (the UI input is seconds); stored as ms. Outside the
	// band this route advertises on its GET the value is REJECTED, not clamped:
	// a 0 would collapse the grace and an over-large one would pretend to exceed
	// permTimeout(), and a clamp answers "saved" for a value the caller never
	// asked for. The spawner still re-clamps against the live permTimeout().
	if v, st := httpx.PatchInt(&shape, req.PermissionAbsentGraceSeconds, "permission_absent_grace_seconds"); st != httpx.PatchAbsent {
		next := defaults.PermissionAbsentGraceMS
		if st == httpx.PatchSet {
			if v < delegate.AbsentGraceMinSeconds || v > delegate.AbsentGraceMaxSeconds {
				ranges.OutOfRange("permission_absent_grace_seconds", fmt.Sprintf(
					"permission_absent_grace_seconds must be between %d and %d",
					delegate.AbsentGraceMinSeconds, delegate.AbsentGraceMaxSeconds))
			}
			next = v * 1000
		}
		set(func(t *domain.TeamSettings) { t.PermissionAbsentGraceMS = next })
	}

	if shape.Flush(w, http.StatusBadRequest) {
		return nil, false
	}
	if ranges.Flush(w, http.StatusUnprocessableEntity) {
		return nil, false
	}
	return func(t *domain.TeamSettings) {
		for _, m := range mutators {
			m(t)
		}
	}, true
}

// --------------------------------------------------------------------
// /api/orgs/{org_id}/settings — org members (GET), org admin (PATCH)
//
// Path-scoped rather than session-scoped because the write is admin-gated:
// the caller asserts a scope and the handler authorizes them against the id in
// the path, which is what the authorization check has to be about. The read
// sits on the same resource so there is one address for the settings, not one
// per verb.
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
	// BackgroundJobsModel is the model the scorer and repo
	// profiler run on — a catalog key. Always emitted (not omitempty): "" is
	// the org's "not picked yet", which is the state the settings form has to
	// render, and an omitted field would read to a client as "unchanged".
	BackgroundJobsModel string `json:"background_jobs_model"`
	// MaxDailyCostUSD is the org-wide daily LLM spend cap (TFAC-477); 0 = no
	// cap. Always emitted (not omitempty) so the Settings form can render the
	// numeric input's current value, including an explicit "0 / no cap".
	MaxDailyCostUSD float64 `json:"max_daily_cost_usd"`
	// MaxConcurrentRuns is the org-wide concurrent-run ceiling; 0 = unlimited.
	// Always emitted (not omitempty) for the same reason as MaxDailyCostUSD —
	// the form renders the numeric input's current value, "0 / unlimited"
	// included.
	MaxConcurrentRuns int `json:"max_concurrent_runs"`
	// LLMAuthMethod is where this org's Claude credentials come from:
	// "system" (the host's, resolved by the SDK from the environment the agent
	// subprocess inherits) or "byok" (the org's own bound material). The
	// EFFECTIVE value, so a multi-mode deployment always reads "byok" — the
	// client never has to know that "system" is local-only to render the right
	// control, which is the mode difference travelling as data.
	//
	// Read alongside has_anthropic_api_key / has_bedrock_credentials, never
	// derived from them: those say which providers are bound, this says what it
	// means that none are. Always emitted — a client rendering the source
	// picker has to know the current selection, and an omitted field would read
	// as "unchanged".
	LLMAuthMethod      string `json:"llm_auth_method"`
	HasAnthropicAPIKey bool   `json:"has_anthropic_api_key"`
	HasBedrockCreds    bool   `json:"has_bedrock_credentials"`
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
	// Version is the org_settings row's optimistic-concurrency token. The read
	// hands it out and the PATCH requires it back: the settings save is a
	// read-modify-write over org_settings' own columns, so without it two
	// admins editing different sections silently undo each other. A PATCH
	// carrying a stale token is refused with 409 VERSION_CONFLICT and the
	// loser refetches — this field is what the refetch is FOR, so it is not
	// omitempty.
	//
	// It does NOT cover github_base_url / github_poll_interval / jira_base_url
	// / jira_poll_interval: those live on org_event_sources now, and this
	// route writes them in the same transaction as the guarded org_settings
	// update — a stale token still refuses the whole save with nothing
	// written, by ordinary transaction atomicity, with no second token
	// needed. What it does NOT gate is a concurrent write to the same fields
	// through the admin-only per-source route (PATCH
	// /api/orgs/{org_id}/sources/{kind}), which is an unguarded,
	// last-writer-wins upsert per source — the same concurrency shape that
	// route's disabled switch already has.
	Version int `json:"version"`
	// Warning is advisory prose about a save that SUCCEEDED — today, that a
	// newly-lowered model cap now clamps the default team's preference. Only the
	// PATCH response carries it; the GET leaves it empty and omitempty drops it,
	// so the read and the write answer one shape.
	Warning string `json:"warning,omitempty"`
}

// handleOrgSettingsGet serves the org's configuration. Any org member — the
// base URLs and poll cadence are read by member-facing surfaces (the repo
// page's GitHub host), and nothing here is a secret.
//
// GET /api/orgs/{org_id}/settings
func (s *Server) handleOrgSettingsGet(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	resp, ok := s.readOrgSettings(w, r, orgID, userID, nil)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// readOrgSettings assembles the org-settings resource. Shared by the GET and by
// the PATCH's read-back, so a write answers with exactly what a follow-up read
// would return — including the version the next PATCH has to carry.
//
// known, when non-nil, is the settings row a write in the same request already
// produced (off UpdateSettingsVersioned's RETURNING) — skip the redundant
// GetSettings and build the rest of the composite response (credentials,
// member count, Bedrock config) around it, mirroring readTeamSettings.
func (s *Server) readOrgSettings(w http.ResponseWriter, r *http.Request, orgID, userID string, known *domain.OrgSettings) (orgSettingsResponse, bool) {
	var (
		out    orgSettingsResponse
		orgSet domain.OrgSettings
		creds  auth.Credentials
	)
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
		if known != nil {
			orgSet = *known
		} else {
			orgSet, err = tx.Orgs.GetSettings(r.Context(), orgID)
			if err != nil {
				return err
			}
		}
		// A vault fault is a 500, never "not configured" — the status read
		// drives the whole settings page's connected/disconnected rendering.
		creds, err = integrations.Load(r.Context(), tx.Secrets, orgID)
		if err != nil {
			return fmt.Errorf("load integration credentials: %w", err)
		}
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
		return out, false
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
		return out, false
	}

	// Marker-based "is Jira connected" (matches the integrations-status signal),
	// so a Cloud org with no PAT still reports a stored credential.
	_, hasJiraCred := integrations.JiraSystemConfig(creds)

	return orgSettingsResponse{
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
		BackgroundJobsModel:       orgSet.BackgroundJobsModel,
		MaxDailyCostUSD:           orgSet.MaxDailyCostUSD,
		MaxConcurrentRuns:         orgSet.MaxConcurrentRuns,
		LLMAuthMethod:             domain.EffectiveLLMAuthMethod(orgSet.LLMAuthMethod, !local),
		HasAnthropicAPIKey:        orgSet.AnthropicAPIKeyRef != "",
		HasBedrockCreds:           orgSet.BedrockCredentialsRef != "",
		BedrockAuthMethod:         bedrockAuthMethodFromRef(orgSet.BedrockCredentialsRef),
		BedrockRegion:             bedrockRegion,
		BedrockModelID:            bedrockModelID,
		BedrockBaseURL:            bedrockBaseURL,
		BedrockRoleARN:            bedrockRoleARN,
		BedrockExternalID:         bedrockExternalID,
		MemberCount:               memberCount,
		Version:                   orgSet.Version,
	}, true
}

// orgSettingsPatch is the body of PATCH /api/orgs/{org_id}/settings — the org's
// PURE CONFIG. No secrets: the GitHub PAT, the Jira service credential and the
// LLM provider material each live on their own resource (see org_credentials.go
// and llm_credentials.go), so this route touches no vault key, makes no
// outbound call, and cannot revoke access as a side effect. A credential field
// in the body is a 400 UNKNOWN_FIELD, not a silent store.
//
// Every field is json.RawMessage under the one clearing convention: absent
// keeps, explicit null clears (a base URL, a cap) or resets to the shipped
// default (a poll interval, the clone protocol), any other value is applied and
// validated. There is no empty-string sentinel and no zero-means-clear: an
// explicit 0 cap is a 422, because "cap at $0" and "no cap" are different
// intents and the caller has a spelling for the second one.
type orgSettingsPatch struct {
	GitHubBaseURL       json.RawMessage `json:"github_base_url"`
	GitHubPollInterval  json.RawMessage `json:"github_poll_interval"`
	GitHubCloneProtocol json.RawMessage `json:"github_clone_protocol"`
	JiraBaseURL         json.RawMessage `json:"jira_base_url"`
	JiraPollInterval    json.RawMessage `json:"jira_poll_interval"`
	MaxLLMModelTier     json.RawMessage `json:"max_llm_model_tier"`
	BackgroundJobsModel json.RawMessage `json:"background_jobs_model"`
	LLMAuthMethod       json.RawMessage `json:"llm_auth_method"`
	MaxDailyCostUSD     json.RawMessage `json:"max_daily_cost_usd"`
	MaxConcurrentRuns   json.RawMessage `json:"max_concurrent_runs"`
	// Version is the token the caller read this row at. Required, and a plain
	// int rather than a patch field: it is not something you can clear, and a
	// PATCH without it would be exactly the unconditional last-write-wins save
	// this route stopped being.
	Version *int `json:"version"`
}

// handleOrgSettingsPatch saves the org's configuration. Org-admin only.
//
// Base URLs stay here rather than moving onto the credential resources: the
// GitHub App path has to set a host with no credential in sight (the manifest
// is built against the stored host, before any App exists), so the host is
// genuinely config. Clearing one no longer destroys the matching credential the
// way it used to — disconnecting is an explicit DELETE on the credential.
//
// PATCH /api/orgs/{org_id}/settings
func (s *Server) handleOrgSettingsPatch(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}

	var req orgSettingsPatch
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if req.Version == nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "version is required: send the value the settings read returned",
			Field:   "version",
		})
		return
	}
	apply, ok := s.resolveOrgSettingsPatch(w, r, orgID, req)
	if !ok {
		return
	}

	// ONE transaction for the read-modify-write, with the concurrency guard in
	// the write itself. It used to span two — read the row, mutate in Go, write
	// it back — so two admins saving different sections of the settings page
	// each carried the other's fields as their client last saw them and the
	// first one's change was undone with a 200.
	var prevOrgSet, orgSet domain.OrgSettings
	err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		cur, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		prevOrgSet = cur
		apply(&cur)
		// A background-jobs model whose provider this org has not connected is
		// a model its next cycle would refuse, so it is not worth storing —
		// checked here, inside the transaction, against the row the write is
		// landing on rather than the one the client last read. Only on a
		// change: a save that re-sends a model stored before a credential was
		// disconnected must not be blocked by something this caller did not do.
		// No team restriction is consulted, and there is no team to consult one
		// for — these jobs are the org's own work.
		if cur.BackgroundJobsModel != "" && cur.BackgroundJobsModel != prevOrgSet.BackgroundJobsModel {
			if e := modelaccess.ForOrg(cur).Check(cur.BackgroundJobsModel, nil); e != nil {
				return e
			}
		}
		// Running on the host's credentials means holding none of your own:
		// credential resolution reaches for a stored key whenever one exists,
		// so a row claiming "system" beside a bound provider would describe a
		// run that does not happen. Removing the material is its own act on its
		// own resource — this refuses rather than reaching across and deleting
		// it, which is exactly the overloaded write the LLM credential routes
		// were split up to stop.
		//
		// Checked here, against the row the write is landing on, and only when
		// this write is the one asking for it: a save that re-sends a method
		// stored before someone else bound a key must not be blamed for a state
		// it did not create.
		if cur.LLMAuthMethod == domain.LLMAuthSystem && cur.LLMAuthMethod != prevOrgSet.LLMAuthMethod {
			if bound := boundLLMProviders(cur); len(bound) > 0 {
				return fmt.Errorf("%w: disconnect %s first", errLLMAuthMethodHasCredentials, strings.Join(bound, " and "))
			}
		}
		orgSet, err = tx.Orgs.UpdateSettingsVersioned(r.Context(), orgID, cur, *req.Version)
		return err
	})
	if writeModelAccessError(w, err, "background_jobs_model") {
		return
	}
	if errors.Is(err, errLLMAuthMethodHasCredentials) {
		// 422, not 400: the value is a legal one and the body is well formed —
		// what is wrong is the state of the resource it would land on, which is
		// the same distinction the credential binds draw for a key the provider
		// rejects.
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: err.Error(), Field: "llm_auth_method",
		})
		return
	}
	if errors.Is(err, db.ErrOrgSettingsVersion) {
		// No server-side merge: the two writers disagree about fields neither of
		// them named, so the only honest resolution is for the loser to see the
		// winner's row and decide again.
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonVersionConflict,
			Message: "these settings changed since you loaded them — reload and re-apply your edit",
			Field:   "version",
		})
		return
	}
	if err != nil {
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

	resp, ok := s.readOrgSettings(w, r, orgID, userID, &orgSet)
	if !ok {
		return
	}
	// Lowering the cap doesn't block the save — the admin has authority — but if
	// the default team already prefers a higher tier, surface that its effective
	// model just dropped. Gated on an actual cap change so an unrelated save
	// doesn't re-warn each time the default team sits above an unchanged cap.
	// Single-team-per-org today, so we check the default team; broadens to a
	// team list when multi-team lands.
	if orgSet.MaxLLMModelTier != "" && orgSet.MaxLLMModelTier != prevOrgSet.MaxLLMModelTier {
		resp.Warning = s.capDowngradeWarning(r.Context(), orgID, userID, orgSet.MaxLLMModelTier)
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveOrgSettingsPatch validates the body once and returns the mutation it
// describes. Two statuses for two fault classes, exactly as the team sibling:
// shape and vocabulary answer 400, a well-formed value outside its band answers
// 422. Checks that need the world rather than the body — the App-registration
// gate on clearing the GitHub host, the SSH preflight — run here too, before
// any transaction is open, because one of them takes fifteen seconds.
//
// On any failure the response is already written and ok is false.
func (s *Server) resolveOrgSettingsPatch(w http.ResponseWriter, r *http.Request, orgID string, req orgSettingsPatch) (apply func(*domain.OrgSettings), ok bool) {
	if !httpx.PatchNamed(
		req.GitHubBaseURL, req.GitHubPollInterval, req.GitHubCloneProtocol,
		req.JiraBaseURL, req.JiraPollInterval, req.MaxLLMModelTier,
		req.BackgroundJobsModel, req.LLMAuthMethod, req.MaxDailyCostUSD, req.MaxConcurrentRuns,
	) {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no fields to update: name at least one settings field (null clears it)",
		})
		return nil, false
	}

	var (
		shape    httpx.Validation
		ranges   httpx.Validation
		mutators []func(*domain.OrgSettings)
		defaults = domain.DefaultOrgSettings()
	)
	set := func(f func(*domain.OrgSettings)) { mutators = append(mutators, f) }

	// base_url writes go through one shared loop, keyed by the source's own
	// registration rather than special-cased per field: eventsource.HasHost
	// is the same gate that decides whether ANY kind may carry a base_url
	// override at all — a source with no self-host story must never get one,
	// because base_url decides where a credential is sent. GitHub and Jira
	// both declare a host today, so the check always passes for them; a
	// future kind added to this route inherits the refusal for free rather
	// than needing its own copy of it.
	clearingGitHubHost := false
	for _, f := range []struct {
		raw   json.RawMessage
		field string
		kind  string
		clear *bool // set true when this field is the one being cleared
		set   func(*domain.OrgSettings, string)
	}{
		{req.GitHubBaseURL, "github_base_url", eventsource.KindGitHub, &clearingGitHubHost,
			func(o *domain.OrgSettings, v string) { o.GitHubBaseURL = v }},
		{req.JiraBaseURL, "jira_base_url", eventsource.KindJira, nil,
			func(o *domain.OrgSettings, v string) { o.JiraBaseURL = v }},
	} {
		v, st := httpx.PatchString(&shape, f.raw, f.field)
		if st == httpx.PatchAbsent {
			continue
		}
		if !eventsource.HasHost(f.kind) {
			shape.Invalid(f.field, f.field+" cannot be set: "+f.kind+" has no self-host option")
			continue
		}
		apply := f.set
		switch st {
		case httpx.PatchClear:
			if f.clear != nil {
				*f.clear = true
			}
			set(func(o *domain.OrgSettings) { apply(o, "") })
		case httpx.PatchSet:
			// Non-empty hosts go through the same validator + canonicalizer the
			// reachability probe uses, so what the column holds is what the probe
			// accepted. Persisting verbatim let a value the probe merely tolerated
			// ("  https://ghes.example.com/ ") through, and the App/PAT host
			// derivation reads the column, not the probe's answer. A blank string
			// is not the clear — null is — so it fails here like any other
			// unusable URL.
			base, valid := normalizeBaseURL(v)
			if !valid {
				shape.Invalid(f.field, f.field+" must be a valid http(s) URL, or null to clear it")
			} else {
				set(func(o *domain.OrgSettings) { apply(o, base) })
			}
		}
	}
	// A malformed duration is rejected rather than silently ignored — the old
	// parse-and-drop behavior meant a typo'd interval reported "saved" while
	// keeping the previous value. null resets to that poller's shipped
	// default. Gated on eventsource.Polled per kind for the same reason the
	// base_url loop above is gated on HasHost: github/jira are both polled
	// today, so the check always passes, but it is the registry — not this
	// route's two hardcoded fields — that decides.
	for _, f := range []struct {
		raw   json.RawMessage
		field string
		kind  string
		def   time.Duration
		set   func(*domain.OrgSettings, time.Duration)
	}{
		{req.GitHubPollInterval, "github_poll_interval", eventsource.KindGitHub, defaults.GitHubPollInterval,
			func(o *domain.OrgSettings, d time.Duration) { o.GitHubPollInterval = d }},
		{req.JiraPollInterval, "jira_poll_interval", eventsource.KindJira, defaults.JiraPollInterval,
			func(o *domain.OrgSettings, d time.Duration) { o.JiraPollInterval = d }},
	} {
		v, st := httpx.PatchString(&shape, f.raw, f.field)
		if st == httpx.PatchAbsent {
			continue
		}
		if !eventsource.Polled(f.kind) {
			shape.Invalid(f.field, f.field+" cannot be set: "+f.kind+" has no poll cadence")
			continue
		}
		next := f.def
		if st == httpx.PatchSet {
			d, err := parseMinDuration(v, orgPollIntervalMinMinutes)
			if err != nil {
				ranges.OutOfRange(f.field, fmt.Sprintf(
					"%s must be a duration of at least %dm (e.g. \"15m\"), or null for the default",
					f.field, orgPollIntervalMinMinutes))
			}
			next = d
		}
		apply := f.set
		set(func(o *domain.OrgSettings) { apply(o, next) })
	}
	if v, st := httpx.PatchString(&shape, req.GitHubCloneProtocol, "github_clone_protocol"); st != httpx.PatchAbsent {
		// The cleared-to-default value goes through the same resolver the read
		// side does, so a null clears to what this deployment can actually
		// honor. The package default is "ssh" (right for local), and writing it
		// unresolved would plant, via an explicit clear, exactly the value the
		// PatchSet arm below refuses to accept in multi.
		next := domain.EffectiveCloneProtocol(defaults.GitHubCloneProtocol, runmode.Current() == runmode.ModeMulti)
		if st == httpx.PatchSet {
			switch {
			case v != "ssh" && v != "https":
				shape.Invalid("github_clone_protocol", "github_clone_protocol must be \"ssh\" or \"https\", or null for the default")
			case v == "ssh" && runmode.Current() != runmode.ModeLocal:
				// Multi-mode is HTTPS-only: refuse an ssh write rather than
				// persist a value the effective resolver (and the clone path)
				// will ignore. The UI hides the control in multi mode; this
				// rejects a direct API call.
				shape.Invalid("github_clone_protocol", "ssh clone protocol is not available in this deployment; use https")
			default:
				next = v
			}
		}
		set(func(o *domain.OrgSettings) { o.GitHubCloneProtocol = next })
	}
	if v, st := httpx.PatchString(&shape, req.MaxLLMModelTier, "max_llm_model_tier"); st != httpx.PatchAbsent {
		switch {
		case st == httpx.PatchClear:
			set(func(o *domain.OrgSettings) { o.MaxLLMModelTier = "" })
		case st == httpx.PatchSet && domain.ParseTier(v) == domain.TierUnknown:
			shape.Invalid("max_llm_model_tier", "max_llm_model_tier must be haiku, sonnet, or opus, or null for no cap")
		case st == httpx.PatchSet:
			set(func(o *domain.OrgSettings) { o.MaxLLMModelTier = v })
		}
	}
	// background_jobs_model must name a model this deployment offers. The picker
	// draws from the same catalog and the jobs dispatch the stored value
	// verbatim, so there is no room for a spelling nothing can invoke. The R5
	// delegation gates (tool support, a 64k window) deliberately do NOT narrow
	// it — these jobs are toolless and short-context, so every catalog entry is
	// a legitimate choice.
	//
	// Whether the org has connected the provider that serves it is checked in
	// the transaction below, against the row this write is landing on.
	//
	// Null clears it, and clearing is a real intent: it turns the background
	// jobs off, which is the only way to stop them short of unbinding the
	// credential every other feature shares. The jobs then skip with a warning
	// naming this setting — never a model of TF's choosing.
	if v, st := httpx.PatchString(&shape, req.BackgroundJobsModel, "background_jobs_model"); st != httpx.PatchAbsent {
		switch {
		case st == httpx.PatchClear:
			set(func(o *domain.OrgSettings) { o.BackgroundJobsModel = "" })
		case !modelcatalog.Offers(strings.TrimSpace(v)):
			shape.Invalid("background_jobs_model", "background_jobs_model must name a model this deployment offers, or be null to stop running background jobs: "+strings.Join(modelcatalog.Keys(), ", "))
		default:
			set(func(o *domain.OrgSettings) { o.BackgroundJobsModel = strings.TrimSpace(v) })
		}
	}
	// llm_auth_method names where the org's Claude credentials come from, and
	// it is the one settings field whose legal values depend on the deployment:
	// "system" means the agent subprocess inherits the host's environment, and
	// a hosted deployment's environment is one environment shared by every
	// tenant. Refused there rather than stored-and-ignored, so an admin is told
	// no instead of being left with a row that reads one way and runs another.
	//
	// Not nullable. There is no third state to clear TO — an org's credentials
	// come from one of the two places — so an explicit null is a rejected value
	// rather than a spelling of "unset".
	//
	// Whether the org still holds material that "system" would contradict is
	// checked in the transaction below, against the row this write is landing
	// on.
	if v, st := httpx.PatchString(&shape, req.LLMAuthMethod, "llm_auth_method"); st != httpx.PatchAbsent {
		method := strings.TrimSpace(v)
		switch {
		case st == httpx.PatchClear:
			shape.Invalid("llm_auth_method", "llm_auth_method cannot be cleared: an organization's Claude credentials come from the host (\"system\") or from its own bound material (\"byok\")")
		case method != domain.LLMAuthSystem && method != domain.LLMAuthBYOK:
			shape.Invalid("llm_auth_method", "llm_auth_method must be \""+domain.LLMAuthSystem+"\" or \""+domain.LLMAuthBYOK+"\"")
		case method == domain.LLMAuthSystem && runmode.Current() == runmode.ModeMulti:
			shape.Invalid("llm_auth_method", "this deployment has no host credentials to run under — every organization brings its own, so llm_auth_method must be \""+domain.LLMAuthBYOK+"\"")
		default:
			set(func(o *domain.OrgSettings) { o.LLMAuthMethod = method })
		}
	}
	// "No cap" is null. An explicit 0 is refused rather than quietly read as
	// "no cap": capping at $0 and having no cap are different intents, and a
	// caller who means the second one has a spelling for it.
	if v, st := httpx.PatchFloat(&shape, req.MaxDailyCostUSD, "max_daily_cost_usd"); st != httpx.PatchAbsent {
		next := 0.0
		if st == httpx.PatchSet {
			if v <= 0 {
				ranges.OutOfRange("max_daily_cost_usd", "max_daily_cost_usd must be greater than 0, or null for no cap")
			}
			next = v
		}
		set(func(o *domain.OrgSettings) { o.MaxDailyCostUSD = next })
	}
	// Same reasoning as the cap above: null is "unlimited", 0 is a refusal. The
	// ceiling keeps a validated value inside the Postgres int4 column rather
	// than 500ing on "integer out of range".
	if v, st := httpx.PatchInt(&shape, req.MaxConcurrentRuns, "max_concurrent_runs"); st != httpx.PatchAbsent {
		next := 0
		if st == httpx.PatchSet {
			switch {
			case v <= 0:
				ranges.OutOfRange("max_concurrent_runs", "max_concurrent_runs must be greater than 0, or null for unlimited")
			case v > domain.MaxConcurrentClaimsCeiling:
				ranges.OutOfRange("max_concurrent_runs", fmt.Sprintf("max_concurrent_runs must be at most %d", domain.MaxConcurrentClaimsCeiling))
			}
			next = v
		}
		set(func(o *domain.OrgSettings) { o.MaxConcurrentRuns = next })
	}

	if shape.Flush(w, http.StatusBadRequest) {
		return nil, false
	}
	if ranges.Flush(w, http.StatusUnprocessableEntity) {
		return nil, false
	}

	// Blanking the host while an App registration exists is REFUSED. The
	// resolver's base lookup falls org_settings → the github_url secret →
	// github.com, so an empty column silently re-points a GHES org's App at
	// github.com: wrong host, no error, nothing in any log.
	//
	// Refused rather than skipped, which is where this differs from the PAT
	// unbind's identical hazard. There, clearing the host is a side effect of
	// unbinding a token, so quietly keeping it is right. Here the clear IS the
	// request, and answering "saved" for work we declined to do is the
	// parse-and-drop bug in another costume.
	//
	// Re-targeting to a different NON-empty host stays allowed: that's a real
	// move during a GHES domain change, and whatever breaks is at least the
	// value the admin typed rather than a default they never chose.
	//
	// GitHub-only: jira.CanonicalHost returns ok=false on a blank base URL, so
	// the Jira surfaces fail loudly instead of resolving somewhere wrong.
	if clearingGitHubHost {
		app, err := s.githubApps.GetForOrgSystem(r.Context(), orgID)
		if err != nil {
			internalError(w, "settings/org", err)
			return nil, false
		}
		if app != nil {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
				Reason:  httpx.ReasonConflict,
				Message: "this workspace's GitHub App is registered against this host — remove the App before clearing it",
				Field:   "github_base_url",
			})
			return nil, false
		}
	}

	if !s.orgSettingsSSHPreflight(w, r, orgID, mutators) {
		return nil, false
	}

	return func(o *domain.OrgSettings) {
		for _, m := range mutators {
			m(o)
		}
	}, true
}

// orgSettingsSSHPreflight gates the transition INTO SSH clone mode. Local-mode
// only — PreflightSSH writes the container's ~/.ssh/known_hosts and probes the
// operator's ssh-agent, neither of which belongs in a hosted runtime. In multi
// mode the ssh write is already rejected above, so the explicit mode gate makes
// the no-SSH-in-multi guarantee provable at this call site too.
//
// It reads the stored settings to decide whether this save is a TRANSITION, and
// that read is deliberately outside the write's transaction: the probe can take
// fifteen seconds and must not hold one open. The read is only a decision input
// — if another admin flips the protocol in between, their write bumps the row's
// version and this request's write is refused, so the stale answer never
// reaches the column.
func (s *Server) orgSettingsSSHPreflight(w http.ResponseWriter, r *http.Request, orgID string, mutators []func(*domain.OrgSettings)) bool {
	if runmode.Current() != runmode.ModeLocal {
		return true
	}
	userID := ClaimsFrom(r.Context()).Subject
	var cur domain.OrgSettings
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var err error
		cur, err = tx.Orgs.GetSettings(r.Context(), orgID)
		return err
	}); err != nil {
		internalError(w, "settings/org", err)
		return false
	}
	next := cur
	for _, m := range mutators {
		m(&next)
	}
	if next.GitHubCloneProtocol != "ssh" || cur.GitHubCloneProtocol == "ssh" {
		return true
	}

	sshHost := worktree.SSHHostFromBaseURL(next.GitHubBaseURL)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	err := worktree.PreflightSSH(ctx, sshHost)
	cancel()
	if err == nil {
		return true
	}
	// The probe's stderr used to ride along in its own key, a dialect only this
	// route spoke. It is already the second half of the message, and the full
	// output is in the log line above.
	settingsOrgLog.Warn("blocked ssh switch", "ssh_host", sshHost, "error", err)
	httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
		Reason:  httpx.ReasonInvalidField,
		Message: fmt.Sprintf("SSH preflight against %s failed — fix your SSH setup or keep HTTPS. %s", sshHost, err.Error()),
		Field:   "github_clone_protocol",
	})
	return false
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
