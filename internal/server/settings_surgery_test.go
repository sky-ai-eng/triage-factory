package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The route surgery's own contracts: the org settings row's optimistic
// concurrency, the single clearing convention on both settings PATCHes, the
// poller kicks the fused setup route used to own, and the removal of the routes
// this ticket retired.

// TestOrgSettingsPatch_ConcurrentSaves_OneWinnerOneConflict is the property the
// version column exists for. The settings save is a read-modify-write over the
// whole form, so two admins editing different sections of the page each carried
// the other's fields as their client last read them, and the first one's change
// was undone with a 200 and nothing to notice.
//
// Both writers read the same version, so exactly one may commit; the loser gets
// 409 VERSION_CONFLICT and a refetch shows the winner's write, unmerged. There
// is no server-side merge, because the two writers disagree about fields
// neither of them named.
func TestOrgSettingsPatch_ConcurrentSaves_OneWinnerOneConflict(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	// Both admins load the form at the same version.
	shared := orgSettingsVersion(t, s)

	first := doJSON(t, s, http.MethodPatch, orgSettingsPath(), map[string]any{
		"version": shared, "github_poll_interval": "17m0s",
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first save = %d, want 200; body=%s", first.Code, first.Body.String())
	}

	second := doJSON(t, s, http.MethodPatch, orgSettingsPath(), map[string]any{
		"version": shared, "github_poll_interval": "23m0s",
	})
	if second.Code != http.StatusConflict {
		t.Fatalf("second save at a stale version = %d, want 409; body=%s", second.Code, second.Body.String())
	}
	assertFirstError(t, second, httpx.ReasonVersionConflict, "version")

	// The refetch shows the winner's write untouched, and hands out the token
	// the loser has to re-apply against.
	after := orgSettingsSnapshot(t, s)
	if after["github_poll_interval"] != "17m0s" {
		t.Errorf("after the conflict, github_poll_interval = %v, want the winner's 17m0s", after["github_poll_interval"])
	}
	fresh := orgSettingsVersion(t, s)
	if fresh == shared {
		t.Fatalf("version did not move after a successful save: still %d", fresh)
	}

	// Re-applying at the fresh version is the loser's whole remedy.
	retry := doJSON(t, s, http.MethodPatch, orgSettingsPath(), map[string]any{
		"version": fresh, "github_poll_interval": "23m0s",
	})
	if retry.Code != http.StatusOK {
		t.Fatalf("retry at the fresh version = %d, want 200; body=%s", retry.Code, retry.Body.String())
	}
	if got := orgSettingsSnapshot(t, s)["github_poll_interval"]; got != "23m0s" {
		t.Errorf("after the retry, github_poll_interval = %v, want 23m0s", got)
	}
}

// TestOrgSettingsPatch_VersionRequired: a PATCH with no version is exactly the
// unconditional last-write-wins save this route stopped being, so it is
// refused rather than silently defaulted.
func TestOrgSettingsPatch_VersionRequired(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPatch, orgSettingsPath(), map[string]any{
		"github_poll_interval": "17m0s",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("versionless PATCH = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonMissingField, "version")
}

// TestSettingsPatch_EmptyBodyRefused: a PATCH that names no field wrote
// nothing, and "saved" is the one response a client cannot tell from a real
// write. Both settings routes refuse it.
func TestSettingsPatch_EmptyBodyRefused(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	org := doJSON(t, s, http.MethodPatch, orgSettingsPath(), map[string]any{
		"version": orgSettingsVersion(t, s),
	})
	if org.Code != http.StatusBadRequest {
		t.Errorf("org PATCH naming no field = %d, want 400; body=%s", org.Code, org.Body.String())
	}
	team := doJSON(t, s, http.MethodPatch, teamSettingsPath("default"), map[string]any{})
	if team.Code != http.StatusBadRequest {
		t.Errorf("team PATCH naming no field = %d, want 400; body=%s", team.Code, team.Body.String())
	}
}

// TestOrgSettingsPatch_NullClearsEveryClearableField enumerates the org's
// clearable fields and pins that null clears each one and the clear round-trips
// through the GET. One convention, no empty-string sentinels, no
// zero-means-null — the three encodings this route used to mix.
func TestOrgSettingsPatch_NullClearsEveryClearableField(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	// Seed every field with a non-default value, then clear them all at once.
	patchOrgSettingsOK(t, s, map[string]any{
		"github_base_url":      "https://ghe.example.com",
		"jira_base_url":        "https://jira.example.com",
		"github_poll_interval": "31m0s",
		"jira_poll_interval":   "37m0s",
		// github_clone_protocol is seeded at its own default rather than at
		// "https", because clearing it is a transition INTO ssh mode and that
		// transition runs the local ssh preflight — a real gate, and one no test
		// environment can satisfy. The clear still round-trips; it just starts
		// from the value it ends at.
		"github_clone_protocol": defaultsCloneProtocol(),
		"enabled_models":        []string{domain.ModelAliasSonnet},
		"background_jobs_model": domain.ModelAliasOpus,
		"max_daily_cost_usd":    42.5,
		"max_concurrent_runs":   9,
	})

	defaults := domain.DefaultOrgSettings()
	cleared := []struct {
		field string
		want  any
	}{
		{"github_base_url", ""},
		{"jira_base_url", ""},
		{"github_poll_interval", defaults.GitHubPollInterval.String()},
		{"jira_poll_interval", defaults.JiraPollInterval.String()},
		{"github_clone_protocol", defaults.GitHubCloneProtocol},
		// enabled_models is NOT omitempty on the wire: null is the org's "no
		// preference expressed", which is a state the settings form has to
		// render, and an absent key would read to a client as "unchanged".
		{"enabled_models", nil},
		// background_jobs_model is NOT omitempty on the wire: "" is the state
		// in which the background jobs do not run, and a form that has to
		// render it needs the key present rather than absent.
		{"background_jobs_model", ""},
		{"max_daily_cost_usd", float64(0)},
		{"max_concurrent_runs", float64(0)},
	}

	body := map[string]any{}
	for _, c := range cleared {
		body[c.field] = nil
	}
	patchOrgSettingsOK(t, s, body)

	after := orgSettingsSnapshot(t, s)
	for _, c := range cleared {
		if after[c.field] != c.want {
			t.Errorf("after null, %s = %#v, want %#v", c.field, after[c.field], c.want)
		}
	}
}

// TestTeamSettingsPatch_NullClearsEveryClearableField is the team mirror, and
// it includes ai_model — which previously could not be cleared AT ALL, because
// its wire encoding read a blank as "keep".
func TestTeamSettingsPatch_NullClearsEveryClearableField(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	patchTeamSettingsOK(t, s, "default", map[string]any{
		"ai_model":                           domain.ModelAliasOpus,
		"ai_auto_delegate_enabled":           false,
		"auto_mode_enabled":                  false,
		"ai_reprioritize_threshold":          11,
		"ai_preference_update_interval":      33,
		"branch_template":                    "wip/<ticket-id>",
		"review_posture":                     domain.ReviewPostureAuto,
		"base_branch_push_policy":            domain.BaseBranchPushAlways,
		"permission_absent_autodeny_enabled": false,
		"permission_absent_grace_seconds":    22,
	})

	defaults := domain.DefaultTeamSettingsFor(runmode.Current() == runmode.ModeMulti)
	body := map[string]any{}
	for _, f := range []string{
		"ai_model", "ai_auto_delegate_enabled", "auto_mode_enabled", "ai_reprioritize_threshold",
		"ai_preference_update_interval", "branch_template", "review_posture",
		"base_branch_push_policy", "permission_absent_autodeny_enabled",
		"permission_absent_grace_seconds",
	} {
		body[f] = nil
	}
	patchTeamSettingsOK(t, s, "default", body)

	got := teamSettingsSnapshot(t, s)
	// ai_model clears to "inherit", which is the empty value — not a default
	// model, because the point of clearing it is to stop having a preference.
	if got.DefaultModel != "" {
		t.Errorf("ai_model after null = %q, want \"\" (inherit)", got.DefaultModel)
	}
	if got.AutoDelegateEnabled != defaults.AutoDelegateEnabled ||
		got.AutoModeEnabled != defaults.AutoModeEnabled ||
		got.AIReprioritizeThreshold != defaults.AIReprioritizeThreshold ||
		got.AIPreferenceUpdateInterval != defaults.AIPreferenceUpdateInterval ||
		got.BranchTemplate != defaults.BranchTemplate ||
		got.ReviewPosture != defaults.ReviewPosture ||
		got.BaseBranchPushPolicy != defaults.BaseBranchPushPolicy ||
		got.PermissionAbsentAutodenyEnabled != defaults.PermissionAbsentAutodenyEnabled ||
		got.PermissionAbsentGraceMS != defaults.PermissionAbsentGraceMS {
		t.Errorf("null did not reset every field to its default:\n got: %+v\nwant: %+v", got, defaults)
	}
}

// TestTeamSettingsPatch_RangeChecks pins the two numerics that were previously
// unvalidated: a threshold or interval at or below zero is refused rather than
// persisted as a value nothing downstream can act on.
func TestTeamSettingsPatch_RangeChecks(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	for _, field := range []string{"ai_reprioritize_threshold", "ai_preference_update_interval"} {
		for _, v := range []int{0, -3} {
			rec := doJSON(t, s, http.MethodPatch, teamSettingsPath("default"), map[string]any{field: v})
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s=%d → %d, want 422; body=%s", field, v, rec.Code, rec.Body.String())
				continue
			}
			assertFirstError(t, rec, httpx.ReasonOutOfRange, field)
		}
	}
	// The grace band is advertised on the GET, so the API caller can discover
	// what the slider already knows.
	if _, minS, maxS := teamGrace(t, s); minS != delegate.AbsentGraceMinSeconds || maxS != delegate.AbsentGraceMaxSeconds {
		t.Errorf("advertised grace band [%d,%d], want [%d,%d]", minS, maxS,
			delegate.AbsentGraceMinSeconds, delegate.AbsentGraceMaxSeconds)
	}
}

// TestTeamSettingsPatch_RejectsJiraProjects: the rules moved to their own
// replace-set resource, and a stale caller that still sends them here is told
// so rather than half-saved.
func TestTeamSettingsPatch_RejectsJiraProjects(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPatch, teamSettingsPath("default"), map[string]any{
		"jira_projects": []map[string]any{{"key": "SKY"}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("jira_projects on the settings PATCH = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonUnknownField, "jira_projects")
}

// TestTeamJiraProjectsPut_ReplaceSetRoundTrip pins the new resource's own
// contract: the body IS the desired set, the GET echoes it, and a key absent
// from a later PUT is untracked.
func TestTeamJiraProjectsPut_ReplaceSetRoundTrip(t *testing.T) {
	// Adding a key is gated against the org's live catalog, so the workspace is
	// connected to one holding both projects.
	s, _ := newServerWithJiraCatalog(t, "SKY", "OPS")

	two := map[string]any{"jira_projects": []map[string]any{
		{"key": "sky", "pickup": validPickup(), "in_progress": validInProgress(), "done": validDone()},
		{"key": "OPS", "pickup": validPickup(), "in_progress": validInProgress(), "done": validDone()},
	}}
	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), two); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	// Keys are canonicalized on the way in, and the response echoes the stored
	// form rather than what the caller happened to send.
	if got := teamJiraProjectKeys(t, s); len(got) != 2 || got[0] != "SKY" || got[1] != "OPS" {
		t.Errorf("keys after the first PUT = %v, want [SKY OPS] in order", got)
	}

	one := map[string]any{"jira_projects": []map[string]any{
		{"key": "OPS", "pickup": validPickup(), "in_progress": validInProgress(), "done": validDone()},
	}}
	if rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), one); rec.Code != http.StatusOK {
		t.Fatalf("replace PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := teamJiraProjectKeys(t, s); len(got) != 1 || got[0] != "OPS" {
		t.Errorf("keys after the replace = %v, want [OPS]", got)
	}
}

// TestSetupWizardSequence_KicksThePoller is the regression the fused setup
// route's removal owes. That route bound the PAT, wrote the base URLs and the
// clone protocol, AND re-dued polling; the wizard now drives three discrete
// writes, and the poller kick has to survive the split or a freshly-configured
// org sits idle until its next scheduled cycle.
func TestSetupWizardSequence_KicksThePoller(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	var mu sync.Mutex
	github, jira := 0, 0
	ghDone := make(chan struct{}, 8)
	jiraDone := make(chan struct{}, 8)
	s.SetOnGitHubChanged(func(string) {
		mu.Lock()
		github++
		mu.Unlock()
		ghDone <- struct{}{}
	})
	s.SetOnJiraChanged(func(string) {
		mu.Lock()
		jira++
		mu.Unlock()
		jiraDone <- struct{}{}
	})

	gh := githubUserStub(t, "acme-bot")
	if rec := bindOrgGitHubPAT(t, s, gh.URL, "ghp_test"); rec.Code != http.StatusOK {
		t.Fatalf("pat bind = %d, body=%s", rec.Code, rec.Body.String())
	}
	<-ghDone

	// The Jira credential bind is the leg that regressed when the fused route
	// went away: it never kicked on its own, it just happened to ride the
	// GitHub restart the fused route fired for the whole call.
	jiraStub := jiraMyselfStub(t, `{"accountId":"org-bot","displayName":"Org Bot"}`, nil)
	if rec := doJSON(t, s, http.MethodPut, jiraCredentialRoute(), map[string]any{
		"deployment": "data_center", "url": jiraStub.URL, "pat": "jira_test",
	}); rec.Code != http.StatusOK {
		t.Fatalf("jira credential bind = %d, body=%s", rec.Code, rec.Body.String())
	}
	<-jiraDone

	// The clone-protocol save is the wizard's last call, and it moves a field
	// the GitHub poller/clone path reads — so it kicks too.
	if rec := patchOrgSettings(t, s, map[string]any{"github_clone_protocol": "https"}); rec.Code != http.StatusOK {
		t.Fatalf("clone protocol save = %d, body=%s", rec.Code, rec.Body.String())
	}
	<-ghDone

	mu.Lock()
	defer mu.Unlock()
	if github < 2 {
		t.Errorf("github poller kicks = %d across the wizard's writes, want at least 2 "+
			"(the PAT bind and the settings save each own their own re-due now)", github)
	}
	if jira < 1 {
		t.Errorf("jira poller kicks = %d, want at least 1 (the credential bind owns its own re-due)", jira)
	}
}

// TestRetiredRoutes_AreGone pins the removals. A route that still answers is a
// second way to do something this ticket gave exactly one way to do, and the
// pair of them drift.
func TestRetiredRoutes_AreGone(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/integrations/setup"},
		{http.MethodDelete, "/api/integrations"},
		{http.MethodPost, "/api/anthropic/connect"},
		{http.MethodPost, "/api/bedrock/connect"},
		{http.MethodGet, "/api/settings/org"},
		{http.MethodPost, "/api/settings/org"},
		{http.MethodGet, "/api/settings/team/default"},
		{http.MethodPost, "/api/settings/team/default"},
	} {
		rec := doJSON(t, s, c.method, c.path, map[string]any{})
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 (route retired)", c.method, c.path, rec.Code)
		}
	}
}

