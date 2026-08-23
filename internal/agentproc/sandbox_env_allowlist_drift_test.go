//go:build linux

package agentproc

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/egressrelay"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/llmproxy"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestSandboxEnvAllowlistCoversEveryProducer is the drift guard for the
// broker's env allowlist (sandbox.allowedSandboxEnvKeys). Under privilege
// separation the broker rejects the whole launch if any sandbox env key is
// not on that allowlist, so a new key added to one of the producers here —
// the base env, the egress proxy, the LLM proxy (every provider variant),
// the git-config block, the push-capture hook, or the delegate/resume
// run-metadata set — would silently break every privsep-on run. This test
// exercises the real producers and asserts sandbox.EnvKeyAllowed accepts
// every key they emit, so such a drift fails here instead of in production.
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

	// Base runtime floor (+ the run-metadata keys the base is appended with
	// in LaunchToolHost). agentRuntimeEnv is folded in by buildSandboxEnv.
	add(buildSandboxEnv(nil))
	add([]string{githooks.BinEnvVar + "=/usr/local/bin/triagefactory"})

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

	// Real-gh channel env (GH_HOST / GH_ENTERPRISE_TOKEN / SSL_CERT_FILE + the
	// two suppressors). A new key here without an allowlist entry would break
	// every gh-channel run at launch validation.
	add(ghChannelEnv(&GHChannelParams{Host: "10.42.1.1:8443", Token: "run-token"}))

	// Run-scoped metadata set by delegate/run.go + delegate/resume.go as
	// ExtraEnv. Those live in a package agentproc's tests can't import
	// (delegate imports agentproc), so the literals are mirrored here; a
	// rename there without updating the allowlist trips this test.
	add([]string{
		"TRIAGE_FACTORY_CONVERSATION_ID=x",
		"TRIAGE_FACTORY_CONVERSATION_ROOT=/work",
		"TRIAGE_FACTORY_BLUEPRINT_RUN_ID=x",
		"TRIAGE_FACTORY_REPO=o/r",
		"TRIAGE_FACTORY_GIT_COAUTHOR_TRAILER=Co-authored-by: X <x@y>",
	})

	for _, k := range keys {
		if !sandbox.EnvKeyAllowed(k) {
			t.Errorf("sandbox env key %q is produced but NOT on sandbox.allowedSandboxEnvKeys — "+
				"a privsep-on run would be rejected by the broker; add it to the allowlist", k)
		}
	}
}
