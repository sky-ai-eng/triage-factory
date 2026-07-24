// Package ghinjector is the per-run credential-injector proxy for the real
// `gh` CLI (TFAC-669). It is the git-proxy pattern extended to gh: the agent's
// gh talks to this listener via GH_HOST (base-URL redirect) holding only a
// per-run placeholder token; the injector strips the placeholder and injects
// the run's real GitHub credential on the upstream hop. The real credential —
// an App-org's single team-repo-set-scoped installation token, or the org PAT —
// lives only in the capless sidecar that runs this proxy and never enters the
// jail's env, argv, or any file (placeholder only).
//
// # No traffic policy
//
// The credential carries the policy: an App-org token is minted scoped to the
// team's authorized repo set, so a request for a repo outside that set gets
// GitHub's own 404 masking, verbatim. The injector therefore does NO path
// allowlist and NO GraphQL inspection — GraphQL opacity is irrelevant because
// the token, not the traffic, is what's bounded. It only (a) maps the two
// GHE-shaped path prefixes gh emits to the org's real API and (b) selects the
// one credential to inject. That is the whole of its logic.
//
// # TLS, but no interception
//
// gh forces https to any GH_HOST and rejects scheme prefixes, so the injector
// serves TLS with a per-run self-signed certificate (GenerateCert), published to
// the jail through a bind-mounted trust file SSL_CERT_FILE points at
// (TrustBundlePEM — the system roots plus this run's leaf, since that env var is
// process-global). There is no TLS interception of GitHub anywhere — the
// injector terminates gh's connection with its own per-run cert and opens its
// own TLS connection to the real upstream.
//
// # Path surface
//
// gh, treating GH_HOST as a GitHub Enterprise host, emits exactly two prefixes:
// REST under /api/v3/* and GraphQL at /api/graphql (spike-verified across gh
// api / pr view / pr list / pr diff / mutations). The injector maps /api/v3/<x>
// to the REST upstream and /api/graphql to the sibling GraphQL endpoint derived
// from it, mirroring internal/github's own derivation.
//
// # Observation
//
// Exec-verb self-reporting doesn't exist on this channel, so the injector emits
// an observation for the two artifact-bearing REST mutations gh performs —
// POST .../pulls (PR created) and POST .../pulls/{n}/reviews (review posted) —
// via a caller-supplied callback the sidecar turns into an orchestrator relay.
// GraphQL is never inspected (gh does these via REST).
package ghinjector

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// restPrefix is the GHE REST path prefix gh emits; graphqlPath is the GraphQL
// path. These are the only two surfaces the injector routes (spike finding 4).
const (
	restPrefix  = "/api/v3"
	graphqlPath = "/api/graphql"
)

// maxObserveBody caps how much of an observed mutation's response body the
// injector will BUFFER to parse the created object's coordinates. PR-create and
// review-post responses are a few KB; the cap only guards against a pathological
// upstream. A body past the cap is never truncated — the consumed prefix is
// stitched back in front of the untouched remainder and observation is skipped
// for that response (the reconciler backstop covers it). The agent's response is
// always delivered whole; observation is the only thing that degrades.
const maxObserveBody = 1 << 20

// TokenSource supplies the single real GitHub credential to inject on every
// request — the team-set-scoped installation token (App orgs) or the org PAT.
// Read once per request with no proxy-side caching so a mid-run brain re-seal is
// picked up; an error (or empty token) surfaces to the agent as a 502, never a
// silently-unauthenticated forward.
type TokenSource func(ctx context.Context) (string, error)

// ObservedMutation is one artifact-bearing REST mutation the injector saw
// complete successfully. The sidecar relays it to the orchestrator, which owns
// the DB and builds the artifact row (the injector holds no DB handle and no
// domain types). Kind is "pull_request" or "review".
type ObservedMutation struct {
	Kind   string
	Owner  string
	Repo   string
	Number int // PR number (from the response for a PR; from the path for a review)

	// PR-create fields.
	NodeID string
	Head   string
	Base   string
	URL    string
	Title  string
	Body   string
	Draft  bool

	// Review-post fields.
	ReviewID    int
	ReviewState string // GitHub's review state, e.g. APPROVED / COMMENTED / CHANGES_REQUESTED
}

