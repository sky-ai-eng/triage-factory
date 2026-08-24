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
// A stored project is the team's commitment to WATCH it; being ARMED — all
// three rules complete — is the later state reached by mapping the workflow's
// statuses. So each rule is independently complete-or-empty, and what is
// refused is a rule that cannot be executed: members with no write target, or a
// write target that is not one of them. The jpsr_*_complete_or_empty CHECK
// constraints are the DB-level mirror of the first half.

func validProject(key string) domain.JiraProjectStatusRules {
	return domain.JiraProjectStatusRules{
		ProjectKey:          key,
		PickupMembers:       []domain.JiraStatusRef{{ID: statusToDoID, Name: "To Do"}},
		InProgressMembers:   []domain.JiraStatusRef{{ID: statusInProgressID, Name: "In Progress"}},
		InProgressCanonical: domain.JiraStatusRef{ID: statusInProgressID, Name: "In Progress"},
		DoneMembers:         []domain.JiraStatusRef{{ID: statusDoneID, Name: "Done"}},
		DoneCanonical:       domain.JiraStatusRef{ID: statusDoneID, Name: "Done"},
	}
}

// validProjectRule is validProject under the name the tests that seed the
// store directly (rather than going through the handler) call it by.
func validProjectRule(key string) domain.JiraProjectStatusRules { return validProject(key) }

func TestValidateProjectRules_Valid(t *testing.T) {
	if err := validateProjectRules(validProject("SKY")); err != nil {
		t.Fatalf("fully-armed project should be valid, got: %v", err)
	}
}

func TestValidateProjectRules_NoRulesAtAll_Valid(t *testing.T) {
	// Watched, unarmed: the state a project lands in when it is picked from the
	// catalog and nobody has mapped its statuses yet.
	if err := validateProjectRules(domain.JiraProjectStatusRules{ProjectKey: "SKY"}); err != nil {
		t.Fatalf("watched-but-unmapped project should be valid, got: %v", err)
	}
}

func TestValidateProjectRules_PickupOnly_Valid(t *testing.T) {
	// Each rule is independently complete-or-empty, so a half-finished mapping
	// persists instead of forcing the whole workflow to be mapped at once.
	p := validProject("SKY")
	p.InProgressMembers, p.InProgressCanonical = nil, domain.JiraStatusRef{}
	p.DoneMembers, p.DoneCanonical = nil, domain.JiraStatusRef{}
	if err := validateProjectRules(p); err != nil {
		t.Fatalf("pickup-only project should be valid, got: %v", err)
	}
}

