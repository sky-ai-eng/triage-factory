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
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go/eventstream"
	"github.com/maximhq/bifrost/core/schemas"

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

// TestClient_BedrockThroughRunProxy is the Bedrock half of the run's hop, for
// both shapes of Bedrock credential a run's sidecar holds: a Bedrock API key,
// which the proxy swaps for the real bearer, and an access-key pair, whose
// signature the proxy replaces with one under the real secret.
//
// bifrost's Bedrock provider dials its endpoint over https whatever scheme
// the URL names, so a plain-http proxy fails this at the handshake. The client
// is built from the env shape the sidecar hands the native engine, with the
// proxy's certificate beside it, the way agentloop.EnvCredentials builds it.
func TestClient_BedrockThroughRunProxy(t *testing.T) {
	const (
		model    = "us.anthropic.claude-sonnet-5"
		region   = "us-west-2"
		runToken = "AKIAPERRUNPLACEHOLDER"
	)
	realTriple := aws.Credentials{AccessKeyID: "AKIAREALORGKEY", SecretAccessKey: "the-real-secret"}

	cases := []struct {
		name  string
		proxy llmproxy.Config
		env   map[string]string
		// check inspects what reached the provider and the body it carried.
		check func(t *testing.T, r *http.Request, body []byte)
	}{
		{
			name: "bedrock api key",
			proxy: llmproxy.Config{
				Provider:      llmproxy.ProviderBedrockBearer,
				APIKey:        "the-real-bedrock-key",
				IncomingToken: runToken,
			},
			env: map[string]string{
				"AWS_BEARER_TOKEN_BEDROCK": runToken,
				"CLAUDE_CODE_USE_BEDROCK":  "1",
			},
			check: func(t *testing.T, r *http.Request, _ []byte) {
				if got := r.Header.Get("Authorization"); got != "Bearer the-real-bedrock-key" {
					t.Errorf("upstream Authorization = %q, want the real Bedrock key", got)
				}
			},
		},
		{
			name: "access-key pair",
			proxy: llmproxy.Config{
				Provider: llmproxy.ProviderBedrockSigV4,
				SigV4Credentials: &llmproxy.SigV4Credentials{
					AccessKeyID:     realTriple.AccessKeyID,
					SecretAccessKey: realTriple.SecretAccessKey,
					Region:          region,
				},
				IncomingToken: runToken,
			},
			env: map[string]string{
				"AWS_ACCESS_KEY_ID":       runToken,
				"AWS_SECRET_ACCESS_KEY":   "tf-proxy-placeholder-not-a-credential",
				"AWS_REGION":              region,
				"CLAUDE_CODE_USE_BEDROCK": "1",
			},
			check: func(t *testing.T, r *http.Request, body []byte) {
				if err := sigV4Matches(r, body, realTriple, region); err != nil {
					t.Errorf("the request that reached the provider is not signed under the real secret: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu   sync.Mutex
				seen []string
			)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read the forwarded body: %v", err)
				}
				mu.Lock()
				seen = append(seen, r.URL.Path)
				mu.Unlock()
				tc.check(t, r, body)
				writeConverseStream(t, w)
			}))
			defer upstream.Close()

			cfg := tc.proxy
			cfg.Upstream = upstream.URL
			proxyURL, certPEM := startRunProxy(t, cfg, "127.0.0.1")

			env := maps.Clone(tc.env)
			env["ANTHROPIC_BEDROCK_BASE_URL"] = proxyURL
			creds, err := ProviderCredentialsFromEnv(env, []string{model})
			if err != nil {
				t.Fatal(err)
			}
			creds.CACertPEM = certPEM
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
			got, err := client.Stream(ctx, Request{
				Provider: ProviderBedrock,
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
			if got.Usage.PromptTokens != 10 || got.Usage.OutputTokens != 5 {
				t.Errorf("usage = %+v, want 10 in / 5 out", got.Usage)
			}

			mu.Lock()
			defer mu.Unlock()
			want := "/model/" + model + "/converse-stream"
			if len(seen) != 1 || seen[0] != want {
				t.Errorf("upstream paths = %v, want one call to %s", seen, want)
			}
		})
	}
}

