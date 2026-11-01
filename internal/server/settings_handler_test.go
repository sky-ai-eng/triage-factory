package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
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
// statuses. The jpsr_*_complete_or_empty CHECK constraints are the DB-level
// mirror of each rule's complete-or-empty requirement.

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

// TestValidateProjectRules: each rule is independently complete-or-empty, so a
// watched project with no rules or a half-finished mapping is valid, and a rule
// that cannot be executed (members with no write target, or a write target that
// is not one of them) is refused.
func TestValidateProjectRules(t *testing.T) {
	for _, c := range []struct {
		name      string
		mutate    func(p *domain.JiraProjectStatusRules)
		wantErr   string // empty: the project is valid
		wantArmed bool
	}{
		{"fully armed", nil, "", true},
		{"watched with no rules", func(p *domain.JiraProjectStatusRules) {
			*p = domain.JiraProjectStatusRules{ProjectKey: "SKY"}
		}, "", false},
		{"pickup only", func(p *domain.JiraProjectStatusRules) {
			p.InProgressMembers, p.InProgressCanonical = nil, domain.JiraStatusRef{}
			p.DoneMembers, p.DoneCanonical = nil, domain.JiraStatusRef{}
		}, "", false},
		// Rows written before statuses were identified carry names and no ids.
		// They are live data, so they stay valid: the canonical matches its
		// member by name.
		{"name-only row", func(p *domain.JiraProjectStatusRules) {
			*p = domain.JiraProjectStatusRules{
				ProjectKey:          "SKY",
				PickupMembers:       []domain.JiraStatusRef{{Name: "To Do"}},
				InProgressMembers:   []domain.JiraStatusRef{{Name: "In Progress"}},
				InProgressCanonical: domain.JiraStatusRef{Name: "In Progress"},
				DoneMembers:         []domain.JiraStatusRef{{Name: "Done"}},
				DoneCanonical:       domain.JiraStatusRef{Name: "Done"},
			}
		}, "", true},
		{"in_progress missing canonical", func(p *domain.JiraProjectStatusRules) {
			p.InProgressCanonical = domain.JiraStatusRef{}
		}, "in_progress canonical is required", false},
		{"in_progress canonical without members", func(p *domain.JiraProjectStatusRules) {
			p.InProgressMembers = nil
		}, "in_progress members are required", false},
		{"in_progress canonical not in members", func(p *domain.JiraProjectStatusRules) {
			p.InProgressCanonical = domain.JiraStatusRef{ID: "10009", Name: "Doing"}
		}, "not in members", false},
		{"done missing canonical", func(p *domain.JiraProjectStatusRules) {
			p.DoneCanonical = domain.JiraStatusRef{}
		}, "done canonical is required", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := validProject("SKY")
			if c.mutate != nil {
				c.mutate(&p)
			}
			err := validateProjectRules(p)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got: %v", err)
				}
				if p.Armed() != c.wantArmed {
					t.Errorf("Armed() = %v, want %v", p.Armed(), c.wantArmed)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("want an error containing %q, got: %v", c.wantErr, err)
			}
		})
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

func TestJiraProjectIDRe(t *testing.T) {
	// A Jira project id is a decimal integer as both deployments write one; a
	// project key is not one, so it never reaches Jira as an id.
	valid := []string{"10000", "1", "42"}
	invalid := []string{"", "SKY", "-1", "10 000", "1e3", " 10000"}
	for _, id := range valid {
		if !jiraProjectIDRe.MatchString(id) {
			t.Errorf("expected %q to match jiraProjectIDRe", id)
		}
	}
	for _, id := range invalid {
		if jiraProjectIDRe.MatchString(id) {
			t.Errorf("expected %q to NOT match jiraProjectIDRe", id)
		}
	}
}

// --- Handler tests: the jira-projects PUT rejects invalid rules -------------

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

