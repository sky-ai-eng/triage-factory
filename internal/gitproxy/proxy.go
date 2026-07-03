// Package gitproxy is a per-run HTTP intermediary that holds a GitHub
// App installation access token on the trusted side and injects it as
// Basic auth on outbound git protocol requests.
//
// # The threat it addresses
//
// The same Property B problem the LLM proxy solves (see
// internal/llmproxy) applies to git auth: anything the sandboxed agent
// can read — env vars, .git/config, the worktree filesystem — is
// exfiltratable via prompt injection. A long-lived GitHub PAT in the
// sandbox is a tenant-wide credential one tool-output leak away from
// public exposure.
//
// This package keeps the credential on the host side, hands the agent
// only an unauthenticated proxy URL, and rewrites the Authorization
// header on each outbound request before forwarding to GitHub.
//
// # Credential class
//
// Phase 1 minted PATs straight from the user. This phase moves git
// auth onto GitHub App installation access tokens, which:
//
//   - Live 1 hour, not indefinitely. A leaked token has minutes of
//     useful life.
//   - Scope to one installation (one org's repo set), not the user's
//     entire access. A compromised proxy cannot reach a different
//     tenant's data.
//   - Mint on demand from the App's private key + installation ID via
//     internal/githubapp. The private key never crosses to the agent.
//
// # Auth-header shape
//
// GitHub's git-over-HTTPS protocol accepts installation tokens via the
// Basic-auth scheme with a fixed username:
//
//	Authorization: Basic base64("x-access-token:" + <token>)
//
// This is documented at
// docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation.
// The "x-access-token" string is the literal sentinel — it's not the
// installation's display name or anything similar.
//
// # Caching
//
// The proxy caches the token in memory for the lifetime of the Server.
// First request mints; subsequent requests reuse until the token is
// within refreshThreshold of its expires_at, at which point a fresh
// mint replaces it. Concurrent requests during refresh coalesce on a
// single mint call via the mutex — no thundering herd.
//
// The TokenSource need not be an App minter. A run's source resolves
// App-installation-token-or-PAT uniformly (App preferred), so an org
// with no App but a configured PAT works with no proxy change. A PAT
// has no mint lifetime the proxy tracks (zero ExpiresAt); the cache
// treats that as "never refresh", so a PAT source mints exactly once.
//
// A run's proxy is single-installation, so a single cached token
// suffices. Multi-installation orgs are out of scope for v1.
//
// # Trust model on the local hop
//
// Same as the LLM proxy. Loopback-only by default; non-loopback
// (sandbox veth IP) requires AllowNonLoopback=true. The local hop is
// unauthenticated because reaching the proxy means the caller is on
// the correct side of the sandbox boundary — that boundary is
// enforced by the gVisor netns + iptables, not by proxy-level auth.
package gitproxy

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// refreshThreshold is how close to expiry the cached token gets before
// the next request triggers a fresh mint. Installation tokens are
// 1-hour TTL; 5 minutes' headroom is comfortably more than the
// roundtrip + mint time and gives in-flight requests time to finish
// against the old token even if the clock skews.
const refreshThreshold = 5 * time.Minute

// defaultUpstream is the github.com hostname used by git-over-HTTPS.
// Different from the REST API base (api.github.com); both are
// configurable but they're not the same thing.
const defaultUpstream = "https://github.com"

// Token is the contract between the minter and the proxy. The proxy
// doesn't care how the token was obtained, only that it has a value
// and (optionally) an expiry. Compatible by-shape with githubapp.Token
// but typed separately so this package doesn't force the dependency on
// callers who want to plug in a different source (e.g. for tests).
//
// ExpiresAt zero means "no tracked lifetime": a PAT, which never
// expires from the proxy's point of view. The refresh logic treats a
// zero expiry as "never refresh" rather than "already expired", so a
// PAT source mints exactly once. A non-zero expiry (an App installation
// token, ~1h) drives the refresh-on-threshold path.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// TokenSource is the abstraction over "how to get a fresh installation
// token for ONE repo". owner/repo identify the repo the request targets;
// production callers wrap the resolver's per-repo scoped mint
// (TokenForRepoScoped), so the injected credential is narrowed to exactly
// that repo. Tests pass a stub returning a fixed value (ignoring owner/repo).
//
// The Server calls TokenSource lazily on first request for a repo, caches
// the result per owner/repo, and re-invokes when that repo's cached token is
// within refreshThreshold of expiry. Implementations should be safe for
// concurrent invocation, though the proxy serializes calls itself.
type TokenSource func(ctx context.Context, owner, repo string) (Token, error)

