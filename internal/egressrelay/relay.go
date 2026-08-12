// Package egressrelay is the sandbox egress "fetch relay" lane: a per-run,
// host-side HTTP service that fetches from a fixed set of public upstreams
// on the jail's behalf, resolving redirects on the host so the jail never
// needs the redirect targets reachable.
//
// It exists for exactly one class of upstream: registries that serve
// artifacts via redirects into SHARED multi-tenant storage hostnames.
// proxy.golang.org is the canonical case — it 302s large module zips (and
// every toolchain) to storage.googleapis.com. The tunnel lane
// (internal/egressproxy) cannot admit that hop: it is CONNECT-only, so it
// matches hostnames and never paths, and a shared storage namespace admits
// every tenant's bucket alongside the registry's — a writable,
// unattributable exfiltration channel for code whose job is reading
// hostile PR text. Registries on dedicated hostnames do NOT belong here;
// they get a tunnel-lane allowlist entry instead. The decision procedure
// and the contributor checklist live in this package's CLAUDE.md and in
// docs/security/sandbox-egress.md.
//
// The engine is generic: a relay is an ordered route table (Config.Routes)
// mapping path prefixes to upstreams, plus the env entries that aim the
// in-jail tool at it. Ecosystem knowledge lives in catalog.go as data, not
// in this file.
//
// Two consequences of the shape are deliberate, because both look like
// oversights otherwise:
//
//   - It follows redirects, which is the entire point — and therefore
//     dials every hop through egressproxy.VettedDialContext. A host-side
//     fetcher acting on a sandbox's behalf is a confused deputy by
//     construction; without the denylist at the dial, an upstream redirect
//     to the cloud metadata endpoint would be fetched with the host's
//     reach and streamed into the jail.
//   - It carries no per-run token, unlike the LLM and git proxies. Those
//     hold real credentials; this holds none and relays only what a public
//     upstream would serve anyway. Cross-run reach is already denied at L3
//     (each run's egress is pinned to its own gateway, and the denylist
//     covers the sandbox pool and sibling gateways), which is
//     proportionate for a credential-free relay.
package egressrelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/egressproxy"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

var relayLog = logging.Component("egressrelay")

const (
	// maxRedirects caps the redirect chain. The real chains are one hop
	// (registry → CDN); anything longer is a malfunctioning or hostile
	// upstream, and every hop costs a vetted dial.
	maxRedirects = 5

	// responseHeaderTimeout bounds how long an upstream may take to send
	// response HEADERS. Body transfer is deliberately unbounded — artifact
	// bodies reach tens of MB and a slow link may legitimately take
	// minutes, so a whole-request deadline would fail exactly the large
	// fetches this lane exists to make work.
	responseHeaderTimeout = 60 * time.Second
)

// Route is one entry in a relay's ordered route table. The first matching
// route wins, so put narrow prefixes before broad ones. Exactly one
// behavior applies per route:
//
//   - SyntheticStatus != 0: answer locally with that status and an empty
//     body, contacting no upstream. For capability probes an upstream
//     cannot answer (the Go sumdb /supported endpoint).
//   - Upstream != "": forward to that upstream, optionally stripping
//     Prefix from the path first.
//   - neither: a fence — refuse with 404 (and DenyMessage, when set) so a
//     namespace does not fall through to a broader route where it would
//     fail for a misleading reason.
type Route struct {
	// Prefix matches against the ESCAPED request path. Must begin with
	// "/". Include a trailing "/" yourself when a path-segment boundary
	// matters — matching is plain prefix matching.
	Prefix string

	// Exact requires the full path to equal Prefix instead of merely
	// beginning with it.
	Exact bool

	// Upstream is the scheme://host to forward to. Only scheme + host —
	// a path, query, or fragment is rejected at construction because it
	// would silently corrupt every forwarded URL.
	Upstream string

	// StripPrefix removes Prefix from the forwarded path (a leading "/"
	// is restored if stripping consumed it).
	StripPrefix bool

	// SyntheticStatus, when non-zero, answers locally with this status.
	SyntheticStatus int

	// DenyMessage is the response body for a fence route. Optional.
	DenyMessage string
}

