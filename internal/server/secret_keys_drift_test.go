package server

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// TestSecretKeyLiteralsMatchIntegrations pins the cross-package secret-key
// literals the local uninstall sweep (integrations.AllLocalSweepKeys) depends
// on. integrations can't import server (server imports integrations — cycle), so
// these keys are hand-copied into integrations with "keep in sync" comments. A
// silent rename on either side would make `triagefactory uninstall` sweep the
// wrong key and leave the real secret in the keychain, with no compile error.
// server sees both its own unexported write-path constants and the integrations
// exports, so it can hold the two together — the same drift-guard discipline
// internal/jira/keys_drift_test.go uses for the Jira keys.
func TestSecretKeyLiteralsMatchIntegrations(t *testing.T) {
	if integrations.KeyAnthropicAPIKey != secretKeyAnthropicAPIKey {
		t.Errorf("anthropic key drift: integrations.KeyAnthropicAPIKey=%q, server.secretKeyAnthropicAPIKey=%q\n"+
			"the uninstall sweep would miss the real anthropic_api_key keychain entry",
			integrations.KeyAnthropicAPIKey, secretKeyAnthropicAPIKey)
	}
	if integrations.KeyJiraOAuthClientSecret != jiraOAuthClientSecretKey {
		t.Errorf("jira oauth secret drift: integrations.KeyJiraOAuthClientSecret=%q, server.jiraOAuthClientSecretKey=%q\n"+
			"the uninstall sweep would miss the real jira_oauth_client_secret keychain entry",
			integrations.KeyJiraOAuthClientSecret, jiraOAuthClientSecretKey)
	}
}
