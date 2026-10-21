package egressproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
	"unicode/utf8"
)

// hostsOnly is registries for hosts with no read POST paths.
func hostsOnly(hosts ...string) []Registry {
	out := make([]Registry, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, Registry{Host: h})
	}
	return out
}

// npmAndPyPI is the shape of the default table, with one read path on npm.
var npmAndPyPI = []Registry{
	{Host: "registry.npmjs.org", ReadPOSTPaths: []string{"/-/npm/v1/security/advisories/bulk"}},
	{Host: "files.pythonhosted.org"},
}

// --- test PKI ----------------------------------------------------------------

// testPKI is a CA standing in for the public roots: a registry stand-in
// presents a certificate it issued, and upstreamRoots trusts it for the test.
type testPKI struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test public root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testPKI{cert: cert, key: key, pool: pool}
}

// issue signs a server certificate for names, valid until notAfter.
func (p *testPKI) issue(t *testing.T, notAfter time.Time, names ...string) tls.Certificate {
	t.Helper()
	return signServerCert(t, p.cert, p.key, notAfter, names...)
}

// selfSigned is a server certificate for names that no root vouches for.
func selfSigned(t *testing.T, names ...string) tls.Certificate {
	t.Helper()
	return signServerCert(t, nil, nil, time.Now().Add(time.Hour), names...)
}

func signServerCert(t *testing.T, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, notAfter time.Time, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := randomSerial()
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    notAfter.Add(-2 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// trustUpstream makes upstreamRoots pool for the rest of the test.
func trustUpstream(t *testing.T, pool *x509.CertPool) {
	t.Helper()
	upstreamRoots = pool
	t.Cleanup(func() { upstreamRoots = nil })
}

// --- harness ----------------------------------------------------------------

// stubRegistry is an HTTPS server presenting cert, counting the requests that
// reach it.
type stubRegistry struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newStubRegistry(t *testing.T, cert tls.Certificate, h http.HandlerFunc) *stubRegistry {
	t.Helper()
	reg := &stubRegistry{}
	reg.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reg.hits.Add(1)
		h(w, r)
	}))
	reg.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	reg.srv.StartTLS()
	t.Cleanup(reg.srv.Close)
	return reg
}

// interceptHarness is a proxy whose resolver answers a public address for
// every host and whose dial reaches a stub registry, with every denial
// collected.
type interceptHarness struct {
	srv     *Server
	addr    string
	denials chan DeniedConnect

	mu      sync.Mutex
	resolve []netip.Addr
	dials   int
}

// opts adjust the Server after the harness has set its seams and before it
// starts, so a test can wrap them.
func newInterceptHarness(t *testing.T, registries []Registry, upstreamAddr string, opts ...func(*Server)) *interceptHarness {
	t.Helper()
	h := &interceptHarness{
		denials: make(chan DeniedConnect, 64),
		resolve: []netip.Addr{netip.MustParseAddr("104.16.0.35")},
	}
	s, err := New(Config{
		Registries:   registries,
		RecordDenial: func(_ context.Context, d DeniedConnect) { h.denials <- d },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.resolve = func(context.Context, string) ([]netip.Addr, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.resolve, nil
	}
	s.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		h.mu.Lock()
		h.dials++
		h.mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, upstreamAddr)
	}
	for _, opt := range opts {
		opt(s)
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
	h.srv, h.addr = s, addr
	return h
}

func (h *interceptHarness) dialCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dials
}

// jailRoots is what the jail's trust file adds: the engagement's CA.
func (h *interceptHarness) jailRoots(t *testing.T) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(h.srv.CACertPEM()) {
		t.Fatal("CACertPEM did not parse")
	}
	return pool
}

// client is a sandbox client: the proxy from its env, the CA from its trust
// file, and no transparent decompression, so a test sees the bytes on the
// wire.
func (h *interceptHarness) client(t *testing.T) *http.Client {
	t.Helper()
	proxyURL, _ := url.Parse("http://" + h.addr)
	return &http.Client{
		Transport: &http.Transport{
			Proxy:              http.ProxyURL(proxyURL),
			TLSClientConfig:    &tls.Config{RootCAs: h.jailRoots(t)},
			DisableCompression: true,
		},
		Timeout: 5 * time.Second,
	}
}

