package gitproxy_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
)

// fakeUpstreamRecord captures what the fake upstream observed so tests
// can assert the proxy rewrote things correctly.
type fakeUpstreamRecord struct {
	mu         sync.Mutex
	method     string
	path       string
	authHeader string
	host       string
	body       string
	hits       atomic.Int64
}

func (r *fakeUpstreamRecord) record(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.method = req.Method
	r.path = req.URL.Path
	r.authHeader = req.Header.Get("Authorization")
	r.host = req.Host
	r.body = string(body)
}

func (r *fakeUpstreamRecord) snapshot() (method, path, auth, host, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.method, r.path, r.authHeader, r.host, r.body
}

// fakeGitHub stands in for github.com — records what arrived and
// returns a small canned response shaped like git-upload-pack output.
func fakeGitHub(rec *fakeUpstreamRecord) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits.Add(1)
		rec.record(r)
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("001e# service=git-upload-pack\n0000"))
	}))
}

// constantTokenSource returns a fixed token; its mints counter lets
// tests assert caching behavior independently of the proxy's own
// MintCount accessor.
type constantTokenSource struct {
	value     string
	expiresAt time.Time
	mints     atomic.Int64
	err       error
}

func (c *constantTokenSource) source(ctx context.Context, owner, repo string) (gitproxy.Token, error) {
	c.mints.Add(1)
	if c.err != nil {
		return gitproxy.Token{}, c.err
	}
	return gitproxy.Token{Value: c.value, ExpiresAt: c.expiresAt}, nil
}

// startProxy boots a gitproxy.Server pointed at upstream and returns
// the Server and its "http://127.0.0.1:PORT" URL. Caller-supplied
// TokenSource (so tests can pin behavior); Cleanup registers shutdown.
func startProxy(t *testing.T, ts gitproxy.TokenSource, upstream string) (*gitproxy.Server, string) {
	t.Helper()
	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource: ts,
		Upstream:    upstream,
	})
	if err != nil {
		t.Fatalf("gitproxy.New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Server.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv, "http://" + addr
}

// TestProxyInjectsBasicAuthWithXAccessToken is the load-bearing
// assertion: a request arriving at the proxy with NO Authorization
// header should leave the proxy with one matching the documented
// GitHub App installation-token shape.
//
//	Authorization: Basic base64("x-access-token:" + <token>)
//
// This is the credential-injection step that makes the agent-side env
// clean.
func TestProxyInjectsBasicAuthWithXAccessToken(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	const installationToken = "ghs_REALINSTALLATIONTOKEN1234"
	ts := &constantTokenSource{
		value:     installationToken,
		expiresAt: time.Now().Add(time.Hour),
	}
	_, proxyURL := startProxy(t, ts.source, upstream.URL)

	req, _ := http.NewRequest("GET", proxyURL+"/owner/repo.git/info/refs?service=git-upload-pack", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()

	if rec.hits.Load() != 1 {
		t.Fatalf("upstream hit count = %d, want 1", rec.hits.Load())
	}
	_, path, auth, _, _ := rec.snapshot()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+installationToken))
	if auth != want {
		t.Errorf("upstream Authorization = %q, want %q", auth, want)
	}
	if path != "/owner/repo.git/info/refs" {
		t.Errorf("upstream path = %q, want git protocol path preserved", path)
	}
}

// TestProxyOverwritesCallerSuppliedAuth pins that a request arriving
// with an existing Authorization header gets it overwritten, not
// duplicated or appended. Important because the agent's git might
// have stashed a stale credential we don't want to forward.
func TestProxyOverwritesCallerSuppliedAuth(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	ts := &constantTokenSource{
		value:     "ghs_REAL",
		expiresAt: time.Now().Add(time.Hour),
	}
	_, proxyURL := startProxy(t, ts.source, upstream.URL)

	req, _ := http.NewRequest("GET", proxyURL+"/owner/repo.git/info/refs", nil)
	// Bogus PAT-style header the caller "supplied" — proxy must
	// replace, not stack.
	req.Header.Set("Authorization", "Basic Z2hwX0NBTExFUl9TVVBQTElFRDpY")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()

	_, _, auth, _, _ := rec.snapshot()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_REAL"))
	if auth != want {
		t.Errorf("upstream Authorization = %q, want %q (proxy must overwrite caller-supplied auth)", auth, want)
	}
	// Defensive: assert there's exactly one header (no duplicate from
	// Add vs Set).
	if strings.Contains(auth, ",") {
		t.Errorf("upstream Authorization contains comma — appears to have multiple values: %q", auth)
	}
}

