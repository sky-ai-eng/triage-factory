package egressproxy

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --- denylist ---------------------------------------------------------------

// TestIPDenied_CoversInternalRanges pins the operator denylist against
// the concrete addresses the sandbox-fleet spec §3.1 names: cloud
// metadata, RFC1918 (incl. the sandbox pool + a sibling gateway),
// loopback, CGNAT, Fly's ULA network — plus the IPv4-mapped-IPv6
// smuggling forms the §3.6 alternate-encodings gate exists for.
func TestIPDenied_CoversInternalRanges(t *testing.T) {
	denied := []string{
		"169.254.169.254",        // cloud metadata
		"10.0.0.5",               // RFC1918
		"10.42.3.1",              // sibling run's gateway (sandbox pool)
		"172.16.0.1",             // RFC1918
		"192.168.1.1",            // RFC1918
		"127.0.0.1",              // loopback
		"0.0.0.0",                // unspecified ("this host" on Linux)
		"100.64.0.1",             // CGNAT (Fly private v4)
		"224.0.0.1",              // multicast
		"255.255.255.255",        // broadcast
		"::1",                    // v6 loopback
		"fe80::1",                // v6 link-local
		"fdaa::3",                // Fly internal resolver (ULA)
		"fc00::1",                // ULA
		"::",                     // v6 unspecified
		"::ffff:169.254.169.254", // v4-mapped metadata — the smuggling form
		"::ffff:10.0.0.5",        // v4-mapped RFC1918
		"::ffff:127.0.0.1",       // v4-mapped loopback
	}
	for _, s := range denied {
		if !ipDenied(netip.MustParseAddr(s)) {
			t.Errorf("ipDenied(%s) = false; internal address must be denied", s)
		}
	}

	allowed := []string{
		"104.16.0.35",     // public v4 (a Cloudflare edge)
		"142.250.80.46",   // public v4 (a Google front end)
		"2606:4700::1",    // public v6
		"2600:1901:0:1::", // public v6
	}
	for _, s := range allowed {
		if ipDenied(netip.MustParseAddr(s)) {
			t.Errorf("ipDenied(%s) = true; public address must pass", s)
		}
	}
}

// TestIPDenied_NAT64EmbeddedV4 pins the RFC 6052 defense: a DNS64
// resolver can synthesize 64:ff9b::<v4> for any A record, and a NAT64
// gateway delivers to the embedded v4 host — so a NAT64 coat around an
// internal v4 must be denied by the embedded address, while a NAT64
// coat around a public v4 must pass (denying the whole prefix would
// fail closed on legitimate NAT64-only networks).
func TestIPDenied_NAT64EmbeddedV4(t *testing.T) {
	denied := []string{
		"64:ff9b::a9fe:a9fe", // 169.254.169.254 — metadata behind NAT64
		"64:ff9b::a00:5",     // 10.0.0.5 — RFC1918 behind NAT64
		"64:ff9b::7f00:1",    // 127.0.0.1 behind NAT64
	}
	for _, s := range denied {
		if !ipDenied(netip.MustParseAddr(s)) {
			t.Errorf("ipDenied(%s) = false; NAT64-embedded internal v4 must be denied", s)
		}
	}
	if ipDenied(netip.MustParseAddr("64:ff9b::6810:23")) { // 104.16.0.35
		t.Error("ipDenied(64:ff9b::6810:23) = true; NAT64-embedded PUBLIC v4 must pass")
	}
}

// --- allowlist matching -----------------------------------------------------