// session CONNECTs to target and completes TLS for serverName, offering
// nextProtos.
func (h *interceptHarness) session(t *testing.T, target, serverName string, nextProtos []string) (*tls.Conn, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s = %s, want 200", target, resp.Status)
	}
	tc := tls.Client(conn, &tls.Config{ServerName: serverName, RootCAs: h.jailRoots(t), NextProtos: nextProtos})
	return tc, tc.Handshake()
}

// roundTrip writes raw on an established session and reads the response.
func roundTrip(t *testing.T, tc *tls.Conn, raw string) *http.Response {
	t.Helper()
	if _, err := io.WriteString(tc, raw); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp
}

// expectDenial waits for a recorded denial whose reason contains substr.
func (h *interceptHarness) expectDenial(t *testing.T, substr string) DeniedConnect {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case d := <-h.denials:
			if strings.Contains(d.Reason, substr) {
				return d
			}
		case <-deadline:
			t.Fatalf("no denial recorded containing %q", substr)
			return DeniedConnect{}
		}
	}
}

// npmRegistry is a stub registry.npmjs.org under a test CA the proxy trusts.
func npmRegistry(t *testing.T, h http.HandlerFunc, opts ...func(*Server)) (*stubRegistry, *interceptHarness) {
	t.Helper()
	pki := newTestPKI(t)
	trustUpstream(t, pki.pool)
	reg := newStubRegistry(t, pki.issue(t, time.Now().Add(time.Hour), "registry.npmjs.org"), h)
	return reg, newInterceptHarness(t, npmAndPyPI, reg.srv.Listener.Addr().String(), opts...)
}

// --- relaying -----------------------------------------------------------------

// TestIntercept_RelaysReadsByteForByte pins pure pass-through: the body the
// jail receives is the body the registry sent, compressed as the registry
// compressed it, whether framed by length or chunked; the upstream request
// carries the CONNECT host and no encoding the client did not ask for; and
// the session reuses the one verified upstream connection.
func TestIntercept_RelaysReadsByteForByte(t *testing.T) {
	payload := make([]byte, 256<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []*http.Request
	reg, h := npmRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(context.Background()))
		mu.Unlock()
		w.Header().Set("Content-Encoding", "gzip")
		switch r.URL.Path {
		case "/fixed":
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		case "/chunked":
			_, _ = w.Write(payload[:1000])
			w.(http.Flusher).Flush()
			_, _ = w.Write(payload[1000:])
		}
	})
	client := h.client(t)

	for _, path := range []string{"/fixed", "/chunked", "/fixed?write=no"} {
		resp, err := client.Get("https://registry.npmjs.org" + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %s", path, resp.Status)
		}
		if !bytes.Equal(body, payload) {
			t.Errorf("GET %s: body differs from the registry's (%d bytes vs %d)", path, len(body), len(payload))
		}
		if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
			t.Errorf("GET %s: Content-Encoding = %q, want the registry's gzip", path, got)
		}
	}

	resp, err := client.Head("https://registry.npmjs.org/fixed")
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	_ = resp.Body.Close()
	if resp.ContentLength != int64(len(payload)) {
		t.Errorf("HEAD Content-Length = %d, want %d", resp.ContentLength, len(payload))
	}

	mu.Lock()
	defer mu.Unlock()
	for _, r := range seen {
		if r.Host != "registry.npmjs.org" {
			t.Errorf("upstream Host = %q, want the CONNECT host", r.Host)
		}
		if ae := r.Header.Get("Accept-Encoding"); ae != "" {
			t.Errorf("upstream saw Accept-Encoding %q the client never sent", ae)
		}
		for _, hdr := range []string{"Via", "X-Forwarded-For", "Proxy-Authorization"} {
			if v := r.Header.Get(hdr); v != "" {
				t.Errorf("upstream saw %s: %q", hdr, v)
			}
		}
	}
	if got := reg.hits.Load(); got != 4 {
		t.Errorf("registry served %d requests, want 4", got)
	}
	if got := h.dialCount(); got != 1 {
		t.Errorf("dialed upstream %d times, want 1: a kept-alive session reuses the verified connection", got)
	}
}