// TestClient_ThroughRunProxy_UntrustedCertificate pins that the hop to the
// run's proxy is verified: a client not given the proxy's certificate fails
// the handshake, and nothing reaches the provider.
func TestClient_ThroughRunProxy_UntrustedCertificate(t *testing.T) {
	cases := []struct {
		name     string
		provider schemas.ModelProvider
		model    string
		proxy    llmproxy.Config
		creds    func(baseURL string) ProviderCredentials
	}{
		{
			name:     "anthropic",
			provider: ProviderAnthropic,
			model:    "claude-sonnet-5",
			proxy:    llmproxy.Config{Provider: llmproxy.ProviderAnthropic, APIKey: "sk-ant-real", IncomingToken: "run-token"},
			creds: func(baseURL string) ProviderCredentials {
				return ProviderCredentials{Provider: ProviderAnthropic, APIKey: "run-token", BaseURL: baseURL, Models: []string{"claude-sonnet-5"}}
			},
		},
		{
			name:     "bedrock",
			provider: ProviderBedrock,
			model:    "us.anthropic.claude-sonnet-5",
			proxy:    llmproxy.Config{Provider: llmproxy.ProviderBedrockBearer, APIKey: "real-bearer", IncomingToken: "run-token"},
			creds: func(baseURL string) ProviderCredentials {
				return ProviderCredentials{
					Provider: ProviderBedrock, APIKey: "run-token", BaseURL: baseURL,
					Models: []string{"us.anthropic.claude-sonnet-5"}, Bedrock: &BedrockCredentials{Region: "us-east-1"},
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("a call over an unverified hop reached the provider")
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()

			cfg := tc.proxy
			cfg.Upstream = upstream.URL
			proxyURL, _ := startRunProxy(t, cfg, "127.0.0.1")

			acct, err := NewAccount(tc.creds(proxyURL))
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
				Provider: tc.provider,
				Model:    tc.model,
				Rows:     []domain.Message{{Role: "user", Content: "hi"}},
			})
			if err == nil {
				t.Fatal("a client that does not trust the proxy's certificate completed a call")
			}
			if !strings.Contains(err.Error(), "certificate") {
				t.Errorf("the error must name the certificate the client refused: %v", err)
			}
		})
	}
}

// startRunProxy starts cfg's proxy the way a run's sidecar does: TLS on host,
// under a certificate minted for that address. It returns the https base URL
// and the certificate the run's client is given to trust.
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

// writeConverseStream answers with a minimal well-formed Bedrock
// ConverseStream response: one text block, a stop reason, and usage, each an
// AWS event-stream frame.
func writeConverseStream(t *testing.T, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	w.WriteHeader(http.StatusOK)
	enc := eventstream.NewEncoder()
	for _, ev := range []struct{ typ, payload string }{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"hello"}}`},
		{"contentBlockStop", `{"contentBlockIndex":0}`},
		{"messageStop", `{"stopReason":"end_turn"}`},
		{"metadata", `{"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15},"metrics":{"latencyMs":1}}`},
	} {
		msg := eventstream.Message{
			Headers: eventstream.Headers{
				{Name: ":message-type", Value: eventstream.StringValue("event")},
				{Name: ":event-type", Value: eventstream.StringValue(ev.typ)},
				{Name: ":content-type", Value: eventstream.StringValue("application/json")},
			},
			Payload: []byte(ev.payload),
		}
		if err := enc.Encode(w, msg); err != nil {
			t.Errorf("encode the %s frame: %v", ev.typ, err)
			return
		}
		w.(http.Flusher).Flush()
	}
}

// sigV4Matches reports whether r, as it arrived, carries the signature creds
// give it: it rebuilds the request from the method, host, path, body, and the
// headers the Authorization header names as signed, signs that at the same
// time and scope, and compares the two Authorization headers. The signer
// itself is checked against AWS's published test vector in llmproxy; what
// this checks is that the request the proxy signed is the one that reached the
// provider, on the path bifrost calls.
func sigV4Matches(r *http.Request, body []byte, creds aws.Credentials, region string) error {
	authz := r.Header.Get("Authorization")
	_, signed, ok := strings.Cut(authz, "SignedHeaders=")
	if !ok {
		return fmt.Errorf("Authorization %q names no signed headers", authz)
	}
	signed, _, _ = strings.Cut(signed, ",")
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return fmt.Errorf("parse X-Amz-Date: %w", err)
	}
	target := "http://" + r.Host + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	rebuilt, err := http.NewRequest(r.Method, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for _, name := range strings.Split(signed, ";") {
		if name == "host" || name == "content-length" {
			continue
		}
		rebuilt.Header[http.CanonicalHeaderKey(name)] = r.Header.Values(name)
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