func TestHostSet_NormalizesCaseAndTrailingDot(t *testing.T) {
	set := newHostSet([]string{"Registry.NPMJS.org"})
	for _, h := range []string{"registry.npmjs.org", "REGISTRY.NPMJS.ORG", "registry.npmjs.org."} {
		if _, ok := set[normalizeHost(h)]; !ok {
			t.Errorf("host %q should match the normalized allowlist", h)
		}
	}
	if _, ok := set[normalizeHost("evil-registry.npmjs.org.attacker.com")]; ok {
		t.Error("unrelated host matched the allowlist")
	}
	// No wildcard semantics: a subdomain of an allowed host is NOT allowed.
	if _, ok := set[normalizeHost("sub.registry.npmjs.org")]; ok {
		t.Error("subdomain matched; v0 allowlist is exact-hostname only")
	}
}

// TestDefaultRegistryHosts_NoGitHubNoInternal pins two curation
// invariants on the built-in list: no GitHub hosts (git egress must
// stay on the authenticated git proxy — an entry here would bypass its
// Authorize gate) and nothing that could resolve internally by
// construction (all entries are public registry FQDNs).
func TestDefaultRegistryHosts_NoGitHubNoInternal(t *testing.T) {
	for _, h := range DefaultRegistryHosts() {
		if strings.Contains(h, "github") {
			t.Errorf("DefaultRegistryHosts contains %q — git egress must go through the git proxy, not the egress proxy", h)
		}
		if !strings.Contains(h, ".") {
			t.Errorf("DefaultRegistryHosts contains bare name %q", h)
		}
	}
}

// --- proxy behavior ---------------------------------------------------------

// newTestServer starts a proxy whose resolver maps every allowed host
// to resolveTo and whose dialer records the addresses it was asked to
// dial. The real net.Dialer is kept unless dialTo is non-nil.
func newTestServer(t *testing.T, cfg Config, resolveTo []netip.Addr, dialed *[]string, dialTo func() (net.Conn, error)) string {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		return resolveTo, nil
	}
	if dialTo != nil {
		s.dial = func(_ context.Context, _, addr string) (net.Conn, error) {
			if dialed != nil {
				*dialed = append(*dialed, addr)
			}
			return dialTo()
		}
	} else if dialed != nil {
		inner := s.dial
		s.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			*dialed = append(*dialed, addr)
			return inner(ctx, network, addr)
		}
	}
	addr, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return addr
}

// rawConnect dials the proxy and sends a bare CONNECT, returning the
// status line + full response head. Driving the wire directly (rather
// than through http.Transport) keeps assertions on 403/407 bodies easy.
func rawConnect(t *testing.T, proxyAddr, target, proxyAuth string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if proxyAuth != "" {
		req += "Proxy-Authorization: " + proxyAuth + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("read response: %v", err)
	}
	return string(buf[:n])
}

func basicAuth(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(BasicUser+":"+token))
}

// TestProxy_DeniesOffAllowlistHost pins the default-deny: a public
// host not on the allowlist gets 403 with an actionable reason and is
// never resolved or dialed. The audit record arrives asynchronously,
// so the test waits on a channel sink (same pattern as gitproxy's
// denialSink) rather than reading a shared slice.
func TestProxy_DeniesOffAllowlistHost(t *testing.T) {
	var dialed []string
	denialCh := make(chan DeniedConnect, 16)
	addr := newTestServer(t, Config{
		AllowedHosts: []string{"registry.npmjs.org"},
		RecordDenial: func(_ context.Context, d DeniedConnect) { denialCh <- d },
	}, []netip.Addr{netip.MustParseAddr("104.16.0.35")}, &dialed, nil)

	resp := rawConnect(t, addr, "evil.example.com:443", "")
	if !strings.Contains(resp, "403") {
		t.Fatalf("off-allowlist CONNECT got:\n%s\nwant 403", resp)
	}
	if !strings.Contains(resp, "not on the sandbox egress allowlist") {
		t.Errorf("403 body should name the allowlist policy; got:\n%s", resp)
	}
	if len(dialed) != 0 {
		t.Errorf("proxy dialed %v for a denied host; must not touch the network", dialed)
	}
	select {
	case d := <-denialCh:
		if d.Target != "evil.example.com:443" {
			t.Errorf("RecordDenial target = %q, want evil.example.com:443", d.Target)
		}
	case <-time.After(2 * time.Second):
		t.Error("RecordDenial not invoked within 2s of the denial")
	}
}