// TestIntercept_RedialIsVetted pins that a connection the session dials after
// the first goes through the same gate: when the registry closes the first
// connection and the host now resolves internally, the next request is
// refused rather than dialed.
func TestIntercept_RedialIsVetted(t *testing.T) {
	_, h := npmRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, "ok")
	})
	tc, err := h.session(t, "registry.npmjs.org:443", "registry.npmjs.org", nil)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	resp := roundTrip(t, tc, "GET /a HTTP/1.1\r\nHost: registry.npmjs.org\r\n\r\n")
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first GET = %s", resp.Status)
	}
	// The registry closed its side; the jail's session stays open, since the
	// client never asked to close, and the next request redials.

	h.mu.Lock()
	h.resolve = []netip.Addr{netip.MustParseAddr("169.254.169.254")}
	h.mu.Unlock()

	resp = roundTrip(t, tc, "GET /b HTTP/1.1\r\nHost: registry.npmjs.org\r\n\r\n")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET after rebinding to metadata = %s, want 403", resp.Status)
	}
	h.expectDenial(t, "internal/denied addresses")
	if got := h.dialCount(); got != 1 {
		t.Errorf("dialed %d times; a denylisted redial must never reach the dialer", got)
	}
}

// TestIntercept_AllowsListedReadPOST pins the one write-shaped method a
// registry takes: a POST to a listed read path, relayed with its body intact,
// including after an Expect: 100-continue.
func TestIntercept_AllowsListedReadPOST(t *testing.T) {
	gzipped := []byte("\x1f\x8b\x08\x00 not really gzip, but bytes the interceptor must not touch")
	var got atomic.Value
	_, h := npmRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Store(b)
		_, _ = io.WriteString(w, "{}")
	})

	req, _ := http.NewRequest(http.MethodPost, "https://registry.npmjs.org/-/npm/v1/security/advisories/bulk", bytes.NewReader(gzipped))
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := h.client(t).Do(req)
	if err != nil {
		t.Fatalf("POST bulk: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST bulk = %s, want 200", resp.Status)
	}
	if b, _ := got.Load().([]byte); !bytes.Equal(b, gzipped) {
		t.Errorf("registry received body %q, want %q", b, gzipped)
	}

	tc, err := h.session(t, "registry.npmjs.org:443", "registry.npmjs.org", nil)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if _, err := io.WriteString(tc, "POST /-/npm/v1/security/advisories/bulk HTTP/1.1\r\nHost: registry.npmjs.org\r\nContent-Length: 4\r\nExpect: 100-continue\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(tc)
	line, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 100") {
		t.Fatalf("after Expect: 100-continue got %q (%v), want 100 Continue", line, err)
	}
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tc, "data"); err != nil {
		t.Fatal(err)
	}
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST after 100-continue = %s", resp.Status)
	}
	if b, _ := got.Load().([]byte); string(b) != "data" {
		t.Errorf("registry received %q after 100-continue, want %q", b, "data")
	}
}

// --- refusals -----------------------------------------------------------------

// TestIntercept_RefusesWrites pins read-only registries: npm publish's PUT
// and every other write is refused with a reason naming the rule, never
// reaches the registry, and is recorded with its method.
func TestIntercept_RefusesWrites(t *testing.T) {
	reg, h := npmRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "should not be reached")
	})
	client := h.client(t)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPut, "/my-package"}, // npm publish
		{http.MethodDelete, "/my-package/-rev/1-abc"},
		{http.MethodPatch, "/my-package"},
		{http.MethodPost, "/-/v1/login"},
		{http.MethodPost, "/-/npm/v1/security/advisories/bulk/extra"},
		{http.MethodPost, "/-/npm/v1/security/advisories%2Fbulk"},
		{http.MethodOptions, "/"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, "https://registry.npmjs.org"+tc.path, strings.NewReader(`{"name":"my-package"}`))
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", tc.method, tc.path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s %s = %s, want 403", tc.method, tc.path, resp.Status)
			}
			if !strings.Contains(resp.Status, "read-only") || !strings.Contains(string(body), "read-only") {
				t.Errorf("refusal should name the read-only rule in its status line and body; got %q / %q", resp.Status, body)
			}
			h.expectDenial(t, tc.method+" ")
		})
	}
	if got := reg.hits.Load(); got != 0 {
		t.Errorf("registry received %d refused requests", got)
	}
}