func TestValidateProjectRules_InProgressMissingCanonical_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.InProgressCanonical = domain.JiraStatusRef{}
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "in_progress canonical is required") {
		t.Errorf("missing in_progress canonical should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_InProgressCanonicalWithoutMembers_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.InProgressMembers = nil
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "in_progress members are required") {
		t.Errorf("canonical with no members should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_InProgressCanonicalNotInMembers_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.InProgressCanonical = domain.JiraStatusRef{ID: "10009", Name: "Doing"}
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "not in members") {
		t.Errorf("canonical-not-in-members should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_DoneMissingCanonical_Rejected(t *testing.T) {
	p := validProject("SKY")
	p.DoneCanonical = domain.JiraStatusRef{}
	err := validateProjectRules(p)
	if err == nil || !strings.Contains(err.Error(), "done canonical is required") {
		t.Errorf("missing done canonical should be rejected, got: %v", err)
	}
}

func TestValidateProjectRules_LegacyNameOnlyRow_Valid(t *testing.T) {
	// A row written before statuses were identified carries names and no ids.
	// It is live data, not a migration that has not run, so it stays valid —
	// the canonical matches its member by name.
	p := domain.JiraProjectStatusRules{
		ProjectKey:          "SKY",
		PickupMembers:       []domain.JiraStatusRef{{Name: "To Do"}},
		InProgressMembers:   []domain.JiraStatusRef{{Name: "In Progress"}},
		InProgressCanonical: domain.JiraStatusRef{Name: "In Progress"},
		DoneMembers:         []domain.JiraStatusRef{{Name: "Done"}},
		DoneCanonical:       domain.JiraStatusRef{Name: "Done"},
	}
	if err := validateProjectRules(p); err != nil {
		t.Fatalf("legacy name-only row should stay valid, got: %v", err)
	}
	if !p.Armed() {
		t.Error("legacy name-only row should still read as armed")
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

// teamProjectsBodyWith builds a replace-set body carrying a single project, to
// exercise one project's rules through the jira-projects PUT. The rules are
// written the way the wire takes them: status IDS, with the display names
// resolved server-side.
func teamProjectsBodyWith(key string, pickup, inProgress, done map[string]any) map[string]any {
	project := map[string]any{"key": key}
	if pickup != nil {
		project["pickup"] = pickup
	}
	if inProgress != nil {
		project["in_progress"] = inProgress
	}
	if done != nil {
		project["done"] = done
	}
	return map[string]any{"jira_projects": []map[string]any{project}}
}

func validPickup() map[string]any {
	return map[string]any{"member_ids": []string{statusToDoID}}
}

func validInProgress() map[string]any {
	return map[string]any{"member_ids": []string{statusInProgressID}, "canonical_id": statusInProgressID}
}

func validDone() map[string]any {
	return map[string]any{"member_ids": []string{statusDoneID}, "canonical_id": statusDoneID}
}

func TestTeamJiraProjectsPut_PickupCanonicalID_Refused(t *testing.T) {
	// Pickup has no write target — TF never transitions a ticket back into it —
	// so the field does not exist on that rule's shape and the strict decode is
	// what says so.
	s, _ := newServerWithJiraCatalog(t, "SKY")
	body := teamProjectsBodyWith("SKY",
		map[string]any{"member_ids": []string{statusToDoID}, "canonical_id": statusToDoID},
		validInProgress(),
		validDone(),
	)
	rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := firstErrorMessage(t, rec); !strings.Contains(msg, "canonical_id") {
		t.Errorf("error should name the field that does not belong on pickup, got: %q", msg)
	}
}

func TestTeamJiraProjectsPut_InProgressCanonicalNotInMembers_Rejected(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")
	body := teamProjectsBodyWith("SKY",
		validPickup(),
		// A real status of this project's workflow, but not one of the members
		// it was chosen from.
		map[string]any{"member_ids": []string{statusInProgressID}, "canonical_id": statusDoneID},
		validDone(),
	)
	rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := firstErrorMessage(t, rec)
	if !strings.Contains(resp, "not in members") {
		t.Errorf("error should mention canonical not in members, got: %q", resp)
	}
}

func TestTeamJiraProjectsPut_InProgressMembersWithoutCanonical_Rejected(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")
	body := teamProjectsBodyWith("SKY",
		validPickup(),
		map[string]any{"member_ids": []string{statusInProgressID}},
		validDone(),
	)
	rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := firstErrorMessage(t, rec)
	if !strings.Contains(resp, "canonical is required") {
		t.Errorf("error should mention canonical required, got: %q", resp)
	}
}

func TestTeamJiraProjectsPut_DoneCanonicalNotInMembers_Rejected(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")
	body := teamProjectsBodyWith("SKY",
		validPickup(),
		validInProgress(),
		map[string]any{"member_ids": []string{statusToDoID}, "canonical_id": statusDoneID},
	)
	rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := firstErrorMessage(t, rec)
	if !strings.Contains(resp, "not in members") {
		t.Errorf("error should mention canonical not in members, got: %q", resp)
	}
}

// TestSettingsPatch_PerProjectRules_RoundTrip verifies the core
// contract: two projects in the same team can carry different rules,
// and saving → loading preserves each project's rules independently.
// Exercises the JiraStatusRulesStore directly (the HTTP handler's
// keychain write isn't available in the test env).
func TestSettingsPatch_PerProjectRules_RoundTrip(t *testing.T) {
	s := newTestServer(t)
	ctx := t.Context()
	teamID := runmode.LocalDefaultTeamID

	rules := []domain.JiraProjectStatusRules{
		{
			ProjectKey:          "SKY",
			PickupMembers:       jiraRefs("Backlog", "Selected"),
			InProgressMembers:   jiraRefs("In Progress"),
			InProgressCanonical: jiraRef("In Progress"),
			DoneMembers:         jiraRefs("Done"),
			DoneCanonical:       jiraRef("Done"),
		},
		{
			ProjectKey:          "OPS",
			PickupMembers:       jiraRefs("New", "Triage"),
			InProgressMembers:   jiraRefs("Active"),
			InProgressCanonical: jiraRef("Active"),
			DoneMembers:         jiraRefs("Resolved", "Verified"),
			DoneCanonical:       jiraRef("Resolved"),
		},
	}
	if err := s.jiraRules.ReplaceForTeam(ctx, teamID, rules); err != nil {
		t.Fatalf("ReplaceForTeam: %v", err)
	}
	got, err := s.jiraRules.ListForTeamSystem(ctx, teamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if r := domain.RuleForProject(got, "SKY"); r == nil || r.InProgressCanonical.Name != "In Progress" || !r.PickupContains(jiraRef("Backlog")) {
		t.Errorf("SKY rules round-trip: %+v", r)
	}
	if r := domain.RuleForProject(got, "OPS"); r == nil || r.DoneCanonical.Name != "Resolved" || !r.PickupContains(jiraRef("Triage")) {
		t.Errorf("OPS rules round-trip: %+v", r)
	}

	// Edit only SKY's rules; OPS must stay untouched.
	for i, p := range rules {
		if p.ProjectKey == "SKY" {
			rules[i].PickupMembers = jiraRefs("Ready")
		}
	}
	if err := s.jiraRules.ReplaceForTeam(ctx, teamID, rules); err != nil {
		t.Fatalf("ReplaceForTeam (edit SKY): %v", err)
	}
	got, err = s.jiraRules.ListForTeamSystem(ctx, teamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if r := domain.RuleForProject(got, "SKY"); r == nil || !r.PickupContains(jiraRef("Ready")) || r.PickupContains(jiraRef("Backlog")) {
		t.Errorf("SKY edit didn't apply: %+v", r)
	}
	if r := domain.RuleForProject(got, "OPS"); r == nil || !r.PickupContains(jiraRef("Triage")) || r.DoneCanonical.Name != "Resolved" {
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
	if r := domain.RuleForProject(got, "OPS"); r == nil || r.DoneCanonical.Name != "Resolved" {
		t.Errorf("OPS rules should persist after dropping SKY: %+v", r)
	}
}

// TestSettingsPatch_DuplicateProjectKey_Rejected verifies that the
// handler rejects two entries with the same key — the rules table
// keys on (team_id, project_key) and a duplicate would silently
// last-write-win.
func TestTeamJiraProjectsPut_DuplicateProjectKey_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := map[string]any{
		"jira_projects": []map[string]any{
			{"key": "SKY", "pickup": validPickup(), "in_progress": validInProgress(), "done": validDone()},
			{"key": "SKY", "pickup": validPickup(), "in_progress": validInProgress(), "done": validDone()},
		},
	}
	rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"), body)
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

	orgRec := doJSON(t, s, "GET", orgSettingsPath(), nil)
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

	teamRec := doJSON(t, s, "GET", teamSettingsPath("default"), nil)
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

// --- model enable-sets ------------------------------------------------------
//
// The set algebra's unit behavior is covered in internal/domain. These cover
// the handler wiring: which selections a save refuses, and what a save that
// succeeds says about the teams it just broke.

// patchTeamSettings PATCHes the team settings row and returns the raw response,
// for tests about a refusal.
func patchTeamSettings(t *testing.T, s *Server, teamID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, s, http.MethodPatch, teamSettingsPath(teamID), body)
}

// patchTeamSettingsOK is patchTeamSettings for the happy path: it fails on any
// non-200 and hands back the decoded settings resource — which is what the
// PATCH answers with, including any advisory `warning`.
func patchTeamSettingsOK(t *testing.T, s *Server, teamID string, body any) map[string]any {
	t.Helper()
	path := teamSettingsPath(teamID)
	rec := patchTeamSettings(t, s, teamID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH %s: %d: %s", path, rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s response: %v", path, err)
	}
	return resp
}

// TestOrgSettingsPatch_BlankGitHubURLWithApp_409 closes the settings-route half
// of the hazard TestGitHubPATDelete_StagedApp_KeepsHost covers for the PAT
// unbind: githubBaseFor falls org_settings → github_url secret → github.com, so
// blanking the column while an App is registered silently sends a GHES org's
// App to github.com. The config route refuses instead of skipping, because here
// the clear is the request itself, not a side effect of one.
func TestOrgSettingsPatch_BlankGitHubURLWithApp_409(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	const host = "https://github.example.com"

	patchOrgSettingsOK(t, s, map[string]any{"github_base_url": host})
	seedLocalApp(t, s, false) // staged is enough — the App resolves against this host either way

	rec := patchOrgSettings(t, s, map[string]any{"github_base_url": nil})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, "github_base_url") {
		t.Errorf("body %s should name the offending field", got)
	}

	var stored string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COALESCE(base_url, '') FROM org_event_sources WHERE org_id = ? AND kind = 'github'`,
		runmode.LocalDefaultOrgID).Scan(&stored); err != nil {
		t.Fatalf("read github base_url: %v", err)
	}
	if stored != host {
		t.Errorf("github base_url = %q, want %q — the refused save must not have partially applied", stored, host)
	}
}

// TestOrgSettingsPatch_BlankGitHubURLWithoutApp_OK is the other half: with no App
// to strand, clearing the host is an ordinary config edit and stays allowed. The
// guard above must not turn into a blanket ban on emptying the field.
func TestOrgSettingsPatch_BlankGitHubURLWithoutApp_OK(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	patchOrgSettingsOK(t, s, map[string]any{"github_base_url": "https://github.example.com"})
	patchOrgSettingsOK(t, s, map[string]any{"github_base_url": nil})

	var stored string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COALESCE(base_url, '') FROM org_event_sources WHERE org_id = ? AND kind = 'github'`,
		runmode.LocalDefaultOrgID).Scan(&stored); err != nil {
		t.Fatalf("read github base_url: %v", err)
	}
	if stored != "" {
		t.Errorf("github base_url = %q, want cleared", stored)
	}
}

// TestOrgSettingsPatch_RetargetGitHubURLWithApp_OK: moving an App org to a
// different non-empty host is a legitimate GHES domain change and stays
// allowed. The guard is aimed at the silent-default case, not at host edits.
func TestOrgSettingsPatch_RetargetGitHubURLWithApp_OK(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	patchOrgSettingsOK(t, s, map[string]any{"github_base_url": "https://old.example.com"})
	seedLocalApp(t, s, true)
	patchOrgSettingsOK(t, s, map[string]any{"github_base_url": "https://new.example.com"})

	var stored string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COALESCE(base_url, '') FROM org_event_sources WHERE org_id = ? AND kind = 'github'`,
		runmode.LocalDefaultOrgID).Scan(&stored); err != nil {
		t.Fatalf("read github base_url: %v", err)
	}
	if stored != "https://new.example.com" {
		t.Errorf("github base_url = %q, want the retargeted host", stored)
	}
}

// TestOrgSettingsPatch_GitHubURLClear_PreservesUserIdentity asserts the
// access/identity decoupling: clearing the org's GitHub URL is an
// access change (PAT_1), and it must NOT touch the user's per-user GitHub
// identity (PAT_2). Identity is owned solely by its own capture surface (the
// setup wizard's User step / the Connect gate page), never mutated as a side
// effect of an org-credential save. A leftover row is durable + harmless (the
// runtime tolerates absent/stale identity) and is still valid if GitHub is
// reconnected to the same host.
func TestOrgSettingsPatch_GitHubURLClear_PreservesUserIdentity(t *testing.T) {
	keyring.MockInit() // in-memory keychain — the sandbox has no dbus backend
	s := newTestServer(t)
	ctx := t.Context()
	const host = "https://github.example.com"

	if err := integrations.Save(ctx, s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{GitHubURL: host}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	if err := s.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "", "", "pat"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	if login, _ := s.users.GetGitHubLogin(ctx, runmode.LocalDefaultUserID, host); login != "octocat" {
		t.Fatalf("precondition: GetGitHubLogin = %q, want octocat", login)
	}

	// Disconnect the org's GitHub access: clear the GitHub URL.
	patchOrgSettingsOK(t, s, map[string]any{"github_base_url": nil})

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
		map[string]any{"deployment": "data_center", "url": jiraStub.URL, "pat": "org_pat"})
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
		map[string]any{"deployment": "cloud", "url": jiraStub.URL, "email": "bot@acme.com", "token": "cloud_tok"})
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
		map[string]any{"deployment": "cloud", "url": jiraStub.URL, "email": "bot@acme.com", "token": "tok"})
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
		map[string]any{"deployment": "data_center", "url": jiraStub.URL, "pat": "dc_pat"})
	if rec.Code != http.StatusOK {
		t.Fatalf("DC connect status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	creds, _ := integrations.Load(ctx, s.secrets, runmode.LocalDefaultOrgID)
	if creds.JiraPAT != "dc_pat" || creds.JiraAuthMethod != string(jira.AuthMethodDCPAT) {
		t.Fatalf("after DC connect, unexpected creds: %+v", creds)
	}

	// 2) Reconnect the same org as Cloud (email + API token).
	rec = doJSON(t, s, "PUT", jiraCredentialRoute(),
		map[string]any{"deployment": "cloud", "url": jiraStub.URL, "email": "bot@acme.com", "token": "cloud_tok"})
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
		map[string]any{"deployment": "data_center", "url": jiraStub.URL, "pat": "dc_pat_2"})
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
		"deployment": "data_center", "url": jiraStub.URL, "pat": "org_pat",
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

// TestOrgSettingsPatch_JiraURLClear_PreservesUserIdentity is the disconnect
// mirror of the GitHub URL-clear test: clearing the org's Jira URL is an
// access change (PAT_1) and must NOT wipe the user's now-independent Jira
// identity. Per-user Jira access is cleared only by its own surface.
func TestOrgSettingsPatch_JiraURLClear_PreservesUserIdentity(t *testing.T) {
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
	patchOrgSettingsOK(t, s, map[string]any{"jira_base_url": nil})

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

// settingsWarning reads the advisory prose off a settings response, or "" when
// the save carried none (omitempty drops the key).
func settingsWarning(resp map[string]any) string {
	w, _ := resp["warning"].(string)
	return w
}

// An org save that disables a model a team still selects SUCCEEDS, and names
// that team. Refusing until every team re-picked would couple an org admin to
// team settings they are barred from editing; saying nothing would leave the
// team to discover it as a failed run.
func TestOrgSettingsPatch_DisablingATeamsDefault_WarnsAndSaves(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	patchTeamSettingsOK(t, s, "default", map[string]any{"ai_model": domain.ModelAliasOpus})

	resp := patchOrgSettingsOK(t, s, map[string]any{"enabled_models": []string{domain.ModelAliasSonnet}})
	w := settingsWarning(resp)
	if !strings.Contains(w, domain.ModelAliasOpus) {
		t.Errorf("warning %q does not name the disabled model", w)
	}
	if !strings.Contains(w, "re-pick") {
		t.Errorf("warning %q does not say what fixes it", w)
	}
	// The save landed: the warning is about a save that happened.
	if got, _ := resp["enabled_models"].([]any); len(got) != 1 {
		t.Errorf("enabled_models = %v, want the one model the save named", resp["enabled_models"])
	}
}

// A save that leaves every team's selection intact says nothing. The warning is
// computed from what the save REMOVED, so widening — or clearing the set back to
// the catalog default — is silent.
func TestOrgSettingsPatch_WideningTheSet_NoWarning(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	patchTeamSettingsOK(t, s, "default", map[string]any{"ai_model": domain.ModelAliasSonnet})

	if w := settingsWarning(patchOrgSettingsOK(t, s, map[string]any{
		"enabled_models": []string{domain.ModelAliasSonnet, domain.ModelAliasOpus},
	})); w != "" {
		t.Errorf("narrowing to a set that keeps the team's default warned: %q", w)
	}
	if w := settingsWarning(patchOrgSettingsOK(t, s, map[string]any{"enabled_models": nil})); w != "" {
		t.Errorf("clearing the set back to the catalog default warned: %q", w)
	}
}

// Every unknown key is named, not the first — an admin fixing a hand-written
// list should see the whole list of what is wrong with it. An empty array is
// refused too: clearing has its own spelling, and a set naming nothing is a
// deployment where nothing runs.
func TestOrgSettingsPatch_EnabledModels_Validation(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := patchOrgSettings(t, s, map[string]any{
		"enabled_models": []string{domain.ModelAliasSonnet, "gpt-9", "llama-99"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown keys = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonInvalidField, "enabled_models")
	for _, want := range []string{"gpt-9", "llama-99"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("refusal does not name %q: %s", want, rec.Body.String())
		}
	}

	if rec := patchOrgSettings(t, s, map[string]any{"enabled_models": []string{}}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty set = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if rec := patchOrgSettings(t, s, map[string]any{
		"enabled_models": []string{domain.ModelAliasSonnet, domain.ModelAliasSonnet},
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("duplicate key = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}

	// A null resets to the catalog default: the settings read shows null, and
	// the models read shows every entry enabled.
	resp := patchOrgSettingsOK(t, s, map[string]any{"enabled_models": nil})
	if resp["enabled_models"] != nil {
		t.Errorf("after a null reset the settings read shows %v, want null", resp["enabled_models"])
	}
	models := doJSON(t, s, http.MethodGet, "/api/orgs/"+runmode.LocalDefaultOrgID+"/models", nil)
	if models.Code != http.StatusOK {
		t.Fatalf("models read = %d: %s", models.Code, models.Body.String())
	}
	for _, item := range decodeModels(t, models.Body.Bytes()).Items {
		if !item.Enabled {
			t.Errorf("%s: not enabled after a reset to the catalog default", item.Key)
		}
	}
}

// The team save is held to the org's set and to its own in one pass, and both
// fields are named when both fail: a save that stored a default the team cannot
// dispatch would report success for a configuration whose only observable
// effect is a failed run later.
func TestTeamSettingsPatch_EnabledModels_HeldToBothSets(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	patchOrgSettingsOK(t, s, map[string]any{
		"enabled_models": []string{domain.ModelAliasSonnet, domain.ModelAliasHaiku},
	})

	// A superset of the org's set is refused, naming enabled_models.
	rec := patchTeamSettings(t, s, "default", map[string]any{
		"enabled_models": []string{domain.ModelAliasSonnet, domain.ModelAliasOpus},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("superset = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	assertFieldError(t, rec, "enabled_models")

	// A default outside the team's own set is refused, naming ai_model.
	rec = patchTeamSettings(t, s, "default", map[string]any{
		"enabled_models": []string{domain.ModelAliasHaiku},
		"ai_model":       domain.ModelAliasSonnet,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("default outside the set = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	assertFieldError(t, rec, "ai_model")

	// Both wrong at once reports both.
	rec = patchTeamSettings(t, s, "default", map[string]any{
		"enabled_models": []string{domain.ModelAliasOpus},
		"ai_model":       domain.ModelAliasSonnet,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("both wrong = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	assertFieldError(t, rec, "enabled_models")
	assertFieldError(t, rec, "ai_model")

	// A legal pair saves, and the team's set round-trips as what it named.
	ok := patchTeamSettingsOK(t, s, "default", map[string]any{
		"enabled_models": []string{domain.ModelAliasHaiku},
		"ai_model":       domain.ModelAliasHaiku,
	})
	set, _ := ok["team_settings"].(map[string]any)
	got, _ := set["EnabledModels"].([]any)
	if len(got) != 1 || got[0] != domain.ModelAliasHaiku {
		t.Errorf("stored team set = %v, want [%s]", set["EnabledModels"], domain.ModelAliasHaiku)
	}
}

// --- TFAC-477 daily spend cap ----------------------------------------------

// orgDailyCap reads the org settings GET and returns max_daily_cost_usd.
func orgDailyCap(t *testing.T, s *Server) float64 {
	t.Helper()
	rec := doJSON(t, s, "GET", orgSettingsPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", orgSettingsPath(), rec.Code, rec.Body.String())
	}
	var resp struct {
		MaxDailyCostUSD float64 `json:"max_daily_cost_usd"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return resp.MaxDailyCostUSD
}

// TestOrgSettingsPatch_DailyCostCap_RoundTrip pins the wire round-trip: a
// positive cap reflects on the next GET, and null clears it back to "no cap".
func TestOrgSettingsPatch_DailyCostCap_RoundTrip(t *testing.T) {
	s := newTestServer(t)

	if got := orgDailyCap(t, s); got != 0 {
		t.Fatalf("fresh org daily cap = %v, want 0 (no cap by default)", got)
	}

	patchOrgSettingsOK(t, s, map[string]any{"max_daily_cost_usd": 25.5})
	if got := orgDailyCap(t, s); got != 25.5 {
		t.Errorf("after setting 25.5, GET max_daily_cost_usd = %v, want 25.5", got)
	}

	// null clears the cap — the one clearing convention.
	patchOrgSettingsOK(t, s, map[string]any{"max_daily_cost_usd": nil})
	if got := orgDailyCap(t, s); got != 0 {
		t.Errorf("after null, GET max_daily_cost_usd = %v, want 0 (cleared)", got)
	}
}

// TestOrgSettingsPatch_DailyCostCap_ZeroRejected: "no cap" has exactly one
// spelling, and it is null. An explicit 0 is refused rather than quietly read as
// "no cap" — capping at $0 and having no cap are different intents, and a caller
// who means the second one already has a way to say it.
func TestOrgSettingsPatch_DailyCostCap_ZeroRejected(t *testing.T) {
	s := newTestServer(t)
	patchOrgSettingsOK(t, s, map[string]any{"max_daily_cost_usd": 12})

	rec := patchOrgSettings(t, s, map[string]any{"max_daily_cost_usd": 0})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("explicit 0 should 422, got %d: %s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonOutOfRange, "max_daily_cost_usd")
	if got := orgDailyCap(t, s); got != 12 {
		t.Errorf("the refused save changed the cap: got %v, want 12", got)
	}
}

// TestOrgSettingsPatch_DailyCostCap_OmittedFieldUntouched pins the absent-means-
// keep half of the convention: a save that omits max_daily_cost_usd must leave a
// previously-set cap intact (an unrelated org save can't stomp the cap).
func TestOrgSettingsPatch_DailyCostCap_OmittedFieldUntouched(t *testing.T) {
	s := newTestServer(t)
	patchOrgSettingsOK(t, s, map[string]any{"max_daily_cost_usd": 40})

	// A save touching only the enable-set omits the cap → it must survive.
	patchOrgSettingsOK(t, s, map[string]any{"enabled_models": []string{domain.ModelAliasSonnet}})
	if got := orgDailyCap(t, s); got != 40 {
		t.Errorf("omitting max_daily_cost_usd cleared the cap: got %v, want 40 preserved", got)
	}
}

// TestOrgSettingsPatch_DailyCostCap_NegativeRejected pins the range check. A
// well-formed number outside the band a field honors is semantic, not a shape
// fault, so it answers 422 OUT_OF_RANGE.
func TestOrgSettingsPatch_DailyCostCap_NegativeRejected(t *testing.T) {
	s := newTestServer(t)
	rec := patchOrgSettings(t, s, map[string]any{"max_daily_cost_usd": -1})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("negative cap should 422, got %d: %s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonOutOfRange, "max_daily_cost_usd")
}

// --- TFAC-638 concurrent-run limit -----------------------------------------

// orgConcurrentRuns reads the org settings GET and returns max_concurrent_runs.
func orgConcurrentRuns(t *testing.T, s *Server) int {
	t.Helper()
	rec := doJSON(t, s, "GET", orgSettingsPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", orgSettingsPath(), rec.Code, rec.Body.String())
	}
	var resp struct {
		MaxConcurrentRuns int `json:"max_concurrent_runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return resp.MaxConcurrentRuns
}

// TestOrgSettingsPatch_ConcurrentRuns_RoundTrip pins the wire round-trip: a
// positive limit reflects on the next GET, and null clears it to "unlimited".
func TestOrgSettingsPatch_ConcurrentRuns_RoundTrip(t *testing.T) {
	s := newTestServer(t)

	if got := orgConcurrentRuns(t, s); got != 0 {
		t.Fatalf("fresh org concurrent-run limit = %v, want 0 (unlimited by default)", got)
	}

	patchOrgSettingsOK(t, s, map[string]any{"max_concurrent_runs": 8})
	if got := orgConcurrentRuns(t, s); got != 8 {
		t.Errorf("after setting 8, GET max_concurrent_runs = %v, want 8", got)
	}

	// null clears the limit — the same convention as the cost cap above.
	patchOrgSettingsOK(t, s, map[string]any{"max_concurrent_runs": nil})
	if got := orgConcurrentRuns(t, s); got != 0 {
		t.Errorf("after null, GET max_concurrent_runs = %v, want 0 (cleared)", got)
	}

	// An explicit 0 is refused, exactly as on the cost cap: "unlimited" is null.
	rec := patchOrgSettings(t, s, map[string]any{"max_concurrent_runs": 0})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("explicit 0 should 422, got %d: %s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonOutOfRange, "max_concurrent_runs")
}

// TestOrgSettingsPatch_ConcurrentRuns_OmittedFieldUntouched pins the absent-
// means-keep half: a save that omits max_concurrent_runs must leave a
// previously-set limit intact (an unrelated org save can't stomp it).
func TestOrgSettingsPatch_ConcurrentRuns_OmittedFieldUntouched(t *testing.T) {
	s := newTestServer(t)
	patchOrgSettingsOK(t, s, map[string]any{"max_concurrent_runs": 5})

	// A save touching only the enable-set omits the limit → it must survive.
	patchOrgSettingsOK(t, s, map[string]any{"enabled_models": []string{domain.ModelAliasSonnet}})
	if got := orgConcurrentRuns(t, s); got != 5 {
		t.Errorf("omitting max_concurrent_runs cleared the limit: got %v, want 5 preserved", got)
	}
}

// TestOrgSettingsPatch_ConcurrentRuns_NegativeRejected pins the range check.
func TestOrgSettingsPatch_ConcurrentRuns_NegativeRejected(t *testing.T) {
	s := newTestServer(t)
	rec := patchOrgSettings(t, s, map[string]any{"max_concurrent_runs": -1})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("negative limit should 422, got %d: %s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonOutOfRange, "max_concurrent_runs")
}

// TestOrgSettingsPatch_ConcurrentRuns_OversizedRejected pins the upper bound: a
// value beyond the ceiling 400s at the handler rather than overflowing the
// Postgres int4 column into a 500.
func TestOrgSettingsPatch_ConcurrentRuns_OversizedRejected(t *testing.T) {
	s := newTestServer(t)
	rec := patchOrgSettings(t, s,
		map[string]any{"max_concurrent_runs": domain.MaxConcurrentClaimsCeiling + 1})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized limit should 422, got %d: %s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonOutOfRange, "max_concurrent_runs")
	// The ceiling itself is accepted.
	patchOrgSettingsOK(t, s,
		map[string]any{"max_concurrent_runs": domain.MaxConcurrentClaimsCeiling})
	if got := orgConcurrentRuns(t, s); got != domain.MaxConcurrentClaimsCeiling {
		t.Errorf("ceiling value should round-trip, got %v want %v", got, domain.MaxConcurrentClaimsCeiling)
	}
}

// --- TFAC-392 unattended-prompt grace window bounds ------------------------

// teamGrace reads the team settings GET and returns the stored grace (ms) plus
// the advertised min/max second bounds that drive the UI slider's range.
func teamGrace(t *testing.T, s *Server) (graceMS, minS, maxS int) {
	t.Helper()
	rec := doJSON(t, s, "GET", teamSettingsPath("default"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", teamSettingsPath("default"), rec.Code, rec.Body.String())
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

// TestTeamSettingsPatch_GraceOutOfBandRejected pins the write-side band check: a
// value outside [min, max] is REJECTED, and an in-band one round-trips
// verbatim. It used to clamp, which answered "saved" for a grace the caller
// never asked for — invisible to an API client, since the UI slider can only
// express in-band values in the first place. A well-formed number outside the
// band it honors is semantic rather than a shape fault, so it answers 422.
func TestTeamSettingsPatch_GraceOutOfBandRejected(t *testing.T) {
	s := newTestServer(t)

	before, _, _ := teamGrace(t, s)
	for _, secs := range []int{99999, 0, -1} {
		rec := doJSON(t, s, "PATCH", teamSettingsPath("default"), map[string]any{
			"permission_absent_grace_seconds": secs,
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("grace %ds = %d, want 422; body=%s", secs, rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonOutOfRange, "permission_absent_grace_seconds")
		if ms, _, _ := teamGrace(t, s); ms != before {
			t.Errorf("rejected grace %ds still moved the stored value to %dms", secs, ms)
		}
	}

	patchTeamSettingsOK(t, s, "default", map[string]any{
		"permission_absent_grace_seconds": 20,
	})
	if ms, _, _ := teamGrace(t, s); ms != 20000 {
		t.Errorf("after saving 20s, stored grace = %dms, want 20000ms (in-band, verbatim)", ms)
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
	rec := doJSON(t, s, "GET", teamSettingsPath("default"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", teamSettingsPath("default"), rec.Code, rec.Body.String())
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

// TestTeamSettingsPatch_ReviewPosture pins the posture's write path: it defaults
// to the identity-derived value, accepts each known posture, resets to the
// default on an explicit null, is left untouched by a save that omits the key
// (an unrelated save must not silently re-gate a team's reviews), and rejects an
// unknown value rather than storing something that would quietly resolve to
// "stage everything". A blank string is NOT a spelling of the reset any more —
// null is the one clearing convention, and "" is just an unknown posture.
func TestTeamSettingsPatch_ReviewPosture(t *testing.T) {
	s := newTestServer(t)

	if got := teamReviewPosture(t, s); got != domain.DefaultReviewPosture {
		t.Errorf("fresh team posture = %q, want the %q default", got, domain.DefaultReviewPosture)
	}

	for _, p := range domain.ValidReviewPostures {
		patchTeamSettingsOK(t, s, "default", map[string]any{"review_posture": p})
		if got := teamReviewPosture(t, s); got != p {
			t.Errorf("after saving %q, stored posture = %q", p, got)
		}
	}

	// An unrelated save leaves the stored posture alone.
	patchTeamSettingsOK(t, s, "default", map[string]any{"review_posture": domain.ReviewPostureAuto})
	patchTeamSettingsOK(t, s, "default", map[string]any{"ai_model": domain.ModelAliasHaiku})
	if got := teamReviewPosture(t, s); got != domain.ReviewPostureAuto {
		t.Errorf("an unrelated save clobbered the posture: got %q, want %q", got, domain.ReviewPostureAuto)
	}

	// null resets to the default rather than persisting an empty column.
	patchTeamSettingsOK(t, s, "default", map[string]any{"review_posture": nil})
	if got := teamReviewPosture(t, s); got != domain.DefaultReviewPosture {
		t.Errorf("null posture stored as %q, want the %q default", got, domain.DefaultReviewPosture)
	}

	// ...and a blank string is refused, not read as the reset.
	if rec := doJSON(t, s, "PATCH", teamSettingsPath("default"), map[string]any{"review_posture": ""}); rec.Code != http.StatusBadRequest {
		t.Errorf("blank posture = %d, want 400 (null is the reset); body=%s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, s, "PATCH", teamSettingsPath("default"), map[string]any{"review_posture": "yolo"})
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
	rec := doJSON(t, s, "GET", teamSettingsPath("default"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", teamSettingsPath("default"), rec.Code, rec.Body.String())
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

// TestTeamSettingsPatch_BaseBranchPushPolicy pins the policy's write path, same
// contract as the review posture: defaults to refusing, accepts each known
// value, resets to the default on null, survives an unrelated save untouched
// (an unrelated save must never re-open base-branch pushes, nor silently close
// them on a team that opted in), and rejects an unknown value — which would
// otherwise resolve as "never" forever and read as a bug.
func TestTeamSettingsPatch_BaseBranchPushPolicy(t *testing.T) {
	s := newTestServer(t)

	if got := teamBasePushPolicy(t, s); got != domain.DefaultBaseBranchPushPolicy {
		t.Errorf("fresh team policy = %q, want the %q default", got, domain.DefaultBaseBranchPushPolicy)
	}

	for _, p := range domain.ValidBaseBranchPushPolicies {
		patchTeamSettingsOK(t, s, "default", map[string]any{"base_branch_push_policy": p})
		if got := teamBasePushPolicy(t, s); got != p {
			t.Errorf("after saving %q, stored policy = %q", p, got)
		}
	}

	patchTeamSettingsOK(t, s, "default", map[string]any{"base_branch_push_policy": domain.BaseBranchPushAlways})
	patchTeamSettingsOK(t, s, "default", map[string]any{"ai_model": domain.ModelAliasHaiku})
	if got := teamBasePushPolicy(t, s); got != domain.BaseBranchPushAlways {
		t.Errorf("an unrelated save clobbered the policy: got %q, want %q", got, domain.BaseBranchPushAlways)
	}

	patchTeamSettingsOK(t, s, "default", map[string]any{"base_branch_push_policy": nil})
	if got := teamBasePushPolicy(t, s); got != domain.DefaultBaseBranchPushPolicy {
		t.Errorf("null policy stored as %q, want the %q default", got, domain.DefaultBaseBranchPushPolicy)
	}
	if rec := doJSON(t, s, "PATCH", teamSettingsPath("default"), map[string]any{"base_branch_push_policy": ""}); rec.Code != http.StatusBadRequest {
		t.Errorf("blank policy = %d, want 400 (null is the reset); body=%s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, s, "PATCH", teamSettingsPath("default"), map[string]any{"base_branch_push_policy": "sometimes"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown policy = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := teamBasePushPolicy(t, s); got != domain.DefaultBaseBranchPushPolicy {
		t.Errorf("a rejected policy must not be stored: got %q", got)
	}
}