// TestProxy_DeniesTheGitHubAPIHosts is the control that makes one hole in a
// sibling package's coverage a refusal rather than a silence.
//
// The per-run gh injector observes only the requests gh builds from the base
// url GH_HOST redirects. A handful of commands — the release asset verbs, and
// release delete — instead follow an ABSOLUTE url out of a response body
// (the upload endpoint, an asset's own url, the release's api url), which names
// GitHub directly and never that listener. The injector cannot see those
// requests and so cannot audit them, which would be an unlogged write under an
// org credential if anything could carry one out.
//
// Nothing can, and this is half of why: a sandboxed run reaches the public
// internet only through this proxy, and these two hosts are not on the list it
// admits. (The other half is that the jail holds a placeholder rather than a
// credential, so the request would be unauthenticated even with a route.) The
// attempt is refused here and recorded as a denial, which is the point — the
// act ends in a refusal row rather than in nothing at all.
//
// It asserts against the shipped list rather than one written for the test,
// because what makes the escape fail closed is what a real run admits. When
// per-profile allowlists replace that constant with customer data, this
// property has to survive the move: a profile carrying either host would open
// an unaudited write path, not merely widen package reach.
func TestProxy_DeniesTheGitHubAPIHosts(t *testing.T) {
	for _, host := range []string{"api.github.com", "uploads.github.com"} {
		t.Run(host, func(t *testing.T) {
			var dialed []string
			denialCh := make(chan DeniedConnect, 4)
			addr := newTestServer(t, Config{
				AllowedHosts: DefaultRegistryHosts(),
				RecordDenial: func(_ context.Context, d DeniedConnect) { denialCh <- d },
			}, []netip.Addr{netip.MustParseAddr("140.82.121.6")}, &dialed, nil)

			target := host + ":443"
			resp := rawConnect(t, addr, target, "")
			if !strings.Contains(resp, "403") {
				t.Fatalf("CONNECT %s got:\n%s\nwant 403 — a run must have no route to the host the gh "+
					"release commands escape to", target, resp)
			}
			// The deny precedes resolution, so the stub resolver above is never
			// consulted and nothing touches the network: a refused host is not
			// one this proxy looks up on the caller's behalf.
			if len(dialed) != 0 {
				t.Errorf("proxy dialed %v for a denied host", dialed)
			}
			select {
			case d := <-denialCh:
				if d.Target != target {
					t.Errorf("RecordDenial target = %q, want %q", d.Target, target)
				}
			case <-time.After(2 * time.Second):
				t.Error("the denial left no record — a refused escape that logs nothing is the outcome " +
					"this whole arrangement exists to delete")
			}
		})
	}
}

// TestProxy_ConnectDenialStatusLineCarriesReason pins the denial's delivery
// channel: the reason must ride the status LINE, not just the body, because a
// client tunneling through a proxy discards the body of a refused CONNECT and
// Go's transport surfaces only the text after the status code. This is
// exactly how `gh run view` — which follows the run object's absolute
// api.github.com jobs_url out of the injector channel — used to die with a
// bare "Forbidden" and nothing to act on.
//
// It also pins which hosts carry a remedy: the GitHub API hosts point at the
// exec verbs, github.com points at the run's git path, and a host with no
// sanctioned alternative gets the bare allowlist reason and no invented one.
func TestProxy_ConnectDenialStatusLineCarriesReason(t *testing.T) {
	cases := []struct {
		host        string
		wantPhrase  []string
		absentInAll string
	}{
		{host: "api.github.com", wantPhrase: []string{
			"not on the sandbox egress allowlist",
			"gh actions download-logs",
		}},
		{host: "github.com", wantPhrase: []string{
			"not on the sandbox egress allowlist",
			"credential proxy",
		}},
		{host: "evil.example.com",
			wantPhrase:  []string{"not on the sandbox egress allowlist"},
			absentInAll: "exec verbs"},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			addr := newTestServer(t, Config{AllowedHosts: DefaultRegistryHosts()}, nil, nil, nil)
			resp := rawConnect(t, addr, tc.host+":443", "")
			statusLine, _, _ := strings.Cut(resp, "\r\n")
			for _, want := range tc.wantPhrase {
				if !strings.Contains(statusLine, want) {
					t.Errorf("status line %q should carry %q — the body of a refused CONNECT is never read", statusLine, want)
				}
			}
			if tc.absentInAll != "" && strings.Contains(resp, tc.absentInAll) {
				t.Errorf("denial for %s mentions %q; a host with no sanctioned alternative must not point at one:\n%s",
					tc.host, tc.absentInAll, resp)
			}
		})
	}
}

