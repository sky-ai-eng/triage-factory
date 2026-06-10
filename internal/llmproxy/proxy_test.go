package llmproxy_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/llmproxy"
)

// upstreamRecord captures what the fake upstream observed so tests
// can assert the proxy rewrote things correctly.
type upstreamRecord struct {
	method     string
	path       string
	xAPIKey    string
	authHeader string
	host       string
	body       string
	hits       atomic.Int64
}

// fakeUpstream stands in for api.anthropic.com or
// bedrock-runtime.<region>.amazonaws.com. Records what arrived,
// returns a canned response (optionally streaming for the SSE test).
func fakeUpstream(rec *upstreamRecord, stream bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits.Add(1)
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.xAPIKey = r.Header.Get("x-api-key")
		rec.authHeader = r.Header.Get("Authorization")
		rec.host = r.Host
		body, _ := io.ReadAll(r.Body)
		rec.body = string(body)

		if stream {
			// Mimic SSE — ReverseProxy needs to handle chunked writes
			// without buffering or the live SDK case will deadlock on
			// long-running tool-use streams.
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, "event: chunk\ndata: {\"i\":%d}\n\n", i)
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(10 * time.Millisecond)
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`))
	}))
}

// startProxyWithAddr boots a llmproxy.Server pointed at upstream and
// returns both the Server and its "http://127.0.0.1:PORT" URL ready
// to drop into a request. Caller is responsible for Shutdown (via
// t.Cleanup registered here).
func startProxyWithAddr(t *testing.T, provider llmproxy.Provider, apiKey, upstream string) (*llmproxy.Server, string) {
	t.Helper()
	srv, err := llmproxy.New(llmproxy.Config{
		Provider: provider,
		APIKey:   apiKey,
		Upstream: upstream,
	})
	if err != nil {
		t.Fatalf("llmproxy.New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Server.Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	return srv, "http://" + addr
}

// TestProxyInjectsAnthropicAPIKey is the load-bearing assertion of
// Phase 1's Anthropic-direct path: a request arriving at the proxy
// with NO x-api-key header should leave the proxy with x-api-key set
// to the real key from the config. This is the credential-injection
// step that makes the agent-side env clean.
func TestProxyInjectsAnthropicAPIKey(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec, false)
	defer upstream.Close()

	_, proxyURL := startProxyWithAddr(t, llmproxy.ProviderAnthropic, "sk-ant-REAL-KEY", upstream.URL)

	body := strings.NewReader(`{"model":"claude-haiku","messages":[]}`)
	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", body)
	// Caller-supplied x-api-key is deliberately wrong — proxy must
	// overwrite, not duplicate or append.
	req.Header.Set("x-api-key", "caller-supplied-garbage")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy roundtrip: %v", err)
	}
	defer resp.Body.Close()

	if rec.hits.Load() != 1 {
		t.Fatalf("upstream hit count = %d, want 1", rec.hits.Load())
	}
	if rec.xAPIKey != "sk-ant-REAL-KEY" {
		t.Errorf("upstream x-api-key = %q, want sk-ant-REAL-KEY (proxy must overwrite the caller's value)", rec.xAPIKey)
	}
	if rec.path != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages (proxy must preserve the request path)", rec.path)
	}
	if rec.body != `{"model":"claude-haiku","messages":[]}` {
		t.Errorf("upstream body = %q; want passthrough unchanged", rec.body)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestProxyInjectsBedrockBearer is the Bedrock-bearer-path mirror of
// the Anthropic test. The header name is different (Authorization)
// and the value shape is "Bearer <key>" but the rewrite logic is
// identical.
func TestProxyInjectsBedrockBearer(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec, false)
	defer upstream.Close()

	_, proxyURL := startProxyWithAddr(t, llmproxy.ProviderBedrockBearer, "bdrk-REAL-BEARER", upstream.URL)

	req, _ := http.NewRequest("POST", proxyURL+"/model/anthropic.claude-haiku-4-5/invoke", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer caller-supplied-garbage")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy roundtrip: %v", err)
	}
	defer resp.Body.Close()

	if rec.authHeader != "Bearer bdrk-REAL-BEARER" {
		t.Errorf("upstream Authorization = %q, want \"Bearer bdrk-REAL-BEARER\"", rec.authHeader)
	}
	if !strings.HasPrefix(rec.path, "/model/") {
		t.Errorf("upstream path = %q, want Bedrock invoke path preserved", rec.path)
	}
}

// TestProxyStreamingResponsePassesThrough pins SSE / chunked-response
// behavior. The agent SDK uses streaming for every model call; if
// the proxy buffers responses, the agent's stream-json parser sees
// the whole batch at once and behavior diverges from a direct
// connection.
//
// The test actually pins streaming (rather than just final body
// content) by timing the first byte read: upstream writes one chunk
// and stays alive for ~600ms before the second; the client's first
// Read must return well before that gap closes. A buffering proxy
// would block the read for the full 600ms.
func TestProxyStreamingResponsePassesThrough(t *testing.T) {
	// Upstream that flushes chunk 1, sleeps, flushes chunk 2. The
	// long inter-chunk gap is what gives the test its discrimination
	// — a buffering proxy can't get under it.
	const interChunkSleep = 600 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Consume the request body before responding, like a real LLM
		// upstream (it can't stream a completion without reading the
		// prompt). Responding to headers alone lets the response race
		// the proxy transport's request-body write: the proxy server's
		// first flush closes the shared body and the transport's
		// writeLoop can hit ErrBodyReadAfterClose, killing the upstream
		// conn mid-stream. eofLatchedBody (proxy.go) shields the
		// post-EOF drain read; the ordering where the response outruns
		// the body *data* read is reachable only when the upstream
		// skips the body entirely, which no LLM API does.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)

		_, _ = w.Write([]byte("data: {\"chunk\":0}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(interChunkSleep)
		_, _ = w.Write([]byte("data: {\"chunk\":1}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	_, proxyURL := startProxyWithAddr(t, llmproxy.ProviderAnthropic, "sk-ant-stream", upstream.URL)

	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy roundtrip: %v", err)
	}
	defer resp.Body.Close()

	// Discrimination criterion: the first chunk must arrive well
	// before the upstream's inter-chunk sleep would have elapsed if
	// the proxy were buffering. 100ms is comfortably less than the
	// 600ms gap; if first read takes longer, the proxy buffered.
	start := time.Now()
	buf := make([]byte, 256)
	n, err := resp.Body.Read(buf)
	elapsed := time.Since(start)
	if err != nil && err != io.EOF {
		t.Fatalf("first read: %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("first chunk took %v to arrive (upstream sleeps %v between chunks); proxy is buffering",
			elapsed, interChunkSleep)
	}
	if n == 0 || !strings.Contains(string(buf[:n]), `"chunk":0`) {
		t.Errorf("first read got %q, expected chunk 0", string(buf[:n]))
	}

	// Drain the rest and check chunk 1 is also present.
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), `"chunk":1`) {
		t.Errorf("second chunk missing from drained body: %q", string(rest))
	}
}

// TestProxyRequestCount tracks ReverseProxy.ModifyResponse firing
// per upstream response. Useful as the test-time signal that an
// agent actually went through the proxy (vs. bypassing it via some
// fallback path).
func TestProxyRequestCount(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec, false)
	defer upstream.Close()

	srv, proxyURL := startProxyWithAddr(t, llmproxy.ProviderAnthropic, "sk", upstream.URL)

	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}
	if got := srv.RequestCount(); got != 5 {
		t.Errorf("RequestCount = %d, want 5", got)
	}
}

// TestProxyRejectsInvalidConfig pins the construction-time validation
// so callers fail loudly at boot rather than producing a Server that
// silently doesn't work.
func TestProxyRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  llmproxy.Config
	}{
		{"unknown_provider", llmproxy.Config{Provider: "vertex", APIKey: "k", Upstream: "https://x"}},
		{"empty_apikey", llmproxy.Config{Provider: llmproxy.ProviderAnthropic, APIKey: "", Upstream: "https://x"}},
		{"empty_upstream", llmproxy.Config{Provider: llmproxy.ProviderAnthropic, APIKey: "k", Upstream: ""}},
		{"upstream_no_scheme", llmproxy.Config{Provider: llmproxy.ProviderAnthropic, APIKey: "k", Upstream: "api.anthropic.com"}},
		{"upstream_with_path", llmproxy.Config{Provider: llmproxy.ProviderAnthropic, APIKey: "k", Upstream: "https://api.anthropic.com/v1"}},
		{"upstream_with_query", llmproxy.Config{Provider: llmproxy.ProviderAnthropic, APIKey: "k", Upstream: "https://api.anthropic.com?x=1"}},
		{"upstream_with_fragment", llmproxy.Config{Provider: llmproxy.ProviderAnthropic, APIKey: "k", Upstream: "https://api.anthropic.com#frag"}},
		{"upstream_http_non_loopback", llmproxy.Config{Provider: llmproxy.ProviderAnthropic, APIKey: "k", Upstream: "http://api.anthropic.com"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := llmproxy.New(c.cfg); err == nil {
				t.Errorf("New(%+v) = nil err; want validation error", c.cfg)
			}
		})
	}
}

// TestProxyUpstreamWithTrailingSlash pins that "https://api.anthropic.com/"
// (path == "/") is accepted as semantically equivalent to no path —
// users commonly write URLs with a trailing slash and refusing it
// would be needlessly strict.
func TestProxyUpstreamWithTrailingSlash(t *testing.T) {
	if _, err := llmproxy.New(llmproxy.Config{
		Provider: llmproxy.ProviderAnthropic,
		APIKey:   "k",
		Upstream: "https://api.anthropic.com/",
	}); err != nil {
		t.Errorf("trailing slash should be accepted: %v", err)
	}
}

// TestProxyUpstreamLoopbackHTTPAllowed confirms that http:// upstreams
// pointing at a loopback address are accepted — this is the httptest
// pattern used by all unit tests. Non-loopback http must be rejected
// (see upstream_http_non_loopback case in TestProxyRejectsInvalidConfig).
func TestProxyUpstreamLoopbackHTTPAllowed(t *testing.T) {
	cases := []string{
		"http://127.0.0.1:8080",
		"http://127.0.0.1",
	}
	for _, upstream := range cases {
		t.Run(upstream, func(t *testing.T) {
			if _, err := llmproxy.New(llmproxy.Config{
				Provider: llmproxy.ProviderAnthropic,
				APIKey:   "k",
				Upstream: upstream,
			}); err != nil {
				t.Errorf("loopback http upstream should be accepted: %v", err)
			}
		})
	}
}

// TestProxyStartRejectsDoubleStart pins that calling Start twice on the
// same Server returns an error rather than silently leaking the first
// listener. Server is documented as per-run; a double-Start is a caller
// bug that should fail loudly.
func TestProxyStartRejectsDoubleStart(t *testing.T) {
	srv, err := llmproxy.New(llmproxy.Config{
		Provider: llmproxy.ProviderAnthropic,
		APIKey:   "k",
		Upstream: "https://api.anthropic.com",
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
		t.Error("second Start returned nil error; want already-started error")
	}
}

// TestProxyStartRejectsNonLoopback pins the safety default: binding
// to anything other than loopback returns an error unless the caller
// has explicitly set AllowNonLoopback. Without this, a caller doing
// `Start("0.0.0.0:NNNN")` accidentally would expose a credentialed
// proxy to the LAN.
func TestProxyStartRejectsNonLoopback(t *testing.T) {
	cases := []struct {
		name string
		addr string
	}{
		{"all_interfaces", "0.0.0.0:0"},
		{"empty_host", ":0"},
		{"ipv6_all_interfaces", "[::]:0"},
		// 192.0.2.0/24 is TEST-NET-1 — guaranteed non-routable, so
		// the test doesn't accidentally hit a real interface. We
		// only need the loopback check to reject it, not bind.
		{"test_net_address", "192.0.2.1:0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, err := llmproxy.New(llmproxy.Config{
				Provider: llmproxy.ProviderAnthropic,
				APIKey:   "k",
				Upstream: "https://api.anthropic.com",
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

// TestProxyStartAllowsNonLoopbackOptIn pins the opt-in path: when
// AllowNonLoopback=true, assertLoopback is skipped and Start accepts
// non-loopback addresses. We bind to 0.0.0.0:0 (all-interfaces) —
// assertLoopback rejects that address without the flag, so a successful
// Start with the flag proves the bypass is working. The test also
// verifies the inverse: without AllowNonLoopback, the same address
// is rejected before net.Listen is called.
func TestProxyStartAllowsNonLoopbackOptIn(t *testing.T) {
	// Inverse check: 0.0.0.0 is rejected without the flag.
	srvStrict, err := llmproxy.New(llmproxy.Config{
		Provider: llmproxy.ProviderAnthropic,
		APIKey:   "k",
		Upstream: "https://api.anthropic.com",
	})
	if err != nil {
		t.Fatalf("New (strict): %v", err)
	}
	if _, err := srvStrict.Start("0.0.0.0:0"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srvStrict.Shutdown(ctx)
		t.Fatal("Start(\"0.0.0.0:0\") without AllowNonLoopback must be rejected")
	}

	// Opt-in check: same address accepted with the flag.
	srv, err := llmproxy.New(llmproxy.Config{
		Provider:         llmproxy.ProviderAnthropic,
		APIKey:           "k",
		Upstream:         "https://api.anthropic.com",
		AllowNonLoopback: true,
	})
	if err != nil {
		t.Fatalf("New (opt-in): %v", err)
	}
	addr, err := srv.Start("0.0.0.0:0")
	if err != nil {
		t.Fatalf("Start(\"0.0.0.0:0\") with AllowNonLoopback=true: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	if addr == "" {
		t.Error("Start returned empty address")
	}
}

// TestProxyLocalhostHostname pins that "localhost" resolves through
// the loopback check (it should, since localhost typically resolves
// to 127.0.0.1 / ::1). Important because many callers write
// "localhost:NNNN" out of habit and we don't want to reject it.
func TestProxyLocalhostHostname(t *testing.T) {
	srv, err := llmproxy.New(llmproxy.Config{
		Provider: llmproxy.ProviderAnthropic,
		APIKey:   "k",
		Upstream: "https://api.anthropic.com",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := srv.Start("localhost:0"); err != nil {
		t.Errorf("Start(\"localhost:0\") = %v; want accept (localhost resolves to loopback)", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
}

// TestProxyForwardsHostHeader confirms the Host header sent to the
// upstream matches the upstream's hostname, not the proxy's. Some
// HTTPS terminators / WAFs reject requests where Host doesn't match
// the SNI / certificate, so getting this wrong would cause real
// Anthropic to 4xx the request.
func TestProxyForwardsHostHeader(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec, false)
	defer upstream.Close()

	_, proxyURL := startProxyWithAddr(t, llmproxy.ProviderAnthropic, "k", upstream.URL)
	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()

	// fakeUpstream is an httptest.Server, so its host is something
	// like 127.0.0.1:NNNN. The proxy must rewrite req.Host to match
	// that, not pass through the proxy's own host.
	wantHost := strings.TrimPrefix(upstream.URL, "http://")
	if rec.host != wantHost {
		t.Errorf("upstream Host = %q, want %q (proxy must rewrite Host header to upstream's value)", rec.host, wantHost)
	}
}

// TestProxyErrChannelCleanOnShutdown asserts that the Err() channel is
// closed without sending an error when the server is shut down normally.
// This is the expected path for every healthy run end.
func TestProxyErrChannelCleanOnShutdown(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec, false)
	defer upstream.Close()

	srv, _ := llmproxy.New(llmproxy.Config{
		Provider: llmproxy.ProviderAnthropic,
		APIKey:   "k",
		Upstream: upstream.URL,
	})
	if _, err := srv.Start(""); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)

	// After normal shutdown the channel must be closed (select falls
	// through to the nil-error case) rather than containing an error.
	select {
	case err, ok := <-srv.Err():
		if ok {
			t.Errorf("Err() = %v after clean shutdown; want channel closed with no error", err)
		}
		// ok==false means channel closed cleanly — the expected path.
	case <-time.After(time.Second):
		t.Error("Err() channel not closed within 1s after Shutdown")
	}
}

// TestProxyTokenGateAnthropic pins SKY-395 Part A for the Anthropic
// path: with IncomingToken set, only a request presenting that exact
// token on x-api-key is forwarded; a wrong or missing token gets 401
// and never reaches the upstream. This is the fail-closed guarantee
// that a sibling run (which holds a different token) can't spend this
// run's credential even if it reaches the proxy over the shared host
// namespace.
func TestProxyTokenGateAnthropic(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec, false)
	defer upstream.Close()

	const token = "sk-ant-the-only-valid-token"
	srv, err := llmproxy.New(llmproxy.Config{
		Provider:      llmproxy.ProviderAnthropic,
		APIKey:        "sk-ant-REAL-KEY",
		Upstream:      upstream.URL,
		IncomingToken: token,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	proxyURL := "http://" + addr

	// Wrong token → 401.
	reqBad, _ := http.NewRequest("POST", proxyURL+"/v1/messages", strings.NewReader("{}"))
	reqBad.Header.Set("x-api-key", "sk-ant-some-OTHER-runs-token")
	respBad, err := http.DefaultClient.Do(reqBad)
	if err != nil {
		t.Fatalf("bad-token roundtrip: %v", err)
	}
	_ = respBad.Body.Close()
	if respBad.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", respBad.StatusCode)
	}

	// Missing token → 401 too.
	reqMissing, _ := http.NewRequest("POST", proxyURL+"/v1/messages", strings.NewReader("{}"))
	respMissing, err := http.DefaultClient.Do(reqMissing)
	if err != nil {
		t.Fatalf("missing-token roundtrip: %v", err)
	}
	_ = respMissing.Body.Close()
	if respMissing.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d, want 401", respMissing.StatusCode)
	}

	if rec.hits.Load() != 0 {
		t.Fatalf("upstream was hit %d time(s) by unauthorized requests; want 0 (proxy must not forward before auth)", rec.hits.Load())
	}

	// Correct token → forwarded, upstream sees the rewritten real key.
	reqOK, _ := http.NewRequest("POST", proxyURL+"/v1/messages", strings.NewReader("{}"))
	reqOK.Header.Set("x-api-key", token)
	respOK, err := http.DefaultClient.Do(reqOK)
	if err != nil {
		t.Fatalf("good-token roundtrip: %v", err)
	}
	_ = respOK.Body.Close()
	if respOK.StatusCode != 200 {
		t.Errorf("correct token: status = %d, want 200", respOK.StatusCode)
	}
	if rec.hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (only the authorized request forwarded)", rec.hits.Load())
	}
	if rec.xAPIKey != "sk-ant-REAL-KEY" {
		t.Errorf("upstream x-api-key = %q, want the real key (rewrite still swaps the token for the real key)", rec.xAPIKey)
	}
}

// TestProxyTokenGateBedrock mirrors the Anthropic gate test for the
// Bedrock bearer path: the token rides "Authorization: Bearer <token>"
// and a wrong/missing bearer gets 401 with the upstream untouched.
func TestProxyTokenGateBedrock(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec, false)
	defer upstream.Close()

	const token = "bedrock-run-token"
	srv, err := llmproxy.New(llmproxy.Config{
		Provider:      llmproxy.ProviderBedrockBearer,
		APIKey:        "bdrk-REAL-BEARER",
		Upstream:      upstream.URL,
		IncomingToken: token,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	proxyURL := "http://" + addr

	// Wrong bearer → 401, upstream untouched.
	reqBad, _ := http.NewRequest("POST", proxyURL+"/model/x/invoke", strings.NewReader("{}"))
	reqBad.Header.Set("Authorization", "Bearer wrong-token")
	respBad, err := http.DefaultClient.Do(reqBad)
	if err != nil {
		t.Fatalf("bad-bearer roundtrip: %v", err)
	}
	_ = respBad.Body.Close()
	if respBad.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong bearer: status = %d, want 401", respBad.StatusCode)
	}
	if rec.hits.Load() != 0 {
		t.Fatalf("upstream hit by unauthorized bearer; want 0")
	}

	// Correct bearer → forwarded, real bearer injected upstream.
	reqOK, _ := http.NewRequest("POST", proxyURL+"/model/x/invoke", strings.NewReader("{}"))
	reqOK.Header.Set("Authorization", "Bearer "+token)
	respOK, err := http.DefaultClient.Do(reqOK)
	if err != nil {
		t.Fatalf("good-bearer roundtrip: %v", err)
	}
	_ = respOK.Body.Close()
	if respOK.StatusCode != 200 {
		t.Errorf("correct bearer: status = %d, want 200", respOK.StatusCode)
	}
	if rec.authHeader != "Bearer bdrk-REAL-BEARER" {
		t.Errorf("upstream Authorization = %q, want \"Bearer bdrk-REAL-BEARER\"", rec.authHeader)
	}
}

// TestProxyNoTokenDisablesGate pins the opt-out: an empty IncomingToken
// leaves the proxy unauthenticated (the loopback/test + single-tenant
// direct path), so a request with no credential header is still
// forwarded. This is what keeps the pre-SKY-395 loopback usage working.
func TestProxyNoTokenDisablesGate(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec, false)
	defer upstream.Close()

	// startProxyWithAddr constructs Config without IncomingToken.
	_, proxyURL := startProxyWithAddr(t, llmproxy.ProviderAnthropic, "sk-ant-REAL", upstream.URL)

	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", strings.NewReader("{}"))
	// Deliberately no x-api-key.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (empty IncomingToken disables the gate)", resp.StatusCode)
	}
	if rec.hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", rec.hits.Load())
	}
}
