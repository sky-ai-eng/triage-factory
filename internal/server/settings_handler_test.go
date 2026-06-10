package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestDefaultedCloneProtocolView_ModeAware pins the API GET view:
// multi mode always reports "https" regardless of the stored value, while
// local mode reports the literal "ssh" or coerces everything else to "https".
// This is what the Settings page reads to render the (multi-hidden) control.
func TestDefaultedCloneProtocolView_ModeAware(t *testing.T) {
	t.Run("multi coerces ssh to https", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeMulti)
		if got := defaultedCloneProtocolView("ssh"); got != "https" {
			t.Errorf("multi-mode view of stored=ssh = %q, want https", got)
		}
		if got := defaultedCloneProtocolView("https"); got != "https" {
			t.Errorf("multi-mode view of stored=https = %q, want https", got)
		}
	})
	t.Run("local honors ssh", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeLocal)
		if got := defaultedCloneProtocolView("ssh"); got != "ssh" {
			t.Errorf("local-mode view of stored=ssh = %q, want ssh", got)
		}
		if got := defaultedCloneProtocolView(""); got != "https" {
			t.Errorf("local-mode view of stored=\"\" = %q, want https", got)
		}
	})
}

// --- Unit tests: validateProjectRules --------------------------------------
//
// Every persisted project must be fully configured. Partial saves aren't a
// supported state — the FE prevents them, this handler rejects them, and the
// jpsr_*_populated CHECK constraints catch any path that slips past both.
// These unit tests cover the per-project invariant directly:
//   - Pickup: members required, canonical must be empty (TF never writes).
//   - InProgress/Done: members + canonical required, canonical ∈ members.

func validProject(key string) jiraProjectConfig {
	return jiraProjectConfig{
		Key:        key,
		Pickup:     jiraStatusRule{Members: []string{"To Do"}},
		InProgress: jiraStatusRule{Members: []string{"In Progress"}, Canonical: "In Progress"},
		Done:       jiraStatusRule{Members: []string{"Done"}, Canonical: "Done"},
	}
}

// validProjectRule returns the same project as validProject but in
// the domain shape stored by JiraStatusRulesStore. Used by tests that
// seed rules directly through the store (rather than the handler).
func validProjectRule(key string) domain.JiraProjectStatusRules {
	return domain.JiraProjectStatusRules{
		ProjectKey:          key,
		PickupMembers:       []string{"To Do"},
		InProgressMembers:   []string{"In Progress"},
		InProgressCanonical: "In Progress",
		DoneMembers:         []string{"Done"},
		DoneCanonical:       "Done",
	}
}

func TestValidateProjectRules_Valid(t *testing.T) {
	if err := validateProjectRules(validProject("SKY")); err != nil {
		t.Fatalf("fully-configured project should be valid, got: %v", err)
	}
}