// TestProxy_GoClientSurfacesDenialReason drives the report's exact shape end
// to end: a Go http.Client behind the proxy (what gh is) asking for a GitHub
// API URL must fail with an error that names the policy and the verb to use
// instead — not the canonical "Forbidden" the transport reduces a normal 403
// CONNECT response to.
func TestProxy_GoClientSurfacesDenialReason(t *testing.T) {
	addr := newTestServer(t, Config{AllowedHosts: DefaultRegistryHosts()}, nil, nil, nil)
	proxyURL, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatalf("parse proxy addr: %v", err)
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   2 * time.Second,
	}
	_, err = client.Get("https://api.github.com/repos/o/r/actions/runs/1/jobs?per_page=100")
	if err == nil {
		t.Fatal("GET through the proxy succeeded; api.github.com must be refused")
	}
	for _, want := range []string{"not on the sandbox egress allowlist", "gh actions download-logs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("client error %q should carry %q for the agent to act on", err.Error(), want)
		}
	}
}

// TestReasonPhrase_SingleLineAndBounded pins the response-splitting backstop:
// whatever composes the reason, the phrase that reaches the status line is one
// bounded line with control bytes neutralized.
func TestReasonPhrase_SingleLineAndBounded(t *testing.T) {
	got := reasonPhrase("a\r\nSet-Cookie: x\nb\x00c")
	if strings.ContainsAny(got, "\r\n\x00") {
		t.Errorf("reasonPhrase left control bytes in %q", got)
	}
	if got != "a  Set-Cookie: x b c" {
		t.Errorf("reasonPhrase = %q; control bytes should map to spaces", got)
	}
	if long := reasonPhrase(strings.Repeat("x", 4096)); len(long) > 512 {
		t.Errorf("reasonPhrase length = %d; must stay well under the client's 4 KiB line buffer", len(long))
	}
}

// TestProxy_SlowDenialSinkDoesNotDelayResponse pins the async-audit
// contract: with a sink wedged on a never-closed channel, the 403
// still returns immediately — the response path must never wait on
// the audit sink (a slow DB write behind a synchronous hook would let
// a sandbox spamming denials hold handler goroutines hostage).
func TestProxy_SlowDenialSinkDoesNotDelayResponse(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	addr := newTestServer(t, Config{
		AllowedHosts: []string{"registry.npmjs.org"},
		RecordDenial: func(ctx context.Context, _ DeniedConnect) {
			select {
			case <-blocked:
			case <-ctx.Done(): // per-record timeout also unblocks the drainer
			}
		},
	}, []netip.Addr{netip.MustParseAddr("104.16.0.35")}, nil, nil)

	start := time.Now()
	resp := rawConnect(t, addr, "evil.example.com:443", "")
	elapsed := time.Since(start)
	if !strings.Contains(resp, "403") {
		t.Fatalf("CONNECT got:\n%s\nwant 403", resp)
	}
	if elapsed > time.Second {
		t.Errorf("403 took %v with a wedged audit sink; the response must not wait on RecordDenial", elapsed)
	}

	// A second denial while the sink is still wedged must also return
	// promptly (queued, not blocked behind the in-flight sink call).
	start = time.Now()
	resp = rawConnect(t, addr, "evil2.example.com:443", "")
	if !strings.Contains(resp, "403") {
		t.Fatalf("second CONNECT got:\n%s\nwant 403", resp)
	}
	if elapsed = time.Since(start); elapsed > time.Second {
		t.Errorf("second 403 took %v; queue must absorb denials while the sink is busy", elapsed)
	}
}

