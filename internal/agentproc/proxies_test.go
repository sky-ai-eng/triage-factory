package agentproc

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/egressproxy"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/llmproxy"
)

// TestProxyConfigFromCreds_AnthropicDirect pins the resolver → proxy
// config mapping for the most common case: an org configured with
// just anthropic_api_key. Upstream defaults to api.anthropic.com.
func TestProxyConfigFromCreds_AnthropicDirect(t *testing.T) {
	creds := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-real-key",
	}
	cfg, err := proxyConfigFromCreds(creds)
	if err != nil {
		t.Fatalf("proxyConfigFromCreds: %v", err)
	}
	if cfg.llm.Provider != llmproxy.ProviderAnthropic {
		t.Errorf("Provider = %q, want anthropic", cfg.llm.Provider)
	}
	if cfg.llm.APIKey != "sk-ant-real-key" {
		t.Errorf("APIKey = %q; real key should flow into proxy config", cfg.llm.APIKey)
	}
	if cfg.llm.Upstream != "https://api.anthropic.com" {
		t.Errorf("Upstream = %q, want https://api.anthropic.com", cfg.llm.Upstream)
	}
	if !cfg.llm.AllowNonLoopback {
		t.Error("AllowNonLoopback = false; sandbox path must opt in (proxy binds on veth IP, not loopback)")
	}
}

// TestProxyConfigFromCreds_AnthropicWithGateway pins the org-gateway
// override path: an org with ANTHROPIC_BASE_URL set has the proxy
// forward to the gateway, not api.anthropic.com. The gateway's own
// auth flow takes it from there.
func TestProxyConfigFromCreds_AnthropicWithGateway(t *testing.T) {
	creds := map[string]string{
		"ANTHROPIC_API_KEY":  "sk-ant-real-key",
		"ANTHROPIC_BASE_URL": "https://gateway.example.com/",
	}
	cfg, err := proxyConfigFromCreds(creds)
	if err != nil {
		t.Fatalf("proxyConfigFromCreds: %v", err)
	}
	// Trailing slash should be stripped — the proxy upstream
	// validation rejects a path component.
	if cfg.llm.Upstream != "https://gateway.example.com" {
		t.Errorf("Upstream = %q, want https://gateway.example.com (trailing slash stripped)", cfg.llm.Upstream)
	}
}

// TestProxyConfigFromCreds_BedrockBearer pins the AWS Bedrock bearer
// path. Region threads through into the upstream URL; default to
// us-east-1 when not set (the SDK's own fallback).
func TestProxyConfigFromCreds_BedrockBearer(t *testing.T) {
	creds := map[string]string{
		"AWS_BEARER_TOKEN_BEDROCK": "bdrk-real-bearer",
		"AWS_REGION":               "us-west-2",
	}
	cfg, err := proxyConfigFromCreds(creds)
	if err != nil {
		t.Fatalf("proxyConfigFromCreds: %v", err)
	}
	if cfg.llm.Provider != llmproxy.ProviderBedrockBearer {
		t.Errorf("Provider = %q, want bedrock_bearer", cfg.llm.Provider)
	}
	if cfg.llm.APIKey != "bdrk-real-bearer" {
		t.Errorf("APIKey = %q, want bdrk-real-bearer", cfg.llm.APIKey)
	}
	if cfg.llm.Upstream != "https://bedrock-runtime.us-west-2.amazonaws.com" {
		t.Errorf("Upstream = %q, want regional Bedrock endpoint", cfg.llm.Upstream)
	}
}

// TestProxyConfigFromCreds_BedrockBearerRegionFallback pins the
// missing-region fallback. The resolver omits AWS_REGION when not
// configured; the proxy still needs *some* region for the upstream
// URL — defaults to us-east-1 (Bedrock's primary Anthropic region).
func TestProxyConfigFromCreds_BedrockBearerRegionFallback(t *testing.T) {
	creds := map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "bdrk"}
	cfg, err := proxyConfigFromCreds(creds)
	if err != nil {
		t.Fatalf("proxyConfigFromCreds: %v", err)
	}
	if cfg.llm.Upstream != "https://bedrock-runtime.us-east-1.amazonaws.com" {
		t.Errorf("Upstream = %q, want us-east-1 fallback", cfg.llm.Upstream)
	}
}

// TestProxyConfigFromCreds_AWSTripleSigV4 pins the Phase 2 mapping:
// the AWS access-key triple resolves to the SigV4 re-signing provider,
// with the full triple (including the optional session token) landing
// in SigV4Credentials and the region feeding both the upstream URL and
// the signing scope.
func TestProxyConfigFromCreds_AWSTripleSigV4(t *testing.T) {
	creds := map[string]string{
		"AWS_ACCESS_KEY_ID":       "AKIA-test",
		"AWS_SECRET_ACCESS_KEY":   "secret-test",
		"AWS_SESSION_TOKEN":       "session-test",
		"AWS_REGION":              "us-west-2",
		"CLAUDE_CODE_USE_BEDROCK": "1",
	}
	cfg, err := proxyConfigFromCreds(creds)
	if err != nil {
		t.Fatalf("proxyConfigFromCreds: %v", err)
	}
	if cfg.llm.Provider != llmproxy.ProviderBedrockSigV4 {
		t.Errorf("Provider = %q, want bedrock_sigv4", cfg.llm.Provider)
	}
	if cfg.llm.Upstream != "https://bedrock-runtime.us-west-2.amazonaws.com" {
		t.Errorf("Upstream = %q, want the regional Bedrock endpoint", cfg.llm.Upstream)
	}
	sc := cfg.llm.SigV4Credentials
	if sc == nil {
		t.Fatal("SigV4Credentials is nil; the real triple must flow into the proxy config")
	}
	if sc.AccessKeyID != "AKIA-test" || sc.SecretAccessKey != "secret-test" || sc.SessionToken != "session-test" {
		t.Errorf("SigV4Credentials = %+v, want the resolver's triple", sc)
	}
	if sc.Region != "us-west-2" {
		t.Errorf("SigV4Credentials.Region = %q, want us-west-2 (must match the endpoint region)", sc.Region)
	}
	if !cfg.llm.AllowNonLoopback {
		t.Error("AllowNonLoopback = false; sandbox path must opt in")
	}
}

// TestProxyConfigFromCreds_AWSTripleRegionFallback mirrors the bearer
// path's missing-region default: us-east-1 for both the endpoint and
// the signing scope.
func TestProxyConfigFromCreds_AWSTripleRegionFallback(t *testing.T) {
	cfg, err := proxyConfigFromCreds(map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIA-test",
		"AWS_SECRET_ACCESS_KEY": "secret-test",
	})
	if err != nil {
		t.Fatalf("proxyConfigFromCreds: %v", err)
	}
	if cfg.llm.Upstream != "https://bedrock-runtime.us-east-1.amazonaws.com" {
		t.Errorf("Upstream = %q, want us-east-1 fallback", cfg.llm.Upstream)
	}
	if got := cfg.llm.SigV4Credentials.Region; got != "us-east-1" {
		t.Errorf("SigV4Credentials.Region = %q, want us-east-1 fallback", got)
	}
	if got := cfg.llm.SigV4Credentials.SessionToken; got != "" {
		t.Errorf("SessionToken = %q, want empty when the resolver provided none", got)
	}
}

