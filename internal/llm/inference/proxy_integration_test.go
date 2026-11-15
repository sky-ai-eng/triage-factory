package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go/eventstream"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/llmproxy"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/proxycert"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// TestClient_ThroughRunProxy drives the hop a sandboxed run actually makes:
// the client holds only the per-run placeholder token, a base URL pointing at
// its own credential proxy, and the certificate that proxy serves TLS with,
// and the proxy is what attaches the real key on the upstream leg.
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

	proxyURL, certPEM := startRunProxy(t, llmproxy.Config{
		Provider:      llmproxy.ProviderAnthropic,
		APIKey:        realKey,
		Upstream:      upstream.URL,
		IncomingToken: runToken,
	}, "127.0.0.1")

	// The run's view: placeholder credential, proxy base URL, the proxy's
	// certificate. Property B — no real key is reachable from here.
	acct, err := NewAccount(ProviderCredentials{
		Provider:  ProviderAnthropic,
		APIKey:    runToken,
		BaseURL:   proxyURL,
		Models:    []string{model},
		CACertPEM: certPEM,
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
		Rows:     []domain.Message{{Role: "user", Content: "hi"}},
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
	if got.Usage.PromptTokens != 10 || got.Usage.OutputTokens != 5 {
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

	proxyURL, certPEM := startRunProxy(t, llmproxy.Config{
		Provider:      llmproxy.ProviderAnthropic,
		APIKey:        "sk-ant-real",
		Upstream:      upstream.URL,
		IncomingToken: "the-right-token",
	}, "127.0.0.1")
	addr := strings.TrimPrefix(proxyURL, "https://")

	acct, err := NewAccount(ProviderCredentials{
		Provider:  ProviderAnthropic,
		APIKey:    "the-wrong-token",
		BaseURL:   proxyURL,
		Models:    []string{"claude-sonnet-5"},
		CACertPEM: certPEM,
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
		Rows:     []domain.Message{{Role: "user", Content: "hi"}},
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
// was unreachable and enough of the cause for Classify to read a network
// failure rather than a permanent one.
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
		Rows:     []domain.Message{{Role: "user", Content: "hi"}},
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
	if got, _ := Classify(ctx, err); got != upstream.Transient {
		t.Errorf("a dead endpoint classifies %q, want %q: %v", got, upstream.Transient, err)
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

	proxyURL, certPEM := startRunProxy(t, llmproxy.Config{
		Provider: llmproxy.ProviderAnthropic,
		APIKey:   "sk-ant-real",
		Upstream: upstream.URL,
		// The per-run veth gateway is not loopback; the proxy's own bind guard
		// wants that acknowledged, exactly as the sandbox integration does.
		AllowNonLoopback: true,
		IncomingToken:    "per-run-placeholder",
	}, host)

	// The opt-in is what this case is about. That the delegate path actually
	// sets it is the other half, pinned in internal/agentloop next to the
	// decision — it cannot be asserted from here without an import cycle.
	acct, err := NewAccount(ProviderCredentials{
		Provider:            ProviderAnthropic,
		APIKey:              "per-run-placeholder",
		BaseURL:             proxyURL,
		Models:              []string{model},
		AllowPrivateNetwork: true,
		CACertPEM:           certPEM,
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
		Rows:     []domain.Message{{Role: "user", Content: "hi"}},
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
		Rows:     []domain.Message{{Role: "user", Content: "hi"}},
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

// TestClient_BedrockThroughRunProxy drives bifrost's Bedrock client through a
// TLS run proxy with each Bedrock credential a run can hold. bifrost dials
// Bedrock only over https, so this is the hop a plain-http proxy broke. The
// client is built from the env the sidecar hands the native engine.
func TestClient_BedrockThroughRunProxy(t *testing.T) {
	const (
		model    = "us.anthropic.claude-sonnet-5"
		runToken = "AKIAPERRUNPLACEHOLDER"
	)
	realPair := aws.Credentials{AccessKeyID: "AKIAREALORGKEY", SecretAccessKey: "the-real-secret"}
	cases := map[string]struct {
		proxy llmproxy.Config
		env   map[string]string
		check func(r *http.Request, body []byte) error
	}{
		"api key": {
			proxy: llmproxy.Config{Provider: llmproxy.ProviderBedrockBearer, APIKey: "the-real-key"},
			env:   map[string]string{"AWS_BEARER_TOKEN_BEDROCK": runToken},
			check: func(r *http.Request, _ []byte) error {
				if got := r.Header.Get("Authorization"); got != "Bearer the-real-key" {
					return fmt.Errorf("Authorization = %q, want the real key", got)
				}
				return nil
			},
		},
		"access-key pair": {
			proxy: llmproxy.Config{Provider: llmproxy.ProviderBedrockSigV4, SigV4Credentials: &llmproxy.SigV4Credentials{
				AccessKeyID: realPair.AccessKeyID, SecretAccessKey: realPair.SecretAccessKey, Region: "us-west-2",
			}},
			env:   map[string]string{"AWS_ACCESS_KEY_ID": runToken, "AWS_SECRET_ACCESS_KEY": "placeholder", "AWS_REGION": "us-west-2"},
			check: func(r *http.Request, body []byte) error { return sigV4Matches(r, body, realPair, "us-west-2") },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			paths := make(chan string, 8)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				paths <- r.URL.Path
				if err := tc.check(r, body); err != nil {
					t.Errorf("the request that reached the provider: %v", err)
				}
				writeConverseStream(t, w)
			}))
			defer upstream.Close()

			cfg := tc.proxy
			cfg.Upstream, cfg.IncomingToken = upstream.URL, runToken
			proxyURL, certPEM := startRunProxy(t, cfg, "127.0.0.1")

			env := maps.Clone(tc.env)
			env["ANTHROPIC_BEDROCK_BASE_URL"] = proxyURL
			creds, err := ProviderCredentialsFromEnv(env, []string{model})
			if err != nil {
				t.Fatal(err)
			}
			creds.CACertPEM = certPEM
			got, err := streamOnce(t, creds, model)
			if err != nil {
				t.Fatalf("stream through the run proxy: %v", err)
			}
			if c := got.Message.Content; c == nil || c.ContentStr == nil || *c.ContentStr != "hello" {
				t.Errorf("reassembled content = %+v, want %q", c, "hello")
			}
			if len(paths) != 1 {
				t.Fatalf("the provider got %d calls, want 1", len(paths))
			}
			if p, want := <-paths, "/model/"+model+"/converse-stream"; p != want {
				t.Errorf("path = %q, want %q", p, want)
			}
		})
	}
}

// TestClient_ThroughRunProxy_UntrustedCertificate pins that the hop is
// verified: a client not given the proxy's certificate fails the handshake,
// and nothing reaches the provider.
func TestClient_ThroughRunProxy_UntrustedCertificate(t *testing.T) {
	const model = "us.anthropic.claude-sonnet-5"
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a call over an unverified hop reached the provider")
	}))
	defer upstream.Close()
	proxyURL, _ := startRunProxy(t, llmproxy.Config{Provider: llmproxy.ProviderBedrockBearer, APIKey: "real", Upstream: upstream.URL}, "127.0.0.1")

	_, err := streamOnce(t, ProviderCredentials{
		Provider: ProviderBedrock, APIKey: "run", BaseURL: proxyURL, Models: []string{model},
		Bedrock: &BedrockCredentials{Region: "us-east-1"},
	}, model)
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("want the handshake refused for the proxy's certificate, got %v", err)
	}
}