// TestProxy_DenialQueueOverflowDropsNotBlocks pins the flood behavior:
// when the queue is full behind a wedged sink, further denials drop
// the audit record (counted via DroppedDenials) instead of blocking
// the 403 — bounded memory, bounded goroutines, no attacker-priced
// backlog.
func TestProxy_DenialQueueOverflowDropsNotBlocks(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	sinkEntered := make(chan struct{}, 1)
	s, err := New(Config{
		AllowedHosts: []string{"registry.npmjs.org"},
		RecordDenial: func(ctx context.Context, _ DeniedConnect) {
			select {
			case sinkEntered <- struct{}{}:
			default:
			}
			select {
			case <-blocked:
			case <-ctx.Done():
			}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Shrink the queue to 1 before Start so overflow is reachable with
	// three requests: first → in-flight in the sink, second → fills the
	// queue, third → must drop.
	s.denials = make(chan DeniedConnect, 1)
	addr, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})

	// First denial: wait until the sink actually holds it, so the
	// queue-capacity accounting below is deterministic.
	if resp := rawConnect(t, addr, "evil0.example.com:443", ""); !strings.Contains(resp, "403") {
		t.Fatalf("CONNECT got:\n%s\nwant 403", resp)
	}
	select {
	case <-sinkEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("sink never received the first denial")
	}

	for i := 1; i <= 2; i++ {
		start := time.Now()
		resp := rawConnect(t, addr, fmt.Sprintf("evil%d.example.com:443", i), "")
		if !strings.Contains(resp, "403") {
			t.Fatalf("CONNECT %d got:\n%s\nwant 403", i, resp)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("403 #%d took %v; overflow must drop, not block", i, elapsed)
		}
	}

	if got := s.DroppedDenials(); got != 1 {
		t.Errorf("DroppedDenials = %d, want 1 (one in-flight + one queued + one dropped)", got)
	}
}

// TestProxy_DeniesWhenResolvedIPInternal pins the SSRF core: an
// ALLOWLISTED hostname whose DNS answer points at internal space (the
// rebinding / poisoned-DNS shape) is refused at the resolved-IP check
// and never dialed. Metadata endpoint + v4-mapped form both covered.
func TestProxy_DeniesWhenResolvedIPInternal(t *testing.T) {
	for _, ip := range []string{"169.254.169.254", "10.42.3.1", "::ffff:169.254.169.254"} {
		t.Run(ip, func(t *testing.T) {
			var dialed []string
			addr := newTestServer(t, Config{
				AllowedHosts: []string{"registry.npmjs.org"},
			}, []netip.Addr{netip.MustParseAddr(ip)}, &dialed, nil)

			resp := rawConnect(t, addr, "registry.npmjs.org:443", "")
			if !strings.Contains(resp, "403") {
				t.Fatalf("CONNECT with internal-resolving host got:\n%s\nwant 403", resp)
			}
			if !strings.Contains(resp, "internal/denied addresses") {
				t.Errorf("403 body should name the denied-address policy; got:\n%s", resp)
			}
			if len(dialed) != 0 {
				t.Errorf("proxy dialed %v; a denylisted resolution must never be dialed", dialed)
			}
		})
	}
}

// TestProxy_DialsVettedIPNotHostname pins the no-second-resolution
// property: the upstream dial target is the vetted IP literal, not the
// hostname (which a rebinding attacker could re-answer differently).
func TestProxy_DialsVettedIPNotHostname(t *testing.T) {
	var dialed []string
	addr := newTestServer(t, Config{
		AllowedHosts: []string{"registry.npmjs.org"},
	}, []netip.Addr{netip.MustParseAddr("104.16.0.35")}, &dialed, func() (net.Conn, error) {
		return nil, fmt.Errorf("synthetic dial failure")
	})

	resp := rawConnect(t, addr, "registry.npmjs.org:443", "")
	if !strings.Contains(resp, "502") {
		t.Fatalf("CONNECT with failing dial got:\n%s\nwant 502", resp)
	}
	if len(dialed) != 1 || dialed[0] != "104.16.0.35:443" {
		t.Errorf("dialed %v, want exactly [104.16.0.35:443] — the vetted IP literal, never the hostname", dialed)
	}
}

// TestProxy_DeniesNonConnectAndBadPort pins the method/port funnel.
func TestProxy_DeniesNonConnectAndBadPort(t *testing.T) {
	addr := newTestServer(t, Config{
		AllowedHosts: []string{"registry.npmjs.org"},
	}, []netip.Addr{netip.MustParseAddr("104.16.0.35")}, nil, nil)

	// Non-CONNECT: absolute-form GET.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_, _ = conn.Write([]byte("GET http://registry.npmjs.org/ HTTP/1.1\r\nHost: registry.npmjs.org\r\n\r\n"))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, _ := conn.Read(buf)
	_ = conn.Close()
	if !strings.Contains(string(buf[:n]), "403") {
		t.Errorf("plain-HTTP GET through proxy got:\n%s\nwant 403 (CONNECT-only)", string(buf[:n]))
	}

	// CONNECT to a non-443 port on an allowed host.
	resp := rawConnect(t, addr, "registry.npmjs.org:6379", "")
	if !strings.Contains(resp, "403") {
		t.Errorf("CONNECT to :6379 got:\n%s\nwant 403 (443 only)", resp)
	}

	// CONNECT to an IP literal — hostname allowlist can't match it.
	resp = rawConnect(t, addr, "104.16.0.35:443", "")
	if !strings.Contains(resp, "403") {
		t.Errorf("CONNECT to IP literal got:\n%s\nwant 403 (allowlist is hostnames)", resp)
	}
}

// TestProxy_TokenAuth pins the per-run caller auth: no token → 407
// with a Basic challenge; wrong token → 407; right token proceeds to
// policy (the request below then 403s on the allowlist, proving it got
// past auth).
func TestProxy_TokenAuth(t *testing.T) {
	const token = "per-run-secret"
	addr := newTestServer(t, Config{
		AllowedHosts:  []string{"registry.npmjs.org"},
		IncomingToken: token,
	}, []netip.Addr{netip.MustParseAddr("104.16.0.35")}, nil, nil)

	if resp := rawConnect(t, addr, "evil.example.com:443", ""); !strings.Contains(resp, "407") {
		t.Errorf("no-auth CONNECT got:\n%s\nwant 407", resp)
	}
	if resp := rawConnect(t, addr, "evil.example.com:443", basicAuth("wrong")); !strings.Contains(resp, "407") {
		t.Errorf("wrong-token CONNECT got:\n%s\nwant 407", resp)
	}
	resp := rawConnect(t, addr, "evil.example.com:443", basicAuth(token))
	if !strings.Contains(resp, "403") {
		t.Errorf("right-token CONNECT got:\n%s\nwant 403 (past auth, denied by allowlist)", resp)
	}
	if strings.Contains(resp, "407") {
		t.Errorf("right token was rejected as unauthorized:\n%s", resp)
	}
}

// TestProxy_EndToEndTunnel drives a real HTTPS request through the
// full stack — http.Client with the proxy URL (userinfo-carried
// token), CONNECT, vetted dial, tunnel — against a local TLS server
// standing in for a registry. The test resolver maps the allowed
// hostname to the TLS server's loopback address; the denylist would
// veto loopback, so this test swaps the dial seam to connect to the
// listener directly after asserting the vetted address was the
// resolver's answer. This is the closest a unit test can get to
// `pnpm install` without live network.
func TestProxy_EndToEndTunnel(t *testing.T) {
	const token = "tunnel-token"

	// The stand-in registry: TLS with a self-signed cert.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tarball":"ok"}`))
	}))
	defer upstream.Close()
	upstreamAddr := upstream.Listener.Addr().String()

	// Resolver says the registry lives at a public IP; the dial seam
	// routes that vetted target to the local TLS server. This keeps
	// the denylist logic honest (a loopback resolution would 403)
	// while letting the tunnel carry real TLS bytes.
	var dialed []string
	addr := newTestServer(t, Config{
		AllowedHosts:  []string{"registry.npmjs.org"},
		IncomingToken: token,
	}, []netip.Addr{netip.MustParseAddr("104.16.0.35")}, &dialed, func() (net.Conn, error) {
		return net.Dial("tcp", upstreamAddr)
	})

	proxyURL, _ := url.Parse("http://" + BasicUser + ":" + token + "@" + addr)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // self-signed stand-in cert
			},
		},
		Timeout: 5 * time.Second,
	}

	resp, err := client.Get("https://registry.npmjs.org/react")
	if err != nil {
		t.Fatalf("HTTPS through proxy tunnel: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "tarball") {
		t.Errorf("tunneled response = %d %q, want 200 with upstream body", resp.StatusCode, body)
	}
	if len(dialed) != 1 || dialed[0] != "104.16.0.35:443" {
		t.Errorf("dialed %v, want the vetted IP literal", dialed)
	}
}

