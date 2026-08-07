package modproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plainDial is the test stand-in for the production vetted dialer. The real
// one refuses loopback (127.0.0.0/8 is on the operator denylist), which is
// the gate working correctly — httptest servers just happen to live there.
func plainDial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// newTestServer builds a Server relaying to upstream/sumdb with a dialer
// that can reach loopback, and returns it fronted by an httptest server.
func newTestServer(t *testing.T, upstream, sumdb string) *httptest.Server {
	t.Helper()
	s, err := New(Config{
		Upstream:    upstream,
		SumDB:       sumdb,
		DialContext: plainDial,
		RunID:       "run-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	front := httptest.NewServer(s.Handler())
	t.Cleanup(front.Close)
	return front
}

// noRedirectClient sees exactly what the relay returned, rather than
// following anything itself — the distinction the redirect test turns on.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// TestRelay_FollowsRedirectHostSide is the reason this package exists.
//
// proxy.golang.org 302s large module zips to a CDN host that a fail-closed
// jail cannot reach. The naive implementation — httputil.ReverseProxy —
// passes that 302 straight through to the client, so the sandbox chases the
// CDN itself and is blocked exactly as before. Resolving the hop host-side
// is the whole fix, so this asserts the client sees 200 and the payload,
// never the 302.
func TestRelay_FollowsRedirectHostSide(t *testing.T) {
	const payload = "MODULE-ZIP-BYTES"
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, payload)
	}))
	defer cdn.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/signed-url", http.StatusFound)
	}))
	defer origin.Close()

	front := newTestServer(t, origin.URL, DefaultSumDB)

	resp, err := noRedirectClient().Get(front.URL + "/golang.org/toolchain/@v/v0.0.1-go1.26.5.linux-amd64.zip")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — a 302 here means the redirect reached the sandbox instead of being resolved host-side", resp.StatusCode)
	}
	if string(body) != payload {
		t.Errorf("body = %q, want %q", body, payload)
	}
}

// TestRelay_RedirectToDeniedTargetIsRefused pins the confused-deputy gate.
//
// The relay follows redirects with the HOST's network reach, so an upstream
// that redirects to the cloud metadata endpoint or a private address would
// otherwise have the host fetch it and stream it into the jail. The denylist
// rides on DialContext, so this uses a dialer that refuses the redirect
// target and asserts the failure surfaces as 502 rather than a leak.
func TestRelay_RedirectToDeniedTargetIsRefused(t *testing.T) {
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "INTERNAL-METADATA")
	}))
	defer secret.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, secret.URL+"/latest/meta-data/", http.StatusFound)
	}))
	defer origin.Close()

	_, secretPort, _ := net.SplitHostPort(strings.TrimPrefix(secret.URL, "http://"))

	// Allows the origin, denies the redirect target — the shape the real
	// denylist produces when a public upstream points somewhere internal.
	gated := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if _, port, _ := net.SplitHostPort(addr); port == secretPort {
			return nil, errors.New("resolves only to internal/denied addresses; refusing to connect")
		}
		return plainDial(ctx, network, addr)
	}

	s, err := New(Config{Upstream: origin.URL, DialContext: gated})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	front := httptest.NewServer(s.Handler())
	defer front.Close()

	resp, err := noRedirectClient().Get(front.URL + "/example.com/mod/@v/v1.0.0.zip")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for a denied redirect target", resp.StatusCode)
	}
	if strings.Contains(string(body), "INTERNAL-METADATA") {
		t.Fatal("relay leaked the denied target's body into the response")
	}
}

// TestSumDB_SupportedIsSynthesized pins the capability probe.
//
// cmd/go asks $GOPROXY/sumdb/<host>/supported before routing checksum
// traffic through a proxy; a non-200 sends it to sum.golang.org DIRECTLY,
// which the jail cannot reach. It must be synthesized rather than relayed,
// because the upstream module proxy answers 404 for it.
func TestSumDB_SupportedIsSynthesized(t *testing.T) {
	var upstreamHits int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer origin.Close()

	front := newTestServer(t, origin.URL, DefaultSumDB)
	resp, err := noRedirectClient().Get(front.URL + "/sumdb/sum.golang.org/supported")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200; anything else sends cmd/go to sum.golang.org directly", resp.StatusCode)
	}
	if upstreamHits != 0 {
		t.Errorf("probe reached the upstream %d times; it must be answered locally", upstreamHits)
	}
}

// TestSumDB_RelaysToChecksumHost asserts the sumdb arm strips the
// /sumdb/<host> prefix and targets the checksum database, not the module
// proxy. Getting this wrong yields "module not found" for every build.
func TestSumDB_RelaysToChecksumHost(t *testing.T) {
	var gotPath string
	sumdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		fmt.Fprint(w, "signed-tree-head")
	}))
	defer sumdb.Close()

	moduleProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("sumdb traffic reached the module proxy at %q", r.URL.Path)
	}))
	defer moduleProxy.Close()

	sumHost := strings.TrimPrefix(sumdb.URL, "http://")
	front := newTestServer(t, moduleProxy.URL, sumdb.URL)

	resp, err := noRedirectClient().Get(front.URL + "/sumdb/" + sumHost + "/lookup/github.com/zalando/go-keyring@v0.2.8")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if want := "/lookup/github.com/zalando/go-keyring@v0.2.8"; gotPath != want {
		t.Errorf("upstream path = %q, want %q", gotPath, want)
	}
}