// Decision is the result of Config.Authorize for one (owner, repo): whether
// the run may touch the repo at all, and — for a push — the exact set of full
// refs (refs/heads/...) it may update. An empty AllowedRefs on an Allowed
// decision means "no push is permitted" (every receive-pack ref will be
// rejected); a fetch/advertise only needs Allowed.
type Decision struct {
	Allowed     bool
	AllowedRefs []string
}

// DeniedGitOp is one denied git operation handed to Config.RecordDenial for
// the audit log. Op is one of the gitOp* discriminators ("advertise",
// "fetch", "push", "path"); Ref is the offending ref on a ref-level denial
// (empty otherwise); Reason is a short machine token. Pure data — no domain
// dependency — so the proxy stays free of the audit model; the wiring layer
// maps it to an external_actions row.
type DeniedGitOp struct {
	Owner  string
	Repo   string
	Ref    string
	Op     string
	Reason string
}

// Config bundles the inputs a Server needs. Per-run construction; the
// token cache is per-Server so a new run gets a fresh credential and a
// dead one's tokens go to GC.
type Config struct {
	// TokenSource mints fresh installation tokens. Required.
	TokenSource TokenSource

	// Upstream is the absolute URL of the real git host — usually
	// "https://github.com". GitHub Enterprise Server passes its own
	// hostname (the customer's responsibility per the ticket scope).
	//
	// Only scheme + host are honored; path / query / fragment are
	// rejected at construction so a misconfigured caller fails loudly.
	// Default: defaultUpstream.
	Upstream string

	// AllowNonLoopback opts into binding Start on a non-loopback
	// address. The proxy is unauthenticated on the local hop — the
	// security boundary is "only the agent subprocess can reach it"
	// enforced by network isolation. An accidental "0.0.0.0:NNNN" bind
	// would expose a credentialed proxy to the LAN.
	//
	// The sandbox case (binding to the host-side veth IP, e.g.
	// 192.168.99.1) is the legitimate non-loopback use case and opts
	// in via this flag.
	AllowNonLoopback bool

	// RunID is the run identifier this proxy serves. Carried for
	// future per-run policy / observability; the proxy itself does not
	// branch on it today.
	RunID string

	// IncomingToken, when non-empty, is the per-run secret every request
	// must present before the proxy injects the real credential and
	// forwards upstream. The caller (internal/agentproc) generates a
	// fresh random token per run, sets it here, and routes that run's
	// sandbox git at the proxy with the same value as the HTTPS Basic
	// password (remote-URL userinfo or http.<url>.extraHeader, set
	// host-side). The proxy reads the inbound Authorization, compares the
	// presented password constant-time, and returns 401 on mismatch —
	// then replaces it with the resolved Basic x-access-token credential
	// before forwarding.
	//
	// This is the application-layer half of cross-tenant isolation: a
	// sibling run that reaches this proxy over the shared host namespace
	// holds a *different* token, so it cannot spend this run's GitHub
	// credential even if a packet gets through the network allowlist. It
	// is NOT a durable credential and does not violate Property B — the
	// real token still lives only in the proxy (injected upstream by the
	// rewrite hook) and never enters any sandbox.
	//
	// Empty disables the check (loopback/test usage, or single-tenant
	// direct paths where the local hop is already trusted).
	IncomingToken string

	// RecordPush, when non-nil, makes the proxy the capture point for branch
	// pushes. On each git-receive-pack POST it transits, the proxy tees the
	// request body, parses the pkt-line ref-update commands (the block that
	// precedes the packfile), and invokes RecordPush once per non-delete ref
	// after the upstream responds — for EVERY final status, with that status
	// on the PushedRef. The wiring is what branches on the outcome: a 2xx
	// (PushedRef.Succeeded) upserts the branch artifact (same constructor +
	// dedup key as the pre-push hook, so a normal hook+push never
	// double-records), while a refused push (403/404/422/5xx — nothing
	// landed) must record only an audit-log failure row, never an artifact.
	//
	// Because every push transits this proxy — `git push --no-verify` skips
	// client-side hooks but not the network layer — this is multi mode's
	// authoritative push-outcome observer; the pre-push hook (which fires
	// BEFORE the transfer and cannot know the outcome) stands down there.
	// Local mode has no proxy, so its hook-based capture (pre-push timing,
	// --no-verify gap) is accepted (nil here).
	//
	// Contract: the proxy never blocks, alters, or fails a push on account of
	// this callback. It runs after the response is back to the client, under a
	// bounded context, and any failure is the callback's to swallow. See
	// TFAC-467.
	RecordPush func(ctx context.Context, push PushedRef)

	// Authorize, when non-nil, is the per-request live authorization gate. The
	// proxy calls it with the (owner, repo) parsed from the request path
	// BEFORE minting/forwarding; a !Allowed decision is a 403 (the request is
	// never forwarded and no token is minted-for-use). For a push the returned
	// AllowedRefs is enforced per-ref against the receive-pack command block.
	// Reads live host-side state each call, so untracking a repo / removing a
	// worktree propagates to the very next request with no re-mint.
	//
	// A nil Authorize disables the gate (allow-all) — the loopback/test path,
	// mirroring an empty IncomingToken. In multi mode the wiring always sets
	// it, so production is always gated.
	Authorize func(ctx context.Context, owner, repo string) (Decision, error)

	// RecordDenial, when non-nil, is invoked once per denied operation (repo
	// gate, ref gate, path-shape, or a fail-closed authorize error) for the
	// audit log. The proxy calls it on a detached, bounded goroutine, so a slow
	// sink never adds latency to the denial response; best-effort (a denial
	// queued right before Shutdown may be dropped). Kept domain-free
	// (DeniedGitOp is pure data) so gitproxy doesn't depend on the audit model.
	RecordDenial func(ctx context.Context, denied DeniedGitOp)
}