// Config bundles the per-run injector inputs. One Server per run.
type Config struct {
	// Upstream is the org's real REST API base — "https://api.github.com" or a
	// GHES "{host}/api/v3". The GraphQL endpoint is derived from it. Required.
	Upstream string

	// TokenSource resolves the single real credential to inject. Required.
	TokenSource TokenSource

	// IncomingToken is the per-run placeholder every request must present (gh
	// sends it as "Authorization: token <placeholder>"). Constant-time compared;
	// a sibling run holds a different one and so cannot spend this run's
	// credential. Empty disables the check (loopback/test path).
	IncomingToken string

	// Cert is the per-run self-signed certificate the injector serves TLS with
	// (from GenerateCert). Required — gh forces https.
	Cert tls.Certificate

	// Observe, when non-nil, is called for each PR-create / review-post the
	// injector sees complete. Best-effort and out of the response's critical
	// path shape (called after the body is buffered, before it streams on).
	Observe func(ctx context.Context, m ObservedMutation)

	// AllowNonLoopback opts into binding a non-loopback address (the veth IP the
	// sandbox reaches). Loopback-only by default, exactly like the sibling
	// proxies.
	AllowNonLoopback bool
}

// Server is one per-run injector instance.
type Server struct {
	cfg        Config
	restURL    *url.URL
	graphqlURL *url.URL
	proxy      *httputil.ReverseProxy

	requestCount atomic.Int64

	listener net.Listener
	httpSrv  *http.Server
	serveErr chan error
}

// New validates the config and constructs a Server without listening.
func New(cfg Config) (*Server, error) {
	if cfg.TokenSource == nil {
		return nil, errors.New("ghinjector: TokenSource is required")
	}
	rest, err := parseUpstream(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	gql, err := url.Parse(graphqlUpstream(cfg.Upstream))
	if err != nil {
		return nil, fmt.Errorf("ghinjector: derive graphql upstream: %w", err)
	}

	s := &Server{cfg: cfg, restURL: rest, graphqlURL: gql}
	s.proxy = &httputil.ReverseProxy{
		Rewrite:        s.rewrite,
		ModifyResponse: s.modifyResponse,
	}
	return s, nil
}

// parseUpstream validates the REST base: scheme + host, https unless loopback.
func parseUpstream(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("ghinjector: Upstream is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("ghinjector: parse upstream %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("ghinjector: upstream %q missing scheme or host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("ghinjector: upstream %q must not include query or fragment", raw)
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("ghinjector: upstream %q must use https (loopback http allowed for tests)", raw)
		}
	}
	return u, nil
}

// graphqlUpstream derives the GraphQL endpoint from the REST base:
//
//	https://api.github.com      → https://api.github.com/graphql
//	https://<host>/api/v3       → https://<host>/api/graphql
//
// Dotcom is detected by an EXACT hostname compare, not a substring test: a
// GHES host that merely contains the dotcom name (api.github.com.example.com,
// or a lookalike an org mis-configures) must keep routing GraphQL to its own
// host, never to github.com. A base that won't parse falls through to the
// sibling-path derivation, which is the safe default (it stays on whatever host
// the REST base named).
func graphqlUpstream(restBase string) string {
	trimmed := strings.TrimRight(restBase, "/")
	if u, err := url.Parse(trimmed); err == nil && strings.EqualFold(u.Hostname(), dotcomAPIHost) {
		return "https://api.github.com/graphql"
	}
	return strings.TrimSuffix(trimmed, "/v3") + "/graphql"
}

// dotcomAPIHost is the public GitHub REST API host, matched exactly.
const dotcomAPIHost = "api.github.com"

// authCtxKey carries the resolved upstream Authorization value from the Handler
// wrapper to the Rewrite hook (off the inbound header set, so it never logs).
type authCtxKey struct{}

// Handler exposes the proxy as an http.Handler (production Start wraps it in
// TLS; tests can drive it via httptest.NewUnstartedServer + StartTLS).
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.callerAuthorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		tok, err := s.cfg.TokenSource(r.Context())
		if err != nil || tok == "" {
			// 502: the proxy is alive but the credential pipeline is broken.
			// Never leak the source error (may carry credential-setup detail).
			http.Error(w, "ghinjector: failed to resolve upstream credential", http.StatusBadGateway)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), authCtxKey{}, tok))
		s.proxy.ServeHTTP(w, r)
	})
}

// callerAuthorized reports whether the request presents the per-run placeholder.
// gh sends "Authorization: token <placeholder>"; Bearer and Basic are also
// accepted defensively. An empty IncomingToken disables the gate (test path).
func (s *Server) callerAuthorized(r *http.Request) bool {
	if s.cfg.IncomingToken == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	presented := auth
	switch {
	case strings.HasPrefix(auth, "token "):
		presented = strings.TrimPrefix(auth, "token ")
	case strings.HasPrefix(auth, "Bearer "):
		presented = strings.TrimPrefix(auth, "Bearer ")
	case strings.HasPrefix(auth, "Basic "):
		_, presented, _ = r.BasicAuth()
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.IncomingToken)) == 1
}

