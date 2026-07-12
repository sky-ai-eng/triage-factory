// Package llmproxy is a per-run HTTP intermediary that holds an LLM
// provider's API credential on the trusted side and exposes only a
// localhost (or unix-socket) base URL to the agent subprocess.
//
// # The threat it addresses
//
// The gVisor sandbox validation (docs/for-agents/specs/sky-254-runsc-validation/)
// proved that whatever we inject into the OCI bundle's process.env is
// readable inside the sandbox via `env` or /proc/self/environ. That
// means injecting ANTHROPIC_API_KEY directly leaks the credential to
// the agent subprocess, which is the wrong posture for a multi-tenant
// product where the agent's prompts (and therefore behavior) are
// user-controlled.
//
// The proxy holds the real credential server-side and exposes only a
// base URL. The agent SDK natively supports this via the
// ANTHROPIC_BASE_URL env var (Anthropic direct) and
// ANTHROPIC_BEDROCK_BASE_URL (Bedrock). Per the Claude Code docs and
// the SDK source-grep of @anthropic-ai/claude-agent-sdk, requests sent
// to those URLs are forwarded transparently — the proxy doesn't need
// to know the request shape, only that it must rewrite the auth
// header before forwarding upstream.
//
// # Phase 1 scope (this file)
//
// Bearer paths only:
//
//   - Anthropic direct: rewrite "x-api-key" with the real key.
//   - Bedrock bearer (AWS_BEARER_TOKEN_BEDROCK): rewrite "Authorization"
//     with "Bearer <real-bearer>".
//
// Both are HTTP-layer header injection — no body parsing, no protocol
// translation. The proxy is built on httputil.ReverseProxy which
// handles streaming responses (SSE / chunked) natively.
//
// # Phase 2 (sigv4.go)
//
// SigV4 re-signing for the Bedrock AWS-access-key-triple path
// (ProviderBedrockSigV4). The proxy buffers the request body, strips
// the placeholder signature the sandbox SDK produced, and re-signs the
// whole outgoing request with the org's real triple via aws-sdk-go-v2's
// SignHTTP. See sigv4.go's file doc for the full flow.
//
// # Trust model on the local hop
//
// In multi mode the proxy binds to the host-side veth IP of one run's
// sandbox (10.42.<N>.1). That address lives in the host/container root
// netns — the shared default gateway every concurrent sandbox routes
// through — so it is physically reachable from a *sibling* run's
// sandbox, not just its own. A compromised agent in run B can send
// packets at run A's proxy. We must NOT assume "reaching the proxy
// means it's our own run": that assumption (the original Phase 1
// rationale) was false the moment two tenants' sandboxes shared the
// host namespace, and it is the cross-tenant credential-abuse hole
// this proxy closes.
//
// So the proxy authenticates the caller with a per-run secret. Config
// carries IncomingToken — a fresh random value the caller generates per
// run and also injects into that run's sandbox as the credential the
// SDK sends (x-api-key for Anthropic, "Authorization: Bearer" for
// Bedrock). Every request is checked, constant-time, against that token
// before anything is forwarded upstream; a missing or wrong token gets
// 401 and never reaches the real provider. A sibling run holds a
// *different* token, so even though it can reach this proxy on the
// network it cannot spend this run's credential.
//
// This does not weaken Property B. The per-run token is a capability
// scoped to one run's own proxy — generated fresh, destroyed at
// teardown, authorizing only what that run is already allowed to do. It
// is not a durable, reusable, or cross-scope credential. The real
// provider key still lives only in the proxy (injected upstream by the
// rewrite hook) and never enters any sandbox.
//
// Defense-in-depth: the network layer (per-sandbox egress allowlist)
// aims to make a sibling proxy unreachable in the first
// place; this token makes it useless even if a packet gets through.
// Empty IncomingToken disables the check — the loopback/test path and
// any single-tenant direct usage where the local hop is already trusted.
package llmproxy

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// Provider distinguishes the auth-header shape the proxy injects.
// Bearer-style providers live here; ProviderBedrockSigV4 (sigv4.go)
// re-signs the whole request instead of rewriting one header. More
// providers (Vertex) get added in later phases.
type Provider string