// TestProxyCachesToken pins that successive requests reuse a single
// minted token rather than minting one per request. This is what
// makes the 1-mint-per-run claim load-bearing for the run wall-clock.
func TestProxyCachesToken(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	ts := &constantTokenSource{
		value:     "ghs_CACHED",
		expiresAt: time.Now().Add(time.Hour),
	}
	srv, proxyURL := startProxy(t, ts.source, upstream.URL)

	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("GET", proxyURL+"/o/x.git/info/refs", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}
	if got := ts.mints.Load(); got != 1 {
		t.Errorf("TokenSource invocations = %d, want 1 (cache should reuse the token across calls)", got)
	}
	if got := srv.MintCount(); got != 1 {
		t.Errorf("Server.MintCount = %d, want 1", got)
	}
	if got := srv.RequestCount(); got != 5 {
		t.Errorf("RequestCount = %d, want 5", got)
	}
}

// TestProxyRefreshesNearExpiry pins that the cache rotates when the
// stored token ages within refreshThreshold of expiry. Without this,
// long-running runs would hit GitHub with expired tokens and see 401s
// near the 1-hour mark.
//
// Mechanism: pin the proxy's clock so the first request caches a
// fresh-when-cached 1-hour token, then advance the clock past the
// refresh threshold and observe the second request triggers a new
// mint. Avoids real sleeps and avoids the dead-code-path that would
// result from feeding the source itself a too-short-TTL token (which
// the proxy rejects at TokenSource time — see TestProxyRejectsExpiredTokenFromSource).
func TestProxyRefreshesNearExpiry(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	// Injected clock the test advances between requests. Protected
	// because the proxy reads it from request goroutines while the
	// test mutates it from the main goroutine.
	var (
		clockMu sync.Mutex
		fakeNow = time.Now()
	)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return fakeNow
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		defer clockMu.Unlock()
		fakeNow = fakeNow.Add(d)
	}

	var (
		callsMu sync.Mutex
		calls   int
	)
	source := func(ctx context.Context, owner, repo string) (gitproxy.Token, error) {
		callsMu.Lock()
		defer callsMu.Unlock()
		calls++
		return gitproxy.Token{
			Value:     fmt.Sprintf("ghs_TOKEN_%d", calls),
			ExpiresAt: clock().Add(time.Hour),
		}, nil
	}
	srv, proxyURL := startProxy(t, source, upstream.URL)
	srv.SetNowForTest(clock)

	// First request: cache empty → mint → cache (1h TTL from now).
	req1, _ := http.NewRequest("GET", proxyURL+"/o/x.git/info/refs", nil)
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	_ = resp1.Body.Close()
	_, _, firstAuth, _, _ := rec.snapshot()

	// Age the clock 56 minutes — cached token now has 4 minutes left,
	// inside the 5-minute refresh threshold.
	advance(56 * time.Minute)

	// Second request: cache check fails (within threshold) → re-mint.
	req2, _ := http.NewRequest("GET", proxyURL+"/o/x.git/info/refs", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	_ = resp2.Body.Close()
	_, _, secondAuth, _, _ := rec.snapshot()

	callsMu.Lock()
	gotCalls := calls
	callsMu.Unlock()
	if gotCalls != 2 {
		t.Errorf("TokenSource invocations = %d, want 2 (cache aging past threshold should re-mint)", gotCalls)
	}
	if firstAuth == "" {
		t.Errorf("first request did not reach upstream — proxy returned an error before forwarding")
	}
	if firstAuth == secondAuth {
		t.Errorf("first and second auth headers identical — refresh did not rotate the token")
	}
	wantFirst := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_TOKEN_1"))
	wantSecond := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_TOKEN_2"))
	if firstAuth != wantFirst {
		t.Errorf("first auth = %q, want %q", firstAuth, wantFirst)
	}
	if secondAuth != wantSecond {
		t.Errorf("second auth = %q, want %q", secondAuth, wantSecond)
	}
}