// TestJiraIdentityPAT_RejectsWrongDeploymentFields: which credential shape a
// user may bind is the ORG's deployment, not their choice. The route used to
// dispatch on that server-side state and silently ignore the other shape's
// fields, so a Cloud user pasting a Data Center PAT got "a Jira personal access
// token is required" for a token they had just pasted. The wrong shape's field
// is now rejected by name.
func TestJiraIdentityPAT_RejectsWrongDeploymentFields(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	jiraStub := jiraMyselfStub(t, `{"accountId":"acct","displayName":"Bot"}`, nil)
	// Bind the org as Data Center, so the org's deployment marker is DC.
	if rec := doJSON(t, s, http.MethodPut, jiraCredentialRoute(), map[string]any{
		"deployment": "data_center", "url": jiraStub.URL, "pat": "org_pat",
	}); rec.Code != http.StatusOK {
		t.Fatalf("org jira bind = %d, body=%s", rec.Code, rec.Body.String())
	}

	identityRoute := "/api/orgs/" + runmode.LocalDefaultOrgID + "/jira/identity/pat"
	rec := doJSON(t, s, http.MethodPost, identityRoute, map[string]any{
		"email": "me@acme.com", "token": "cloud_tok",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Cloud fields against a Data Center org = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonUnknownField, "email")

	// The right shape still works, so the rejection is about the field set and
	// not about the route.
	ok := doJSON(t, s, http.MethodPost, identityRoute, map[string]any{"pat": "user_pat"})
	if ok.Code != http.StatusOK {
		t.Fatalf("Data Center PAT against a Data Center org = %d, want 200; body=%s", ok.Code, ok.Body.String())
	}
}

// TestJiraCredentialPut_RejectsCrossFlavorFields: the deployment is an explicit
// discriminator, and each flavor decodes against its own struct — so the other
// flavor's field is a 400 naming it rather than a value silently dropped.
func TestJiraCredentialPut_RejectsCrossFlavorFields(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	for _, c := range []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"pat on cloud", map[string]any{
			"deployment": "cloud", "url": "https://acme.atlassian.net",
			"email": "a@b.c", "token": "t", "pat": "nope",
		}, "pat"},
		{"email on data center", map[string]any{
			"deployment": "data_center", "url": "https://jira.example.com",
			"pat": "p", "email": "a@b.c",
		}, "email"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPut, jiraCredentialRoute(), c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			assertFirstError(t, rec, httpx.ReasonUnknownField, c.field)
		})
	}

	// An absent or unrecognized discriminator is refused rather than guessed —
	// guessing is what the explicit field replaced.
	for _, body := range []map[string]any{
		{"url": "https://jira.example.com", "pat": "p"},
		{"deployment": "server", "url": "https://jira.example.com", "pat": "p"},
	} {
		rec := doJSON(t, s, http.MethodPut, jiraCredentialRoute(), body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("deployment=%v → %d, want 400; body=%s", body["deployment"], rec.Code, rec.Body.String())
		}
	}
}