// TestProxyConfigFromCreds_BedrockEndpointOverride pins that both
// Bedrock paths honor the org's endpoint override (surfaced by the
// resolver as ANTHROPIC_BEDROCK_BASE_URL) as the proxy upstream — the
// VPC-endpoint / GovCloud / China-partition path, whose hostnames the
// regional formula can't produce. For SigV4 the signing scope must
// STILL come from aws_region, not the URL: a vpce hostname embeds a
// region a formula-parse couldn't recover, and AWS validates the
// scope against the endpoint's real region.
func TestProxyConfigFromCreds_BedrockEndpointOverride(t *testing.T) {
	const vpce = "https://vpce-0abc123-xyz.bedrock-runtime.us-gov-west-1.vpce.amazonaws.com"

	t.Run("sigv4", func(t *testing.T) {
		cfg, err := proxyConfigFromCreds(map[string]string{
			"AWS_ACCESS_KEY_ID":          "AKIA-test",
			"AWS_SECRET_ACCESS_KEY":      "secret-test",
			"AWS_REGION":                 "us-gov-west-1",
			"ANTHROPIC_BEDROCK_BASE_URL": vpce + "/", // trailing slash must be stripped
		})
		if err != nil {
			t.Fatalf("proxyConfigFromCreds: %v", err)
		}
		if cfg.llm.Upstream != vpce {
			t.Errorf("Upstream = %q, want the override %q (trailing slash stripped)", cfg.llm.Upstream, vpce)
		}
		if got := cfg.llm.SigV4Credentials.Region; got != "us-gov-west-1" {
			t.Errorf("SigV4Credentials.Region = %q, want us-gov-west-1 (scope comes from aws_region, never parsed from the URL)", got)
		}
	})

	t.Run("bearer", func(t *testing.T) {
		cfg, err := proxyConfigFromCreds(map[string]string{
			"AWS_BEARER_TOKEN_BEDROCK":   "bdrk-test",
			"ANTHROPIC_BEDROCK_BASE_URL": vpce,
		})
		if err != nil {
			t.Fatalf("proxyConfigFromCreds: %v", err)
		}
		if cfg.llm.Upstream != vpce {
			t.Errorf("Upstream = %q, want the override %q", cfg.llm.Upstream, vpce)
		}
	})
}

// TestProxyConfigFromCreds_RejectsMalformedBedrockEndpoint mirrors the
// Anthropic-gateway validation for the Bedrock override: a URL with a
// path / cleartext scheme / missing scheme fails at proxy-config time
// with an error that names the bedrock upstream, on both auth paths.
func TestProxyConfigFromCreds_RejectsMalformedBedrockEndpoint(t *testing.T) {
	for name, baseURL := range map[string]string{
		"with_path":         "https://vpce.example.amazonaws.com/bedrock",
		"http_non_loopback": "http://vpce.example.amazonaws.com",
		"missing_scheme":    "vpce.example.amazonaws.com",
	} {
		t.Run(name, func(t *testing.T) {
			for _, creds := range []map[string]string{
				{
					"AWS_ACCESS_KEY_ID":          "AKIA-test",
					"AWS_SECRET_ACCESS_KEY":      "secret-test",
					"ANTHROPIC_BEDROCK_BASE_URL": baseURL,
				},
				{
					"AWS_BEARER_TOKEN_BEDROCK":   "bdrk-test",
					"ANTHROPIC_BEDROCK_BASE_URL": baseURL,
				},
			} {
				_, err := proxyConfigFromCreds(creds)
				if err == nil {
					t.Fatalf("proxyConfigFromCreds accepted %q; want validation error", baseURL)
				}
				if !strings.Contains(err.Error(), "bedrock") {
					t.Errorf("err = %v; want it to name the bedrock upstream", err)
				}
			}
		})
	}
}

// TestProxyConfigFromCreds_PartialAWSPair_Unsupported pins the
// defensive rejection of a half-configured pair. The resolver never
// emits one half without the other, so this only fires on a malformed
// caller-built map — but it must fail typed rather than half-signing.
func TestProxyConfigFromCreds_PartialAWSPair_Unsupported(t *testing.T) {
	for name, creds := range map[string]map[string]string{
		"access_key_only": {"AWS_ACCESS_KEY_ID": "AKIA-test"},
		"secret_only":     {"AWS_SECRET_ACCESS_KEY": "secret-test"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := proxyConfigFromCreds(creds)
			if !errors.Is(err, ErrUnsupportedSandboxCredentials) {
				t.Fatalf("err = %v, want ErrUnsupportedSandboxCredentials wrap", err)
			}
		})
	}
}

// TestProxyConfigFromCreds_EmptyCreds_Unsupported is the "resolver
// returned an empty map" guard. In multi mode the resolver itself
// returns ErrNoCredentialsConfigured for unconfigured orgs, so this
// branch is mostly defensive — but it pins the contract: if we get
// to proxy config with no creds, we error rather than silently
// starting a useless proxy.
func TestProxyConfigFromCreds_EmptyCreds_Unsupported(t *testing.T) {
	_, err := proxyConfigFromCreds(map[string]string{})
	if !errors.Is(err, ErrUnsupportedSandboxCredentials) {
		t.Fatalf("err = %v, want ErrUnsupportedSandboxCredentials for empty creds", err)
	}
}

// TestProxyConfigFromCreds_AnthropicWinsOverBedrock pins precedence:
// when both ANTHROPIC_API_KEY and AWS_BEARER_TOKEN_BEDROCK are set,
// Anthropic wins. Mirrors the resolver's own precedence (which
// shouldn't surface this case in practice, since the resolver picks
// one branch — but proxy config is a separate gate and we keep the
// ordering consistent).
func TestProxyConfigFromCreds_AnthropicWinsOverBedrock(t *testing.T) {
	creds := map[string]string{
		"ANTHROPIC_API_KEY":        "sk-ant-wins",
		"AWS_BEARER_TOKEN_BEDROCK": "bdrk-loses",
	}
	cfg, err := proxyConfigFromCreds(creds)
	if err != nil {
		t.Fatalf("proxyConfigFromCreds: %v", err)
	}
	if cfg.llm.Provider != llmproxy.ProviderAnthropic {
		t.Errorf("Provider = %q, want anthropic (wins over bedrock_bearer)", cfg.llm.Provider)
	}
	if cfg.llm.APIKey != "sk-ant-wins" {
		t.Errorf("APIKey = %q, want sk-ant-wins", cfg.llm.APIKey)
	}
}

// TestBuildSandboxProxyEnv_Anthropic pins the sandbox env shape for
// the Anthropic path: ANTHROPIC_BASE_URL points at the proxy,
// ANTHROPIC_API_KEY is the placeholder (NEVER the real key).
func TestBuildSandboxProxyEnv_Anthropic(t *testing.T) {
	cfg := sandboxProxyConfig{providerKind: llmproxy.ProviderAnthropic}
	const token = "sk-ant-per-run-token-abc123"
	env := buildSandboxProxyEnv(cfg, "http://10.42.7.1:53312", token)

	if got := envValue(env, "ANTHROPIC_BASE_URL"); got != "http://10.42.7.1:53312" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want proxy URL", got)
	}
	if got := envValue(env, "ANTHROPIC_API_KEY"); got != token {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the per-run token the proxy authenticates against", got)
	}
}

// TestBuildSandboxProxyEnv_BedrockBearer pins the Bedrock-path env
// shape. ANTHROPIC_BEDROCK_BASE_URL points at the proxy,
// AWS_BEARER_TOKEN_BEDROCK is the placeholder, CLAUDE_CODE_USE_BEDROCK=1
// keeps the SDK routing through the Bedrock client.
func TestBuildSandboxProxyEnv_BedrockBearer(t *testing.T) {
	cfg := sandboxProxyConfig{providerKind: llmproxy.ProviderBedrockBearer}
	const token = "per-run-bedrock-token-xyz"
	env := buildSandboxProxyEnv(cfg, "http://10.42.7.1:53312", token)

	if got := envValue(env, "ANTHROPIC_BEDROCK_BASE_URL"); got != "http://10.42.7.1:53312" {
		t.Errorf("ANTHROPIC_BEDROCK_BASE_URL = %q, want proxy URL", got)
	}
	if got := envValue(env, "AWS_BEARER_TOKEN_BEDROCK"); got != token {
		t.Errorf("AWS_BEARER_TOKEN_BEDROCK = %q, want the per-run token", got)
	}
	if got := envValue(env, "CLAUDE_CODE_USE_BEDROCK"); got != "1" {
		t.Errorf("CLAUDE_CODE_USE_BEDROCK = %q, want 1", got)
	}
}

