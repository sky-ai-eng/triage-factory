package runsidecar

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// TestLinearUpstreamIsLinearsAPIHost pins where the Linear proxy forwards: the
// origin of the client's own endpoint, whose /graphql path is the one the
// proxy lets through.
func TestLinearUpstreamIsLinearsAPIHost(t *testing.T) {
	if linearUpstream != "https://api.linear.app" {
		t.Errorf("linearUpstream = %q, want https://api.linear.app", linearUpstream)
	}
	if linearUpstream+"/graphql" != linear.DefaultEndpoint {
		t.Errorf("linearUpstream + /graphql = %q, want the client's endpoint %q", linearUpstream+"/graphql", linear.DefaultEndpoint)
	}
}

// TestRuntime_LinearAPIProxyInjectsBundleAuth drives the Linear proxy through
// the supervision channel the way the orchestrator does, with the same client
// the agenthost daemon builds against it (linear.ProxyPlaceholder). Upstream
// must see the bundle's credential in the shape Linear takes it — a personal
// API key as the whole Authorization value, an app user's access token as a
// Bearer — and never the placeholder. Each re-sealed bundle is used from the
// next request on, with no restart: the auth is read per request.
func TestRuntime_LinearAPIProxyInjectsBundleAuth(t *testing.T) {
	gotAuth := make(chan string, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			t.Errorf("upstream path = %q, want /graphql", r.URL.Path)
		}
		gotAuth <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"app-user","name":"Triage Factory"}}}`))
	}))
	defer upstream.Close()
	prev := linearUpstream
	linearUpstream = upstream.URL
	t.Cleanup(func() { linearUpstream = prev })

	orch, rt := dialRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	relay := func(lc *credbundle.LinearCreds) {
		t.Helper()
		b := &credbundle.Bundle{LLM: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"}, Linear: lc}
		if err := orch.Call(ctx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, b)}, nil); err != nil {
			t.Fatalf("relay bundle: %v", err)
		}
	}
	relay(&credbundle.LinearCreds{AuthMethod: string(linear.AuthMethodAPIKey), APIKey: "lin_api_REALKEY"})

	var res sidecarproto.StartProxiesResult
	if err := orch.Call(ctx, sidecarproto.KindStartProxies, sidecarproto.StartProxiesBody{
		HostVethIP:       "127.0.0.1",
		LinearAPIEnabled: true,
	}, &res); err != nil {
		t.Fatalf("start proxies: %v", err)
	}
	if res.LinearAPIURL == "" || res.LinearAPIToken == "" {
		t.Fatalf("missing linear api proxy coordinates: %+v", res)
	}
	client := linear.NewClient(linear.ProxyPlaceholder(res.LinearAPIURL, res.LinearAPIToken))

	expectAuth := func(want string) {
		t.Helper()
		viewer, err := client.Viewer(ctx)
		if err != nil {
			t.Fatalf("Viewer through the proxy: %v", err)
		}
		if viewer.ID != "app-user" {
			t.Errorf("viewer = %+v, want the upstream's answer", viewer)
		}
		select {
		case got := <-gotAuth:
			if got != want {
				t.Errorf("upstream Authorization = %q, want %q", got, want)
			}
			if strings.Contains(got, res.LinearAPIToken) {
				t.Errorf("upstream saw the placeholder: %q", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("upstream never received the proxied request")
		}
	}

	expectAuth("lin_api_REALKEY")

	// The brain re-seals: the org is now an app install. The next request
	// carries the app user's access token, as a Bearer.
	relay(&credbundle.LinearCreds{
		AuthMethod:  string(linear.AuthMethodAppInstall),
		AccessToken: "lin_oauth_FIRST",
		ExpiresUnix: time.Now().Add(24 * time.Hour).Unix(),
	})
	expectAuth("Bearer lin_oauth_FIRST")

	relay(&credbundle.LinearCreds{
		AuthMethod:  string(linear.AuthMethodAppInstall),
		AccessToken: "lin_oauth_SECOND",
		ExpiresUnix: time.Now().Add(24 * time.Hour).Unix(),
	})
	expectAuth("Bearer lin_oauth_SECOND")

	// A sibling run's placeholder is refused before anything is resolved or
	// forwarded.
	req, _ := http.NewRequest(http.MethodPost, res.LinearAPIURL+"/graphql", strings.NewReader(`{"query":"{ viewer { id } }"}`))
	req.Header.Set("Authorization", "Bearer sibling-run-placeholder")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request with a wrong placeholder: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong placeholder = %d, want 401", resp.StatusCode)
	}
	select {
	case got := <-gotAuth:
		t.Errorf("a wrong placeholder reached upstream with Authorization %q", got)
	default:
	}
}

// TestRuntime_LinearAPIProxyStartsWithoutCredential pins the degraded run: a
// Linear run whose bundle carries no Linear credential still comes up, its
// proxy answers each request with which miss it is — none configured (403) or
// not resolved for this seal (502, which a re-seal can change) — in the
// reason field the agenthost turns into guidance, and the first re-seal that
// carries the credential is used from the next request on.
func TestRuntime_LinearAPIProxyStartsWithoutCredential(t *testing.T) {
	gotAuth := make(chan string, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"app-user"}}}`))
	}))
	defer upstream.Close()
	prev := linearUpstream
	linearUpstream = upstream.URL
	t.Cleanup(func() { linearUpstream = prev })

	orch, rt := dialRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	relay := func(b *credbundle.Bundle) {
		t.Helper()
		b.LLM = map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"}
		if err := orch.Call(ctx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, b)}, nil); err != nil {
			t.Fatalf("relay bundle: %v", err)
		}
	}

	relay(&credbundle.Bundle{})
	var res sidecarproto.StartProxiesResult
	if err := orch.Call(ctx, sidecarproto.KindStartProxies, sidecarproto.StartProxiesBody{
		HostVethIP:       "127.0.0.1",
		LinearAPIEnabled: true,
	}, &res); err != nil {
		t.Fatalf("start proxies without a Linear credential: %v", err)
	}
	if res.LinearAPIURL == "" {
		t.Fatalf("no linear api proxy coordinates: %+v", res)
	}

	expectMiss := func(wantStatus int, wantReason string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, res.LinearAPIURL+"/graphql", strings.NewReader(`{"query":"{ viewer { id } }"}`))
		req.Header.Set("Authorization", "Bearer "+res.LinearAPIToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request through the proxy: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode != wantStatus || body.Reason != wantReason {
			t.Errorf("miss = (%d, %q), want (%d, %q)", resp.StatusCode, body.Reason, wantStatus, wantReason)
		}
		select {
		case got := <-gotAuth:
			t.Errorf("a request with no credential reached upstream with Authorization %q", got)
		default:
		}
	}

	expectMiss(http.StatusForbidden, credbundle.MissNoLinearCredential)

	relay(&credbundle.Bundle{Unresolved: []string{"linear"}})
	expectMiss(http.StatusBadGateway, credbundle.MissLinearCredentialUnresolved)

	relay(&credbundle.Bundle{Linear: &credbundle.LinearCreds{AuthMethod: string(linear.AuthMethodAPIKey), APIKey: "lin_api_REALKEY"}})
	client := linear.NewClient(linear.ProxyPlaceholder(res.LinearAPIURL, res.LinearAPIToken))
	if _, err := client.Viewer(ctx); err != nil {
		t.Fatalf("Viewer after the re-seal: %v", err)
	}
	if got := <-gotAuth; got != "lin_api_REALKEY" {
		t.Errorf("upstream Authorization after the re-seal = %q, want the key", got)
	}
}

