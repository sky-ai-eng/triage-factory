package runsidecar

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
	"github.com/sky-ai-eng/triage-factory/internal/egressproxy"
	"github.com/sky-ai-eng/triage-factory/internal/ghinjector"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// dialRuntime wires a credRuntime to an in-memory supervision channel and
// returns the orchestrator-side Conn plus the runtime. The orchestrator side
// has no handler (it only originates Calls in these tests).
func dialRuntime(t *testing.T) (*sidecarproto.Conn, *credRuntime) {
	t.Helper()
	return dialRuntimeWithHandler(t, nil)
}

// dialRuntimeWithHandler is dialRuntime with an orchestrator-side handler, for
// the tests that assert on what the sidecar relays UP (the audit notifies).
func dialRuntimeWithHandler(t *testing.T, h sidecarproto.Handler) (*sidecarproto.Conn, *credRuntime) {
	t.Helper()
	rt, err := newCredRuntime()
	if err != nil {
		t.Fatalf("newCredRuntime: %v", err)
	}
	orch, sidecar := net.Pipe()
	sidecarConn := sidecarproto.New(sidecar, rt)
	rt.setConn(sidecarConn)
	orchConn := sidecarproto.New(orch, h)
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

// TestRuntime_EgressDenialRelaysToOrchestrator drives the whole production
// path for a refused sandbox connection: the sidecar starts the run's proxies
// through the same StartRunProxies call the orchestrator asks for, the agent's
// own egress env is used to attempt an off-allowlist CONNECT, and the denial
// must arrive at the orchestrator as a record_egress_denial notify.
//
// This is the regression test for the class of failure that let the egress
// audit hook sit unwired for a release: nothing asserted the production
// construction path reached it. A nil hook at the StartRunProxies call site
// fails here.
func TestRuntime_EgressDenialRelaysToOrchestrator(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	notifies := make(chan sidecarproto.RelayCallBody, 4)
	handler := sidecarproto.HandlerFunc(func(_ context.Context, kind sidecarproto.Kind, body json.RawMessage) (any, error) {
		if kind != sidecarproto.KindRelayNotify {
			return nil, nil
		}
		var rc sidecarproto.RelayCallBody
		if err := json.Unmarshal(body, &rc); err != nil {
			return nil, err
		}
		notifies <- rc
		return nil, nil
	})
	orch, rt := dialRuntimeWithHandler(t, handler)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bundle := &credbundle.Bundle{LLM: map[string]string{
		"ANTHROPIC_API_KEY":  "sk-ant-real",
		"ANTHROPIC_BASE_URL": upstream.URL,
	}}
	if err := orch.Call(ctx, sidecarproto.KindSealedBundle,
		sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, bundle)}, nil); err != nil {
		t.Fatalf("relay bundle: %v", err)
	}

	var res sidecarproto.StartProxiesResult
	if err := orch.Call(ctx, sidecarproto.KindStartProxies,
		sidecarproto.StartProxiesBody{HostVethIP: "127.0.0.1"}, &res); err != nil {
		t.Fatalf("start proxies: %v", err)
	}

	// The proxy URL the sandbox itself would use, userinfo token and all.
	var proxyURL string
	for _, kv := range res.Env {
		if strings.HasPrefix(kv, "HTTPS_PROXY=") {
			proxyURL = strings.TrimPrefix(kv, "HTTPS_PROXY=")
		}
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		t.Fatalf("no usable HTTPS_PROXY in sandbox env: %q (%v)", proxyURL, err)
	}
	token, _ := u.User.Password()

	conn, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err != nil {
		t.Fatalf("dial egress proxy: %v", err)
	}
	defer conn.Close()
	auth := base64.StdEncoding.EncodeToString([]byte(egressproxy.BasicUser + ":" + token))
	_, _ = conn.Write([]byte("CONNECT api.github.com:443 HTTP/1.1\r\nHost: api.github.com:443\r\n" +
		"Proxy-Authorization: Basic " + auth + "\r\n\r\n"))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Read(make([]byte, 512))

	deadline := time.After(4 * time.Second)
	for {
		select {
		case got := <-notifies:
			if got.Op != agentproc.OpRecordEgressDenial {
				continue
			}
			var a agentproc.RecordEgressDenialArgs
			if err := json.Unmarshal(got.Args, &a); err != nil {
				t.Fatalf("unmarshal relayed denial: %v", err)
			}
			if a.Target != "api.github.com:443" || a.Reason == "" {
				t.Fatalf("relayed denial = %+v, want the CONNECT authority and a reason", a)
			}
			return
		case <-deadline:
			t.Fatal("the sidecar never relayed a refused CONNECT — the egress audit hook is unwired at the StartRunProxies call site")
		}
	}
}