// TestBuildSandboxProxyEnv_BedrockSigV4 pins the SigV4-path env shape:
// the proxy URL, the per-run token as the placeholder AWS_ACCESS_KEY_ID
// (the SDK signs with it and the proxy reads it back out of the SigV4
// Authorization header's Credential scope), a constant throwaway
// secret, the Bedrock toggle, and the org's region so the sandbox SDK
// and the proxy's signing scope agree. Property B: the org's REAL
// secret key and session token must not appear anywhere.
func TestBuildSandboxProxyEnv_BedrockSigV4(t *testing.T) {
	const (
		realSecret  = "REAL-AWS-SECRET-MUST-NOT-LEAK"
		realSession = "REAL-SESSION-TOKEN-MUST-NOT-LEAK"
	)
	cfg, err := proxyConfigFromCreds(map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIAREALORGKEY",
		"AWS_SECRET_ACCESS_KEY": realSecret,
		"AWS_SESSION_TOKEN":     realSession,
		"AWS_REGION":            "eu-central-1",
	})
	if err != nil {
		t.Fatalf("proxyConfigFromCreds: %v", err)
	}
	token, err := newSandboxProxyToken(cfg.providerKind)
	if err != nil {
		t.Fatalf("newSandboxProxyToken: %v", err)
	}
	if !strings.HasPrefix(token, "AKIA") {
		t.Errorf("per-run SigV4 token = %q, want an AKIA-prefixed access-key-ID shape", token)
	}
	env := buildSandboxProxyEnv(cfg, "http://10.42.7.1:53312", token)

	if got := envValue(env, "ANTHROPIC_BEDROCK_BASE_URL"); got != "http://10.42.7.1:53312" {
		t.Errorf("ANTHROPIC_BEDROCK_BASE_URL = %q, want proxy URL", got)
	}
	if got := envValue(env, "AWS_ACCESS_KEY_ID"); got != token {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want the per-run token", got)
	}
	if got := envValue(env, "AWS_SECRET_ACCESS_KEY"); got != sigV4PlaceholderSecret {
		t.Errorf("AWS_SECRET_ACCESS_KEY = %q, want the constant placeholder", got)
	}
	if got := envValue(env, "CLAUDE_CODE_USE_BEDROCK"); got != "1" {
		t.Errorf("CLAUDE_CODE_USE_BEDROCK = %q, want 1", got)
	}
	if got := envValue(env, "AWS_REGION"); got != "eu-central-1" {
		t.Errorf("AWS_REGION = %q, want the org's region", got)
	}
	if got := envValue(env, "AWS_SESSION_TOKEN"); got != "" {
		t.Errorf("AWS_SESSION_TOKEN = %q present in sandbox env; the real session token belongs to the proxy only", got)
	}
	for _, e := range env {
		if strings.Contains(e, realSecret) || strings.Contains(e, realSession) {
			t.Errorf("PROPERTY B VIOLATED: sandbox env entry %q carries real AWS credential material", e)
		}
	}
}

// TestProxyConfigFromCreds_BearerWinsOverTriple pins mapping precedence
// within the Bedrock family: a (malformed) map carrying both the bearer
// and the triple resolves to the simpler bearer path, mirroring the
// resolver's own branch order.
func TestProxyConfigFromCreds_BearerWinsOverTriple(t *testing.T) {
	cfg, err := proxyConfigFromCreds(map[string]string{
		"AWS_BEARER_TOKEN_BEDROCK": "bdrk-wins",
		"AWS_ACCESS_KEY_ID":        "AKIA-loses",
		"AWS_SECRET_ACCESS_KEY":    "secret-loses",
	})
	if err != nil {
		t.Fatalf("proxyConfigFromCreds: %v", err)
	}
	if cfg.llm.Provider != llmproxy.ProviderBedrockBearer {
		t.Errorf("Provider = %q, want bedrock_bearer (wins over the triple)", cfg.llm.Provider)
	}
}

// TestBuildSandboxProxyEnv_NoRealCredentials is the load-bearing
// Property B assertion for the proxy env builder: no value in the
// returned slice may match a real credential. We test by passing
// "sentinel-real-key" as the resolver output's key value (which the
// proxy config retains for upstream injection) and asserting the
// sentinel is absent from the env that goes into the sandbox.
//
// Pins the credential-channel CI test acceptance:
// the sandbox env contains only proxy URLs + placeholders.
func TestBuildSandboxProxyEnv_NoRealCredentials(t *testing.T) {
	const realKey = "sk-ant-SENTINEL-REAL-KEY-MUST-NOT-LEAK"

	cfg, err := proxyConfigFromCreds(map[string]string{
		"ANTHROPIC_API_KEY": realKey,
	})
	if err != nil {
		t.Fatalf("proxyConfigFromCreds: %v", err)
	}
	// The per-run token is what the sandbox actually sees; it is a
	// capability scoped to this run's proxy, NOT the real key.
	token, err := newSandboxProxyToken(cfg.providerKind)
	if err != nil {
		t.Fatalf("newSandboxProxyToken: %v", err)
	}
	env := buildSandboxProxyEnv(cfg, "http://10.42.7.1:53312", token)

	for _, e := range env {
		if strings.Contains(e, realKey) {
			t.Errorf("PROPERTY B VIOLATED: sandbox env entry %q carries the real credential", e)
		}
	}
	// And the real key must have flowed into the *proxy config*
	// (where it lives on the host side, injecting upstream).
	if cfg.llm.APIKey != realKey {
		t.Errorf("real key dropped from proxy config; proxy can't inject upstream")
	}
}

// TestStartProxiesForSandbox_AnthropicEndToEnd asserts the proxy
// lifecycle from agentproc's perspective: start on a loopback IP
// (proxy is OK with that because AllowNonLoopback is on), send a
// request to the returned proxy URL, confirm the upstream sees the
// real key.
//
// Loopback is fine here because we're testing the proxy's own
// behavior — the actual binding to a veth IP requires Linux
// netns setup and is exercised by the integration test.
func TestStartProxiesForSandbox_AnthropicEndToEnd(t *testing.T) {
	const realKey = "sk-ant-real-end-to-end"

	// Fake upstream observing what the proxy forwards.
	var observedAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	// Override the upstream by injecting it via ANTHROPIC_BASE_URL —
	// the same admin-UI configuration path an org would use to point
	// at a gateway. Loopback http is permitted by llmproxy validation
	// (loopback is the test path).
	creds := map[string]string{
		"ANTHROPIC_API_KEY":  realKey,
		"ANTHROPIC_BASE_URL": upstream.URL,
	}
	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", creds, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bundle.Shutdown(ctx)
	})

	// Pull the proxy URL + the per-run token out of the sandbox-env
	// response and drive a request through it.
	proxyURL := envValue(env, "ANTHROPIC_BASE_URL")
	if proxyURL == "" {
		t.Fatalf("ANTHROPIC_BASE_URL missing from sandbox env: %v", env)
	}
	sdkKey := envValue(env, "ANTHROPIC_API_KEY")
	if sdkKey == realKey {
		t.Fatalf("PROPERTY B VIOLATED: sandbox env ANTHROPIC_API_KEY is the real key")
	}
	if !strings.HasPrefix(sdkKey, "sk-ant-") {
		t.Errorf("sandbox env ANTHROPIC_API_KEY = %q, want sk-ant- shaped per-run token", sdkKey)
	}

	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", sdkKey) // what the SDK forwards (the per-run token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy roundtrip: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("proxy returned %d for the correct per-run token; want 200 (token gate must accept the injected credential)", resp.StatusCode)
	}
	if observedAPIKey != realKey {
		t.Errorf("upstream observed x-api-key = %q, want real key %q (proxy must rewrite the per-run token with the real key)",
			observedAPIKey, realKey)
	}
}