// Config is the per-run construction input for one relay.
type Config struct {
	// Name identifies the relay in logs and shutdown errors, e.g.
	// "go-modules". Required.
	Name string

	// Routes is the ordered route table. Required, non-empty.
	Routes []Route

	// AllowNonLoopback opts into binding Start on a non-loopback address.
	// The relay is unauthenticated on the local hop; the boundary is
	// "only this run's sandbox can route here", enforced by network
	// isolation. The sandbox case (binding the host-side veth IP) is the
	// legitimate use and opts in here, so an accidental 0.0.0.0 bind
	// still fails.
	AllowNonLoopback bool

	// DialContext replaces the vetted dialer. Production leaves it nil
	// and gets egressproxy.VettedDialContext. Tests set it, because the
	// denylist blocks loopback and an httptest upstream lives there —
	// which is the gate working, not a test-harness problem.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// route is the compiled form of a Route: upstream parsed once at
// construction so ServeHTTP never re-parses or re-validates.
type route struct {
	Route
	upstream *url.URL // nil for synthetic and fence routes
}

// Server is one run's relay for one catalog entry.
type Server struct {
	cfg    Config
	routes []route
	client *http.Client

	listener net.Listener
	httpSrv  *http.Server
	serveErr chan error
}

// New compiles and validates cfg but does not listen. Call Start to bind.
func New(cfg Config) (*Server, error) {
	if cfg.Name == "" {
		return nil, errors.New("egressrelay: Config.Name is required")
	}
	if len(cfg.Routes) == 0 {
		return nil, fmt.Errorf("egressrelay: %s: at least one route is required", cfg.Name)
	}
	routes := make([]route, 0, len(cfg.Routes))
	for i, r := range cfg.Routes {
		if !strings.HasPrefix(r.Prefix, "/") {
			return nil, fmt.Errorf("egressrelay: %s: route %d prefix %q must begin with /", cfg.Name, i, r.Prefix)
		}
		if r.SyntheticStatus != 0 && r.Upstream != "" {
			return nil, fmt.Errorf("egressrelay: %s: route %d sets both SyntheticStatus and Upstream", cfg.Name, i)
		}
		// Range-checked here because ServeHTTP hands the value straight to
		// WriteHeader, which panics outside 100-999 — and that panic is
		// per-REQUEST (net/http recovers it per connection), so a typo'd
		// status would pass construction and then kill every request to
		// the route with a bare EOF. Every other malformed-config mistake
		// fails loudly at New; this one must too.
		if r.SyntheticStatus != 0 && (r.SyntheticStatus < 100 || r.SyntheticStatus > 999) {
			return nil, fmt.Errorf("egressrelay: %s: route %d synthetic status %d is not a valid HTTP status", cfg.Name, i, r.SyntheticStatus)
		}
		compiled := route{Route: r}
		if r.Upstream != "" {
			u, err := parseUpstream(cfg.Name, r.Upstream)
			if err != nil {
				return nil, err
			}
			compiled.upstream = u
		}
		routes = append(routes, compiled)
	}

	dial := cfg.DialContext
	if dial == nil {
		dial = egressproxy.VettedDialContext
	}

	return &Server{
		cfg:    cfg,
		routes: routes,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:           dial,
				ResponseHeaderTimeout: responseHeaderTimeout,
				// A hand-built Transport has NO idle timeout (unlike
				// http.DefaultTransport's 90s), so idle upstream
				// connections — and their goroutine pairs — would live
				// until the upstream drops them. Shutdown closes them
				// explicitly; this bounds them while the relay is up.
				IdleConnTimeout:   90 * time.Second,
				ForceAttemptHTTP2: true,
			},
			// Redirects are followed HERE, host-side — the reason this
			// lane exists. The per-hop denylist check rides on DialContext
			// above, so this policy only bounds the chain and refuses a
			// TLS DOWNGRADE mid-chain. (It does not demand https
			// unconditionally: a deliberately-http upstream — a local
			// mirror, a test server — is a configuration the operator
			// chose with eyes open.)
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("egressrelay: stopped after %d redirects", maxRedirects)
				}
				if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
					return fmt.Errorf("egressrelay: refusing https→%s downgrade redirect to %q", req.URL.Scheme, req.URL.Redacted())
				}
				return nil
			},
		},
	}, nil
}

