package integrations_test

import (
	"context"
	"database/sql"
	"slices"
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
	org := runmode.LocalDefaultOrgID

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
	org := runmode.LocalDefaultOrgID

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
	org := runmode.LocalDefaultOrgID

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
	org := runmode.LocalDefaultOrgID

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
	org := runmode.LocalDefaultOrgID

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
	org := runmode.LocalDefaultOrgID

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

// TestClearJiraOtherScheme pins the scheme-switch cleanup: storing one Jira auth
// scheme then clearing the other removes only the unused scheme's secrets (and
// leaves the URL + marker + in-use scheme intact), so a DC↔Cloud switch can't
// leave a stale credential behind.
func TestClearJiraOtherScheme(t *testing.T) {
	t.Run("cloud in use clears the DC PAT", func(t *testing.T) {
		stores := openStores(t)
		ctx := context.Background()
		org := runmode.LocalDefaultOrgID

		// Org was DC, now reconnected as Cloud: both schemes' secrets present.
		if err := integrations.Save(ctx, stores.Secrets, org, auth.Credentials{
			JiraURL: "https://acme.atlassian.net", JiraPAT: "stale-dc-pat",
			JiraEmail: "bot@acme.com", JiraAPIToken: "cloud-token",
			JiraAuthMethod: "cloud_api_token",
		}); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := integrations.ClearJiraOtherScheme(ctx, stores.Secrets, org, jira.AuthMethodCloudAPIToken); err != nil {
			t.Fatalf("ClearJiraOtherScheme: %v", err)
		}
		got, err := integrations.Load(ctx, stores.Secrets, org)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.JiraPAT != "" {
			t.Errorf("stale DC PAT survived a Cloud switch: %q", got.JiraPAT)
		}
		if got.JiraEmail != "bot@acme.com" || got.JiraAPIToken != "cloud-token" || got.JiraURL == "" {
			t.Errorf("Cloud scheme / URL wrongly cleared: %+v", got)
		}
	})

	t.Run("dc in use clears the Cloud pair", func(t *testing.T) {
		stores := openStores(t)
		ctx := context.Background()
		org := runmode.LocalDefaultOrgID

		if err := integrations.Save(ctx, stores.Secrets, org, auth.Credentials{
			JiraURL: "https://jira.corp.example", JiraPAT: "dc-pat",
			JiraEmail: "stale@acme.com", JiraAPIToken: "stale-cloud-token",
			JiraAuthMethod: "dc_pat",
		}); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := integrations.ClearJiraOtherScheme(ctx, stores.Secrets, org, jira.AuthMethodDCPAT); err != nil {
			t.Fatalf("ClearJiraOtherScheme: %v", err)
		}
		got, err := integrations.Load(ctx, stores.Secrets, org)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.JiraEmail != "" || got.JiraAPIToken != "" {
			t.Errorf("stale Cloud pair survived a DC switch: %+v", got)
		}
		if got.JiraPAT != "dc-pat" || got.JiraURL == "" {
			t.Errorf("DC scheme / URL wrongly cleared: %+v", got)
		}
	})
}

func TestClear_WipesEverything(t *testing.T) {
	stores := openStores(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

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
	org := runmode.LocalDefaultOrgID

	if err := integrations.Save(ctx, stores.Secrets, org, auth.Credentials{
		GitHubURL: "https://kept-keychain.example.com",
		GitHubPAT: "keychain-pat",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv("TRIAGE_FACTORY_GITHUB_BOT_PAT", "env-overrides")
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

// TestLoadSystem_Roundtrip pins that LoadSystem reads back what Save wrote and
// returns the same bundle Load does in local mode (GetSystem → keychain, same
// as Get) — the "local mode unchanged" half of the multi-mode poller fix.
func TestLoadSystem_Roundtrip(t *testing.T) {
	stores := openStores(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	want := auth.Credentials{
		GitHubURL:      "https://github.example.com",
		GitHubPAT:      "ghp-test",
		JiraURL:        "https://acme.atlassian.net",
		JiraEmail:      "bot@acme.com",
		JiraAPIToken:   "cloud-token",
		JiraAuthMethod: "cloud_api_token",
	}
	if err := integrations.Save(ctx, stores.Secrets, org, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	gotSystem, err := integrations.LoadSystem(ctx, stores.Secrets, org)
	if err != nil {
		t.Fatalf("LoadSystem: %v", err)
	}
	if gotSystem != want {
		t.Errorf("LoadSystem got=%+v want=%+v", gotSystem, want)
	}
	gotLoad, err := integrations.Load(ctx, stores.Secrets, org)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotSystem != gotLoad {
		t.Errorf("LoadSystem and Load disagree in local mode:\n  LoadSystem=%+v\n  Load      =%+v", gotSystem, gotLoad)
	}
}

// recordingSecrets records which credential keys are read through the
// claims-checked Get door vs the claims-free GetSystem door, so a test can
// prove Load and LoadSystem read the same key set and that LoadSystem never
// touches Get. The embedded db.SecretStore is nil and never dereferenced: Load
// calls only Get, LoadSystem only GetSystem, both overridden below.
type recordingSecrets struct {
	db.SecretStore
	viaGet       []string
	viaGetSystem []string
}

func (r *recordingSecrets) Get(_ context.Context, _ string, key string) (string, error) {
	r.viaGet = append(r.viaGet, key)
	return "", nil
}

func (r *recordingSecrets) GetSystem(_ context.Context, _ string, key string) (string, error) {
	r.viaGetSystem = append(r.viaGetSystem, key)
	return "", nil
}

// TestLoadSystem_SameKeySetAsLoad pins that LoadSystem reads exactly the keys
// Load reads — but through the claims-free GetSystem door, never the
// claims-checked Get. If a new credential key lands in one and not the other the
// sets diverge and this fails; if LoadSystem ever regresses to Get its GetSystem
// set goes empty and this fails. The background/poller twin of the resolver's
// keys-drift guard, and the guard behind the poll path's "no claims-scoped Get"
// invariant.
func TestLoadSystem_SameKeySetAsLoad(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	rec := &recordingSecrets{}
	if _, err := integrations.Load(ctx, rec, org); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := integrations.LoadSystem(ctx, rec, org); err != nil {
		t.Fatalf("LoadSystem: %v", err)
	}

	if len(rec.viaGet) == 0 {
		t.Fatal("Load read no keys via Get — stub wired wrong")
	}
	if len(rec.viaGetSystem) == 0 {
		t.Fatal("LoadSystem read no keys via the claims-free GetSystem door")
	}
	loadKeys := slices.Sorted(slices.Values(rec.viaGet))
	systemKeys := slices.Sorted(slices.Values(rec.viaGetSystem))
	if !slices.Equal(systemKeys, loadKeys) {
		t.Errorf("LoadSystem key set drifted from Load:\n  LoadSystem (GetSystem) = %v\n  Load       (Get)       = %v", systemKeys, loadKeys)
	}
}

// TestAllKeys_IncludesLegacyKeychainKeys pins that the per-org Clear set still
// sweeps the two legacy keychain keys (github_username and
// jira_display_name). They have no companion DB ref, so they're
// safe on the Clear path. Keeping them in AllKeys also carries them into
// AllLocalSweepKeys (the superset scripts/clean-slate.sh mirrors), which lists
// both — so the script and the Go source can't drift on the legacy keys.
func TestAllKeys_IncludesLegacyKeychainKeys(t *testing.T) {
	for _, k := range []string{"github_username", "jira_display_name"} {
		if !slices.Contains(integrations.AllKeys(), k) {
			t.Errorf("AllKeys missing legacy keychain key %q (clean-slate.sh sweeps it via AllLocalSweepKeys; they must stay in sync)", k)
		}
	}
}

// TestStaticOrgSecretsExcludedFromAllKeys pins the TFAC-405 design decision: the
// org-level Anthropic / Atlassian-OAuth secrets must NOT ride the per-org Clear
// path. AllKeys feeds integrations.Clear ("Clear All Tokens"), which doesn't
// reconcile their companion DB refs (org_settings.anthropic_api_key_ref / the
// org_jira_apps row) — sweeping them there would dangle the ref. They belong
// only in AllLocalSweepKeys (the full uninstall wipe, where the DB is gone too).
func TestStaticOrgSecretsExcludedFromAllKeys(t *testing.T) {
	for _, k := range []string{integrations.KeyAnthropicAPIKey, integrations.KeyJiraOAuthClientSecret} {
		if slices.Contains(integrations.AllKeys(), k) {
			t.Errorf("%q must NOT be in AllKeys: it would ride the per-org Clear path, which can't reconcile its companion DB ref", k)
		}
	}
}

// TestAllLocalSweepKeys_IsAllKeysPlusStaticOrgSecrets pins that the uninstall
// sweep set is exactly AllKeys plus the org-level secrets (the Anthropic key,
// the Atlassian OAuth client secret, and the Bedrock set managed by
// POST /api/bedrock/connect) — a superset that covers everything Clear covers
// and nothing it can't reconcile.
func TestAllLocalSweepKeys_IsAllKeysPlusStaticOrgSecrets(t *testing.T) {
	sweep := integrations.AllLocalSweepKeys()
	for _, k := range integrations.AllKeys() {
		if !slices.Contains(sweep, k) {
			t.Errorf("AllLocalSweepKeys missing AllKeys entry %q (must be a superset)", k)
		}
	}
	orgSecrets := append([]string{integrations.KeyAnthropicAPIKey, integrations.KeyJiraOAuthClientSecret},
		integrations.BedrockKeys()...)
	for _, k := range orgSecrets {
		if !slices.Contains(sweep, k) {
			t.Errorf("AllLocalSweepKeys missing org-level secret %q", k)
		}
	}
	if got, want := len(sweep), len(integrations.AllKeys())+len(orgSecrets); got != want {
		t.Errorf("AllLocalSweepKeys has %d keys, want %d (AllKeys + the org secrets, no dupes)", got, want)
	}
}

// TestGitHubAppKeysFor_Format pins the per-App key shape. This is the single
// source both the server write paths and the uninstall sweep compose through, so
// pinning the format here pins it for every consumer at once
// (github_app_<id>_{pem,client_secret,webhook_secret}). All() returns them in
// PEM/client-secret/webhook order for the sweep.
func TestGitHubAppKeysFor_Format(t *testing.T) {
	ks := integrations.GitHubAppKeysFor("42")
	if ks.PEM != "github_app_42_pem" {
		t.Errorf("PEM = %q, want github_app_42_pem", ks.PEM)
	}
	if ks.ClientSecret != "github_app_42_client_secret" {
		t.Errorf("ClientSecret = %q, want github_app_42_client_secret", ks.ClientSecret)
	}
	if ks.WebhookSecret != "github_app_42_webhook_secret" {
		t.Errorf("WebhookSecret = %q, want github_app_42_webhook_secret", ks.WebhookSecret)
	}
	want := []string{"github_app_42_pem", "github_app_42_client_secret", "github_app_42_webhook_secret"}
	if got := ks.All(); !slices.Equal(got, want) {
		t.Errorf("All() = %v, want %v", got, want)
	}
}

// TestSlackWorkspaceKeysFor_Format pins the per-(workspace, app) key shape
// (slack_ws_<ws>_<app>_{bot_token,signing_secret,app_token}) — the single
// source ee/slack's connect and delete handlers compose through, so the
// names written and the names later removed can't drift.
func TestSlackWorkspaceKeysFor_Format(t *testing.T) {
	ks := integrations.SlackWorkspaceKeysFor("T0123ABCD", "A0987ZYXW")
	if ks.BotToken != "slack_ws_T0123ABCD_A0987ZYXW_bot_token" {
		t.Errorf("BotToken = %q, want slack_ws_T0123ABCD_A0987ZYXW_bot_token", ks.BotToken)
	}
	if ks.SigningSecret != "slack_ws_T0123ABCD_A0987ZYXW_signing_secret" {
		t.Errorf("SigningSecret = %q, want slack_ws_T0123ABCD_A0987ZYXW_signing_secret", ks.SigningSecret)
	}
	if ks.AppToken != "slack_ws_T0123ABCD_A0987ZYXW_app_token" {
		t.Errorf("AppToken = %q, want slack_ws_T0123ABCD_A0987ZYXW_app_token", ks.AppToken)
	}
	want := []string{
		"slack_ws_T0123ABCD_A0987ZYXW_bot_token",
		"slack_ws_T0123ABCD_A0987ZYXW_signing_secret",
		"slack_ws_T0123ABCD_A0987ZYXW_app_token",
	}
	if got := ks.All(); !slices.Equal(got, want) {
		t.Errorf("All() = %v, want %v", got, want)
	}
}
