// Package egressproxy is the per-run gating forward proxy that gives a
// sandboxed agent controlled reach to public package registries
// (TFAC-567, the v0 slice of the TFAC-408 sandbox-fleet egress design).
//
// # The problem it solves
//
// The sandbox's L3 egress policy (internal/sandbox applyEgressPolicy)
// pins all traffic to the run's own gateway IP — the only
// listeners there are the per-run sidecar's. That wall is what keeps a
// compromised agent off cloud metadata, the operator's private network,
// and sibling runs' proxies. But it also means `pnpm install` / `pip
// install` cannot fetch dependencies: there is no route to any registry.
//
// # The design (sandbox-fleet spec §3, docs/for-agents/specs/sandbox-fleet/README.md)
//
// A CONNECT proxy bound on the run's gateway IP becomes the single audited
// door to the public internet. The L3 layer stays unchanged: it keeps
// funneling everything to the gateway; this proxy decides what gets out.
// Hostname policy is inexpressible at L3 (registries live on shared CDNs),
// and the per-profile allowlists TFAC-408 adds need a hostname-aware
// enforcement point.
//
// # Policy
//
// Checked in order, every refusal an explicit error to the client and a
// recorded denial:
//
//  1. The CONNECT target must be an allowlisted registry on port 443
//     (Config.Registries — DefaultRegistries until TFAC-408). Exact
//     hostname match; IP literals never match.
//  2. The proxy resolves the hostname HOST-SIDE, checks every address
//     against the operator-owned, compiled-in internal denylist
//     (denylist.go), and dials the vetted IP literal, so there is no
//     second resolution for DNS rebinding to race (§3.6). The sandbox
//     never resolves anything on this path: its clients hand the hostname
//     to the proxy, whose own address is an IP literal.
//  3. It completes TLS to the registry under upstreamTLSConfig, which
//     verifies the CONNECT host against the system roots, before it
//     answers the CONNECT.
//  4. It terminates the client's TLS with a leaf for the CONNECT host,
//     signed by a CA minted for this engagement and name-constrained to the
//     allowlist, and serves HTTP/1.1 only.
//  5. Each request inside is held to one shape and one hostname: its Host
//     must be the CONNECT host, and a registry takes reads only — GET and
//     HEAD, and POST to the paths the registry lists as reads. Writes such
//     as `npm publish` are refused.
//  6. An admitted request is relayed to the same host and the response
//     copied back verbatim: no rewriting, no decompression, no cache.
//
// The interception design of record is
// docs/for-agents/specs/sandbox-backends/ §6.1, with the conditions this
// package implements.
//
// Redirects are out of scope: the proxy follows nothing. The client follows
// a redirect, and each new host arrives as a fresh CONNECT that is gated
// again.
//
// # Trust model on the local hop
//
// Same shape as llmproxy: the gateway IP lives in the
// shared host netns, so "reaching this proxy" must not be treated as
// "is this run's own agent." Every CONNECT must carry the per-run
// secret (Config.IncomingToken) as a Proxy-Authorization Basic
// password; wrong or missing gets 407 constant-time. The proxy holds
// no credential, but the token still matters: once allowlists are
// per-profile, a sibling reaching another run's proxy could use a
// WIDER allowlist than its own. The token pins each run to its own
// policy even if L3 isolation regresses. Property B is unaffected —
// the token is a per-run capability to this run's own proxy, minted
// fresh and destroyed at teardown, same class as the LLM/git tokens.
package egressproxy

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

var egressLog = logging.Component("egressproxy")

// BasicUser is the conventional username half of the per-run proxy
// credential. Clients build Proxy-Authorization from the proxy URL's
// userinfo (http://x-run:<token>@host:port), so the caller injecting
// that URL and this proxy validating the header must agree on the
// username. The token (password half) is the actual secret.
const BasicUser = "x-run"

// connectPort is the only CONNECT target port the proxy admits. Every
// package registry speaks HTTPS on 443; anything else (25, 6379, ...)
// is not a dependency fetch and is refused. Widening this is a
// TFAC-408 per-profile decision, not a v0 knob.
const connectPort = "443"

// dialTimeout bounds each upstream connection attempt. Applied per
// vetted address, so a host with an unreachable first A record still
// gets its remaining addresses tried within a bounded total.
const dialTimeout = 10 * time.Second

// DeniedConnect describes one refusal for the audit hook: a refused CONNECT,
// or a refusal within an admitted one (a failed handshake, a refused
// request).
type DeniedConnect struct {
	// Target is the CONNECT authority as the client sent it (host:port).
	Target string
	// Reason is the human-readable denial cause (also sent to the client).
	// A refused request's reason starts with its method and path.
	Reason string
}

