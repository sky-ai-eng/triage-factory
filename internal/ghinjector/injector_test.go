package ghinjector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstreamCapture records what the fake GitHub upstream saw, so a test can
// assert the injector stripped the placeholder and injected the real token and
// mapped the GHE path correctly.
type upstreamCapture struct {
	mu     sync.Mutex
	auth   string
	path   string
	method string
	rawq   string
}

func (c *upstreamCapture) snap() (auth, path, method, rawq string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth, c.path, c.method, c.rawq
}

// newInjector starts an injector in front of upstream and returns a client that
// trusts its per-run cert, plus the GH_HOST value (bound addr). The observe
// callback (may be nil) is invoked for observed mutations.
func newInjector(t *testing.T, upstream, incoming string, observe func(context.Context, ObservedMutation)) (*Server, *http.Client, string) {
	t.Helper()
	cert, certPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	srv, err := New(Config{
		Upstream:      upstream,
		IncomingToken: incoming,
		Cert:          cert,
		Observe:       observe,
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
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	return srv, client, addr
}

// TestInjector_StripsPlaceholderInjectsRealToken is the core placeholder
// discipline: the caller presents the per-run placeholder, the upstream sees the
// real token, and the GHE /api/v3 prefix is stripped for api.github.com-shaped
// bases.
func TestInjector_StripsPlaceholderInjectsRealToken(t *testing.T) {
	cap := &upstreamCapture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.mu.Lock()
		cap.auth, cap.path, cap.method, cap.rawq = r.Header.Get("Authorization"), r.URL.Path, r.Method, r.URL.RawQuery
		cap.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	_, client, host := newInjector(t, upstream.URL, "placeholder-xyz", nil)

	req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/api/v3/repos/octo/repo/pulls/7?per_page=1", nil)
	req.Header.Set("Authorization", "token placeholder-xyz")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through injector: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	auth, path, _, rawq := cap.snap()
	if auth != "token ghs_realtoken" {
		t.Errorf("upstream Authorization = %q, want the injected real token (not the placeholder)", auth)
	}
	if strings.Contains(auth, "placeholder") {
		t.Error("placeholder leaked to the upstream")
	}
	if path != "/repos/octo/repo/pulls/7" {
		t.Errorf("upstream path = %q, want /api/v3 stripped for api.github.com base", path)
	}
	if rawq != "per_page=1" {
		t.Errorf("upstream query = %q, want per_page=1 preserved", rawq)
	}
}

// TestInjector_RejectsWrongPlaceholder is the fail-closed cross-run isolation
// check: a sibling run's (wrong) placeholder never reaches the credential
// pipeline.
func TestInjector_RejectsWrongPlaceholder(t *testing.T) {
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	_, client, host := newInjector(t, upstream.URL, "correct-token", nil)

	req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/api/v3/repos/o/r", nil)
	req.Header.Set("Authorization", "token WRONG")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a wrong placeholder", resp.StatusCode)
	}
	if reached {
		t.Error("an unauthorized request reached the upstream — the placeholder gate failed open")
	}
}

// TestInjector_GraphQLRoutesToSiblingEndpoint pins the /api/graphql → /graphql
// mapping for an api.github.com-shaped base.
func TestInjector_GraphQLRoutesToSiblingEndpoint(t *testing.T) {
	cap := &upstreamCapture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.mu.Lock()
		cap.path, cap.method = r.URL.Path, r.Method
		cap.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// api.github.com base → graphqlUpstream hardcodes api.github.com/graphql, so
	// use a base literally containing "api.github.com" would route off-box; here
	// we assert the derivation via a GHES-shaped base instead.
	ghesBase := upstream.URL + "/api/v3"
	cert, _, _ := GenerateCert("127.0.0.1")
	srv, err := New(Config{Upstream: ghesBase, IncomingToken: "", Cert: cert,
		TokenSource: func(context.Context) (string, error) { return "ghs_x", nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := srv.graphqlURL.Path; got != "/api/graphql" {
		t.Errorf("derived GraphQL path = %q, want /api/graphql for a GHES /api/v3 base", got)
	}
	if got := srv.restURL.Path; got != "/api/v3" {
		t.Errorf("REST base path = %q, want /api/v3 preserved for GHES", got)
	}
}

// TestInjector_ErrorBodyPassesThroughVerbatim is the loser contract: GitHub's
// own 404 body reaches the agent unrewritten (a scope-denied repo looks exactly
// like a missing one).
func TestInjector_ErrorBodyPassesThroughVerbatim(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com"}`))
	}))
	defer upstream.Close()

	_, client, host := newInjector(t, upstream.URL, "", nil)

	req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/api/v3/repos/o/secret", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 passed through", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Not Found") {
		t.Errorf("body = %q, want GitHub's verbatim 404 body", body)
	}
}

