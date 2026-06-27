package gitproxy_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
)

// basicAuth builds the "Basic base64(user:pass)" header value a git
// client emits for a userinfo'd remote URL or an http.<url>.extraHeader.
// The proxy validates only the password (the per-run token); the
// username is conventional.
func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// startProxyWithIncomingToken boots a proxy that gates every request on
// incomingToken (the per-run secret), returning the Server and its URL.
func startProxyWithIncomingToken(t *testing.T, ts gitproxy.TokenSource, upstream, incomingToken string) (*gitproxy.Server, string) {
	t.Helper()
	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource:   ts,
		Upstream:      upstream,
		IncomingToken: incomingToken,
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

// TestProxyIncomingTokenRejectsCrossRunToken is the centerpiece
// cross-tenant assertion (mirrors the LLM proxy's per-run-auth test):
// run A's proxy holds its own IncomingToken; a request bearing some
// *other* value — what a sibling run B would present over the shared
// host namespace, or a missing/empty credential — gets 401 and never
// reaches the upstream credential pipeline.
func TestProxyIncomingTokenRejectsCrossRunToken(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	ts := &constantTokenSource{value: "ghs_REAL", expiresAt: time.Now().Add(time.Hour)}
	const runAToken = "run-a-per-run-secret"
	_, proxyURL := startProxyWithIncomingToken(t, ts.source, upstream.URL, runAToken)

	cases := []struct {
		name string
		set  func(*http.Request)
	}{
		{"siblings_token", func(r *http.Request) { r.Header.Set("Authorization", basicAuth("x-run", "run-b-per-run-secret")) }},
		{"empty_password", func(r *http.Request) { r.Header.Set("Authorization", basicAuth("x-run", "")) }},
		{"no_auth_header", func(r *http.Request) {}},
		{"bearer_not_basic", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+runAToken) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", proxyURL+"/owner/repo.git/info/refs", nil)
			c.set(req)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("roundtrip: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 for %s", resp.StatusCode, c.name)
			}
		})
	}
	if hits := rec.hits.Load(); hits != 0 {
		t.Fatalf("upstream reached %d time(s) by unauthorized requests; want 0", hits)
	}
	if mints := ts.mints.Load(); mints != 0 {
		t.Errorf("TokenSource invoked %d time(s) for rejected requests; the gate must short-circuit before minting", mints)
	}
}

// TestProxyIncomingTokenAcceptsRunToken pins the accept side: the exact
// per-run token is allowed through, the inbound credential is replaced
// with the resolved Basic x-access-token, and the real token (not the
// per-run secret) is what reaches the upstream.
func TestProxyIncomingTokenAcceptsRunToken(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	ts := &constantTokenSource{value: "ghs_REAL", expiresAt: time.Now().Add(time.Hour)}
	const runToken = "the-correct-per-run-secret"
	_, proxyURL := startProxyWithIncomingToken(t, ts.source, upstream.URL, runToken)

	req, _ := http.NewRequest("GET", proxyURL+"/owner/repo.git/info/refs", nil)
	req.Header.Set("Authorization", basicAuth("x-run", runToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for the correct per-run token", resp.StatusCode)
	}

	_, _, auth, _, _ := rec.snapshot()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_REAL"))
	if auth != want {
		t.Errorf("upstream Authorization = %q, want %q (proxy must replace the per-run token with the real credential)", auth, want)
	}
	// The per-run secret must not leak upstream.
	if got := base64.StdEncoding.EncodeToString([]byte("x-run:" + runToken)); auth == "Basic "+got {
		t.Errorf("upstream saw the per-run secret; it must be replaced host-side, not forwarded")
	}
}

// TestProxyAppAndPATTokensBothForward pins App-or-PAT parity at the
// proxy: an App-installation token (real expiry) and an org PAT (zero
// expiry) both inject as the documented Basic x-access-token credential.
// The proxy is source-agnostic — both TokenFor tiers flow through one
// code path with no proxy change.
func TestProxyAppAndPATTokensBothForward(t *testing.T) {
	cases := []struct {
		name  string
		token gitproxy.Token
	}{
		{"app_installation_token", gitproxy.Token{Value: "ghs_APP", ExpiresAt: time.Now().Add(time.Hour)}},
		{"org_pat_zero_expiry", gitproxy.Token{Value: "ghp_PAT", ExpiresAt: time.Time{}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &fakeUpstreamRecord{}
			upstream := fakeGitHub(rec)
			defer upstream.Close()

			source := func(ctx context.Context, owner, repo string) (gitproxy.Token, error) { return c.token, nil }
			_, proxyURL := startProxy(t, source, upstream.URL)

			req, _ := http.NewRequest("GET", proxyURL+"/owner/repo.git/info/refs", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("roundtrip: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			_, _, auth, _, _ := rec.snapshot()
			want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+c.token.Value))
			if auth != want {
				t.Errorf("upstream Authorization = %q, want %q", auth, want)
			}
		})
	}
}

// TestProxyZeroExpiryPATNeverRefreshes is the no-refresh-storm guard: a
// PAT source (zero ExpiresAt) is minted exactly once and reused for
// every subsequent request, even after the clock advances far past any
// App-token TTL. Without the zero-expiry special-case the cache would
// treat time.Time{} as already-expired and re-mint on every request —
// hammering the secret store for a credential that never rotates.
func TestProxyZeroExpiryPATNeverRefreshes(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	ts := &constantTokenSource{value: "ghp_PAT", expiresAt: time.Time{}}

	srv, proxyURL := startProxy(t, ts.source, upstream.URL)

	// Injected clock the test advances between requests.
	now := time.Now()
	srv.SetNowForTest(func() time.Time { return now })

	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("GET", proxyURL+"/owner/repo.git/info/refs", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (PAT must never go stale)", i, resp.StatusCode)
		}
		// Jump well past an App token's 1h TTL between requests — a
		// non-zero-expiry token would have re-minted by now.
		now = now.Add(2 * time.Hour)
	}

	if mints := ts.mints.Load(); mints != 1 {
		t.Errorf("TokenSource invoked %d time(s); want exactly 1 (zero-expiry PAT must be cached, never refreshed)", mints)
	}
	if got := srv.MintCount(); got != 1 {
		t.Errorf("MintCount = %d, want 1", got)
	}
}