// TestRuntime_GHInjectorRelaysWriteAudit pins the gh channel's second audit
// half: a mutating REST call through the injector relays a record_gh_write
// carrying method, path, and the upstream's status — including a non-2xx,
// which the artifact observation path drops entirely.
//
// It drives the injector built from the production config (ghInjectorConfig)
// rather than through startGHInjector, whose cert write lands under the
// privileged per-run socket root — unreachable to an unprivileged test runner.
// The wiring under test is the config's, so this is the whole of it: a missing
// ObserveWrite fails here.
func TestRuntime_GHInjectorRelaysWriteAudit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// An off-scope write masked as a not-found: the outcome that used to
		// leave no trace at all.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	notifies := make(chan sidecarproto.RelayCallBody, 4)
	handler := sidecarproto.HandlerFunc(func(_ context.Context, kind sidecarproto.Kind, body json.RawMessage) (any, error) {
		if kind != sidecarproto.KindRelayNotify {
			return nil, nil
		}
		var rc sidecarproto.RelayCallBody
		if err := json.Unmarshal(body, &rc); err != nil {
			return nil, err
		}
		notifies <- rc
		return nil, nil
	})
	orch, rt := dialRuntimeWithHandler(t, handler)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bundle := &credbundle.Bundle{
		LLM: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"},
		GitHub: &credbundle.GitHubCreds{
			Mode:     "app",
			CLIToken: &credbundle.RepoToken{Token: "ghs_REALCLITOKEN"},
		},
	}
	if err := orch.Call(ctx, sidecarproto.KindSealedBundle,
		sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, bundle)}, nil); err != nil {
		t.Fatalf("relay bundle: %v", err)
	}

	cert, certPEM, err := ghinjector.GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	const placeholder = "per-run-placeholder"
	srv, err := ghinjector.New(rt.ghInjectorConfig(upstream.URL, cert, placeholder, nil))
	if err != nil {
		t.Fatalf("construct injector: %v", err)
	}
	host, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start injector: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	})

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append injector cert to pool failed")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	req, _ := http.NewRequest(http.MethodPatch, "https://"+host+"/api/v3/repos/acme/widgets/pulls/7", nil)
	req.Header.Set("Authorization", "token "+placeholder)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through injector: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("caller saw %d, want the upstream's 404 (a 502 means the bundle token never resolved)", resp.StatusCode)
	}

	deadline := time.After(4 * time.Second)
	for {
		select {
		case got := <-notifies:
			if got.Op != agentproc.OpRecordGHWrite {
				continue
			}
			var a agentproc.RecordGHWriteArgs
			if err := json.Unmarshal(got.Args, &a); err != nil {
				t.Fatalf("unmarshal relayed gh write: %v", err)
			}
			if a.Method != http.MethodPatch || a.Status != http.StatusNotFound ||
				!strings.Contains(a.Path, "/repos/acme/widgets/pulls/7") {
				t.Fatalf("relayed write = %+v, want the PATCH, its path, and the 404", a)
			}
			return
		case <-deadline:
			t.Fatal("a gh-channel REST write never reached the orchestrator's audit path")
		}
	}
}

// TestRuntime_JiraAPIProxyReportsDeployment pins the classification the
// orchestrator cannot derive for itself: only this process opens the bundle
// that names the org's Jira backend, and the orchestrator's proxy client needs
// it to pick the REST version whose rich-text write shape the backend accepts.
// The injected upstream auth is asserted alongside so the two stay consistent —
// a Cloud bundle must produce both Basic auth and the Cloud classification.
func TestRuntime_JiraAPIProxyReportsDeployment(t *testing.T) {
	tests := []struct {
		name           string
		jira           *credbundle.JiraCreds
		wantDeployment string
		wantAuthPrefix string
	}{
		{
			name:           "cloud api token",
			jira:           &credbundle.JiraCreds{URL: "https://acme.atlassian.net", AuthMethod: "cloud", Email: "bot@acme.com", APIToken: "cloud-token"},
			wantDeployment: string(jira.DeploymentCloud),
			wantAuthPrefix: "Basic ",
		},
		{
			name:           "data center pat",
			jira:           &credbundle.JiraCreds{URL: "https://jira.acme.internal", AuthMethod: "datacenter", PAT: "dc-pat"},
			wantDeployment: string(jira.DeploymentDataCenter),
			wantAuthPrefix: "Bearer dc-pat",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotAuth := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth <- r.Header.Get("Authorization")
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()

			orch, rt := dialRuntime(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			bundle := &credbundle.Bundle{
				LLM:  map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"},
				Jira: tt.jira,
			}
			if err := orch.Call(ctx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, bundle)}, nil); err != nil {
				t.Fatalf("relay bundle: %v", err)
			}

			var res sidecarproto.StartProxiesResult
			if err := orch.Call(ctx, sidecarproto.KindStartProxies, sidecarproto.StartProxiesBody{
				HostVethIP:      "127.0.0.1",
				JiraAPIEnabled:  true,
				JiraAPIUpstream: upstream.URL,
			}, &res); err != nil {
				t.Fatalf("start proxies: %v", err)
			}
			if res.JiraDeployment != tt.wantDeployment {
				t.Errorf("JiraDeployment = %q, want %q", res.JiraDeployment, tt.wantDeployment)
			}

			req, _ := http.NewRequest("GET", res.JiraAPIURL+"/rest/api/3/issue/PROJ-1", nil)
			req.Header.Set("Authorization", "Bearer "+res.JiraAPIToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request through proxy: %v", err)
			}
			_ = resp.Body.Close()

			select {
			case auth := <-gotAuth:
				if !strings.HasPrefix(auth, tt.wantAuthPrefix) {
					t.Fatalf("upstream saw %q, want a value starting %q", auth, tt.wantAuthPrefix)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("upstream never received the proxied request")
			}
		})
	}
}