// Config bundles the inputs a Server needs at construction.
type Config struct {
	// Registries are the hosts the proxy admits (port 443 only) and the
	// read paths each takes a POST on. Empty means the proxy denies
	// everything, which is valid (a fully-closed profile) but probably a
	// caller bug; New does not reject it.
	Registries []Registry

	// IncomingToken, when non-empty, is the per-run secret every
	// CONNECT must present as the Basic password (user BasicUser) in
	// Proxy-Authorization. Missing/wrong → 407, constant-time compare.
	// Empty disables the check (loopback/test usage), mirroring
	// llmproxy.Config.IncomingToken.
	IncomingToken string

	// AllowNonLoopback opts into binding Start on a non-loopback
	// address — the host-side veth IP is the legitimate case. Same
	// conscious-opt-out contract as llmproxy/gitproxy.
	AllowNonLoopback bool

	// GitHub names the hostnames of this run's configured GitHub deployment.
	// It is consulted only to pick the remedy a denial's reason carries (see
	// deniedHostGuidance) — never to widen or narrow the allow decision. The
	// zero value means the public github.com family; build per-run values
	// with GitHubHostsForUpstreams so this and the run's credential channels
	// read the same upstream strings.
	GitHub GitHubHosts

	// RecordDenial, when non-nil, is invoked once per refusal — a refused
	// CONNECT, a failed handshake, or a refused request inside one — for
	// the audit log (TFAC-483 is the intended consumer; nil skips).
	// Delivery is asynchronous through a bounded queue drained by one
	// goroutine, with each sink call bounded by a timeout ctx — a slow
	// sink (a DB write) never adds latency to the 403, and a sandbox
	// spamming denials at wire speed can't amplify into unbounded
	// goroutines or memory (contrast gitproxy's goroutine-per-denial,
	// which is fine there because git-op rates are inherently low; a
	// CONNECT flood is attacker-priced). Best-effort: records are
	// dropped when the queue is full or on Shutdown (drops are counted,
	// see DroppedDenials). Denials are always also logged synchronously
	// via slog, so a dropped record loses the structured audit row, not
	// the operational trace.
	RecordDenial func(ctx context.Context, denied DeniedConnect)
}

// Server is a single per-run gating proxy instance. Create one per run
// via New, bind with Start, tear down with Shutdown.
type Server struct {
	cfg Config

	// registries maps each admitted host to the paths it takes a POST on.
	registries map[string]map[string]struct{}

	// ca signs the leaves the jail is shown. Nil when no host is admitted.
	ca *authority

	// The guidance sets from Config.GitHub, normalized once like registries.
	// Read only by deniedHostGuidance.
	ghAPIHosts map[string]struct{}
	ghGitHosts map[string]struct{}

	// resolve + dial are seams for tests: production uses the host's
	// resolver and a plain net.Dialer; tests substitute a fake resolver
	// (to steer "public" hostnames at denylisted or loopback addresses)
	// and observe exactly what would be dialed.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)

	listener net.Listener
	httpSrv  *http.Server
	serveErr chan error

	// Denial audit plumbing (only allocated when cfg.RecordDenial is
	// set): deny() does a non-blocking send into denials; one drain
	// goroutine (started by Start, stopped via drainStop) delivers to
	// the sink with a per-record timeout. dropped counts records lost
	// to a full queue — see the Config.RecordDenial doc for why this
	// is bounded-and-dropping rather than goroutine-per-denial.
	denials   chan DeniedConnect
	drainStop chan struct{}
	stopOnce  sync.Once
	dropped   atomic.Int64

	// Intercepted sessions' client conns and the cancel for their upstream
	// requests, tracked so Shutdown can end them: http.Server.Shutdown
	// ignores hijacked connections, which would otherwise outlive the run.
	// closed is set by Shutdown as it sweeps them, so a session that
	// finishes its setup afterwards is refused rather than missed.
	mu       sync.Mutex
	sessions map[net.Conn]context.CancelFunc
	closed   bool

	// sessionSlots holds one token per session in flight, from the
	// upstream dial until the session ends (maxSessions).
	sessionSlots chan struct{}
}

// denialQueueCap bounds the denial audit queue. Sized for bursts (a
// package manager fanning out to a dozen blocked hosts) rather than
// sustained floods — a flood is exactly the case where dropping audit
// breadcrumbs beats buffering an attacker's backlog.
const denialQueueCap = 256

