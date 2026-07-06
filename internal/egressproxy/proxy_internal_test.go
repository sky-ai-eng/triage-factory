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
// never resolved or dialed.
func TestProxy_DeniesOffAllowlistHost(t *testing.T) {
	var dialed []string
	var denials []DeniedConnect
	addr := newTestServer(t, Config{
		AllowedHosts: []string{"registry.npmjs.org"},
		RecordDenial: func(d DeniedConnect) { denials = append(denials, d) },
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
	if len(denials) != 1 || denials[0].Target != "evil.example.com:443" {
		t.Errorf("RecordDenial = %+v, want one entry for evil.example.com:443", denials)
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
