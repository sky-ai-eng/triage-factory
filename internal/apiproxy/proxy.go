// Package apiproxy is a per-run HTTP intermediary that holds a REST API
// credential — GitHub's or Jira's — on the trusted side and exposes only
// a base URL plus a per-run placeholder token to the caller.
//
// # The threat it addresses
//
// Same Property B posture as internal/llmproxy and internal/gitproxy:
// the processes that parse hostile agent output must hold no durable
// credentials, because anything they hold is one output-parsing bug or
// prompt-injection leak away from exposure. Their REST clients are
// therefore pointed at this proxy with a throwaway per-run placeholder
// token; the proxy authenticates the placeholder, strips it, and injects
// the real credential on the upstream hop. The real token lives only in
// the process running this proxy and never reaches the caller.
//
// # Provider shapes
//
// GitHub injects "Authorization: Bearer <token>" — the header form the
// REST API accepts and internal/github's client sends. Token selection
// is per repo: the dominant caller request shape is
// /repos/{owner}/{repo}/..., so the proxy parses that pair out of the
// path and hands it to Config.TokenSource — the same shape as the git
// proxy's source, so the wiring can reuse the per-repo scoped mint and
// the injected credential is narrowed to exactly the repo the request
// targets. Paths that carry no repo (/user, /search/..., webhook
// deliveries) resolve with empty owner/repo; the source owns the policy
// for what a default credential means there.
//
// Jira's two deployment flavors differ in auth SCHEME, not just token —
// Cloud is Basic base64(email:api_token) against REST v3, Data Center is
// Bearer <PAT> against REST v2 (see internal/jira). Rather than teach
// this proxy that split, Config.AuthHeaderSource returns the complete
// Authorization header value; the JiraBasic / JiraBearer constructors
// pin the two documented encodings so the wiring can't get them wrong.
//
// # Trust model on the local hop
//
// Identical to internal/llmproxy: in multi mode the proxy binds an
// address in the shared host namespace that a sibling run's processes
// can physically reach, so reaching the proxy proves nothing about who
// is calling. Every request must present the per-run IncomingToken
// (constant-time compared) before any credential is resolved or anything
// is forwarded; a sibling holds a *different* token, so even a packet
// that gets through the network layer cannot spend this run's
// credential. The per-run token is a capability scoped to one run's own
// proxy — generated fresh, destroyed at teardown — not a durable
// credential, so Property B is intact. Empty IncomingToken disables the
// check (the loopback/test path, or single-tenant direct usage where
// the local hop is already trusted).
package apiproxy

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Provider distinguishes the credential-injection shape for the upstream
// REST API.
type Provider string

const (
	// ProviderGitHub injects "Authorization: Bearer <token>" with
	// per-repo token selection via Config.TokenSource. Forwards to
	// api.github.com or a GHES REST base.
	ProviderGitHub Provider = "github"

	// ProviderJira injects the complete Authorization value produced by
	// Config.AuthHeaderSource — Basic for Cloud, Bearer for Data Center.
	ProviderJira Provider = "jira"
)

// TokenSource supplies the real GitHub token for one request's target
// repo. owner/repo are parsed from the request path when it carries the
// /repos/{owner}/{repo}/... shape; both are empty for paths that don't,
// and the source decides what credential (if any) backs those. An error
// (or an empty token) surfaces to the caller as a 502 — the proxy never
// forwards a request it could not authenticate upstream.
//
// Called once per request with no proxy-side caching, so implementations
// own their own reuse/refresh policy. Must be safe for concurrent use.
type TokenSource func(ctx context.Context, owner, repo string) (string, error)

// AuthHeaderSource supplies the complete Authorization header value for
// the Jira upstream — scheme included, e.g. "Basic <b64>" or
// "Bearer <pat>". Returning the full value (rather than a bare token
// plus a mode enum) keeps the Cloud-vs-Data-Center split out of the
// proxy; internal/jira already owns that mapping, and the JiraBasic /
// JiraBearer constructors cover the static cases. An error (or an empty
// value) surfaces as a 502.
//
// Called once per request with no proxy-side caching. Must be safe for
// concurrent use.
type AuthHeaderSource func(ctx context.Context) (string, error)