// recordDenialTimeout bounds each sink invocation so one wedged write
// can't stall the drain goroutine forever (it would otherwise turn the
// bounded queue into a permanently-full one). Mirrors gitproxy's
// per-record timeout.
const recordDenialTimeout = 5 * time.Second

// New constructs a Server but does not listen. Call Start to bind. It mints
// the engagement's interception CA, whose certificate CACertPEM returns.
func New(cfg Config) (*Server, error) {
	gh := cfg.GitHub
	if len(gh.API) == 0 && len(gh.Git) == 0 {
		gh = GitHubHostsForUpstreams("", "")
	}
	ca, err := newAuthority(registryHosts(cfg.Registries), time.Now())
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: dialTimeout}
	s := &Server{
		cfg:        cfg,
		registries: newRegistrySet(cfg.Registries),
		ca:         ca,
		ghAPIHosts: newHostSet(gh.API),
		ghGitHosts: newHostSet(gh.Git),
		resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial:         dialer.DialContext,
		sessions:     make(map[net.Conn]context.CancelFunc),
		sessionSlots: make(chan struct{}, maxSessions),
	}
	if cfg.RecordDenial != nil {
		s.denials = make(chan DeniedConnect, denialQueueCap)
		s.drainStop = make(chan struct{})
	}
	return s, nil
}

// CACertPEM is the engagement's interception CA certificate. The jail's
// trust file must carry it, or every admitted host fails verification in the
// jail's clients. Nil when no host is admitted, since then there is no CA.
func (s *Server) CACertPEM() []byte {
	if s.ca == nil {
		return nil
	}
	return s.ca.certPEM
}

// Start binds addr ("host:port"; port 0 lets the kernel pick) and
// serves in a background goroutine. Returns the bound address.
func (s *Server) Start(addr string) (string, error) {
	if s.httpSrv != nil {
		return "", errors.New("egressproxy: already started; create a new Server per run")
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
		return "", fmt.Errorf("egressproxy: listen on %s: %w", addr, err)
	}
	s.listener = ln
	s.serveErr = make(chan error, 1)
	s.httpSrv = &http.Server{
		Handler: http.HandlerFunc(s.handleRequest),
		// Bounds the CONNECT's header receipt only; a session's lifetime
		// is unbounded by design (a large download can run for minutes).
		ReadHeaderTimeout: 30 * time.Second,
	}
	// No recover: net/http recovers a handler panic per connection, so nothing
	// a request does reaches this frame; Serve itself only accepts, and the
	// buffered send + close cannot panic (one sender, one close). An
	// intercepted session runs on its handler's goroutine, so the same
	// recover covers it.
	go func() {
		err := s.httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr <- err
		}
		close(s.serveErr)
	}()
	if s.denials != nil {
		// Recovers per record — see drainDenials.
		go s.drainDenials()
	}
	return ln.Addr().String(), nil
}

// Shutdown stops the listener, the denial drain goroutine, and every
// live session. Hijacked conns are closed explicitly because
// http.Server.Shutdown ignores them; without this a session open at run
// teardown would outlive the run. Denials still queued at shutdown are
// dropped (best-effort audit, same acceptance as gitproxy).
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	if s.drainStop != nil {
		s.stopOnce.Do(func() { close(s.drainStop) })
	}
	// Sessions first, under the flag that refuses any still being set up,
	// so a handler midway through its upstream dial cannot register one
	// after the sweep.
	s.mu.Lock()
	s.closed = true
	for c, cancel := range s.sessions {
		cancel()
		_ = c.Close()
	}
	s.mu.Unlock()
	return s.httpSrv.Shutdown(ctx)
}

// drainDenials is the single consumer of the denial queue: it delivers
// each record to the sink under a per-record timeout ctx, so one
// wedged sink call is bounded and the queue keeps moving. Exits on
// drainStop; anything still queued then is dropped by design.
//
// The delivery recovers per record: this runs in the per-run credential
// sidecar, where an unrecovered panic in the sink would fail the run and kill
// every other proxy in the process to save one audit breadcrumb this queue
// already declares droppable. Recovering per record also keeps the drain alive
// for the records behind the bad one. See the goroutine rule in
// cmd/runsidecar's package doc.
func (s *Server) drainDenials() {
	for {
		select {
		case d := <-s.denials:
			s.deliverDenial(d)
		case <-s.drainStop:
			return
		}
	}
}

