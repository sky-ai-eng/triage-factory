package agentproc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/egressproxy"
	"github.com/sky-ai-eng/triage-factory/internal/egressrelay"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/llmproxy"
)

// proxyTokenAnthropicPrefix shapes the per-run Anthropic token like a
// real key (sk-ant-…) so the agent SDK's client-side key-format check
// passes — the SDK requires a plausibly-shaped ANTHROPIC_API_KEY to
// construct a request at all. The body is fresh random per run; it is
// NOT a real key. See newSandboxProxyToken and the llmproxy package
// doc's trust-model section for why injecting a per-run token (rather
// than a constant placeholder) is what closes the cross-tenant
// hole: the proxy now authenticates the caller against this exact value.
const proxyTokenAnthropicPrefix = "sk-ant-"

// proxyTokenSigV4Prefix shapes the per-run SigV4 token like an AWS
// access-key ID for the same reason sk-ant- exists on the Anthropic
// path: the token is injected as the sandbox's AWS_ACCESS_KEY_ID, and a
// plausibly-shaped value survives any key-shape check a future SDK
// version might add. The AWS SDK signs with whatever ID it's given and
// the ID rides in cleartext in the Authorization header's Credential=
// scope, which is where the proxy's token gate reads it back.
const proxyTokenSigV4Prefix = "AKIA"

// sigV4PlaceholderSecret is the throwaway AWS_SECRET_ACCESS_KEY the
// sandbox signs with on the SigV4 path. It is NOT a secret and NOT
// per-run: the proxy never verifies the placeholder signature (caller
// auth is the access-key ID / per-run token), it exists only so the
// SDK constructs a Bedrock client and produces a request at all. The
// org's real secret key stays in the proxy and re-signs upstream.
const sigV4PlaceholderSecret = "tf-proxy-placeholder-not-a-credential"

