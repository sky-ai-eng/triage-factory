// Package modproxy is the per-run Go module proxy: a host-side relay that
// lets a sandboxed `go build` fetch modules and toolchains without the jail
// ever reaching a public host directly.
//
// It exists because of one detail of how the public proxy serves large
// artifacts. proxy.golang.org returns small module zips inline, but
// 302-redirects large ones to storage.googleapis.com — and a fail-closed
// egress allowlist cannot follow that hop without opening every bucket in
// Google Cloud Storage to the jail. That is not a narrow entry: the egress
// proxy is CONNECT-only, so it matches hostnames and never paths, and the
// signed URLs are path-style, so there is no per-bucket hostname to allow
// instead. Allowing it would hand hostile-input-exposed code a general
// object store to write to.
//
// So the redirect is resolved on the host, where egress is unrestricted,
// and only the bytes cross into the jail. The sandbox's GOPROXY points at
// this server on the run's own gateway; the jail needs no public host at
// all, which makes the posture strictly tighter than allowing the registry
// directly.
//
// Two consequences of that shape are worth stating, because both look like
// oversights otherwise:
//
//   - **It follows redirects, which is the entire point** — and therefore
//     dials every hop through egressproxy.VettedDialContext. A host-side
//     fetcher acting on a sandbox's behalf is a confused deputy by
//     construction; without the denylist at the dial, an upstream redirect
//     to the cloud metadata endpoint would be fetched with the host's reach
//     and streamed into the jail.
//   - **It carries no per-run token**, unlike the LLM and git proxies. Those
//     hold real credentials, so a sibling run reaching them would be a
//     credential compromise; this one holds none, and relays only what a
//     public proxy would serve anyway. cmd/go also has no clean way to
//     present one — GOPROXY userinfo is not honored, and .netrc/GOAUTH would
//     need a per-run file mounted into the jail. Cross-run reach is already
//     denied at L3 (each run's egress is pinned to its own gateway, and the
//     denylist covers the sandbox pool and sibling gateways), which is
//     proportionate for a credential-free relay.
package modproxy

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

var modLog = logging.Component("modproxy")

const (
	// DefaultUpstream is the public Go module proxy.
	DefaultUpstream = "https://proxy.golang.org"

	// DefaultSumDB is the public checksum database. It is a SEPARATE
	// upstream from the module proxy, not a path under it: proxy.golang.org
	// answers 404 for /sumdb/... because sumdb relaying is a feature for
	// private proxies whose clients cannot reach the internet — exactly our
	// situation. Verified against the live service; without our own relay
	// the jail would still have to reach sum.golang.org directly.
	DefaultSumDB = "https://sum.golang.org"

	// maxRedirects caps the redirect chain. The real chain is one hop
	// (proxy → CDN); anything longer is a malfunctioning or hostile
	// upstream, and every hop costs a vetted dial.
	maxRedirects = 5

	// responseHeaderTimeout bounds how long an upstream may take to send
	// response HEADERS. Body transfer is deliberately unbounded — the
	// toolchain zip is ~70MB and a slow link may legitimately take minutes,
	// so a whole-request deadline would fail exactly the large fetches this
	// package exists to make work.
	responseHeaderTimeout = 60 * time.Second
)