// TestIntercept_OneHostnameEndToEnd pins fronting closed at both layers: a
// client that handshakes for another allowlisted name on this CONNECT fails
// its handshake, and a request whose Host is anything but the CONNECT host is
// refused.
func TestIntercept_OneHostnameEndToEnd(t *testing.T) {
	reg, h := npmRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	_, err := h.session(t, "registry.npmjs.org:443", "files.pythonhosted.org", nil)
	if err == nil {
		t.Fatal("handshake for files.pythonhosted.org on a registry.npmjs.org CONNECT succeeded")
	}
	if !strings.Contains(err.Error(), "unrecognized name") {
		t.Errorf("handshake error = %v, want an unrecognized_name alert", err)
	}
	d := h.expectDenial(t, "TLS server name")
	if !strings.Contains(d.Reason, "files.pythonhosted.org") {
		t.Errorf("denial %q should name the server name the client asked for", d.Reason)
	}

	for name, raw := range map[string]string{
		"other allowlisted host": "GET / HTTP/1.1\r\nHost: files.pythonhosted.org\r\n\r\n",
		"other port":             "GET / HTTP/1.1\r\nHost: registry.npmjs.org:8443\r\n\r\n",
		"no Host":                "GET / HTTP/1.0\r\n\r\n",
		"absolute form":          "GET https://files.pythonhosted.org/ HTTP/1.1\r\nHost: registry.npmjs.org\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			tc, err := h.session(t, "registry.npmjs.org:443", "registry.npmjs.org", nil)
			if err != nil {
				t.Fatalf("handshake: %v", err)
			}
			resp := roundTrip(t, tc, raw)
			if resp.StatusCode < 400 {
				t.Fatalf("request with %s = %s, want a refusal", name, resp.Status)
			}
		})
	}
	if got := reg.hits.Load(); got != 0 {
		t.Errorf("registry received %d requests addressed elsewhere", got)
	}
}

// TestIntercept_OneRequestShape pins condition 5: everything that could make a
// read act as something else, or be framed two ways, is refused before it
// reaches the registry.
func TestIntercept_OneRequestShape(t *testing.T) {
	reg, h := npmRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	const bulk = "/-/npm/v1/security/advisories/bulk"
	for name, tc := range map[string]struct {
		raw    string
		status int
	}{
		"upgrade":                   {"GET / HTTP/1.1\r\nHost: registry.npmjs.org\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", http.StatusForbidden},
		"nested CONNECT":            {"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n", http.StatusForbidden},
		"length and chunked":        {"POST " + bulk + " HTTP/1.1\r\nHost: registry.npmjs.org\r\nContent-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", http.StatusBadRequest},
		"two lengths":               {"POST " + bulk + " HTTP/1.1\r\nHost: registry.npmjs.org\r\nContent-Length: 4\r\nContent-Length: 4\r\n\r\nabcd", http.StatusBadRequest},
		"GET with a body":           {"GET / HTTP/1.1\r\nHost: registry.npmjs.org\r\nContent-Length: 3\r\n\r\nabc", http.StatusForbidden},
		"GET with a chunked body":   {"GET / HTTP/1.1\r\nHost: registry.npmjs.org\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", http.StatusForbidden},
		"HEAD with a body":          {"HEAD / HTTP/1.1\r\nHost: registry.npmjs.org\r\nContent-Length: 3\r\n\r\nabc", http.StatusForbidden},
		"X-HTTP-Method-Override":    {"GET / HTTP/1.1\r\nHost: registry.npmjs.org\r\nX-HTTP-Method-Override: PUT\r\n\r\n", http.StatusForbidden},
		"X-HTTP-Method":             {"GET / HTTP/1.1\r\nHost: registry.npmjs.org\r\nX-HTTP-Method: DELETE\r\n\r\n", http.StatusForbidden},
		"X-Method-Override":         {"GET / HTTP/1.1\r\nHost: registry.npmjs.org\r\nx-method-override: PUT\r\n\r\n", http.StatusForbidden},
		"underscored override":      {"GET / HTTP/1.1\r\nHost: registry.npmjs.org\r\nX_HTTP_Method_Override: PUT\r\n\r\n", http.StatusForbidden},
		"_method parameter":         {"GET /left-pad?_method=PUT HTTP/1.1\r\nHost: registry.npmjs.org\r\n\r\n", http.StatusForbidden},
		"override on a listed POST": {"POST " + bulk + " HTTP/1.1\r\nHost: registry.npmjs.org\r\nX-HTTP-Method-Override: PUT\r\nContent-Length: 2\r\n\r\n{}", http.StatusForbidden},
		"unknown expectation":       {"GET / HTTP/1.1\r\nHost: registry.npmjs.org\r\nExpect: something\r\n\r\n", http.StatusExpectationFailed},
		"HTTP/2 preface":            {"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n", 0},
		"head too large":            {"GET / HTTP/1.1\r\nHost: registry.npmjs.org\r\nX-Pad: " + strings.Repeat("a", maxRequestHead) + "\r\n\r\n", http.StatusRequestHeaderFieldsTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			sess, err := h.session(t, "registry.npmjs.org:443", "registry.npmjs.org", nil)
			if err != nil {
				t.Fatalf("handshake: %v", err)
			}
			resp := roundTrip(t, sess, tc.raw)
			if tc.status == 0 {
				if resp.StatusCode < 400 {
					t.Fatalf("got %s, want a refusal", resp.Status)
				}
			} else if resp.StatusCode != tc.status {
				t.Fatalf("got %s, want %d", resp.Status, tc.status)
			}
			if !resp.Close {
				t.Error("a refusal must close the session")
			}
		})
	}
	if got := reg.hits.Load(); got != 0 {
		t.Errorf("registry received %d refused requests", got)
	}
}