func TestValidateProjectRules_PickupEmptyMembers_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.Pickup.Members = nil
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "pickup members are required") {
		t.Errorf("empty pickup members should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_PickupCanonicalSet_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.Pickup.Canonical = "To Do"
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "pickup canonical must be empty") {
		t.Errorf("pickup canonical should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_InProgressEmptyMembers_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.InProgress.Members = nil
	p.InProgress.Canonical = ""
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "in_progress members are required") {
		t.Errorf("empty in_progress members should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_InProgressMissingCanonical_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.InProgress.Canonical = ""
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "in_progress canonical is required") {
		t.Errorf("missing in_progress canonical should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_InProgressCanonicalNotInMembers_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.InProgress.Canonical = "Doing" // not in Members
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "not in members") {
		t.Errorf("canonical-not-in-members should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_DoneEmptyMembers_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.Done.Members = nil
	p.Done.Canonical = ""
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "done members are required") {
		t.Errorf("empty done members should be rejected, got: %v", err)
	}
}

// --- Unit tests: project key normalization + regex -------------------------

func TestNormalizeJiraProjectKey(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"sky", "SKY"},
		{"  SKY  ", "SKY"},
		{"Mixed_Case", "MIXED_CASE"},
		{"", ""},
	} {
		if got := normalizeJiraProjectKey(tc.in); got != tc.want {
			t.Errorf("normalizeJiraProjectKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestJiraProjectKeyRe(t *testing.T) {
	// Matches the canonical Jira project-key shape: a leading uppercase
	// letter followed by uppercase letters or digits. Underscores are
	// not in Jira's allowed set even though some forks of the spec
	// claim otherwise — rejecting them keeps storage aligned with
	// what Jira's own UI would let you create.
	valid := []string{"SKY", "SKY1", "A", "PROJ2024", "XYZ"}
	invalid := []string{"", "1SKY", "_SKY", "sky", "SKY-X", "SKY.X", "SK Y", "PROJ_2024"}
	for _, k := range valid {
		if !jiraProjectKeyRe.MatchString(k) {
			t.Errorf("expected %q to match jiraProjectKeyRe", k)
		}
	}
	for _, k := range invalid {
		if jiraProjectKeyRe.MatchString(k) {
			t.Errorf("expected %q to NOT match jiraProjectKeyRe", k)
		}
	}
}

// --- Handler tests: POST /api/settings/team/default rejects invalid rules ---
//
// These confirm the wire-up — validation errors on any of the three rules
// propagate to a 400 before any persistence fires. Happy-path round-trip
// isn't tested here because it'd write to the real keychain;
// those invariants are covered by the unit tests above.

// teamPostBodyWithProject builds a request that exercises validation
// of a single project's rules via the team settings endpoint.
func teamPostBodyWithProject(key string, pickup, inProgress, done jiraStatusRule) map[string]any {
	return map[string]any{
		"jira_projects": []map[string]any{
			{
				"key":         key,
				"pickup":      pickup,
				"in_progress": inProgress,
				"done":        done,
			},
		},
	}
}

func validInProgress() jiraStatusRule {
	return jiraStatusRule{Members: []string{"In Progress"}, Canonical: "In Progress"}
}

func validDone() jiraStatusRule {
	return jiraStatusRule{Members: []string{"Done"}, Canonical: "Done"}
}

func validPickup() jiraStatusRule {
	return jiraStatusRule{Members: []string{"To Do"}}
}

func TestTeamSettingsPost_PickupCanonical_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := teamPostBodyWithProject("SKY",
		jiraStatusRule{Members: []string{"To Do"}, Canonical: "To Do"},
		validInProgress(),
		validDone(),
	)
	rec := doJSON(t, s, "POST", "/api/settings/team/default", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "canonical must be empty") {
		t.Errorf("error should mention pickup canonical invariant, got: %q", resp["error"])
	}
}

func TestTeamSettingsPost_InProgressCanonicalNotInMembers_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := teamPostBodyWithProject("SKY",
		validPickup(),
		jiraStatusRule{Members: []string{"In Progress"}, Canonical: "Doing"},
		validDone(),
	)
	rec := doJSON(t, s, "POST", "/api/settings/team/default", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "not in members") {
		t.Errorf("error should mention canonical not in members, got: %q", resp["error"])
	}
}

func TestTeamSettingsPost_InProgressMembersWithoutCanonical_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := teamPostBodyWithProject("SKY",
		validPickup(),
		jiraStatusRule{Members: []string{"In Progress"}},
		validDone(),
	)
	rec := doJSON(t, s, "POST", "/api/settings/team/default", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "canonical is required") {
		t.Errorf("error should mention canonical required, got: %q", resp["error"])
	}
}

func TestTeamSettingsPost_DoneCanonicalNotInMembers_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := teamPostBodyWithProject("SKY",
		validPickup(),
		validInProgress(),
		jiraStatusRule{Members: []string{"Resolved", "Verified"}, Canonical: "Done"},
	)
	rec := doJSON(t, s, "POST", "/api/settings/team/default", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "not in members") {
		t.Errorf("error should mention canonical not in members, got: %q", resp["error"])
	}
}

// TestSettingsPost_PerProjectRules_RoundTrip verifies the core SKY-272
// contract: two projects in the same team can carry different rules,
// and saving → loading preserves each project's rules independently.
// Exercises the JiraStatusRulesStore directly (the HTTP handler's
// keychain write isn't available in the test env).
func TestSettingsPost_PerProjectRules_RoundTrip(t *testing.T) {
	s := newTestServer(t)
	ctx := t.Context()
	teamID := runmode.LocalDefaultTeamID

	rules := []domain.JiraProjectStatusRules{
		{
			ProjectKey:          "SKY",
			PickupMembers:       []string{"Backlog", "Selected"},
			InProgressMembers:   []string{"In Progress"},
			InProgressCanonical: "In Progress",
			DoneMembers:         []string{"Done"},
			DoneCanonical:       "Done",
		},
		{
			ProjectKey:          "OPS",
			PickupMembers:       []string{"New", "Triage"},
			InProgressMembers:   []string{"Active"},
			InProgressCanonical: "Active",
			DoneMembers:         []string{"Resolved", "Verified"},
			DoneCanonical:       "Resolved",
		},
	}
	if err := s.jiraRules.ReplaceForTeam(ctx, teamID, rules); err != nil {
		t.Fatalf("ReplaceForTeam: %v", err)
	}
	got, err := s.jiraRules.ListForTeamSystem(ctx, teamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if r := domain.RuleForProject(got, "SKY"); r == nil || r.InProgressCanonical != "In Progress" || !r.PickupContains("Backlog") {
		t.Errorf("SKY rules round-trip: %+v", r)
	}
	if r := domain.RuleForProject(got, "OPS"); r == nil || r.DoneCanonical != "Resolved" || !r.PickupContains("Triage") {
		t.Errorf("OPS rules round-trip: %+v", r)
	}

	// Edit only SKY's rules; OPS must stay untouched.
	for i, p := range rules {
		if p.ProjectKey == "SKY" {
			rules[i].PickupMembers = []string{"Ready"}
		}
	}
	if err := s.jiraRules.ReplaceForTeam(ctx, teamID, rules); err != nil {
		t.Fatalf("ReplaceForTeam (edit SKY): %v", err)
	}
	got, err = s.jiraRules.ListForTeamSystem(ctx, teamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if r := domain.RuleForProject(got, "SKY"); r == nil || !r.PickupContains("Ready") || r.PickupContains("Backlog") {
		t.Errorf("SKY edit didn't apply: %+v", r)
	}
	if r := domain.RuleForProject(got, "OPS"); r == nil || !r.PickupContains("Triage") || r.DoneCanonical != "Resolved" {
		t.Errorf("OPS untouched check failed: %+v", r)
	}

	// Drop SKY — the rules row for SKY should vanish while OPS persists.
	kept := make([]domain.JiraProjectStatusRules, 0, len(rules))
	for _, p := range rules {
		if p.ProjectKey != "SKY" {
			kept = append(kept, p)
		}
	}
	if err := s.jiraRules.ReplaceForTeam(ctx, teamID, kept); err != nil {
		t.Fatalf("ReplaceForTeam (drop SKY): %v", err)
	}
	got, err = s.jiraRules.ListForTeamSystem(ctx, teamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if r := domain.RuleForProject(got, "SKY"); r != nil {
		t.Errorf("SKY rules should be gone after drop, got: %+v", r)
	}
	if r := domain.RuleForProject(got, "OPS"); r == nil || r.DoneCanonical != "Resolved" {
		t.Errorf("OPS rules should persist after dropping SKY: %+v", r)
	}
}

// TestSettingsPost_DuplicateProjectKey_Rejected verifies that the
// handler rejects two entries with the same key — the rules table
// keys on (team_id, project_key) and a duplicate would silently
// last-write-win.
func TestTeamSettingsPost_DuplicateProjectKey_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := map[string]any{
		"jira_projects": []map[string]any{
			{"key": "SKY", "pickup": validPickup(), "in_progress": validInProgress(), "done": validDone()},
			{"key": "SKY", "pickup": validPickup(), "in_progress": validInProgress(), "done": validDone()},
		},
	}
	rec := doJSON(t, s, "POST", "/api/settings/team/default", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on duplicate project key, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "duplicate project key") {
		t.Errorf("error should mention duplicate, got: %q", resp["error"])
	}
}

// TestSettingsGet_MemberCountAndRole verifies the scope GET responses carry
// the membership signals the frontend's N=1 collapse + role gating read
// (SKY-358). Local mode is the degenerate single-member world, so the org
// and team both report one member and the caller is the team admin.
func TestSettingsGet_MemberCountAndRole(t *testing.T) {
	s := newTestServer(t)

	orgRec := doJSON(t, s, "GET", "/api/settings/org", nil)
	if orgRec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings/org: %d: %s", orgRec.Code, orgRec.Body.String())
	}
	var org struct {
		MemberCount int `json:"member_count"`
	}
	if err := json.Unmarshal(orgRec.Body.Bytes(), &org); err != nil {
		t.Fatalf("decode org: %v", err)
	}
	if org.MemberCount != 1 {
		t.Errorf("org member_count = %d, want 1 (local mode is single-member)", org.MemberCount)
	}

	teamRec := doJSON(t, s, "GET", "/api/settings/team/default", nil)
	if teamRec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings/team/default: %d: %s", teamRec.Code, teamRec.Body.String())
	}
	var team struct {
		MemberCount int    `json:"member_count"`
		Role        string `json:"role"`
	}
	if err := json.Unmarshal(teamRec.Body.Bytes(), &team); err != nil {
		t.Fatalf("decode team: %v", err)
	}
	if team.MemberCount != 1 {
		t.Errorf("team member_count = %d, want 1", team.MemberCount)
	}
	if team.Role != "admin" {
		t.Errorf("team role = %q, want admin (local sole member)", team.Role)
	}
}

// --- SKY-359 model cap warnings --------------------------------------------
//
// EffectiveModel's unit behavior is covered in internal/domain. These cover
// the handler wiring: the cap never blocks a save, it just surfaces a
// warning so the admin/team know the effective model differs from the input.

func postJSONResp(t *testing.T, s *Server, path string, body any) map[string]string {
	t.Helper()
	rec := doJSON(t, s, "POST", path, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s: %d: %s", path, rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s response: %v", path, err)
	}
	return resp
}