// PushedRef is one non-delete ref update the backstop parsed from a
// git-receive-pack request body, handed to Config.RecordPush. Repo is the
// "owner/repo" from the request path; Ref is the full remote ref
// (refs/heads/...); NewSHA is the commit the push carried for it; Created is
// true when the push would create the ref (the remote held no prior value);
// Status is the upstream's final HTTP status for the receive-pack POST — the
// outcome discriminator the wiring branches on (see Succeeded). It is pure
// data — no domain dependency — so the proxy stays free of the artifact model;
// the wiring layer maps it to a branch artifact / audit row.
type PushedRef struct {
	Repo    string
	Ref     string
	NewSHA  string
	Created bool
	Status  int
}

// Succeeded reports whether the upstream accepted the push's transport (a 2xx
// final status). Only a succeeded push may become an artifact; a refused one
// is audit-log material only.
func (p PushedRef) Succeeded() bool {
	return p.Status >= 200 && p.Status < 300
}

// Server is a single per-run proxy instance with a cached installation
// token. Not safe to share across runs — the token it holds is
// installation-scoped, and the request counter is request-scoped.
type Server struct {
	cfg         Config
	upstreamURL *url.URL
	proxy       *httputil.ReverseProxy

	requestCount atomic.Int64

	// tokenMu serializes per-repo token cache access. Concurrent requests
	// for the same repo during a refresh coalesce on the mutex rather than
	// thundering-herd the minter. Cross-repo concurrency is serialized too
	// (one mutex); git ops per run are few, so the simplicity wins.
	tokenMu      sync.Mutex
	cachedTokens map[string]Token // keyed lower("owner/repo")
	// mintCount counts successful mint-and-cache cycles across all repos
	// (TokenSource returned a valid token we cached). Failed TokenSource
	// calls produce a 502 and do NOT increment this. Observable via
	// MintCount() for tests.
	mintCount atomic.Int64

	// listener is owned once Start has been called. nil until then.
	listener net.Listener
	httpSrv  *http.Server
	// serveErr receives the first non-ErrServerClosed error from
	// httpSrv.Serve. Buffered(1) so the goroutine never blocks on
	// send, even if the caller never reads it.
	serveErr chan error

	// now is the testable clock. nil in production (time.Now is used).
	now func() time.Time
}