// Name returns the relay's catalog name, for shutdown errors and logs.
func (s *Server) Name() string { return s.cfg.Name }

// parseUpstream validates an upstream: absolute, http(s), host present,
// and no path/query/fragment.
func parseUpstream(name, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("egressrelay: %s: parse upstream %q: %w", name, raw, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("egressrelay: %s: upstream %q must be http or https", name, raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("egressrelay: %s: upstream %q has no host", name, raw)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("egressrelay: %s: upstream %q must be scheme://host only (no path, query, or fragment)", name, raw)
	}
	// Userinfo is rejected because url.Parse puts everything before the
	// last "@" into User and everything after into Host — so an upstream
	// that READS as one host dials another ("https://proxy.golang.org@evil.com"
	// dials evil.com). Today every upstream is a compile-time catalog
	// constant, but this validator is the only check between an upstream
	// string and a host the relay fetches from with the host's network
	// reach, and per-profile rules (TFAC-408) will eventually feed it
	// non-source-code strings.
	if u.User != nil {
		return nil, fmt.Errorf("egressrelay: %s: upstream %q must not carry userinfo (everything after its last @ would be dialed as the host)", name, raw)
	}
	u.Path = ""
	return u, nil
}

// ServeHTTP routes by hand on the ESCAPED path rather than through
// http.ServeMux, deliberately. Artifact paths carry encodings the
// protocols depend on (Go's "!"-case-encoding, "@" and "+incompatible"
// in versions); ServeMux's path cleaning and redirecting would mangle
// exactly those, and the corruption would surface as a confusing
// "not found" rather than an obvious routing bug.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every relayed protocol is read-only. Anything else is a client bug
	// or a probe; either way it must not reach an upstream.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "egressrelay: only GET and HEAD are supported", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.EscapedPath()
	for _, rt := range s.routes {
		if rt.Exact {
			if path != rt.Prefix {
				continue
			}
		} else if !strings.HasPrefix(path, rt.Prefix) {
			continue
		}

		switch {
		case rt.SyntheticStatus != 0:
			w.WriteHeader(rt.SyntheticStatus)
		case rt.upstream != nil:
			s.relay(w, r, rt.upstream, forwardPath(path, rt))
		default:
			msg := rt.DenyMessage
			if msg == "" {
				msg = "egressrelay: path not served by this relay"
			}
			http.Error(w, msg, http.StatusNotFound)
		}
		return
	}
	http.Error(w, "egressrelay: no route matches", http.StatusNotFound)
}

