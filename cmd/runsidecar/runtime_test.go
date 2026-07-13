package runsidecar

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// dialRuntime wires a credRuntime to an in-memory supervision channel and
// returns the orchestrator-side Conn plus the runtime. The orchestrator side
// has no handler (it only originates Calls in these tests).
func dialRuntime(t *testing.T) (*sidecarproto.Conn, *credRuntime) {
	t.Helper()
	rt, err := newCredRuntime()
	if err != nil {
		t.Fatalf("newCredRuntime: %v", err)
	}
	orch, sidecar := net.Pipe()
	sidecarConn := sidecarproto.New(sidecar, rt)
	rt.setConn(sidecarConn)
	orchConn := sidecarproto.New(orch, nil)
	t.Cleanup(func() {
		_ = orchConn.Close()
		_ = sidecarConn.Close()
		_ = orch.Close()
		_ = sidecar.Close()
	})
	return orchConn, rt
}

func sealBundleTo(t *testing.T, pub [32]byte, b *credbundle.Bundle) []byte {
	t.Helper()
	plaintext, err := b.Marshal()
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	sealed, err := credseal.Seal(pub, plaintext)
	if err != nil {
		t.Fatalf("seal bundle: %v", err)
	}
	return sealed
}

// TestRuntime_UnsealsOwnBundle pins the happy path: a bundle sealed to the
// runtime's published public key is relayed over the channel, the runtime
// unseals it with its own private key, and holds the plaintext.
func TestRuntime_UnsealsOwnBundle(t *testing.T) {
	orch, rt := dialRuntime(t)

	want := &credbundle.Bundle{
		BootEpoch: 7,
		LLM:       map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"},
	}
	sealed := sealBundleTo(t, rt.keypair.Public, want)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := orch.Call(ctx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealed, BootEpoch: 7}, nil); err != nil {
		t.Fatalf("relay sealed bundle: %v", err)
	}

	got := rt.currentBundle()
	if got == nil {
		t.Fatal("runtime holds no bundle after relay")
	}
	if got.LLM["ANTHROPIC_API_KEY"] != "sk-ant-real" {
		t.Fatalf("unexpected LLM material: %+v", got.LLM)
	}
}

// TestRuntime_RejectsAnotherSidecarsBundle is the cross-sidecar isolation
// guarantee: a bundle sealed to a DIFFERENT sidecar's public key must not
// open with this runtime's private key. The relay Call returns the remote
// unseal error and the runtime holds nothing — one sidecar can never read
// another run's credentials even if it is handed the ciphertext.
func TestRuntime_RejectsAnotherSidecarsBundle(t *testing.T) {
	orch, rt := dialRuntime(t)

	// A foreign keypair standing in for another run's sidecar.
	foreign, err := credseal.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate foreign keypair: %v", err)
	}
	sealed := sealBundleTo(t, foreign.Public, &credbundle.Bundle{
		LLM: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-not-yours"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = orch.Call(ctx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealed}, nil)
	if err == nil {
		t.Fatal("expected unseal to fail for a foreign-sealed bundle")
	}
	if rt.currentBundle() != nil {
		t.Fatal("runtime must hold no bundle after a failed unseal")
	}
}

// TestRuntime_GitHubAPIProxyInjectsBundleToken drives the full API-proxy
// path through the runtime: a relayed bundle carries the real repo token, the
// orchestrator asks the sidecar to start the GitHub REST proxy, and a request
// presenting only the per-run placeholder reaches the upstream carrying the
// REAL token — the placeholder never leaves the caller and the real token
// never leaves the sidecar.
func TestRuntime_GitHubAPIProxyInjectsBundleToken(t *testing.T) {
	// Fake api.github.com that reports back the Authorization it received.
	gotAuth := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	orch, rt := dialRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Relay a bundle carrying an LLM key (so the LLM proxy starts) and a
	// repo-scoped GitHub token.
	bundle := &credbundle.Bundle{
		LLM: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"},
		GitHub: &credbundle.GitHubCreds{
			Mode:       "app",
			RepoTokens: map[string]credbundle.RepoToken{"acme/widgets": {Token: "ghs_REALINSTALLTOKEN"}},
		},
	}
	if err := orch.Call(ctx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, bundle)}, nil); err != nil {
		t.Fatalf("relay bundle: %v", err)
	}

	var res sidecarproto.StartProxiesResult
	if err := orch.Call(ctx, sidecarproto.KindStartProxies, sidecarproto.StartProxiesBody{
		HostVethIP:        "127.0.0.1",
		GitHubAPIEnabled:  true,
		GitHubAPIUpstream: upstream.URL,
	}, &res); err != nil {
		t.Fatalf("start proxies: %v", err)
	}
	if res.GitHubAPIURL == "" || res.GitHubAPIToken == "" {
		t.Fatalf("missing github api proxy coordinates: %+v", res)
	}

	// Present only the placeholder, targeting a repo path the proxy scopes on.
	req, _ := http.NewRequest("GET", res.GitHubAPIURL+"/repos/acme/widgets/pulls/1", nil)
	req.Header.Set("Authorization", "Bearer "+res.GitHubAPIToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case auth := <-gotAuth:
		if auth != "Bearer ghs_REALINSTALLTOKEN" {
			t.Fatalf("upstream saw %q, want the real bundle token injected", auth)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the proxied request")
	}
}