// New constructs a Server with the given config but does not start
// listening. Call Start to bind a port and begin serving.
//
// Validates config eagerly so a misconfigured caller fails at
// construction time rather than producing a Server that silently
// 5xx's every request.
func New(cfg Config) (*Server, error) {
	if cfg.TokenSource == nil {
		return nil, errors.New("gitproxy: TokenSource is required")
	}
	upstream := cfg.Upstream
	if upstream == "" {
		upstream = defaultUpstream
	}
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("gitproxy: parse upstream URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("gitproxy: upstream URL missing scheme or host: %q", upstream)
	}
	// Reject paths / queries / fragments on the upstream URL. The proxy
	// preserves the incoming request path verbatim; a caller who passed
	// "https://github.com/x" by mistake would route every git request
	// under "/x/" and 404 at the upstream.
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("gitproxy: upstream URL must not include a path (got %q); the incoming request path is forwarded as-is", upstream)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("gitproxy: upstream URL must not include query or fragment: %q", upstream)
	}
	// Require HTTPS for non-loopback upstreams — the installation
	// token travels in the rewritten Authorization header and must not
	// cross cleartext. Loopback http is permitted for httptest in unit
	// tests; real GitHub / GHE are https.
	if u.Scheme != "https" {
		// u.Hostname() strips the port AND the IPv6 brackets, so it
		// works for "127.0.0.1:8080", "[::1]:8080", and "[::1]" alike.
		// Doing this by hand with net.SplitHostPort would reject the
		// port-less IPv6 literal because SplitHostPort returns an error
		// and the bracket form ("[::1]") then fails net.ParseIP.
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("gitproxy: upstream %q must use https (loopback http is allowed for tests)", upstream)
		}
	}

	s := &Server{cfg: cfg, upstreamURL: u}
	s.proxy = &httputil.ReverseProxy{
		Rewrite:        s.rewrite,
		ModifyResponse: s.modifyResponse,
	}
	return s, nil
}

// Handler exposes the proxy as an http.Handler. Useful for tests that
// drive the proxy via httptest.NewServer rather than the production
// Start path, and for callers that want to compose middleware (e.g.
// adding observability) outside the listener loop.
//
// The returned handler does the credential injection before delegating
// to the underlying ReverseProxy: a failure to mint a token surfaces
// as a 502 here rather than via the ReverseProxy's silent-pass-broken-
// auth path.
//
// CONNECT requests are rejected explicitly with 501. Git clients
// configured with http.proxy=<this> AND an https:// remote URL would
// issue CONNECT to tunnel TLS through the proxy; once tunneled, the
// traffic is opaque end-to-end TLS that we cannot inject credentials
// into. Failing fast with a clear error here surfaces the misconfig
// instead of producing an authenticated-looking but unauth'd request.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			http.Error(w,
				"gitproxy: CONNECT not supported; use http:// remote URLs so the proxy can inject credentials (https:// would tunnel TLS opaquely)",
				http.StatusNotImplemented)
			return
		}
		// Per-run caller auth: validate the inbound credential against
		// this run's IncomingToken BEFORE minting/injecting the real
		// one. A sibling run (or any unauthorized caller) gets 401 and
		// never reaches the upstream credential pipeline. Empty
		// IncomingToken disables the gate (loopback/test path).
		if !s.callerAuthorized(r) {
			// 401 without a WWW-Authenticate challenge: the caller is our
			// own per-run git config, not an interactive client, so
			// there's no auth to negotiate. Terse body; the status is the
			// signal. Read-then-replace happens entirely host-side, so
			// reading the inbound header here doesn't conflict with the
			// rewrite that overwrites it on the way upstream.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Path-shape allowlist: forward ONLY the three git smart-HTTP shapes
		// and extract the (owner, repo) they target. Everything else — most
		// importantly a GHES API path "/api/v3/..." (same host as git), plus
		// LFS and web routes — is rejected here, before any credential is
		// injected. This is what makes "the git proxy can only do git" real on
		// GHES, where the host split that protects github.com doesn't exist.
		owner, repo, op, ok := parseGitPath(r.Method, r.URL.Path)
		if !ok {
			s.recordDenial(DeniedGitOp{Op: gitOpPath, Reason: "non-git-path"})
			http.Error(w, "gitproxy: non-git path", http.StatusForbidden)
			return
		}

		// Live per-repo authorization. A nil Authorize is allow-all
		// (loopback/test); in multi mode the wiring always sets it. A backend
		// error fails closed with a 502 (the gate is up but its data source is
		// broken — don't forward); a !Allowed decision is a 403. Both are
		// blocked git activity, so both leave an audit trail.
		decision := Decision{Allowed: true}
		if s.cfg.Authorize != nil {
			d, err := s.cfg.Authorize(r.Context(), owner, repo)
			if err != nil {
				// An authz outage/misconfig blocks the op just as a hard deny
				// does — record it so the audit trail isn't silent on outages.
				s.recordDenial(DeniedGitOp{Owner: owner, Repo: repo, Op: op, Reason: "authorize-error"})
				http.Error(w, "gitproxy: authorization check failed", http.StatusBadGateway)
				return
			}
			if !d.Allowed {
				s.recordDenial(DeniedGitOp{Owner: owner, Repo: repo, Op: op, Reason: "repo-not-authorized"})
				http.Error(w, "gitproxy: repo not authorized for this run", http.StatusForbidden)
				return
			}
			decision = d
		}

		tok, err := s.installationToken(r.Context(), owner, repo)
		if err != nil {
			// 502 Bad Gateway maps cleanly: the proxy is alive but the
			// upstream credential pipeline is broken. Avoid leaking the
			// error detail to the agent — the underlying mint error
			// may include the App ID or other identifying info.
			http.Error(w, "gitproxy: failed to obtain installation token", http.StatusBadGateway)
			return
		}
		// Stash the token on the request context so Rewrite can pick
		// it up. Passing through context (rather than mutating headers
		// here) keeps the token off the inbound request's header set,
		// which means it never appears in a log of pr.In.Header.
		r = r.WithContext(context.WithValue(r.Context(), tokenCtxKey{}, tok.Value))

		if op == gitOpPush {
			if s.cfg.Authorize != nil {
				// Gated path: buffer the ref-update command block, enforce the
				// per-ref allowlist (reject deletes / foreign refs) BEFORE
				// forwarding, then proxy the reconstructed stream and report
				// each ref + the upstream's final status to RecordPush.
				s.serveReceivePackGated(w, r, owner, repo, decision.AllowedRefs)
				return
			}
			// Non-gated (loopback/test): the observe-only backstop (TFAC-467)
			// — tee the body, report refs + outcome status, never block.
			if s.cfg.RecordPush != nil {
				s.serveReceivePack(w, r)
				return
			}
		}
		s.proxy.ServeHTTP(w, r)
	})
}