// newSandboxProxyToken mints the fresh per-run secret that both
// authenticates the sandbox to its own proxy (set as
// llmproxy.Config.IncomingToken) and is injected into the sandbox as
// the credential the SDK sends. 32 bytes of crypto/rand, hex-encoded.
// For Anthropic it carries the sk-ant- prefix so the SDK's key-shape
// check passes; for Bedrock SigV4 it is uppercased behind an AKIA
// prefix so it reads as an access-key ID (that path injects it as
// AWS_ACCESS_KEY_ID); the Bedrock bearer path has no shape requirement.
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
	switch kind {
	case llmproxy.ProviderAnthropic:
		return proxyTokenAnthropicPrefix + tok, nil
	case llmproxy.ProviderBedrockSigV4:
		return proxyTokenSigV4Prefix + strings.ToUpper(tok), nil
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
// egress proxy, the gating package-registry egress proxy (TFAC-567),
// and, for runs with git egress (a GitHub repo in scope), the git
// credential proxy. Any may be nil — a prompt-only run (scorer /
// classifier) has no git proxy.
type runProxies struct {
	llm    *llmproxy.Server
	git    *gitproxy.Server
	egress *egressproxy.Server
	relays []*egressrelay.Server

	// llmEnv is the LLM proxy's address + per-run placeholder, in the
	// provider vocabulary the caller reads. Always populated; never folded
	// into the sandbox env — no jail is ever pointed at the LLM proxy. Read
	// via RunProxyHandle.LLMEnv by the native engine, which runs in the
	// executor process, not the jail.
	llmEnv []string

	// gitProxyURL / gitProxyToken are the git proxy's own address and per-run
	// placeholder token, surfaced so the orchestrator's pre-sandbox clone can
	// route through the SAME proxy (holding only the placeholder). Empty when
	// no git proxy was started. Read via RunProxyHandle.GitProxy.
	gitProxyURL   string
	gitProxyToken string
}

// Shutdown stops every proxy in the bundle. Returns errors.Join of
// every proxy's Shutdown error so all failures surface, not just the
// first.
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
	if p.egress != nil {
		if err := p.egress.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("egress proxy shutdown: %w", err))
		}
	}
	for _, rel := range p.relays {
		if err := rel.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s relay shutdown: %w", rel.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// ErrUnsupportedSandboxCredentials is returned when the resolved
// credentials don't map to a proxy-able shape: an empty map, or a
// partial AWS access-key pair (one half of the triple without the
// other — a malformed admin config the resolver normally filters out).
// Every complete credential shape the resolver produces now has a
// proxy path: Anthropic direct, Bedrock bearer, and Bedrock SigV4
// (the access-key triple, Phase 2). Surfaced as a typed error so the
// caller (delegate / scorer) can produce a clear admin-facing message
// rather than a confusing upstream auth error from inside the Node
// subprocess.
var ErrUnsupportedSandboxCredentials = errors.New("agentproc: resolved credentials shape not supported in sandbox mode")

// ErrNoGitCredentials is returned when a run that needs managed Git egress has
// no resolvable GitHub credential (no App installation and no PAT). The caller
// surfaces it before clone or push rather than falling back to ambient auth.
var ErrNoGitCredentials = errors.New("agentproc: no GitHub credential resolved for managed git egress")

// sigV4LiveSource re-reads the run's newest sealed LLM material for the
// SigV4 proxy — the executor's live-refresh channel (StartRunProxies'
// llmSource). Returns the newest bundle.LLM env map plus its
// expiry; nil means the proxy freezes the run-start snapshot (the default
// for bearer / anthropic, brain-side consumers, and local).
type sigV4LiveSource func(ctx context.Context) (env map[string]string, expiry time.Time, err error)

// GitProxyConfig is the per-run git-egress wiring the caller hands either the
// local loopback proxy or the multi-mode sidecar proxy.
type GitProxyConfig struct {
	// TokenSource resolves the host-side GitHub credential the proxy injects
	// on outbound git-over-HTTPS for ONE repo (owner/repo). Built over the
	// GitHub resolver's managed-Git mint. An App token is narrowed to exactly
	// the repo being touched while inheriting that installation's granted
	// permissions; a PAT org falls through unscoped (a PAT can't be narrowed).
	// Required when GitProxyConfig is non-nil. Lazy + cached per-repo inside the
	// proxy; a zero-expiry token (a PAT) mints once per repo.
	TokenSource gitproxy.TokenSource

	// Upstream is the real git host base — empty defaults to
	// the deployment default. Both the proxy's upstream and the insteadOf
	// rewrite prefix derive from it.
	Upstream string

	// RecordPush, when non-nil, is forwarded to the git proxy as its
	// receive-pack capture backstop: the proxy parses each branch push it
	// transits and calls this with the pushed ref so the caller can record a
	// branch artifact for a `git push --no-verify` that bypassed the pre-push
	// hook. The delegate wires it to the same record path the hook uses; nil
	// (a test fixture, or a caller that doesn't record) disables the backstop.
	// See gitproxy.Config.RecordPush and TFAC-467.
	RecordPush func(ctx context.Context, push gitproxy.PushedRef)

	// Authorize, when non-nil, is the per-request live authorization gate
	// (gitproxy.Config.Authorize): the proxy calls it with the (owner, repo)
	// the request targets and a !Allowed decision is a 403. Production delegates
	// always wire it (repo ∈ the run's tracked + materialized set,
	// with the per-repo allowed push refs); a nil here is allow-all (test).
	Authorize func(ctx context.Context, owner, repo string) (gitproxy.Decision, error)

	// RecordDenial, when non-nil, is the audit hook for a denied git op
	// (gitproxy.Config.RecordDenial). Best-effort; never blocks a request.
	RecordDenial func(ctx context.Context, denied gitproxy.DeniedGitOp)

	// SharedOriginHost, when non-empty, is the host of the run's single fake-GHE
	// origin — the address GH_HOST names, whose listener serves the GitHub API
	// and git smart-HTTP alike. Set it and the sandbox's git is routed at that
	// host instead of the git proxy's own address, which is what makes `gh`'s
	// repo inference work from a worktree: gh matches git remotes against
	// GH_HOST, and until both name the same host no repo-contextual gh command
	// can resolve without an explicit -R.
	//
	// Empty keeps the pre-existing routing (straight at the git proxy's own
	// http:// address) — the shape a run with no gh channel, or one whose shared
	// origin could not be bound, still gets.
	SharedOriginHost string

	// SharedOriginCAPath is the in-sandbox path of the shared origin's trust
	// file. Required alongside SharedOriginHost: that listener serves TLS with a
	// per-run self-signed cert, and git — unlike gh, which reads the
	// process-global SSL_CERT_FILE — needs the CA named in its own config for
	// the host it is talking to.
	SharedOriginCAPath string

	// ProbeCredentials, when non-nil, is the run-start credential check: it
	// resolves whether the org has ANY usable GitHub credential, returning
	// ErrNoGitCredentials if not, so a no-credential org surfaces a
	// clear admin failure at run start rather than a mid-push 502. Replaces
	// the old eager repo-less TokenSource probe (which no longer type-checks
	// now that TokenSource is per-repo).
	ProbeCredentials func(ctx context.Context) error
}

// GitProxyHandle is one live git credential proxy. It exposes only the
// non-secret coordinates a caller needs to route git through it; the real
// credential stays behind TokenSource inside the server.
type GitProxyHandle struct {
	srv      *gitproxy.Server
	url      string
	token    string
	upstream string
}

// StartGitProxy starts a per-run git proxy on bindIP. Local callers bind to
// loopback with allowNonLoopback=false; sandbox sidecars bind to their veth
// address and opt in explicitly.
func StartGitProxy(ctx context.Context, bindIP string, allowNonLoopback bool, git *GitProxyConfig) (*GitProxyHandle, error) {
	if git == nil || git.TokenSource == nil {
		return nil, errors.New("agentproc: GitProxyConfig.TokenSource is required")
	}
	if git.ProbeCredentials != nil {
		if err := git.ProbeCredentials(ctx); err != nil {
			return nil, fmt.Errorf("agentproc: resolve git credential: %w", err)
		}
	}

	incoming, err := randomHexToken()
	if err != nil {
		return nil, err
	}
	// An unset upstream is the deployment's default GitHub — the same answer
	// an org with no base URL resolves to everywhere else, so the git proxy
	// and the API client cannot land on different hosts for one org.
	upstream := git.Upstream
	if upstream == "" {
		upstream = ghbase.DefaultBaseURL()
	}
	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource:      git.TokenSource,
		Upstream:         upstream,
		AllowNonLoopback: allowNonLoopback,
		IncomingToken:    incoming,
		RecordPush:       git.RecordPush,
		Authorize:        git.Authorize,
		RecordDenial:     git.RecordDenial,
	})
	if err != nil {
		return nil, fmt.Errorf("agentproc: construct git proxy: %w", err)
	}
	addr, err := srv.Start(net.JoinHostPort(bindIP, "0"))
	if err != nil {
		return nil, fmt.Errorf("agentproc: start git proxy on %s: %w", bindIP, err)
	}
	return &GitProxyHandle{
		srv:      srv,
		url:      "http://" + addr,
		token:    incoming,
		upstream: upstream,
	}, nil
}