// --- helpers ---------------------------------------------------------------

// defaultsCloneProtocol is the shipped clone transport, named so the clearing
// test reads as "seed it at the default" rather than as a magic string.
func defaultsCloneProtocol() string { return domain.DefaultOrgSettings().GitHubCloneProtocol }

// orgSettingsSnapshot decodes the org settings GET as a generic map, so a test
// can assert on the wire shape (including keys that omitempty drops) without a
// struct per assertion.
func orgSettingsSnapshot(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, orgSettingsPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", orgSettingsPath(), rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return out
}

// teamSettingsSnapshot reads the team settings row back off its GET.
func teamSettingsSnapshot(t *testing.T, s *Server) domain.TeamSettings {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, teamSettingsPath("default"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", teamSettingsPath("default"), rec.Code, rec.Body.String())
	}
	var out struct {
		TeamSettings domain.TeamSettings `json:"team_settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode team settings: %v", err)
	}
	return out.TeamSettings
}

// teamJiraProjectKeys reads the team's tracked Jira project keys, in order.
func teamJiraProjectKeys(t *testing.T, s *Server) []string {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, teamSettingsPath("default"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", teamSettingsPath("default"), rec.Code, rec.Body.String())
	}
	var out struct {
		JiraProjects []struct {
			Key string `json:"key"`
		} `json:"jira_projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode team settings: %v", err)
	}
	keys := make([]string, 0, len(out.JiraProjects))
	for _, p := range out.JiraProjects {
		keys = append(keys, p.Key)
	}
	return keys
}
