//go:build linux

package agentproc

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/cell"
	"github.com/sky-ai-eng/triage-factory/internal/egressrelay"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/llmproxy"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestSandboxEnvAllowlistCoversEveryProducer is the drift guard for the
// broker's env allowlist (sandbox.allowedSandboxEnvKeys). Under privilege
// separation the broker rejects the whole launch if any sandbox env key is
// not on that allowlist, so a new key added to one of the producers here —
// the jail's own environment (cell.JailEnv), the egress proxy, the LLM proxy
// (every provider variant), the git-config block, the push-capture hook, or
// the delegate/resume run-metadata set — would silently break every
// privsep-on run. This test exercises the real producers and asserts
// sandbox.EnvKeyAllowed accepts every key they emit, so such a drift fails
// here instead of in production.
func TestSandboxEnvAllowlistCoversEveryProducer(t *testing.T) {
	var keys []string
	add := func(env []string) {
		for _, kv := range env {
			if i := strings.IndexByte(kv, '='); i >= 0 {
				keys = append(keys, kv[:i])
			} else {
				keys = append(keys, kv)
			}
		}
	}

	// The jail's own environment, with every optional input present: the
	// floor, the git-hooks bin, the CA bundle's trust variables and the
	// real-gh channel (GH_HOST / GH_ENTERPRISE_TOKEN and the two suppressors).
	add(cell.JailEnv(cell.LaunchSpec{GHChannel: &cell.GHChannel{Host: "10.42.1.1:8443", Token: "run-token"}}))

	// Egress proxy routing.
	add(sandboxEgressProxyEnv("10.42.1.1:9000", "10.42.1.1", "run-token"))

	// Fetch-relay routing (GOPROXY today). Iterating the catalog makes
	// this guard automatic for future entries: a contributor adding a
	// relay whose env key is missing from the broker allowlist fails HERE,
	// with the key's name, instead of having every privsep-on run rejected
	// at launch.
	for _, entry := range egressrelay.Catalog() {
		add(entry.Env("10.42.1.1:9001"))
	}

	// LLM proxy placeholders — every provider variant, since each emits a
	// different key set (Anthropic vs. Bedrock bearer vs. Bedrock SigV4).
	for _, kind := range []llmproxy.Provider{
		llmproxy.ProviderAnthropic,
		llmproxy.ProviderBedrockBearer,
		llmproxy.ProviderBedrockSigV4,
	} {
		cfg := sandboxProxyConfig{providerKind: kind}
		if kind == llmproxy.ProviderBedrockSigV4 {
			cfg.llm.SigV4Credentials = &llmproxy.SigV4Credentials{Region: "us-east-1"}
		}
		add(buildSandboxProxyEnv(cfg, "http://10.42.1.1:9000", "run-token"))
	}

	// Git routing config (indexed GIT_CONFIG_* form) + the push-capture hook.
	add(encodeGitConfigEnv([][2]string{
		{"core.hooksPath", "/hooks"},
		{"url.http://p/.insteadOf", "https://github.com/"},
	}))
	add([]string{githooks.PushCaptureEnvVar + "=" + githooks.PushCaptureProxy})

	// Run-scoped metadata set by delegate/run.go + delegate/resume.go as
	// ExtraEnv. Those live in a package agentproc's tests can't import
	// (delegate imports agentproc), so the literals are mirrored here; a
	// rename there without updating the allowlist trips this test.
	add([]string{
		"TRIAGE_FACTORY_CONVERSATION_ID=x",
		"TRIAGE_FACTORY_CONVERSATION_ROOT=/work",
		"TRIAGE_FACTORY_WORKSPACE_KEY=x",
		"TRIAGE_FACTORY_GIT_COAUTHOR_TRAILER=Co-authored-by: X <x@y>",
	})

	for _, k := range keys {
		if !sandbox.EnvKeyAllowed(k) {
			t.Errorf("sandbox env key %q is produced but NOT on sandbox.allowedSandboxEnvKeys — "+
				"a privsep-on run would be rejected by the broker; add it to the allowlist", k)
		}
	}
}