// TestTeamJiraProjectsPut_InvalidRulesRejected: the PUT refuses a rule it could
// not execute with a 400 whose message names the fault.
func TestTeamJiraProjectsPut_InvalidRulesRejected(t *testing.T) {
	for _, c := range []struct {
		name                     string
		pickup, inProgress, done map[string]any
		wantMsg                  string
	}{
		// Pickup has no write target — TF never transitions a ticket back into
		// it — so the field does not exist on that rule's shape and the strict
		// decode is what says so.
		{"pickup canonical_id",
			map[string]any{"member_ids": []string{statusToDoID}, "canonical_id": statusToDoID}, validInProgress(), validDone(),
			"canonical_id"},
		// A real status of this project's workflow, but not one of the members
		// it was chosen from.
		{"in_progress canonical not in members",
			validPickup(), map[string]any{"member_ids": []string{statusInProgressID}, "canonical_id": statusDoneID}, validDone(),
			"not in members"},
		{"in_progress members without canonical",
			validPickup(), map[string]any{"member_ids": []string{statusInProgressID}}, validDone(),
			"canonical is required"},
		{"done canonical not in members",
			validPickup(), validInProgress(), map[string]any{"member_ids": []string{statusToDoID}, "canonical_id": statusDoneID},
			"not in members"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newServerWithJiraCatalog(t, "SKY")
			rec := putTeamJiraProjects(t, s, teamProjectsBodyWith("SKY", c.pickup, c.inProgress, c.done))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if msg := firstErrorMessage(t, rec); !strings.Contains(msg, c.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", msg, c.wantMsg)
			}
		})
	}
}

// TestTeamJiraProjectsPut_DuplicateProject_Rejected verifies that the handler
// rejects two entries naming one project — two entries for one project would
// silently last-write-win.
func TestTeamJiraProjectsPut_DuplicateProject_Rejected(t *testing.T) {
	s := newTestServer(t)
	body := map[string]any{
		"jira_projects": []map[string]any{
			{"key": "SKY", "pickup": validPickup(), "in_progress": validInProgress(), "done": validDone()},
			{"key": "SKY", "pickup": validPickup(), "in_progress": validInProgress(), "done": validDone()},
		},
	}
	rec := putTeamJiraProjects(t, s, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on a duplicate project, got %d: %s", rec.Code, rec.Body.String())
	}
	items := decodeErrorItems(t, rec)
	if len(items) != 1 || items[0].Field != "jira_projects[1].jira_id" || !strings.Contains(items[0].Message, "already in this set at jira_projects[0]") {
		t.Errorf("errors = %+v, want one naming the second entry as the first's project", items)
	}
}

// TestSettingsGet_MemberCountAndRole verifies the scope GET responses carry
// the membership signals the frontend's N=1 collapse + role gating read.
// Local mode is the single-member case: one member, and the caller is team admin.
func TestSettingsGet_MemberCountAndRole(t *testing.T) {
	s := newTestServer(t)

	orgRec := doJSON(t, s, "GET", orgSettingsPath(), nil)
	if orgRec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", orgSettingsPath(), orgRec.Code, orgRec.Body.String())
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
		t.Fatalf("GET %s: %d: %s", teamSettingsPath("default"), teamRec.Code, teamRec.Body.String())
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

// TestOrgSettingsPatch_GitHubURLHeldByACredential_409: a token or an App works
// only on the GitHub that issued it, so while one is bound (a staged App
// included) the URL neither moves nor clears: a 409 on github_base_url, and the
// stored host stays. Moving hosts is a disconnect and a fresh connect.
func TestOrgSettingsPatch_GitHubURLHeldByACredential_409(t *testing.T) {
	const oldHost = "https://old.example.com"
	bindPAT := func(t *testing.T, s *Server) {
		t.Helper()
		if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{GitHubPAT: "ghp_bound"}); err != nil {
			t.Fatalf("seed creds: %v", err)
		}
	}
	for _, tc := range []struct {
		name   string
		bind   func(t *testing.T, s *Server)
		target any
	}{
		{"active App, retarget", func(t *testing.T, s *Server) { seedLocalApp(t, s, true) }, "https://new.example.com"},
		{"staged App, clear", func(t *testing.T, s *Server) { seedLocalApp(t, s, false) }, nil},
		{"PAT, retarget", bindPAT, "https://new.example.com"},
		{"PAT, clear", bindPAT, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			patchOrgSettingsOK(t, s, map[string]any{"github_base_url": oldHost})
			tc.bind(t, s)

			rec := patchOrgSettings(t, s, map[string]any{"github_base_url": tc.target})
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}
			if e := firstError(t, rec); e.Field != "github_base_url" {
				t.Errorf("error = %+v, want it on github_base_url", e)
			}
			if got := sourceHost(t, s, eventsource.KindGitHub); got != oldHost {
				t.Errorf("github base_url = %q, want %q kept", got, oldHost)
			}
		})
	}
}