// Config is the per-run construction input.
type Config struct {
	// Upstream is the module proxy to relay to. Default DefaultUpstream.
	// Only scheme + host are honored; a path is rejected at construction so
	// a misconfigured operator fails loudly rather than silently serving
	// 404s for every module.
	Upstream string

	// SumDB is the checksum database to relay to. Default DefaultSumDB.
	SumDB string

	// AllowNonLoopback opts into binding Start on a non-loopback address.
	// The relay is unauthenticated on the local hop; the boundary is "only
	// this run's sandbox can route here", enforced by network isolation. The
	// sandbox case (binding the host-side veth IP) is the legitimate use and
	// opts in here, so an accidental 0.0.0.0 bind still fails.
	AllowNonLoopback bool

	// RunID is the run this proxy serves. Carried for observability; the
	// relay does not branch on it.
	RunID string

	// DialContext replaces the vetted dialer. Production leaves it nil and
	// gets egressproxy.VettedDialContext. Tests set it, because the denylist
	// blocks loopback and an httptest upstream lives there — which is the
	// gate working, not a test-harness problem.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Server is one run's module relay.
type Server struct {
	upstream *url.URL
	sumdb    *url.URL
	client   *http.Client
	cfg      Config

	listener net.Listener
	httpSrv  *http.Server
	serveErr chan error
}

// New constructs a Server but does not listen. Call Start to bind.
func New(cfg Config) (*Server, error) {
	upstream, err := parseUpstream("upstream", cfg.Upstream, DefaultUpstream)
	if err != nil {
		return nil, err
	}
	sumdb, err := parseUpstream("sumdb", cfg.SumDB, DefaultSumDB)
	if err != nil {
		return nil, err
	}

	dial := cfg.DialContext
	if dial == nil {
		dial = egressproxy.VettedDialContext
	}

	return &Server{
		upstream: upstream,
		sumdb:    sumdb,
		cfg:      cfg,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:           dial,
				ResponseHeaderTimeout: responseHeaderTimeout,
				ForceAttemptHTTP2:     true,
			},
			// Redirects are followed HERE, host-side — the reason this
			// package exists. The per-hop denylist check rides on
			// DialContext above, so this policy only has to bound the chain
			// and keep it on TLS.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("modproxy: stopped after %d redirects", maxRedirects)
				}
				// Refuse a TLS DOWNGRADE rather than requiring https
				// outright. The property worth protecting is that an
				// https upstream cannot be talked down to cleartext
				// mid-chain; demanding https unconditionally would also
				// forbid a deliberately-http upstream (a local mirror, a
				// test server), which is a configuration the operator has
				// already chosen with eyes open.
				if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
					return fmt.Errorf("modproxy: refusing https→%s downgrade redirect to %q", req.URL.Scheme, req.URL.Redacted())
				}
				return nil
			},
		},
	}, nil
}