const (
	// ProviderAnthropic injects "x-api-key: <key>". Forwards to
	// api.anthropic.com (or whatever Upstream is configured).
	ProviderAnthropic Provider = "anthropic"

	// ProviderBedrockBearer injects "Authorization: Bearer <key>".
	// Used for AWS_BEARER_TOKEN_BEDROCK (Option E in the Claude Code
	// on Bedrock docs). Forwards to bedrock-runtime.<region>.amazonaws.com.
	ProviderBedrockBearer Provider = "bedrock_bearer"
)

// Config bundles the inputs a Server needs at construction. Each
// agent run gets its own Server with the org's resolved credentials.
type Config struct {
	// Provider selects the auth-header injection style.
	Provider Provider

	// APIKey is the real credential the proxy injects upstream for the
	// bearer-style providers (Anthropic, Bedrock bearer). Never appears
	// in the agent subprocess's env. Ignored when Provider ==
	// ProviderBedrockSigV4, whose credential is SigV4Credentials.
	APIKey string

	// SigV4Credentials is the real AWS access-key triple for the
	// Bedrock SigV4 path. Required when Provider ==
	// ProviderBedrockSigV4 and SigV4CredentialSource is nil; ignored
	// otherwise. See sigv4.go.
	SigV4Credentials *SigV4Credentials

	// SigV4CredentialSource, when non-nil, supplies the SigV4 signing
	// triple live per request (the git-proxy TokenSource pattern) so a
	// role-mode run picks up a re-minted STS credential mid-run. Takes
	// precedence over SigV4Credentials; exactly one of the two must be set
	// for ProviderBedrockSigV4. See sigv4.go.
	SigV4CredentialSource SigV4CredentialSource

	// Upstream is the absolute URL of the real LLM provider — e.g.
	// "https://api.anthropic.com" or "https://bedrock-runtime.us-east-1.amazonaws.com".
	// Only scheme + host are honored; path / query / fragment must
	// be empty and are rejected at construction time so a caller who
	// passes "https://api.anthropic.com/v1" by mistake fails loudly
	// rather than silently misrouting (the incoming request path is
	// what we forward).
	Upstream string

	// AllowNonLoopback opts into binding Start on a non-loopback
	// address. The security boundary on the local hop is per-run token
	// auth (IncomingToken; see the package-level trust model) backed by
	// the network-isolation allowlist — not "loopback-only", which by
	// itself never authenticated the caller. An accidental "0.0.0.0:NNNN"
	// bind would still expose the proxy to the LAN (where the token is
	// the only thing standing between a LAN attacker and the org's key),
	// so loopback-only-by-default remains the safety default and this
	// flag is the conscious opt-out.
	//
	// The sandbox integration (the host-side veth IP for the gVisor
	// netns) is the legitimate non-loopback use case. Set this true when
	// binding to the veth gateway IP so the caller has consciously
	// acknowledged the bind is not loopback.
	AllowNonLoopback bool

	// IncomingToken, when non-empty, is the per-run secret every
	// request must present before the proxy forwards it upstream. The
	// caller (internal/agentproc) generates a fresh random token per
	// run, sets it here, and injects the same value into that run's
	// sandbox as the credential the SDK sends — x-api-key for Anthropic,
	// "Authorization: Bearer" for Bedrock. The proxy compares the
	// presented value constant-time and returns 401 on mismatch (see
	// the package doc's trust-model section).
	//
	// This is the fail-closed half of cross-tenant isolation:
	// a sibling run that reaches this proxy over the shared host
	// namespace holds a *different* token, so it cannot spend this run's
	// credential. It is NOT a durable credential and does not violate
	// Property B — the real provider key still lives only in APIKey and
	// never enters any sandbox.
	//
	// Empty disables the check (loopback/test usage, or single-tenant
	// direct paths where the local hop is already trusted).
	IncomingToken string
}

