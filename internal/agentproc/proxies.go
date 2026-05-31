package agentproc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/llmproxy"
)

// proxyTokenAnthropicPrefix shapes the per-run Anthropic token like a
// real key (sk-ant-…) so the agent SDK's client-side key-format check
// passes — the SDK requires a plausibly-shaped ANTHROPIC_API_KEY to
// construct a request at all. The body is fresh random per run; it is
// NOT a real key. See newSandboxProxyToken and the llmproxy package
// doc's trust-model section for why injecting a per-run token (rather
// than a constant placeholder) is what closes the SKY-395 cross-tenant
// hole: the proxy now authenticates the caller against this exact value.
const proxyTokenAnthropicPrefix = "sk-ant-"

// newSandboxProxyToken mints the fresh per-run secret that both
// authenticates the sandbox to its own proxy (set as
// llmproxy.Config.IncomingToken) and is injected into the sandbox as
// the credential the SDK sends. 32 bytes of crypto/rand, hex-encoded.
// For Anthropic it carries the sk-ant- prefix so the SDK's key-shape
// check passes; the Bedrock bearer path has no such shape requirement.
//
// This is a per-run capability, not a durable credential: generated
// fresh here and destroyed when the proxy is torn down at run end. It
// does not violate Property B — the real provider key stays in the
// proxy (injected upstream by the rewrite hook) and never enters the
// sandbox.
func newSandboxProxyToken(kind llmproxy.Provider) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("agentproc: generate per-run proxy token: %w", err)
	}
	tok := hex.EncodeToString(raw)
	if kind == llmproxy.ProviderAnthropic {
		return proxyTokenAnthropicPrefix + tok, nil
	}
	return tok, nil
}

// runProxies bundles the per-run proxy handles for shutdown. Only
// the LLM proxy is wired in SKY-335; the git proxy slot is reserved
// for the sibling ticket and remains nil until it lands.
type runProxies struct {
	llm *llmproxy.Server
	// git *gitproxy.Server // sibling ticket (SKY-335's twin)
}

// Shutdown stops every proxy in the bundle. Returns errors.Join of
// every proxy's Shutdown error so a future bundle with multiple
// proxies (git proxy slot) surfaces all failures, not just the
// first. Today, with only the LLM proxy wired, the result is either
// nil or a single wrapped error.
func (p *runProxies) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var errs []error
	if p.llm != nil {
		if err := p.llm.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("llm proxy shutdown: %w", err))
		}
	}
	return errors.Join(errs...)
}

// ErrUnsupportedSandboxCredentials is returned when the resolved
// credentials don't map to a proxy-able shape. AWS SigV4 (access-key
// triple without bearer) is the current gap: the Phase 1 llmproxy
// only handles bearer-style headers, so an org configured with the
// AWS triple can't run in multi mode until the Phase 2 SigV4 proxy
// lands. Surfaced as a typed error so the caller (delegate / scorer)
// can produce a clear admin-facing message rather than a confusing
// upstream auth error from inside the Node subprocess.
var ErrUnsupportedSandboxCredentials = errors.New("agentproc: resolved credentials shape not supported in sandbox mode")