// JiraBasic builds an AuthHeaderSource for an Atlassian Cloud API token:
// Basic auth over email + token, the only scheme Cloud API tokens speak.
// The encoding matches internal/jira's basicAuth (http.Request.SetBasicAuth).
func JiraBasic(email, apiToken string) AuthHeaderSource {
	value := "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+apiToken))
	return func(context.Context) (string, error) { return value, nil }
}

// JiraBearer builds an AuthHeaderSource for a Jira Server / Data Center
// personal access token, matching internal/jira's bearerAuth.
func JiraBearer(pat string) AuthHeaderSource {
	value := "Bearer " + pat
	return func(context.Context) (string, error) { return value, nil }
}

// Config bundles the inputs a Server needs at construction. Per-run
// construction: each run gets its own Server with its own placeholder
// token and credential sources.
type Config struct {
	// Provider selects the credential-injection shape and which source
	// field below is consulted.
	Provider Provider

	// Upstream is the absolute URL of the real API — e.g.
	// "https://api.github.com", a GHES REST root "{ghes}/api/v3", or the
	// Jira Cloud gateway "https://api.atlassian.com/ex/jira/{cloud_id}". A
	// base PATH is honored (SetURL joins it ahead of the incoming request
	// path), which is what lets a caller point its REST client at the bare
	// proxy URL and reach a path-mounted upstream: the client sends
	// "/repos/o/r" or "/rest/api/3/…" and the proxy prepends the upstream's
	// "/api/v3" or "/ex/jira/{id}". Query / fragment are still rejected —
	// the incoming request owns those.
	Upstream string

	// TokenSource resolves the real GitHub token per request (see the
	// type doc). Required for ProviderGitHub; must be nil otherwise — a
	// config naming one provider but carrying the other's source is a
	// wiring bug best caught at construction.
	TokenSource TokenSource

	// AuthHeaderSource resolves the full Jira Authorization value per
	// request (see the type doc). Required for ProviderJira; must be nil
	// otherwise.
	AuthHeaderSource AuthHeaderSource

	// AllowNonLoopback opts into binding Start on a non-loopback address.
	// The security boundary on the local hop is per-run token auth
	// (IncomingToken) backed by network isolation — not "loopback-only",
	// which by itself never authenticated the caller. But an accidental
	// "0.0.0.0:NNNN" bind would still expose the proxy to the LAN, so
	// loopback-only-by-default remains the safety default and this flag
	// is the conscious opt-out. Binding a host-side veth IP is the
	// legitimate non-loopback use case.
	AllowNonLoopback bool

	// IncomingToken, when non-empty, is the per-run secret every request
	// must present before the proxy resolves a credential or forwards
	// anything. The caller generates a fresh random token per run, sets
	// it here, and configures that run's REST clients with the same value
	// as their credential — so it arrives as "Authorization: Bearer
	// <token>" (the GitHub client and Jira Data Center shapes) or as the
	// Basic-auth password (the Jira Cloud shape). The proxy compares the
	// presented value constant-time and returns 401 on mismatch.
	//
	// This is the fail-closed half of cross-run isolation: a sibling run
	// that reaches this proxy holds a *different* token, so it cannot
	// spend this run's credential. It is NOT a durable credential and
	// does not violate Property B — the real token still lives only in
	// the sources above and never reaches any caller.
	//
	// Empty disables the check (loopback/test usage, or single-tenant
	// direct paths where the local hop is already trusted).
	IncomingToken string
}

// Server is a single per-run proxy instance. Not safe to share across
// runs — the credential sources it holds are run-scoped, and the request
// counter is request-scoped.
type Server struct {
	cfg         Config
	upstreamURL *url.URL
	proxy       *httputil.ReverseProxy

	requestCount atomic.Int64

	// listener is owned once Start has been called. nil until then.
	listener net.Listener
	httpSrv  *http.Server
	// serveErr receives the first non-ErrServerClosed error from
	// httpSrv.Serve. Buffered(1) so the goroutine never blocks on send,
	// even if the caller never reads it.
	serveErr chan error
}