// TestStartProxiesForSandbox_GitProxyStandsDownPrePushHook pins the push-capture
// handoff env: when the git proxy is wired, the sandbox env carries
// TF_GIT_PUSH_CAPTURE=proxy so the pre-push hook records nothing — the proxy's
// receive-pack capture (which sees the upstream's actual outcome) owns branch
// capture. Without a git proxy the entry is absent and the hook keeps its
// local-mode capture role.
func TestStartProxiesForSandbox_GitProxyStandsDownPrePushHook(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	creds := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x", "ANTHROPIC_BASE_URL": upstream.URL}

	git := &GitProxyConfig{
		TokenSource: func(context.Context, string, string) (gitproxy.Token, error) {
			return gitproxy.Token{Value: "ghs"}, nil
		},
		Upstream: upstream.URL,
	}
	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", creds, true, git, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox (with git): %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bundle.Shutdown(ctx)
	})
	if got := envValue(env, githooks.PushCaptureEnvVar); got != githooks.PushCaptureProxy {
		t.Errorf("%s = %q with a git proxy wired, want %q", githooks.PushCaptureEnvVar, got, githooks.PushCaptureProxy)
	}

	bundleNoGit, envNoGit, err := startProxiesForSandbox(context.Background(), "127.0.0.1", creds, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox (no git): %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bundleNoGit.Shutdown(ctx)
	})
	if got := envValue(envNoGit, githooks.PushCaptureEnvVar); got != "" {
		t.Errorf("%s = %q with no git proxy, want absent (the hook keeps capture)", githooks.PushCaptureEnvVar, got)
	}
}

// TestStartProxiesForSandbox_TokenAuthEnforced pins the token-auth invariant from
// agentproc's perspective: the proxy started for a run rejects a request
// bearing some *other* value (what a sibling run, or the old constant
// placeholder, would send) with 401, but accepts the exact token that
// was injected into this run's sandbox env. The upstream is only ever
// reached by the authorized request.
func TestStartProxiesForSandbox_TokenAuthEnforced(t *testing.T) {
	const realKey = "sk-ant-real-enforced"
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", map[string]string{
		"ANTHROPIC_API_KEY":  realKey,
		"ANTHROPIC_BASE_URL": upstream.URL,
	}, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bundle.Shutdown(ctx)
	})

	proxyURL := envValue(env, "ANTHROPIC_BASE_URL")
	sdkKey := envValue(env, "ANTHROPIC_API_KEY")

	// A sibling run would present its own (different) token; an attacker
	// might guess the prior constant placeholder. Both must 401,
	// as must a missing header.
	for _, wrong := range []string{
		"sk-ant-PROXY-PLACEHOLDER-DO-NOT-USE", // the prior constant
		"sk-ant-some-other-runs-token",
		"", // missing
	} {
		req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", strings.NewReader("{}"))
		if wrong != "" {
			req.Header.Set("x-api-key", wrong)
		}
		resp, derr := http.DefaultClient.Do(req)
		if derr != nil {
			t.Fatalf("wrong-token roundtrip (%q): %v", wrong, derr)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("x-api-key=%q: status = %d, want 401", wrong, resp.StatusCode)
		}
	}
	if hits != 0 {
		t.Fatalf("upstream reached %d time(s) by unauthorized requests; want 0", hits)
	}

	// The exact injected token is accepted and forwarded.
	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("x-api-key", sdkKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("good-token roundtrip: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("injected token: status = %d, want 200", resp.StatusCode)
	}
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1", hits)
	}
}

// TestStartProxiesForSandbox_SigV4LifecycleAndGate pins the Phase 2
// provider from agentproc's perspective: an org configured with the
// AWS triple gets a running proxy (no more typed rejection), the
// sandbox env carries the SigV4 placeholder shape with zero real
// credential material, and the token gate 401s callers that don't
// present this run's placeholder access-key ID. Only unauthorized
// requests are sent — they fail closed at the gate, so nothing ever
// dials the real Bedrock upstream the proxy points at.
func TestStartProxiesForSandbox_SigV4LifecycleAndGate(t *testing.T) {
	const realSecret = "REAL-AWS-SECRET-MUST-NOT-LEAK"
	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIAREALORGKEY",
		"AWS_SECRET_ACCESS_KEY": realSecret,
		"AWS_REGION":            "us-east-1",
	}, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bundle.Shutdown(ctx)
	})

	proxyURL := envValue(env, "ANTHROPIC_BEDROCK_BASE_URL")
	if proxyURL == "" {
		t.Fatalf("ANTHROPIC_BEDROCK_BASE_URL missing from sandbox env: %v", env)
	}
	token := envValue(env, "AWS_ACCESS_KEY_ID")
	if !strings.HasPrefix(token, "AKIA") {
		t.Errorf("sandbox AWS_ACCESS_KEY_ID = %q, want the AKIA-shaped per-run token", token)
	}
	if got := envValue(env, "AWS_SECRET_ACCESS_KEY"); got != sigV4PlaceholderSecret {
		t.Errorf("sandbox AWS_SECRET_ACCESS_KEY = %q, want the constant placeholder", got)
	}
	for _, e := range env {
		if strings.Contains(e, realSecret) {
			t.Fatalf("PROPERTY B VIOLATED: sandbox env entry %q carries the real AWS secret", e)
		}
	}

	// No credential header at all → 401 at the gate.
	reqBare, _ := http.NewRequest("POST", proxyURL+"/model/m/invoke", strings.NewReader("{}"))
	respBare, err := http.DefaultClient.Do(reqBare)
	if err != nil {
		t.Fatalf("bare roundtrip: %v", err)
	}
	_ = respBare.Body.Close()
	if respBare.StatusCode != http.StatusUnauthorized {
		t.Errorf("no Authorization: status = %d, want 401", respBare.StatusCode)
	}

	// A sibling run's placeholder AKID in a SigV4-shaped header → 401.
	// The gate parses only the Credential= access-key ID, so a dummy
	// signature suffices and nothing is forwarded upstream.
	reqSibling, _ := http.NewRequest("POST", proxyURL+"/model/m/invoke", strings.NewReader("{}"))
	reqSibling.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=AKIAOTHERRUNSTOKEN/20260707/us-east-1/bedrock/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef")
	respSibling, err := http.DefaultClient.Do(reqSibling)
	if err != nil {
		t.Fatalf("sibling roundtrip: %v", err)
	}
	_ = respSibling.Body.Close()
	if respSibling.StatusCode != http.StatusUnauthorized {
		t.Errorf("sibling AKID: status = %d, want 401", respSibling.StatusCode)
	}
}