// recordDenial invokes the audit hook for one denied operation, if wired. The
// hook runs on a detached, bounded goroutine (its own context, not the
// request's, which is cancelled the moment the handler returns) so a slow sink
// — the default writes a DB row — never adds latency to the denial response
// sent to the per-run git client. Best-effort: a denial queued right before
// Shutdown may be dropped, which is acceptable for an audit breadcrumb.
func (s *Server) recordDenial(denied DeniedGitOp) {
	if s.cfg.RecordDenial == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), recordPushTimeout)
		defer cancel()
		s.cfg.RecordDenial(ctx, denied)
	}()
}

// tokenCtxKey is the request-context key used to hand the resolved
// installation token from the Handler wrapper to the Rewrite hook.
// Unexported empty struct so external code cannot collide.
type tokenCtxKey struct{}

// callerAuthorized reports whether the request presents the per-run
// IncomingToken as its HTTPS Basic password. Git emits the token via
// remote-URL userinfo or http.<url>.extraHeader (set host-side), both
// of which arrive as "Authorization: Basic base64(user:token)"; the
// username is irrelevant (conventionally x-run) — only the password is
// validated.
//
// subtle.ConstantTimeCompare is constant-time only in the CONTENT of two
// equal-length inputs: it can't be byte-probed for an equal-length wrong
// guess. It short-circuits (returns 0 immediately) when the lengths
// differ, so a missing, malformed, or wrong-length credential is rejected
// without a content compare — and that length-dependent timing leaks
// nothing here, because the token is a fixed-length 64-hex secret whose
// length an attacker already knows. The empty-string from a missing
// Basic header thus fails the compare and 401s.
//
// An empty IncomingToken disables the gate (loopback/test path, or a
// single-tenant direct usage where the local hop is already trusted).
func (s *Server) callerAuthorized(r *http.Request) bool {
	if s.cfg.IncomingToken == "" {
		return true
	}
	_, presented, _ := r.BasicAuth()
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.IncomingToken)) == 1
}