// TestProxyRejectsExpiredTokenFromSource pins the defensive check on
// the return value of TokenSource: a token that's already past expiry,
// or with TTL shorter than refreshThreshold, is rejected with 502
// rather than cached. Catches a TokenSource implementation that
// silently hands back a stale credential — without the check, the
// proxy would forward the request, GitHub would 401, and the agent
// would see a confusing failure rather than fail-fast.
func TestProxyRejectsExpiredTokenFromSource(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	cases := []struct {
		name      string
		expiresIn time.Duration
	}{
		{"already_expired", -5 * time.Minute},
		{"expires_now", 0},
		{"within_refresh_threshold", 4 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := func(ctx context.Context, owner, repo string) (gitproxy.Token, error) {
				return gitproxy.Token{
					Value:     "ghs_STALE",
					ExpiresAt: time.Now().Add(c.expiresIn),
				}, nil
			}
			startCount := rec.hits.Load()
			_, proxyURL := startProxy(t, source, upstream.URL)
			req, _ := http.NewRequest("GET", proxyURL+"/o/x.git/info/refs", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("roundtrip: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("status = %d, want 502 (stale token must not be cached or forwarded)", resp.StatusCode)
			}
			if got := rec.hits.Load() - startCount; got != 0 {
				t.Errorf("upstream hits = %d, want 0 (stale token must not reach upstream)", got)
			}
		})
	}
}