// TestProxy_ShutdownClosesLiveTunnels pins the hijacked-conn cleanup:
// a tunnel still open at Shutdown is force-closed rather than leaking
// its copy goroutines past run teardown.
func TestProxy_ShutdownClosesLiveTunnels(t *testing.T) {
	// Upstream that accepts and holds the conn open silently.
	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer upstreamLn.Close()
	go func() {
		for {
			c, aerr := upstreamLn.Accept()
			if aerr != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()

	s, err := New(Config{AllowedHosts: []string{"registry.npmjs.org"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("104.16.0.35")}, nil
	}
	s.dial = func(context.Context, string, string) (net.Conn, error) {
		return net.Dial("tcp", upstreamLn.Addr().String())
	}
	addr, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("CONNECT registry.npmjs.org:443 HTTP/1.1\r\nHost: registry.npmjs.org:443\r\n\r\n"))
	buf := make([]byte, 256)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, rerr := conn.Read(buf); rerr != nil || !strings.Contains(string(buf[:n]), "200") {
		t.Fatalf("CONNECT setup failed: %q err=%v", string(buf[:n]), rerr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil && !strings.Contains(err.Error(), "context") {
		t.Fatalf("Shutdown: %v", err)
	}

	// The client side of the tunnel must observe the close promptly.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, rerr := conn.Read(buf); rerr == nil {
		// A second read must eventually error once the close propagates.
		if _, rerr2 := conn.Read(buf); rerr2 == nil {
			t.Error("tunnel conn still readable after Shutdown; hijacked conns must be closed")
		}
	}
}

// TestStart_LoopbackDefault pins the bind-safety default shared with
// the sibling proxies: non-loopback binds require the explicit opt-in.
func TestStart_LoopbackDefault(t *testing.T) {
	s, err := New(Config{AllowedHosts: DefaultRegistryHosts()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Start("0.0.0.0:0"); err == nil {
		t.Error("Start(0.0.0.0) without AllowNonLoopback should be rejected")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}
}