// TestStartProxiesForSandbox_TokensArePerRun pins that two runs get
// distinct tokens — the property that makes the auth cross-tenant-safe.
// If runs shared a token, a sibling could replay it against this run's
// proxy.
func TestStartProxiesForSandbox_TokensArePerRun(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	creds := map[string]string{
		"ANTHROPIC_API_KEY":  "k",
		"ANTHROPIC_BASE_URL": upstream.URL,
	}

	b1, env1, err := startProxiesForSandbox(context.Background(), "127.0.0.1", creds, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	t.Cleanup(func() { _ = b1.Shutdown(context.Background()) })
	b2, env2, err := startProxiesForSandbox(context.Background(), "127.0.0.1", creds, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	t.Cleanup(func() { _ = b2.Shutdown(context.Background()) })

	tok1 := envValue(env1, "ANTHROPIC_API_KEY")
	tok2 := envValue(env2, "ANTHROPIC_API_KEY")
	if tok1 == "" || tok2 == "" {
		t.Fatalf("empty token(s): %q / %q", tok1, tok2)
	}
	if tok1 == tok2 {
		t.Errorf("two runs got the same proxy token %q; tokens must be per-run so a sibling can't replay", tok1)
	}
}

// TestStartProxiesForSandbox_ShutdownTearsDownProxy pins the
// lifecycle invariant: after Shutdown, the proxy stops accepting
// connections. Required for the "kill the agent run mid-
// execution, assert both proxies are torn down" acceptance check.
func TestStartProxiesForSandbox_ShutdownTearsDownProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	creds := map[string]string{
		"ANTHROPIC_API_KEY":  "k",
		"ANTHROPIC_BASE_URL": upstream.URL,
	}
	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", creds, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox: %v", err)
	}

	// Live before Shutdown — sanity check.
	proxyURL := envValue(env, "ANTHROPIC_BASE_URL")
	req, _ := http.NewRequest("POST", proxyURL, strings.NewReader("{}"))
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatalf("pre-shutdown roundtrip: %v", err)
	} else {
		_ = resp.Body.Close()
	}

	// Shutdown and confirm the proxy is no longer reachable.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := bundle.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Tight per-request timeout — a dead listener should fail fast
	// with a connection-refused, not hang waiting on the bound port.
	client := &http.Client{Timeout: 1 * time.Second}
	if _, err := client.Post(proxyURL, "application/json", strings.NewReader("{}")); err == nil {
		t.Error("proxy still accepting connections after Shutdown")
	}
}

// TestStartProxiesForSandbox_EmptyHostIPRejected pins the
// caller-bug guard. An empty hostVethIP would let the proxy bind on
// the kernel's default interface (0.0.0.0) which would expose the
// credentialed proxy to anything that can reach the host. Fail
// loudly at construction.
func TestStartProxiesForSandbox_EmptyHostIPRejected(t *testing.T) {
	_, _, err := startProxiesForSandbox(context.Background(), "", map[string]string{"ANTHROPIC_API_KEY": "k"}, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err == nil {
		t.Fatal("startProxiesForSandbox accepted empty hostVethIP; should reject")
	}
}

// TestRunProxies_ShutdownIsNilSafe pins the defensive nil-guard. The
// caller's defer fires even when startProxiesForSandbox returned an
// error and bundle is nil; Shutdown must handle that without
// panicking.
func TestRunProxies_ShutdownIsNilSafe(t *testing.T) {
	var p *runProxies
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("nil Shutdown returned err: %v", err)
	}
}

// TestProxyConfigFromCreds_RejectsMalformedGateway pins the
// pre-flight gates that mirror llmproxy.New. An org-configured
// ANTHROPIC_BASE_URL with a path / query / fragment / cleartext
// non-loopback scheme is rejected at proxy-config time so the
// admin-facing error names "anthropic upstream" instead of falling
// through to a generic llmproxy error from inside Start.
func TestProxyConfigFromCreds_RejectsMalformedGateway(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
	}{
		{"with_path", "https://gateway.example.com/v1"},
		{"with_query", "https://gateway.example.com?token=x"},
		{"with_fragment", "https://gateway.example.com#frag"},
		{"http_non_loopback", "http://gateway.example.com"},
		{"missing_scheme", "gateway.example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := proxyConfigFromCreds(map[string]string{
				"ANTHROPIC_API_KEY":  "k",
				"ANTHROPIC_BASE_URL": c.baseURL,
			})
			if err == nil {
				t.Fatalf("proxyConfigFromCreds accepted %q; want validation error", c.baseURL)
			}
			if !strings.Contains(err.Error(), "anthropic upstream") {
				t.Errorf("err = %v; want it to name \"anthropic upstream\" so the admin-facing message is clear", err)
			}
		})
	}
}

// TestRunProxies_ShutdownAggregatesErrors pins the errors.Join
// behavior that the doc comment promises. Today only the LLM proxy
// is wired, so this exercises the one-error path; the test shape is
// future-proof for the git-proxy slot landing.
func TestRunProxies_ShutdownAggregatesErrors(t *testing.T) {
	// Construct a bundle with a Server we can shut down twice — the
	// second Shutdown is a no-op (returns nil), so we can't easily
	// force an error without a fake. Use httptest + a real proxy and
	// just confirm a clean shutdown returns nil.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	bundle, _, err := startProxiesForSandbox(context.Background(), "127.0.0.1", map[string]string{
		"ANTHROPIC_API_KEY":  "k",
		"ANTHROPIC_BASE_URL": upstream.URL,
	}, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := bundle.Shutdown(ctx); err != nil {
		t.Errorf("clean Shutdown = %v, want nil", err)
	}
}

// --- git proxy wiring (TFAC-302) -------------------------------------

// gitConfigMap decodes git's indexed env-config form (GIT_CONFIG_COUNT +
// GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n) in env into a key→value map.
// Index-agnostic so a test can look up a config key by name regardless of
// where it lands in the block — the consolidated sandbox block puts
// core.hooksPath at index 0 and the proxy pairs after it.
func gitConfigMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	count, _ := strconv.Atoi(envValue(env, "GIT_CONFIG_COUNT"))
	out := make(map[string]string, count)
	for i := 0; i < count; i++ {
		k := envValue(env, fmt.Sprintf("GIT_CONFIG_KEY_%d", i))
		v := envValue(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d", i))
		out[k] = v
	}
	return out
}

// gitConfigValueWithSuffix returns the value of the single git config key
// ending in suffix (e.g. ".insteadOf", ".extraHeader"), failing if zero
// or more than one match.
func gitConfigValueWithSuffix(t *testing.T, env []string, suffix string) (key, value string) {
	t.Helper()
	var matches []string
	cfg := gitConfigMap(t, env)
	for k := range cfg {
		if strings.HasSuffix(k, suffix) {
			matches = append(matches, k)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly one git config key ending %q, got %v (env: %v)", suffix, matches, env)
	}
	return matches[0], cfg[matches[0]]
}

// gitProxyBaseFromEnv extracts the "http://host:port" proxy base the
// git env entries route to, parsed out of the url.<base>.insteadOf key.
func gitProxyBaseFromEnv(t *testing.T, env []string) string {
	t.Helper()
	k, _ := gitConfigValueWithSuffix(t, env, ".insteadOf")
	v := strings.TrimSuffix(strings.TrimPrefix(k, "url."), ".insteadOf")
	return strings.TrimRight(v, "/")
}

// gitProxyTokenFromEnv extracts the per-run token from the
// http.<base>.extraHeader Basic credential the git env entries carry.
func gitProxyTokenFromEnv(t *testing.T, env []string) string {
	t.Helper()
	_, hv := gitConfigValueWithSuffix(t, env, ".extraHeader")
	b64 := strings.TrimPrefix(hv, "Authorization: Basic ")
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode extraHeader %q: %v", hv, err)
	}
	_, tok, ok := strings.Cut(string(raw), ":")
	if !ok {
		t.Fatalf("extraHeader credential not user:token: %q", raw)
	}
	return tok
}

// TestSandboxGitProxyPairs_Shape pins the exact git config pairs that
// route the in-sandbox git through the proxy: an insteadOf rewrite from
// the upstream host to the proxy base, and an extraHeader carrying the
// per-run token (NOT the real credential) as the Basic password. The
// pairs are folded into the consolidated GIT_CONFIG_* block (with
// core.hooksPath) by startProxiesForSandbox; here we assert the pairs
// themselves.
func TestSandboxGitProxyPairs_Shape(t *testing.T) {
	pairs := sandboxGitProxyPairs("http://10.42.7.1:5123", "https://github.com", "per-run-secret", "", "")
	got := map[string]string{}
	for _, p := range pairs {
		got[p[0]] = p[1]
	}

	if len(pairs) != 2 {
		t.Errorf("len(pairs) = %d, want 2", len(pairs))
	}
	if v := got["url.http://10.42.7.1:5123/.insteadOf"]; v != "https://github.com/" {
		t.Errorf("insteadOf value = %q, want https://github.com/ (rewrites this prefix)", v)
	}
	wantHeader := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-run:per-run-secret"))
	if v := got["http.http://10.42.7.1:5123/.extraHeader"]; v != wantHeader {
		t.Errorf("extraHeader value = %q, want %q", v, wantHeader)
	}
}

