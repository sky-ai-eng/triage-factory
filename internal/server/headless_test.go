package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestParseCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a", []string{"a"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ,, c ", []string{"a", "b", "c"}}, // trim + drop empties
		{"In Progress,Backlog", []string{"In Progress", "Backlog"}},
	}
	for _, c := range cases {
		if got := parseCSV(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseCSV(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseRepos(t *testing.T) {
	got := parseRepos(" acme/api , acme/web ,bogus, /no-owner , no-repo/ ,octo/cat")
	want := []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "api"},
		{Owner: "acme", Repo: "web"},
		{Owner: "octo", Repo: "cat"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseRepos = %v, want %v", got, want)
	}
	if parseRepos("") != nil {
		t.Errorf("parseRepos(\"\") should be nil")
	}
}

func TestParseCloneProtocol(t *testing.T) {
	cases := map[string]string{
		"":        "https", // default
		"https":   "https",
		"HTTPS":   "https",
		"  ssh  ": "ssh",
		"SSH":     "ssh",
		"git":     "https", // invalid → fall back to https
		"http":    "https", // invalid → fall back to https
	}
	for in, want := range cases {
		if got := parseCloneProtocol(in); got != want {
			t.Errorf("parseCloneProtocol(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeadlessJiraRules(t *testing.T) {
	cfg := headlessConfig{
		jiraProjects:         []string{"SKY", "TFAC"},
		jiraPickupStatuses:   []string{"To Do", "Backlog"},
		jiraInProgressStatus: "In Progress",
		jiraDoneStatus:       "Done",
	}
	rules := headlessJiraRules(cfg)
	if len(rules) != 2 {
		t.Fatalf("want 2 rules (one per project), got %d", len(rules))
	}
	for i, key := range []string{"SKY", "TFAC"} {
		r := rules[i]
		if r.ProjectKey != key {
			t.Errorf("rule[%d].ProjectKey = %q, want %q", i, r.ProjectKey, key)
		}
		// The same global mapping is applied to every project.
		if !reflect.DeepEqual(r.PickupMembers, []string{"To Do", "Backlog"}) {
			t.Errorf("rule[%d].PickupMembers = %v", i, r.PickupMembers)
		}
		// CHECK constraints want non-empty members + canonical for both write targets.
		if r.InProgressCanonical != "In Progress" || !reflect.DeepEqual(r.InProgressMembers, []string{"In Progress"}) {
			t.Errorf("rule[%d] in-progress = %q/%v", i, r.InProgressCanonical, r.InProgressMembers)
		}
		if r.DoneCanonical != "Done" || !reflect.DeepEqual(r.DoneMembers, []string{"Done"}) {
			t.Errorf("rule[%d] done = %q/%v", i, r.DoneCanonical, r.DoneMembers)
		}
	}
}

func TestHeadlessConfig_JiraCompleteAndIntent(t *testing.T) {
	full := headlessConfig{
		jiraProjects:         []string{"SKY"},
		jiraPickupStatuses:   []string{"To Do"},
		jiraInProgressStatus: "In Progress",
		jiraDoneStatus:       "Done",
	}
	if !full.jiraComplete() || !full.jiraIntent() {
		t.Errorf("full config should be complete and show intent")
	}
	// Missing the done status → intent but not complete.
	partial := full
	partial.jiraDoneStatus = ""
	if partial.jiraComplete() {
		t.Errorf("partial config should not be complete")
	}
	if !partial.jiraIntent() {
		t.Errorf("partial config still shows intent (worth a warn)")
	}
	// Nothing Jira-related → neither.
	var none headlessConfig
	if none.jiraComplete() || none.jiraIntent() {
		t.Errorf("empty config should show no intent and not be complete")
	}
	// Only a user PAT counts as intent (the operator meant to wire Jira).
	onlyPAT := headlessConfig{jiraUserPAT: "x"}
	if !onlyPAT.jiraIntent() || onlyPAT.jiraComplete() {
		t.Errorf("user-PAT-only should show intent but not be complete")
	}
}

func TestHeadlessEnabled(t *testing.T) {
	t.Setenv(envHeadless, "")
	if HeadlessEnabled() {
		t.Errorf("empty TF_HEADLESS should be disabled")
	}
	t.Setenv(envHeadless, "1")
	if !HeadlessEnabled() {
		t.Errorf("TF_HEADLESS=1 should be enabled")
	}
}

// TestRunHeadlessBootstrap_MultiModeNoOp pins the load-bearing invariant: even
// if invoked directly in multi mode, the bootstrap is a no-op and never touches
// a tenant. A bare &Server{} is safe because the guard returns before any field
// is read.
func TestRunHeadlessBootstrap_MultiModeNoOp(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := &Server{}
	if err := s.RunHeadlessBootstrap(context.Background()); err != nil {
		t.Fatalf("multi-mode RunHeadlessBootstrap should be a nil-error no-op, got %v", err)
	}
}

// TestRunHeadlessBootstrap_NoGitHubCredsSkips verifies that with no GitHub bot
// credentials in env, the bootstrap warns and skips before seeding — even when
// repo seed vars are present.
func TestRunHeadlessBootstrap_NoGitHubCredsSkips(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	auth.ResetSecretBackendForTest(t)
	s := newTestServer(t)
	ctx := t.Context()

	// Seed vars present, but no GitHub URL/PAT → must skip everything.
	t.Setenv(envHeadless, "1")
	t.Setenv(envRepos, "acme/api")

	if err := s.RunHeadlessBootstrap(ctx); err != nil {
		t.Fatalf("RunHeadlessBootstrap: %v", err)
	}
	repos, err := s.allStores.TeamGitHubRepos.ListForTeamSystem(ctx, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if len(repos) != 0 {
		t.Errorf("expected no repos seeded when GitHub creds are absent, got %d", len(repos))
	}
	orgSet, err := s.orgs.GetSettingsSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	if orgSet.GitHubBaseURL != "" {
		t.Errorf("expected GitHubBaseURL untouched, got %q", orgSet.GitHubBaseURL)
	}
}

// TestRunHeadlessBootstrap_HappyPath drives a full env-only provision against a
// stubbed GitHub host: tenant provisioned, repos tracked, identity bound — then
// asserts idempotency and that a later UI edit is never clobbered.
func TestRunHeadlessBootstrap_HappyPath(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	auth.ResetSecretBackendForTest(t)

	// Stub GitHub: ValidateGitHub (bot + user) hits {host}/api/v3/user.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/user" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	t.Cleanup(gh.Close)

	t.Setenv(envHeadless, "1")
	t.Setenv("TRIAGE_FACTORY_GITHUB_URL", gh.URL)
	t.Setenv("TRIAGE_FACTORY_GITHUB_BOT_PAT", "bot-token")
	t.Setenv(envGitHubUserPAT, "user-token")
	t.Setenv(envRepos, "acme/api,acme/web")

	s := newTestServer(t)
	ctx := t.Context()
	clearLocalProvisioningSeed(t, s)

	if err := s.RunHeadlessBootstrap(ctx); err != nil {
		t.Fatalf("RunHeadlessBootstrap: %v", err)
	}

	// Tenant provisioned.
	if org, err := s.orgs.GetOrgSystem(ctx, runmode.LocalDefaultOrgID); err != nil || org == nil {
		t.Fatalf("expected tenant provisioned, org=%v err=%v", org, err)
	}

	// Org settings: GitHub base URL set, clone protocol pinned to https.
	orgSet, err := s.orgs.GetSettingsSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	if orgSet.GitHubBaseURL != gh.URL {
		t.Errorf("GitHubBaseURL = %q, want %q", orgSet.GitHubBaseURL, gh.URL)
	}
	if orgSet.GitHubCloneProtocol != "https" {
		t.Errorf("GitHubCloneProtocol = %q, want https", orgSet.GitHubCloneProtocol)
	}

	// Repos tracked.
	repos, err := s.allStores.TeamGitHubRepos.ListForTeamSystem(ctx, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("want 2 tracked repos, got %d (%v)", len(repos), repos)
	}

	// GitHub identity bound from the user PAT.
	ghWeb, _ := resolveGitHubHost(gh.URL)
	login, err := s.users.GetGitHubLogin(ctx, runmode.LocalDefaultUserID, ghWeb)
	if err != nil {
		t.Fatalf("GetGitHubLogin: %v", err)
	}
	if login != "octocat" {
		t.Errorf("github identity login = %q, want octocat", login)
	}

	// Idempotent re-run: already provisioned → no error, no change.
	if err := s.RunHeadlessBootstrap(ctx); err != nil {
		t.Fatalf("re-run RunHeadlessBootstrap: %v", err)
	}

	// Never-overwrite: simulate a UI edit (drop to one repo), re-run, and
	// confirm the env set is NOT re-applied over the operator's change.
	if err := s.allStores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, []domain.TeamGitHubRepo{{Owner: "acme", Repo: "api"}}); err != nil {
		t.Fatalf("simulate UI edit: %v", err)
	}
	if err := s.RunHeadlessBootstrap(ctx); err != nil {
		t.Fatalf("re-run after edit: %v", err)
	}
	repos, err = s.allStores.TeamGitHubRepos.ListForTeamSystem(ctx, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem after edit: %v", err)
	}
	if len(repos) != 1 {
		t.Errorf("never-overwrite violated: want 1 repo after UI edit + re-run, got %d", len(repos))
	}
}

// TestRunHeadlessBootstrap_CloneProtocolSSHOverride verifies that
// TRIAGE_FACTORY_CLONE_PROTOCOL=ssh makes the headless seed pin ssh instead of
// the https default, for an operator whose headless box has an SSH agent.
func TestRunHeadlessBootstrap_CloneProtocolSSHOverride(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	auth.ResetSecretBackendForTest(t)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/user" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	t.Cleanup(gh.Close)

	t.Setenv(envHeadless, "1")
	t.Setenv("TRIAGE_FACTORY_GITHUB_URL", gh.URL)
	t.Setenv("TRIAGE_FACTORY_GITHUB_BOT_PAT", "bot-token")
	t.Setenv(envGitHubUserPAT, "user-token")
	t.Setenv(envRepos, "acme/api")
	t.Setenv(envCloneProtocol, "ssh")

	s := newTestServer(t)
	ctx := t.Context()
	clearLocalProvisioningSeed(t, s)

	if err := s.RunHeadlessBootstrap(ctx); err != nil {
		t.Fatalf("RunHeadlessBootstrap: %v", err)
	}
	orgSet, err := s.orgs.GetSettingsSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	if orgSet.GitHubCloneProtocol != "ssh" {
		t.Errorf("GitHubCloneProtocol = %q, want ssh", orgSet.GitHubCloneProtocol)
	}
}

// TestRunHeadlessBootstrap_UnsetIdentityPATsWarn verifies that when the access
// side is fully configured but the per-user identity PATs are omitted, the
// bootstrap still provisions repos + Jira config yet leaves both identities
// unbound — and logs a WARN for each, so a half-configured headless deploy
// stuck at the Connect gate is diagnosable. The Jira bot credential isn't
// network-validated, so no Jira stub is needed.
func TestRunHeadlessBootstrap_UnsetIdentityPATsWarn(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	auth.ResetSecretBackendForTest(t)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/user" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	t.Cleanup(gh.Close)

	// Access fully configured (GitHub + Jira DC), identity PATs deliberately omitted.
	t.Setenv(envHeadless, "1")
	t.Setenv("TRIAGE_FACTORY_GITHUB_URL", gh.URL)
	t.Setenv("TRIAGE_FACTORY_GITHUB_BOT_PAT", "bot-token")
	t.Setenv(envRepos, "acme/api")
	t.Setenv("TRIAGE_FACTORY_JIRA_URL", "https://jira.example.com")
	t.Setenv("TRIAGE_FACTORY_JIRA_BOT_PAT", "jira-bot")
	t.Setenv(envJiraProjects, "SKY")
	t.Setenv(envJiraPickupStatuses, "To Do")
	t.Setenv(envJiraInProgressStatus, "In Progress")
	t.Setenv(envJiraDoneStatus, "Done")

	var logs bytes.Buffer
	restore := logging.SetOutput(&logs)
	t.Cleanup(restore)

	s := newTestServer(t)
	ctx := t.Context()
	clearLocalProvisioningSeed(t, s)

	if err := s.RunHeadlessBootstrap(ctx); err != nil {
		t.Fatalf("RunHeadlessBootstrap: %v", err)
	}

	// Access config still seeded.
	repos, err := s.allStores.TeamGitHubRepos.ListForTeamSystem(ctx, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if len(repos) != 1 {
		t.Errorf("want repos seeded despite missing identity PAT, got %d", len(repos))
	}
	rules, err := s.allStores.JiraStatusRules.ListForTeamSystem(ctx, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("JiraStatusRules.ListForTeamSystem: %v", err)
	}
	if len(rules) != 1 {
		t.Errorf("want Jira rules seeded despite missing identity PAT, got %d", len(rules))
	}

	// Identities left unbound.
	ghWeb, _ := resolveGitHubHost(gh.URL)
	if login, _ := s.users.GetGitHubLogin(ctx, runmode.LocalDefaultUserID, ghWeb); login != "" {
		t.Errorf("GitHub identity should be unbound, got %q", login)
	}
	jiraHost, _ := resolveJiraHost("https://jira.example.com")
	if acct, _, _ := s.users.GetJiraIdentity(ctx, runmode.LocalDefaultUserID, jiraHost); acct != "" {
		t.Errorf("Jira identity should be unbound, got %q", acct)
	}

	// Each missing identity PAT warned.
	out := logs.String()
	if !strings.Contains(out, "TRIAGE_FACTORY_GITHUB_USER_PAT is unset") {
		t.Errorf("expected a WARN about the unset GitHub identity PAT; logs:\n%s", out)
	}
	if !strings.Contains(out, "TRIAGE_FACTORY_JIRA_USER_PAT is unset") {
		t.Errorf("expected a WARN about the unset Jira identity PAT; logs:\n%s", out)
	}
}

// TestRunHeadlessBootstrap_JiraBotCredsWithoutConfigWarns covers the case where
// the Jira bot credentials are set but the project/status config is missing:
// GitHub still provisions, Jira is skipped, and the operator gets an
// incomplete-config WARN rather than silently discovering an empty Jira page.
func TestRunHeadlessBootstrap_JiraBotCredsWithoutConfigWarns(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	auth.ResetSecretBackendForTest(t)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/user" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	t.Cleanup(gh.Close)

	t.Setenv(envHeadless, "1")
	t.Setenv("TRIAGE_FACTORY_GITHUB_URL", gh.URL)
	t.Setenv("TRIAGE_FACTORY_GITHUB_BOT_PAT", "bot-token")
	t.Setenv(envRepos, "acme/api")
	// Jira bot creds present, but no project/status config.
	t.Setenv("TRIAGE_FACTORY_JIRA_URL", "https://jira.example.com")
	t.Setenv("TRIAGE_FACTORY_JIRA_BOT_PAT", "jira-bot")

	var logs bytes.Buffer
	restore := logging.SetOutput(&logs)
	t.Cleanup(restore)

	s := newTestServer(t)
	ctx := t.Context()
	clearLocalProvisioningSeed(t, s)

	if err := s.RunHeadlessBootstrap(ctx); err != nil {
		t.Fatalf("RunHeadlessBootstrap: %v", err)
	}

	repos, err := s.allStores.TeamGitHubRepos.ListForTeamSystem(ctx, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if len(repos) != 1 {
		t.Errorf("GitHub should still provision, got %d repos", len(repos))
	}
	rules, err := s.allStores.JiraStatusRules.ListForTeamSystem(ctx, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("JiraStatusRules.ListForTeamSystem: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("Jira should be skipped without project config, got %d rules", len(rules))
	}
	if !strings.Contains(logs.String(), "Jira config is incomplete") {
		t.Errorf("expected an incomplete-Jira WARN when bot creds are set without config; logs:\n%s", logs.String())
	}
}

// clearLocalProvisioningSeed drops the agent rows newTestServer pre-seeds, so
// the org no longer reads as fully provisioned and a test can exercise a real
// fresh provision. Raw SQL because agents/team_agents are bootstrap-only (no
// delete store method) — kept in one place so a future table rename is a single
// fix rather than silent no-ops scattered across tests.
func clearLocalProvisioningSeed(t *testing.T, s *Server) {
	t.Helper()
	for _, table := range []string{"team_agents", "agents"} {
		if _, err := s.db.Exec("DELETE FROM " + table); err != nil {
			t.Fatalf("clear %s seed: %v", table, err)
		}
	}
}
