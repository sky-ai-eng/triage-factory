package ghinjector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newRoutedInjector starts an injector whose listener also fronts gitHandler,
// returning a client that trusts the per-run cert and the bound host:port.
func newRoutedInjector(t *testing.T, upstream, incoming string, gitHandler http.Handler) (*http.Client, string) {
	t.Helper()
	cert, certPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	srv, err := New(Config{
		Upstream:      upstream,
		IncomingToken: incoming,
		Cert:          cert,
		GitHandler:    gitHandler,
		TokenSource:   func(context.Context) (string, error) { return "ghs_realtoken", nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append cert PEM to pool failed")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}, addr
}

// TestInjector_RoutesGitPathsToGitHandler is the shared-front-door contract: one
// listener at the address GH_HOST names answers both halves of a fake-GHE
// origin. Git smart-HTTP reaches the git handler with its own credential intact
// and never touches the API credential path; the API paths keep behaving exactly
// as they do without a git handler wired.
func TestInjector_RoutesGitPathsToGitHandler(t *testing.T) {
	var gitSaw struct {
		path   string
		method string
		auth   string
		hits   int
	}
	gitHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gitSaw.path, gitSaw.method, gitSaw.auth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		gitSaw.hits++
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = w.Write([]byte("001e# service=git-upload-pack\n"))
	})

	apiHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHits++
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	client, host := newRoutedInjector(t, upstream.URL, "placeholder-xyz", gitHandler)

	// Git advertise, carrying the GIT proxy's Basic password — not the gh
	// placeholder. It must reach the git handler unaltered, which is the whole
	// reason routing happens before the API caller-auth gate.
	req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/octo/repo.git/info/refs?service=git-upload-pack", nil)
	req.SetBasicAuth("x-run", "git-per-run-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("git advertise: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("git advertise status = %d, want 200 (body %q)", resp.StatusCode, body)
	}
	if gitSaw.hits != 1 {
		t.Fatalf("git handler hits = %d, want 1", gitSaw.hits)
	}
	if gitSaw.path != "/octo/repo.git/info/refs" || gitSaw.method != http.MethodGet {
		t.Errorf("git handler saw %s %s, want GET /octo/repo.git/info/refs", gitSaw.method, gitSaw.path)
	}
	if wantAuth := "Basic eC1ydW46Z2l0LXBlci1ydW4tdG9rZW4="; gitSaw.auth != wantAuth {
		t.Errorf("git handler saw Authorization %q, want the caller's own %q", gitSaw.auth, wantAuth)
	}
	if apiHits != 0 {
		t.Errorf("API upstream saw %d requests; a git path must never reach the API credential path", apiHits)
	}

	// The API half of the same listener is unchanged: the gh placeholder is
	// required, and the request forwards upstream.
	req, _ = http.NewRequest(http.MethodGet, "https://"+host+"/api/v3/repos/octo/repo", nil)
	req.Header.Set("Authorization", "token placeholder-xyz")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("api request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("api status = %d, want 200", resp.StatusCode)
	}
	if apiHits != 1 {
		t.Errorf("API upstream hits = %d, want 1", apiHits)
	}
	if gitSaw.hits != 1 {
		t.Errorf("git handler hits = %d after the API call, want 1", gitSaw.hits)
	}

	// The API prefixes win over a git-shaped tail: /api/v3/… is this listener's
	// other half by construction, and no real GHE serves a repository at
	// "api/v3".
	req, _ = http.NewRequest(http.MethodGet, "https://"+host+"/api/v3/octo/repo/info/refs", nil)
	req.Header.Set("Authorization", "token placeholder-xyz")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("api path with a git-shaped tail: %v", err)
	}
	_ = resp.Body.Close()
	if apiHits != 2 {
		t.Errorf("API upstream hits = %d, want 2; an /api/v3 path must stay on the API half whatever its tail looks like", apiHits)
	}
	if gitSaw.hits != 1 {
		t.Errorf("git handler hits = %d, want 1", gitSaw.hits)
	}

	// A git-shaped path the git gate would refuse still routes to the gate, so a
	// malformed one is never forwarded upstream under the real API token.
	req, _ = http.NewRequest(http.MethodPost, "https://"+host+"/oc%2fto/repo/git-receive-pack", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("malformed git path: %v", err)
	}
	_ = resp.Body.Close()
	if gitSaw.hits != 2 {
		t.Errorf("git handler hits = %d, want 2 (the malformed git path is the gate's to refuse)", gitSaw.hits)
	}
	if apiHits != 2 {
		t.Errorf("API upstream hits = %d, want 2 (unchanged by the malformed git path)", apiHits)
	}
}

// TestInjector_NoGitHandlerLeavesAPIPathUnchanged pins the degradation: with no
// git handler wired (local mode, and every pre-existing caller), a git-shaped
// path is just another API path — the listener behaves exactly as before.
func TestInjector_NoGitHandlerLeavesAPIPathUnchanged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	client, host := newRoutedInjector(t, upstream.URL, "placeholder-xyz", nil)

	// Unauthenticated: with no git handler this is an API request, so the
	// caller-auth gate applies and refuses it.
	req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/octo/repo.git/info/refs", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (no git handler ⇒ the API auth gate owns every path)", resp.StatusCode)
	}
}

// TestInjector_ServePreBoundListener covers the fd-passed path: the run's shared
// origin answers on a listener bound by another process (the cap-broker, which
// holds the privilege to bind 443), handed to a Server that never binds anything
// itself.
func TestInjector_ServePreBoundListener(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cert, certPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	srv, err := New(Config{
		Upstream:    upstream.URL,
		Cert:        cert,
		TokenSource: func(context.Context) (string, error) { return "ghs_realtoken", nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln := mustListen(t, "127.0.0.1:0")
	addr, err := srv.Serve(ln)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	if addr != ln.Addr().String() {
		t.Errorf("Serve returned %q, want the listener's own address %q", addr, ln.Addr().String())
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append cert PEM to pool failed")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := client.Get("https://" + addr + "/api/v3/repos/octo/repo")
	if err != nil {
		t.Fatalf("request over pre-bound listener: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if _, err := srv.Serve(mustListen(t, "127.0.0.1:0")); err == nil {
		t.Error("second Serve succeeded; a Server is per-run and must refuse to serve twice")
	}
}

// mustListen binds a TCP listener the test owns, standing in for the fd the
// cap-broker passes down in production.
func mustListen(t *testing.T, addr string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	return ln
}
