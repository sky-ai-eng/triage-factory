package agentproc

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// TestAnthropicKeyMatchesIntegrations pins agentproc's Anthropic read-path key
// literal against integrations.KeyAnthropicAPIKey, the canonical name the local
// uninstall sweep (integrations.AllLocalSweepKeys) removes. integrations can't
// import agentproc-adjacent packages without a cycle, so the literal is
// hand-copied with a "keep in sync" comment; agentproc can import integrations,
// so it holds the two together here. Combined with
// server.TestSecretKeyLiteralsMatchIntegrations (which pins the write-path key
// to the same export), this guarantees the read path, the write path, and the
// uninstall sweep all agree on "anthropic_api_key" — a silent rename on any one
// side fails a test instead of silently leaving the secret behind / breaking
// resolution, with no compile error.
func TestAnthropicKeyMatchesIntegrations(t *testing.T) {
	if integrations.KeyAnthropicAPIKey != secretAnthropicAPIKey {
		t.Errorf("anthropic key drift: integrations.KeyAnthropicAPIKey=%q, agentproc.secretAnthropicAPIKey=%q\n"+
			"the resolver would read a different key than the uninstall sweep removes",
			integrations.KeyAnthropicAPIKey, secretAnthropicAPIKey)
	}
}

// TestBedrockKeysMatchIntegrations extends the same drift discipline to
// the Bedrock secret set: the resolver's read-path literals (this
// package's catalog) must match the integrations exports that the
// Bedrock connect write path (internal/server) stores under and the
// local uninstall sweep removes. A silent rename on any side would
// leave a stored credential unresolvable — or worse, unswept.
func TestBedrockKeysMatchIntegrations(t *testing.T) {
	pairs := []struct {
		name         string
		integrations string
		agentproc    string
	}{
		{"aws_access_key_id", integrations.KeyAWSAccessKeyID, secretAWSAccessKeyID},
		{"aws_secret_access_key", integrations.KeyAWSSecretAccessKey, secretAWSSecretKey},
		{"aws_session_token", integrations.KeyAWSSessionToken, secretAWSSessionToken},
		{"aws_region", integrations.KeyAWSRegion, secretAWSRegion},
		{"aws_bearer_token_bedrock", integrations.KeyAWSBearerTokenBedrock, secretAWSBearerTokenBedrock},
		{"bedrock_model_id", integrations.KeyBedrockModelID, secretBedrockModelID},
		{"bedrock_base_url", integrations.KeyBedrockBaseURL, secretBedrockBaseURL},
		{"aws_role_arn", integrations.KeyAWSRoleARN, secretAWSRoleARN},
	}
	for _, p := range pairs {
		if p.integrations != p.agentproc {
			t.Errorf("%s drift: integrations=%q, agentproc=%q", p.name, p.integrations, p.agentproc)
		}
	}
	// Every managed Bedrock key must be in the uninstall sweep — the
	// connect endpoint is a local-mode write path for all of them.
	sweep := map[string]bool{}
	for _, k := range integrations.AllLocalSweepKeys() {
		sweep[k] = true
	}
	for _, k := range integrations.BedrockKeys() {
		if !sweep[k] {
			t.Errorf("bedrock key %q missing from AllLocalSweepKeys; uninstall would leave it in the keychain", k)
		}
	}
}