// TestRuntime_StartProxiesRequiresBundle pins that a StartProxies before any
// bundle has been relayed is rejected rather than starting credential-less
// proxies.
func TestRuntime_StartProxiesRequiresBundle(t *testing.T) {
	orch, _ := dialRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var res sidecarproto.StartProxiesResult
	err := orch.Call(ctx, sidecarproto.KindStartProxies, sidecarproto.StartProxiesBody{HostVethIP: "10.42.1.1"}, &res)
	if err == nil {
		t.Fatal("expected start-proxies before any bundle to fail")
	}
}

// TestRuntime_LLMSourceReflectsRefresh pins the live-refresh contract: after a
// newer bundle is relayed (the brain re-minted role-mode STS creds), the
// runtime's llmSource returns the newer material, and an expired bundle
// surfaces an error so the proxy 502s rather than signing stale.
func TestRuntime_LLMSourceReflectsRefresh(t *testing.T) {
	orch, rt := dialRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// First bundle: a live (future-expiry) session triple.
	future := time.Now().Add(time.Hour)
	b1 := &credbundle.Bundle{
		LLM:           map[string]string{"AWS_ACCESS_KEY_ID": "AKIA1", "AWS_SECRET_ACCESS_KEY": "s1"},
		LLMExpiryUnix: future.Unix(),
	}
	if err := orch.Call(ctx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, b1)}, nil); err != nil {
		t.Fatalf("relay b1: %v", err)
	}
	env, _, err := rt.llmSource(ctx)
	if err != nil {
		t.Fatalf("llmSource after b1: %v", err)
	}
	if env["AWS_ACCESS_KEY_ID"] != "AKIA1" {
		t.Fatalf("expected AKIA1, got %v", env["AWS_ACCESS_KEY_ID"])
	}

	// Refresh: a newer triple replaces it.
	b2 := &credbundle.Bundle{
		LLM:           map[string]string{"AWS_ACCESS_KEY_ID": "AKIA2", "AWS_SECRET_ACCESS_KEY": "s2"},
		LLMExpiryUnix: future.Unix(),
	}
	if err := orch.Call(ctx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, b2)}, nil); err != nil {
		t.Fatalf("relay b2: %v", err)
	}
	env, _, err = rt.llmSource(ctx)
	if err != nil {
		t.Fatalf("llmSource after b2: %v", err)
	}
	if env["AWS_ACCESS_KEY_ID"] != "AKIA2" {
		t.Fatalf("expected refreshed AKIA2, got %v", env["AWS_ACCESS_KEY_ID"])
	}

	// An expired bundle must surface an error (proxy 502s with the
	// refresh-lagging hint rather than signing a dead session token).
	past := time.Now().Add(-time.Minute)
	b3 := &credbundle.Bundle{
		LLM:           map[string]string{"AWS_ACCESS_KEY_ID": "AKIA3", "AWS_SECRET_ACCESS_KEY": "s3"},
		LLMExpiryUnix: past.Unix(),
	}
	if err := orch.Call(ctx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, b3)}, nil); err != nil {
		t.Fatalf("relay b3: %v", err)
	}
	if _, _, err := rt.llmSource(ctx); err == nil {
		t.Fatal("expected llmSource to error on an expired bundle")
	}
}