// rewrite maps the GHE-shaped request onto the org's real API and swaps in the
// real credential. No traffic policy: every request is forwarded; only the base
// URL and the Authorization header change.
func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	inPath := pr.In.URL.Path
	if inPath == graphqlPath || strings.HasPrefix(inPath, graphqlPath+"/") {
		setTarget(pr, s.graphqlURL, "")
	} else {
		rest := strings.TrimPrefix(inPath, restPrefix)
		setTarget(pr, s.restURL, rest)
	}

	// Suppress proxy-chain headers (we deliberately do not SetXForwarded).
	pr.Out.Header.Del("X-Forwarded-For")
	pr.Out.Header.Del("X-Forwarded-Host")
	pr.Out.Header.Del("X-Forwarded-Proto")
	pr.Out.Header.Del("Forwarded")
	pr.Out.Header.Del("Proxy-Authorization")
	pr.Out.Header.Del("Proxy-Connection")

	tok, _ := pr.In.Context().Value(authCtxKey{}).(string)
	if tok == "" {
		// Fail closed: strip the caller placeholder so the request goes
		// anonymous (upstream rejects) rather than forwarding the placeholder.
		pr.Out.Header.Del("Authorization")
		return
	}
	// gh's native scheme for installation tokens; api.github.com and GHES both
	// accept "token <ghs_...>".
	pr.Out.Header.Set("Authorization", "token "+tok)
}

// setTarget points the outgoing request at base with base's path joined ahead
// of extraPath, and fixes the Host header. Mirrors the effect of SetURL but
// lets the injector strip the /api/v3 prefix and pick REST vs GraphQL per
// request.
func setTarget(pr *httputil.ProxyRequest, base *url.URL, extraPath string) {
	pr.Out.URL.Scheme = base.Scheme
	pr.Out.URL.Host = base.Host
	pr.Out.URL.Path = singleJoiningSlash(base.Path, extraPath)
	// RawPath left empty so net/http re-derives escaping from Path; the incoming
	// query is preserved on Out.URL.RawQuery by ReverseProxy.
	pr.Out.URL.RawPath = ""
	pr.Out.Host = base.Host
	pr.Out.Header.Set("Host", base.Host)
}

// singleJoiningSlash joins a and b with exactly one slash between them (the
// stdlib httputil helper, inlined since it is unexported).
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash && b != "":
		return a + "/" + b
	}
	return a + b
}

// modifyResponse bumps the request counter and, for the two observed mutation
// shapes, buffers+parses the response body to emit an observation before
// streaming it on unchanged. Never returns an error (that would 502 the agent
// on a successful upstream write) — observation failures are silently dropped
// and left to the reconciler backstop.
func (s *Server) modifyResponse(resp *http.Response) error {
	s.requestCount.Add(1)
	if s.cfg.Observe == nil || resp.Request == nil || resp.Request.Method != http.MethodPost {
		return nil
	}
	kind, owner, repo, number, ok := classifyMutationPath(resp.Request.URL.Path)
	if !ok || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}

	// Read one byte past the cap so "body exceeded the cap" is distinguishable
	// from "body is exactly the cap" (ReadAll on a LimitReader reports no error
	// when it stops at the limit).
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxObserveBody+1))
	if err != nil || len(buf) > maxObserveBody {
		// The read broke mid-stream, or the body is bigger than we will buffer.
		// Either way the agent's response must arrive intact: put the consumed
		// prefix back in front of the untouched remainder (keeping the original
		// body as the Closer, since it is still open) and skip observation.
		resp.Body = &prefixedBody{r: io.MultiReader(bytes.NewReader(buf), resp.Body), c: resp.Body}
		return nil
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(buf))

	if m, parsed := parseObservation(kind, owner, repo, number, buf); parsed && s.cfg.Observe != nil {
		s.cfg.Observe(context.Background(), m)
	}
	return nil
}

// prefixedBody re-presents an already-partially-consumed response body: reads
// deliver the buffered prefix followed by the live remainder, while Close still
// closes the original body (which was never closed, so the remainder is intact).
type prefixedBody struct {
	r io.Reader
	c io.Closer
}

func (b *prefixedBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *prefixedBody) Close() error               { return b.c.Close() }