// Server is a single per-run proxy instance. Not safe to share
// across runs — the credential it holds is org-scoped, and the
// request counter is request-scoped.
type Server struct {
	cfg         Config
	upstreamURL *url.URL
	proxy       *httputil.ReverseProxy
	// handler is what the http.Server actually serves: the bare
	// reverse proxy when IncomingToken is empty, or the token-gated
	// wrapper (tokenGate) when a per-run secret is configured.
	handler      http.Handler
	requestCount atomic.Int64
	// signer re-signs outgoing requests for ProviderBedrockSigV4; nil
	// for the bearer providers. Stateless and safe for concurrent use.
	signer *v4.Signer

	// sigV4Mu guards the cached live signing material (cfg.SigV4CredentialSource
	// path). One credential, cached until within sigV4RefreshThreshold of
	// expiry, mirroring the git proxy's per-repo token cache — coalesces
	// concurrent requests onto one re-mint.
	sigV4Mu     sync.Mutex
	sigV4Cached *SigV4Material

	// listener is owned once Start has been called. nil until then.
	listener net.Listener
	httpSrv  *http.Server
	// serveErr receives the first non-ErrServerClosed error from
	// httpSrv.Serve. Buffered(1) so the goroutine never blocks on
	// send, even if the caller never reads it.
	serveErr chan error
}

// New constructs a Server with the given config but does not start
// listening. Call Start to bind a port and begin serving.
//
// Validates config eagerly so a misconfigured caller fails at
// construction time rather than producing a Server that silently
// can't serve requests.
func New(cfg Config) (*Server, error) {
	switch cfg.Provider {
	case ProviderAnthropic, ProviderBedrockBearer:
		if cfg.APIKey == "" {
			return nil, errors.New("llmproxy: APIKey is required")
		}
	case ProviderBedrockSigV4:
		// Exactly one of the static triple or the live source. The source
		// path defers all credential validation to mint time (the triple
		// isn't known at construction), so only shape-check the static path.
		if cfg.SigV4CredentialSource == nil {
			if err := cfg.SigV4Credentials.validate(); err != nil {
				return nil, err
			}
		} else if cfg.SigV4Credentials != nil {
			return nil, errors.New("llmproxy: set exactly one of SigV4Credentials or SigV4CredentialSource, not both")
		}
	default:
		return nil, fmt.Errorf("llmproxy: unsupported provider %q", cfg.Provider)
	}
	if cfg.Upstream == "" {
		return nil, errors.New("llmproxy: Upstream is required")
	}
	u, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("llmproxy: parse upstream URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("llmproxy: upstream URL missing scheme or host: %q", cfg.Upstream)
	}
	// Reject paths / queries / fragments on the upstream URL. The
	// proxy preserves the incoming request path verbatim; a caller
	// who passed "https://api.anthropic.com/v1" would silently get
	// requests routed to "/v1/v1/messages" (path-joined) and 404
	// at the upstream. Fail at construction so the misconfiguration
	// surfaces at boot, not on the first request.
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("llmproxy: upstream URL must not include a path (got %q); the incoming request path is forwarded as-is", cfg.Upstream)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("llmproxy: upstream URL must not include query or fragment: %q", cfg.Upstream)
	}
	// Require HTTPS for non-loopback upstreams — the real credential
	// travels on this hop (in the rewritten auth header) and must not
	// cross cleartext. Loopback http is permitted for httptest usage in
	// unit tests; every real LLM provider uses https anyway.
	if u.Scheme != "https" {
		host, _, _ := net.SplitHostPort(u.Host)
		if host == "" {
			host = u.Host
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("llmproxy: upstream %q must use https (loopback http is allowed for tests)", cfg.Upstream)
		}
	}

	s := &Server{cfg: cfg, upstreamURL: u}
	s.proxy = &httputil.ReverseProxy{
		Rewrite:        s.rewrite,
		ModifyResponse: s.modifyResponse,
		// Default ErrorHandler logs to stderr and 502s. That's fine
		// for Phase 1; upgrade observability comes later.
	}
	s.handler = s.proxy
	// The SigV4 path needs the full body in hand before the reverse
	// proxy runs (the signature covers the payload hash); the buffering
	// middleware owns the 400/413 error paths the Rewrite hook doesn't
	// have.
	if cfg.Provider == ProviderBedrockSigV4 {
		s.signer = v4.NewSigner()
		s.handler = s.bufferSigV4Body(s.handler)
	}
	// When a per-run token is configured, gate every request on it
	// before anything else runs — outermost, so an
	// unauthenticated caller costs no body buffering either. Empty
	// token = no gate (the loopback/test path).
	if cfg.IncomingToken != "" {
		s.handler = s.tokenGate(s.handler)
	}
	return s, nil
}