// Coordinates returns the proxy URL and its per-run placeholder token.
func (h *GitProxyHandle) Coordinates() (url, token string) {
	if h == nil {
		return "", ""
	}
	return h.url, h.token
}

// Handler returns the same gated handler served by the standalone listener.
// A gh injector mounts it behind the shared fake-GHE TLS origin.
func (h *GitProxyHandle) Handler() http.Handler {
	if h == nil || h.srv == nil {
		return nil
	}
	return h.srv.Handler()
}

// GitConfigPairs returns the process-scoped git settings that route GitHub's
// HTTPS and common SSH-shaped remotes through this proxy. When sharedHost and
// sharedCAPath are both present, that shared fake-GHE origin is used so gh can
// infer the repository from git's rewritten remote.
func (h *GitProxyHandle) GitConfigPairs(sharedHost, sharedCAPath string) [][2]string {
	if h == nil {
		return nil
	}
	return gitProxyPairs(h.url, h.upstream, h.token, sharedHost, sharedCAPath, false)
}

// GitConfigPairsWithSSH includes rewrites for GitHub's SCP-like and ssh://
// remote forms. Local mode uses it because existing worktrees may carry the
// operator-selected SSH form even though the managed channel speaks HTTPS.
func (h *GitProxyHandle) GitConfigPairsWithSSH(sharedHost, sharedCAPath string) [][2]string {
	if h == nil {
		return nil
	}
	return gitProxyPairs(h.url, h.upstream, h.token, sharedHost, sharedCAPath, true)
}