// TestIntercept_ServesOnlyHTTP11 pins the jail-facing protocol: a client
// offering only HTTP/2 fails its handshake, and one offering both gets
// HTTP/1.1.
func TestIntercept_ServesOnlyHTTP11(t *testing.T) {
	_, h := npmRegistry(t, func(w http.ResponseWriter, _ *http.Request) {})
	if _, err := h.session(t, "registry.npmjs.org:443", "registry.npmjs.org", []string{"h2"}); err == nil {
		t.Error("an h2-only client completed its handshake")
	}
	tc, err := h.session(t, "registry.npmjs.org:443", "registry.npmjs.org", []string{"h2", "http/1.1"})
	if err != nil {
		t.Fatalf("handshake offering h2 and http/1.1: %v", err)
	}
	if got := tc.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
		t.Errorf("negotiated %q, want http/1.1", got)
	}
}

// TestIntercept_RefusesUnverifiedUpstream pins condition 8's refusal tests:
// the CONNECT is refused, loudly and on the record, unless the registry's
// certificate verifies for the CONNECT host under the roots.
func TestIntercept_RefusesUnverifiedUpstream(t *testing.T) {
	pki := newTestPKI(t)
	for name, cert := range map[string]tls.Certificate{
		"self-signed":                selfSigned(t, "registry.npmjs.org"),
		"expired":                    pki.issue(t, time.Now().Add(-time.Minute), "registry.npmjs.org"),
		"wrong hostname":             pki.issue(t, time.Now().Add(time.Hour), "example.com"),
		"another allowlisted host's": pki.issue(t, time.Now().Add(time.Hour), "files.pythonhosted.org"),
	} {
		t.Run(name, func(t *testing.T) {
			trustUpstream(t, pki.pool)
			reg := newStubRegistry(t, cert, func(w http.ResponseWriter, _ *http.Request) {})
			h := newInterceptHarness(t, npmAndPyPI, reg.srv.Listener.Addr().String())
			resp := rawConnect(t, h.addr, "registry.npmjs.org:443", "")
			if !strings.HasPrefix(resp, "HTTP/1.1 502") || !strings.Contains(resp, "TLS with registry.npmjs.org failed") {
				t.Fatalf("CONNECT with a %s certificate got:\n%s\nwant 502 naming the failed verification", name, resp)
			}
			h.expectDenial(t, "TLS with registry.npmjs.org failed")
			if got := reg.hits.Load(); got != 0 {
				t.Errorf("registry served %d requests over an unverified connection", got)
			}
		})
	}
}

// TestIntercept_ShutdownEndsSessions pins that a session open at teardown is
// closed rather than outliving the run.
func TestIntercept_ShutdownEndsSessions(t *testing.T) {
	_, h := npmRegistry(t, func(w http.ResponseWriter, _ *http.Request) {})
	tc, err := h.session(t, "registry.npmjs.org:443", "registry.npmjs.org", nil)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = h.srv.Shutdown(ctx)
	_ = tc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := tc.Read(make([]byte, 1)); err == nil {
		t.Error("session still readable after Shutdown")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("session still open after Shutdown (read timed out instead of failing)")
	}
}

