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
