package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
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
	if msg := firstErrorMessage(t, rec); !strings.Contains(msg, "canonical must be empty") {
		t.Errorf("error should mention pickup canonical invariant, got: %q", msg)
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
	resp := firstErrorMessage(t, rec)
	if !strings.Contains(resp, "not in members") {
		t.Errorf("error should mention canonical not in members, got: %q", resp)
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
	resp := firstErrorMessage(t, rec)
	if !strings.Contains(resp, "canonical is required") {
		t.Errorf("error should mention canonical required, got: %q", resp)
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
	resp := firstErrorMessage(t, rec)
	if !strings.Contains(resp, "not in members") {
		t.Errorf("error should mention canonical not in members, got: %q", resp)
	}
}

// TestSettingsPost_PerProjectRules_RoundTrip verifies the core
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

// TestTeamSettingsPost_DuplicateProjectKey_Rejected verifies that the
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
	resp := firstErrorMessage(t, rec)
	if !strings.Contains(resp, "duplicate project key") {
		t.Errorf("error should mention duplicate, got: %q", resp)
	}
}

// TestSettingsGet_MemberCountAndRole verifies the scope GET responses carry
// the membership signals the frontend's N=1 collapse + role gating read.
// Local mode is the degenerate single-member world, so the org
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

// --- model cap warnings -----------------------------------------------------
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

// TestOrgSettingsPost_BlankGitHubURLWithApp_409 closes the settings-route half
// of the hazard TestGitHubPATDelete_StagedApp_KeepsHost covers for the PAT
// unbind: githubBaseFor falls org_settings → github_url secret → github.com, so
// blanking the column while an App is registered silently sends a GHES org's
// App to github.com. The config route refuses instead of skipping, because here
// the clear is the request itself, not a side effect of one.
func TestOrgSettingsPost_BlankGitHubURLWithApp_409(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	const host = "https://github.example.com"

	postJSONResp(t, s, "/api/settings/org", map[string]any{"github_base_url": host})
	seedLocalApp(t, s, false) // staged is enough — the App resolves against this host either way

	rec := doJSON(t, s, "POST", "/api/settings/org", map[string]any{"github_base_url": ""})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, "github_base_url") {
		t.Errorf("body %s should name the offending field", got)
	}

	var stored string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COALESCE(github_base_url, '') FROM org_settings WHERE org_id = ?`,
		runmode.LocalDefaultOrgID).Scan(&stored); err != nil {
		t.Fatalf("read github_base_url: %v", err)
	}
	if stored != host {
		t.Errorf("github_base_url = %q, want %q — the refused save must not have partially applied", stored, host)
	}
}

// TestOrgSettingsPost_BlankGitHubURLWithoutApp_OK is the other half: with no App
// to strand, clearing the host is an ordinary config edit and stays allowed. The
// guard above must not turn into a blanket ban on emptying the field.
func TestOrgSettingsPost_BlankGitHubURLWithoutApp_OK(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	postJSONResp(t, s, "/api/settings/org", map[string]any{"github_base_url": "https://github.example.com"})
	postJSONResp(t, s, "/api/settings/org", map[string]any{"github_base_url": ""})

	var stored string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COALESCE(github_base_url, '') FROM org_settings WHERE org_id = ?`,
		runmode.LocalDefaultOrgID).Scan(&stored); err != nil {
		t.Fatalf("read github_base_url: %v", err)
	}
	if stored != "" {
		t.Errorf("github_base_url = %q, want cleared", stored)
	}
}