// --- the authority -------------------------------------------------------------

// TestAuthority_ConstrainedToTheAllowlist pins condition 1's certificate: a
// CA for hours, signing only the hosts the tunnel admits, so a leaf for any
// other name fails verification even when the key itself signed it.
func TestAuthority_ConstrainedToTheAllowlist(t *testing.T) {
	now := time.Now()
	hosts := []string{"registry.npmjs.org", "files.pythonhosted.org"}
	a, err := newAuthority(hosts, now)
	if err != nil {
		t.Fatal(err)
	}
	c := a.cert
	if !c.IsCA || !c.MaxPathLenZero || c.MaxPathLen != 0 {
		t.Errorf("CA must be a CA with path length zero; IsCA=%v MaxPathLen=%d", c.IsCA, c.MaxPathLen)
	}
	if !c.PermittedDNSDomainsCritical || strings.Join(c.PermittedDNSDomains, ",") != strings.Join(hosts, ",") {
		t.Errorf("PermittedDNSDomains = %v (critical %v), want exactly %v", c.PermittedDNSDomains, c.PermittedDNSDomainsCritical, hosts)
	}
	if len(c.ExcludedIPRanges) != 2 {
		t.Errorf("ExcludedIPRanges = %v, want all of IPv4 and IPv6", c.ExcludedIPRanges)
	}
	if c.NotAfter.Sub(now) > authorityValidity || c.NotBefore.After(now) {
		t.Errorf("validity %s to %s; want from before now to at most %s ahead", c.NotBefore, c.NotAfter, authorityValidity)
	}
	if block, rest := pem.Decode(a.certPEM); block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		t.Error("certPEM must hold the certificate and nothing else")
	}

	roots := x509.NewCertPool()
	roots.AddCert(c)
	verify := func(der []byte, name string) error {
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		return err
	}

	leaf, err := a.leaf("registry.npmjs.org")
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(leaf.Certificate[0], "registry.npmjs.org"); err != nil {
		t.Errorf("leaf for an admitted host does not verify: %v", err)
	}
	if again, _ := a.leaf("registry.npmjs.org"); again != leaf {
		t.Error("leaf was minted twice for one host")
	}

	forged := signServerCert(t, c, a.key, now.Add(time.Hour), "api.github.com")
	if err := verify(forged.Certificate[0], "api.github.com"); err == nil {
		t.Error("a leaf the CA signed for api.github.com verified; the name constraint must refuse it")
	}
}

// TestNew_NoRegistriesNoAuthority pins that an empty allowlist gets no CA: an
// unconstrained one is what a CA with no permitted names would be.
func TestNew_NoRegistriesNoAuthority(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if s.CACertPEM() != nil {
		t.Error("CACertPEM is non-nil with no registries")
	}
}

// TestIntercept_SessionCap pins the per-run bound on sessions: once a run
// holds the cap, the next CONNECT is refused with a reason and recorded, and
// the slot a session frees when it ends can be used again.
func TestIntercept_SessionCap(t *testing.T) {
	_, h := npmRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}, func(s *Server) { s.sessionSlots = make(chan struct{}, 1) })

	first, err := h.session(t, "registry.npmjs.org:443", "registry.npmjs.org", nil)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	resp := rawConnect(t, h.addr, "registry.npmjs.org:443", "")
	if !strings.HasPrefix(resp, "HTTP/1.1 503") || !strings.Contains(resp, "connections open") {
		t.Fatalf("CONNECT past the cap got:\n%s\nwant 503 naming the limit", resp)
	}
	h.expectDenial(t, "connections open")

	_ = first.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if resp = rawConnect(t, h.addr, "registry.npmjs.org:443", ""); strings.HasPrefix(resp, "HTTP/1.1 200") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("CONNECT after the first session closed got:\n%s\nwant 200; its slot was not freed", resp)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestIntercept_ShutdownRefusesASessionStillDialing pins that a CONNECT still
