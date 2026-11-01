package server

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// TestSecretKeyLiteralsMatchIntegrations: integrations cannot import server, so
// it hand-copies the secret keys the local uninstall sweep needs; a rename on
// either side would leave the real secret in the keychain with no compile error.
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
	if integrations.KeyLinearOAuthClientSecret != linearOAuthClientSecretKey {
		t.Errorf("linear oauth secret drift: integrations.KeyLinearOAuthClientSecret=%q, server.linearOAuthClientSecretKey=%q\n"+
			"the uninstall sweep would miss the real linear_oauth_client_secret keychain entry",
			integrations.KeyLinearOAuthClientSecret, linearOAuthClientSecretKey)
	}
}