// forwardPath applies a route's StripPrefix, restoring the leading "/"
// when stripping consumed it.
func forwardPath(path string, rt route) string {
	if !rt.StripPrefix {
		return path
	}
	p := strings.TrimPrefix(path, rt.Prefix)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// relay performs the host-side fetch and streams the result back.
//
// It builds a FRESH outbound request rather than mutating the inbound one.
// That keeps three separate hazards from arising at all: a server-side
// request carries RequestURI, which http.Client rejects outright; reusing
// the inbound URL risks re-encoding the escaped path; and forwarding
// inbound headers would carry whatever the jail sent — Authorization
// included — to a public upstream. Copying nothing means there is no
// header-stripping step to get wrong later.
func (s *Server) relay(w http.ResponseWriter, r *http.Request, target *url.URL, escapedPath string) {
	outURL, err := url.Parse(target.Scheme + "://" + target.Host + escapedPath)
	if err != nil {
		http.Error(w, "egressrelay: bad request path", http.StatusBadRequest)
		return
	}
	outURL.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), nil)
	if err != nil {
		http.Error(w, "egressrelay: build upstream request", http.StatusInternalServerError)
		return
	}

	resp, err := s.client.Do(req)
	if err != nil {
		// A denylisted redirect target lands here. Log it — this is the
		// confused-deputy gate firing, which is worth seeing — but keep
		// the client-visible body generic.
		relayLog.Info("upstream fetch failed", "relay", s.cfg.Name, "url", outURL.Redacted(), "err", err)
		http.Error(w, "egressrelay: upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	// Status is passed through verbatim, including 404 and 410: clients
	// read those to decide an artifact is absent (cmd/go additionally
	// advances its GOPROXY list on them). Rewriting them to 502 would
	// turn "not found" into "relay broken".
	w.WriteHeader(resp.StatusCode)

	// Streamed, never buffered — artifact bodies reach tens of MB and the
	// executor runs under a memory budget.
	//
	// A cancelled copy is normal, not a fault: clients fetch speculatively
	// (cmd/go's sumdb tiles) and drop what they turn out not to need,
	// which cancels the request mid-body. Logging those would put a line
	// in the executor log on every build for something working as
	// intended.
	if _, err := io.Copy(w, resp.Body); err != nil && !errors.Is(err, context.Canceled) {
		relayLog.Info("relay body truncated", "relay", s.cfg.Name, "url", outURL.Redacted(), "err", err)
	}
}

// hopByHopHeaders are meaningful only for a single transport hop and must
// not be forwarded to the client.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	for _, h := range hopByHopHeaders {
		dst.Del(h)
	}
}

// Handler exposes the relay for in-process wiring and tests.
func (s *Server) Handler() http.Handler { return s }

// Start binds addr ("host:port"; port 0 lets the kernel pick) and serves
// in a background goroutine. Returns the bound address.
func (s *Server) Start(addr string) (string, error) {
	if s.httpSrv != nil {
		return "", fmt.Errorf("egressrelay: %s already started; create a new Server per run", s.cfg.Name)
	}
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if !s.cfg.AllowNonLoopback {
		if err := assertLoopback(s.cfg.Name, addr); err != nil {
			return "", err
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("egressrelay: %s: listen on %s: %w", s.cfg.Name, addr, err)
	}
	s.listener = ln
	s.serveErr = make(chan error, 1)
	s.httpSrv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: a large artifact download legitimately
		// outlasts any fixed deadline, same reasoning as the git proxy's
		// pack transfers.
	}
	// No recover: net/http recovers a handler panic per connection, so nothing
	// a request does reaches this frame; Serve itself only accepts, and the
	// buffered send + close cannot panic (one sender, one close).
	go func() {
		err := s.httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr <- err
		}
		close(s.serveErr)
	}()
	return ln.Addr().String(), nil
}

// assertLoopback rejects a bind address that isn't loopback, so an
// unauthenticated relay can't be exposed beyond the host by accident.
func assertLoopback(name, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("egressrelay: %s: parse bind address %q: %w", name, addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("egressrelay: %s: refusing to bind non-loopback %q without Config.AllowNonLoopback", name, addr)
	}
	return nil
}

// Err returns a channel receiving the first unexpected Serve error.
// Closed when the serve goroutine exits, so a receive unblocks after
// Shutdown.
func (s *Server) Err() <-chan error { return s.serveErr }

// Shutdown stops the server. Safe on a never-started Server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.httpSrv == nil {
		return nil
	}
	err := s.httpSrv.Shutdown(ctx)
	// The listener is the server half; the client Transport's pooled
	// upstream connections are the other half, and nothing else ever
	// closes them. Without this, every relay a long-lived process starts
	// leaves its idle sockets and read/write goroutines behind until the
	// UPSTREAM drops them — Shutdown means "this relay is done", not
	// "this relay's listener is done".
	s.client.CloseIdleConnections()
	if err != nil {
		return fmt.Errorf("egressrelay: %s: shutdown: %w", s.cfg.Name, err)
	}
	return nil
}
