package agentproc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
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
	tok, err := randomHexToken()
	if err != nil {
		return "", err
	}
	if kind == llmproxy.ProviderAnthropic {
		return proxyTokenAnthropicPrefix + tok, nil
	}
	return tok, nil
}

// randomHexToken mints 32 bytes of crypto/rand, hex-encoded — the raw
// per-run secret both proxy token minters build on. The git proxy uses
// it verbatim (the HTTPS Basic password has no key-shape requirement);
// newSandboxProxyToken prefixes it for the Anthropic SDK's key check.
func randomHexToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("agentproc: generate per-run proxy token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// runProxies bundles the per-run proxy handles for shutdown: the LLM
// egress proxy and, for runs with git egress (a GitHub repo in scope),
// the git credential proxy. Either may be nil — a prompt-only run
// (scorer / classifier) has no git proxy.
type runProxies struct {
	llm *llmproxy.Server
	git *gitproxy.Server
}

// Shutdown stops every proxy in the bundle. Returns errors.Join of
// every proxy's Shutdown error so both proxies' failures surface, not
// just the first. With a single proxy wired the result is either nil
// or one wrapped error.
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
	if p.git != nil {
		if err := p.git.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("git proxy shutdown: %w", err))
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

// ErrNoSandboxGitCredentials is returned when a run that needs git
// egress has no resolvable GitHub credential (no App installation, no
// PAT). Surfaced as a typed error — mirroring ErrUnsupportedSandboxCredentials
// — so the caller (delegate) produces a clear admin-facing message
// rather than letting the agent hit a confusing 502 the first time it
// pushes from inside the sandbox. The caller building the GitProxy
// TokenSource wraps the resolver's no-credentials sentinel in this.
var ErrNoSandboxGitCredentials = errors.New("agentproc: no GitHub credential resolved for sandbox git egress")

// defaultGitUpstream is the git-over-HTTPS host the git proxy forwards
// to, and the URL prefix the in-sandbox git is rewritten away from. GHES
// orgs override via GitProxyConfig.Upstream (the customer's responsibility
// per the single-installation scope).
const defaultGitUpstream = "https://github.com"

// GitProxyConfig is the per-run git-egress wiring the caller (delegate)
// hands startProxiesForSandbox for a run with a GitHub repo in scope.
// nil disables the git proxy entirely (prompt-only runs — scorer,
// classifier — and Jira-only runs that pre-clone nothing).
type GitProxyConfig struct {
	// TokenSource resolves the host-side GitHub credential the proxy
	// injects on outbound git-over-HTTPS. Built over the GitHub
	// resolver's TokenFor (App installation token tier-1 or org PAT
	// tier-3, App preferred) so App-and-PAT orgs both work with no proxy
	// change. Required when GitProxyConfig is non-nil. Lazy + cached
	// inside the proxy; a zero-expiry token (a PAT) mints exactly once.
	TokenSource gitproxy.TokenSource

	// Upstream is the real git host base — empty defaults to
	// defaultGitUpstream. Both the proxy's upstream and the insteadOf
	// rewrite prefix derive from it.
	Upstream string
}

// startProxiesForSandbox starts the per-run LLM proxy, plus the git
// credential proxy when git is non-nil, on hostVethIP. Returns the
// proxy bundle for shutdown plus the env entries the sandbox should
// inject so the agent reaches the proxies instead of the real upstreams.
//
// hostVethIP is the host-side veth address — the 10.42.<idx>.1 IP
// that the sandbox's netns can reach via its default route. Binding
// here (not loopback) is the whole point: the sandboxed agent sees
// only the netns and can't reach 127.0.0.1, so a loopback-bound proxy
// would be invisible to it.
//
// The resolvedCreds map is what resolveCredentials produced — the
// shape determines the LLM proxy's provider + upstream. See the switch
// below for the mapping.
//
// ctx scopes the eager git-credential probe (it surfaces a
// no-credentials condition as ErrNoSandboxGitCredentials before the
// run proceeds, rather than as a 502 mid-push); it does not bound the
// proxies' own lifetime, which the caller owns via Shutdown.
//
// Caller MUST call returned.Shutdown when the run completes (normal
// or cancelled). On error, no proxies are running and the returned
// bundle is nil — defer Shutdown is safe but a no-op.
func startProxiesForSandbox(ctx context.Context, hostVethIP string, resolvedCreds map[string]string, git *GitProxyConfig) (*runProxies, []string, error) {
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

	// Assemble the single GIT_CONFIG_* block for the sandboxed git. It
	// always carries core.hooksPath (F2, TFAC-456) so the TF hooks fire
	// for every repo the agent touches — including subdir clones in a
	// repo-less Jira run, which never reaches the git-proxy branch below.
	// The proxy pairs layer on when a repo is in scope. Consolidating into
	// one block keeps a single GIT_CONFIG_COUNT: the sandbox env merge
	// (internal/sandbox) appends this slice verbatim and the base sandbox
	// env carries no GIT_CONFIG, so there is exactly one count to read.
	gitPairs := [][2]string{
		{githooks.ConfigKey, githooks.SandboxDir},
	}

	// Git proxy: a second per-run proxy on its own port of hostVethIP
	// that holds the GitHub credential host-side and injects Basic auth
	// on outbound git-over-HTTPS. The sandbox git is pointed at it via
	// GIT_CONFIG env entries (insteadOf + extraHeader). Only wired for
	// runs with a repo in scope; prompt-only runs pass git=nil and skip
	// it (but still get core.hooksPath above).
	if git != nil {
		proxyPairs, gitSrv, gerr := startGitProxyForSandbox(ctx, hostVethIP, git)
		if gerr != nil {
			// The LLM proxy is already listening; tear it down so a git
			// failure doesn't leak it, then return a clean nil bundle.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = bundle.Shutdown(shutdownCtx)
			return nil, nil, gerr
		}
		bundle.git = gitSrv
		gitPairs = append(gitPairs, proxyPairs...)
	}

	env = append(env, encodeGitConfigEnv(gitPairs)...)

	return bundle, env, nil
}

// startGitProxyForSandbox mints the per-run git secret, starts the git
// credential proxy on a free port of hostVethIP, and returns the git
// config (key, value) pairs that route the sandbox git through it. Split
// from startProxiesForSandbox so the LLM and git paths read independently
// and the git failure path stays a single early return. The caller folds
// the returned pairs into the single GIT_CONFIG_* block alongside
// core.hooksPath.
//
// The eager TokenSource probe runs first: it surfaces a no-credentials
// org as ErrNoSandboxGitCredentials at run start (a clear admin-facing
// failure) instead of a confusing 502 the first time the agent pushes.
// For an App-installation source the minted token is cached, so the
// proxy's own lazy resolve on first request reuses it at no extra mint;
// a PAT source pays one extra (cheap) secret-store read at run start.
func startGitProxyForSandbox(ctx context.Context, hostVethIP string, git *GitProxyConfig) ([][2]string, *gitproxy.Server, error) {
	if git.TokenSource == nil {
		return nil, nil, errors.New("agentproc: GitProxyConfig.TokenSource is required")
	}

	if _, err := git.TokenSource(ctx); err != nil {
		// ErrNoSandboxGitCredentials (wrapped by the caller's TokenSource)
		// propagates as-is so the run fails with the typed admin message;
		// any other (transient) resolve error fails the run too rather
		// than spawning a proxy that can't authenticate.
		return nil, nil, fmt.Errorf("agentproc: resolve git credential for sandbox: %w", err)
	}

	incoming, err := randomHexToken()
	if err != nil {
		return nil, nil, err
	}

	upstream := git.Upstream
	if upstream == "" {
		upstream = defaultGitUpstream
	}

	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource:      git.TokenSource,
		Upstream:         upstream,
		AllowNonLoopback: true,
		IncomingToken:    incoming,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("agentproc: construct git proxy: %w", err)
	}
	addr, err := srv.Start(net.JoinHostPort(hostVethIP, "0"))
	if err != nil {
		return nil, nil, fmt.Errorf("agentproc: start git proxy on %s: %w", hostVethIP, err)
	}

	return sandboxGitProxyPairs("http://"+addr, upstream, incoming), srv, nil
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

// gitProxyBasicUser is the conventional username half of the per-run
// git Basic credential. The proxy ignores it and validates only the
// password (the per-run token), but git's "Basic base64(user:pass)"
// encoding needs a username, so we pin a stable sentinel.
const gitProxyBasicUser = "x-run"

// sandboxGitProxyPairs returns the git config (key, value) pairs that
// route the in-sandbox git through the per-run git proxy. Two settings,
// both host-side (the agent never sees the real GitHub credential):
//
//   - url.<proxyURL>/.insteadOf=<upstream>/ rewrites every git URL the
//     sandbox resolves under the upstream host to the proxy's address,
//     so native git push/fetch transit the proxy instead of trying (and
//     failing, under the egress allowlist) to reach the host directly.
//   - http.<proxyURL>/.extraHeader carries the per-run token as the
//     Basic password — the value the proxy authenticates against before
//     swapping in the real credential. Mirrors the base64("user:"+token)
//     encoding internal/worktree uses for the host-side clone.
//
// The caller folds these into the single GIT_CONFIG_* block (with
// core.hooksPath) via encodeGitConfigEnv. Delivered via env-config
// rather than a .git/config write because the bind-mounted worktree
// shares the bare clone's config; the env form scopes the routing to
// this one sandboxed git without touching shared on-disk state, and
// keeps the token out of argv. proxyURL is "http://host:port" (no
// trailing slash); upstream is the real git host base (no trailing
// slash).
//
// Property B: the only secret-shaped value is the per-run token, a
// capability scoped to this run's own proxy — never the real GitHub
// credential, which stays in the proxy on the host.
func sandboxGitProxyPairs(proxyURL, upstream, incomingToken string) [][2]string {
	// Trailing slash on both the rewritten base and the matched prefix
	// so "<upstream>/owner/repo" maps cleanly to "<proxy>/owner/repo".
	proxyBase := strings.TrimRight(proxyURL, "/") + "/"
	upstreamPrefix := strings.TrimRight(upstream, "/") + "/"

	creds := gitProxyBasicUser + ":" + incomingToken
	extraHeader := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(creds))

	return [][2]string{
		{"url." + proxyBase + ".insteadOf", upstreamPrefix},
		{"http." + proxyBase + ".extraHeader", extraHeader},
	}
}

// encodeGitConfigEnv encodes git config (key, value) pairs into git's
// indexed env-config form (GIT_CONFIG_COUNT + GIT_CONFIG_KEY_n /
// GIT_CONFIG_VALUE_n). GIT_CONFIG_COUNT is derived from the pair count
// rather than hardcoded: git stops reading at the declared count, so a
// literal that drifted from the entries would silently drop settings
// with no compile- or run-time error.
//
// This is the one GIT_CONFIG_* block the sandbox env carries — the base
// sandbox env sets none — so it owns GIT_CONFIG_COUNT outright and needs
// no next-free-index bookkeeping (unlike the local direct-spawn path,
// which layers over the inherited operator env).
func encodeGitConfigEnv(pairs [][2]string) []string {
	env := make([]string, 0, 1+2*len(pairs))
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(pairs)))
	for i, kv := range pairs {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]),
		)
	}
	return env
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