// parseUpstream validates an upstream override: absolute, https-or-http,
// host present, and no path/query/fragment (which would silently corrupt
// every relayed URL).
func parseUpstream(name, raw, def string) (*url.URL, error) {
	if raw == "" {
		raw = def
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("modproxy: parse %s %q: %w", name, raw, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("modproxy: %s %q must be http or https", name, raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("modproxy: %s %q has no host", name, raw)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("modproxy: %s %q must be scheme://host only (no path, query, or fragment)", name, raw)
	}
	u.Path = ""
	return u, nil
}

// SumDBPrefix is the request-path prefix cmd/go uses when it routes
// checksum-database traffic through a proxy: /sumdb/<sumdb-host>/...
func (s *Server) sumDBPrefix() string { return "/sumdb/" + s.sumdb.Host }

// ServeHTTP implements the GOPROXY protocol surface by relaying it.
//
// Routing is done by hand on the ESCAPED path rather than through
// http.ServeMux, deliberately. Module paths carry case-encoding ("!" before
// an uppercase letter) and versions carry "@" and "+incompatible";
// ServeMux's path cleaning and redirecting would mangle exactly those, and
// the corruption would show up as a confusing "module not found" rather
// than an obvious routing bug.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The protocol is read-only. Anything else is a client bug or a probe;
	// either way it must not reach an upstream.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "modproxy: only GET and HEAD are supported", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.EscapedPath()
	prefix := s.sumDBPrefix()

	switch {
	case path == prefix+"/supported":
		// The capability probe. cmd/go asks this before routing any
		// checksum traffic through the proxy; a 200 means "I relay the
		// sumdb", and anything else sends it to sum.golang.org directly —
		// which the jail cannot reach. Synthesized rather than relayed
		// precisely because the upstream module proxy answers 404 here.
		w.WriteHeader(http.StatusOK)

	case strings.HasPrefix(path, prefix+"/"):
		s.relay(w, r, s.sumdb, strings.TrimPrefix(path, prefix))

	case strings.HasPrefix(path, "/sumdb/"):
		// A sumdb we don't relay. Say so rather than forwarding it to the
		// module proxy, where it would 404 for an unrelated reason.
		http.Error(w, "modproxy: unsupported checksum database", http.StatusNotFound)

	default:
		s.relay(w, r, s.upstream, path)
	}
}

// relay performs the host-side fetch and streams the result back.
//
// It builds a FRESH outbound request rather than mutating the inbound one.
// That is what keeps three separate hazards from arising at all: a
// server-side request carries RequestURI, which http.Client rejects
// outright; reusing the inbound URL risks re-encoding the escaped path; and
// forwarding inbound headers would carry whatever the jail sent —
// Authorization included — to a public upstream. Copying nothing means
// there is no header-stripping step to get wrong later.
func (s *Server) relay(w http.ResponseWriter, r *http.Request, target *url.URL, escapedPath string) {
	outURL, err := url.Parse(target.Scheme + "://" + target.Host + escapedPath)
	if err != nil {
		http.Error(w, "modproxy: bad request path", http.StatusBadRequest)
		return
	}
	outURL.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), nil)
	if err != nil {
		http.Error(w, "modproxy: build upstream request", http.StatusInternalServerError)
		return
	}

	resp, err := s.client.Do(req)
	if err != nil {
		// A denylisted redirect target lands here. Log it — this is the
		// confused-deputy gate firing, which is worth seeing — but keep the
		// client-visible body generic.
		modLog.Info("upstream fetch failed", "run_id", s.cfg.RunID, "url", outURL.Redacted(), "err", err)
		http.Error(w, "modproxy: upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	// Status is passed through verbatim, including 404 and 410: cmd/go
	// reads those to decide a module is absent and to advance its GOPROXY
	// list. Rewriting them to 502 would turn "not found" into "proxy
	// broken" and break that fallthrough.
	w.WriteHeader(resp.StatusCode)

	// Streamed, never buffered — these bodies reach ~70MB and the executor
	// runs under a memory budget.
	//
	// A cancelled copy is normal, not a fault: cmd/go fetches checksum tiles
	// speculatively and drops the ones it turns out not to need, which
	// cancels the request mid-body. Logging those would put a line in the
	// executor log on every single build for something working as intended.
	if _, err := io.Copy(w, resp.Body); err != nil && !errors.Is(err, context.Canceled) {
		modLog.Info("relay body truncated", "run_id", s.cfg.RunID, "url", outURL.Redacted(), "err", err)
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

// Start binds addr ("host:port"; port 0 lets the kernel pick) and serves in
// a background goroutine. Returns the bound address.
func (s *Server) Start(addr string) (string, error) {
	if s.httpSrv != nil {
		return "", errors.New("modproxy: already started; create a new Server per run")
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
		return "", fmt.Errorf("modproxy: listen on %s: %w", addr, err)
	}
	s.listener = ln
	s.serveErr = make(chan error, 1)
	s.httpSrv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: a large module download legitimately outlasts
		// any fixed deadline, same reasoning as the git proxy's pack
		// transfers.
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

// assertLoopback rejects a bind address that isn't loopback, so the
// unauthenticated relay can't be exposed beyond the host by accident.
func assertLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("modproxy: parse bind address %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("modproxy: refusing to bind non-loopback %q without Config.AllowNonLoopback", addr)
	}
	return nil
}

// Err returns a channel receiving the first unexpected Serve error. Closed
// when the serve goroutine exits, so a receive unblocks after Shutdown.
func (s *Server) Err() <-chan error { return s.serveErr }

// Shutdown stops the server. Safe on a never-started Server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.httpSrv == nil {
		return nil
	}
	if err := s.httpSrv.Shutdown(ctx); err != nil {
		return fmt.Errorf("modproxy: shutdown: %w", err)
	}
	return nil
}
