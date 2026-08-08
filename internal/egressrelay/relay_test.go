package egressrelay

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

// newTestServer fronts cfg with an httptest server, substituting the
// loopback-capable dialer.
func newTestServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	cfg.DialContext = plainDial
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	front := httptest.NewServer(s.Handler())
	t.Cleanup(front.Close)
	return front
}

// goTestConfig is the Go entry's route shape pointed at test upstreams —
// the same table production ships (via the shared goModules constructor),
// so the go-shaped tests exercise real catalog data.
func goTestConfig(moduleUpstream, sumDB string) Config {
	return goModules(moduleUpstream, sumDB)
}

// noRedirectClient sees exactly what the relay returned, rather than
// following anything itself — the distinction the redirect test turns on.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// TestRelay_FollowsRedirectHostSide is the reason this lane exists.
//
// Registries in this catalog 302 artifacts to shared-storage CDN hosts
// that a fail-closed jail cannot reach. The naive implementation —
// httputil.ReverseProxy — passes that 302 straight through to the client,
// so the sandbox chases the CDN itself and is blocked exactly as before.
// Resolving the hop host-side is the whole fix, so this asserts the client
// sees 200 and the payload, never the 302.
func TestRelay_FollowsRedirectHostSide(t *testing.T) {
	const payload = "ARTIFACT-BYTES"
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, payload)
	}))
	defer cdn.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/signed-url", http.StatusFound)
	}))
	defer origin.Close()

	front := newTestServer(t, goTestConfig(origin.URL, GoSumDB))

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
// The relay follows redirects with the HOST's network reach, so an
// upstream that redirects to the cloud metadata endpoint or a private
// address would otherwise have the host fetch it and stream it into the
// jail. The denylist rides on DialContext, so this uses a dialer that
// refuses the redirect target and asserts the failure surfaces as 502
// rather than a leak.
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

	cfg := Config{
		Name:        "test",
		Routes:      []Route{{Prefix: "/", Upstream: origin.URL}},
		DialContext: gated,
	}
	s, err := New(cfg)
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

// TestRoutes_SyntheticAnsweredLocally pins synthetic routes: answered with
// the configured status, contacting no upstream. The Go sumdb /supported
// probe is the motivating case — cmd/go routes checksum traffic through
// the relay only on a 200 here, and the upstream module proxy answers 404.
func TestRoutes_SyntheticAnsweredLocally(t *testing.T) {
	var upstreamHits int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer origin.Close()

	front := newTestServer(t, goTestConfig(origin.URL, GoSumDB))
	resp, err := noRedirectClient().Get(front.URL + "/sumdb/sum.golang.org/supported")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200; anything else sends cmd/go to sum.golang.org directly", resp.StatusCode)
	}
	if upstreamHits != 0 {
		t.Errorf("probe reached the upstream %d times; a synthetic route must be answered locally", upstreamHits)
	}
}

// TestRoutes_StripPrefixTargetsSecondUpstream asserts a StripPrefix route
// rewrites onto its own upstream with the prefix removed and the leading
// slash restored. In the Go entry, getting this wrong yields "module not
// found" for every build (checksum lookups landing on the module proxy).
func TestRoutes_StripPrefixTargetsSecondUpstream(t *testing.T) {
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
	front := newTestServer(t, goTestConfig(moduleProxy.URL, sumdb.URL))

	resp, err := noRedirectClient().Get(front.URL + "/sumdb/" + sumHost + "/lookup/github.com/zalando/go-keyring@v0.2.8")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if want := "/lookup/github.com/zalando/go-keyring@v0.2.8"; gotPath != want {
		t.Errorf("upstream path = %q, want %q", gotPath, want)
	}
}

// TestRoutes_FenceStopsFallthrough keeps a fenced namespace from falling
// through to a broader route, where it would fail for a misleading reason
// (an unrelayed checksum database 404ing off the module proxy).
func TestRoutes_FenceStopsFallthrough(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fenced path reached the fallthrough upstream at %q", r.URL.Path)
	}))
	defer origin.Close()

	front := newTestServer(t, goTestConfig(origin.URL, GoSumDB))
	resp, err := noRedirectClient().Get(front.URL + "/sumdb/evil.example.com/lookup/x@v1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestRoutes_FirstMatchWins pins the ordered-table contract the catalog
// depends on: a narrow route listed before a broad one takes the request
// even though both prefixes match.
func TestRoutes_FirstMatchWins(t *testing.T) {
	var narrowHits, broadHits int
	narrow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { narrowHits++ }))
	defer narrow.Close()
	broad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { broadHits++ }))
	defer broad.Close()

	front := newTestServer(t, Config{
		Name: "test",
		Routes: []Route{
			{Prefix: "/nested/deeper/", Upstream: narrow.URL},
			{Prefix: "/", Upstream: broad.URL},
		},
	})
	resp, err := noRedirectClient().Get(front.URL + "/nested/deeper/thing")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if narrowHits != 1 || broadHits != 0 {
		t.Errorf("narrow=%d broad=%d; want the first matching route to win", narrowHits, broadHits)
	}
}