// startProxiesForSandbox starts the per-run LLM proxy (and, when the
// sibling ticket lands, the git proxy) on hostVethIP. Returns the
// proxy bundle for shutdown plus the env entries the sandbox should
// inject so the agent reaches the proxy instead of the real upstream.
//
// hostVethIP is the host-side veth address — the 10.42.<idx>.1 IP
// that the sandbox's netns can reach via its default route. Binding
// here (not loopback) is the whole point: the sandboxed agent sees
// only the netns and can't reach 127.0.0.1, so a loopback-bound proxy
// would be invisible to it.
//
// The resolvedCreds map is what resolveCredentials produced — the
// shape determines the proxy's provider + upstream. See the switch
// below for the mapping.
//
// Caller MUST call returned.Shutdown when the run completes (normal
// or cancelled). On error, no proxies are running and the returned
// bundle is nil — defer Shutdown is safe but a no-op.
func startProxiesForSandbox(hostVethIP string, resolvedCreds map[string]string) (*runProxies, []string, error) {
	if hostVethIP == "" {
		return nil, nil, errors.New("agentproc: startProxiesForSandbox: hostVethIP is required")
	}

	cfg, err := proxyConfigFromCreds(resolvedCreds)
	if err != nil {
		return nil, nil, err
	}

	// Mint the per-run token that authenticates this sandbox to its own
	// proxy (SKY-395 Part A). It's both the value the proxy checks
	// (IncomingToken) and the credential we inject into the sandbox
	// below, so a sibling run — which holds a different token — cannot
	// spend this run's key even if it reaches this proxy over the
	// shared host namespace.
	token, err := newSandboxProxyToken(cfg.providerKind)
	if err != nil {
		return nil, nil, err
	}
	cfg.llm.IncomingToken = token

	bundle := &runProxies{}

	llm, err := llmproxy.New(cfg.llm)
	if err != nil {
		return nil, nil, fmt.Errorf("agentproc: construct llm proxy: %w", err)
	}
	// Port 0 — let the kernel pick a free port. We don't need a
	// predictable port (the env var carries the actual address into
	// the sandbox), and a random port avoids collision when multiple
	// runs share a subnet pool.
	addr, err := llm.Start(net.JoinHostPort(hostVethIP, "0"))
	if err != nil {
		// llmproxy.Start failed before any listener was set; nothing
		// to clean up. Return a clean error so the caller doesn't try
		// to Shutdown a half-constructed server.
		return nil, nil, fmt.Errorf("agentproc: start llm proxy on %s: %w", hostVethIP, err)
	}
	bundle.llm = llm

	// http:// (not https://) on the local hop — the agent talks
	// cleartext to the proxy across the veth pair, the proxy talks
	// TLS to the real upstream. The agent-side hop is bounded by the
	// netns + iptables / gVisor egress isolation, so no cleartext
	// credential ever crosses an exposed network boundary (the
	// placeholder is what the agent sends, not the real key).
	llmURL := "http://" + addr

	env := buildSandboxProxyEnv(cfg, llmURL, token)

	// Git proxy slot: the sibling ticket (per SKY-335's body) will
	// spawn a second proxy on a different port of hostVethIP and
	// inject http.proxy git config into the worktree's .git/config.
	// Until then, multi-mode agents that try to push will fail at the
	// git-clone or git-push step because no proxy is listening — that
	// is the intended interim state (multi mode is not user-facing
	// yet; the gate is the parent SKY-242 epic). The SKY-322
	// credential resolver exposes the GitHub PAT via the org's vault;
	// the future git proxy will consume it the same way startProxies
	// consumes the Anthropic key here.

	return bundle, env, nil
}

// sandboxProxyConfig collects the parsed proxy-side configuration the
// resolver implies. Internal — not exported because the only consumer
// is startProxiesForSandbox in this file.
type sandboxProxyConfig struct {
	llm llmproxy.Config
	// One of "anthropic" or "bedrock_bearer". Drives the placeholder
	// env shape (different env vars for each provider).
	providerKind llmproxy.Provider
}

// proxyConfigFromCreds maps a resolveCredentials output to the
// llmproxy.Config + provider kind. The mapping mirrors the resolver's
// precedence order: Anthropic key wins over Bedrock; Bedrock bearer
// wins over the AWS triple.
//
// AWS triple without bearer is rejected with
// ErrUnsupportedSandboxCredentials — the Phase 1 proxy doesn't
// implement SigV4 re-signing. An org configured this way can't run
// in multi mode until the Phase 2 SigV4 proxy lands. Admin UX:
// surface this as "switch to Bedrock API key or Anthropic direct".
func proxyConfigFromCreds(creds map[string]string) (sandboxProxyConfig, error) {
	if apiKey := creds["ANTHROPIC_API_KEY"]; apiKey != "" {
		// Anthropic direct (or org-gateway) path. The org may have
		// overridden ANTHROPIC_BASE_URL — point the proxy upstream at
		// the gateway instead of api.anthropic.com so the gateway's
		// own auth / quota / logging is preserved. Default to the
		// public Anthropic endpoint when no override.
		upstream := strings.TrimRight(creds["ANTHROPIC_BASE_URL"], "/")
		if upstream == "" {
			upstream = "https://api.anthropic.com"
		}
		if err := validateProxyUpstream(upstream); err != nil {
			return sandboxProxyConfig{}, fmt.Errorf("anthropic upstream: %w", err)
		}
		return sandboxProxyConfig{
			llm: llmproxy.Config{
				Provider:         llmproxy.ProviderAnthropic,
				APIKey:           apiKey,
				Upstream:         upstream,
				AllowNonLoopback: true,
			},
			providerKind: llmproxy.ProviderAnthropic,
		}, nil
	}

	if bearer := creds["AWS_BEARER_TOKEN_BEDROCK"]; bearer != "" {
		// Bedrock bearer path. Region is required for the upstream
		// URL — the Bedrock endpoint is regional. The resolver
		// already injected AWS_REGION when configured; default to
		// us-east-1 (Bedrock's primary region for Anthropic models)
		// when missing rather than refusing — matches the SDK's own
		// region resolution fallback.
		region := creds["AWS_REGION"]
		if region == "" {
			region = "us-east-1"
		}
		upstream := fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", region)
		if err := validateProxyUpstream(upstream); err != nil {
			return sandboxProxyConfig{}, fmt.Errorf("bedrock upstream: %w", err)
		}
		return sandboxProxyConfig{
			llm: llmproxy.Config{
				Provider:         llmproxy.ProviderBedrockBearer,
				APIKey:           bearer,
				Upstream:         upstream,
				AllowNonLoopback: true,
			},
			providerKind: llmproxy.ProviderBedrockBearer,
		}, nil
	}

	if creds["AWS_ACCESS_KEY_ID"] != "" || creds["AWS_SECRET_ACCESS_KEY"] != "" {
		// The resolver only returns the AWS triple when both halves
		// are present; checking either is sufficient to detect the
		// triple-without-bearer case here.
		return sandboxProxyConfig{}, fmt.Errorf("%w: AWS access-key triple requires SigV4 proxy (Phase 2); configure aws_bearer_token_bedrock or anthropic_api_key instead", ErrUnsupportedSandboxCredentials)
	}

	return sandboxProxyConfig{}, fmt.Errorf("%w: empty credentials map", ErrUnsupportedSandboxCredentials)
}