// TestOrgSettingsPatch_RetargetGitHubURLWithInstallation: an org on the
// deployment's App cannot move off a host one of its installations is on. An
// installation on some other GitHub records its own host and holds nothing.
func TestOrgSettingsPatch_RetargetGitHubURLWithInstallation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		instHost string
		wantCode int
		wantBase string
	}{
		{"installed on the current host", "https://old.example.com", http.StatusConflict, "https://old.example.com"},
		{"installed only on another host", "https://elsewhere.example.com", http.StatusOK, "https://new.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			ctx := t.Context()

			patchOrgSettingsOK(t, s, map[string]any{"github_base_url": "https://old.example.com"})
			if _, err := s.orgs.SetGitHubCredentialClass(ctx, runmode.LocalDefaultOrgID, domain.GitHubCredentialClassManagedApp); err != nil {
				t.Fatalf("set credential class: %v", err)
			}
			if _, err := s.githubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
				InstallationID: "456", OrgID: runmode.LocalDefaultOrgID, AccountType: "Organization",
				AccountLogin: "acme", GitHubHost: tc.instHost,
			}); err != nil {
				t.Fatalf("seed installation: %v", err)
			}

			rec := patchOrgSettings(t, s, map[string]any{"github_base_url": "https://new.example.com"})
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if got := sourceHost(t, s, eventsource.KindGitHub); got != tc.wantBase {
				t.Errorf("github base_url = %q, want %q", got, tc.wantBase)
			}
		})
	}
}