// TestProxyTokenSourceFailureReturns502 pins that a mint failure
// surfaces as a 502 with no token disclosure rather than silently
// passing through an empty auth header. The agent then knows the run
// can't proceed; a 200 with empty auth would just look like a GitHub
// 401 to the caller.
func TestProxyTokenSourceFailureReturns502(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	ts := &constantTokenSource{err: errors.New("mint failed: bad app key")}
	_, proxyURL := startProxy(t, ts.source, upstream.URL)

	req, _ := http.NewRequest("GET", proxyURL+"/o/x.git/info/refs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	// The underlying mint error MUST NOT leak to the agent — error
	// detail may include App ID or other identifying info that the
	// agent (and therefore a prompt-injection attacker) shouldn't see.
	if strings.Contains(string(body), "bad app key") {
		t.Errorf("response body leaks mint error: %q", body)
	}
	if rec.hits.Load() != 0 {
		t.Errorf("upstream hit count = %d, want 0 (mint failure must not forward)", rec.hits.Load())
	}
}

// TestProxyRejectsCONNECT pins that CONNECT requests fail fast with
// 501. A git client configured with http.proxy=<this> AND an https://
// remote URL would issue CONNECT to tunnel TLS; once tunneled, the
// traffic is opaque end-to-end TLS and the proxy can't inject the
// installation token. Failing with a clear error surfaces the misconfig
// instead of producing a confusing connection drop or 502.
func TestProxyRejectsCONNECT(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	ts := &constantTokenSource{value: "ghs_x", expiresAt: time.Now().Add(time.Hour)}
	_, proxyURL := startProxy(t, ts.source, upstream.URL)

	// Drive a CONNECT directly via a raw TCP connection to the proxy.
	// Going through http.Client/DefaultTransport would route via the
	// process's HTTP_PROXY env var (which CI runners often set), so
	// the request would never reach our proxy. Raw TCP avoids all
	// that and exercises exactly what a git client would do.
	addr := strings.TrimPrefix(proxyURL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 Not Implemented for CONNECT", resp.StatusCode)
	}
	if !strings.Contains(string(body), "CONNECT not supported") {
		t.Errorf("body = %q; want explanation mentioning CONNECT", body)
	}
	// Token must not be touched on a rejected method — the mint cost is
	// preserved for legitimate requests.
	if rec.hits.Load() != 0 {
		t.Errorf("upstream received %d hits on CONNECT; want 0", rec.hits.Load())
	}
}

// TestProxyStripsProxyAuthorization pins that any Proxy-Authorization
// header on an inbound request is dropped before forwarding to GitHub.
// If the agent's git is misconfigured with proxy credentials (or some
// inherited config), the header would otherwise leak those credentials
// upstream. httputil.ReverseProxy strips hop-by-hop headers by default,
// but we do it explicitly as defense in depth.
func TestProxyStripsProxyAuthorization(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	gotHeaders := http.Header{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits.Add(1)
		rec.record(r)
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	ts := &constantTokenSource{value: "ghs_x", expiresAt: time.Now().Add(time.Hour)}
	_, proxyURL := startProxy(t, ts.source, upstream.URL)

	req, _ := http.NewRequest("GET", proxyURL+"/o/x.git/info/refs", nil)
	req.Header.Set("Proxy-Authorization", "Basic Y2FsbGVyOnNlY3JldA==")
	req.Header.Set("Proxy-Connection", "keep-alive")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()

	for _, h := range []string{"Proxy-Authorization", "Proxy-Connection"} {
		if v := gotHeaders.Get(h); v != "" {
			t.Errorf("upstream saw %s = %q; want stripped (credential leak risk)", h, v)
		}
	}
}

// TestProxyForwardsHostHeader confirms the Host header sent to the
// upstream matches the upstream's hostname, not the proxy's. GitHub's
// edge has been observed rejecting requests where Host doesn't match
// the SNI / certificate.
func TestProxyForwardsHostHeader(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	ts := &constantTokenSource{value: "ghs_x", expiresAt: time.Now().Add(time.Hour)}
	_, proxyURL := startProxy(t, ts.source, upstream.URL)

	req, _ := http.NewRequest("GET", proxyURL+"/o/x.git/info/refs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()

	_, _, _, host, _ := rec.snapshot()
	wantHost := strings.TrimPrefix(upstream.URL, "http://")
	if host != wantHost {
		t.Errorf("upstream Host = %q, want %q (proxy must rewrite Host to upstream value)", host, wantHost)
	}
}

// TestProxyForwardsPostBody pins that POST bodies (used for git push
// over smart HTTP, payload type application/x-git-receive-pack-request)
// pass through unchanged. Mutating the body would break the pack-file
// upload silently.
func TestProxyForwardsPostBody(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	ts := &constantTokenSource{value: "ghs_x", expiresAt: time.Now().Add(time.Hour)}
	_, proxyURL := startProxy(t, ts.source, upstream.URL)

	const payload = "PACK\x00\x00\x00\x02\x00\x00\x00\x01..."
	req, _ := http.NewRequest("POST", proxyURL+"/owner/repo.git/git-receive-pack",
		strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()

	method, _, _, _, body := rec.snapshot()
	if method != "POST" {
		t.Errorf("upstream method = %q, want POST", method)
	}
	if body != payload {
		t.Errorf("upstream body = %q, want unchanged payload", body)
	}
}

// TestProxyDropsForwardedHeaders pins that X-Forwarded-* headers from
// any caller are stripped, not passed upstream. GitHub doesn't use
// them and they'd just be noise / a footgun for any future header-
// based filtering on the upstream.
func TestProxyDropsForwardedHeaders(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	gotHeaders := http.Header{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits.Add(1)
		rec.record(r)
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	ts := &constantTokenSource{value: "ghs_x", expiresAt: time.Now().Add(time.Hour)}
	_, proxyURL := startProxy(t, ts.source, upstream.URL)

	req, _ := http.NewRequest("GET", proxyURL+"/o/x.git/info/refs", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Forwarded", "for=1.2.3.4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()

	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
		if v := gotHeaders.Get(h); v != "" {
			t.Errorf("upstream saw %s = %q; want stripped", h, v)
		}
	}
}

// TestProxyRejectsInvalidConfig pins construction-time validation.
func TestProxyRejectsInvalidConfig(t *testing.T) {
	ts := func(ctx context.Context, owner, repo string) (gitproxy.Token, error) {
		return gitproxy.Token{}, nil
	}
	cases := []struct {
		name string
		cfg  gitproxy.Config
	}{
		{"nil_token_source", gitproxy.Config{TokenSource: nil, Upstream: "https://github.com"}},
		{"upstream_no_scheme", gitproxy.Config{TokenSource: ts, Upstream: "github.com"}},
		{"upstream_with_path", gitproxy.Config{TokenSource: ts, Upstream: "https://github.com/owner"}},
		{"upstream_with_query", gitproxy.Config{TokenSource: ts, Upstream: "https://github.com?x=1"}},
		{"upstream_with_fragment", gitproxy.Config{TokenSource: ts, Upstream: "https://github.com#frag"}},
		{"upstream_http_non_loopback", gitproxy.Config{TokenSource: ts, Upstream: "http://github.com"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := gitproxy.New(c.cfg); err == nil {
				t.Errorf("New(%+v) = nil err; want validation error", c.cfg)
			}
		})
	}
}

// TestProxyUpstreamDefault pins that an empty Upstream defaults to
// https://github.com so callers using stock GitHub can omit the field.
func TestProxyUpstreamDefault(t *testing.T) {
	ts := func(ctx context.Context, owner, repo string) (gitproxy.Token, error) {
		return gitproxy.Token{Value: "x", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	_, err := gitproxy.New(gitproxy.Config{TokenSource: ts})
	if err != nil {
		t.Errorf("New with empty Upstream: %v; want default to apply", err)
	}
}

// TestProxyUpstreamTrailingSlashAccepted mirrors the LLM-proxy behavior:
// "https://github.com/" is semantically equivalent to "https://github.com"
// and accepted.
func TestProxyUpstreamTrailingSlashAccepted(t *testing.T) {
	ts := func(ctx context.Context, owner, repo string) (gitproxy.Token, error) {
		return gitproxy.Token{Value: "x", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	if _, err := gitproxy.New(gitproxy.Config{TokenSource: ts, Upstream: "https://github.com/"}); err != nil {
		t.Errorf("trailing slash should be accepted: %v", err)
	}
}

// TestProxyUpstreamLoopbackHTTPVariants pins that loopback http
// upstreams parse correctly for both IPv4 and IPv6 forms, with and
// without an explicit port. The IPv6-no-port case is the regression
// guard: bare "[::1]" leaves u.Host bracketed, and a naive
// SplitHostPort + ParseIP would reject it.
func TestProxyUpstreamLoopbackHTTPVariants(t *testing.T) {
	ts := func(ctx context.Context, owner, repo string) (gitproxy.Token, error) {
		return gitproxy.Token{Value: "x", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	cases := []string{
		"http://127.0.0.1",
		"http://127.0.0.1:8080",
		"http://[::1]",
		"http://[::1]:8080",
	}
	for _, upstream := range cases {
		t.Run(upstream, func(t *testing.T) {
			if _, err := gitproxy.New(gitproxy.Config{TokenSource: ts, Upstream: upstream}); err != nil {
				t.Errorf("loopback http upstream %q should be accepted: %v", upstream, err)
			}
		})
	}
}

// TestProxyStartRejectsDoubleStart pins that a second Start fails
// rather than silently leaking the first listener.
func TestProxyStartRejectsDoubleStart(t *testing.T) {
	ts := &constantTokenSource{value: "x", expiresAt: time.Now().Add(time.Hour)}
	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource: ts.source,
		Upstream:    "https://github.com",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := srv.Start(""); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	if _, err := srv.Start(""); err == nil {
		t.Error("second Start returned nil err; want already-started error")
	}
}

// TestProxyStartRejectsNonLoopback pins the safety default: a non-
// loopback bind is rejected unless AllowNonLoopback=true.
func TestProxyStartRejectsNonLoopback(t *testing.T) {
	ts := &constantTokenSource{value: "x", expiresAt: time.Now().Add(time.Hour)}
	cases := []struct {
		name string
		addr string
	}{
		{"all_interfaces", "0.0.0.0:0"},
		{"empty_host", ":0"},
		{"ipv6_all_interfaces", "[::]:0"},
		{"test_net_address", "192.0.2.1:0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, err := gitproxy.New(gitproxy.Config{
				TokenSource: ts.source,
				Upstream:    "https://github.com",
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := srv.Start(c.addr); err == nil {
				t.Errorf("Start(%q) = nil err; want non-loopback rejection", c.addr)
			}
		})
	}
}

// TestProxyStartAllowsNonLoopbackOptIn pins that AllowNonLoopback=true
// lets a non-loopback bind through — the legitimate sandbox-veth case.
func TestProxyStartAllowsNonLoopbackOptIn(t *testing.T) {
	ts := &constantTokenSource{value: "x", expiresAt: time.Now().Add(time.Hour)}
	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource:      ts.source,
		Upstream:         "https://github.com",
		AllowNonLoopback: true,
	})
	if err != nil {
		t.Fatalf("New (opt-in): %v", err)
	}
	addr, err := srv.Start("0.0.0.0:0")
	if err != nil {
		t.Fatalf("Start with AllowNonLoopback=true: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	if addr == "" {
		t.Error("Start returned empty addr")
	}
}

// TestProxyErrChannelCleanOnShutdown pins that the Err() channel closes
// without sending an error on a normal shutdown — the healthy-run path.
func TestProxyErrChannelCleanOnShutdown(t *testing.T) {
	ts := &constantTokenSource{value: "x", expiresAt: time.Now().Add(time.Hour)}
	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource: ts.source,
		Upstream:    "https://github.com",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := srv.Start(""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)

	select {
	case err, ok := <-srv.Err():
		if ok {
			t.Errorf("Err() = %v after clean shutdown; want channel closed with no error", err)
		}
	case <-time.After(time.Second):
		t.Error("Err() channel not closed within 1s after Shutdown")
	}
}

// TestProxyShutdownIdempotent pins that calling Shutdown on a Server
// that was never Start'd (or already shut down) doesn't panic. Useful
// for caller error paths that defer Shutdown immediately after New.
func TestProxyShutdownIdempotent(t *testing.T) {
	ts := &constantTokenSource{value: "x", expiresAt: time.Now().Add(time.Hour)}
	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource: ts.source,
		Upstream:    "https://github.com",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown without Start: %v", err)
	}
}

// TestPropertyB_TokenNotObservableInChildProcess is the architectural
// acceptance check from the ticket: a child process running git with
// http.proxy=<our proxy> must not observe the installation token in
// any of its own observable surfaces (env, ~/.gitconfig).
//
// The token lives only in the proxy's memory; the agent sees a proxy
// URL and nothing else.
//
// The proxy's Upstream is pinned to a local httptest server so the
// test runs entirely on loopback — no DNS, no outbound network, no
// flake on offline CI runners. Git still exercises the full proxy
// roundtrip (forward-proxy mode with absolute URI in the request line)
// and the test still inspects exactly what the ticket asks for.
//
// Skips if git isn't installed (CI runners without git would only
// produce a noisy false-pass otherwise; the test is about the env
// shape, not git's behavior).
func TestPropertyB_TokenNotObservableInChildProcess(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not installed; skipping Property B check: %v", err)
	}

	// Fake upstream stands in for github.com. Returns a smart-HTTP
	// service advertisement so git's ls-remote progresses past
	// connection setup and the proxy roundtrip is fully exercised.
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	// Marker token: a distinctive string that, if it appears in any
	// child-observable surface, definitively proves a leak. The
	// "_LEAK_CANARY_" substring is what tests grep for.
	const markerToken = "ghs_PROPERTY_B_LEAK_CANARY_DO_NOT_LEAK_xyz"
	ts := &constantTokenSource{
		value:     markerToken,
		expiresAt: time.Now().Add(time.Hour),
	}
	_, proxyURL := startProxy(t, ts.source, upstream.URL)

	// Set up a synthetic HOME for the child so the test reads back a
	// .gitconfig under our control rather than the developer's real
	// one (which may legitimately contain credentials and would cause
	// false-positive failures).
	tmpHome := t.TempDir()
	gitconfigPath := filepath.Join(tmpHome, ".gitconfig")
	gitconfigBody := fmt.Sprintf("[http]\n\tproxy = %s\n", proxyURL)
	if err := os.WriteFile(gitconfigPath, []byte(gitconfigBody), 0600); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}

	// Minimal env: no inheritance from the test process. PATH and HOME
	// are the only env vars the child needs.
	childEnv := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + tmpHome,
	}

	// Sanity: the env we constructed contains no token. Catches a bug
	// where the test setup itself accidentally pastes the token in.
	for _, e := range childEnv {
		if strings.Contains(e, "LEAK_CANARY") {
			t.Fatalf("child env contains marker token before we even spawn — test setup bug: %q", e)
		}
	}

	// Run git against an http:// URL so it uses HTTP forward-proxy
	// mode (not CONNECT-tunneled HTTPS, which a reverse-proxy listener
	// can't terminate without a custom CA). The URL points at the
	// upstream's actual host: in forward-proxy mode our proxy rewrites
	// the destination anyway, but pointing at the local httptest host
	// avoids any libcurl version quirk that might pre-validate the
	// target hostname before sending the absolute URI to the proxy.
	// The proxy is what we're testing the leak guarantees about, not
	// the URL hostname.
	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitBin, "ls-remote", "http://"+upstreamHost+"/owner/repo.git")
	cmd.Env = childEnv
	cmd.Dir = tmpHome
	// Capture combined output so we can also grep for the token in
	// whatever git printed.
	out, _ := cmd.CombinedOutput()

	// 1. The .gitconfig we wrote must contain the proxy URL but not
	//    the token. (Sanity-check the file ground truth.)
	gitconfigContent, err := os.ReadFile(gitconfigPath)
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	if !strings.Contains(string(gitconfigContent), proxyURL) {
		t.Errorf(".gitconfig missing proxy URL — test setup broken: %s", gitconfigContent)
	}
	if strings.Contains(string(gitconfigContent), "LEAK_CANARY") {
		t.Errorf(".gitconfig contains the installation token — Property B violated: %s", gitconfigContent)
	}

	// 2. The env we passed to the child contains no token.
	for _, e := range childEnv {
		if strings.Contains(e, "LEAK_CANARY") {
			t.Errorf("child env contains the installation token — Property B violated: %q", e)
		}
	}

	// 3. The child's stdout/stderr doesn't echo the token (an unlikely
	//    failure mode, but git could conceivably print the resolved
	//    proxy auth in verbose modes; we don't enable verbose, but
	//    check anyway).
	if strings.Contains(string(out), "LEAK_CANARY") {
		t.Errorf("child output contains the installation token — Property B violated: %s", out)
	}

	// 4. Also check the bare token value itself, not just the marker
	//    substring, so a future refactor that changes the marker
	//    format still catches a real leak.
	if strings.Contains(string(gitconfigContent), markerToken) ||
		strings.Contains(string(out), markerToken) {
		t.Errorf("marker token observed in child surfaces; full output: %s", out)
	}

	// 5. Positive assertion: the proxy actually forwarded the request
	//    AND injected the credential upstream. Without this, a buggy
	//    proxy that 502'd before forwarding would trivially "pass" the
	//    leak checks above — there's nothing to leak if nothing
	//    happened. The marker must be visible on the upstream side
	//    (base64-decoded out of the Basic-auth header) while invisible
	//    on the child side.
	if rec.hits.Load() == 0 {
		t.Errorf("upstream received 0 requests — git didn't reach the proxy, so the leak assertions above are vacuous (out: %s)", out)
	}
	_, _, upstreamAuth, _, _ := rec.snapshot()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+markerToken))
	if upstreamAuth != want {
		t.Errorf("upstream Authorization = %q, want %q — proxy is not actually injecting credentials, so the no-leak result is meaningless",
			upstreamAuth, want)
	}
}

// TestProxyConcurrentRequestsCoalesceMint pins that N concurrent
// requests against a cold cache produce exactly one mint call, not N.
// Without serialization, a run kicking off parallel git operations on
// startup would hammer the GitHub mint endpoint and could trip rate
// limits.
func TestProxyConcurrentRequestsCoalesceMint(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	ts := &constantTokenSource{value: "ghs_x", expiresAt: time.Now().Add(time.Hour)}
	_, proxyURL := startProxy(t, ts.source, upstream.URL)

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", proxyURL+"/o/x.git/info/refs", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("concurrent call: %v", err)
				return
			}
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()

	if got := ts.mints.Load(); got != 1 {
		t.Errorf("TokenSource invocations = %d, want 1 (concurrent requests should coalesce)", got)
	}
	if got := rec.hits.Load(); int(got) != N {
		t.Errorf("upstream hits = %d, want %d", got, N)
	}
}