// New constructs a Server with the given config but does not start
// listening. Call Start to bind a port and begin serving.
//
// Validates config eagerly so a misconfigured caller fails at
// construction time rather than producing a Server that silently 5xx's
// every request.
func New(cfg Config) (*Server, error) {
	switch cfg.Provider {
	case ProviderGitHub:
		if cfg.TokenSource == nil {
			return nil, errors.New("apiproxy: TokenSource is required for ProviderGitHub")
		}
		if cfg.AuthHeaderSource != nil {
			return nil, errors.New("apiproxy: AuthHeaderSource is only for ProviderJira")
		}
	case ProviderJira:
		if cfg.AuthHeaderSource == nil {
			return nil, errors.New("apiproxy: AuthHeaderSource is required for ProviderJira")
		}
		if cfg.TokenSource != nil {
			return nil, errors.New("apiproxy: TokenSource is only for ProviderGitHub")
		}
	default:
		return nil, fmt.Errorf("apiproxy: unsupported provider %q", cfg.Provider)
	}

	u, err := parseUpstream(cfg.Upstream)
	if err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, upstreamURL: u}
	s.proxy = &httputil.ReverseProxy{
		Rewrite:        s.rewrite,
		ModifyResponse: s.modifyResponse,
	}
	return s, nil
}

// parseUpstream validates the upstream base URL: scheme + host only, and
// https unless loopback. The credential travels on the upstream hop (in
// the rewritten Authorization header) and must not cross cleartext;
// loopback http is permitted for httptest usage in unit tests.
func parseUpstream(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("apiproxy: Upstream is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("apiproxy: parse upstream URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("apiproxy: upstream URL missing scheme or host: %q", raw)
	}
	// A base path IS allowed (SetURL joins it ahead of the incoming path) so a
	// GHES "/api/v3" or a Jira Cloud "/ex/jira/{id}" upstream routes correctly
	// while callers point at the bare proxy URL. Query / fragment are not —
	// the incoming request owns those, and a base carrying them would
	// duplicate or corrupt the forwarded query.
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("apiproxy: upstream URL must not include query or fragment: %q", raw)
	}
	if u.Scheme != "https" {
		// u.Hostname() strips the port AND the IPv6 brackets, so it works
		// for "127.0.0.1:8080", "[::1]:8080", and "[::1]" alike.
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("apiproxy: upstream %q must use https (loopback http is allowed for tests)", raw)
		}
	}
	return u, nil
}

// authCtxKey is the request-context key used to hand the resolved
// Authorization value from the Handler wrapper to the Rewrite hook.
// Unexported empty struct so external code cannot collide.
type authCtxKey struct{}

// Handler exposes the proxy as an http.Handler. Useful for tests that
// drive the proxy via httptest.NewServer rather than the production
// Start path.
//
// The returned handler resolves the upstream credential before
// delegating to the underlying ReverseProxy: a source failure surfaces
// as a 502 here rather than via a silently-unauthenticated forward.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Per-run caller auth first, so an unauthorized caller costs no
		// credential resolution either. A sibling run (or any
		// unauthorized caller) gets 401 and never reaches the credential
		// pipeline.
		if !s.callerAuthorized(r) {
			// 401 without a WWW-Authenticate challenge: the caller is our
			// own REST client, not a browser, so there's no interactive
			// auth to negotiate. Terse body; the status is the signal.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		value, err := s.authHeaderValue(r.Context(), r.URL.Path)
		if err != nil {
			// 502 Bad Gateway maps cleanly: the proxy is alive but the
			// upstream credential pipeline is broken. Avoid leaking the
			// error detail to the caller — the underlying source error may
			// include identifying info about the credential setup.
			http.Error(w, "apiproxy: failed to resolve upstream credential", http.StatusBadGateway)
			return
		}
		// Stash the resolved value on the request context so Rewrite can
		// pick it up. Passing through context (rather than mutating
		// headers here) keeps the credential off the inbound request's
		// header set, which means it never appears in a log of
		// pr.In.Header.
		r = r.WithContext(context.WithValue(r.Context(), authCtxKey{}, value))
		s.proxy.ServeHTTP(w, r)
	})
}