// TestRoutes_ExactDoesNotMatchLongerPaths keeps an Exact route (the
// synthetic-probe shape) from swallowing real paths that merely share its
// prefix.
func TestRoutes_ExactDoesNotMatchLongerPaths(t *testing.T) {
	var upstreamPaths []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPaths = append(upstreamPaths, r.URL.Path)
	}))
	defer origin.Close()

	front := newTestServer(t, Config{
		Name: "test",
		Routes: []Route{
			{Prefix: "/supported", Exact: true, SyntheticStatus: 200},
			{Prefix: "/", Upstream: origin.URL},
		},
	})
	resp, err := noRedirectClient().Get(front.URL + "/supported-but-longer")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if len(upstreamPaths) != 1 || upstreamPaths[0] != "/supported-but-longer" {
		t.Errorf("upstream saw %v; the longer path must fall past the Exact route", upstreamPaths)
	}
}

// TestRelay_PreservesEscapedPath guards the encoding relayed protocols
// depend on: Go module paths case-encode uppercase letters as "!x", and
// versions carry "@" and "+incompatible". Re-encoding any of it turns a
// valid fetch into a spurious "not found", so the escaped path must arrive
// byte-identical.
func TestRelay_PreservesEscapedPath(t *testing.T) {
	const want = "/github.com/!aidan!allchin/bifrost/core/@v/v1.7.4+incompatible.info"
	var got string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.EscapedPath()
	}))
	defer origin.Close()

	front := newTestServer(t, goTestConfig(origin.URL, GoSumDB))
	resp, err := noRedirectClient().Get(front.URL + want)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	if got != want {
		t.Errorf("upstream escaped path = %q, want %q", got, want)
	}
}

// TestRelay_PassesThroughNotFound keeps 404/410 verbatim. Clients read
// those to conclude an artifact is absent (cmd/go additionally advances
// its GOPROXY list on them); rewriting them to 502 would turn "not found"
// into "relay broken".
func TestRelay_PassesThroughNotFound(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusGone} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		front := newTestServer(t, goTestConfig(origin.URL, GoSumDB))
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

	front := newTestServer(t, goTestConfig(origin.URL, GoSumDB))
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

	front := newTestServer(t, goTestConfig(origin.URL, GoSumDB))
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

func TestNew_RejectsMalformedConfig(t *testing.T) {
	valid := Route{Prefix: "/", Upstream: "https://example.com"}
	for name, cfg := range map[string]Config{
		"no name":             {Routes: []Route{valid}},
		"no routes":           {Name: "t"},
		"prefix no slash":     {Name: "t", Routes: []Route{{Prefix: "x", Upstream: "https://example.com"}}},
		"upstream with path":  {Name: "t", Routes: []Route{{Prefix: "/", Upstream: "https://example.com/base"}}},
		"upstream bad scheme": {Name: "t", Routes: []Route{{Prefix: "/", Upstream: "ftp://example.com"}}},
		"upstream no host":    {Name: "t", Routes: []Route{{Prefix: "/", Upstream: "https://"}}},
		"upstream no scheme":  {Name: "t", Routes: []Route{{Prefix: "/", Upstream: "example.com"}}},
		// url.Parse puts everything after the last "@" into Host, so an
		// upstream that reads as one host would dial another.
		"upstream with userinfo": {Name: "t", Routes: []Route{{Prefix: "/", Upstream: "https://proxy.golang.org@evil.com"}}},
		// WriteHeader panics outside 100-999, and the panic would be
		// per-request rather than at construction.
		"synthetic status out of range": {Name: "t", Routes: []Route{{Prefix: "/s", Exact: true, SyntheticStatus: 20}}},
		"synthetic and upstream": {Name: "t", Routes: []Route{
			{Prefix: "/", Upstream: "https://example.com", SyntheticStatus: 200},
		}},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%s) = nil error; want a loud construction failure", name)
		}
	}
}

// TestStart_RefusesNonLoopbackWithoutOptIn keeps an unauthenticated relay
// off the LAN by accident; the veth bind opts in explicitly.
func TestStart_RefusesNonLoopbackWithoutOptIn(t *testing.T) {
	s, err := New(GoModules())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Start("0.0.0.0:0"); err == nil {
		t.Error("Start(0.0.0.0:0) succeeded without AllowNonLoopback")
	}
}

func TestShutdown_SafeBeforeStart(t *testing.T) {
	s, err := New(GoModules())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown before Start = %v, want nil", err)
	}
}