// classifyMutationPath recognizes the two artifact-bearing POST paths on the
// upstream request URL (post-rewrite, so REST base already prepended): a PR
// create (.../repos/{o}/{r}/pulls) or a review post
// (.../repos/{o}/{r}/pulls/{n}/reviews). Returns the kind + coordinates parsed
// from the path; number is 0 for a PR create (assigned in the response).
func classifyMutationPath(path string) (kind, owner, repo string, number int, ok bool) {
	i := strings.Index(path, "/repos/")
	if i < 0 {
		return "", "", "", 0, false
	}
	segs := strings.Split(strings.Trim(path[i+len("/repos/"):], "/"), "/")
	// pulls:            owner repo pulls
	// reviews:          owner repo pulls {n} reviews
	if len(segs) == 3 && segs[2] == "pulls" && segs[0] != "" && segs[1] != "" {
		return "pull_request", segs[0], segs[1], 0, true
	}
	if len(segs) == 5 && segs[2] == "pulls" && segs[4] == "reviews" {
		n, err := strconv.Atoi(segs[3])
		if err != nil || segs[0] == "" || segs[1] == "" {
			return "", "", "", 0, false
		}
		return "review", segs[0], segs[1], n, true
	}
	return "", "", "", 0, false
}

// parseObservation extracts the created object's coordinates from a mutation's
// JSON response body.
func parseObservation(kind, owner, repo string, number int, body []byte) (ObservedMutation, bool) {
	switch kind {
	case "pull_request":
		var pr struct {
			Number  int    `json:"number"`
			NodeID  string `json:"node_id"`
			HTMLURL string `json:"html_url"`
			Title   string `json:"title"`
			Body    string `json:"body"`
			Draft   bool   `json:"draft"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		}
		if err := json.Unmarshal(body, &pr); err != nil || pr.Number == 0 {
			return ObservedMutation{}, false
		}
		return ObservedMutation{
			Kind:   "pull_request",
			Owner:  owner,
			Repo:   repo,
			Number: pr.Number,
			NodeID: pr.NodeID,
			Head:   pr.Head.Ref,
			Base:   pr.Base.Ref,
			URL:    pr.HTMLURL,
			Title:  pr.Title,
			Body:   pr.Body,
			Draft:  pr.Draft,
		}, true
	case "review":
		var rv struct {
			ID      int    `json:"id"`
			HTMLURL string `json:"html_url"`
			State   string `json:"state"`
		}
		if err := json.Unmarshal(body, &rv); err != nil || rv.ID == 0 {
			return ObservedMutation{}, false
		}
		return ObservedMutation{
			Kind:        "review",
			Owner:       owner,
			Repo:        repo,
			Number:      number,
			ReviewID:    rv.ID,
			ReviewState: rv.State,
			URL:         rv.HTMLURL,
		}, true
	}
	return ObservedMutation{}, false
}

// Start binds addr and serves TLS with the per-run cert until Shutdown. Returns
// the bound "host:port" (no scheme) — the value GH_HOST is set to.
func (s *Server) Start(addr string) (string, error) {
	if s.httpSrv != nil {
		return "", errors.New("ghinjector: already started; create a new Server per run")
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
		return "", fmt.Errorf("ghinjector: listen on %s: %w", addr, err)
	}
	s.listener = ln
	s.serveErr = make(chan error, 1)
	s.httpSrv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{s.cfg.Cert}},
	}
	go func() {
		// Certs already live in TLSConfig, so ServeTLS's file args are empty.
		err := s.httpSrv.ServeTLS(ln, "", "")
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr <- err
		}
		close(s.serveErr)
	}()
	return ln.Addr().String(), nil
}

// Err returns the first unexpected Serve error (channel closes on exit).
func (s *Server) Err() <-chan error { return s.serveErr }

// Shutdown stops serving and drains in-flight requests. Idempotent.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

// RequestCount returns how many upstream responses the proxy has observed.
func (s *Server) RequestCount() int64 { return s.requestCount.Load() }

// assertLoopback mirrors apiproxy.assertLoopback: nil iff addr binds loopback.
func assertLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ghinjector: parse bind address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("ghinjector: bind address %q binds all interfaces — set AllowNonLoopback=true to confirm intent", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("ghinjector: bind address %q is not loopback — set AllowNonLoopback=true", addr)
		}
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("ghinjector: resolve %q: %w", host, err)
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("ghinjector: bind host %q resolves to non-loopback %s — set AllowNonLoopback=true", host, a)
		}
	}
	return nil
}