// TestOrgSettingsPost_GitHubURLClear_PreservesUserIdentity asserts the
// access/identity decoupling (SKY-458): clearing the org's GitHub URL is an
// access change (PAT_1), and it must NOT touch the user's per-user GitHub
// identity (PAT_2). Identity is owned solely by its own capture surface (the
// setup wizard's User step / the Connect gate page), never mutated as a side
// effect of an org-credential save. A leftover row is durable + harmless (the
// runtime tolerates absent/stale identity) and is still valid if GitHub is
// reconnected to the same host.
func TestOrgSettingsPost_GitHubURLClear_PreservesUserIdentity(t *testing.T) {
	keyring.MockInit() // in-memory keychain — the sandbox has no dbus backend
	s := newTestServer(t)
	ctx := t.Context()
	const host = "https://github.example.com"

	if err := integrations.Save(ctx, s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{GitHubURL: host}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	if err := s.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "pat"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	if login, _ := s.users.GetGitHubLogin(ctx, runmode.LocalDefaultUserID, host); login != "octocat" {
		t.Fatalf("precondition: GetGitHubLogin = %q, want octocat", login)
	}

	// Disconnect the org's GitHub access: clear the GitHub URL.
	postJSONResp(t, s, "/api/settings/org", map[string]any{"github_base_url": ""})

	login, err := s.users.GetGitHubLogin(ctx, runmode.LocalDefaultUserID, host)
	if err != nil {
		t.Fatalf("GetGitHubLogin after clear: %v", err)
	}
	if login != "octocat" {
		t.Errorf("identity for %s should survive an org-access disconnect, got login=%q", host, login)
	}
}

// --- Jira access/identity decoupling ---------------------------------------
//
// These mirror the GitHub untangle above for Jira: org-level Jira ACCESS
// (PAT_1, the bot connection) must never write or clear the *caller's* per-user
// Jira identity or credential. The only writer of users.jira_account_id is the
// dedicated bind surface (POST .../identity/jira/pat); the org connect /
// settings-save / disconnect paths leave both the identity and the per-user
// credential untouched. We assert that by seeding a distinct user identity /
// credential and proving the org operation didn't disturb it.

// TestJiraConnect_DoesNotWriteUserIdentity covers the two-stage Settings connect
// (POST /api/jira/connect → handleJiraConnect): validating + storing the org's
// Jira credential must NOT bind the caller's identity, even though the same
// /myself round-trip yields an account. The credential here belongs to the org
// bot, not the user.
func TestJiraConnect_DoesNotWriteUserIdentity(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit() // in-memory keychain — the sandbox has no dbus backend
	s := newTestServer(t)
	ctx := t.Context()

	// The org connects Jira with a credential whose /myself maps to a DIFFERENT
	// account (the org bot). The connect must not overwrite the user's identity.
	jiraStub := jiraMyselfStub(t, `{"accountId":"org-bot","displayName":"Org Bot"}`, nil)

	// Seed a pre-existing per-user Jira identity, as the bind flow would have,
	// keyed on the org's Jira host (SKY-397).
	if err := s.users.UpsertJiraIdentity(ctx, runmode.LocalDefaultUserID, jiraStub.URL, "user-acct", "User Name", "pat"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	rec := doJSON(t, s, "POST", "/api/jira/connect",
		map[string]any{"url": jiraStub.URL, "pat": "org_pat"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	accountID, displayName, err := s.users.GetJiraIdentity(ctx, runmode.LocalDefaultUserID, jiraStub.URL)
	if err != nil {
		t.Fatalf("GetJiraIdentity: %v", err)
	}
	if accountID != "user-acct" || displayName != "User Name" {
		t.Errorf("org Jira connect overwrote the caller's identity: got (%q, %q), want (user-acct, User Name)", accountID, displayName)
	}
}

// TestOrgSettingsPost_JiraPAT_DoesNotWriteUserIdentity covers the org settings
// save path (POST /api/settings/org with jira_pat → handleOrgSettingsPost):
// saving the org Jira PAT (PAT_1) validates the token but must not bind the
// caller's identity.
func TestOrgSettingsPost_JiraPAT_DoesNotWriteUserIdentity(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()

	// The PAT branch reads creds.JiraURL, so the org must already have a Jira
	// host; the /myself stub maps the token to the org's bot account.
	jiraStub := jiraMyselfStub(t, `{"accountId":"org-bot","displayName":"Org Bot"}`, nil)
	if err := integrations.Save(ctx, s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{JiraURL: jiraStub.URL}); err != nil {
		t.Fatalf("seed creds url: %v", err)
	}
	// A pre-existing user identity (keyed on the org's Jira host) the org PAT
	// save must leave alone.
	if err := s.users.UpsertJiraIdentity(ctx, runmode.LocalDefaultUserID, jiraStub.URL, "user-acct", "User Name", "pat"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	postJSONResp(t, s, "/api/settings/org", map[string]any{"jira_pat": "org_pat"})

	accountID, displayName, err := s.users.GetJiraIdentity(ctx, runmode.LocalDefaultUserID, jiraStub.URL)
	if err != nil {
		t.Fatalf("GetJiraIdentity: %v", err)
	}
	if accountID != "user-acct" || displayName != "User Name" {
		t.Errorf("org Jira PAT save overwrote the caller's identity: got (%q, %q), want (user-acct, User Name)", accountID, displayName)
	}
}

// TestOrgSettingsPost_JiraURLClear_PreservesUserIdentity is the disconnect
// mirror of the GitHub URL-clear test: clearing the org's Jira URL is an
// access change (PAT_1) and must NOT wipe the user's now-independent Jira
// identity. Per-user Jira access is cleared only by its own surface.
func TestOrgSettingsPost_JiraURLClear_PreservesUserIdentity(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()
	const host = "https://jira.example.com"

	if err := integrations.Save(ctx, s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{JiraURL: host, JiraPAT: "org-pat"}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	if err := s.users.UpsertJiraIdentity(ctx, runmode.LocalDefaultUserID, host, "user-acct", "User Name", "pat"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	// Disconnect the org's Jira access: clear the Jira URL.
	postJSONResp(t, s, "/api/settings/org", map[string]any{"jira_base_url": ""})

	accountID, displayName, err := s.users.GetJiraIdentity(ctx, runmode.LocalDefaultUserID, host)
	if err != nil {
		t.Fatalf("GetJiraIdentity after clear: %v", err)
	}
	if accountID != "user-acct" || displayName != "User Name" {
		t.Errorf("org Jira disconnect wiped the caller's identity: got (%q, %q), want it preserved", accountID, displayName)
	}
}

// TestOrgSettingsPost_JiraPatClear_PreservesUserCredential is the credential
// half of the untangle (the URL-clear test above covers the identity half):
// clearing the org's Jira PAT (PAT_1) deletes only the org credential key —
// the user's own stored credential, custodied under the separate per-user
// "jira_token/<host>" namespace, is untouched. Org access and per-user access
// are independent.
func TestOrgSettingsPost_JiraPatClear_PreservesUserCredential(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()
	const host = "https://jira.example.com"

	// The org has a Jira PAT (the thing we'll clear)...
	if err := integrations.Save(ctx, s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{JiraURL: host, JiraPAT: "org-pat"}); err != nil {
		t.Fatalf("seed org creds: %v", err)
	}
	// ...and the caller has bound their own per-user credential, as the bind
	// flow stores it (under the host-scoped per-user key).
	if err := s.secrets.PutUser(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "jira_token/"+host, "user-token", "Jira user access token"); err != nil {
		t.Fatalf("seed user credential: %v", err)
	}

	// Clear the org Jira PAT.
	postJSONResp(t, s, "/api/settings/org", map[string]any{"jira_pat": ""})

	// The user's per-user credential survives — it lives under a different key.
	stored, err := s.secrets.GetUserSystem(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "jira_token/"+host)
	if err != nil {
		t.Fatalf("GetUserSystem after org PAT clear: %v", err)
	}
	if stored != "user-token" {
		t.Errorf("org Jira PAT clear wiped the user's per-user credential: got %q, want user-token", stored)
	}
}

func TestTeamSettingsPost_ModelExceedsOrgCap_Warns(t *testing.T) {
	s := newTestServer(t)
	postJSONResp(t, s, "/api/settings/org", map[string]any{"max_llm_model_tier": "sonnet"})

	resp := postJSONResp(t, s, "/api/settings/team/default", map[string]any{"ai_model": "opus"})
	if !strings.Contains(resp["warning"], "exceeds the org cap") {
		t.Errorf("expected org-cap warning, got warning=%q", resp["warning"])
	}
}

func TestTeamSettingsPost_ModelWithinOrgCap_NoWarning(t *testing.T) {
	s := newTestServer(t)
	postJSONResp(t, s, "/api/settings/org", map[string]any{"max_llm_model_tier": "opus"})

	resp := postJSONResp(t, s, "/api/settings/team/default", map[string]any{"ai_model": "sonnet"})
	if resp["warning"] != "" {
		t.Errorf("expected no warning when team default is within cap, got %q", resp["warning"])
	}
}

func TestOrgSettingsPost_CapBelowTeamDefault_Warns(t *testing.T) {
	s := newTestServer(t)
	postJSONResp(t, s, "/api/settings/team/default", map[string]any{"ai_model": "opus"})

	resp := postJSONResp(t, s, "/api/settings/org", map[string]any{"max_llm_model_tier": "sonnet"})
	if !strings.Contains(resp["warning"], "exceeds the new cap") {
		t.Errorf("expected cap-downgrade warning, got warning=%q", resp["warning"])
	}
}