// TestServeHTTP_UnknownSumDBIs404 keeps an unrelayed checksum database from
// falling through to the module proxy, where it would 404 for a misleading
// reason.
func TestServeHTTP_UnknownSumDBIs404(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unknown sumdb reached the module proxy at %q", r.URL.Path)
	}))
	defer origin.Close()

	front := newTestServer(t, origin.URL, DefaultSumDB)
	resp, err := noRedirectClient().Get(front.URL + "/sumdb/evil.example.com/lookup/x@v1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestRelay_PreservesEscapedPath guards the encoding the GOPROXY protocol
// depends on: module paths case-encode uppercase letters as "!x", and
// versions carry "@" and "+incompatible". Re-encoding any of it turns a
// valid fetch into a spurious "module not found", so the escaped path must
// arrive byte-identical.
func TestRelay_PreservesEscapedPath(t *testing.T) {
	const want = "/github.com/!aidan!allchin/bifrost/core/@v/v1.7.4+incompatible.info"
	var got string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.EscapedPath()
	}))
	defer origin.Close()

	front := newTestServer(t, origin.URL, DefaultSumDB)
	resp, err := noRedirectClient().Get(front.URL + want)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	if got != want {
		t.Errorf("upstream escaped path = %q, want %q", got, want)
	}
}

// TestRelay_PassesThroughNotFound keeps 404/410 verbatim. cmd/go reads those
// to conclude a module is absent and to advance its GOPROXY list; rewriting
// them to 502 would turn "not found" into "proxy broken".
func TestRelay_PassesThroughNotFound(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusGone} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		front := newTestServer(t, origin.URL, DefaultSumDB)
		resp, err := noRedirectClient().Get(front.URL + "/example.com/m/@v/list")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != code {
			t.Errorf("status = %d, want %d passed through verbatim", resp.StatusCode, code)
		}
		origin.Close()
	}
}

// TestRelay_DropsInboundHeaders asserts nothing the jail sent reaches a
// public upstream. The relay builds a fresh request and copies no inbound
// headers, so this is structural rather than a strip step that could be
// forgotten — but it is exactly the kind of structure a later refactor
// "helpfully" reintroduces, so it is pinned.
func TestRelay_DropsInboundHeaders(t *testing.T) {
	var gotAuth, gotCookie string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
	}))
	defer origin.Close()

	front := newTestServer(t, origin.URL, DefaultSumDB)
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/example.com/m/@v/list", nil)
	req.Header.Set("Authorization", "Bearer run-token-should-not-escape")
	req.Header.Set("Cookie", "session=nope")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	if gotAuth != "" {
		t.Errorf("Authorization forwarded upstream as %q; it must never leave the host", gotAuth)
	}
	if gotCookie != "" {
		t.Errorf("Cookie forwarded upstream as %q", gotCookie)
	}
}

// TestServeHTTP_RejectsWriteMethods keeps the read-only protocol surface
// read-only: a write must not reach an upstream at all.
func TestServeHTTP_RejectsWriteMethods(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("%s reached the upstream", r.Method)
	}))
	defer origin.Close()

	front := newTestServer(t, origin.URL, DefaultSumDB)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequest(method, front.URL+"/example.com/m/@v/list", strings.NewReader("x"))
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, resp.StatusCode)
		}
	}
}

func TestNew_RejectsMalformedUpstream(t *testing.T) {
	for _, raw := range []string{
		"https://proxy.golang.org/base/path", // a path would corrupt every relayed URL
		"ftp://proxy.golang.org",
		"https://",
		"proxy.golang.org", // no scheme
	} {
		if _, err := New(Config{Upstream: raw}); err == nil {
			t.Errorf("New(Upstream=%q) = nil error; want a loud construction failure", raw)
		}
	}
}

func TestNew_DefaultsToPublicProxyAndSumDB(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := s.upstream.String(); got != DefaultUpstream {
		t.Errorf("upstream = %q, want %q", got, DefaultUpstream)
	}
	if got := s.sumDBPrefix(); got != "/sumdb/sum.golang.org" {
		t.Errorf("sumdb prefix = %q, want /sumdb/sum.golang.org", got)
	}
}

// TestStart_RefusesNonLoopbackWithoutOptIn keeps an unauthenticated relay
// off the LAN by accident; the veth bind opts in explicitly.
func TestStart_RefusesNonLoopbackWithoutOptIn(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Start("0.0.0.0:0"); err == nil {
		t.Error("Start(0.0.0.0:0) succeeded without AllowNonLoopback")
	}
}

func TestShutdown_SafeBeforeStart(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown before Start = %v, want nil", err)
	}
}
