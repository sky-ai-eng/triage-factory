package auth

import (
	"slices"
	"testing"
)

// TestEnvProvided_Jira pins that either complete Jira credential counts as an
// env-supplied Jira: the Data Center PAT, or the Cloud email + API token. Half
// a Cloud pair, or a credential with no host, is not a configuration.
func TestEnvProvided_Jira(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"nothing", nil, false},
		{"url only", map[string]string{"TRIAGE_FACTORY_JIRA_URL": "https://jira.example.com"}, false},
		{"data center", map[string]string{
			"TRIAGE_FACTORY_JIRA_URL":     "https://jira.example.com",
			"TRIAGE_FACTORY_JIRA_BOT_PAT": "dc-pat",
		}, true},
		{"cloud", map[string]string{
			"TRIAGE_FACTORY_JIRA_URL":       "https://acme.atlassian.net",
			"TRIAGE_FACTORY_JIRA_EMAIL":     "bot@acme.example",
			"TRIAGE_FACTORY_JIRA_API_TOKEN": "cloud-token",
		}, true},
		{"cloud without email", map[string]string{
			"TRIAGE_FACTORY_JIRA_URL":       "https://acme.atlassian.net",
			"TRIAGE_FACTORY_JIRA_API_TOKEN": "cloud-token",
		}, false},
		{"cloud without url", map[string]string{
			"TRIAGE_FACTORY_JIRA_EMAIL":     "bot@acme.example",
			"TRIAGE_FACTORY_JIRA_API_TOKEN": "cloud-token",
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{
				"TRIAGE_FACTORY_JIRA_URL", "TRIAGE_FACTORY_JIRA_BOT_PAT",
				"TRIAGE_FACTORY_JIRA_EMAIL", "TRIAGE_FACTORY_JIRA_API_TOKEN",
			} {
				t.Setenv(name, tc.env[name])
			}
			if got := slices.Contains(EnvProvided(), "jira"); got != tc.want {
				t.Errorf("EnvProvided reports jira = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGetSecret_JiraCloudOverlay pins the key each Cloud var overlays, so a
// read of jira_email / jira_api_token returns the env value over a stored one.
func TestGetSecret_JiraCloudOverlay(t *testing.T) {
	useFileBackend(t, testKeyHex)
	for key, envName := range map[string]string{
		"jira_email":     "TRIAGE_FACTORY_JIRA_EMAIL",
		"jira_api_token": "TRIAGE_FACTORY_JIRA_API_TOKEN",
	} {
		if err := PutSecret(key, "stored"); err != nil {
			t.Fatalf("PutSecret(%s): %v", key, err)
		}
		t.Setenv(envName, "from-env")
		if got, err := GetSecret(key); err != nil || got != "from-env" {
			t.Errorf("GetSecret(%s) = (%q, %v), want the %s value", key, got, err, envName)
		}
		if !EnvProvidesKey(key) {
			t.Errorf("EnvProvidesKey(%s) = false with %s set", key, envName)
		}
	}
}
