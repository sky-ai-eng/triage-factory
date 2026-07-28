package inference

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llmproxy"
)

// TestClient_ThroughRunProxy drives the hop a sandboxed run actually makes:
// the client holds only the per-run placeholder token and a base URL pointing
// at its own credential proxy, and the proxy is what attaches the real key on
// the upstream leg.
//
// Nothing else covers that seam. The client's own tests stop at the request it
// builds and the proxy's stop at the request it forwards, so a mismatch
// between them — a header the proxy strips, a streaming response the client
// cannot read back — is exactly the class that shows up first as a live run
// failing at its first provider call.
func TestClient_ThroughRunProxy(t *testing.T) {
	const (
		realKey  = "sk-ant-the-real-key"
		runToken = "per-run-placeholder"
		model    = "claude-sonnet-5"
	)

	// The provider, as far as the proxy is concerned.
	var sawKey, sawPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawKey, sawPath = r.Header.Get("x-api-key"), r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range anthropicStreamFixture(model) {
			fmt.Fprint(w, frame)
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()

	proxy, err := llmproxy.New(llmproxy.Config{
		Provider:      llmproxy.ProviderAnthropic,
		APIKey:        realKey,
		Upstream:      upstream.URL,
		IncomingToken: runToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := proxy.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxy.Shutdown(ctx)
	}()

	// The run's view: placeholder credential, proxy base URL. Property B — no
	// real key is reachable from here.
	acct, err := NewAccount(ProviderCredentials{
		Provider: ProviderAnthropic,
		APIKey:   runToken,
		BaseURL:  "http://" + addr,
		Models:   []string{model},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(acct)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := client.Stream(ctx, Request{
		Provider: ProviderAnthropic,
		Model:    model,
		Rows:     []domain.Message{{Role: "user", Subtype: "text", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream through the run proxy: %v", err)
	}

	if got.Message.Content == nil || got.Message.Content.ContentStr == nil ||
		*got.Message.Content.ContentStr != "hello" {
		t.Errorf("reassembled content = %+v, want %q", got.Message.Content, "hello")
	}
	if got.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want %q", got.FinishReason, "stop")
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want 10 in / 5 out", got.Usage)
	}

	// The swap happened: the proxy replaced the run's placeholder with the org's
	// real key, and the path the client chose survived the hop.
	if sawKey != realKey {
		t.Errorf("upstream saw x-api-key %q, want the real key", sawKey)
	}
	if !strings.HasPrefix(sawPath, "/v1/") {
		t.Errorf("upstream path = %q, want the client's provider path", sawPath)
	}
}

// TestClient_ThroughRunProxy_WrongToken pins that the failure a
// mis-provisioned run gets is legible. The proxy answers 401 rather than
// forwarding, and the operator needs to see that in the error — not the fixed
// sentence bifrost attaches to every failure alike.
func TestClient_ThroughRunProxy_WrongToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a request with the wrong per-run token must never reach upstream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy, err := llmproxy.New(llmproxy.Config{
		Provider:      llmproxy.ProviderAnthropic,
		APIKey:        "sk-ant-real",
		Upstream:      upstream.URL,
		IncomingToken: "the-right-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := proxy.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxy.Shutdown(ctx)
	}()

	acct, err := NewAccount(ProviderCredentials{
		Provider: ProviderAnthropic,
		APIKey:   "the-wrong-token",
		BaseURL:  "http://" + addr,
		Models:   []string{"claude-sonnet-5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(acct)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = client.Stream(ctx, Request{
		Provider: ProviderAnthropic,
		Model:    "claude-sonnet-5",
		Rows:     []domain.Message{{Role: "user", Subtype: "text", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("a rejected token must fail the call")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("the error must name the hop that rejected it: %v", err)
	}
}

// TestClient_UnreachableEndpoint is the shape of a run whose credential proxy
// never came up: the dial fails, and the error has to carry both which host
// was unreachable and enough of the cause for the retry classifier to see a
// network failure rather than a permanent one.
func TestClient_UnreachableEndpoint(t *testing.T) {
	// Bind and immediately release, so the address is well-formed and dead.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()

	acct, err := NewAccount(ProviderCredentials{
		Provider: ProviderAnthropic,
		APIKey:   "placeholder",
		BaseURL:  "http://" + addr,
		Models:   []string{"claude-sonnet-5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(acct)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = client.Stream(ctx, Request{
		Provider: ProviderAnthropic,
		Model:    "claude-sonnet-5",
		Rows:     []domain.Message{{Role: "user", Subtype: "text", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("dialing a dead endpoint must fail")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("the error must name the unreachable host: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "refused") {
		t.Errorf("the error must carry the dial cause: %v", err)
	}
}

// TestClient_ThroughRunProxy_OnPrivateIP is the same hop on the address
// production actually uses.
//
// The loopback version above cannot substitute for it. bifrost refuses to dial
// an RFC 1918 address unless the account opts in, and exempts loopback from
// that check — so a proxy on 127.0.0.1 passes while the identical proxy on the
// run's 10.42.x.1 veth IP is refused before a packet leaves the process. That
// gap is not hypothetical: it is what a native run failed on while the
// loopback test reported the seam healthy.
func TestClient_ThroughRunProxy_OnPrivateIP(t *testing.T) {
	host := privateIPv4(t)
	if host == "" {
		t.Skip("no RFC 1918 address on any local interface; nothing to bind the private-IP case on")
	}
	const model = "claude-sonnet-5"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range anthropicStreamFixture(model) {
			fmt.Fprint(w, frame)
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()

	proxy, err := llmproxy.New(llmproxy.Config{
		Provider: llmproxy.ProviderAnthropic,
		APIKey:   "sk-ant-real",
		Upstream: upstream.URL,
		// The per-run veth gateway is not loopback; the proxy's own bind guard
		// wants that acknowledged, exactly as the sandbox integration does.
		AllowNonLoopback: true,
		IncomingToken:    "per-run-placeholder",
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := proxy.Start(net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxy.Shutdown(ctx)
	}()

	// The opt-in is what this case is about. That the delegate path actually
	// sets it is the other half, pinned in internal/agentloop next to the
	// decision — it cannot be asserted from here without an import cycle.
	acct, err := NewAccount(ProviderCredentials{
		Provider:            ProviderAnthropic,
		APIKey:              "per-run-placeholder",
		BaseURL:             "http://" + addr,
		Models:              []string{model},
		AllowPrivateNetwork: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(acct)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := client.Stream(ctx, Request{
		Provider: ProviderAnthropic,
		Model:    model,
		Rows:     []domain.Message{{Role: "user", Subtype: "text", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream through a proxy on a private IP: %v", err)
	}
	if got.Message.Content == nil || got.Message.Content.ContentStr == nil ||
		*got.Message.Content.ContentStr != "hello" {
		t.Errorf("reassembled content = %+v", got.Message.Content)
	}
}

// TestPrivateNetworkOptInIsRequired pins that the opt-in is load-bearing
// rather than incidental — without it the same call is refused, which is why
// the field exists at all.
func TestPrivateNetworkOptInIsRequired(t *testing.T) {
	host := privateIPv4(t)
	if host == "" {
		t.Skip("no RFC 1918 address on any local interface")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("cannot bind %s: %v", host, err)
	}
	defer listener.Close()

	acct, err := NewAccount(ProviderCredentials{
		Provider: ProviderAnthropic,
		APIKey:   "placeholder",
		BaseURL:  "http://" + listener.Addr().String(),
		Models:   []string{"claude-sonnet-5"},
		// AllowPrivateNetwork deliberately left false.
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(acct)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = client.Stream(ctx, Request{
		Provider: ProviderAnthropic,
		Model:    "claude-sonnet-5",
		Rows:     []domain.Message{{Role: "user", Subtype: "text", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("without the opt-in, a private-IP endpoint must be refused")
	}
	if !strings.Contains(err.Error(), "private IP") {
		t.Errorf("expected bifrost's private-IP refusal, got: %v", err)
	}
}

// privateIPv4 returns an RFC 1918 address bound to some local interface, or ""
// when the host has none. Real runs reach their sidecar over a private veth
// address, and loopback is exempt from the guard under test — so a
// loopback-only host genuinely cannot exercise these cases.
func privateIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		if ip.IsPrivate() {
			return ip.String()
		}
	}
	return ""
}

// anthropicStreamFixture is a minimal well-formed Anthropic SSE response:
// one text block, a stop reason, and usage on both ends.
func anthropicStreamFixture(model string) []string {
	return []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"" + model + "\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
}