// installationToken returns a valid cached token for owner/repo, minting a
// fresh one (scoped to that repo) if the cache is empty for it or within
// refreshThreshold of expiry.
//
// Serialized via tokenMu — concurrent requests during a refresh coalesce on
// the mutex rather than stampeding the upstream mint endpoint. The mutex held
// during the upstream call is acceptable here because mint is on the run's
// critical path anyway and TokenSource implementations are expected to be fast
// (sub-second for the GitHub /app/installations/.../access_tokens endpoint).
func (s *Server) installationToken(ctx context.Context, owner, repo string) (Token, error) {
	key := strings.ToLower(owner + "/" + repo)
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()

	now := s.timeNow()
	if tok, ok := s.cachedTokens[key]; ok && tok.Value != "" && !tokenStale(tok, now) {
		return tok, nil
	}

	tok, err := s.cfg.TokenSource(ctx, owner, repo)
	if err != nil {
		return Token{}, fmt.Errorf("token source: %w", err)
	}
	if tok.Value == "" {
		return Token{}, errors.New("token source returned empty token")
	}
	// A non-zero expiry must be comfortably in the future — reject a
	// source that hands back an already-expired or near-expiry token
	// (fail fast rather than forward a credential GitHub will 401). A
	// ZERO expiry is the PAT case: the source tracks no lifetime, so we
	// cache it and never refresh rather than treating the zero value as
	// already-expired (which would re-mint on every request — a refresh
	// storm against the secret store for a credential that never rotates).
	if !tok.ExpiresAt.IsZero() && !tok.ExpiresAt.After(now.Add(refreshThreshold)) {
		return Token{}, fmt.Errorf("token source returned expired or near-expiry token (expires_at=%s)", tok.ExpiresAt.Format(time.RFC3339))
	}
	if s.cachedTokens == nil {
		s.cachedTokens = make(map[string]Token)
	}
	s.cachedTokens[key] = tok
	s.mintCount.Add(1)
	return tok, nil
}

// tokenStale reports whether a cached token needs re-minting: true when it is
// within refreshThreshold of its expiry. A zero ExpiresAt (a PAT — no tracked
// lifetime) is never stale, so a PAT source mints exactly once per repo. (A PAT
// reaches the proxy only for a genuine no-App org under App-XOR-PAT, where the
// PAT is the org's actual credential — never a silent degrade from an App.)
func tokenStale(tok Token, now time.Time) bool {
	if tok.ExpiresAt.IsZero() {
		return false
	}
	return !now.Add(refreshThreshold).Before(tok.ExpiresAt)
}

// rewrite is the Go 1.20+ ReverseProxy hook (replacing Director). It
// runs before the request is sent upstream and has explicit control
// over Out — unlike Director, the stdlib does NOT add X-Forwarded-*
// after Rewrite returns unless we call pr.SetXForwarded() ourselves.
// That lets us suppress those proxy-chain headers, which would just
// be noise to GitHub.
//
// The header rewrite uses Set (not Add) so any client-supplied auth
// header is overwritten, not duplicated. A duplicate Authorization
// would confuse some HTTP/2 implementations and would absolutely
// confuse the upstream's auth path.
func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	// SetURL rewrites Out.URL.Scheme, Out.URL.Host, joins paths, and
	// sets Out.Host. Since we validated the upstream URL has no path,
	// SetURL preserves the incoming request path verbatim.
	pr.SetURL(s.upstreamURL)

	// Defensive: drop any X-Forwarded-* headers an upstream might
	// trust. We deliberately do not call pr.SetXForwarded().
	pr.Out.Header.Del("X-Forwarded-For")
	pr.Out.Header.Del("X-Forwarded-Host")
	pr.Out.Header.Del("X-Forwarded-Proto")
	pr.Out.Header.Del("Forwarded")

	// Strip proxy-auth headers explicitly. httputil.ReverseProxy
	// already removes hop-by-hop headers (RFC 7230 §6.1 — Proxy-
	// Authorization and Proxy-Connection are in that set) but we
	// belt-and-braces this: if the agent's git is misconfigured with
	// proxy credentials, or a future stdlib change ever weakened the
	// hop-by-hop filter, forwarding Proxy-Authorization would leak
	// those credentials to GitHub.
	pr.Out.Header.Del("Proxy-Authorization")
	pr.Out.Header.Del("Proxy-Connection")

	tok, _ := pr.In.Context().Value(tokenCtxKey{}).(string)
	if tok == "" {
		// Defense in depth: if the Handler wrapper somehow skipped
		// us, fail closed by stripping any caller-supplied auth so
		// the request goes anonymous (which github will 401) rather
		// than passing through a potentially-leaked credential.
		pr.Out.Header.Del("Authorization")
		return
	}
	pr.Out.Header.Set("Authorization", basicAuthHeader(tok))
}

