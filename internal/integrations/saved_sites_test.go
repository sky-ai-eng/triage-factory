package integrations_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// savedJiraURLKey is the keychain key a local install kept its Jira site
// under, spelled out because the package holds it unexported: the fixture has
// to write exactly what an install stored.
const savedJiraURLKey = "jira_url"

func putSecret(t *testing.T, s db.SecretStore, key, value string) {
	t.Helper()
	if err := s.Put(context.Background(), runmode.LocalDefaultOrgID, key, value, ""); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func getSecret(t *testing.T, s db.SecretStore, key string) string {
	t.Helper()
	v, err := s.GetSystem(context.Background(), runmode.LocalDefaultOrgID, key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return v
}

func jiraSite(t *testing.T, stores db.Stores) string {
	t.Helper()
	set, err := stores.Orgs.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	return set.JiraBaseURL
}

func setJiraSite(t *testing.T, stores db.Stores, site string) {
	t.Helper()
	if _, err := stores.Orgs.SetSourceBaseURL(context.Background(), runmode.LocalDefaultOrgID, "jira", site); err != nil {
		t.Fatalf("set jira site: %v", err)
	}
}

func adopt(t *testing.T, stores db.Stores) {
	t.Helper()
	if err := integrations.AdoptSavedJiraSite(context.Background(), stores.Secrets, stores.Orgs,
		runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("AdoptSavedJiraSite: %v", err)
	}
}

// The site saved beside a stored credential becomes the org's Jira site, in
// canonical form, and the saved key is gone, so the service client now talks
// where it always did.
func TestAdoptSavedJiraSite_MovesTheSiteIntoSettings(t *testing.T) {
	stores := openStores(t)
	putSecret(t, stores.Secrets, savedJiraURLKey, " https://Jira.Example.com/ ")
	putSecret(t, stores.Secrets, integrations.KeyJiraPAT, "jira-pat")

	adopt(t, stores)

	if got := jiraSite(t, stores); got != "https://jira.example.com" {
		t.Errorf("jira site = %q, want https://jira.example.com", got)
	}
	if got := getSecret(t, stores.Secrets, savedJiraURLKey); got != "" {
		t.Errorf("saved jira site survived: %q", got)
	}
	if got := getSecret(t, stores.Secrets, integrations.KeyJiraPAT); got != "jira-pat" {
		t.Errorf("jira pat = %q, want it untouched", got)
	}
	// Once the key is gone, a second boot changes nothing.
	setJiraSite(t, stores, "https://other.example.com")
	adopt(t, stores)
	if got := jiraSite(t, stores); got != "https://other.example.com" {
		t.Errorf("a second boot moved the site to %q", got)
	}
}

// Where settings name another site, the credential's client was talking to the
// saved one, so the saved one wins.
func TestAdoptSavedJiraSite_TheCredentialsSiteWinsOverADifferentSetting(t *testing.T) {
	stores := openStores(t)
	setJiraSite(t, stores, "https://elsewhere.example.com")
	putSecret(t, stores.Secrets, savedJiraURLKey, "https://jira.example.com")
	putSecret(t, stores.Secrets, integrations.KeyJiraPAT, "jira-pat")

	adopt(t, stores)

	if got := jiraSite(t, stores); got != "https://jira.example.com" {
		t.Errorf("jira site = %q, want the credential's site", got)
	}
}

// With no credential stored, the saved site names nothing in use: it is
// deleted and the setting left alone.
func TestAdoptSavedJiraSite_WithoutACredentialOnlyDeletesTheKey(t *testing.T) {
	stores := openStores(t)
	setJiraSite(t, stores, "https://users-only.example.com")
	putSecret(t, stores.Secrets, savedJiraURLKey, "https://jira.example.com")

	adopt(t, stores)

	if got := jiraSite(t, stores); got != "https://users-only.example.com" {
		t.Errorf("jira site = %q, want the setting untouched", got)
	}
	if got := getSecret(t, stores.Secrets, savedJiraURLKey); got != "" {
		t.Errorf("saved jira site survived: %q", got)
	}
}

// A user's Jira token is keyed by the site's spelling when it was bound. A
// capitalised saved site moves the token to the canonical key; a token
// already under the canonical key wins.
func TestAdoptSavedJiraSite_MovesTheUsersTokenToTheCanonicalKey(t *testing.T) {
	ctx := context.Background()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	oldKey := jira.UserTokenKey("https://Jira.Example.com")
	newKey := jira.UserTokenKey("https://jira.example.com")

	for name, tc := range map[string]struct {
		existing, want string
	}{
		"moves":                 {want: "user-token"},
		"keeps the newer token": {existing: "newer-token", want: "newer-token"},
	} {
		t.Run(name, func(t *testing.T) {
			stores := openStores(t)
			putSecret(t, stores.Secrets, savedJiraURLKey, "https://Jira.Example.com/")
			putSecret(t, stores.Secrets, integrations.KeyJiraPAT, "jira-pat")
			if err := stores.Secrets.PutUserSystem(ctx, org, user, oldKey, "user-token", ""); err != nil {
				t.Fatal(err)
			}
			if tc.existing != "" {
				if err := stores.Secrets.PutUserSystem(ctx, org, user, newKey, tc.existing, ""); err != nil {
					t.Fatal(err)
				}
			}

			adopt(t, stores)

			if got, _ := stores.Secrets.GetUserSystem(ctx, org, user, newKey); got != tc.want {
				t.Errorf("token under the canonical key = %q, want %q", got, tc.want)
			}
			if got, _ := stores.Secrets.GetUserSystem(ctx, org, user, oldKey); got != "" {
				t.Errorf("token under the bound spelling survived: %q", got)
			}
		})
	}
}

// ClearJira sweeps the saved site too, so a disconnect on an install that never
// booted the adoption leaves no orphan keychain row.
func TestClearJira_SweepsTheSavedSite(t *testing.T) {
	stores := openStores(t)
	putSecret(t, stores.Secrets, savedJiraURLKey, "https://jira.example.com")
	if err := integrations.ClearJira(context.Background(), stores.Secrets, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("ClearJira: %v", err)
	}
	if got := getSecret(t, stores.Secrets, savedJiraURLKey); got != "" {
		t.Errorf("saved jira site survived ClearJira: %q", got)
	}
}

// savedGitHubURLKey is the keychain key a local install kept its GitHub host
// under beside the PAT, spelled out for the same reason as savedJiraURLKey.
const savedGitHubURLKey = "github_url"

func githubURL(t *testing.T, stores db.Stores) string {
	t.Helper()
	set, err := stores.Orgs.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	return set.GitHubBaseURL
}

func adoptGitHub(t *testing.T, stores db.Stores) {
	t.Helper()
	if err := integrations.AdoptSavedGitHubHost(context.Background(), stores, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("AdoptSavedGitHubHost: %v", err)
	}
}

// The URL saved beside the PAT becomes the org's GitHub URL when the setting
// is empty, in canonical form, and the saved key is gone, so a second boot
// changes nothing.
func TestAdoptSavedGitHubHost_MovesTheSavedURLIntoAnEmptySetting(t *testing.T) {
	stores := openStores(t)
	putSecret(t, stores.Secrets, savedGitHubURLKey, " https://GHE.Example.com/ ")
	putSecret(t, stores.Secrets, integrations.KeyGitHubPAT, "ghp-test")

	adoptGitHub(t, stores)

	if got := githubURL(t, stores); got != "https://ghe.example.com" {
		t.Errorf("github url = %q, want https://ghe.example.com", got)
	}
	if got := getSecret(t, stores.Secrets, savedGitHubURLKey); got != "" {
		t.Errorf("saved github url survived: %q", got)
	}
	if got := getSecret(t, stores.Secrets, integrations.KeyGitHubPAT); got != "ghp-test" {
		t.Errorf("github pat = %q, want it untouched", got)
	}
	if _, err := stores.Orgs.SetSourceBaseURL(context.Background(), runmode.LocalDefaultOrgID, "github", "https://other.example.com"); err != nil {
		t.Fatalf("set github url: %v", err)
	}
	adoptGitHub(t, stores)
	if got := githubURL(t, stores); got != "https://other.example.com" {
		t.Errorf("a second boot moved the github url to %q", got)
	}
}

// A set GitHub URL is what every client already talked to, so the saved one
// never replaces it; the saved key is still deleted.
func TestAdoptSavedGitHubHost_KeepsASetURL(t *testing.T) {
	stores := openStores(t)
	if _, err := stores.Orgs.SetSourceBaseURL(context.Background(), runmode.LocalDefaultOrgID, "github", "https://github.com"); err != nil {
		t.Fatalf("set github url: %v", err)
	}
	putSecret(t, stores.Secrets, savedGitHubURLKey, "https://ghe.example.com")

	adoptGitHub(t, stores)

	if got := githubURL(t, stores); got != "https://github.com" {
		t.Errorf("github url = %q, want the set https://github.com", got)
	}
	if got := getSecret(t, stores.Secrets, savedGitHubURLKey); got != "" {
		t.Errorf("saved github url survived: %q", got)
	}
}

// With nothing saved, an org already using GitHub was on the deployment's
// GitHub and is given it; an org using nothing GitHub is left without a URL.
func TestAdoptSavedGitHubHost_WithNothingSaved(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, stores db.Stores)
		want string
	}{
		{"a stored PAT", func(t *testing.T, stores db.Stores) {
			putSecret(t, stores.Secrets, integrations.KeyGitHubPAT, "ghp-test")
		}, "https://github.com"},
		{"repositories on the deployment's GitHub", func(t *testing.T, stores db.Stores) {
			if _, err := stores.Repos.Upsert(context.Background(), runmode.LocalDefaultOrgID, domain.Repository{
				Host: "https://github.com", Owner: "acme", Repo: "api", DefaultBranch: "main",
			}); err != nil {
				t.Fatalf("seed repository: %v", err)
			}
		}, "https://github.com"},
		{"nothing GitHub", func(*testing.T, db.Stores) {}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ghbase.SetDefaultBaseURLForTest(t, "")
			stores := openStores(t)
			tc.seed(t, stores)

			adoptGitHub(t, stores)

			if got := githubURL(t, stores); got != tc.want {
				t.Errorf("github url = %q, want %q", got, tc.want)
			}
		})
	}
}