// dialing its registry when Shutdown sweeps the sessions is refused once the
// dial returns, rather than becoming a session nothing ends.
func TestIntercept_ShutdownRefusesASessionStillDialing(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	_, h := npmRegistry(t, func(w http.ResponseWriter, _ *http.Request) {}, func(s *Server) {
		inner := s.dial
		s.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			once.Do(func() { close(entered) })
			<-release
			return inner(ctx, network, addr)
		}
	})

	conn, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "CONNECT registry.npmjs.org:443 HTTP/1.1\r\nHost: registry.npmjs.org:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	<-entered

	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.srv.Shutdown(ctx)
		close(done)
	}()
	for {
		h.srv.mu.Lock()
		closed := h.srv.closed
		h.srv.mu.Unlock()
		if closed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if strings.Contains(string(buf[:n]), "200") {
		t.Fatalf("a CONNECT that finished dialing after Shutdown was answered %q", buf[:n])
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the connection was left open after Shutdown")
	}
	<-done
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.sessions) != 0 {
		t.Errorf("%d sessions registered after Shutdown", len(h.srv.sessions))
	}
}

// TestIntercept_SessionEndsWithCloseNotify pins how a session ends: with a TLS
// close_notify before the TCP close. A response with neither a length nor
// chunking is delimited by that close, and OpenSSL 3 clients such as curl
// report a body ended without close_notify as an unexpected EOF. Go's client
// does not, so the test reads the records on the wire instead, over TLS 1.2,
// where a record's type is not encrypted.
func TestIntercept_SessionEndsWithCloseNotify(t *testing.T) {
	_, h := npmRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(brw, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nuntil the connection closes")
		_ = brw.Flush()
	})
	raw, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(raw, "CONNECT registry.npmjs.org:443 HTTP/1.1\r\nHost: registry.npmjs.org:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(raw)
	if resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect}); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	wire := &recordingConn{Conn: raw, r: br}
	tc := tls.Client(wire, &tls.Config{ServerName: "registry.npmjs.org", RootCAs: h.jailRoots(t), MaxVersion: tls.VersionTLS12})
	if _, err := io.WriteString(tc, "GET /stream HTTP/1.1\r\nHost: registry.npmjs.org\r\n\r\n"); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := io.ReadAll(tc); err != nil {
		t.Fatalf("read the response: %v", err)
	}

	const alertRecord = 21
	var types []byte
	for rec := wire.seen.Bytes(); len(rec) >= 5; {
		types = append(types, rec[0])
		n := 5 + int(rec[3])<<8 + int(rec[4])
		if n > len(rec) {
			break
		}
		rec = rec[n:]
	}
	if len(types) == 0 || types[len(types)-1] != alertRecord {
		t.Errorf("record types from the interceptor %v; the session did not end with close_notify", types)
	}
}

// recordingConn keeps every byte read through it, reading first from r,
// which may hold bytes already buffered from the connection.
type recordingConn struct {
	net.Conn
	r    io.Reader
	seen bytes.Buffer
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.seen.Write(p[:n])
	return n, err
}

// TestPeekRequestHead_ByteAtATime pins the head scan against the slowest
// delivery there is: the head comes back whole, through its blank line, with
// nothing consumed, for CRLF and bare-LF lines alike.
func TestPeekRequestHead_ByteAtATime(t *testing.T) {
	for _, head := range []string{
		"GET / HTTP/1.1\r\nHost: registry.npmjs.org\r\nAccept: */*\r\n\r\n",
		"GET / HTTP/1.1\nHost: registry.npmjs.org\n\n",
	} {
		br := bufio.NewReaderSize(iotest.OneByteReader(strings.NewReader(head+"next")), maxRequestHead)
		got, err := peekRequestHead(br)
		if err != nil {
			t.Fatalf("peekRequestHead(%q): %v", head, err)
		}
		if string(got) != head {
			t.Errorf("peekRequestHead = %q, want %q", got, head)
		}
		if rest, _ := br.Peek(len(head)); string(rest) != head {
			t.Errorf("peekRequestHead consumed input: buffer starts %q", rest)
		}
	}
}

// TestQuotable_BoundsClientValues pins the cap on client-supplied text in a
// refusal, without splitting a UTF-8 sequence.
func TestQuotable_BoundsClientValues(t *testing.T) {
	q := quotable(strings.Repeat("é", maxQuotedValue))
	if len(q) > maxQuotedValue+len("...") || !utf8.ValidString(q) {
		t.Errorf("quotable gave %d bytes (valid UTF-8 %v), want at most %d", len(q), utf8.ValidString(q), maxQuotedValue+3)
	}
	if got := quotable("short"); got != "short" {
		t.Errorf("quotable(short) = %q", got)
	}
}