// deliverDenial hands one record to the sink under its own timeout, so a
// panicking sink costs that record and nothing else.
func (s *Server) deliverDenial(d DeniedConnect) {
	defer func() {
		if r := recover(); r != nil {
			egressLog.Error("panic recording egress denial; audit record dropped",
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), recordDenialTimeout)
	defer cancel()
	s.cfg.RecordDenial(ctx, d)
}

// DroppedDenials reports how many audit records were dropped because
// the denial queue was full. Nonzero under a denial flood; the
// synchronous slog line per denial remains the operational trace.
func (s *Server) DroppedDenials() int64 { return s.dropped.Load() }

// Err exposes the background Serve goroutine's first unexpected error,
// mirroring llmproxy.Server.Err. Closed on goroutine exit.
func (s *Server) Err() <-chan error { return s.serveErr }

// handleRequest is the CONNECT funnel: auth, then method and target checks,
// then the resolve-and-vet dial and the verified handshake with the registry,
// all before the CONNECT is answered. Every refusal goes through s.refuse so
// the audit hook and the client-visible reason stay in lockstep.
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	if !s.callerAuthorized(r) {
		// 407 (not 401): proxy-hop semantics — clients that carry
		// userinfo in the proxy URL (curl, git, npm, pip, go) retry
		// with Proxy-Authorization on this challenge if they didn't
		// send it preemptively.
		w.Header().Set("Proxy-Authenticate", `Basic realm="tf-egress"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}

	if r.Method != http.MethodConnect {
		// CONNECT-only by design: registries are all HTTPS, and
		// absolute-form plain-HTTP proxying would add a request-parsing
		// surface for no consumer. The body names the policy so an
		// agent reading its failed tool output can adjust.
		s.deny(w, r, "egress proxy is CONNECT-only (HTTPS tunneling); plain-HTTP proxying is not supported")
		return
	}

	// CONNECT authority form is host:port.
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		s.deny(w, r, fmt.Sprintf("malformed CONNECT target %q (want host:port)", r.Host))
		return
	}
	if port != connectPort {
		s.deny(w, r, fmt.Sprintf("port %s is not tunneled; only %s (HTTPS) is allowed", port, connectPort))
		return
	}
	hostNorm := normalizeHost(host)
	if _, ok := s.registries[hostNorm]; !ok {
		// Covers IP-literal targets too: the allowlist holds hostnames,
		// so a literal never matches — default-deny, as specified.
		reason := fmt.Sprintf("host %q is not on the sandbox egress allowlist", host)
		// Hosts with a sanctioned alternative say so: the refusal is the
		// one message guaranteed to reach whoever aimed at the host, and
		// "no" without "use this instead" reads as a hurdle to route around.
		if g := s.deniedHostGuidance(hostNorm); g != "" {
			reason += "; " + g
		}
		s.deny(w, r, reason)
		return
	}

	select {
	case s.sessionSlots <- struct{}{}:
		defer func() { <-s.sessionSlots }()
	default:
		s.refuse(w, r, http.StatusServiceUnavailable, fmt.Sprintf(
			"this run already has %d connections open through the egress proxy; close some before opening more", maxSessions))
		return
	}

	upstream, err := s.dialUpstream(r.Context(), hostNorm)
	if err != nil {
		var pd policyDenial
		var tlsErr upstreamTLSError
		switch {
		case errors.As(err, &pd):
			s.deny(w, r, pd.reason)
		case errors.As(err, &tlsErr):
			// Never a fallback to tunneling: a registry that fails
			// verification is refused, loudly, like any other policy.
			s.refuse(w, r, http.StatusBadGateway, tlsErr.Error())
		default:
			egressLog.Warn("upstream dial failed", "target", r.Host, "error", err)
			http.Error(w, "egress proxy: upstream dial failed", http.StatusBadGateway)
		}
		return
	}

	s.intercept(w, hostNorm, r.Host, upstream)
}

// policyDenial marks a dialVetted failure that is a policy decision
// (every resolved address denylisted) rather than a network fault, so
// the handler can 403 with the reason instead of 502.
type policyDenial struct{ reason string }

func (p policyDenial) Error() string { return p.reason }

// dialVetted resolves host on the HOST side, drops every denylisted
// address, and dials the survivors as IP literals in resolver order.
// Dialing the vetted literal — never the hostname — is the DNS-
// rebinding defense: there is no second resolution between check and
// connect for an attacker-controlled record to flip (§3.6).
func (s *Server) dialVetted(ctx context.Context, host string) (net.Conn, error) {
	return vettedDial(ctx, s.resolve, s.dial, host, connectPort)
}

// deny refuses with 403. See refuse.
func (s *Server) deny(w http.ResponseWriter, r *http.Request, reason string) {
	s.refuse(w, r, http.StatusForbidden, reason)
}

// refuse answers with status and a client-visible reason and enqueues the
// audit record. One funnel so the agent-visible message and the audit
// record can never drift apart.
//
// For a CONNECT, the reason is also the status line's reason phrase.
// A client tunneling through a proxy never reads the body of a refused
// CONNECT: Go's transport surfaces only the text after the status code
// (and gh is a Go client), Python's raises it as "Tunnel connection
// failed: 403 <phrase>" — so a canonical status line reduces every
// denial to a bare "Forbidden" with the actual reason discarded unread.
// The phrase is the one channel that survives, which means writing the
// response by hand: ResponseWriter always emits the canonical phrase
// for the code.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, status int, reason string) {
	s.recordDenial(r.Host, reason)
	if r.Method == http.MethodConnect && refuseConnect(w, status, reason) {
		return
	}
	// Non-CONNECT denials (and the can't-hijack fallback) keep the plain
	// response: those clients read the body, where the reason already is.
	http.Error(w, "egress proxy: "+reason, status)
}

// recordDenial logs a refusal and enqueues its audit record. The enqueue is
// non-blocking: a refusal must never wait on the audit sink (a slow DB write
// behind a synchronous hook would let a sandbox spamming denials hold
// handler goroutines hostage — the DoS shape the bounded queue exists to
// kill). A full queue drops the record and counts it; the slog line is the
// trace that survives drops.
func (s *Server) recordDenial(target, reason string) {
	egressLog.Info("egress denied", "target", target, "reason", reason)
	if s.denials == nil {
		return
	}
	select {
	case s.denials <- DeniedConnect{Target: target, Reason: reason}:
	default:
		if s.dropped.Add(1) == 1 {
			// Warn once per Server, not per drop — a flood is
			// exactly when per-drop logging would amplify.
			egressLog.Warn("denial audit queue full; dropping records (see DroppedDenials)")
		}
	}
}

// refuseConnect writes the CONNECT refusal by hand over the hijacked conn so
// the status line carries reason as its phrase. Reports false — response
// unwritten, caller falls back to http.Error — when the ResponseWriter can't
// hijack or the hijack fails.
func refuseConnect(w http.ResponseWriter, status int, reason string) bool {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return false
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	body := "egress proxy: " + reason + "\n"
	fmt.Fprintf(brw, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nX-Content-Type-Options: nosniff\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, reasonPhrase(reason), len(body), body)
	_ = brw.Flush()
	return true
}

// reasonPhrase renders reason as a status-line reason phrase: one line of
// SP / HTAB / visible characters (the RFC 9112 grammar), everything else
// mapped to a space so no input can splice a header or a second status line
// into the response head. The denial reasons are our own format strings, and
// the one caller-controlled part — the CONNECT target — arrives %q-escaped;
// the mapping is the backstop, not the primary defense. Capped well under
// the 4 KiB line buffer Go's transport reads a proxy response with.
func reasonPhrase(reason string) string {
	const maxPhraseBytes = 512
	clean := strings.Map(func(r rune) rune {
		if r == '\t' || r == ' ' || (r > 0x20 && r != 0x7f) {
			return r
		}
		return ' '
	}, reason)
	if len(clean) > maxPhraseBytes {
		clean = strings.ToValidUTF8(clean[:maxPhraseBytes], "")
	}
	return clean
}

// callerAuthorized validates the Proxy-Authorization Basic credential
// against the per-run token, constant-time over the whole
// "user:password" string. Empty IncomingToken disables the check
// (loopback/test path), mirroring llmproxy.
func (s *Server) callerAuthorized(r *http.Request) bool {
	if s.cfg.IncomingToken == "" {
		return true
	}
	const prefix = "Basic "
	header := r.Header.Get("Proxy-Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(header[len(prefix):])
	if err != nil {
		return false
	}
	want := BasicUser + ":" + s.cfg.IncomingToken
	return subtle.ConstantTimeCompare(decoded, []byte(want)) == 1
}

// assertLoopback enforces the loopback-only-by-default bind, same
// contract as llmproxy: the veth-IP sandbox bind is the conscious
// opt-out via AllowNonLoopback.
func assertLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("egressproxy: parse bind address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("egressproxy: bind address %q binds to all interfaces — set AllowNonLoopback=true to confirm intent", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("egressproxy: bind address %q is not loopback — set AllowNonLoopback=true to confirm intent", addr)
		}
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("egressproxy: resolve %q: %w", host, err)
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("egressproxy: bind host %q resolves to %s (not loopback) — set AllowNonLoopback=true to confirm intent", host, a)
		}
	}
	return nil
}