// callerAuthorized reports whether the request presents the per-run
// IncomingToken. The placeholder arrives in whichever shape the caller's
// client emits — "Bearer <token>" (GitHub, Jira Data Center) or as the
// Basic-auth password (Jira Cloud) — so both are accepted; the Basic
// username is irrelevant and only the password is validated.
//
// subtle.ConstantTimeCompare can't be byte-probed for an equal-length
// wrong guess; it short-circuits on a length mismatch, which covers the
// missing-header (empty string) case and leaks nothing here because the
// token is a fixed-length secret whose length an attacker already knows.
// A Bearer header without the prefix fails the compare unchanged
// (TrimPrefix is a no-op on it).
//
// An empty IncomingToken disables the gate (loopback/test path).
func (s *Server) callerAuthorized(r *http.Request) bool {
	if s.cfg.IncomingToken == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	presented := strings.TrimPrefix(auth, "Bearer ")
	if strings.HasPrefix(auth, "Basic ") {
		_, presented, _ = r.BasicAuth()
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.IncomingToken)) == 1
}

// authHeaderValue resolves the real Authorization value for one request,
// provider-shaped: GitHub selects a token by the repo the path targets;
// Jira delegates the whole value to the source. An empty result is
// treated as a source failure — the proxy never forwards a request it
// could not authenticate upstream, because an anonymous GitHub call
// would not 401 but silently succeed against public data with wrong
// (unauthenticated, rate-limited) semantics.
func (s *Server) authHeaderValue(ctx context.Context, path string) (string, error) {
	switch s.cfg.Provider {
	case ProviderGitHub:
		owner, repo := repoFromPath(path)
		tok, err := s.cfg.TokenSource(ctx, owner, repo)
		if err != nil {
			return "", fmt.Errorf("token source: %w", err)
		}
		if tok == "" {
			return "", errors.New("token source returned empty token")
		}
		return "Bearer " + tok, nil
	case ProviderJira:
		value, err := s.cfg.AuthHeaderSource(ctx)
		if err != nil {
			return "", fmt.Errorf("auth header source: %w", err)
		}
		if value == "" {
			return "", errors.New("auth header source returned empty value")
		}
		return value, nil
	}
	// Unreachable: New rejects unknown providers at construction.
	return "", fmt.Errorf("apiproxy: unsupported provider %q", s.cfg.Provider)
}

// repoFromPath extracts the target repo from a /repos/{owner}/{repo}/...
// request path. Only that shape is parsed — it is the dominant caller
// shape, and guessing a repo out of anything else (e.g. the numeric
// /repositories/{id} form, which a per-repo mint couldn't scope anyway)
// would hand the token source a wrong answer. Everything else returns
// the empty pair and lets the source apply its default policy.
func repoFromPath(path string) (owner, repo string) {
	rest, ok := strings.CutPrefix(path, "/repos/")
	if !ok {
		return "", ""
	}
	owner, rest, _ = strings.Cut(rest, "/")
	repo, _, _ = strings.Cut(rest, "/")
	if owner == "" || repo == "" {
		return "", ""
	}
	return owner, repo
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
// response mid-body. Latching EOF makes the post-consumption drain read
// benign. A close that truncates a body NOT yet read to EOF still
// surfaces as an error, as it must — the latch only engages after every
// byte was delivered.
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
// runs before the request is sent upstream and has explicit control over
// Out — unlike Director, the stdlib does NOT add X-Forwarded-* after
// Rewrite returns unless we call pr.SetXForwarded() ourselves. That lets
// us suppress those proxy-chain headers, which would just be noise to
// the upstream API.
//
// The header rewrite uses Set (not Add) so the caller's placeholder
// Authorization is overwritten, not duplicated. A duplicate would
// confuse some HTTP/2 implementations and would absolutely confuse the
// upstream's auth path. Every other header (Accept, Content-Type,
// If-None-Match, ...) passes through untouched — REST semantics like
// media-type versioning and conditional requests belong to the caller.
func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	// SetURL rewrites Out.URL.Scheme, Out.URL.Host, joins the upstream's
	// base path ahead of the incoming request path, and sets Out.Host. A
	// path-less upstream (api.github.com) forwards the request path
	// verbatim; a path-mounted one (GHES /api/v3, Jira Cloud /ex/jira/{id})
	// prepends its base — see Config.Upstream.
	pr.SetURL(s.upstreamURL)

	// Shield the upstream exchange from the server's response-time close
	// of the shared request body (see eofLatchedBody).
	if pr.Out.Body != nil {
		pr.Out.Body = &eofLatchedBody{rc: pr.Out.Body}
	}

	// Defensive: if the incoming request happened to carry X-Forwarded-*
	// headers (some misconfigured caller), drop them. We deliberately do
	// not call pr.SetXForwarded().
	pr.Out.Header.Del("X-Forwarded-For")
	pr.Out.Header.Del("X-Forwarded-Host")
	pr.Out.Header.Del("X-Forwarded-Proto")
	pr.Out.Header.Del("Forwarded")

	// Strip proxy-auth headers explicitly. httputil.ReverseProxy already
	// removes hop-by-hop headers (RFC 7230 §6.1 includes
	// Proxy-Authorization and Proxy-Connection) but we belt-and-braces
	// this: forwarding Proxy-Authorization would leak those credentials
	// to the upstream.
	pr.Out.Header.Del("Proxy-Authorization")
	pr.Out.Header.Del("Proxy-Connection")

	value, _ := pr.In.Context().Value(authCtxKey{}).(string)
	if value == "" {
		// Defense in depth: if the Handler wrapper somehow skipped us,
		// fail closed by stripping the caller-supplied auth so the
		// request goes anonymous (which the upstream will reject) rather
		// than passing through the placeholder.
		pr.Out.Header.Del("Authorization")
		return
	}
	pr.Out.Header.Set("Authorization", value)
}