// TestInjector_ObservesPRCreate asserts the injector emits a PR-create
// observation with coordinates parsed from the 201 response, while the response
// body still reaches the caller intact.
func TestInjector_ObservesPRCreate(t *testing.T) {
	prJSON := `{"number":42,"node_id":"PR_kwABC","html_url":"https://github.com/octo/repo/pull/42",` +
		`"title":"Fix it","body":"the fix","draft":true,"head":{"ref":"fix/thing"},"base":{"ref":"main"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(prJSON))
	}))
	defer upstream.Close()

	var (
		mu  sync.Mutex
		got []ObservedMutation
	)
	observe := func(_ context.Context, m ObservedMutation) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}
	_, client, host := newInjector(t, upstream.URL, "", observe)

	req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/api/v3/repos/octo/repo/pulls",
		strings.NewReader(`{"title":"Fix it"}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"number":42`) {
		t.Errorf("caller body = %q, want the full PR JSON preserved", body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("observations = %d, want exactly 1", len(got))
	}
	m := got[0]
	if m.Kind != "pull_request" || m.Owner != "octo" || m.Repo != "repo" || m.Number != 42 {
		t.Errorf("observation coords = %+v, want octo/repo#42 pull_request", m)
	}
	if m.NodeID != "PR_kwABC" || m.Head != "fix/thing" || m.Base != "main" || !m.Draft {
		t.Errorf("observation details = %+v, want nodeID/head/base/draft parsed", m)
	}
	if m.URL != "https://github.com/octo/repo/pull/42" || m.Title != "Fix it" {
		t.Errorf("observation url/title = %q / %q", m.URL, m.Title)
	}
}

// TestInjector_ObservesReviewPost asserts a review-post observation carries the
// PR number from the path and the review id/state from the response.
func TestInjector_ObservesReviewPost(t *testing.T) {
	reviewJSON := `{"id":9911,"state":"APPROVED","html_url":"https://github.com/octo/repo/pull/7#pullrequestreview-9911"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(reviewJSON))
	}))
	defer upstream.Close()

	var got *ObservedMutation
	observe := func(_ context.Context, m ObservedMutation) { got = &m }
	_, client, host := newInjector(t, upstream.URL, "", observe)

	req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/api/v3/repos/octo/repo/pulls/7/reviews",
		strings.NewReader(`{"event":"APPROVE"}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	if got == nil {
		t.Fatal("no review observation emitted")
	}
	if got.Kind != "review" || got.Owner != "octo" || got.Repo != "repo" || got.Number != 7 {
		t.Errorf("observation coords = %+v, want octo/repo#7 review", *got)
	}
	if got.ReviewID != 9911 || got.ReviewState != "APPROVED" {
		t.Errorf("review id/state = %d / %q, want 9911 / APPROVED", got.ReviewID, got.ReviewState)
	}
}

// TestInjector_TokenSourceFailureIs502 asserts a credential resolution failure
// surfaces as a 502 (proxy alive, credential pipeline broken) and never forwards
// unauthenticated.
func TestInjector_TokenSourceFailureIs502(t *testing.T) {
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer upstream.Close()

	cert, certPEM, _ := GenerateCert("127.0.0.1")
	srv, err := New(Config{Upstream: upstream.URL, Cert: cert,
		TokenSource: func(context.Context) (string, error) { return "", io.ErrUnexpectedEOF }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr, _ := srv.Start("127.0.0.1:0")
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}

	resp, err := client.Get("https://" + addr + "/api/v3/repos/o/r")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 on credential resolution failure", resp.StatusCode)
	}
	if reached {
		t.Error("forwarded upstream despite no resolvable credential")
	}
}

// TestParseObservation_IgnoresMalformed guards the parser: a non-JSON or
// number-less body yields no observation rather than a bogus artifact.
func TestParseObservation_IgnoresMalformed(t *testing.T) {
	if _, ok := parseObservation("pull_request", "o", "r", 0, []byte("not json")); ok {
		t.Error("parsed an observation from non-JSON PR body")
	}
	if _, ok := parseObservation("pull_request", "o", "r", 0, []byte(`{"title":"x"}`)); ok {
		t.Error("parsed an observation from a PR body with no number")
	}
	// Sanity: a well-formed body still parses.
	if _, ok := parseObservation("review", "o", "r", 3, []byte(`{"id":5,"state":"COMMENTED"}`)); !ok {
		t.Error("failed to parse a well-formed review body")
	}
}