// TestOrgSettingsPost_RetargetGitHubURLWithApp_OK: moving an App org to a
// different non-empty host is a legitimate GHES domain change and stays
// allowed. The guard is aimed at the silent-default case, not at host edits.
func TestOrgSettingsPost_RetargetGitHubURLWithApp_OK(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	postJSONResp(t, s, "/api/settings/org", map[string]any{"github_base_url": "https://old.example.com"})
	seedLocalApp(t, s, true)
	postJSONResp(t, s, "/api/settings/org", map[string]any{"github_base_url": "https://new.example.com"})

	var stored string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COALESCE(github_base_url, '') FROM org_settings WHERE org_id = ?`,
		runmode.LocalDefaultOrgID).Scan(&stored); err != nil {
		t.Fatalf("read github_base_url: %v", err)
	}
	if stored != "https://new.example.com" {
		t.Errorf("github_base_url = %q, want the retargeted host", stored)
	}
}

// TestOrgSettingsPost_GitHubURLClear_PreservesUserIdentity asserts the
// access/identity decoupling: clearing the org's GitHub URL is an
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
	if err := s.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "", "pat"); err != nil {
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
// Jira identity or credential. The only writer of user_jira_identities is the
// dedicated bind surface (POST .../jira/identity/pat); the org connect /
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
	// keyed on the org's Jira host.
	if err := s.users.UpsertJiraIdentity(ctx, runmode.LocalDefaultUserID, jiraStub.URL, "user-acct", "User Name", "pat"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	rec := doJSON(t, s, "PUT", jiraCredentialRoute(),
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

// TestJiraConnect_Cloud_StoresAPITokenCredential covers the Cloud org-access
// variant: posting an email + API token validates against the Cloud config
// (Basic auth, REST v3 /myself) and stores the Cloud credential pair plus the
// auth-method marker, so the system resolver later rebuilds a Cloud client. No
// DC PAT is written.
func TestJiraConnect_Cloud_StoresAPITokenCredential(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit() // in-memory keychain — the sandbox has no dbus backend
	s := newTestServer(t)
	ctx := t.Context()

	var gotAuth, gotPath string
	jiraStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"accountId":"cloud-bot","displayName":"Cloud Bot"}`)
	}))
	t.Cleanup(jiraStub.Close)

	rec := doJSON(t, s, "PUT", jiraCredentialRoute(),
		map[string]any{"url": jiraStub.URL, "email": "bot@acme.com", "token": "cloud_tok"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	// Validated through the Cloud config: Basic email:token against REST v3.
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@acme.com:cloud_tok"))
	if gotAuth != wantAuth {
		t.Errorf("validate Authorization = %q, want %q (Cloud uses Basic email:token)", gotAuth, wantAuth)
	}
	if gotPath != "/rest/api/3/myself" {
		t.Errorf("validate path = %q, want /rest/api/3/myself (Cloud uses REST v3)", gotPath)
	}

	// Stored under the Cloud keys + marker; no DC PAT written.
	creds, err := integrations.Load(ctx, s.secrets, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if creds.JiraEmail != "bot@acme.com" || creds.JiraAPIToken != "cloud_tok" {
		t.Errorf("stored cloud creds = (%q, %q), want (bot@acme.com, cloud_tok)", creds.JiraEmail, creds.JiraAPIToken)
	}
	if creds.JiraAuthMethod != string(jira.AuthMethodCloudAPIToken) {
		t.Errorf("auth-method marker = %q, want %q", creds.JiraAuthMethod, jira.AuthMethodCloudAPIToken)
	}
	if creds.JiraPAT != "" {
		t.Errorf("DC PAT unexpectedly stored on a Cloud connect: %q", creds.JiraPAT)
	}
}

// TestJiraConnect_Cloud_RequiresBothHalves pins that a half-filled Cloud form
// (email without token) is a 400 and stores nothing, rather than silently
// falling through to the Data Center path.
func TestJiraConnect_Cloud_RequiresBothHalves(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()

	rec := doJSON(t, s, "PUT", jiraCredentialRoute(),
		map[string]any{"url": "https://acme.atlassian.net", "email": "bot@acme.com"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	creds, err := integrations.Load(ctx, s.secrets, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if creds.JiraEmail != "" || creds.JiraAPIToken != "" || creds.JiraAuthMethod != "" {
		t.Errorf("partial Cloud form persisted credentials: %+v", creds)
	}
}

// TestJiraConnect_Cloud_MismatchHint pins the deployment-mismatch hint: when the
// user picks Cloud (sends email + token) but the host rejects the credential AND
// the URL isn't a Cloud host, the 422 message nudges them toward the Data Center
// scheme rather than only saying "bad token". The hint is advisory — validation,
// not the hostname, gates storage, so a working combo is never blocked.
func TestJiraConnect_Cloud_MismatchHint(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	// A non-atlassian.net host (DeploymentForHost → Data Center) that rejects
	// the Cloud credential.
	jiraStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(jiraStub.Close)

	rec := doJSON(t, s, "PUT", jiraCredentialRoute(),
		map[string]any{"url": jiraStub.URL, "email": "bot@acme.com", "token": "tok"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Data Center URL") {
		t.Errorf("expected a Data Center deployment hint, got: %s", rec.Body.String())
	}
}

// TestJiraConnect_SchemeSwitch_ClearsStaleCredential pins the scheme-switch
// cleanup: reconnecting an org under a different Jira auth scheme drops the
// previous scheme's stored secret, so no stale credential lingers and no later
// read can mistake the org for the old scheme. DC → Cloud here; the DC PAT must
// be gone afterward.
func TestJiraConnect_SchemeSwitch_ClearsStaleCredential(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()

	// One stub answering both REST v2 (DC validate) and v3 (Cloud validate).
	jiraStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"accountId":"acct","displayName":"Bot"}`)
	}))
	t.Cleanup(jiraStub.Close)

	// 1) Connect as Data Center (PAT).
	rec := doJSON(t, s, "PUT", jiraCredentialRoute(),
		map[string]any{"url": jiraStub.URL, "pat": "dc_pat"})
	if rec.Code != http.StatusOK {
		t.Fatalf("DC connect status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	creds, _ := integrations.Load(ctx, s.secrets, runmode.LocalDefaultOrgID)
	if creds.JiraPAT != "dc_pat" || creds.JiraAuthMethod != string(jira.AuthMethodDCPAT) {
		t.Fatalf("after DC connect, unexpected creds: %+v", creds)
	}

	// 2) Reconnect the same org as Cloud (email + API token).
	rec = doJSON(t, s, "PUT", jiraCredentialRoute(),
		map[string]any{"url": jiraStub.URL, "email": "bot@acme.com", "token": "cloud_tok"})
	if rec.Code != http.StatusOK {
		t.Fatalf("Cloud connect status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	creds, _ = integrations.Load(ctx, s.secrets, runmode.LocalDefaultOrgID)
	if creds.JiraAuthMethod != string(jira.AuthMethodCloudAPIToken) {
		t.Errorf("auth-method marker = %q, want cloud_api_token", creds.JiraAuthMethod)
	}
	if creds.JiraEmail != "bot@acme.com" || creds.JiraAPIToken != "cloud_tok" {
		t.Errorf("cloud creds not stored: %+v", creds)
	}
	if creds.JiraPAT != "" {
		t.Errorf("stale DC PAT survived the switch to Cloud: %q", creds.JiraPAT)
	}

	// 3) Switch back to Data Center — the Cloud pair must now be gone, leaving
	//    exactly the DC scheme.
	rec = doJSON(t, s, "PUT", jiraCredentialRoute(),
		map[string]any{"url": jiraStub.URL, "pat": "dc_pat_2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("DC reconnect status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	creds, _ = integrations.Load(ctx, s.secrets, runmode.LocalDefaultOrgID)
	if creds.JiraAuthMethod != string(jira.AuthMethodDCPAT) || creds.JiraPAT != "dc_pat_2" {
		t.Errorf("after switch back to DC, unexpected creds: %+v", creds)
	}
	if creds.JiraEmail != "" || creds.JiraAPIToken != "" {
		t.Errorf("stale Cloud pair survived the switch to DC: email=%q token=%q", creds.JiraEmail, creds.JiraAPIToken)
	}
}

// TestJiraCredentialPut_DoesNotWriteUserIdentity covers the org Jira credential
// resource: binding the org's service credential (PAT_1) validates the token
// but must not bind the caller's identity, even though the same /myself
// round-trip yields an account. (This used to ride the bulk settings save; the
// property is the same, the route isn't.)
func TestJiraCredentialPut_DoesNotWriteUserIdentity(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()

	// The /myself stub maps the token to the org's bot account.
	jiraStub := jiraMyselfStub(t, `{"accountId":"org-bot","displayName":"Org Bot"}`, nil)
	// A pre-existing user identity (keyed on the org's Jira host) the org
	// credential bind must leave alone.
	if err := s.users.UpsertJiraIdentity(ctx, runmode.LocalDefaultUserID, jiraStub.URL, "user-acct", "User Name", "pat"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	rec := doJSON(t, s, "PUT", jiraCredentialRoute(), map[string]any{
		"url": jiraStub.URL, "pat": "org_pat",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("jira bind = %d, body=%s", rec.Code, rec.Body.String())
	}

	accountID, displayName, err := s.users.GetJiraIdentity(ctx, runmode.LocalDefaultUserID, jiraStub.URL)
	if err != nil {
		t.Fatalf("GetJiraIdentity: %v", err)
	}
	if accountID != "user-acct" || displayName != "User Name" {
		t.Errorf("org Jira credential bind overwrote the caller's identity: got (%q, %q), want (user-acct, User Name)", accountID, displayName)
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

// TestJiraCredentialDelete_PreservesUserCredential is the credential half of
// the untangle (the URL-clear test above covers the identity half): unbinding
// the org's Jira credential (PAT_1) deletes only the org key — the user's own
// stored credential, custodied under the separate per-user "jira_token/<host>"
// namespace, is untouched. Org access and per-user access are independent.
func TestJiraCredentialDelete_PreservesUserCredential(t *testing.T) {
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

	// Unbind the org Jira credential.
	rec := doJSON(t, s, "DELETE", jiraCredentialRoute(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("jira unbind = %d, body=%s", rec.Code, rec.Body.String())
	}

	// The user's per-user credential survives — it lives under a different key.
	stored, err := s.secrets.GetUserSystem(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "jira_token/"+host)
	if err != nil {
		t.Fatalf("GetUserSystem after org unbind: %v", err)
	}
	if stored != "user-token" {
		t.Errorf("org Jira unbind wiped the user's per-user credential: got %q, want user-token", stored)
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

// --- TFAC-477 daily spend cap ----------------------------------------------

// orgDailyCap reads the org settings GET and returns max_daily_cost_usd.
func orgDailyCap(t *testing.T, s *Server) float64 {
	t.Helper()
	rec := doJSON(t, s, "GET", "/api/settings/org", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings/org: %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		MaxDailyCostUSD float64 `json:"max_daily_cost_usd"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return resp.MaxDailyCostUSD
}

// TestOrgSettingsPost_DailyCostCap_RoundTrip pins the GET/POST wire round-trip:
// POSTing a positive cap reflects it on the next GET, and POSTing 0 clears it
// back to "no cap".
func TestOrgSettingsPost_DailyCostCap_RoundTrip(t *testing.T) {
	s := newTestServer(t)

	if got := orgDailyCap(t, s); got != 0 {
		t.Fatalf("fresh org daily cap = %v, want 0 (no cap by default)", got)
	}

	postJSONResp(t, s, "/api/settings/org", map[string]any{"max_daily_cost_usd": 25.5})
	if got := orgDailyCap(t, s); got != 25.5 {
		t.Errorf("after POST 25.5, GET max_daily_cost_usd = %v, want 25.5", got)
	}

	// 0 clears the cap.
	postJSONResp(t, s, "/api/settings/org", map[string]any{"max_daily_cost_usd": 0})
	if got := orgDailyCap(t, s); got != 0 {
		t.Errorf("after POST 0, GET max_daily_cost_usd = %v, want 0 (cleared)", got)
	}
}

// TestOrgSettingsPost_DailyCostCap_OmittedFieldUntouched pins the pointer-nil
// semantics: a save that omits max_daily_cost_usd must leave a previously-set
// cap intact (an unrelated org save can't stomp the cap).
func TestOrgSettingsPost_DailyCostCap_OmittedFieldUntouched(t *testing.T) {
	s := newTestServer(t)
	postJSONResp(t, s, "/api/settings/org", map[string]any{"max_daily_cost_usd": 40})

	// A save touching only the model tier omits the cap → it must survive.
	postJSONResp(t, s, "/api/settings/org", map[string]any{"max_llm_model_tier": "sonnet"})
	if got := orgDailyCap(t, s); got != 40 {
		t.Errorf("omitting max_daily_cost_usd cleared the cap: got %v, want 40 preserved", got)
	}
}

// TestOrgSettingsPost_DailyCostCap_NegativeRejected pins the >= 0 validation.
func TestOrgSettingsPost_DailyCostCap_NegativeRejected(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, "POST", "/api/settings/org", map[string]any{"max_daily_cost_usd": -1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative cap should 400, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := firstErrorMessage(t, rec)
	if !strings.Contains(resp, "max_daily_cost_usd must be >= 0") {
		t.Errorf("expected >= 0 validation message, got: %q", resp)
	}
}

// --- TFAC-638 concurrent-run limit -----------------------------------------

// orgConcurrentRuns reads the org settings GET and returns max_concurrent_runs.
func orgConcurrentRuns(t *testing.T, s *Server) int {
	t.Helper()
	rec := doJSON(t, s, "GET", "/api/settings/org", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings/org: %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		MaxConcurrentRuns int `json:"max_concurrent_runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return resp.MaxConcurrentRuns
}

// TestOrgSettingsPost_ConcurrentRuns_RoundTrip pins the GET/POST wire round-trip:
// POSTing a positive limit reflects it on the next GET, and POSTing 0 clears it
// back to "unlimited".
func TestOrgSettingsPost_ConcurrentRuns_RoundTrip(t *testing.T) {
	s := newTestServer(t)

	if got := orgConcurrentRuns(t, s); got != 0 {
		t.Fatalf("fresh org concurrent-run limit = %v, want 0 (unlimited by default)", got)
	}

	postJSONResp(t, s, "/api/settings/org", map[string]any{"max_concurrent_runs": 8})
	if got := orgConcurrentRuns(t, s); got != 8 {
		t.Errorf("after POST 8, GET max_concurrent_runs = %v, want 8", got)
	}

	// 0 clears the limit.
	postJSONResp(t, s, "/api/settings/org", map[string]any{"max_concurrent_runs": 0})
	if got := orgConcurrentRuns(t, s); got != 0 {
		t.Errorf("after POST 0, GET max_concurrent_runs = %v, want 0 (cleared)", got)
	}
}

// TestOrgSettingsPost_ConcurrentRuns_OmittedFieldUntouched pins the pointer-nil
// semantics: a save that omits max_concurrent_runs must leave a previously-set
// limit intact (an unrelated org save can't stomp it).
func TestOrgSettingsPost_ConcurrentRuns_OmittedFieldUntouched(t *testing.T) {
	s := newTestServer(t)
	postJSONResp(t, s, "/api/settings/org", map[string]any{"max_concurrent_runs": 5})

	// A save touching only the model tier omits the limit → it must survive.
	postJSONResp(t, s, "/api/settings/org", map[string]any{"max_llm_model_tier": "sonnet"})
	if got := orgConcurrentRuns(t, s); got != 5 {
		t.Errorf("omitting max_concurrent_runs cleared the limit: got %v, want 5 preserved", got)
	}
}

// TestOrgSettingsPost_ConcurrentRuns_NegativeRejected pins the >= 0 validation.
func TestOrgSettingsPost_ConcurrentRuns_NegativeRejected(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, "POST", "/api/settings/org", map[string]any{"max_concurrent_runs": -1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative limit should 400, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := firstErrorMessage(t, rec)
	if !strings.Contains(resp, "max_concurrent_runs must be >= 0") {
		t.Errorf("expected >= 0 validation message, got: %q", resp)
	}
}

// TestOrgSettingsPost_ConcurrentRuns_OversizedRejected pins the upper bound: a
// value beyond the ceiling 400s at the handler rather than overflowing the
// Postgres int4 column into a 500.
func TestOrgSettingsPost_ConcurrentRuns_OversizedRejected(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, "POST", "/api/settings/org",
		map[string]any{"max_concurrent_runs": domain.MaxConcurrentRunsCeiling + 1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized limit should 400, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := firstErrorMessage(t, rec)
	if !strings.Contains(resp, "max_concurrent_runs must be at most") {
		t.Errorf("expected upper-bound validation message, got: %q", resp)
	}
	// The ceiling itself is accepted.
	postJSONResp(t, s, "/api/settings/org",
		map[string]any{"max_concurrent_runs": domain.MaxConcurrentRunsCeiling})
	if got := orgConcurrentRuns(t, s); got != domain.MaxConcurrentRunsCeiling {
		t.Errorf("ceiling value should round-trip, got %v want %v", got, domain.MaxConcurrentRunsCeiling)
	}
}

// --- TFAC-392 unattended-prompt grace window bounds ------------------------

// teamGrace reads the team settings GET and returns the stored grace (ms) plus
// the advertised min/max second bounds that drive the UI slider's range.
func teamGrace(t *testing.T, s *Server) (graceMS, minS, maxS int) {
	t.Helper()
	rec := doJSON(t, s, "GET", "/api/settings/team/default", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings/team/default: %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		TeamSettings struct {
			PermissionAbsentGraceMS int `json:"PermissionAbsentGraceMS"`
		} `json:"team_settings"`
		Min int `json:"permission_absent_grace_min_seconds"`
		Max int `json:"permission_absent_grace_max_seconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode team settings: %v", err)
	}
	return resp.TeamSettings.PermissionAbsentGraceMS, resp.Min, resp.Max
}

// TestTeamSettingsGet_GraceBoundsAdvertised pins that the GET surfaces the
// backend-honored grace band so the slider can track it instead of hardcoding.
func TestTeamSettingsGet_GraceBoundsAdvertised(t *testing.T) {
	s := newTestServer(t)
	_, minS, maxS := teamGrace(t, s)
	if minS != delegate.AbsentGraceMinSeconds {
		t.Errorf("grace min = %d, want %d", minS, delegate.AbsentGraceMinSeconds)
	}
	if maxS != delegate.AbsentGraceMaxSeconds {
		t.Errorf("grace max = %d, want %d", maxS, delegate.AbsentGraceMaxSeconds)
	}
	if minS < 1 || maxS <= minS {
		t.Errorf("nonsensical grace bounds [%d, %d]", minS, maxS)
	}
}

// TestTeamSettingsPost_GraceOutOfBandRejected pins the POST-side band check: a
// value outside [min, max] is REJECTED, and an in-band one round-trips
// verbatim. It used to clamp, which answered "saved" for a grace the caller
// never asked for — invisible to an API client, since the UI slider can only
// express in-band values in the first place.
func TestTeamSettingsPost_GraceOutOfBandRejected(t *testing.T) {
	s := newTestServer(t)

	before, _, _ := teamGrace(t, s)
	for _, secs := range []int{99999, 0, -1} {
		rec := doJSON(t, s, "POST", "/api/settings/team/default", map[string]any{
			"permission_absent_grace_seconds": secs,
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST grace %ds = %d, want 400; body=%s", secs, rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonOutOfRange, "permission_absent_grace_seconds")
		if ms, _, _ := teamGrace(t, s); ms != before {
			t.Errorf("rejected grace %ds still moved the stored value to %dms", secs, ms)
		}
	}

	postJSONResp(t, s, "/api/settings/team/default", map[string]any{
		"permission_absent_grace_seconds": 20,
	})
	if ms, _, _ := teamGrace(t, s); ms != 20000 {
		t.Errorf("after POST 20s, stored grace = %dms, want 20000ms (in-band, verbatim)", ms)
	}
}

// jiraCredentialRoute is the org's Jira service-credential resource in local
// mode — the PUT that replaced POST /api/jira/connect when credentials became
// addressable resources rather than fields on the bulk settings save.
func jiraCredentialRoute() string {
	return "/api/orgs/" + runmode.LocalDefaultOrgID + "/jira/access/credential"
}

// --- review posting posture ------------------------------------------------

// teamReviewPosture reads the stored posture back off the team settings GET.
// The GET serializes domain.TeamSettings wholesale (no JSON tags), so the wire
// key is the Go field name.
func teamReviewPosture(t *testing.T, s *Server) string {
	t.Helper()
	rec := doJSON(t, s, "GET", "/api/settings/team/default", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings/team/default: %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		TeamSettings struct {
			ReviewPosture string `json:"ReviewPosture"`
		} `json:"team_settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode team settings: %v", err)
	}
	return resp.TeamSettings.ReviewPosture
}

// TestTeamSettingsPost_ReviewPosture pins the posture's write path: it defaults
// to the identity-derived value, accepts each known posture, coalesces a blank
// to the default, is left untouched by a save that omits the key (the pointer
// field's whole purpose — an unrelated save must not silently re-gate a team's
// reviews), and rejects an unknown value rather than storing something that
// would quietly resolve to "stage everything".
func TestTeamSettingsPost_ReviewPosture(t *testing.T) {
	s := newTestServer(t)

	if got := teamReviewPosture(t, s); got != domain.DefaultReviewPosture {
		t.Errorf("fresh team posture = %q, want the %q default", got, domain.DefaultReviewPosture)
	}

	for _, p := range domain.ValidReviewPostures {
		postJSONResp(t, s, "/api/settings/team/default", map[string]any{"review_posture": p})
		if got := teamReviewPosture(t, s); got != p {
			t.Errorf("after POST %q, stored posture = %q", p, got)
		}
	}

	// An unrelated save leaves the stored posture alone.
	postJSONResp(t, s, "/api/settings/team/default", map[string]any{"review_posture": domain.ReviewPostureAuto})
	postJSONResp(t, s, "/api/settings/team/default", map[string]any{"ai_model": "haiku"})
	if got := teamReviewPosture(t, s); got != domain.ReviewPostureAuto {
		t.Errorf("an unrelated save clobbered the posture: got %q, want %q", got, domain.ReviewPostureAuto)
	}

	// A blank coalesces to the default rather than persisting an empty column.
	postJSONResp(t, s, "/api/settings/team/default", map[string]any{"review_posture": ""})
	if got := teamReviewPosture(t, s); got != domain.DefaultReviewPosture {
		t.Errorf("blank posture stored as %q, want the %q default", got, domain.DefaultReviewPosture)
	}

	rec := doJSON(t, s, "POST", "/api/settings/team/default", map[string]any{"review_posture": "yolo"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown posture = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := teamReviewPosture(t, s); got != domain.DefaultReviewPosture {
		t.Errorf("a rejected posture must not be stored: got %q", got)
	}
}

// --- base-branch push policy -----------------------------------------------

// teamBasePushPolicy reads the stored policy back off the team settings GET
// (same wholesale-serialized shape as the posture above).
func teamBasePushPolicy(t *testing.T, s *Server) string {
	t.Helper()
	rec := doJSON(t, s, "GET", "/api/settings/team/default", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings/team/default: %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		TeamSettings struct {
			BaseBranchPushPolicy string `json:"BaseBranchPushPolicy"`
		} `json:"team_settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode team settings: %v", err)
	}
	return resp.TeamSettings.BaseBranchPushPolicy
}

// TestTeamSettingsPost_BaseBranchPushPolicy pins the policy's write path, same
// contract as the review posture: defaults to refusing, accepts each known
// value, coalesces a blank to the default, survives an unrelated save
// untouched (an unrelated save must never re-open base-branch pushes, nor
// silently close them on a team that opted in), and rejects an unknown value —
// which would otherwise resolve as "never" forever and read as a bug.
func TestTeamSettingsPost_BaseBranchPushPolicy(t *testing.T) {
	s := newTestServer(t)

	if got := teamBasePushPolicy(t, s); got != domain.DefaultBaseBranchPushPolicy {
		t.Errorf("fresh team policy = %q, want the %q default", got, domain.DefaultBaseBranchPushPolicy)
	}

	for _, p := range domain.ValidBaseBranchPushPolicies {
		postJSONResp(t, s, "/api/settings/team/default", map[string]any{"base_branch_push_policy": p})
		if got := teamBasePushPolicy(t, s); got != p {
			t.Errorf("after POST %q, stored policy = %q", p, got)
		}
	}

	postJSONResp(t, s, "/api/settings/team/default", map[string]any{"base_branch_push_policy": domain.BaseBranchPushAlways})
	postJSONResp(t, s, "/api/settings/team/default", map[string]any{"ai_model": "haiku"})
	if got := teamBasePushPolicy(t, s); got != domain.BaseBranchPushAlways {
		t.Errorf("an unrelated save clobbered the policy: got %q, want %q", got, domain.BaseBranchPushAlways)
	}

	postJSONResp(t, s, "/api/settings/team/default", map[string]any{"base_branch_push_policy": ""})
	if got := teamBasePushPolicy(t, s); got != domain.DefaultBaseBranchPushPolicy {
		t.Errorf("blank policy stored as %q, want the %q default", got, domain.DefaultBaseBranchPushPolicy)
	}

	rec := doJSON(t, s, "POST", "/api/settings/team/default", map[string]any{"base_branch_push_policy": "sometimes"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown policy = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := teamBasePushPolicy(t, s); got != domain.DefaultBaseBranchPushPolicy {
		t.Errorf("a rejected policy must not be stored: got %q", got)
	}
}
