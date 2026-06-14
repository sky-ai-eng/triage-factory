package integrations_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/zalando/go-keyring"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// openStores returns a SecretStore-backed Stores bundle against an
// in-memory keychain. Matches the pattern in
// internal/db/sqlite/secrets_test.go so the helper-level tests below
// exercise the same code path production handlers will see.
func openStores(t *testing.T) db.Stores {
	t.Helper()
	keyring.MockInit()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return sqlitestore.New(conn)
}

func TestLoadSave_Roundtrip(t *testing.T) {
	stores := openStores(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	want := auth.Credentials{
		GitHubURL: "https://github.example.com",
		GitHubPAT: "ghp-test",
		JiraURL:   "https://jira.example.com",
		JiraPAT:   "jira-test",
	}
	if err := integrations.Save(ctx, stores.Secrets, org, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := integrations.Load(ctx, stores.Secrets, org)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("Load got=%+v want=%+v", got, want)
	}
}

func TestSave_SkipsEmptyValues(t *testing.T) {
	stores := openStores(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	// Seed existing GitHub PAT then call Save with only Jira fields
	// populated. The GitHub PAT must survive — empty values mean
	// "leave alone."
	if err := integrations.Save(ctx, stores.Secrets, org, auth.Credentials{
		GitHubURL: "https://github.example.com",
		GitHubPAT: "ghp-original",
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := integrations.Save(ctx, stores.Secrets, org, auth.Credentials{
		JiraURL: "https://jira.example.com",
		JiraPAT: "jira-test",
	}); err != nil {
		t.Fatalf("Save jira-only: %v", err)
	}
	got, err := integrations.Load(ctx, stores.Secrets, org)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.GitHubPAT != "ghp-original" {
		t.Errorf("GitHub PAT got=%q want ghp-original (empty-string Save should not clear)", got.GitHubPAT)
	}
	if got.JiraPAT != "jira-test" {
		t.Errorf("Jira PAT got=%q want jira-test", got.JiraPAT)
	}
}

func TestClearGitHub_LeavesJira(t *testing.T) {
	stores := openStores(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	if err := integrations.Save(ctx, stores.Secrets, org, auth.Credentials{
		GitHubURL: "https://github.example.com",
		GitHubPAT: "ghp-test",
		JiraURL:   "https://jira.example.com",
		JiraPAT:   "jira-test",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := integrations.ClearGitHub(ctx, stores.Secrets, org); err != nil {
		t.Fatalf("ClearGitHub: %v", err)
	}
	got, err := integrations.Load(ctx, stores.Secrets, org)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.GitHubURL != "" || got.GitHubPAT != "" {
		t.Errorf("GitHub creds survived ClearGitHub: %+v", got)
	}
	if got.JiraURL == "" || got.JiraPAT == "" {
		t.Errorf("Jira creds disappeared after ClearGitHub: %+v", got)
	}
}

func TestClearJira_LeavesGitHub(t *testing.T) {
	stores := openStores(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	if err := integrations.Save(ctx, stores.Secrets, org, auth.Credentials{
		GitHubURL: "https://github.example.com",
		GitHubPAT: "ghp-test",
		JiraURL:   "https://jira.example.com",
		JiraPAT:   "jira-test",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := integrations.ClearJira(ctx, stores.Secrets, org); err != nil {
		t.Fatalf("ClearJira: %v", err)
	}
	got, err := integrations.Load(ctx, stores.Secrets, org)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.JiraURL != "" || got.JiraPAT != "" {
		t.Errorf("Jira creds survived ClearJira: %+v", got)
	}
	if got.GitHubURL == "" || got.GitHubPAT == "" {
		t.Errorf("GitHub creds disappeared after ClearJira: %+v", got)
	}
}

// TestLoadSave_CloudRoundtrip pins that the Cloud credential fields (email +
// API token + auth-method marker) round-trip through Save/Load alongside the
// URL, the same way the DC PAT does.
func TestLoadSave_CloudRoundtrip(t *testing.T) {
	stores := openStores(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	want := auth.Credentials{
		JiraURL:        "https://acme.atlassian.net",
		JiraEmail:      "bot@acme.com",
		JiraAPIToken:   "cloud-token",
		JiraAuthMethod: "cloud_api_token",
	}
	if err := integrations.Save(ctx, stores.Secrets, org, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := integrations.Load(ctx, stores.Secrets, org)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("Load got=%+v want=%+v", got, want)
	}
}

// TestClearJira_SweepsCloudKeys pins that ClearJira removes the Cloud halves
// (email, API token, marker) too — not just the URL + DC PAT — while leaving
// GitHub untouched, so a disconnect leaves no orphaned Cloud secret.
func TestClearJira_SweepsCloudKeys(t *testing.T) {
	stores := openStores(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	if err := integrations.Save(ctx, stores.Secrets, org, auth.Credentials{
		GitHubURL:      "https://github.example.com",
		GitHubPAT:      "ghp-test",
		JiraURL:        "https://acme.atlassian.net",
		JiraEmail:      "bot@acme.com",
		JiraAPIToken:   "cloud-token",
		JiraAuthMethod: "cloud_api_token",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := integrations.ClearJira(ctx, stores.Secrets, org); err != nil {
		t.Fatalf("ClearJira: %v", err)
	}
	got, err := integrations.Load(ctx, stores.Secrets, org)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.JiraURL != "" || got.JiraEmail != "" || got.JiraAPIToken != "" || got.JiraAuthMethod != "" {
		t.Errorf("Jira cloud creds survived ClearJira: %+v", got)
	}
	if got.GitHubURL == "" || got.GitHubPAT == "" {
		t.Errorf("GitHub creds disappeared after ClearJira: %+v", got)
	}
}

func TestClear_WipesEverything(t *testing.T) {
	stores := openStores(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	if err := integrations.Save(ctx, stores.Secrets, org, auth.Credentials{
		GitHubURL: "https://github.example.com",
		GitHubPAT: "ghp-test",
		JiraURL:   "https://jira.example.com",
		JiraPAT:   "jira-test",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := integrations.Clear(ctx, stores.Secrets, org); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	got, err := integrations.Load(ctx, stores.Secrets, org)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if (got != auth.Credentials{}) {
		t.Errorf("Clear left residue: %+v", got)
	}
}

// TestJiraSystemConfig pins the request-path creds→config builder: the marker
// routes Cloud (Basic, REST v3) vs DC (Bearer, REST v2); an empty marker falls
// back to host-shape detection; and ok=false when the URL or the scheme's
// credential half is missing. It's the request-path mirror of the resolver's
// ForSystem routing, so a Cloud org's status/stock reads build the right client.
func TestJiraSystemConfig(t *testing.T) {
	cases := []struct {
		name       string
		creds      auth.Credentials
		wantOK     bool
		wantDeploy jira.Deployment
		wantVer    jira.APIVersion
	}{
		{
			name: "cloud marker",
			creds: auth.Credentials{
				JiraURL: "https://acme.atlassian.net/", JiraEmail: "bot@acme.com",
				JiraAPIToken: "tok", JiraAuthMethod: "cloud_api_token",
			},
			wantOK: true, wantDeploy: jira.DeploymentCloud, wantVer: jira.APIv3,
		},
		{
			name: "dc marker",
			creds: auth.Credentials{
				JiraURL: "https://jira.corp.example", JiraPAT: "pat", JiraAuthMethod: "dc_pat",
			},
			wantOK: true, wantDeploy: jira.DeploymentDataCenter, wantVer: jira.APIv2,
		},
		{
			name: "empty marker, cloud host inferred",
			creds: auth.Credentials{
				JiraURL: "https://acme.atlassian.net", JiraEmail: "bot@acme.com", JiraAPIToken: "tok",
			},
			wantOK: true, wantDeploy: jira.DeploymentCloud, wantVer: jira.APIv3,
		},
		{
			name:       "empty marker, dc host inferred",
			creds:      auth.Credentials{JiraURL: "https://jira.corp.example", JiraPAT: "pat"},
			wantOK:     true,
			wantDeploy: jira.DeploymentDataCenter, wantVer: jira.APIv2,
		},
		{name: "no url", creds: auth.Credentials{JiraPAT: "pat"}, wantOK: false},
		{
			name: "cloud marker missing token",
			creds: auth.Credentials{
				JiraURL: "https://acme.atlassian.net", JiraEmail: "bot@acme.com", JiraAuthMethod: "cloud_api_token",
			},
			wantOK: false,
		},
		{name: "dc missing pat", creds: auth.Credentials{JiraURL: "https://jira.corp.example"}, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, ok := integrations.JiraSystemConfig(tc.creds)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if cfg.Deployment != tc.wantDeploy {
				t.Errorf("Deployment = %q, want %q", cfg.Deployment, tc.wantDeploy)
			}
			if cfg.APIVersion != tc.wantVer {
				t.Errorf("APIVersion = %q, want %q", cfg.APIVersion, tc.wantVer)
			}
		})
	}
}

func TestLoad_EnvOverlayWins(t *testing.T) {
	stores := openStores(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrg

	if err := integrations.Save(ctx, stores.Secrets, org, auth.Credentials{
		GitHubURL: "https://kept-keychain.example.com",
		GitHubPAT: "keychain-pat",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv("TRIAGE_FACTORY_GITHUB_PAT", "env-overrides")
	got, err := integrations.Load(ctx, stores.Secrets, org)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.GitHubPAT != "env-overrides" {
		t.Errorf("env overlay not honored: got=%q want=env-overrides", got.GitHubPAT)
	}
	if got.GitHubURL != "https://kept-keychain.example.com" {
		t.Errorf("non-env field changed: got=%q", got.GitHubURL)
	}
}