// tokenGate wraps next with per-run caller authentication. Every
// request must present Config.IncomingToken on the provider's
// credential header (x-api-key for Anthropic, "Authorization: Bearer"
// for Bedrock); a missing or mismatched token gets 401 and is never
// forwarded upstream. This is the fail-closed guarantee of the token
// gate: even if network isolation fails and a sibling sandbox reaches
// this proxy, it can't spend the org's key without this run's secret.
func (s *Server) tokenGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.callerAuthorized(r) {
			// 401 without a WWW-Authenticate challenge: the caller is
			// our own SDK, not a browser, so there's no interactive auth
			// to negotiate. Terse body; the real signal is the status.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// callerAuthorized reports whether the request presents the per-run
// token on the provider-appropriate credential header. Constant-time
// compare so a hostile caller can't time-probe the token byte by byte;
// ConstantTimeCompare also returns 0 on a length mismatch, which covers
// the missing-header (empty string) case.
func (s *Server) callerAuthorized(r *http.Request) bool {
	var presented string
	switch s.cfg.Provider {
	case ProviderAnthropic:
		presented = r.Header.Get("x-api-key")
	case ProviderBedrockBearer:
		// The SDK sends "Bearer <token>"; strip the scheme prefix before
		// comparing. A header without the prefix won't match (TrimPrefix
		// returns it unchanged, which then fails the compare).
		presented = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	case ProviderBedrockSigV4:
		// The per-run token is the placeholder access-key ID the sandbox
		// SDK signed with; it rides in cleartext in the Credential=
		// scope of the SigV4 Authorization header. See sigV4AccessKeyID.
		presented = sigV4AccessKeyID(r.Header.Get("Authorization"))
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.IncomingToken)) == 1
}

// eofLatchedBody wraps the outgoing request body so that, once the
// underlying body has returned io.EOF, every later Read reports io.EOF
// without touching the underlying body again.
//
// It exists to break a stdlib race on the shared request body. Out.Body
// IS In.Body (ReverseProxy passes it through), and two goroutines touch
// it: the transport's writeLoop finishes a Content-Length'd outgoing
// body with an extra drain read into io.Discard (transferWriter.writeBody)
// to detect bodies longer than declared, while the proxy's own http.Server
// closes that same body on the handler's first response write
// (chunkWriter.writeHeader) to ready the client conn for reuse — after
// which any Read returns ErrBodyReadAfterClose, even fully consumed. If
// the upstream begins responding before the drain read runs, the close
// can land first; the drain read then errors, Request.write fails, and
// the transport tears down the upstream conn — severing an in-flight
// streaming response mid-body. Latching EOF makes the post-consumption
// drain read benign. A close that truncates a body NOT yet read to EOF
// still surfaces as an error, as it must — the latch only engages after
// every byte was delivered.
//
// No locking: the transport reads a request body from a single goroutine.
type eofLatchedBody struct {
	rc  io.ReadCloser
	eof bool
}

func (b *eofLatchedBody) Read(p []byte) (int, error) {
	if b.eof {
		return 0, io.EOF
	}
	n, err := b.rc.Read(p)
	if err == io.EOF {
		b.eof = true
	}
	return n, err
}

func (b *eofLatchedBody) Close() error { return b.rc.Close() }