// basicAuthHeader returns the "Basic <b64>" string for GitHub App
// installation tokens. Exported via the tests so the encoding can be
// pinned without re-implementing it inline; kept package-private
// because nothing outside the proxy needs it.
func basicAuthHeader(token string) string {
	creds := "x-access-token:" + token
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

// modifyResponse is a hook for observability and per-request counter
// bumping. Returning an error here would 502 the client; we never do
// that — just observe.
func (s *Server) modifyResponse(resp *http.Response) error {
	s.requestCount.Add(1)
	return nil
}

// Start binds a TCP port and serves until Shutdown is called. Returns
// the bound address so the caller can construct the agent-side git
// proxy URL.
//
// Defaults to 127.0.0.1:0 (random loopback port) when addr is "". A
// non-loopback bind requires Config.AllowNonLoopback=true — the proxy
// is unauthenticated on the local hop, so an accidental "0.0.0.0:NNNN"
// would expose a credentialed proxy to the LAN. The sandbox case
// (binding to the host-side veth IP, e.g. 192.168.99.1) is the
// legitimate non-loopback use and opts in via the Config flag.
func (s *Server) Start(addr string) (string, error) {
	if s.httpSrv != nil {
		return "", errors.New("gitproxy: already started; create a new Server per run")
	}
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if !s.cfg.AllowNonLoopback {
		if err := assertLoopback(addr); err != nil {
			return "", err
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("gitproxy: listen on %s: %w", addr, err)
	}
	s.listener = ln
	s.serveErr = make(chan error, 1)
	s.httpSrv = &http.Server{
		Handler: s.Handler(),
		// Conservative timeouts. Git operations can be slow (large
		// pack-files take minutes); ReadHeaderTimeout limits header
		// receive, not body. Total request time is unbounded (no
		// WriteTimeout) because git pack uploads can run for minutes.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		err := s.httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr <- err
		}
		close(s.serveErr)
	}()
	return ln.Addr().String(), nil
}

// Err returns a channel that receives the first unexpected error from
// the background Serve goroutine (i.e. not http.ErrServerClosed). The
// channel is closed when the goroutine exits, so a range or select on
// it unblocks after Shutdown.
//
// Callers that do not need to monitor the error can ignore this channel
// safely — it is buffered and the goroutine never blocks on send.
func (s *Server) Err() <-chan error { return s.serveErr }

// Shutdown stops serving and waits for in-flight requests to drain
// (up to the context's deadline). Idempotent.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

// RequestCount returns the number of upstream responses the proxy has
// observed. Useful for tests asserting the agent actually went through
// the proxy.
func (s *Server) RequestCount() int64 { return s.requestCount.Load() }

// MintCount returns the number of successful TokenSource invocations
// (i.e. mints whose returned token passed validation and was cached).
// TokenSource calls that returned an error or a stale/invalid token
// are NOT counted — those produce a 502 and the cache is unchanged.
//
// Exposed so tests can verify caching behavior (first request for a repo
// mints; subsequent requests for the same repo reuse; a second repo mints
// again). Tests asserting "mint was attempted at all" should pin upstream
// hits or response status instead, since a failed attempt is still
// observable via the 502 it produces.
func (s *Server) MintCount() int64 { return s.mintCount.Load() }

// timeNow returns the current time, honoring the testable now hook.
func (s *Server) timeNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// assertLoopback returns nil iff addr binds to a loopback interface.
// Hostnames resolve via the OS resolver; every resolved IP must be
// loopback for the check to pass. The empty-host case (":NNNN" form
// binds to all interfaces) is rejected explicitly.
//
// Used by Start to enforce the safety default of loopback-only when
// AllowNonLoopback is false. The veth-IP sandbox case opts out.
func assertLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("gitproxy: parse bind address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("gitproxy: bind address %q binds to all interfaces — set AllowNonLoopback=true to confirm intent", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("gitproxy: bind address %q is not loopback — set AllowNonLoopback=true to confirm intent", addr)
		}
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("gitproxy: resolve %q: %w", host, err)
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("gitproxy: bind host %q resolves to %s (not loopback) — set AllowNonLoopback=true to confirm intent", host, a)
		}
	}
	return nil
}