// TestSandboxGitProxyPairs_SharedOrigin pins the routing that makes gh's repo
// inference work: with a shared origin the sandbox's git resolves every upstream
// URL onto https://<GH_HOST>/, so `git remote -v` — which is what gh reads —
// reports a remote on the very host GH_HOST names. The per-run Basic placeholder
// is unchanged (same proxy, same credential discipline) and the TLS trust file
// is named explicitly, because git resolves CA trust from its own config rather
// than the process-global SSL_CERT_FILE gh reads.
func TestSandboxGitProxyPairs_SharedOrigin(t *testing.T) {
	pairs := sandboxGitProxyPairs("http://10.42.7.1:5123", "https://github.com", "per-run-secret", "10.42.7.1", "/run/tf-gh-injector.crt")
	got := map[string]string{}
	for _, p := range pairs {
		got[p[0]] = p[1]
	}

	if len(pairs) != 3 {
		t.Fatalf("len(pairs) = %d, want 3 (insteadOf + extraHeader + sslCAInfo)", len(pairs))
	}
	if v := got["url.https://10.42.7.1/.insteadOf"]; v != "https://github.com/" {
		t.Errorf("insteadOf value = %q, want https://github.com/ rewritten onto the shared origin", v)
	}
	wantHeader := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-run:per-run-secret"))
	if v := got["http.https://10.42.7.1/.extraHeader"]; v != wantHeader {
		t.Errorf("extraHeader value = %q, want %q", v, wantHeader)
	}
	if v := got["http.https://10.42.7.1/.sslCAInfo"]; v != "/run/tf-gh-injector.crt" {
		t.Errorf("sslCAInfo value = %q, want the mounted trust file", v)
	}
	// The shared origin carries no port: gh drops it when matching a remote
	// against GH_HOST, so a ported rewrite target would leave inference broken
	// exactly as the git proxy's own address does.
	for _, p := range pairs {
		if strings.Contains(p[0], "10.42.7.1:") {
			t.Errorf("pair key %q names a port; the shared origin must be portless or gh's inference cannot match GH_HOST", p[0])
		}
	}

	// Half-wired degrades to the git proxy's own address rather than routing git
	// at a TLS listener whose CA it was never told about.
	for _, c := range []struct{ host, ca string }{
		{"10.42.7.1", ""},
		{"", "/run/tf-gh-injector.crt"},
	} {
		half := sandboxGitProxyPairs("http://10.42.7.1:5123", "https://github.com", "per-run-secret", c.host, c.ca)
		if len(half) != 2 || half[0][0] != "url.http://10.42.7.1:5123/.insteadOf" {
			t.Errorf("half-wired shared origin (host=%q ca=%q) produced %v; want the git proxy's own routing", c.host, c.ca, half)
		}
	}
}

// TestEncodeGitConfigEnv_HooksAlwaysPresent pins that the consolidated
// GIT_CONFIG_* block always carries core.hooksPath at index 0 (F2,
// TFAC-456) and a single coherent GIT_CONFIG_COUNT covering it plus any
// proxy pairs layered after it.
func TestEncodeGitConfigEnv_HooksAlwaysPresent(t *testing.T) {
	pairs := append([][2]string{{githooks.ConfigKey, githooks.SandboxDir}},
		sandboxGitProxyPairs("http://10.42.7.1:5123", "https://github.com", "tok", "", "")...)
	env := encodeGitConfigEnv(pairs)

	if got := envValue(env, "GIT_CONFIG_COUNT"); got != "3" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 3 (hooks + 2 proxy pairs)", got)
	}
	cfg := gitConfigMap(t, env)
	if cfg[githooks.ConfigKey] != githooks.SandboxDir {
		t.Errorf("%s = %q, want %q", githooks.ConfigKey, cfg[githooks.ConfigKey], githooks.SandboxDir)
	}
}

// TestStartGitProxyForSandbox_RoutesAndAuthenticates is the git-proxy
// analogue of the LLM end-to-end test: a request bearing this run's
// per-run token is forwarded with the real credential swapped in, while
// a different run's token (the cross-run case) gets 401 and never
// reaches the upstream. The real GitHub token never appears in the env.
func TestStartGitProxyForSandbox_RoutesAndAuthenticates(t *testing.T) {
	const realToken = "ghs_REAL_GIT_TOKEN"
	var (
		gotAuth string
		hits    int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	src := func(ctx context.Context, owner, repo string) (gitproxy.Token, error) {
		return gitproxy.Token{Value: realToken, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	pairs, proxyURL, incomingToken, srv, err := startGitProxyForSandbox(context.Background(), "127.0.0.1", &GitProxyConfig{TokenSource: src, Upstream: upstream.URL})
	if err != nil {
		t.Fatalf("startGitProxyForSandbox: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	env := encodeGitConfigEnv(pairs)

	base := gitProxyBaseFromEnv(t, env)
	runToken := gitProxyTokenFromEnv(t, env)
	// The surfaced coordinates (for the orchestrator's own clone) must match
	// what the sandbox git-config pairs encode — same proxy, same per-run token.
	if proxyURL != base {
		t.Errorf("surfaced git proxy URL = %q, want the pairs' base %q", proxyURL, base)
	}
	if incomingToken != runToken {
		t.Errorf("surfaced git proxy token != the token encoded in the pairs")
	}
	if incomingToken == realToken {
		t.Fatal("PROPERTY B VIOLATED: surfaced git proxy token equals the real credential")
	}
	if runToken == realToken {
		t.Fatal("PROPERTY B VIOLATED: per-run git token equals the real credential")
	}
	if strings.Contains(strings.Join(env, "\n"), realToken) {
		t.Fatal("PROPERTY B VIOLATED: real git credential present in sandbox git env")
	}

	// Authorized: this run's token → 200, upstream sees the real cred.
	req, _ := http.NewRequest("GET", base+"/owner/repo/info/refs?service=git-receive-pack", nil)
	req.SetBasicAuth(gitProxyBasicUser, runToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authorized roundtrip: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("authorized status = %d, want 200", resp.StatusCode)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+realToken))
	if gotAuth != want {
		t.Errorf("upstream Authorization = %q, want %q (proxy must inject the real credential)", gotAuth, want)
	}

	// Cross-run: a sibling run's token → 401, upstream not reached again.
	hitsBefore := hits
	req2, _ := http.NewRequest("GET", base+"/owner/repo/info/refs", nil)
	req2.SetBasicAuth(gitProxyBasicUser, "some-other-runs-token")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("cross-run roundtrip: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("cross-run token status = %d, want 401", resp2.StatusCode)
	}
	if hits != hitsBefore {
		t.Errorf("upstream reached by a cross-run (unauthorized) request; want fail-closed at the proxy")
	}
}

// TestStartProxiesForSandbox_GitNilSkipsGitProxy pins that a run with no
// git egress need (prompt-only) gets no git proxy and no proxy GIT_CONFIG
// entries — only the LLM proxy. core.hooksPath (F2) is still set: it is
// proxy-independent, so a repo-less run that clones into a subdir is still
// covered.
func TestStartProxiesForSandbox_GitNilSkipsGitProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer upstream.Close()

	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", map[string]string{
		"ANTHROPIC_API_KEY":  "k",
		"ANTHROPIC_BASE_URL": upstream.URL,
	}, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox: %v", err)
	}
	t.Cleanup(func() { _ = bundle.Shutdown(context.Background()) })

	if bundle.git != nil {
		t.Error("git proxy started despite nil GitProxy")
	}
	// Only the hooks entry — no proxy insteadOf/extraHeader pairs.
	if got := envValue(env, "GIT_CONFIG_COUNT"); got != "1" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 1 (core.hooksPath only, no proxy pairs)", got)
	}
	cfg := gitConfigMap(t, env)
	if cfg[githooks.ConfigKey] != githooks.SandboxDir {
		t.Errorf("%s = %q, want %q even with nil GitProxy", githooks.ConfigKey, cfg[githooks.ConfigKey], githooks.SandboxDir)
	}
	for k := range cfg {
		if strings.HasSuffix(k, ".insteadOf") || strings.HasSuffix(k, ".extraHeader") {
			t.Errorf("unexpected proxy git config %q present despite nil GitProxy", k)
		}
	}
}