// rewrite is the Go 1.20+ ReverseProxy hook (replacing Director). It
// runs before the request is sent upstream and has explicit control
// over Out — unlike Director, the stdlib does NOT add X-Forwarded-*
// after Rewrite returns unless we call pr.SetXForwarded() ourselves.
// That lets us suppress those proxy-chain headers, which would just
// be noise to an LLM provider.
//
// The header rewrite uses Set (not Add) so any client-supplied auth
// header is overwritten, not duplicated. That matters because the SDK
// sends an empty or placeholder x-api-key depending on env shape; a
// duplicate header could confuse some HTTP/2 implementations and
// would absolutely confuse the upstream's auth path.
func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	// SetURL rewrites Out.URL.Scheme, Out.URL.Host, joins paths, and
	// sets Out.Host. Since we validated the upstream URL has no path,
	// SetURL preserves the incoming request path verbatim.
	pr.SetURL(s.upstreamURL)

	// Shield the upstream exchange from the server's response-time close
	// of the shared request body (see eofLatchedBody). The SigV4 path
	// is exempt: rewriteSigV4 replaces the body outright with a fresh
	// in-memory reader, so there is no shared server-owned body to race.
	if s.cfg.Provider != ProviderBedrockSigV4 && pr.Out.Body != nil {
		pr.Out.Body = &eofLatchedBody{rc: pr.Out.Body}
	}

	// Defensive: if the incoming request happened to carry
	// X-Forwarded-* headers (some misconfigured caller), drop them.
	// We deliberately do not call pr.SetXForwarded() — the stdlib
	// only adds these when explicitly invoked under the Rewrite API.
	// For SigV4 this must precede signing so a deleted header can't be
	// part of the signed set.
	pr.Out.Header.Del("X-Forwarded-For")
	pr.Out.Header.Del("X-Forwarded-Host")
	pr.Out.Header.Del("X-Forwarded-Proto")
	pr.Out.Header.Del("Forwarded")

	switch s.cfg.Provider {
	case ProviderAnthropic:
		pr.Out.Header.Set("x-api-key", s.cfg.APIKey)
		// The SDK already sets anthropic-version; we don't override.
	case ProviderBedrockBearer:
		pr.Out.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	case ProviderBedrockSigV4:
		s.rewriteSigV4(pr)
	}
}

// modifyResponse is a hook for observability and per-request counter
// bumping. Returning an error here would 502 the client; we never do
// that in Phase 1 — just observe.
func (s *Server) modifyResponse(resp *http.Response) error {
	s.requestCount.Add(1)
	return nil
}

// Handler exposes the proxy as a standard http.Handler. Useful for
// tests that want to drive the proxy via httptest.NewServer instead
// of the production Start path. Returns the token-gated handler when
// IncomingToken is set, so tests exercise the same auth path as
// production.
func (s *Server) Handler() http.Handler { return s.handler }

// Start binds a TCP port and serves until Shutdown is called. Returns
// the bound address so the caller can construct the agent's BASE_URL
// env var.
//
// Defaults to 127.0.0.1:0 (random loopback port) when addr is "". A
// non-loopback bind requires Config.AllowNonLoopback=true — an
// accidental "0.0.0.0:NNNN" would expose the proxy to the LAN, where
// (when IncomingToken is set) the per-run token is the only barrier to
// the org's key, and (when it isn't) there is none. The sandbox case
// (binding to the host-side veth IP, e.g. 10.42.<idx>.1) is the
// legitimate non-loopback use case and opts in via the Config flag.
func (s *Server) Start(addr string) (string, error) {
	if s.httpSrv != nil {
		return "", errors.New("llmproxy: already started; create a new Server per run")
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
		return "", fmt.Errorf("llmproxy: listen on %s: %w", addr, err)
	}
	s.listener = ln
	s.serveErr = make(chan error, 1)
	s.httpSrv = &http.Server{
		Handler: s.handler,
		// Conservative timeouts. The SDK uses long-lived streaming
		// connections for tool-use loops; ReadHeaderTimeout limits the
		// time to receive the request headers, not the body, so 30s is
		// fine. Total request time is unbounded (no WriteTimeout)
		// because streaming responses can run for minutes.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		err := s.httpSrv.Serve(ln)
		// ErrServerClosed is the normal return from Serve after
		// Shutdown; anything else is an unexpected accept/listener
		// failure that the caller may want to react to.
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

// RequestCount returns the number of upstream responses the proxy
// has observed. Useful for tests asserting the agent actually went
// through the proxy.
func (s *Server) RequestCount() int64 { return s.requestCount.Load() }

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
		return fmt.Errorf("llmproxy: parse bind address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("llmproxy: bind address %q binds to all interfaces — set AllowNonLoopback=true to confirm intent", addr)
	}
	// If host parses as an IP literal, check directly.
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("llmproxy: bind address %q is not loopback — set AllowNonLoopback=true to confirm intent", addr)
		}
		return nil
	}
	// Hostname (e.g. "localhost"); resolve and require every result
	// to be loopback. "localhost" passes; "myhost.local" pointing at
	// a routable LAN IP does not.
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("llmproxy: resolve %q: %w", host, err)
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("llmproxy: bind host %q resolves to %s (not loopback) — set AllowNonLoopback=true to confirm intent", host, a)
		}
	}
	return nil
}
