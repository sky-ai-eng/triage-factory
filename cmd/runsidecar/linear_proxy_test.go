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
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
		b := &credbundle.Bundle{
			LLM:     map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"},
			Linear:  lc,
			Sources: domain.SourceManifest{"linear": {State: domain.SourceSealed}},
		}
		if err := orch.Call(ctx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, b)}, nil); err != nil {
			t.Fatalf("relay bundle: %v", err)
		}
	}
	relay(&credbundle.LinearCreds{AuthMethod: string(linear.AuthMethodAPIKey), APIKey: "lin_api_REALKEY"})

	var res sidecarproto.StartProxiesResult
	if err := orch.Call(ctx, sidecarproto.KindStartProxies, sidecarproto.StartProxiesBody{
		HostVethIP: "127.0.0.1",
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

// TestRuntime_LinearAPIProxyStartsWhileUnresolved pins the degraded run: a
// run whose manifest names Linear unresolved still gets its proxy, which
// answers each request with the manifest's state and the source — unresolved
// (502, which a re-seal can change), and unconfigured (403) once a re-seal
// says the org no longer has a credential — in the members the agenthost turns
// into guidance; and the first re-seal that carries the credential is used
// from the next request on, with no restart.
func TestRuntime_LinearAPIProxyStartsWhileUnresolved(t *testing.T) {
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
	manifest := func(e domain.SourceEntry) domain.SourceManifest { return domain.SourceManifest{"linear": e} }

	relay(&credbundle.Bundle{Sources: manifest(domain.SourceEntry{State: domain.SourceUnresolved, Reason: domain.SourceReasonTokenEndpointUnavailable})})
	var res sidecarproto.StartProxiesResult
	if err := orch.Call(ctx, sidecarproto.KindStartProxies, sidecarproto.StartProxiesBody{HostVethIP: "127.0.0.1"}, &res); err != nil {
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
			Source string `json:"source"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode != wantStatus || body.Reason != wantReason || body.Source != "linear" {
			t.Errorf("miss = (%d, %q, %q), want (%d, %q, linear)", resp.StatusCode, body.Reason, body.Source, wantStatus, wantReason)
		}
		select {
		case got := <-gotAuth:
			t.Errorf("a request with no credential reached upstream with Authorization %q", got)
		default:
		}
	}

	expectMiss(http.StatusBadGateway, credbundle.MissSourceUnresolved)

	relay(&credbundle.Bundle{Sources: manifest(domain.SourceEntry{State: domain.SourceUnconfigured})})
	expectMiss(http.StatusForbidden, credbundle.MissSourceUnconfigured)

	relay(&credbundle.Bundle{
		Linear:  &credbundle.LinearCreds{AuthMethod: string(linear.AuthMethodAPIKey), APIKey: "lin_api_REALKEY"},
		Sources: manifest(domain.SourceEntry{State: domain.SourceSealed}),
	})
	client := linear.NewClient(linear.ProxyPlaceholder(res.LinearAPIURL, res.LinearAPIToken))
	if _, err := client.Viewer(ctx); err != nil {
		t.Fatalf("Viewer after the re-seal: %v", err)
	}
	if got := <-gotAuth; got != "lin_api_REALKEY" {
		t.Errorf("upstream Authorization after the re-seal = %q, want the key", got)
	}
}

// TestRuntime_LinearAuthHeaderNamesTheMiss pins what the Linear auth source
// answers for each bundle it cannot use: the manifest's state for Linear, an
// expired app token as unresolved — the retry-later answer, rather than
// forwarded for Linear to refuse as if the install were gone — and an
// unnamed miss for a bundle whose credential disagrees with its manifest.
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
	sealed := domain.SourceManifest{"linear": {State: domain.SourceSealed}}
	missState := func(err error) domain.SourceState {
		var miss *credbundle.SourceMissError
		if !errors.As(err, &miss) || miss.Kind != "linear" {
			return ""
		}
		return miss.State
	}

	if _, err := rt.linearAuthHeader(ctx); !errors.Is(err, credbundle.ErrNoBundle) {
		t.Errorf("no bundle: error %v, want ErrNoBundle", err)
	}
	for _, state := range []domain.SourceState{domain.SourceUnresolved, domain.SourceUnconfigured, domain.SourceDisabled} {
		hold(&credbundle.Bundle{Sources: domain.SourceManifest{"linear": {State: state}}})
		if _, err := rt.linearAuthHeader(ctx); missState(err) != state {
			t.Errorf("%s: error %v, want a %s Linear miss", state, err, state)
		}
	}
	hold(&credbundle.Bundle{})
	if _, err := rt.linearAuthHeader(ctx); missState(err) != domain.SourceUnconfigured {
		t.Errorf("no manifest entry: error %v, want an unconfigured Linear miss", err)
	}

	for _, tc := range []struct {
		name string
		lc   *credbundle.LinearCreds
	}{
		{"no linear half", nil},
		{"empty api key", &credbundle.LinearCreds{AuthMethod: string(linear.AuthMethodAPIKey)}},
		{"empty access token", &credbundle.LinearCreds{AuthMethod: string(linear.AuthMethodAppInstall)}},
		{"unknown auth method", &credbundle.LinearCreds{AuthMethod: "connect_oauth", AccessToken: "x"}},
	} {
		hold(&credbundle.Bundle{Linear: tc.lc, Sources: sealed})
		if v, err := rt.linearAuthHeader(ctx); err == nil || credbundle.MissReason(err) != credbundle.MissOther {
			t.Errorf("%s: (%q, %v), want an unnamed miss", tc.name, v, err)
		}
	}

	past := time.Now().Add(-time.Hour)
	hold(&credbundle.Bundle{
		Linear: &credbundle.LinearCreds{
			AuthMethod:  string(linear.AuthMethodAppInstall),
			AccessToken: "lin_oauth_STALE",
			ExpiresUnix: past.Unix(),
		},
		Sources: domain.SourceManifest{"linear": {State: domain.SourceSealed, ExpiresAt: &past}},
	})
	if v, err := rt.linearAuthHeader(ctx); missState(err) != domain.SourceUnresolved {
		t.Errorf("expired app token = (%q, %v), want an unresolved Linear miss", v, err)
	}
	if e := rt.Sources()["linear"]; e.State != domain.SourceUnresolved || e.Reason != domain.SourceReasonTokenExpired {
		t.Errorf("held manifest for an expired token = %+v, want unresolved/token_expired", e)
	}
}

// TestRuntime_ProxiesFollowTheManifest pins decision 6: the sidecar starts a
// Jira and a Linear proxy for exactly the sources the bundle's manifest names
// sealed or unresolved, with nothing in the request saying which source
// triggered the run — so a GitHub-triggered run in an org with Jira and
// Linear reaches both.
func TestRuntime_ProxiesFollowTheManifest(t *testing.T) {
	jiraSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer jiraSite.Close()
	jiraCreds := &credbundle.JiraCreds{URL: jiraSite.URL, AuthMethod: "datacenter", PAT: "dc-pat"}

	for _, tc := range []struct {
		name              string
		bundle            *credbundle.Bundle
		wantJira, wantLin bool
	}{
		{
			name: "both sealed",
			bundle: &credbundle.Bundle{Jira: jiraCreds, Linear: &credbundle.LinearCreds{AuthMethod: "api_key", APIKey: "k"}, Sources: domain.SourceManifest{
				"github": {State: domain.SourceSealed}, "jira": {State: domain.SourceSealed}, "linear": {State: domain.SourceSealed},
			}},
			wantJira: true, wantLin: true,
		},
		{
			name: "linear unresolved",
			bundle: &credbundle.Bundle{Sources: domain.SourceManifest{
				"jira": {State: domain.SourceUnconfigured}, "linear": {State: domain.SourceUnresolved},
			}},
			wantLin: true,
		},
		{
			name: "turned off and unconfigured",
			bundle: &credbundle.Bundle{Sources: domain.SourceManifest{
				"jira": {State: domain.SourceDisabled}, "linear": {State: domain.SourceUnconfigured},
			}},
		},
		{
			name:   "no manifest",
			bundle: &credbundle.Bundle{Jira: jiraCreds},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch, rt := dialRuntime(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tc.bundle.LLM = map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"}
			if err := orch.Call(ctx, sidecarproto.KindSealedBundle, sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, tc.bundle)}, nil); err != nil {
				t.Fatalf("relay bundle: %v", err)
			}
			var res sidecarproto.StartProxiesResult
			if err := orch.Call(ctx, sidecarproto.KindStartProxies, sidecarproto.StartProxiesBody{HostVethIP: "127.0.0.1"}, &res); err != nil {
				t.Fatalf("start proxies: %v", err)
			}
			if got := res.JiraAPIURL != ""; got != tc.wantJira {
				t.Errorf("jira proxy started = %v, want %v", got, tc.wantJira)
			}
			if got := res.LinearAPIURL != ""; got != tc.wantLin {
				t.Errorf("linear proxy started = %v, want %v", got, tc.wantLin)
			}
		})
	}
}