// TestOrgSettingsPatch_GitHubURLClear_PreservesUserIdentity: with nothing bound,
// clearing the org's GitHub URL is allowed and clears it, and the caller's
// per-user GitHub identity survives: only its own capture surface writes it.
func TestOrgSettingsPatch_GitHubURLClear_PreservesUserIdentity(t *testing.T) {
	keyring.MockInit() // in-memory keychain — the sandbox has no dbus backend
	s := newTestServer(t)
	ctx := t.Context()
	const host = "https://github.example.com"

	seedSourceHost(t, s, eventsource.KindGitHub, host)
	if err := integrations.Save(ctx, s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	if err := s.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "", "", "pat"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	if login, _ := s.users.GetGitHubLogin(ctx, runmode.LocalDefaultUserID, host); login != "octocat" {
		t.Fatalf("precondition: GetGitHubLogin = %q, want octocat", login)
	}

	patchOrgSettingsOK(t, s, map[string]any{"github_base_url": nil})
	if got := sourceHost(t, s, eventsource.KindGitHub); got != "" {
		t.Errorf("github base_url = %q, want cleared", got)
	}

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
// The org's Jira service credential never writes or clears a caller's per-user
// Jira identity or credential; only the identity bind route writes those. Each
// test seeds a user identity or credential and checks the org operation left it.

// TestJiraConnect_DoesNotWriteUserIdentity: binding the org's Jira credential
// validates it against /myself but does not bind the caller's identity to that
// account, which is the org bot's.
func TestJiraConnect_DoesNotWriteUserIdentity(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit() // in-memory keychain — the sandbox has no dbus backend
	s := newTestServer(t)
	ctx := t.Context()

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

// TestJiraConnect_Cloud_MismatchHint: a Cloud credential rejected by a host that
// is not a Cloud host is a 422 hinting at the Data Center scheme. The hint is
// advisory: validation, not the hostname, gates storage.
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

// TestJiraConnect_SchemeSwitch_ClearsStaleCredential: reconnecting the org under
// the other Jira auth scheme stores that scheme's credential and marker and drops
// the previous one's secret, in both directions. Cloud validates with Basic
// email:token against REST v3.
func TestJiraConnect_SchemeSwitch_ClearsStaleCredential(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()

	// One stub answering both REST v2 (DC validate) and v3 (Cloud validate).
	var gotAuth, gotPath string
	jiraStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
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
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@acme.com:cloud_tok")); gotAuth != want {
		t.Errorf("validate Authorization = %q, want %q (Cloud uses Basic email:token)", gotAuth, want)
	}
	if gotPath != "/rest/api/3/myself" {
		t.Errorf("validate path = %q, want /rest/api/3/myself (Cloud uses REST v3)", gotPath)
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

// TestOrgSettingsPatch_JiraURLClear_PreservesUserIdentity: clearing the Jira URL
// of an org with no service credential (its members bind their own) leaves the
// caller's Jira identity in place.
func TestOrgSettingsPatch_JiraURLClear_PreservesUserIdentity(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()
	const host = "https://jira.example.com"

	seedSourceHost(t, s, eventsource.KindJira, host)
	if err := s.users.UpsertJiraIdentity(ctx, runmode.LocalDefaultUserID, host, "user-acct", "User Name", "pat"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	patchOrgSettingsOK(t, s, map[string]any{"jira_base_url": nil})

	accountID, displayName, err := s.users.GetJiraIdentity(ctx, runmode.LocalDefaultUserID, host)
	if err != nil {
		t.Fatalf("GetJiraIdentity after clear: %v", err)
	}
	if accountID != "user-acct" || displayName != "User Name" {
		t.Errorf("org Jira disconnect wiped the caller's identity: got (%q, %q), want it preserved", accountID, displayName)
	}
}

// TestJiraCredentialDelete_PreservesUserCredential: unbinding the org's Jira
// credential deletes only the org key; the caller's own credential, stored
// under the per-user "jira_token/<host>" key, survives.
func TestJiraCredentialDelete_PreservesUserCredential(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()
	const host = "https://jira.example.com"

	seedSourceHost(t, s, eventsource.KindJira, host)
	if err := integrations.Save(ctx, s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{JiraPAT: "org-pat"}); err != nil {
		t.Fatalf("seed org creds: %v", err)
	}
	if err := s.secrets.PutUser(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "jira_token/"+host, "user-token", "Jira user access token"); err != nil {
		t.Fatalf("seed user credential: %v", err)
	}

	rec := doJSON(t, s, "DELETE", jiraCredentialRoute(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("jira unbind = %d, body=%s", rec.Code, rec.Body.String())
	}

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

// --- daily spend cap and concurrent-run limit ------------------------------

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

// TestOrgSettingsPatch_DailyCostCap_OmittedFieldUntouched: a fresh org has no
// cost cap and no concurrent-run limit, and a save that omits them leaves the
// stored values alone.
func TestOrgSettingsPatch_DailyCostCap_OmittedFieldUntouched(t *testing.T) {
	s := newTestServer(t)
	if capUSD, runs := orgDailyCap(t, s), orgConcurrentRuns(t, s); capUSD != 0 || runs != 0 {
		t.Fatalf("fresh org cap = %v, concurrent runs = %v; want 0 and 0 (no limit by default)", capUSD, runs)
	}
	patchOrgSettingsOK(t, s, map[string]any{"max_daily_cost_usd": 40, "max_concurrent_runs": 5})

	// A save touching only the enable-set omits both → they must survive.
	patchOrgSettingsOK(t, s, map[string]any{"enabled_models": []string{domain.ModelAliasSonnet}})
	if got := orgDailyCap(t, s); got != 40 {
		t.Errorf("omitting max_daily_cost_usd cleared the cap: got %v, want 40 preserved", got)
	}
	if got := orgConcurrentRuns(t, s); got != 5 {
		t.Errorf("omitting max_concurrent_runs cleared the limit: got %v, want 5 preserved", got)
	}
}

// TestOrgSettingsPatch_LimitsOutOfRange: zero, a negative, or a value past the
// ceiling is a 422 OUT_OF_RANGE that leaves the stored limit alone. "No limit"
// is spelled null, and the ceiling itself is accepted.
func TestOrgSettingsPatch_LimitsOutOfRange(t *testing.T) {
	s := newTestServer(t)
	patchOrgSettingsOK(t, s, map[string]any{"max_daily_cost_usd": 12, "max_concurrent_runs": 4})

	for _, c := range []struct {
		field string
		value int
	}{
		{"max_daily_cost_usd", 0},
		{"max_daily_cost_usd", -1},
		{"max_concurrent_runs", 0},
		{"max_concurrent_runs", -1},
		// Past the ceiling it would overflow the Postgres int4 column into a 500.
		{"max_concurrent_runs", domain.MaxConcurrentClaimsCeiling + 1},
	} {
		t.Run(fmt.Sprintf("%s=%d", c.field, c.value), func(t *testing.T) {
			rec := patchOrgSettings(t, s, map[string]any{c.field: c.value})
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
			}
			assertFirstError(t, rec, httpx.ReasonOutOfRange, c.field)
		})
	}
	if got := orgDailyCap(t, s); got != 12 {
		t.Errorf("a refused save changed the cap: got %v, want 12", got)
	}
	if got := orgConcurrentRuns(t, s); got != 4 {
		t.Errorf("a refused save changed the concurrent-run limit: got %v, want 4", got)
	}

	patchOrgSettingsOK(t, s, map[string]any{"max_concurrent_runs": domain.MaxConcurrentClaimsCeiling})
	if got := orgConcurrentRuns(t, s); got != domain.MaxConcurrentClaimsCeiling {
		t.Errorf("ceiling value should round-trip, got %v want %v", got, domain.MaxConcurrentClaimsCeiling)
	}
}

// --- unattended-prompt grace window bounds ---------------------------------

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

// TestTeamSettingsPatch_GraceOutOfBandRejected: a grace outside the band is a
// 422 OUT_OF_RANGE that leaves the stored value, never a clamp, and an in-band
// value round-trips verbatim.
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
// mode.
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

// TestTeamSettingsPatch_ReviewPosture: the posture defaults to the
// identity-derived value, accepts each known posture, resets on null, survives
// an unrelated save, and refuses a blank or unknown value rather than storing it.
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

// TestTeamSettingsPatch_BaseBranchPushPolicy: the same contract as the review
// posture. An unrelated save must neither open nor close base-branch pushes, and
// an unknown value, which would resolve as "never", is refused.
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

// changeKicks records the GitHub and Jira change hooks a write fires. The hooks
// are set directly rather than through SetOnGitHubChanged, which would also
// start a reachable-repo refresh this test is not about.
type changeKicks struct{ github, jira chan string }

func recordChangeKicks(s *Server) changeKicks {
	k := changeKicks{github: make(chan string, 8), jira: make(chan string, 8)}
	s.onGitHubChanged = func(orgID string) { k.github <- orgID }
	s.onJiraChanged = func(orgID string) { k.jira <- orgID }
	return k
}

// fired reports whether ch received a kick within a short window.
func fired(ch chan string) bool {
	select {
	case <-ch:
		return true
	case <-time.After(300 * time.Millisecond):
		return false
	}
}

// markPollsComplete records a completed poll for every source in sources, so a
// later write's restart mark is observable as readiness going false.
func markPollsComplete(t *testing.T, s *Server, sources ...string) {
	t.Helper()
	for _, src := range sources {
		if err := s.allStores.PollReadiness.MarkPollComplete(t.Context(), runmode.LocalDefaultOrgID, src, time.Time{}); err != nil {
			t.Fatalf("MarkPollComplete(%s): %v", src, err)
		}
	}
}

// pollReady reads source's poll readiness for the local org.
func pollReady(t *testing.T, s *Server, source string) bool {
	t.Helper()
	ok, err := s.allStores.PollReadiness.Ready(t.Context(), runmode.LocalDefaultOrgID, source)
	if err != nil {
		t.Fatalf("Ready(%s): %v", source, err)
	}
	return ok
}

// TestOrgSettingsPatch_GitHubChange_RestartsGitHubReadinessOnly: a GitHub host
// change kicks the GitHub poller and clears GitHub poll readiness only; a save
// changing GitHub and Jira together kicks and clears both.
func TestOrgSettingsPatch_GitHubChange_RestartsGitHubReadinessOnly(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	kicks := recordChangeKicks(s)

	markPollsComplete(t, s, "github", "jira")
	patchOrgSettingsOK(t, s, map[string]any{"github_base_url": "https://ghe.example.com"})
	if !fired(kicks.github) {
		t.Error("a GitHub host change did not kick the GitHub poller")
	}
	if fired(kicks.jira) {
		t.Error("a GitHub-only change kicked the Jira poller")
	}
	if pollReady(t, s, "github") {
		t.Error("GitHub readiness survived a GitHub host change")
	}
	if !pollReady(t, s, "jira") {
		t.Error("a GitHub-only change cleared Jira readiness")
	}

	markPollsComplete(t, s, "github", "jira")
	patchOrgSettingsOK(t, s, map[string]any{
		"github_base_url": "https://ghe2.example.com",
		"jira_base_url":   "https://jira.example.com",
	})
	if !fired(kicks.github) || !fired(kicks.jira) {
		t.Error("a combined GitHub and Jira change must kick both pollers")
	}
	if pollReady(t, s, "github") || pollReady(t, s, "jira") {
		t.Error("a combined GitHub and Jira change must clear both readiness rows")
	}
}