// TestStartProxiesForSandbox_GitNoCredentialsTypedError pins the
// ErrUnsupportedSandboxCredentials parity for git: a run whose
// TokenSource reports no credential fails fast with the typed
// ErrNoGitCredentials (so the caller renders an admin message),
// and the bundle is nil so the deferred Shutdown is a safe no-op — the
// already-started LLM proxy having been torn down on the way out.
func TestStartProxiesForSandbox_GitNoCredentialsTypedError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer upstream.Close()

	src := func(ctx context.Context, owner, repo string) (gitproxy.Token, error) {
		return gitproxy.Token{Value: "unused"}, nil
	}
	// The no-credentials check now lives in ProbeCredentials (the run-start
	// probe), not the per-repo TokenSource.
	probe := func(ctx context.Context) error {
		return fmt.Errorf("resolver said: %w", ErrNoGitCredentials)
	}
	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", map[string]string{
		"ANTHROPIC_API_KEY":  "k",
		"ANTHROPIC_BASE_URL": upstream.URL,
	}, true, &GitProxyConfig{TokenSource: src, ProbeCredentials: probe}, egressproxy.GitHubHosts{}, nil, nil)
	if !errors.Is(err, ErrNoGitCredentials) {
		t.Fatalf("err = %v, want ErrNoGitCredentials", err)
	}
	if bundle != nil {
		t.Errorf("bundle = %v, want nil on git failure (caller's defer Shutdown must no-op)", bundle)
	}
	if env != nil {
		t.Errorf("env = %v, want nil on git failure", env)
	}
}

// TestStartProxiesForSandbox_GitProxyTornDownOnShutdown pins that the
// git proxy, like the LLM proxy, stops accepting connections after the
// bundle's Shutdown — the "kill the run, both proxies die"
// invariant extended to the git slot.
func TestStartProxiesForSandbox_GitProxyTornDownOnShutdown(t *testing.T) {
	llmUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer llmUp.Close()
	gitUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer gitUp.Close()

	src := func(ctx context.Context, owner, repo string) (gitproxy.Token, error) {
		return gitproxy.Token{Value: "ghs_x", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", map[string]string{
		"ANTHROPIC_API_KEY":  "k",
		"ANTHROPIC_BASE_URL": llmUp.URL,
	}, true, &GitProxyConfig{TokenSource: src, Upstream: gitUp.URL}, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox: %v", err)
	}
	if bundle.git == nil {
		t.Fatal("git proxy not started despite non-nil GitProxy")
	}

	base := gitProxyBaseFromEnv(t, env)
	runToken := gitProxyTokenFromEnv(t, env)

	// Live before Shutdown.
	req, _ := http.NewRequest("GET", base+"/owner/repo/info/refs", nil)
	req.SetBasicAuth(gitProxyBasicUser, runToken)
	if resp, derr := http.DefaultClient.Do(req); derr != nil {
		t.Fatalf("pre-shutdown roundtrip: %v", derr)
	} else {
		_ = resp.Body.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := bundle.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	client := &http.Client{Timeout: 1 * time.Second}
	if _, err := client.Get(base + "/owner/repo/info/refs"); err == nil {
		t.Error("git proxy still accepting connections after Shutdown")
	}
}

// TestStartProxiesForSandbox_FoldsOrgIdentity pins that the org commit identity
// (TFAC-452) lands in the sandbox's single GIT_CONFIG_* block: core.hooksPath
// stays at index 0, user.name/user.email layer in after it, and the one
// GIT_CONFIG_COUNT covers all three. git=nil here, so the block is hooks +
// identity only (no proxy pairs).
func TestStartProxiesForSandbox_FoldsOrgIdentity(t *testing.T) {
	creds := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"}
	identity := githooks.IdentityConfigPairs("acme-bot[bot]", "acme-bot[bot]@users.noreply.github.com")

	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", creds, true, nil, egressproxy.GitHubHosts{}, nil, nil, identity...)
	if err != nil {
		t.Fatalf("startProxiesForSandbox: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bundle.Shutdown(ctx)
	})

	cfg := gitConfigMap(t, env)
	if cfg[githooks.ConfigKey] != githooks.SandboxDir {
		t.Errorf("%s = %q, want %q (hooks must remain present)", githooks.ConfigKey, cfg[githooks.ConfigKey], githooks.SandboxDir)
	}
	if cfg["user.name"] != "acme-bot[bot]" {
		t.Errorf("user.name = %q, want acme-bot[bot]", cfg["user.name"])
	}
	if cfg["user.email"] != "acme-bot[bot]@users.noreply.github.com" {
		t.Errorf("user.email = %q, want the bot noreply email", cfg["user.email"])
	}
	if k := envValue(env, "GIT_CONFIG_KEY_0"); k != githooks.ConfigKey {
		t.Errorf("GIT_CONFIG_KEY_0 = %q, want %q (hooks must stay at index 0)", k, githooks.ConfigKey)
	}
	if got := envValue(env, "GIT_CONFIG_COUNT"); got != "3" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 3 (hooks + 2 identity pairs)", got)
	}
	// Exactly one GIT_CONFIG_COUNT line — the consolidated-block invariant.
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("GIT_CONFIG_COUNT appears %d times, want exactly 1: %v", n, env)
	}
}

// envValue returns the value associated with KEY in a slice of
// KEY=VALUE strings, or "" if KEY is absent. Used by the proxy tests
// to assert env shape without writing the split-on-equals dance
// repeatedly.
func envValue(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return e[len(prefix):]
		}
	}
	return ""
}

// TestStartProxiesForSandbox_EgressProxyEnv pins the TFAC-567 wiring:
// every sandbox run's env carries all four proxy-var spellings pointing
// at the gating egress proxy with the per-run token as URL userinfo,
// plus the NO_PROXY exemption for the gateway IP. The exemption is
// load-bearing — git's insteadOf rewrite targets http://<gateway>:<port>,
// and git honors http_proxy for http:// URLs, so without it every
// sandbox git op would try to tunnel through the egress proxy and 403.
func TestStartProxiesForSandbox_EgressProxyEnv(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	creds := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x", "ANTHROPIC_BASE_URL": upstream.URL}

	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", creds, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bundle.Shutdown(ctx)
	})

	if bundle.egress == nil {
		t.Fatal("bundle.egress is nil; the gating egress proxy must start for every sandbox run")
	}

	httpsProxy := envValue(env, "HTTPS_PROXY")
	if httpsProxy == "" {
		t.Fatalf("HTTPS_PROXY missing from sandbox env: %v", env)
	}
	for _, key := range []string{"https_proxy", "HTTP_PROXY", "http_proxy"} {
		if got := envValue(env, key); got != httpsProxy {
			t.Errorf("%s = %q, want %q (all four spellings must agree)", key, got, httpsProxy)
		}
	}

	// URL shape: scheme http, userinfo x-run:<token>, host = the veth IP.
	u, perr := url.Parse(httpsProxy)
	if perr != nil {
		t.Fatalf("parse HTTPS_PROXY %q: %v", httpsProxy, perr)
	}
	if u.User == nil || u.User.Username() != egressproxy.BasicUser {
		t.Errorf("proxy URL user = %v, want %q", u.User, egressproxy.BasicUser)
	}
	if pw, ok := u.User.Password(); !ok || pw == "" {
		t.Error("proxy URL must carry the per-run token as the password")
	}
	if host, _, _ := net.SplitHostPort(u.Host); host != "127.0.0.1" {
		t.Errorf("proxy URL host = %q, want the veth IP", u.Host)
	}

	// NO_PROXY carries the gateway IP so the git/LLM proxy hops stay
	// direct instead of looping through the egress proxy.
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		got := envValue(env, key)
		if !strings.Contains(got, "127.0.0.1") {
			t.Errorf("%s = %q, must exempt the gateway IP", key, got)
		}
	}
}

