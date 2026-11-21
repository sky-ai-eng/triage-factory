package server

import (
	"bytes"
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
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/poller"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelaccess"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/models"
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
	UserSettings domain.UserSettings `json:"user_settings" doc:"The caller's settings."`
}

var opMeSettingsGet = httpx.NewJSONOp[httpx.NoBody, userSettingsResponse](httpx.OpSpec{
	Pattern:     "GET /api/me/settings",
	ID:          "get_my_settings",
	Summary:     "Get your settings",
	Description: "Returns the caller's own settings, which are the same in every organization they belong to.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleMeSettingsGet answers the caller's settings resource.
//
// GET /api/me/settings
func (s *Server) handleMeSettingsGet(w http.ResponseWriter, r *http.Request) {
	userID := ClaimsFrom(r.Context()).Subject
	orgID := OrgIDFrom(r.Context())

	resp, ok := s.readUserSettings(w, r, orgID, userID, nil)
	if !ok {
		return
	}
	opMeSettingsGet.Write(w, resp)
}

// readUserSettings assembles the settings resource, shared by the GET and by
// the PATCH's answer so the write answers exactly what a follow-up read would.
// known is the row a caller already holds — the one a save's write just
// returned — and when non-nil this composes the response around it rather than
// re-reading the row its own write produced. The GET route passes nil.
func (s *Server) readUserSettings(w http.ResponseWriter, r *http.Request, orgID, userID string, known *domain.UserSettings) (userSettingsResponse, bool) {
	var resp userSettingsResponse
	if known != nil {
		resp.UserSettings = *known
		return resp, true
	}
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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

// userSettingsPatch is the PATCH body. user_settings absent leaves the stored
// row alone entirely; present applies the fields it names. Strict decoding
// makes a key this resource does not have a named 400 rather than a value
// quietly dropped, at either level.
//
// It is a Patch rather than a *userSettingsFields because a typed pointer
// reads an explicit null as nil, which is the same thing it reads "absent" as —
// so `{"user_settings": null}` would answer 200 having written nothing, the one
// response a client cannot tell from a real save. Here the two are different:
// absent writes nothing on purpose, and null is refused by name.
type userSettingsPatch struct {
	UserSettings httpx.Patch[userSettingsFields] `json:"user_settings" doc:"The settings to write, an object naming only the fields to change; absent writes nothing, and null is refused."`
}

// userSettingsPatchRaw is the same body with user_settings kept as bytes, which
// decodeUserSettingsFields decodes itself: it refuses a key the object does not
// have as UNKNOWN_FIELD by that key's name, where Patch.Read would refuse it on
// user_settings.
type userSettingsPatchRaw struct {
	UserSettings json.RawMessage `json:"user_settings"`
}

// decodeUserSettingsFields reads the settings object out of the PATCH body.
//
// The object is the resource, not a field of it, so the clearing convention
// does not reach this level: there is no state "the user has no settings", and
// a null that meant "reset everything" would silently widen with every pref
// added — a client clearing today's one marker would wipe tomorrow's theme.
// So null, and every other non-object, is one named fault. The check is
// explicit because encoding/json cannot attribute it: a type error against a
// struct target carries no field name, and httpx's decoder needs one to
// answer INVALID_FIELD rather than a bare INVALID_BODY.
func decodeUserSettingsFields(w http.ResponseWriter, raw json.RawMessage) (userSettingsFields, bool) {
	var fields userSettingsFields
	if value := bytes.TrimLeft(raw, " \t\r\n"); len(value) == 0 || value[0] != '{' {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: "user_settings must be an object naming the fields to write; omit it to write nothing",
			Field:   "user_settings",
		})
		return fields, false
	}
	// Strict again on the way in, so a misspelled pref inside is a named 400
	// rather than a key the outer decode already consumed into raw bytes.
	if !httpx.DecodeJSONStrictBytes(w, raw, &fields) {
		return fields, false
	}
	return fields, true
}

// userSettingsFields is the settings object inside the PATCH body, declared
// separately from domain.UserSettings so each field speaks the one clearing
// convention: absent keeps the stored value, an explicit null clears it, any
// other value is applied. A typed pointer collapses the first two — both
// decode to nil — which is the distinction this resource needs, since clearing
// the marker (back to "never opened") is a thing a client can mean.
type userSettingsFields struct {
	OverviewSeenAt httpx.Patch[string] `json:"overview_seen_at" doc:"When the caller last opened the Overview, as an RFC 3339 timestamp at most five minutes ahead of the server's clock, or null to record that they have never opened it."`
}

// overviewSeenAtField is the dotted path the faults name, since the field is
// one level in and "overview_seen_at" alone would not say where to look.
const overviewSeenAtField = "user_settings.overview_seen_at"

// overviewSeenSkew is how far ahead of this server's clock a marker may land.
// The value is the client's own now, so the two clocks disagree by whatever
// they disagree by, and refusing a few seconds of ordinary drift would make the
// write fail for a machine that is merely slightly fast. Past that the value is
// not drift but a broken clock, and it is refused rather than stored: a marker
// in the future inverts the away line for as long as it stands, and nothing
// walks it back — the next visit writes a timestamp EARLIER than the stored
// one, so every counted-since window reads empty until real time catches up.
const overviewSeenSkew = 5 * time.Minute

var opMeSettingsPatch = httpx.NewJSONOp[userSettingsPatch, userSettingsResponse](httpx.OpSpec{
	Pattern:     "PATCH /api/me/settings",
	ID:          "update_my_settings",
	Summary:     "Update your settings",
	Description: "Sets or clears the fields of the caller's own settings that user_settings names, and returns the settings as the read does. A body without user_settings, or with an empty user_settings object, writes nothing and returns the current settings.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleMeSettingsPatch applies a partial update to the caller's settings row
// and answers the settings resource as a follow-up GET would return it — so a
// body that describes no change answers the state, not the word "saved".
//
// PATCH /api/me/settings
func (s *Server) handleMeSettingsPatch(w http.ResponseWriter, r *http.Request) {
	userID := ClaimsFrom(r.Context()).Subject
	orgID := OrgIDFrom(r.Context())
	body, ok := httpx.BodyBytes(w, r)
	if !ok {
		return
	}
	if _, ok := opMeSettingsPatch.DecodeBytes(w, body); !ok {
		return
	}
	// The strict decode above accepted the body, so this one cannot fail.
	var req userSettingsPatchRaw
	_ = json.Unmarshal(body, &req)

	var known *domain.UserSettings
	if req.UserSettings != nil {
		fields, ok := decodeUserSettingsFields(w, req.UserSettings)
		if !ok {
			return
		}
		apply, ok := resolveUserSettingsPatch(w, fields)
		if !ok {
			return
		}
		var saved domain.UserSettings
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			// UpdateSettings takes the row's end state, so the partial write is
			// composed here — read, apply, write — inside the one transaction,
			// which is what keeps a field this body does not name at the value
			// it actually had rather than at the value it had a moment ago.
			current, err := tx.Users.GetSettings(r.Context(), userID)
			if err != nil {
				return fmt.Errorf("load user settings: %w", err)
			}
			apply(&current)
			saved, err = tx.Users.UpdateSettings(r.Context(), userID, current)
			return err
		}); err != nil {
			internalError(w, "me/settings", err)
			return
		}
		known = &saved
	}

	resp, ok := s.readUserSettings(w, r, orgID, userID, known)
	if !ok {
		return
	}
	opMeSettingsPatch.Write(w, resp)
}

// resolveUserSettingsPatch validates the body and returns the mutation it
// describes, applied to the stored row inside the write's transaction. Faults
// are collected in two rounds and flushed separately: shape first (400 — a
// value cannot be out of range until it parses), then semantics (422).
func resolveUserSettingsPatch(w http.ResponseWriter, req userSettingsFields) (apply func(*domain.UserSettings), ok bool) {
	var (
		shape    httpx.Validation
		semantic httpx.Validation
		mutators []func(*domain.UserSettings)
	)

	// overview_seen_at is the anchor the Overview's away line reads from, and
	// the client sends its own now. null clears it back to "never opened",
	// which the page renders as its own sentence rather than as a very old
	// visit — so it is a state a client can legitimately ask for.
	if value, st := req.OverviewSeenAt.Read(&shape, overviewSeenAtField); st != httpx.PatchAbsent {
		switch st {
		case httpx.PatchClear:
			mutators = append(mutators, func(u *domain.UserSettings) { u.OverviewSeenAt = nil })
		case httpx.PatchSet:
			at, err := time.Parse(time.RFC3339, value)
			switch {
			case err != nil:
				shape.Invalid(overviewSeenAtField, overviewSeenAtField+" must be an RFC3339 timestamp, or null")
			case at.After(time.Now().Add(overviewSeenSkew)):
				semantic.OutOfRange(overviewSeenAtField, fmt.Sprintf(
					"%s must not be more than %s ahead of the server's clock", overviewSeenAtField, overviewSeenSkew))
			default:
				// UTC on the way in: the value is compared against stored
				// timestamps and rendered as an absolute instant, and a
				// local-offset value reads as its wall clock in both places.
				seen := at.UTC()
				mutators = append(mutators, func(u *domain.UserSettings) { u.OverviewSeenAt = &seen })
			}
		}
	}

	if shape.Flush(w, http.StatusBadRequest) {
		return nil, false
	}
	if semantic.Flush(w, http.StatusUnprocessableEntity) {
		return nil, false
	}
	// A body naming no field is not an error on this route: the settings
	// resource answers the state either way, and the two no-change spellings
	// ({} and {"user_settings": {}}) both mean "tell me where I am".
	return func(u *domain.UserSettings) {
		for _, m := range mutators {
			m(u)
		}
	}, true
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
// The Jira project rules and the Linear team rules are NOT part of the PATCH
// body. Each is a child collection with its own replace-set write (PUT
// /api/teams/{team_id}/jira-projects, PUT /api/teams/{team_id}/linear-teams),
// matching the tracked-repo and github-group siblings; they still ride the
// composite GET, which is a deliberate convenience for the settings page
// rather than an oversight.
// --------------------------------------------------------------------

type teamSettingsResponse struct {
	TeamSettings teamSettingsJSON      `json:"team_settings" doc:"The team's settings, under the names PATCH /api/teams/{team_id}/settings writes them by."`
	JiraProjects []jiraProjectSettings `json:"jira_projects" doc:"The Jira projects the team tracks on the organization's current Jira site, in display order, with their workflow status rules."`
	// LinearTeams is the team's tracked Linear teams in display order, the
	// shape PUT /api/teams/{team_id}/linear-teams answers with.
	LinearTeams []linearTeamSettings `json:"linear_teams" doc:"The Linear teams the team tracks in the organization's current Linear workspace, in display order, with their workflow state rules."`
	// MemberCount + Role describe the caller's relationship to this team,
	// so the frontend can collapse to the flat N=1 layout and gate the
	// write-side fields without a second round trip. They live on the
	// team-scope response (not /api/me) because they're properties of the
	// team, not the user — switching teams refetches this endpoint and
	// gets the new team's count + the caller's role in it.
	MemberCount int    `json:"member_count" doc:"How many people are members of the team."`
	Role        string `json:"role" doc:"The caller's role in the team, admin, member, or viewer, or empty when the caller is not a member."`
	// PermissionAbsentGraceMinSeconds / PermissionAbsentGraceMaxSeconds advertise
	// the honored bounds of the unattended-prompt grace window so the team
	// settings UI can render a slider whose range tracks the backend (the 1s
	// floor clampGrace enforces and the ceiling just below permTimeout()) instead
	// of hardcoding it.
	PermissionAbsentGraceMinSeconds int `json:"permission_absent_grace_min_seconds" doc:"The smallest value permission_absent_grace_seconds accepts, in seconds."`
	PermissionAbsentGraceMaxSeconds int `json:"permission_absent_grace_max_seconds" doc:"The largest value permission_absent_grace_seconds accepts, in seconds."`
}

// teamSettingsJSON is the team's settings row as the API renders it: each
// field the PATCH writes, under the PATCH's own name and unit, so a client
// reads and writes one vocabulary. The tracked Jira projects and Linear teams
// are not here; the response's jira_projects and linear_teams carry them in
// the same order.
type teamSettingsJSON struct {
	AIModel                         string   `json:"ai_model" doc:"The model key the team's steps run on when they name no model, empty when the team has none."`
	EnabledModels                   []string `json:"enabled_models" doc:"The model keys the team may choose from, or null when every model the organization enables is allowed."`
	AIAutoDelegate                  bool     `json:"ai_auto_delegate_enabled" doc:"Whether event handlers in automate mode may start runs for the team."`
	AutoModeEnabled                 bool     `json:"auto_mode_enabled" doc:"Whether the team's delegated runs on the Claude Code SDK start in its auto permission mode in local mode."`
	ClaudeAIConnectorsEnabled       bool     `json:"claude_ai_connectors_enabled" doc:"Whether the team's delegated runs on the operator's own Claude Code login keep that login's claude.ai connectors."`
	AIReprioritizeThreshold         int      `json:"ai_reprioritize_threshold" doc:"A stored scoring setting that nothing acts on."`
	AIPreferenceUpdateInterval      int      `json:"ai_preference_update_interval" doc:"A stored scoring setting that nothing acts on."`
	BranchTemplate                  string   `json:"branch_template" doc:"The branch name convention the team's agents are asked to follow, in which <ticket-id> stands for the run's ticket id."`
	ReviewPosture                   string   `json:"review_posture" doc:"How the team's delegated reviews reach GitHub: identity, draft, auto, or auto_unless_blocking."`
	BaseBranchPushPolicy            string   `json:"base_branch_push_policy" doc:"Whether the team's agents may push to a repository's base branch: never, manual_only, or always."`
	PermissionAbsentAutodenyEnabled bool     `json:"permission_absent_autodeny_enabled" doc:"Whether a delegated run's permission prompt is denied after permission_absent_grace_seconds when nobody who could answer it has the app open."`
	PermissionAbsentGraceSeconds    int      `json:"permission_absent_grace_seconds" doc:"How long an unanswered permission prompt waits, in seconds, before it is denied while nobody who could answer it has the app open."`
	// MaxDailyCostUSD is the one field here the PATCH does not write: the cap
	// has its own route, PUT /api/teams/{team_id}/usage/cap.
	MaxDailyCostUSD *float64 `json:"max_daily_cost_usd" doc:"The most the team's runs may spend in one UTC day, in USD, or null when the team has no cap; PUT /api/teams/{team_id}/usage/cap sets it."`
}

// toTeamSettingsJSON renders a settings row for the wire. The grace window is
// stored in milliseconds and read in seconds, rounded to the nearest one, which
// is exact for every row the API wrote: the PATCH stores whole seconds.
func toTeamSettingsJSON(t domain.TeamSettings) teamSettingsJSON {
	return teamSettingsJSON{
		AIModel:                         t.DefaultModel,
		EnabledModels:                   t.EnabledModels,
		AIAutoDelegate:                  t.AutoDelegateEnabled,
		AutoModeEnabled:                 t.AutoModeEnabled,
		ClaudeAIConnectorsEnabled:       t.ClaudeAIConnectorsEnabled,
		AIReprioritizeThreshold:         t.AIReprioritizeThreshold,
		AIPreferenceUpdateInterval:      t.AIPreferenceUpdateInterval,
		BranchTemplate:                  t.BranchTemplate,
		ReviewPosture:                   t.ReviewPosture,
		BaseBranchPushPolicy:            t.BaseBranchPushPolicy,
		PermissionAbsentAutodenyEnabled: t.PermissionAbsentAutodenyEnabled,
		PermissionAbsentGraceSeconds:    (t.PermissionAbsentGraceMS + 500) / 1000,
		MaxDailyCostUSD:                 limitOrNull(t.MaxDailyCostUSD),
	}
}

// limitOrNull renders a stored limit, whose 0 is "none", in the spelling the
// PATCHes clear one with: null.
func limitOrNull[T int | float64](v T) *T {
	if v <= 0 {
		return nil
	}
	return &v
}

var opTeamSettingsGet = httpx.NewJSONOp[httpx.NoBody, teamSettingsResponse](httpx.OpSpec{
	Pattern:     "GET /api/teams/{team_id}/settings",
	ID:          "get_team_settings",
	Summary:     "Get a team's settings",
	Description: "Returns the team's settings, the Jira projects and Linear teams it tracks with their workflow rules, its member count, and the caller's role in it.",
	PathParams:  map[string]string{"team_id": "The team's id, or default for the one team in local mode."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	opTeamSettingsGet.Write(w, resp)
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
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var settings domain.TeamSettings
		if known != nil {
			settings = *known
		} else {
			var err error
			if settings, err = tx.Teams.GetSettings(r.Context(), teamID); err != nil {
				return fmt.Errorf("team settings: %w", err)
			}
		}
		resp.TeamSettings = toTeamSettingsJSON(settings)

		// Only the org's current Jira site's and Linear workspace's rules: a
		// rule saved under another names a project or team this one does not
		// have.
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("org settings: %w", err)
		}
		rules, err := tx.JiraStatusRules.ListForTeam(r.Context(), teamID, domain.EntityScope("jira", orgSet))
		if err != nil {
			return fmt.Errorf("jira rules: %w", err)
		}
		projects := rulesToProjectConfigsOrdered(rules, settings.JiraProjects)
		resp.JiraProjects = toJiraProjectSettings(projects)

		linearRules, err := tx.LinearTeamRules.ListForTeam(r.Context(), teamID, domain.EntityScope("linear", orgSet))
		if err != nil {
			return fmt.Errorf("linear rules: %w", err)
		}
		resp.LinearTeams = toLinearTeamSettings(orderLinearTeamRules(linearRules, settings.LinearTeams))
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
// Every field is a Patch under ONE clearing convention: absent keeps the
// stored value, an explicit null clears it (enabled_models) or resets it to the
// shipped default (everything else but ai_model, which refuses it), and any
// other value is applied, so "how do I clear this" has one answer for every
// field.
//
// jira_projects is deliberately absent. The team's Jira rules are a child
// collection with their own replace-set write (PUT
// /api/teams/{team_id}/jira-projects), like the tracked repos and the
// github-group mappings; strict decoding turns a stale caller that still sends
// them here into a 400 naming the field rather than a silent half-save.
type teamSettingsPatch struct {
	AIModel                         httpx.Patch[string]   `json:"ai_model" doc:"The model key the team's steps run on when they name no model, which must be a model the deployment offers and the team enables; null is refused."`
	EnabledModels                   httpx.Patch[[]string] `json:"enabled_models" doc:"The model keys the team may choose from, a non-empty list without duplicates drawn from the models the organization enables, or null to allow every model the organization enables."`
	AIAutoDelegate                  httpx.Patch[bool]     `json:"ai_auto_delegate_enabled" doc:"Whether event handlers in automate mode may start runs for the team, or null to reset it to true."`
	AutoModeEnabled                 httpx.Patch[bool]     `json:"auto_mode_enabled" doc:"Whether the team's delegated runs on the Claude Code SDK start in its auto permission mode in local mode, or null to reset it to true."`
	ClaudeAIConnectorsEnabled       httpx.Patch[bool]     `json:"claude_ai_connectors_enabled" doc:"Whether the team's delegated runs on the operator's own Claude Code login keep that login's claude.ai connectors, or null to reset it to false."`
	AIReprioritizeThreshold         httpx.Patch[int]      `json:"ai_reprioritize_threshold" doc:"A stored scoring setting that nothing acts on, a whole number greater than 0, or null to reset it to 5."`
	AIPreferenceUpdateInterval      httpx.Patch[int]      `json:"ai_preference_update_interval" doc:"A stored scoring setting that nothing acts on, a whole number greater than 0, or null to reset it to 20."`
	BranchTemplate                  httpx.Patch[string]   `json:"branch_template" doc:"The branch name convention the team's agents are asked to follow, in which <ticket-id> stands for the run's ticket id; it must not be blank, and null resets it to tfac/<ticket-id>."`
	ReviewPosture                   httpx.Patch[string]   `json:"review_posture" doc:"How the team's delegated reviews reach GitHub: identity, draft, auto, or auto_unless_blocking, or null to reset it to identity."`
	BaseBranchPushPolicy            httpx.Patch[string]   `json:"base_branch_push_policy" doc:"Whether the team's agents may push to a repository's base branch: never, manual_only, or always, or null to reset it to never."`
	PermissionAbsentAutodenyEnabled httpx.Patch[bool]     `json:"permission_absent_autodeny_enabled" doc:"Whether a delegated run's permission prompt is denied after permission_absent_grace_seconds when nobody who could answer it has the app open, or null to reset it to true."`
	PermissionAbsentGraceSeconds    httpx.Patch[int]      `json:"permission_absent_grace_seconds" doc:"How long an unanswered permission prompt waits, in seconds, before it is denied while nobody who could answer it has the app open, between permission_absent_grace_min_seconds and permission_absent_grace_max_seconds, or null to reset it to 15."`
}

var opTeamSettingsPatch = httpx.NewJSONOp[teamSettingsPatch, teamSettingsResponse](httpx.OpSpec{
	Pattern:     "PATCH /api/teams/{team_id}/settings",
	ID:          "update_team_settings",
	Summary:     "Update a team's settings",
	Description: "Sets the team settings the body names, or resets those set to null to their defaults, and returns the team's settings as the read does. The body must name at least one field.",
	PathParams:  map[string]string{"team_id": "The team's id, or default for the one team in local mode."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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

	req, ok := opTeamSettingsPatch.Decode(w, r)
	if !ok {
		return
	}
	apply, selectionNamed, ok := resolveTeamSettingsPatch(w, req)
	if !ok {
		return
	}

	var (
		prevModel  string
		savedModel string
		saved      domain.TeamSettings
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// The write below stores every field the body does not name as this
		// read found it. The lock makes the read wait for another save of the
		// team's settings in flight, so this one cannot store back a field as
		// it was before that save.
		if err := tx.Teams.LockSettings(r.Context(), teamID); err != nil {
			return err
		}
		teamSet, err := tx.Teams.GetSettings(r.Context(), teamID)
		if err != nil {
			return fmt.Errorf("load team settings: %w", err)
		}
		prevModel = teamSet.DefaultModel
		// The org row decides whether this write happens at all — both halves of
		// the enable-set check are against it — so a failed read stops the save.
		// A failed read is the check's input missing, not the check passing, and
		// saving through it would persist a set the next dispatch refuses while
		// telling the caller it succeeded. A missing org_settings row is not
		// this case: the store answers that with the schema defaults, which
		// resolve to an org enabling the whole catalog and holding no
		// credential, so they refuse nothing.
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		apply(&teamSet)
		savedModel = teamSet.DefaultModel
		// Only when this save is the one choosing. A team whose default its org
		// disabled is broken, but it was not broken by the admin editing a
		// branch template — refusing them would blame this caller for state
		// they did not create, and the dispatch gate already refuses that
		// team's next run by name. Naming either field is asking for the
		// selection to be judged, and then BOTH are, because narrowing a set
		// and moving a default are one decision.
		if selectionNamed {
			if e := checkTeamModelSelection(teamSet, orgSet); e != nil {
				return e
			}
		}
		// A default this team's next run could not authenticate is not a default
		// worth storing. Only on a change: a save that re-sends a model stored
		// before a credential was disconnected must not be blocked by something
		// this caller did not do (that one is the dispatch's to refuse).
		if savedModel != "" && savedModel != prevModel {
			if e := modelaccess.ForOrg(orgSet).Check(savedModel); e != nil {
				return e
			}
		}
		if saved, err = tx.Teams.UpdateSettings(r.Context(), teamID, teamSet); err != nil {
			return fmt.Errorf("save team settings: %w", err)
		}
		return nil
	}); err != nil {
		if writeFieldFaults(w, err) {
			return
		}
		if models.WriteAccessError(w, err, "ai_model") {
			return
		}
		internalError(w, "settings/team", err)
		return
	}

	resp, ok := s.readTeamSettings(w, r, orgID, userID, teamID, &saved)
	if !ok {
		return
	}
	opTeamSettingsPatch.Write(w, resp)
}

// checkTeamModelSelection holds a team's post-apply model selection to the
// org's enable-set: the team's own set must be a subset of it, and the team's
// default must be a member of the set that results. Both are checked against
// the state this save LANDS on rather than the body alone, which is why it runs
// inside the transaction — a team may be narrowing its set and moving its
// default in one call, and either can invalidate the other, so the pair is
// judged whenever either is named.
//
// Violations are refusals, not warnings, and both fields are named when both
// fail: a save that stored a default the team cannot dispatch would report
// success for a configuration whose only observable effect is a failed run
// later.
func checkTeamModelSelection(teamSet domain.TeamSettings, orgSet domain.OrgSettings) error {
	orgEnabled := domain.OrgModelSet(orgSet.EnabledModels, models.DeploymentUniverse().DefaultEnabled())
	var faults fieldFaults
	var outside []string
	for _, m := range teamSet.EnabledModels {
		if !orgEnabled.Has(m) {
			outside = append(outside, m)
		}
	}
	if len(outside) > 0 {
		faults.invalid("enabled_models", fmt.Sprintf(
			"enabled_models must be a subset of the models this organization enables; %s %s not: the organization enables %s",
			strings.Join(outside, ", "), plural(len(outside), "is", "are"), orgEnabled))
	}
	// Resolved from the team's own stored set so the two fields are judged
	// together: a default legal under the OLD set but not the new one is exactly
	// what a save narrowing both at once has to catch.
	teamEnabled := domain.TeamModelSet(teamSet.EnabledModels, orgEnabled)
	if model := strings.TrimSpace(teamSet.DefaultModel); model != "" && !teamEnabled.Has(model) {
		faults.invalid("ai_model", fmt.Sprintf(
			"ai_model must name a model this team has enabled; %s is not one of %s", model, teamEnabled))
	}
	return faults.orNil()
}

// plural picks between two words by count, so a refusal naming one model reads
// as a sentence rather than as a template.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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
//
// selectionNamed is whether the body names ai_model or enabled_models, which is
// when the write judges the team's model selection against its org's.
func resolveTeamSettingsPatch(w http.ResponseWriter, req teamSettingsPatch) (apply func(*domain.TeamSettings), selectionNamed, ok bool) {
	var (
		shape    httpx.Validation
		ranges   httpx.Validation
		names    patchNames
		mutators []func(*domain.TeamSettings)
		defaults = domain.DefaultTeamSettingsFor(runmode.Current() == runmode.ModeMulti)
	)
	set := func(f func(*domain.TeamSettings)) { mutators = append(mutators, f) }

	// ai_model must name a model this deployment offers. The picker draws its
	// options from the same universe, so the closed set is the one the UI shows
	// — and a stored value that is dispatched verbatim has no room for a
	// spelling nothing can invoke, nor for one in the other deployment
	// vocabulary's spelling.
	//
	// It cannot be cleared, and this is the one PATCH field on either scope
	// where null is refused rather than meaning "reset". There is nothing to
	// reset TO: no org-level team default exists, and nothing resolves an empty
	// one at dispatch, so a cleared team is a team whose every unpinned step
	// fails. The provisioning default seeds the column and is not a fallback —
	// picking it here would spend on a choice nobody made. A team changing its
	// mind names the new model; a team that wants fewer choices narrows
	// enabled_models beside this.
	if v, st := req.AIModel.Read(&shape, "ai_model"); names.named(st) {
		selectionNamed = true
		universe := models.DeploymentUniverse()
		switch {
		case st == httpx.PatchClear || v == "":
			shape.Invalid("ai_model", "ai_model must name a model this deployment offers and cannot be cleared — a team with no default has no model to run its unpinned steps on: "+strings.Join(universe.Keys(), ", "))
		case !universe.Offers(v):
			shape.Invalid("ai_model", "ai_model must name a model this deployment offers: "+strings.Join(universe.Keys(), ", "))
		default:
			set(func(t *domain.TeamSettings) { t.DefaultModel = v })
		}
	}
	// enabled_models is the team's own enable-set — which of the models its org
	// enables this team may pick from. The list IS the value: a set replaces
	// wholesale, and null clears it back to inheriting the org's whole set.
	//
	// Only the SHAPE is decided here. Whether the set is one the org actually
	// enables needs the org's row, so it is checked inside the write's
	// transaction against the state this save lands on.
	if v, st := req.EnabledModels.Read(&shape, "enabled_models"); names.named(st) {
		selectionNamed = true
		switch st {
		case httpx.PatchClear:
			set(func(t *domain.TeamSettings) { t.EnabledModels = nil })
		case httpx.PatchSet:
			if enabled, ok := normalizeModelSet(&shape, models.DeploymentUniverse(), v, "enabled_models"); ok {
				set(func(t *domain.TeamSettings) { t.EnabledModels = enabled })
			}
		}
	}
	if v, st := req.AIAutoDelegate.Read(&shape, "ai_auto_delegate_enabled"); names.named(st) {
		next := defaults.AutoDelegateEnabled
		if st == httpx.PatchSet {
			next = v
		}
		set(func(t *domain.TeamSettings) { t.AutoDelegateEnabled = next })
	}
	if v, st := req.AutoModeEnabled.Read(&shape, "auto_mode_enabled"); names.named(st) {
		next := defaults.AutoModeEnabled
		if st == httpx.PatchSet {
			next = v
		}
		set(func(t *domain.TeamSettings) { t.AutoModeEnabled = next })
	}
	if v, st := req.ClaudeAIConnectorsEnabled.Read(&shape, "claude_ai_connectors_enabled"); names.named(st) {
		next := defaults.ClaudeAIConnectorsEnabled
		if st == httpx.PatchSet {
			next = v
		}
		set(func(t *domain.TeamSettings) { t.ClaudeAIConnectorsEnabled = next })
	}
	if v, st := req.AIReprioritizeThreshold.Read(&shape, "ai_reprioritize_threshold"); names.named(st) {
		next := defaults.AIReprioritizeThreshold
		if st == httpx.PatchSet {
			if v <= 0 {
				ranges.OutOfRange("ai_reprioritize_threshold", "ai_reprioritize_threshold must be greater than 0")
			}
			next = v
		}
		set(func(t *domain.TeamSettings) { t.AIReprioritizeThreshold = next })
	}
	if v, st := req.AIPreferenceUpdateInterval.Read(&shape, "ai_preference_update_interval"); names.named(st) {
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
	if v, st := req.BranchTemplate.Read(&shape, "branch_template"); names.named(st) {
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
	if v, st := req.ReviewPosture.Read(&shape, "review_posture"); names.named(st) {
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
	if v, st := req.BaseBranchPushPolicy.Read(&shape, "base_branch_push_policy"); names.named(st) {
		next := defaults.BaseBranchPushPolicy
		if st == httpx.PatchSet {
			if !domain.ValidBaseBranchPushPolicy(v) {
				shape.Invalid("base_branch_push_policy", "base_branch_push_policy must be one of: "+strings.Join(domain.ValidBaseBranchPushPolicies, ", "))
			}
			next = v
		}
		set(func(t *domain.TeamSettings) { t.BaseBranchPushPolicy = next })
	}
	if v, st := req.PermissionAbsentAutodenyEnabled.Read(&shape, "permission_absent_autodeny_enabled"); names.named(st) {
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
	if v, st := req.PermissionAbsentGraceSeconds.Read(&shape, "permission_absent_grace_seconds"); names.named(st) {
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

	// A PATCH that names no field wrote nothing, so it must not answer as
	// though it did — the one response a client can't tell from a real save.
	// Such a body recorded no fault above, so this is its only answer.
	if !names.any {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no fields to update: name at least one settings field (null clears it)",
		})
		return nil, false, false
	}
	if shape.Flush(w, http.StatusBadRequest) {
		return nil, false, false
	}
	if ranges.Flush(w, http.StatusUnprocessableEntity) {
		return nil, false, false
	}
	return func(t *domain.TeamSettings) {
		for _, m := range mutators {
			m(t)
		}
	}, selectionNamed, true
}

// patchNames records, as a PATCH body's fields are read, whether the body
// named any of them.
type patchNames struct{ any bool }

// named reports whether st is a field the body named, and records it if so.
func (n *patchNames) named(st httpx.PatchState) bool {
	if st == httpx.PatchAbsent {
		return false
	}
	n.any = true
	return true
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
	GitHubBaseURL       string `json:"github_base_url" doc:"The base URL of the organization's GitHub, empty when none is configured."`
	GitHubPollInterval  string `json:"github_poll_interval" doc:"How often GitHub is polled, as a Go duration such as 5m0s."`
	GitHubCloneProtocol string `json:"github_clone_protocol" doc:"How repositories are cloned, ssh or https; always https in multi mode."`
	HasGitHubPAT        bool   `json:"has_github_pat" doc:"Whether the organization has a GitHub personal access token connected."`
	// GitHubPATLogin is the @login the org's stored bot PAT authenticates as —
	// the credential's own identity, not the caller's. Not a secret (it's the
	// account name that shows up as the commit author on delegated work), and
	// it's the context that makes replacing the token from Settings feel safe:
	// you can see which account you're about to swap out. Empty (omitted) when
	// no PAT is bound, or when the bind predates the login being recorded (it
	// self-heals on the next bind).
	GitHubPATLogin   string `json:"github_pat_login,omitempty" doc:"The GitHub login the organization's personal access token authenticates as, absent when no token is connected or its login was not recorded."`
	JiraBaseURL      string `json:"jira_base_url" doc:"The base URL of the organization's Jira site, empty when none is configured."`
	JiraPollInterval string `json:"jira_poll_interval" doc:"How often Jira is polled, as a Go duration such as 5m0s."`
	// LinearPollInterval is the Linear poll cadence. Linear is SaaS-only, so
	// unlike its siblings it has no base URL beside it.
	LinearPollInterval string `json:"linear_poll_interval" doc:"How often Linear is polled, as a Go duration such as 5m0s."`
	// HasJiraCredential reports whether a usable Jira service credential is
	// stored for the org's auth-method marker — a Data Center PAT or a Cloud
	// email + API token — rather than the presence of a specific key, so a
	// Cloud org (which has no PAT) still reports true.
	HasJiraCredential bool `json:"has_jira_credential" doc:"Whether the organization has a Jira credential connected, a Data Center personal access token or a Cloud email and API token."`
	// EnabledModels is the org's STORED enable-set, or null when it has
	// expressed no preference. Deliberately not the resolved set: the models
	// read (GET /api/orgs/{org_id}/models) is what answers "which models are
	// enabled right now", and publishing the resolution here too would give a
	// client two places to ask one question and a way to see them disagree.
	//
	// Always emitted (not omitempty): null is the org's "no preference", which
	// is a state the settings form has to render, and an omitted field would
	// read to a client as "unchanged".
	EnabledModels []string `json:"enabled_models" doc:"The model keys the organization enables for its teams as stored, or null when it has chosen none and every model the deployment offers is enabled; GET /api/orgs/{org_id}/models returns the models enabled now."`
	// BackgroundJobsModel is the model the scorer and repo
	// profiler run on — a catalog key. Always emitted (not omitempty): "" is
	// the org's "not picked yet", which is the state the settings form has to
	// render, and an omitted field would read to a client as "unchanged".
	BackgroundJobsModel string `json:"background_jobs_model" doc:"The model key the organization's background jobs run on (priority scoring, repository profiling, and memory writing), empty when none is chosen, in which case those jobs do not run."`
	// MaxDailyCostUSD and MaxConcurrentRuns are always emitted, null for
	// none, so a client reads "no limit" in the spelling the PATCH writes it.
	MaxDailyCostUSD   *float64 `json:"max_daily_cost_usd" doc:"The organization's LLM spend cap per UTC day, in USD, or null when there is no cap."`
	MaxConcurrentRuns *int     `json:"max_concurrent_runs" doc:"The most of the organization's runs that may run at once, or null when there is no limit."`
	// APITokenMaxAgeDays is emitted the same way as the two caps above, and
	// is readable by any org member, not just the admin who can write it: the
	// cap is policy a token minter has to see before choosing an expiry, and
	// the create form states it.
	APITokenMaxAgeDays *int `json:"api_token_max_age_days" doc:"The longest any of the organization's API tokens may live, in days, or null when there is no maximum."`
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
	LLMAuthMethod      string `json:"llm_auth_method" doc:"Where the organization's Claude credentials come from: system for the host's own, or byok for the credentials the organization connected; always byok in multi mode."`
	HasAnthropicAPIKey bool   `json:"has_anthropic_api_key" doc:"Whether the organization has an Anthropic API key connected."`
	HasBedrockCreds    bool   `json:"has_bedrock_credentials" doc:"Whether the organization has Amazon Bedrock credentials connected."`
	// Bedrock non-secret config (TFAC-68). The credential itself never
	// leaves the vault — presence rides has_bedrock_credentials and the
	// method marker below; these three let the Settings form render the
	// current region / model / endpoint without a secrets round-trip.
	BedrockAuthMethod string `json:"bedrock_auth_method,omitempty" doc:"How the organization's Bedrock credentials authenticate: role, bearer, or access_keys, absent when none are connected."` // "role" | "bearer" | "access_keys"
	BedrockRegion     string `json:"bedrock_region,omitempty" doc:"The AWS region Bedrock is called in, absent when none is stored."`
	BedrockModelID    string `json:"bedrock_model_id,omitempty" doc:"The Bedrock model id the organization stored, absent when none is stored."`
	BedrockBaseURL    string `json:"bedrock_base_url,omitempty" doc:"The endpoint Bedrock is called at in place of AWS's own, absent when none is stored."`
	// Role-mode (TFAC-616) non-secret config: the customer role ARN and the
	// TF-generated External ID, so the settings form re-renders the role card
	// and the copyable trust-policy snippet without a round-trip to the
	// role-setup endpoint. Both empty unless the org is in role mode.
	BedrockRoleARN    string `json:"bedrock_role_arn,omitempty" doc:"The ARN of the IAM role Triage Factory assumes to call Bedrock, absent unless the organization connected Bedrock with a role."`
	BedrockExternalID string `json:"bedrock_external_id,omitempty" doc:"The External ID Triage Factory sends when it assumes that role, absent until one is generated."`
	// MemberCount is the number of members in this org. Feeds the
	// frontend's N=1 collapse alongside the team member count. A property
	// of the org, so it rides the org-scope response rather than /api/me.
	MemberCount int `json:"member_count" doc:"How many people are members of the organization."`
	// Version is the org_settings row's optimistic-concurrency token. The read
	// hands it out and the PATCH requires it back: the settings save is a
	// read-modify-write over org_settings' own columns, so without it two
	// admins editing different sections silently undo each other. A PATCH
	// carrying a stale token is refused with 409 VERSION_CONFLICT and the
	// loser refetches — this field is what the refetch is FOR, so it is not
	// omitempty.
	//
	// It does NOT cover github_base_url / github_poll_interval / jira_base_url
	// / jira_poll_interval / linear_poll_interval: those live on
	// org_event_sources, and this
	// route writes them in the same transaction as the guarded org_settings
	// update — a stale token still refuses the whole save with nothing
	// written, by ordinary transaction atomicity, with no second token
	// needed. What it does NOT gate is a concurrent write to the same fields
	// through the admin-only per-source route (PATCH
	// /api/orgs/{org_id}/sources/{kind}), which is an unguarded,
	// last-writer-wins upsert per source — the same concurrency shape that
	// route's disabled switch already has.
	Version int `json:"version" doc:"The settings' version, which an update must send and which each update raises."`
	// Warning is advisory prose about a save that SUCCEEDED — today, that a
	// narrowed enable-set has disabled a model some team still selects. Only the
	// PATCH response carries it; the GET leaves it empty and omitempty drops it,
	// so the read and the write answer one shape.
	Warning string `json:"warning,omitempty" doc:"On an update that disabled a model some team still selects, a description naming those teams, whose new runs fail until they choose again; absent otherwise."`
}

var opOrgSettingsGet = httpx.NewJSONOp[httpx.NoBody, orgSettingsResponse](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/settings",
	ID:          "get_org_settings",
	Summary:     "Get an organization's settings",
	Description: "Returns the organization's configuration: its GitHub, Jira, and Linear hosts and polling, its model and spend settings, which credentials it has connected, its member count, and the version an update must send. No secret is returned.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	opOrgSettingsGet.Write(w, resp)
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

	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
		// every PAT bind. Only meaningful while a PAT is the credential — an App
		// org's bot login (<slug>[bot]) resolves live from the registration, and
		// a name that names the wrong account is worse than no name on a surface
		// whose whole job is "here's what you're about to replace". Best-effort
		// like the Bedrock reads below: a read failure degrades the form to
		// "connected" without a name, not a 500.
		if creds.GitHubPAT != "" {
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

	memberCount, err := s.az.OrgMemberCount(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "settings/org", err)
		return out, false
	}

	// Marker-based "is Jira connected" (matches the integrations-status signal),
	// so a Cloud org with no PAT still reports a stored credential.
	_, hasJiraCred := integrations.JiraSystemConfig(creds, orgSet.JiraBaseURL)

	return orgSettingsResponse{
		GitHubBaseURL:       orgSet.GitHubBaseURL,
		GitHubPollInterval:  orgSet.GitHubPollInterval.String(),
		GitHubCloneProtocol: defaultedCloneProtocolView(orgSet.GitHubCloneProtocol),
		HasGitHubPAT:        creds.GitHubPAT != "",
		GitHubPATLogin:      ghPATLogin,
		JiraBaseURL:         orgSet.JiraBaseURL,
		JiraPollInterval:    orgSet.JiraPollInterval.String(),
		LinearPollInterval:  orgSet.LinearPollInterval.String(),
		HasJiraCredential:   hasJiraCred,
		EnabledModels:       orgSet.EnabledModels,
		BackgroundJobsModel: orgSet.BackgroundJobsModel,
		MaxDailyCostUSD:     limitOrNull(orgSet.MaxDailyCostUSD),
		MaxConcurrentRuns:   limitOrNull(orgSet.MaxConcurrentRuns),
		APITokenMaxAgeDays:  limitOrNull(orgSet.APITokenMaxAgeDays),
		LLMAuthMethod:       domain.EffectiveLLMAuthMethod(orgSet.LLMAuthMethod, runmode.Current() == runmode.ModeMulti),
		HasAnthropicAPIKey:  orgSet.AnthropicAPIKeyRef != "",
		HasBedrockCreds:     orgSet.BedrockCredentialsRef != "",
		BedrockAuthMethod:   bedrockAuthMethodFromRef(orgSet.BedrockCredentialsRef),
		BedrockRegion:       bedrockRegion,
		BedrockModelID:      bedrockModelID,
		BedrockBaseURL:      bedrockBaseURL,
		BedrockRoleARN:      bedrockRoleARN,
		BedrockExternalID:   bedrockExternalID,
		MemberCount:         memberCount,
		Version:             orgSet.Version,
	}, true
}

// orgSettingsPatch is the body of PATCH /api/orgs/{org_id}/settings — the org's
// PURE CONFIG. No secrets: the GitHub PAT, the Jira service credential and the
// LLM provider material each live on their own resource (see org_credentials.go
// and llm_credentials.go), so this route touches no vault key, makes no
// outbound call, and cannot revoke access as a side effect. A credential field
// in the body is a 400 UNKNOWN_FIELD, not a silent store.
//
// Every field is a Patch under the one clearing convention: absent keeps,
// explicit null clears (a base URL, a cap) or resets to the shipped default (a
// poll interval, the clone protocol), any other value is applied and
// validated. There is no empty-string sentinel and no zero-means-clear: an
// explicit 0 cap is a 422, because "cap at $0" and "no cap" are different
// intents and the caller has a spelling for the second one.
type orgSettingsPatch struct {
	GitHubBaseURL       httpx.Patch[string]   `json:"github_base_url" doc:"The base URL of the organization's GitHub, an http or https URL, or null to clear it; it cannot change while a GitHub credential is connected."`
	GitHubPollInterval  httpx.Patch[string]   `json:"github_poll_interval" doc:"How often to poll GitHub, as a Go duration of at least 30s such as 5m, or null to reset it to 5m."`
	GitHubCloneProtocol httpx.Patch[string]   `json:"github_clone_protocol" doc:"How to clone repositories, ssh or https, or null to reset it to https; ssh is refused in multi mode, and switching to it in local mode first checks that SSH reaches the organization's GitHub."`
	JiraBaseURL         httpx.Patch[string]   `json:"jira_base_url" doc:"The base URL of the organization's Jira site, an http or https URL, or null to clear it; it cannot change while a Jira credential is connected."`
	JiraPollInterval    httpx.Patch[string]   `json:"jira_poll_interval" doc:"How often to poll Jira, as a Go duration of at least 30s such as 5m, or null to reset it to 5m."`
	LinearPollInterval  httpx.Patch[string]   `json:"linear_poll_interval" doc:"How often to poll Linear, as a Go duration of at least 30s such as 5m, or null to reset it to 5m."`
	EnabledModels       httpx.Patch[[]string] `json:"enabled_models" doc:"The model keys the organization's teams may choose from, a non-empty list without duplicates drawn from the models the deployment offers, or null to enable every model the deployment offers, including ones it adds later."`
	BackgroundJobsModel httpx.Patch[string]   `json:"background_jobs_model" doc:"The model key the organization's background jobs run on, one the deployment offers, or null to stop running background jobs; a model from a provider the organization has not connected is refused."`
	LLMAuthMethod       httpx.Patch[string]   `json:"llm_auth_method" doc:"Where the organization's Claude credentials come from, system or byok; system is refused in multi mode and while the organization has an LLM credential connected, and null is refused."`
	MaxDailyCostUSD     httpx.Patch[float64]  `json:"max_daily_cost_usd" doc:"The organization's LLM spend cap per UTC day, in USD, greater than 0, or null for no cap."`
	MaxConcurrentRuns   httpx.Patch[int]      `json:"max_concurrent_runs" doc:"The most of the organization's runs that may run at once, from 1 to 1000000, or null for no limit."`
	APITokenMaxAgeDays  httpx.Patch[int]      `json:"api_token_max_age_days" doc:"The longest any of the organization's API tokens may live, in days, from 1 to 365, or null for no maximum; a lower value shortens existing tokens at once."`
	// Version is the token the caller read this row at. Required, and a plain
	// int rather than a patch field: it is not something you can clear, and a
	// PATCH without it would be exactly the unconditional last-write-wins save
	// this route stopped being.
	Version *int `json:"version" doc:"The version the settings read returned, which is required; an update made against an older version is refused."`
}

var opOrgSettingsPatch = httpx.NewJSONOp[orgSettingsPatch, orgSettingsResponse](httpx.OpSpec{
	Pattern:     "PATCH /api/orgs/{org_id}/settings",
	ID:          "update_org_settings",
	Summary:     "Update an organization's settings",
	Description: "Sets the organization settings the body names, or clears or resets those set to null, and returns the settings as the read does. The body must name at least one field and carry the version the read returned; a change to a host or poll interval restarts that source's polling, and switching to SSH cloning in local mode first tests SSH against GitHub.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

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

	req, ok := opOrgSettingsPatch.Decode(w, r)
	if !ok {
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
	apply, hosts, ok := s.resolveOrgSettingsPatch(w, r, orgID, req)
	if !ok {
		return
	}

	// A save that names github_base_url holds the lock every GitHub credential
	// transition takes, from the check below to the write, so a credential
	// cannot be bound between the check that the host may move and the move.
	if hosts.github {
		release, err := s.acquireKeyedLock(r.Context(), &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
		if err != nil {
			internalError(w, "settings/org", err)
			return
		}
		defer release()
	}
	// The same for jira_base_url and the org's Jira credential lock. Taken
	// after the GitHub lock when both are named; nothing holding the Jira lock
	// waits on the GitHub one.
	if hosts.jira {
		release, err := s.lockJiraCredential(r.Context(), orgID)
		if err != nil {
			internalError(w, "settings/org", err)
			return
		}
		defer release()
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
		// A save loaded before another write is answered as stale before
		// anything else is checked: the checks below judge the row this save
		// never saw, and the caller has to reload either way. The versioned
		// write below is still what enforces it.
		if cur.Version != *req.Version {
			return db.ErrOrgSettingsVersion
		}
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
			if e := modelaccess.ForOrg(cur).Check(cur.BackgroundJobsModel); e != nil {
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
		// The GitHub host moves only while no GitHub credential is connected:
		// a token or an App works only on the GitHub that issued it, so the
		// org's host is the one place a credential's host is recorded. Compared
		// as stored, which is canonical, so re-sending the current value under
		// another spelling is not a move.
		if cur.GitHubBaseURL != prevOrgSet.GitHubBaseURL {
			connected, err := githubCredentialConnected(r.Context(), tx, orgID, db.EffectiveGitHubHost(prevOrgSet.GitHubBaseURL))
			if err != nil {
				return err
			}
			if connected != "" {
				return fmt.Errorf("%s is connected: %w", connected, errGitHubHostHasCredential)
			}
		}
		// The Jira site, likewise, moves only while no Jira service credential
		// is stored. Per-user Jira credentials are keyed by site, so they do
		// not hold it in place.
		if cur.JiraBaseURL != prevOrgSet.JiraBaseURL {
			creds, err := integrations.Load(r.Context(), tx.Secrets, orgID)
			if err != nil {
				return fmt.Errorf("load jira credential: %w", err)
			}
			if _, connected := integrations.JiraSystemConfig(creds, prevOrgSet.JiraBaseURL); connected {
				return fmt.Errorf("the org's Jira credential is connected to %s: %w", prevOrgSet.JiraBaseURL, errJiraSiteHasCredential)
			}
		}
		orgSet, err = tx.Orgs.UpdateSettingsVersioned(r.Context(), orgID, cur, *req.Version)
		return err
	})
	if models.WriteAccessError(w, err, "background_jobs_model") {
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
	if errors.Is(err, errGitHubHostHasCredential) {
		// 409, beside the other refusals this route makes over the state of
		// the org's GitHub connection: the value is legal, the connection in
		// place is what stands in the way, and disconnecting it is the fix.
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason: httpx.ReasonConflict, Message: err.Error(), Field: "github_base_url",
		})
		return
	}
	if errors.Is(err, errJiraSiteHasCredential) {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason: httpx.ReasonConflict, Message: err.Error(), Field: "jira_base_url",
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
	linearChanged := orgSet.LinearPollInterval != prevOrgSet.LinearPollInterval

	if ghChanged && s.onGitHubChanged != nil {
		s.MarkGitHubRestarted(r.Context(), orgID)
		go s.onGitHubChanged(orgID)
	}
	if jiraChanged && s.onJiraChanged != nil {
		s.MarkJiraRestarted(r.Context(), orgID)
		go s.onJiraChanged(orgID)
	}
	if linearChanged && s.onLinearChanged != nil {
		s.MarkLinearRestarted(r.Context(), orgID)
		go s.onLinearChanged(orgID)
	}

	// A background-jobs model this org did not have is the setting every
	// memory generation that failed `no_model` was waiting on, so the save is
	// the moment to re-run them — the org-wide doorbell, which sweeps the
	// backlog ignoring the attempt backoff rather than letting each owed
	// conversation age out of it one at a time. Mirrors the scorer's own
	// config-save trigger, and is computed from what this save CHANGED so a
	// re-send of the stored value rings nothing.
	if orgSet.BackgroundJobsModel != "" && orgSet.BackgroundJobsModel != prevOrgSet.BackgroundJobsModel {
		s.kickMemoryOwed(orgID, "")
	}

	resp, ok := s.readOrgSettings(w, r, orgID, userID, &orgSet)
	if !ok {
		return
	}
	// Narrowing the set doesn't block the save — the admin has authority, and
	// the teams whose configuration it invalidates are ones they cannot edit —
	// but the teams it breaks are named, because the only other way they learn
	// is a failed run. Computed from what this save actually REMOVED, so a save
	// that widens the set, or that touches something else entirely, says
	// nothing.
	if removed := modelsRemovedBySave(prevOrgSet, orgSet); len(removed) > 0 {
		resp.Warning = s.disabledModelsWarning(r.Context(), orgID, userID, removed)
	}
	opOrgSettingsPatch.Write(w, resp)
}

// orgHostsNamed is which of the org's source hosts a settings PATCH names.
type orgHostsNamed struct{ github, jira bool }

// resolveOrgSettingsPatch validates the body once and returns the mutation it
// describes. Two statuses for two fault classes, exactly as the team sibling:
// shape and vocabulary answer 400, a well-formed value outside its band answers
// 422. A check that needs the world rather than the body — the SSH preflight —
// runs here too, before any transaction is open, because it takes fifteen
// seconds. The GitHub host's own check runs inside the write, against the row
// it lands on (handleOrgSettingsPatch).
//
// On any failure the response is already written and ok is false.
func (s *Server) resolveOrgSettingsPatch(w http.ResponseWriter, r *http.Request, orgID string, req orgSettingsPatch) (apply func(*domain.OrgSettings), hosts orgHostsNamed, ok bool) {
	var (
		shape    httpx.Validation
		ranges   httpx.Validation
		names    patchNames
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
	for _, f := range []struct {
		patch httpx.Patch[string]
		field string
		kind  string
		named *bool
		set   func(*domain.OrgSettings, string)
	}{
		{req.GitHubBaseURL, "github_base_url", eventsource.KindGitHub, &hosts.github,
			func(o *domain.OrgSettings, v string) { o.GitHubBaseURL = v }},
		{req.JiraBaseURL, "jira_base_url", eventsource.KindJira, &hosts.jira,
			func(o *domain.OrgSettings, v string) { o.JiraBaseURL = v }},
	} {
		v, st := f.patch.Read(&shape, f.field)
		if !names.named(st) {
			continue
		}
		*f.named = true
		if !eventsource.HasHost(f.kind) {
			shape.Invalid(f.field, f.field+" cannot be set: "+f.kind+" has no self-host option")
			continue
		}
		apply := f.set
		switch st {
		case httpx.PatchClear:
			set(func(o *domain.OrgSettings) { apply(o, "") })
		case httpx.PatchSet:
			// Non-empty hosts go through the same validator + canonicalizer the
			// reachability probe uses, so what the column holds is what the probe
			// accepted. Persisting verbatim let a value the probe merely tolerated
			// ("  https://ghes.example.com/ ") through, and the App/PAT host
			// derivation reads the column, not the probe's answer. A blank string
			// is not the clear — null is — so it fails here like any other
			// unusable URL.
			base, valid := ghbase.NormalizeBaseURL(v)
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
	// base_url loop above is gated on HasHost: github, jira and linear are all
	// polled today, so the check always passes, but it is the registry — not
	// this route's hardcoded fields — that decides.
	for _, f := range []struct {
		patch httpx.Patch[string]
		field string
		kind  string
		def   time.Duration
		set   func(*domain.OrgSettings, time.Duration)
	}{
		{req.GitHubPollInterval, "github_poll_interval", eventsource.KindGitHub, defaults.GitHubPollInterval,
			func(o *domain.OrgSettings, d time.Duration) { o.GitHubPollInterval = d }},
		{req.JiraPollInterval, "jira_poll_interval", eventsource.KindJira, defaults.JiraPollInterval,
			func(o *domain.OrgSettings, d time.Duration) { o.JiraPollInterval = d }},
		{req.LinearPollInterval, "linear_poll_interval", eventsource.KindLinear, defaults.LinearPollInterval,
			func(o *domain.OrgSettings, d time.Duration) { o.LinearPollInterval = d }},
	} {
		v, st := f.patch.Read(&shape, f.field)
		if !names.named(st) {
			continue
		}
		if !eventsource.Polled(f.kind) {
			shape.Invalid(f.field, f.field+" cannot be set: "+f.kind+" has no poll cadence")
			continue
		}
		next := f.def
		if st == httpx.PatchSet {
			d, err := parseMinDuration(v, poller.MinPollInterval)
			if err != nil {
				ranges.OutOfRange(f.field, fmt.Sprintf(
					"%s must be a duration of at least %s (e.g. \"5m\"), or null for the default",
					f.field, poller.MinPollInterval))
			}
			next = d
		}
		apply := f.set
		set(func(o *domain.OrgSettings) { apply(o, next) })
	}
	if v, st := req.GitHubCloneProtocol.Read(&shape, "github_clone_protocol"); names.named(st) {
		// The cleared-to-default value goes through the same resolver the read
		// side does, so a null clears to what this deployment can actually
		// honor. The package default is "https" in both modes today, so the
		// resolver is a no-op on it — it stays because the default is a local
		// concern and the resolver is what keeps a future one that multi
		// cannot honor from being planted here by an explicit clear.
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
	// enabled_models is the org's enable-set: which of the models this
	// deployment offers its teams may pick from. The list IS the value — a set
	// replaces wholesale rather than merging — and null resets it to the whole
	// universe, the state an org that has never expressed a preference is in,
	// and the state that keeps tracking new models as releases add them.
	//
	// A save may disable a model some team currently selects, and it SUCCEEDS
	// anyway, with a warning naming that team. Refusing until every team
	// re-picked would couple an org admin to team settings they are barred from
	// editing. Nothing is grandfathered either: those teams' new claims fail at
	// dispatch, by name.
	if v, st := req.EnabledModels.Read(&shape, "enabled_models"); names.named(st) {
		switch st {
		case httpx.PatchClear:
			set(func(o *domain.OrgSettings) { o.EnabledModels = nil })
		case httpx.PatchSet:
			if enabled, ok := normalizeModelSet(&shape, models.DeploymentUniverse(), v, "enabled_models"); ok {
				set(func(o *domain.OrgSettings) { o.EnabledModels = enabled })
			}
		}
	}
	// background_jobs_model must name a model this deployment offers. The picker
	// draws from the same universe and the jobs dispatch the stored value
	// verbatim, so there is no room for a spelling nothing can invoke. The R5
	// delegation gates (tool support, a 64k window) deliberately do NOT narrow
	// it — these jobs are toolless and short-context, so every offered model is
	// a legitimate choice.
	//
	// Where the id names the provider that serves it, whether the org has
	// connected that provider is checked in the transaction below, against the
	// row this write is landing on. An SDK alias names none — the harness
	// resolves the path from the credential — so that check has nothing to bite
	// on there and the universe above is the whole gate.
	//
	// Null clears it, and clearing is a real intent: it turns the background
	// jobs off, which is the only way to stop them short of unbinding the
	// credential every other feature shares. The jobs then skip with a warning
	// naming this setting — never a model of TF's choosing.
	if v, st := req.BackgroundJobsModel.Read(&shape, "background_jobs_model"); names.named(st) {
		universe := models.DeploymentUniverse()
		switch {
		case st == httpx.PatchClear:
			set(func(o *domain.OrgSettings) { o.BackgroundJobsModel = "" })
		case !universe.Offers(v):
			shape.Invalid("background_jobs_model", "background_jobs_model must name a model this deployment offers, or be null to stop running background jobs: "+strings.Join(universe.Keys(), ", "))
		default:
			set(func(o *domain.OrgSettings) { o.BackgroundJobsModel = v })
		}
	}
	// llm_auth_method names where the org's Claude credentials come from, and
	// it is the one settings field whose legal values depend on the deployment:
	// "system" means the agent subprocess inherits the host's environment, and
	// a multi-mode deployment's environment is one environment shared by every
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
	if v, st := req.LLMAuthMethod.Read(&shape, "llm_auth_method"); names.named(st) {
		method := v
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
	if v, st := req.MaxDailyCostUSD.Read(&shape, "max_daily_cost_usd"); names.named(st) {
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
	if v, st := req.MaxConcurrentRuns.Read(&shape, "max_concurrent_runs"); names.named(st) {
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
	// The ceiling on how long any of the org's API tokens may live. Null is
	// "no maximum" and 0 is refused, the third field on this route to take
	// that convention — and here the band starts at 1 for a second reason: a
	// cap of zero days would expire every token the moment it was minted,
	// which is a way to break the org's automation by typing a number the
	// blank field already has a spelling for.
	//
	// Range-checked here even though the Postgres column carries the same
	// CHECK. The DB is the backstop, not the gate: a CHECK violation is a 500
	// with nothing naming the field, and the SQLite twin has no CHECK at all.
	//
	// Lowering the cap is not softened and takes no confirmation: it is
	// applied at use, so it immediately shortens every existing token in the
	// org. That is what the control is for; the UI states the consequence.
	if v, st := req.APITokenMaxAgeDays.Read(&shape, "api_token_max_age_days"); names.named(st) {
		next := 0
		if st == httpx.PatchSet {
			if v < domain.APITokenMaxAgeDaysMin || v > domain.APITokenMaxAgeDaysMax {
				ranges.OutOfRange("api_token_max_age_days", fmt.Sprintf(
					"api_token_max_age_days must be between %d and %d, or null for no maximum",
					domain.APITokenMaxAgeDaysMin, domain.APITokenMaxAgeDaysMax))
			}
			next = v
		}
		set(func(o *domain.OrgSettings) { o.APITokenMaxAgeDays = next })
	}

	// A body naming no field recorded no fault above, so this is its only
	// answer, as on the team route.
	if !names.any {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no fields to update: name at least one settings field (null clears it)",
		})
		return nil, hosts, false
	}
	if shape.Flush(w, http.StatusBadRequest) {
		return nil, hosts, false
	}
	if ranges.Flush(w, http.StatusUnprocessableEntity) {
		return nil, hosts, false
	}

	if !s.orgSettingsSSHPreflight(w, r, orgID, mutators) {
		return nil, hosts, false
	}

	return func(o *domain.OrgSettings) {
		for _, m := range mutators {
			m(o)
		}
	}, hosts, true
}

// orgSettingsSSHPreflight gates the transition INTO SSH clone mode. Local-mode
// only — PreflightSSH writes the container's ~/.ssh/known_hosts and probes the
// operator's ssh-agent, neither of which belongs in a multi-mode runtime. In multi
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
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	if sshHost == "" {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: "SSH cloning needs the organization's GitHub URL, to check SSH access against — set it first",
			Field:   "github_clone_protocol",
		})
		return false
	}
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

// modelsRemovedBySave lists the models this save took OUT of the org's
// effective set — resolved on both sides, so clearing a stored set (which
// widens to the deployment's whole universe) removes nothing and setting one for the first
// time removes everything it leaves out.
func modelsRemovedBySave(before, after domain.OrgSettings) []string {
	universe := models.DeploymentUniverse()
	prev := domain.OrgModelSet(before.EnabledModels, universe.DefaultEnabled())
	next := domain.OrgModelSet(after.EnabledModels, universe.DefaultEnabled())
	var removed []string
	for _, key := range prev.Keys() {
		if !next.Has(key) {
			removed = append(removed, key)
		}
	}
	return removed
}

// disabledModelsWarning names the teams whose stored configuration still points
// at a model this save disabled — their default, or their own enable-set. Those
// teams keep running whatever is already in flight and fail at the next claim,
// by name, so the warning is what turns a delayed failure into something the
// admin can see at the moment they cause it.
//
// Best-effort: empty when nothing is affected or a read fails. It is prose
// about a save that already succeeded, and a lookup failure must not turn that
// into an error the caller reads as "not saved".
func (s *Server) disabledModelsWarning(ctx context.Context, orgID, userID string, removed []string) string {
	gone := make(map[string]bool, len(removed))
	for _, m := range removed {
		gone[m] = true
	}

	var affected []string
	err := s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		teams, e := tx.Teams.ListActiveForOrgSystem(ctx, orgID)
		if e != nil {
			return e
		}
		for _, team := range teams {
			set, e := tx.Teams.GetSettingsSystem(ctx, team.ID)
			if e != nil {
				return e
			}
			// The default is reported ahead of the set because it is the
			// sharper break: a team whose default is gone fails every unset
			// step, while one whose set merely names a disabled model keeps
			// running on whatever it defaults to.
			if gone[set.DefaultModel] {
				affected = append(affected, fmt.Sprintf("%s (default %s)", team.Name, set.DefaultModel))
				continue
			}
			for _, m := range set.EnabledModels {
				if gone[m] {
					affected = append(affected, fmt.Sprintf("%s (enabled models name %s)", team.Name, m))
					break
				}
			}
		}
		return nil
	})
	if err != nil || len(affected) == 0 {
		return ""
	}
	if len(affected) == 1 {
		return fmt.Sprintf(
			"%s still selects a model this change disables. Its new runs will fail until it re-picks.",
			affected[0])
	}
	return fmt.Sprintf(
		"%d teams still select models this change disables: %s. Their new runs will fail until they re-pick.",
		len(affected), strings.Join(affected, ", "))
}

// --------------------------------------------------------------------
// Helpers: duration parsing
// (the authorization checks, team-ID resolution, and resolve-error
// rendering live in the authz package)
// --------------------------------------------------------------------

func parseMinDuration(s string, floor time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < floor {
		return 0, fmt.Errorf("duration %s below minimum %s", s, floor)
	}
	return d, nil
}

func (s *Server) registerSettingsRoutes() {
	// The caller's own settings row. Viewer-relative, so the subject comes
	// from the session and there is no id to address.
	s.op(opMeSettingsGet, s.handleMeSettingsGet)
	s.op(opMeSettingsPatch, s.handleMeSettingsPatch)
	// The team's settings row and its Jira project rules, under the team
	// resource so the path segment the caller asserts is the one the
	// authorization check is about. The rules are their own replace-set write
	// rather than a key inside the settings body — a child collection, like
	// the tracked repos and the github-group mappings, not a field.
	s.op(opTeamSettingsGet, s.handleTeamSettingsGet)
	s.op(opTeamSettingsPatch, s.handleTeamSettingsPatch)

	// The org's pure CONFIG — hosts, poller cadence, clone transport, spend
	// governance. Path-scoped like the org's credential resources because
	// the write is admin-gated, and PATCH rather than POST because it is a
	// partial update: absent keeps, null clears, and the caller's `version`
	// makes a concurrent save a 409 instead of a silent overwrite.
	s.op(opOrgSettingsGet, s.handleOrgSettingsGet)
	s.op(opOrgSettingsPatch, s.handleOrgSettingsPatch)
}