// UpstreamHost returns the bare hostname of the git host this proxy relays to.
// The local run's GIT_SSH_COMMAND dispatcher decides on it: an SSH-shaped
// session for this host belongs on this proxy, one for any other host does
// not. Empty when the upstream cannot be parsed, which leaves the dispatcher
// with no host to match and every session passing through to real ssh.
func (h *GitProxyHandle) UpstreamHost() string {
	if h == nil {
		return ""
	}
	u, err := url.Parse(h.upstream)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// Shutdown drains the proxy. Safe on a nil handle.
func (h *GitProxyHandle) Shutdown(ctx context.Context) error {
	if h == nil || h.srv == nil {
		return nil
	}
	err := h.srv.Shutdown(ctx)
	h.srv = nil
	return err
}

// RunProxyHandle is an opaque, shutdown-only handle to a run's started
// proxies — the exported face of the in-process runProxies bundle. The
// per-run credential sidecar (which holds the unsealed bundle) holds one of
// these and tears it down when the run ends; process exit frees the proxies'
// address space regardless, but an explicit Shutdown drains in-flight
// connections cleanly.
type RunProxyHandle struct {
	p *runProxies
}

// Shutdown stops every proxy the handle owns. Safe on a nil handle.
func (h *RunProxyHandle) Shutdown(ctx context.Context) error {
	if h == nil {
		return nil
	}
	return h.p.Shutdown(ctx)
}

// GitProxy returns the git proxy's address and per-run placeholder token, or
// ("", "") when this run started no git proxy. The sidecar surfaces these to
// the orchestrator so its pre-sandbox clone routes through the same proxy while
// holding only the placeholder — the real token never leaves the sidecar.
func (h *RunProxyHandle) GitProxy() (url, token string) {
	if h == nil || h.p == nil {
		return "", ""
	}
	return h.p.gitProxyURL, h.p.gitProxyToken
}

// LLMEnv returns the LLM proxy's address and per-run placeholder in the
// provider vocabulary, for a caller whose model calls originate outside the
// jail. Non-secret: the placeholder authenticates the caller to this run's own
// proxy and the real key is injected on the upstream hop, so holding these is
// not holding a provider credential.
//
// Returned regardless of whether the same entries were given to the sandbox —
// the two are different questions, and conflating them is what made a jail
// that never calls a provider carry a provider address anyway.
func (h *RunProxyHandle) LLMEnv() []string {
	if h == nil || h.p == nil {
		return nil
	}
	return h.p.llmEnv
}

// GitHandler returns the git proxy's own http.Handler, or nil when this run
// started no git proxy. The sidecar mounts it behind the gh injector's TLS
// listener so one address — the run's shared fake-GHE origin — serves both the
// API and git, the way a real GHE host does.
//
// It is the SAME handler the standalone git-proxy listener serves, deliberately:
// the ref gate, the base-branch push policy, the per-repo authorize relay, and
// the push capture are gitproxy's, and re-homing the handler behind a second
// front door must not fork any of them. Nothing about the credential changes
// either — the real token is still resolved inside the proxy on the upstream
// hop, whichever door the request came through.
func (h *RunProxyHandle) GitHandler() http.Handler {
	if h == nil || h.p == nil || h.p.git == nil {
		return nil
	}
	return h.p.git.Handler()
}

// StartRunProxies is the sidecar-callable entry to the same per-run proxy
// bring-up the in-process path uses (startProxiesForSandbox): it binds the
// LLM + egress proxies (and the git credential proxy when git is non-nil) on
// hostVethIP, holding the real credentials host-side, and returns the
// non-secret sandbox env naming them. Exported so the per-run credential
// sidecar runs the proxies out of the orchestrator's address space — the
// orchestrator relays the env back and never holds a real key. The git
// proxy's TokenSource reads the sidecar's unsealed bundle; its Authorize /
// RecordDenial closures relay the DB-backed decision to the orchestrator, so
// no database handle enters the capless sidecar. See startProxiesForSandbox
// for the full contract.
//
// gh names this run's GitHub hosts for the egress proxy's denial guidance —
// derive it from the run's upstreams with egressproxy.GitHubHostsForUpstreams;
// the zero value reads as the public github.com family.
//
// recordEgressDenial is the egress proxy's audit hook, the exact counterpart of
// git.RecordDenial one layer down: the sidecar relays each refused CONNECT to
// the orchestrator, which writes the row. nil disables egress denial recording
// (the slog line remains) — but the production sidecar always supplies it, and
// a test asserts as much, because this hook shipped unwired once already.
func StartRunProxies(ctx context.Context, hostVethIP string, resolvedCreds map[string]string, git *GitProxyConfig, gh egressproxy.GitHubHosts, recordEgressDenial func(ctx context.Context, denied egressproxy.DeniedConnect), llmSource func(ctx context.Context) (env map[string]string, expiry time.Time, err error), identityPairs ...[2]string) (*RunProxyHandle, []string, error) {
	bundle, env, err := startProxiesForSandbox(ctx, hostVethIP, resolvedCreds, git, gh, recordEgressDenial, sigV4LiveSource(llmSource), identityPairs...)
	if err != nil {
		return nil, nil, err
	}
	return &RunProxyHandle{p: bundle}, env, nil
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
// The proxy is bound unconditionally (it is where the real key lives, and
// the veth address is reachable from the host as well as from the jail), but
// the returned sandbox env never names it — no jail is ever pointed at a
// provider. The caller reads the coordinates off RunProxyHandle.LLMEnv
// instead: the native engine, which makes model calls from the executor
// process, not the jail.
//
// ctx scopes the eager git-credential probe (it surfaces a
// no-credentials condition as ErrNoGitCredentials before the
// run proceeds, rather than as a 502 mid-push); it does not bound the
// proxies' own lifetime, which the caller owns via Shutdown.
//
// identityPairs (variadic) are the org commit identity's git config pairs
// (user.name/user.email, via githooks.IdentityConfigPairs) — folded into the
// single GIT_CONFIG_* block after core.hooksPath so the agent's commits are
// authored by the org identity in the sandbox too (TFAC-452). Empty → the block
// carries core.hooksPath (+ proxy pairs) alone, unchanged. Variadic keeps the
// many test callers that pass no identity compiling untouched.
//
// Caller MUST call returned.Shutdown when the run completes (normal
// or cancelled). On error, no proxies are running and the returned
// bundle is nil — defer Shutdown is safe but a no-op.
func startProxiesForSandbox(ctx context.Context, hostVethIP string, resolvedCreds map[string]string, git *GitProxyConfig, gh egressproxy.GitHubHosts, recordEgressDenial func(ctx context.Context, denied egressproxy.DeniedConnect), llmSource sigV4LiveSource, identityPairs ...[2]string) (*runProxies, []string, error) {
	if hostVethIP == "" {
		return nil, nil, errors.New("agentproc: startProxiesForSandbox: hostVethIP is required")
	}

	cfg, err := proxyConfigFromCreds(resolvedCreds)
	if err != nil {
		return nil, nil, err
	}

	// Executor live-refresh (TFAC-616): when a SigV4 run supplies a live
	// source, the proxy re-reads the newest sealed bundle's triple per
	// request instead of freezing the run-start snapshot — so a role-mode
	// run whose STS session creds are re-minted mid-run keeps signing. The
	// run-start snapshot still determines provider / region / upstream (all
	// stable across mints); only the signing triple goes live. llmproxy
	// rejects both a static triple and a source, so the static one is
	// cleared here.
	if cfg.providerKind == llmproxy.ProviderBedrockSigV4 && llmSource != nil {
		src := llmSource
		cfg.llm.SigV4Credentials = nil
		cfg.llm.SigV4CredentialSource = func(ctx context.Context) (llmproxy.SigV4Material, error) {
			env, expiry, err := src(ctx)
			if err != nil {
				return llmproxy.SigV4Material{}, err
			}
			return llmproxy.SigV4Material{
				SigV4Credentials: llmproxy.SigV4Credentials{
					AccessKeyID:     env["AWS_ACCESS_KEY_ID"],
					SecretAccessKey: env["AWS_SECRET_ACCESS_KEY"],
					SessionToken:    env["AWS_SESSION_TOKEN"],
					Region:          bedrockRegion(env),
				},
				Expiry: expiry,
			}, nil
		}
	}

	// Mint the per-run token that authenticates this sandbox to its own
	// proxy. It's both the value the proxy checks
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

	// The handle always carries the LLM coordinates, because whoever asked
	// for this proxy is calling a provider through it from somewhere — the
	// native engine, in the executor process. The SANDBOX env never carries
	// them: no jail is ever pointed at a provider, so a jail is given no LLM
	// channel at all — nothing to dial, nothing to authenticate with — which
	// is a strictly smaller cell than the one Property B already guaranteed.
	bundle.llmEnv = buildSandboxProxyEnv(cfg, llmURL, token)

	var env []string

	// Gating egress proxy (TFAC-567): the CONNECT-only door to public
	// package registries, so `pnpm install` / `go mod download` /
	// `pip install` work inside the L3-locked sandbox. Started for
	// every sandbox run — prompt-only runs never dial it, and one idle
	// listener is cheaper than a second wiring path.
	egressEnv, egressSrv, err := startEgressProxyForSandbox(hostVethIP, gh, recordEgressDenial)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bundle.Shutdown(shutdownCtx)
		return nil, nil, err
	}
	bundle.egress = egressSrv
	env = append(env, egressEnv...)

	// Fetch relays: one per egressrelay catalog entry, each on its own
	// port of hostVethIP. The relays exist for registries that serve
	// artifacts via redirects into shared multi-tenant storage hostnames
	// (proxy.golang.org → storage.googleapis.com), which the CONNECT-only
	// egress proxy above can never admit — see internal/egressrelay's
	// package doc and CLAUDE.md. Which ecosystems get a relay, their route
	// tables, and their env pointing are all catalog data; this wiring is
	// ecosystem-blind.
	relayEnv, relaySrvs, err := startFetchRelaysForSandbox(hostVethIP)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bundle.Shutdown(shutdownCtx)
		return nil, nil, err
	}
	bundle.relays = relaySrvs
	env = append(env, relayEnv...)

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
	// The org commit identity (user.name/email, TFAC-452) layers right after
	// core.hooksPath (index 0), with the proxy pairs after that. Empty
	// identityPairs leaves the block as hooks (+ proxy) only — unchanged.
	gitPairs = append(gitPairs, identityPairs...)

	// Git proxy: a second per-run proxy on its own port of hostVethIP
	// that holds the GitHub credential host-side and injects Basic auth
	// on outbound git-over-HTTPS. The sandbox git is pointed at it via
	// GIT_CONFIG env entries (insteadOf + extraHeader). Only wired for
	// runs with a repo in scope; prompt-only runs pass git=nil and skip
	// it (but still get core.hooksPath above).
	if git != nil {
		proxyPairs, gitProxyURL, gitProxyToken, gitSrv, gerr := startGitProxyForSandbox(ctx, hostVethIP, git)
		if gerr != nil {
			// The LLM proxy is already listening; tear it down so a git
			// failure doesn't leak it, then return a clean nil bundle.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = bundle.Shutdown(shutdownCtx)
			return nil, nil, gerr
		}
		bundle.git = gitSrv
		bundle.gitProxyURL, bundle.gitProxyToken = gitProxyURL, gitProxyToken
		gitPairs = append(gitPairs, proxyPairs...)
		// The proxy owns push capture for this run: it observes each push's
		// actual upstream outcome (artifact on 2xx, audit failure row
		// otherwise), so the pre-push hook — which fires before the transfer
		// and can't know the outcome — stands down.
		env = append(env, githooks.PushCaptureEnvVar+"="+githooks.PushCaptureProxy)
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
// org as ErrNoGitCredentials at run start (a clear admin-facing
// failure) instead of a confusing 502 the first time the agent pushes.
// For an App-installation source the minted token is cached, so the
// proxy's own lazy resolve on first request reuses it at no extra mint;
// a PAT source pays one extra (cheap) secret-store read at run start.
func startGitProxyForSandbox(ctx context.Context, hostVethIP string, git *GitProxyConfig) ([][2]string, string, string, *gitproxy.Server, error) {
	h, err := StartGitProxy(ctx, hostVethIP, true, git)
	if err != nil {
		return nil, "", "", nil, err
	}
	proxyURL, incoming := h.Coordinates()
	// proxyURL + incoming are returned alongside the sandbox git-config pairs so
	// the orchestrator's own host-side clone can route through this SAME proxy
	// (CloneAuthViaGitProxy) holding only the placeholder — the real token stays
	// in the proxy's TokenSource. The host reaches the veth IP too, so one proxy
	// serves both the in-jail agent and the pre-sandbox clone.
	return h.GitConfigPairs(git.SharedOriginHost, git.SharedOriginCAPath), proxyURL, incoming, h.srv, nil
}

// startEgressProxyForSandbox mints a per-run secret, starts the gating
// package-registry egress proxy (TFAC-567) on a free port of
// hostVethIP, and returns the proxy env entries plus the server for
// the shutdown bundle.
//
// The allowlist is the compiled-in registry set (egressproxy.
// DefaultRegistryHosts); TFAC-408's sandbox profiles replace this
// constant with per-profile data resolved by the spawner — the wiring
// here doesn't change shape. The proxy enforces the operator denylist
// (metadata / private ranges / sandbox pool) against resolved IPs
// regardless of allowlist content.
//
// gh names this run's GitHub hosts so a denial aimed at them carries the
// sanctioned alternative in its reason; it never affects the allow decision.
//
// recordDenial is the caller's audit hook for each refused CONNECT. It is
// passed through rather than built here for the same reason the git proxy's is:
// the DB lives on the orchestrator and this runs in the capless sidecar, so the
// only thing this layer can do is carry the hook. nil leaves denials at the
// slog line the proxy always writes.
func startEgressProxyForSandbox(hostVethIP string, gh egressproxy.GitHubHosts, recordDenial func(ctx context.Context, denied egressproxy.DeniedConnect)) ([]string, *egressproxy.Server, error) {
	incoming, err := randomHexToken()
	if err != nil {
		return nil, nil, err
	}
	srv, err := egressproxy.New(egressproxy.Config{
		AllowedHosts:     egressproxy.DefaultRegistryHosts(),
		IncomingToken:    incoming,
		AllowNonLoopback: true,
		GitHub:           gh,
		RecordDenial:     recordDenial,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("agentproc: construct egress proxy: %w", err)
	}
	addr, err := srv.Start(net.JoinHostPort(hostVethIP, "0"))
	if err != nil {
		return nil, nil, fmt.Errorf("agentproc: start egress proxy on %s: %w", hostVethIP, err)
	}
	return sandboxEgressProxyEnv(addr, hostVethIP, incoming), srv, nil
}

// startFetchRelaysForSandbox starts every egressrelay catalog entry on a
// free port of hostVethIP and returns the combined env entries plus the
// servers for the shutdown bundle. Ecosystem-blind by design: adding a
// relay is a catalog edit (internal/egressrelay/catalog.go), never a
// change here.
//
// Every relay starts for every sandbox run, like the egress proxy: a run
// that never uses the ecosystem never dials it, and an idle listener is
// cheaper than a wiring path conditioned on guessing the repo's language.
//
// Unlike the LLM and git proxies, relays mint no per-run token. They hold
// no credential to steal, the relayed tools have no clean way to present
// one, and cross-run reach is already denied at L3 — see the egressrelay
// package doc.
//
// Self-contained on failure: any relays already started are shut down
// here, so the caller never receives a half-started set alongside an
// error.
func startFetchRelaysForSandbox(hostVethIP string) ([]string, []*egressrelay.Server, error) {
	var env []string
	var servers []*egressrelay.Server
	fail := func(err error) ([]string, []*egressrelay.Server, error) {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, srv := range servers {
			_ = srv.Shutdown(shutdownCtx)
		}
		return nil, nil, err
	}
	for _, entry := range egressrelay.Catalog() {
		cfg := entry.Config
		cfg.AllowNonLoopback = true
		srv, err := egressrelay.New(cfg)
		if err != nil {
			return fail(fmt.Errorf("agentproc: construct %s relay: %w", entry.Config.Name, err))
		}
		addr, err := srv.Start(net.JoinHostPort(hostVethIP, "0"))
		if err != nil {
			return fail(fmt.Errorf("agentproc: start %s relay on %s: %w", entry.Config.Name, hostVethIP, err))
		}
		servers = append(servers, srv)
		env = append(env, entry.Env(addr)...)
	}
	return env, servers, nil
}

// sandboxEgressProxyEnv builds the proxy env entries that route the
// sandbox's package managers through the gating egress proxy. The
// per-run token travels as proxy-URL userinfo — curl, git, npm, pnpm,
// pip, and Go's net/http all turn it into Proxy-Authorization on the
// CONNECT. Both spellings of each var: curl reads only the lowercase
// forms, Go and npm read either, and tools disagree just enough that
// setting all four is the only portable choice.
//
// NO_PROXY is load-bearing, not cosmetic: the sandbox's git is routed at the
// gateway by url.<base>/.insteadOf rewrites — the run's shared origin, or the
// git proxy's own address — and git (libcurl) honors http_proxy/https_proxy for
// both schemes. Without the gateway-IP exemption every sandbox git fetch/push
// would try to CONNECT through the egress proxy and be refused, and multi-mode
// git would break. The exemption is by HOST, so it covers whichever port the
// gateway's listeners land on. It also keeps ANTHROPIC_BASE_URL traffic direct
// should the SDK ever grow proxy support.
//
// Property B: the only secret-shaped value is the per-run token, a
// capability scoped to this run's own egress proxy — same class as
// the LLM and git tokens.
func sandboxEgressProxyEnv(proxyAddr, hostVethIP, incomingToken string) []string {
	proxyURL := "http://" + egressproxy.BasicUser + ":" + incomingToken + "@" + proxyAddr
	noProxy := hostVethIP + ",localhost,127.0.0.1"
	return []string{
		"HTTPS_PROXY=" + proxyURL,
		"https_proxy=" + proxyURL,
		"HTTP_PROXY=" + proxyURL,
		"http_proxy=" + proxyURL,
		"NO_PROXY=" + noProxy,
		"no_proxy=" + noProxy,
	}
}

// sandboxProxyConfig collects the parsed proxy-side configuration the
// resolver implies. Internal — not exported because the only consumer
// is startProxiesForSandbox in this file.
type sandboxProxyConfig struct {
	llm llmproxy.Config
	// One of "anthropic", "bedrock_bearer", or "bedrock_sigv4". Drives
	// the placeholder env shape (different env vars for each provider).
	providerKind llmproxy.Provider
}

// proxyConfigFromCreds maps a resolveCredentials output to the
// llmproxy.Config + provider kind.
//
// The map carries exactly one provider's material — resolution selects a
// provider from the run's model before rendering it — so the order below is
// which key it looks for first, not a choice between two live credentials.
// Within Bedrock it is a real order: the bearer wins over the AWS triple, and
// the triple maps to the SigV4 re-signing provider.
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
		// Bedrock bearer path. Upstream comes from the org's endpoint
		// override when configured, else the standard regional formula —
		// see bedrockUpstream.
		upstream := bedrockUpstream(creds)
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

	if accessKey, secretKey := creds["AWS_ACCESS_KEY_ID"], creds["AWS_SECRET_ACCESS_KEY"]; accessKey != "" && secretKey != "" {
		// Bedrock SigV4 path (Phase 2): the proxy holds the real triple
		// and re-signs each request; the sandbox signs with a throwaway
		// placeholder. Upstream honors the endpoint override like the
		// bearer path, but the SIGNING SCOPE always comes from the
		// region config: a VPC interface endpoint's hostname embeds the
		// region it fronts and GovCloud regions look nothing like the
		// formula's, so the scope region cannot be parsed from the URL —
		// AWS validates the scope against the endpoint's real region and
		// 403s a mismatch, which is the operator's signal that
		// aws_region and bedrock_base_url disagree.
		region := bedrockRegion(creds)
		upstream := bedrockUpstream(creds)
		if err := validateProxyUpstream(upstream); err != nil {
			return sandboxProxyConfig{}, fmt.Errorf("bedrock sigv4 upstream: %w", err)
		}
		return sandboxProxyConfig{
			llm: llmproxy.Config{
				Provider:         llmproxy.ProviderBedrockSigV4,
				Upstream:         upstream,
				AllowNonLoopback: true,
				SigV4Credentials: &llmproxy.SigV4Credentials{
					AccessKeyID:     accessKey,
					SecretAccessKey: secretKey,
					SessionToken:    creds["AWS_SESSION_TOKEN"],
					Region:          region,
				},
			},
			providerKind: llmproxy.ProviderBedrockSigV4,
		}, nil
	}

	if creds["AWS_ACCESS_KEY_ID"] != "" || creds["AWS_SECRET_ACCESS_KEY"] != "" {
		// Defensive: the resolver only emits the triple when both halves
		// are present, so a lone half means a malformed caller-built map.
		return sandboxProxyConfig{}, fmt.Errorf("%w: partial AWS access-key pair (need both aws_access_key_id and aws_secret_access_key)", ErrUnsupportedSandboxCredentials)
	}

	return sandboxProxyConfig{}, fmt.Errorf("%w: empty credentials map", ErrUnsupportedSandboxCredentials)
}

// bedrockRegion resolves the region both Bedrock proxy paths use: the
// resolver-injected AWS_REGION when configured, else us-east-1
// (Bedrock's primary region for Anthropic models — matches the SDK's
// own region-resolution fallback).
func bedrockRegion(creds map[string]string) string {
	if region := creds["AWS_REGION"]; region != "" {
		return region
	}
	return "us-east-1"
}

// bedrockUpstream resolves the Bedrock endpoint the proxy forwards to.
// The org's endpoint override (the bedrock_base_url secret, surfaced by
// the resolver as ANTHROPIC_BEDROCK_BASE_URL) wins when set — that is
// how VPC interface endpoints (PrivateLink), GovCloud, and the China
// partition are reached, since their hostnames don't follow the public
// regional formula. Otherwise the standard
// https://bedrock-runtime.<region>.amazonaws.com is derived from the
// region. Trailing slash is stripped to satisfy the proxy's no-path
// upstream rule, mirroring the ANTHROPIC_BASE_URL gateway handling.
func bedrockUpstream(creds map[string]string) string {
	if override := strings.TrimRight(creds["ANTHROPIC_BEDROCK_BASE_URL"], "/"); override != "" {
		return override
	}
	return fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", bedrockRegion(creds))
}

// buildSandboxProxyEnv constructs the env entries the sandbox sees:
// proxy URLs + the per-run proxy token as the credential value.
// Provider-shaped so the agent SDK reads the right vars —
// ANTHROPIC_BASE_URL for direct, ANTHROPIC_BEDROCK_BASE_URL +
// CLAUDE_CODE_USE_BEDROCK for Bedrock. incomingToken is the value the
// SDK forwards as x-api-key / "Authorization: Bearer", which the proxy
// authenticates against.
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
	case llmproxy.ProviderBedrockSigV4:
		// The per-run token IS the placeholder access-key ID: the SDK
		// signs with it, it rides in cleartext in the Authorization
		// header's Credential= scope, and the proxy authenticates the
		// caller against it (llmproxy.sigV4AccessKeyID). The secret half
		// is a constant throwaway — the placeholder signature is never
		// verified, and the proxy discards it wholesale when re-signing
		// with the org's real triple. AWS_REGION keeps the sandbox SDK's
		// client construction and the proxy's signing scope in agreement.
		env := []string{
			"ANTHROPIC_BEDROCK_BASE_URL=" + llmURL,
			"AWS_ACCESS_KEY_ID=" + incomingToken,
			"AWS_SECRET_ACCESS_KEY=" + sigV4PlaceholderSecret,
			"CLAUDE_CODE_USE_BEDROCK=1",
		}
		if cfg.llm.SigV4Credentials != nil {
			env = append(env, "AWS_REGION="+cfg.llm.SigV4Credentials.Region)
		}
		return env
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
//   - url.<base>/.insteadOf=<upstream>/ rewrites every git URL the
//     sandbox resolves under the upstream host to the proxy's address,
//     so native git push/fetch transit the proxy instead of trying (and
//     failing, under the egress allowlist) to reach the host directly.
//   - http.<base>/.extraHeader carries the per-run token as the
//     Basic password — the value the proxy authenticates against before
//     swapping in the real credential. Mirrors the base64("user:"+token)
//     encoding internal/worktree uses for the host-side clone.
//
// <base> is the run's SHARED ORIGIN when it has one — the single fake-GHE
// address GH_HOST also names — and the git proxy's own http:// address
// otherwise. That choice is what repoints the agent's `origin`: git applies
// insteadOf when it resolves a remote, so `git remote -v` (which is what gh
// reads) reports the rewritten URL, and pointing it at the shared origin is
// what lets gh's inference match GH_HOST from inside a worktree. On the shared
// origin the listener is TLS with a per-run self-signed cert, so a third pair
// names the CA: gh finds it through the process-global SSL_CERT_FILE, but git
// resolves TLS trust from its own config.
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
// credential, which stays in the proxy on the host. Both routings reach the
// same gitproxy handler, so the ref gate and the base-branch push policy are
// identical either way.
func sandboxGitProxyPairs(proxyURL, upstream, incomingToken, sharedOriginHost, sharedOriginCAPath string) [][2]string {
	return gitProxyPairs(proxyURL, upstream, incomingToken, sharedOriginHost, sharedOriginCAPath, false)
}

func gitProxyPairs(proxyURL, upstream, incomingToken, sharedOriginHost, sharedOriginCAPath string, includeSSH bool) [][2]string {
	// Trailing slash on both the rewritten base and the matched prefix
	// so "<upstream>/owner/repo" maps cleanly to "<base>/owner/repo".
	base := strings.TrimRight(proxyURL, "/") + "/"
	upstreamPrefix := strings.TrimRight(upstream, "/") + "/"

	creds := gitProxyBasicUser + ":" + incomingToken
	extraHeader := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(creds))

	// The shared origin needs both halves to be usable — an address with no CA
	// would route git at a TLS listener it cannot verify, which is worse than
	// not routing it there at all. Both-or-neither, so a half-wired caller
	// degrades to the proxy's own address rather than to a broken remote.
	if sharedOriginHost == "" || sharedOriginCAPath == "" {
		return appendGitProxyPairs(nil, base, upstreamPrefix, upstream, extraHeader, "", includeSSH)
	}
	base = "https://" + sharedOriginHost + "/"
	return appendGitProxyPairs(nil, base, upstreamPrefix, upstream, extraHeader, sharedOriginCAPath, includeSSH)
}

func appendGitProxyPairs(pairs [][2]string, base, upstreamPrefix, upstream, extraHeader, caPath string, includeSSH bool) [][2]string {
	pairs = append(pairs, [2]string{"url." + base + ".insteadOf", upstreamPrefix})
	if u, err := url.Parse(upstream); includeSSH && err == nil && u.Host != "" {
		pairs = append(pairs,
			[2]string{"url." + base + ".insteadOf", "git@" + u.Host + ":"},
			[2]string{"url." + base + ".insteadOf", "ssh://git@" + u.Host + "/"},
		)
	}
	pairs = append(pairs, [2]string{"http." + base + ".extraHeader", extraHeader})
	if caPath != "" {
		pairs = append(pairs, [2]string{"http." + base + ".sslCAInfo", caPath})
	}
	return pairs
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