// modifyResponse is a hook for observability and per-request counter
// bumping. Returning an error here would 502 the client; we never do
// that — just observe.
func (s *Server) modifyResponse(resp *http.Response) error {
	s.requestCount.Add(1)
	return nil
}

// Start binds a TCP port and serves until Shutdown is called. Returns
// the bound address so the caller can construct the client-side base
// URL.
//
// Defaults to 127.0.0.1:0 (random loopback port) when addr is "". A
// non-loopback bind requires Config.AllowNonLoopback=true — an
// accidental "0.0.0.0:NNNN" would expose the proxy to the LAN, where
// (when IncomingToken is set) the per-run token is the only barrier to
// the real credential, and (when it isn't) there is none. Binding a
// host-side veth IP is the legitimate non-loopback use case and opts in
// via the Config flag.
func (s *Server) Start(addr string) (string, error) {
	if s.httpSrv != nil {
		return "", errors.New("apiproxy: already started; create a new Server per run")
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
		return "", fmt.Errorf("apiproxy: listen on %s: %w", addr, err)
	}
	s.listener = ln
	s.serveErr = make(chan error, 1)
	s.httpSrv = &http.Server{
		Handler: s.Handler(),
		// Conservative timeouts. ReadHeaderTimeout limits the time to
		// receive the request headers, not the body. Total request time
		// is unbounded (no WriteTimeout) because large paginated
		// responses and slow uploads can run long.
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
// channel is closed when the goroutine exits, so a range or select on it
// unblocks after Shutdown.
//
// Callers that do not need to monitor the error can ignore this channel
// safely — it is buffered and the goroutine never blocks on send.
func (s *Server) Err() <-chan error { return s.serveErr }

// Shutdown stops serving and waits for in-flight requests to drain (up
// to the context's deadline). Idempotent.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

// RequestCount returns the number of upstream responses the proxy has
// observed. Useful for tests asserting the caller actually went through
// the proxy.
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
		return fmt.Errorf("apiproxy: parse bind address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("apiproxy: bind address %q binds to all interfaces — set AllowNonLoopback=true to confirm intent", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("apiproxy: bind address %q is not loopback — set AllowNonLoopback=true to confirm intent", addr)
		}
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("apiproxy: resolve %q: %w", host, err)
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("apiproxy: bind host %q resolves to %s (not loopback) — set AllowNonLoopback=true to confirm intent", host, a)
		}
	}
	return nil
}