// buildSandboxProxyEnv constructs the env entries the sandbox sees:
// proxy URLs + the per-run proxy token as the credential value.
// Provider-shaped so the agent SDK reads the right vars —
// ANTHROPIC_BASE_URL for direct, ANTHROPIC_BEDROCK_BASE_URL +
// CLAUDE_CODE_USE_BEDROCK for Bedrock. incomingToken is the value the
// SDK forwards as x-api-key / "Authorization: Bearer", which the proxy
// authenticates against (SKY-395 Part A).
//
// Property B invariant: every value here is a URL, a provider-selection
// toggle, or the per-run token — a capability scoped to this run's own
// proxy, not a real provider credential. No real secret material
// crosses into the sandbox env; the real key lives only in the proxy.
func buildSandboxProxyEnv(cfg sandboxProxyConfig, llmURL, incomingToken string) []string {
	switch cfg.providerKind {
	case llmproxy.ProviderAnthropic:
		return []string{
			"ANTHROPIC_BASE_URL=" + llmURL,
			"ANTHROPIC_API_KEY=" + incomingToken,
		}
	case llmproxy.ProviderBedrockBearer:
		return []string{
			"ANTHROPIC_BEDROCK_BASE_URL=" + llmURL,
			"AWS_BEARER_TOKEN_BEDROCK=" + incomingToken,
			"CLAUDE_CODE_USE_BEDROCK=1",
		}
	}
	return nil
}

// validateProxyUpstream is a pre-flight check that mirrors the
// llmproxy.New validation. Done here so a malformed org-configured
// ANTHROPIC_BASE_URL surfaces at proxy-config time (before any
// listener opens) with a message that names "anthropic upstream"
// rather than the generic "llmproxy: parse upstream URL" error from
// inside the proxy package. Keep the rules in lockstep with
// llmproxy.New: any rule added there must be added here, or
// org-configured gateway URLs will pass this check and fail later
// with a less debuggable error.
func validateProxyUpstream(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("missing scheme or host: %q", raw)
	}
	// llmproxy.New forwards the incoming request path verbatim; an
	// upstream with a path component (other than "/") would route
	// requests to "/<path>/<request-path>" and 404 at the real
	// upstream. Reject here so the admin-facing error names which
	// proxy upstream is malformed.
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("must not include a path: %q", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must not include query or fragment: %q", raw)
	}
	// Cleartext-credential guard: the real key travels on the
	// proxy→upstream hop inside the rewritten auth header. http://
	// is only safe when the upstream is loopback (httptest pattern).
	if u.Scheme != "https" {
		host, _, _ := net.SplitHostPort(u.Host)
		if host == "" {
			host = u.Host
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("must use https (loopback http allowed for tests): %q", raw)
		}
	}
	return nil
}