// TestRuntime_LinearAuthHeaderNamesTheMiss pins the sentinel the Linear auth
// source wraps for each bundle it cannot answer from, and that it answers an
// expired app token rather than refusing it: Linear's refusal is what the
// verb reports.
func TestRuntime_LinearAuthHeaderNamesTheMiss(t *testing.T) {
	rt, err := newCredRuntime()
	if err != nil {
		t.Fatalf("newCredRuntime: %v", err)
	}
	ctx := context.Background()
	hold := func(b *credbundle.Bundle) {
		rt.mu.Lock()
		rt.bundle = b
		rt.mu.Unlock()
	}

	if _, err := rt.linearAuthHeader(ctx); !errors.Is(err, credbundle.ErrNoBundle) {
		t.Errorf("no bundle: error %v, want ErrNoBundle", err)
	}
	hold(&credbundle.Bundle{Unresolved: []string{"linear"}})
	if _, err := rt.linearAuthHeader(ctx); !errors.Is(err, credbundle.ErrLinearCredentialUnresolved) {
		t.Errorf("unresolved for this seal: error %v, want ErrLinearCredentialUnresolved", err)
	}
	for _, tc := range []struct {
		name string
		lc   *credbundle.LinearCreds
	}{
		{"no linear half", nil},
		{"empty api key", &credbundle.LinearCreds{AuthMethod: string(linear.AuthMethodAPIKey)}},
		{"empty access token", &credbundle.LinearCreds{AuthMethod: string(linear.AuthMethodAppInstall)}},
	} {
		hold(&credbundle.Bundle{Linear: tc.lc})
		if _, err := rt.linearAuthHeader(ctx); !errors.Is(err, credbundle.ErrNoLinearCredential) {
			t.Errorf("%s: error %v, want ErrNoLinearCredential", tc.name, err)
		}
	}

	hold(&credbundle.Bundle{Linear: &credbundle.LinearCreds{AuthMethod: "connect_oauth", AccessToken: "x"}})
	if v, err := rt.linearAuthHeader(ctx); err == nil {
		t.Errorf("unknown auth method answered %q, want an error", v)
	}

	hold(&credbundle.Bundle{Linear: &credbundle.LinearCreds{
		AuthMethod:  string(linear.AuthMethodAppInstall),
		AccessToken: "lin_oauth_STALE",
		ExpiresUnix: time.Now().Add(-time.Hour).Unix(),
	}})
	if v, err := rt.linearAuthHeader(ctx); err != nil || v != "Bearer lin_oauth_STALE" {
		t.Errorf("expired app token = (%q, %v), want it forwarded as a Bearer", v, err)
	}
}