// startRunProxy starts cfg's proxy as a run's sidecar does: TLS on host under
// a certificate minted for it. It returns the https base URL and the
// certificate the run's client trusts.
func startRunProxy(t *testing.T, cfg llmproxy.Config, host string) (string, []byte) {
	t.Helper()
	cert, certPEM, err := proxycert.Generate("llm proxy", host)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Cert = &cert
	proxy, err := llmproxy.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := proxy.Start(net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxy.Shutdown(ctx)
	})
	return "https://" + addr, certPEM
}

// streamOnce makes one call with creds.
func streamOnce(t *testing.T, creds ProviderCredentials, model string) (*Completion, error) {
	t.Helper()
	acct, err := NewAccount(creds)
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
	return client.Stream(ctx, Request{Provider: creds.Provider, Model: model, Rows: []domain.Message{{Role: "user", Content: "hi"}}})
}

// writeConverseStream answers with a minimal Bedrock ConverseStream response,
// one AWS event-stream frame per event.
func writeConverseStream(t *testing.T, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	enc := eventstream.NewEncoder()
	for _, ev := range [][2]string{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"hello"}}`},
		{"messageStop", `{"stopReason":"end_turn"}`},
	} {
		if err := enc.Encode(w, eventstream.Message{Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("event")},
			{Name: ":event-type", Value: eventstream.StringValue(ev[0])},
		}, Payload: []byte(ev[1])}); err != nil {
			t.Errorf("encode %s: %v", ev[0], err)
			return
		}
	}
}

// sigV4Matches reports whether r, as it arrived, carries the signature creds
// give it: it re-signs a request rebuilt from what arrived, over the headers
// the Authorization header names, at the same time and scope, and compares.
// llmproxy checks the signer itself against AWS's test vector; this checks
// that what the proxy signed is what reached the provider.
func sigV4Matches(r *http.Request, body []byte, creds aws.Credentials, region string) error {
	authz := r.Header.Get("Authorization")
	_, signed, _ := strings.Cut(authz, "SignedHeaders=")
	signed, _, _ = strings.Cut(signed, ",")
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return fmt.Errorf("parse X-Amz-Date: %w", err)
	}
	rebuilt, err := http.NewRequest(r.Method, "http://"+r.Host+r.URL.EscapedPath(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	for _, name := range strings.Split(signed, ";") {
		if name != "host" && name != "content-length" {
			rebuilt.Header[http.CanonicalHeaderKey(name)] = r.Header.Values(name)
		}
	}
	sum := sha256.Sum256(body)
	if err := v4.NewSigner().SignHTTP(context.Background(), creds, rebuilt, hex.EncodeToString(sum[:]), "bedrock", region, at); err != nil {
		return err
	}
	if want := rebuilt.Header.Get("Authorization"); want != authz {
		return fmt.Errorf("Authorization\n got %s\nwant %s", authz, want)
	}
	return nil
}