// TestStartProxiesForSandbox_EgressProxyGatesConnect drives one CONNECT
// through the egress proxy that startProxiesForSandbox actually started,
// using the exact env the sandbox would see: right token + off-allowlist
// host → 403 (policy reached, auth passed); wrong token → 407. Pins that
// the token in the env and the token the proxy checks are the same mint.
func TestStartProxiesForSandbox_EgressProxyGatesConnect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	creds := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x", "ANTHROPIC_BASE_URL": upstream.URL}

	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", creds, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bundle.Shutdown(ctx)
	})

	u, err := url.Parse(envValue(env, "HTTPS_PROXY"))
	if err != nil {
		t.Fatalf("parse HTTPS_PROXY: %v", err)
	}
	token, _ := u.User.Password()

	connect := func(auth string) string {
		conn, derr := net.DialTimeout("tcp", u.Host, 2*time.Second)
		if derr != nil {
			t.Fatalf("dial egress proxy: %v", derr)
		}
		defer conn.Close()
		req := "CONNECT evil.example.com:443 HTTP/1.1\r\nHost: evil.example.com:443\r\n"
		if auth != "" {
			req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(auth)) + "\r\n"
		}
		req += "\r\n"
		_, _ = conn.Write([]byte(req))
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 2048)
		n, _ := conn.Read(buf)
		return string(buf[:n])
	}

	if resp := connect(egressproxy.BasicUser + ":" + token); !strings.Contains(resp, "403") {
		t.Errorf("authorized CONNECT to off-allowlist host got:\n%s\nwant 403 (past auth, denied by policy)", resp)
	}
	if resp := connect(egressproxy.BasicUser + ":not-the-token"); !strings.Contains(resp, "407") {
		t.Errorf("wrong-token CONNECT got:\n%s\nwant 407", resp)
	}
}

// TestStartProxiesForSandbox_EgressDenialReachesRecorder is the regression
// test for the gap this ticket closes: egressproxy.Config.RecordDenial existed
// — bounded queue, drain goroutine, drop counter — but the production
// construction path never set it, so denials reached slog and nothing else for
// a full release cycle. The proxy is started through the SAME function
// production calls, and the assertion is behavioral: a refused CONNECT must
// arrive at the caller's hook, target and reason intact.
func TestStartProxiesForSandbox_EgressDenialReachesRecorder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	creds := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x", "ANTHROPIC_BASE_URL": upstream.URL}

	denied := make(chan egressproxy.DeniedConnect, 4)
	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", creds, true, nil, egressproxy.GitHubHosts{},
		func(_ context.Context, d egressproxy.DeniedConnect) { denied <- d }, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bundle.Shutdown(ctx)
	})

	u, err := url.Parse(envValue(env, "HTTPS_PROXY"))
	if err != nil {
		t.Fatalf("parse HTTPS_PROXY: %v", err)
	}
	token, _ := u.User.Password()

	conn, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err != nil {
		t.Fatalf("dial egress proxy: %v", err)
	}
	defer conn.Close()
	auth := base64.StdEncoding.EncodeToString([]byte(egressproxy.BasicUser + ":" + token))
	_, _ = conn.Write([]byte("CONNECT api.github.com:443 HTTP/1.1\r\nHost: api.github.com:443\r\n" +
		"Proxy-Authorization: Basic " + auth + "\r\n\r\n"))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Read(make([]byte, 512))

	select {
	case d := <-denied:
		if d.Target != "api.github.com:443" {
			t.Errorf("recorded target = %q, want the CONNECT authority", d.Target)
		}
		if !strings.Contains(d.Reason, "allowlist") {
			t.Errorf("recorded reason = %q, want the policy reason the agent also saw", d.Reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a refused CONNECT never reached the audit hook — RecordDenial is unwired on the production construction path")
	}
}

// TestStartProxies_SandboxLLMOffKeepsTheJailOffTheProvider is the cell-shrink
// acceptance: an engagement whose engine runs outside the jail asks for a
// proxy it dials itself, and the jail is handed no way to reach a provider at
// all — no address, no placeholder, nothing an SDK client could bootstrap
// from. The proxy is still bound, because that is where the real key lives and
// the caller outside the jail still has to reach it.
func TestStartProxies_SandboxLLMOffKeepsTheJailOffTheProvider(t *testing.T) {
	creds := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real-key-value"}

	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", creds, false, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bundle.Shutdown(ctx)
	})

	// Every LLM key the env allowlist knows about, checked by name rather than
	// by "the ones this provider happens to use": a future provider arm that
	// adds a key must not be able to leak it into a jail that calls nothing.
	for _, key := range []string{
		"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY",
		"ANTHROPIC_BEDROCK_BASE_URL", "AWS_BEARER_TOKEN_BEDROCK",
		"CLAUDE_CODE_USE_BEDROCK", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
	} {
		if v := envValue(env, key); v != "" {
			t.Errorf("sandbox env carries %s=%q; a jail whose engine runs outside it has no LLM channel", key, v)
		}
	}

	// The egress proxy and the git-config block are untouched: the shrink is
	// about the provider hop, not about the rest of the cell.
	if envValue(env, "HTTPS_PROXY") == "" {
		t.Error("sandbox env lost its egress proxy routing")
	}

	// The coordinates still exist for whoever does dial the proxy, and the
	// placeholder there is a placeholder, not the org's key.
	handle := &RunProxyHandle{p: bundle}
	llm := handle.LLMEnv()
	if envValue(llm, "ANTHROPIC_BASE_URL") == "" {
		t.Fatalf("LLMEnv carries no provider address: %v", llm)
	}
	if got := envValue(llm, "ANTHROPIC_API_KEY"); got == creds["ANTHROPIC_API_KEY"] {
		t.Fatal("PROPERTY B VIOLATED: LLMEnv carries the real provider key")
	}
}

// TestStartProxies_SandboxLLMOnStillPointsTheJailAtTheProxy is the other side
// of the same switch — the SDK shape, which must be exactly what it was.
func TestStartProxies_SandboxLLMOnStillPointsTheJailAtTheProxy(t *testing.T) {
	creds := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real-key-value"}

	bundle, env, err := startProxiesForSandbox(context.Background(), "127.0.0.1", creds, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		t.Fatalf("startProxiesForSandbox: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bundle.Shutdown(ctx)
	})

	sandboxURL := envValue(env, "ANTHROPIC_BASE_URL")
	if sandboxURL == "" {
		t.Fatalf("sandbox env lost ANTHROPIC_BASE_URL: %v", env)
	}
	// One proxy, one address: the jail and the handle name the same listener,
	// so nothing about this switch can end up starting two.
	handle := &RunProxyHandle{p: bundle}
	if handleURL := envValue(handle.LLMEnv(), "ANTHROPIC_BASE_URL"); handleURL != sandboxURL {
		t.Errorf("handle LLM address %q != sandbox %q", handleURL, sandboxURL)
	}
	if envValue(env, "ANTHROPIC_API_KEY") == creds["ANTHROPIC_API_KEY"] {
		t.Fatal("PROPERTY B VIOLATED: sandbox env carries the real provider key")
	}
}
