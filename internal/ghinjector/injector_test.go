package ghinjector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
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

// TestParseObservation_ReviewCoordinates asserts a review-post observation
// carries the PR number from the path and the review id/state from the response.
//
// Driven directly rather than through the proxy, because the refusal policy now
// stops a review post before it is forwarded (see gate_test.go) and no response
// to parse ever arrives. The parse is kept, and tested, for the same reason the
// observation is: it is what an artifact row would be built from if the policy
// ever admits the shape, and nothing about it is specific to the policy that
// currently does not.
func TestParseObservation_ReviewCoordinates(t *testing.T) {
	reviewJSON := `{"id":9911,"state":"APPROVED","html_url":"https://github.com/octo/repo/pull/7#pullrequestreview-9911"}`

	got, ok := parseObservation("review", "octo", "repo", 7, []byte(reviewJSON))
	if !ok {
		t.Fatal("review response did not parse")
	}
	if got.Kind != "review" || got.Owner != "octo" || got.Repo != "repo" || got.Number != 7 {
		t.Errorf("observation coords = %+v, want octo/repo#7 review", got)
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

// TestParseGraphQLObservation_KeysOnCreateMutationOnly pins the discrimination
// the gh-driven wire tests exercise end to end, over the shapes that would be
// tedious to provoke through gh: only the exact top-level data.createPullRequest
// key counts, and a url that doesn't carry coordinates yields nothing rather
// than a half-formed artifact.
func TestParseGraphQLObservation_KeysOnCreateMutationOnly(t *testing.T) {
	observed := []struct {
		name                string
		body                string
		owner, repo, nodeID string
		number              int
	}{
		{
			name:  "create mutation",
			body:  `{"data":{"createPullRequest":{"pullRequest":{"id":"PR_kw1","url":"https://github.com/octo/repo/pull/42"}}}}`,
			owner: "octo", repo: "repo", number: 42, nodeID: "PR_kw1",
		},
		{
			name:  "GHES host and a repo named pull",
			body:  `{"data":{"createPullRequest":{"pullRequest":{"id":"PR_kw2","url":"https://ghe.corp/octo/pull/pull/7"}}}}`,
			owner: "octo", repo: "pull", number: 7, nodeID: "PR_kw2",
		},
	}
	for _, tc := range observed {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := parseGraphQLObservation([]byte(tc.body))
			if !ok {
				t.Fatalf("no observation parsed from %s", tc.body)
			}
			if m.Kind != "pull_request" || m.Owner != tc.owner || m.Repo != tc.repo ||
				m.Number != tc.number || m.NodeID != tc.nodeID {
				t.Errorf("observation = %+v, want %s/%s#%d node %s", m, tc.owner, tc.repo, tc.number, tc.nodeID)
			}
		})
	}

	ignored := []struct{ name, body string }{
		// A read nests its pullRequest under repository — recording it would
		// attribute a PR the run only looked at.
		{"pr view query", `{"data":{"repository":{"pullRequest":{"id":"PR_kw1","url":"https://github.com/octo/repo/pull/42"}}}}`},
		{"pr list query", `{"data":{"repository":{"pullRequests":{"nodes":[{"id":"PR_kw1","url":"https://github.com/octo/repo/pull/42"}]}}}}`},
		// Other mutations gh performs, none of which created anything.
		{"review mutation", `{"data":{"addPullRequestReview":{"clientMutationId":null}}}`},
		{"ready mutation", `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"id":"PR_kw1"}}}}`},
		{"merge mutation", `{"data":{"mergePullRequest":{"pullRequest":{"id":"PR_kw1"}}}}`},
		// Create shapes with nothing usable in them.
		{"errors only", `{"data":{"createPullRequest":null},"errors":[{"message":"nope"}]}`},
		{"no url", `{"data":{"createPullRequest":{"pullRequest":{"id":"PR_kw1"}}}}`},
		{"url without coordinates", `{"data":{"createPullRequest":{"pullRequest":{"id":"PR_kw1","url":"https://github.com/octo/repo"}}}}`},
		{"non-numeric number", `{"data":{"createPullRequest":{"pullRequest":{"id":"PR_kw1","url":"https://github.com/octo/repo/pull/abc"}}}}`},
		{"not json", `createPullRequest`},
	}
	for _, tc := range ignored {
		t.Run(tc.name, func(t *testing.T) {
			if m, ok := parseGraphQLObservation([]byte(tc.body)); ok {
				t.Errorf("observed %+v, want nothing from %s", m, tc.body)
			}
		})
	}
}

// TestInjector_OversizedBodyReachesAgentIntact is the regression for the
// observation buffer truncating the agent's response. A mutation response larger
// than maxObserveBody must reach the caller byte-for-byte — only the observation
// degrades (the reconciler backstop covers that). Truncating here would corrupt
// gh's JSON parse on large responses.
func TestInjector_OversizedBodyReachesAgentIntact(t *testing.T) {
	// A well-formed PR JSON padded past the buffer cap.
	pad := strings.Repeat("x", maxObserveBody+4096)
	prJSON := `{"number":42,"node_id":"PR_kwABC","html_url":"https://github.com/octo/repo/pull/42",` +
		`"title":"Fix it","draft":false,"head":{"ref":"fix"},"base":{"ref":"main"},"body":"` + pad + `"}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(prJSON))
	}))
	defer upstream.Close()

	var observed int
	observe := func(context.Context, ObservedMutation) { observed++ }
	_, client, host := newInjector(t, upstream.URL, "", observe)

	req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/api/v3/repos/octo/repo/pulls",
		strings.NewReader(`{"title":"Fix it"}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if len(got) != len(prJSON) {
		t.Errorf("caller received %d bytes, want the full %d — the response was truncated", len(got), len(prJSON))
	}
	if string(got) != prJSON {
		t.Error("caller's body differs from the upstream body")
	}
	// The response must still be parseable JSON, which truncation would break.
	var probe map[string]any
	if err := json.Unmarshal(got, &probe); err != nil {
		t.Errorf("delivered body is not valid JSON (truncated?): %v", err)
	}
	if observed != 0 {
		t.Errorf("observations = %d, want 0 (oversized bodies skip observation)", observed)
	}
}

// countingBody reports how much of a response body was actually read.
type countingBody struct {
	r    io.Reader
	read int
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += n
	return n, err
}

func (b *countingBody) Close() error { return nil }

// TestBufferForObservation_DeclaredOversizeReadsNothing pins the short-circuit:
// when the upstream advertises a Content-Length past the cap the outcome is
// already settled, so the body must stream to the agent untouched rather than
// being dragged through a megabyte of doomed buffering first.
func TestBufferForObservation_DeclaredOversizeReadsNothing(t *testing.T) {
	payload := strings.Repeat("z", maxObserveBody+4096)
	body := &countingBody{r: strings.NewReader(payload)}
	resp := &http.Response{ContentLength: int64(len(payload)), Body: body}

	if buf, ok := bufferForObservation(resp); ok || buf != nil {
		t.Errorf("bufferForObservation = (%d bytes, %v), want (nil, false)", len(buf), ok)
	}
	if body.read != 0 {
		t.Errorf("read %d bytes from an over-cap declared body, want 0", body.read)
	}
	if resp.Body != body {
		t.Error("response body was replaced; an unread body must stream on as-is")
	}
	// The agent still gets every byte.
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != payload {
		t.Errorf("delivered %d bytes, want the full %d", len(got), len(payload))
	}
}

// TestBufferForObservation_UnknownLengthStillBuffers is the other side of that
// short-circuit: a chunked response declares -1, which must not be read as
// "under the cap" nor as "over" — it falls through to the read.
func TestBufferForObservation_UnknownLengthStillBuffers(t *testing.T) {
	const payload = `{"number":1}`
	resp := &http.Response{ContentLength: -1, Body: io.NopCloser(strings.NewReader(payload))}

	buf, ok := bufferForObservation(resp)
	if !ok {
		t.Fatal("bufferForObservation skipped a chunked response")
	}
	if string(buf) != payload {
		t.Errorf("buffered %q, want %q", buf, payload)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != payload {
		t.Errorf("re-presented body = %q, want %q", got, payload)
	}
}

// TestInjector_BodyAtExactlyCapStillObserves guards the off-by-one in the
// oversize probe: a body of exactly maxObserveBody is buffered and observed, not
// misread as "over the cap".
func TestInjector_BodyAtExactlyCapStillObserves(t *testing.T) {
	head := `{"number":7,"node_id":"PR_x","html_url":"u","title":"t","draft":false,` +
		`"head":{"ref":"h"},"base":{"ref":"b"},"body":"`
	tail := `"}`
	pad := strings.Repeat("y", maxObserveBody-len(head)-len(tail))
	prJSON := head + pad + tail
	if len(prJSON) != maxObserveBody {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(prJSON), maxObserveBody)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(prJSON))
	}))
	defer upstream.Close()

	var observed int
	observe := func(context.Context, ObservedMutation) { observed++ }
	_, client, host := newInjector(t, upstream.URL, "", observe)

	req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/api/v3/repos/octo/repo/pulls",
		strings.NewReader(`{}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if len(got) != len(prJSON) {
		t.Errorf("caller received %d bytes, want %d", len(got), len(prJSON))
	}
	if observed != 1 {
		t.Errorf("observations = %d, want 1 (a body exactly at the cap is still observed)", observed)
	}
}

// TestGraphQLUpstream_ExactHostMatch is the regression for the dotcom detector:
// a substring test routed any host merely CONTAINING api.github.com to
// github.com's GraphQL endpoint, sending a GHES org's queries off its own host.
func TestGraphQLUpstream_ExactHostMatch(t *testing.T) {
	cases := []struct{ base, want string }{
		// Real dotcom.
		{"https://api.github.com", "https://api.github.com/graphql"},
		{"https://api.github.com/", "https://api.github.com/graphql"},
		// Lookalikes that must NOT be treated as dotcom.
		{"https://api.github.com.example.com/api/v3", "https://api.github.com.example.com/api/graphql"},
		{"https://evil-api.github.com.attacker.test/api/v3", "https://evil-api.github.com.attacker.test/api/graphql"},
		{"https://ghe.corp/api/v3?x=api.github.com", "https://ghe.corp/api/v3?x=api.github.com/graphql"},
		// Ordinary GHES.
		{"https://ghe.corp/api/v3", "https://ghe.corp/api/graphql"},
	}
	for _, tc := range cases {
		if got := graphqlUpstream(tc.base); got != tc.want {
			t.Errorf("graphqlUpstream(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

// newInjectorWithWrites is newInjector with the write-audit callback wired too
// (the observation callback stays available so a test can assert both fire).
func newInjectorWithWrites(t *testing.T, upstream string,
	observe func(context.Context, ObservedMutation),
	observeWrite func(context.Context, ObservedWrite)) (*http.Client, string) {
	t.Helper()
	cert, certPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	srv, err := New(Config{
		Upstream:     upstream,
		Cert:         cert,
		Observe:      observe,
		ObserveWrite: observeWrite,
		TokenSource:  func(context.Context) (string, error) { return "ghs_realtoken", nil },
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

// TestInjector_ObservesRESTWrites pins the write-audit surface: every mutating
// REST method is reported with its path and the upstream's status — a refused
// write as loudly as a successful one — while reads and GraphQL are not.
// GraphQL's exclusion is the load-bearing one: a porcelain mutation and a
// `gh pr view` are the same POST /api/graphql, so recording it would label
// every read a write.
func TestInjector_ObservesRESTWrites(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		path    string
		body    string
		status  int
		wantHit bool
	}{
		{"patch a PR", http.MethodPatch, "/api/v3/repos/octo/repo/pulls/7", "", http.StatusOK, true},
		{"off-scope patch masked as 404", http.MethodPatch, "/api/v3/repos/other/repo/pulls/7", "", http.StatusNotFound, true},
		// An ungated write under /pulls/{n}, so the audit surface is exercised
		// without tripping the refusal policy — which owns the merge shape and
		// pins it in gate_test.go.
		{"update a PR branch", http.MethodPut, "/api/v3/repos/octo/repo/pulls/7/update-branch", "", http.StatusAccepted, true},
		{"delete a comment", http.MethodDelete, "/api/v3/repos/octo/repo/issues/comments/5", "", http.StatusNoContent, true},
		{"post a comment", http.MethodPost, "/api/v3/repos/octo/repo/issues/7/comments", "", http.StatusCreated, true},
		{"read a PR", http.MethodGet, "/api/v3/repos/octo/repo/pulls/7", "", http.StatusOK, false},
		{"graphql", http.MethodPost, "/api/graphql", `{"query":"query{viewer{login}}"}`, http.StatusOK, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer upstream.Close()

			writes := make(chan ObservedWrite, 4)
			client, host := newInjectorWithWrites(t, upstream.URL, nil,
				func(_ context.Context, w ObservedWrite) { writes <- w })

			body := tc.body
			if body == "" {
				body = `{"base":"main"}`
			}
			req, _ := http.NewRequest(tc.method, "https://"+host+tc.path, strings.NewReader(body))
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("caller saw %d, want the upstream's %d", resp.StatusCode, tc.status)
			}

			select {
			case got := <-writes:
				if !tc.wantHit {
					t.Fatalf("recorded a write for %s %s, want none", tc.method, tc.path)
				}
				if got.Method != tc.method || got.Status != tc.status {
					t.Errorf("recorded %+v, want method %s status %d", got, tc.method, tc.status)
				}
				if !strings.HasSuffix(tc.path, got.Path) {
					t.Errorf("recorded path %q, want the forwarded form of %q", got.Path, tc.path)
				}
			case <-time.After(time.Second):
				if tc.wantHit {
					t.Fatalf("no write recorded for %s %s", tc.method, tc.path)
				}
			}
		})
	}
}

// TestInjector_WriteAuditCoexistsWithObservation pins the intended overlap: a
// REST PR create produces BOTH the artifact observation (the object exists) and
// the write-audit row (the attempt happened). They answer different questions,
// exactly as branch_pushed and branch_push_failed do — and the request body
// still reaches the upstream byte-for-byte, since neither path reads it.
func TestInjector_WriteAuditCoexistsWithObservation(t *testing.T) {
	const body = `{"title":"Fix it","base":"main"}`
	gotBody := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody <- string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":42,"node_id":"PR_x","html_url":"https://github.com/octo/repo/pull/42"}`))
	}))
	defer upstream.Close()

	muts := make(chan ObservedMutation, 2)
	writes := make(chan ObservedWrite, 2)
	client, host := newInjectorWithWrites(t, upstream.URL,
		func(_ context.Context, m ObservedMutation) { muts <- m },
		func(_ context.Context, w ObservedWrite) { writes <- w })

	req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/api/v3/repos/octo/repo/pulls", strings.NewReader(body))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case b := <-gotBody:
		if b != body {
			t.Errorf("upstream saw body %q, want the caller's %q verbatim (requests are never inspected)", b, body)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream never received the request")
	}
	select {
	case m := <-muts:
		if m.Number != 42 {
			t.Errorf("observation = %+v, want the created PR", m)
		}
	case <-time.After(time.Second):
		t.Fatal("no artifact observation for a successful PR create")
	}
	select {
	case w := <-writes:
		if w.Method != http.MethodPost || w.Status != http.StatusCreated {
			t.Errorf("write audit = %+v, want the POST and its 201", w)
		}
	case <-time.After(time.Second):
		t.Fatal("no write audit for a successful PR create")
	}
}

// TestInjector_WriteAuditReadsCreatedObject pins the incident fix's injector
// half: for a shape the shared classifier calls a create, the response body is
// parsed for the new object's id and link so the audit row can name what was
// made — while the caller still receives that body whole, and the request still
// reaches the upstream untouched.
func TestInjector_WriteAuditReadsCreatedObject(t *testing.T) {
	const replyURL = "https://github.com/acme/widgets/pull/841#discussion_r777"
	const respBody = `{"id":777,"in_reply_to_id":555,"html_url":"` + replyURL + `"}`
	const reqBody = `{"body":"good catch"}`
	gotReq := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotReq <- string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(respBody))
	}))
	defer upstream.Close()

	writes := make(chan ObservedWrite, 2)
	client, host := newInjectorWithWrites(t, upstream.URL, nil,
		func(_ context.Context, w ObservedWrite) { writes <- w })

	req, _ := http.NewRequest(http.MethodPost,
		"https://"+host+"/api/v3/repos/acme/widgets/pulls/841/comments/555/replies",
		strings.NewReader(reqBody))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != respBody {
		t.Errorf("caller saw body %q, want the upstream's %q — buffering must be invisible", body, respBody)
	}
	select {
	case b := <-gotReq:
		if b != reqBody {
			t.Errorf("upstream saw request body %q, want %q verbatim (requests are never inspected)", b, reqBody)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream never received the request")
	}

	select {
	case w := <-writes:
		if w.ExternalID != "777" || w.URL != replyURL {
			t.Errorf("write audit = %+v, want the created reply's id and discussion link", w)
		}
	case <-time.After(time.Second):
		t.Fatal("no write audit for a posted review-thread reply")
	}
}

// TestInjector_WriteAuditSkipsBodyOffTheCreatePath pins the cost boundary: a
// shape that creates nothing — an edit, a merge, a refused create — is fully
// described by its path, so its body is never buffered and the audit carries no
// object coordinates. A declared length past the cap proves it: buffering would
// have to skip it, and the row is unaffected either way.
func TestInjector_WriteAuditSkipsBodyOffTheCreatePath(t *testing.T) {
	huge := strings.Repeat("x", maxObserveBody+2048)
	cases := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"an edit names its object in the path", http.MethodPatch, "/api/v3/repos/acme/widgets/pulls/841", http.StatusOK},
		{"a refused create creates nothing", http.MethodPost, "/api/v3/repos/acme/widgets/issues/7/comments", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"id":777,"pad":"` + huge + `"}`))
			}))
			defer upstream.Close()

			writes := make(chan ObservedWrite, 2)
			client, host := newInjectorWithWrites(t, upstream.URL, nil,
				func(_ context.Context, w ObservedWrite) { writes <- w })

			req, _ := http.NewRequest(tc.method, "https://"+host+tc.path, nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			got, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if len(got) < maxObserveBody {
				t.Errorf("caller received %d bytes, want the whole oversized body", len(got))
			}

			select {
			case w := <-writes:
				if w.Status != tc.status {
					t.Errorf("write audit = %+v, want status %d", w, tc.status)
				}
				if w.ExternalID != "" || w.URL != "" {
					t.Errorf("write audit = %+v, want no object coordinates off the create path", w)
				}
			case <-time.After(time.Second):
				t.Fatal("no write audit recorded")
			}
		})
	}
}

// graphQLEnvelope builds the request body gh sends: the document plus its
// variables.
func graphQLEnvelope(t *testing.T, query string, variables map[string]any) string {
	t.Helper()
	body := map[string]any{"query": query}
	if variables != nil {
		body["variables"] = variables
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal graphql envelope: %v", err)
	}
	return string(raw)
}

// graphQLUpstream answers a GraphQL POST with the given body and records what
// it received, so a test can assert the request crossed the hop unaltered.
func graphQLUpstream(t *testing.T, status int, response string) (*httptest.Server, <-chan string) {
	t.Helper()
	seen := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

// TestInjector_GraphQLMutationIsAudited is this ticket's core: a mutation
// through the GraphQL endpoint leaves a record naming the act, which it could
// not before — the response says only that a POST succeeded.
func TestInjector_GraphQLMutationIsAudited(t *testing.T) {
	const commentURL = "https://github.com/acme/widgets/pull/841#issuecomment-7"
	upstream, seen := graphQLUpstream(t, http.StatusOK,
		`{"data":{"addComment":{"commentEdge":{"node":{"url":"`+commentURL+`"}}}}}`)

	writes := make(chan ObservedWrite, 2)
	client, host := newInjectorWithWrites(t, upstream.URL, nil,
		func(_ context.Context, w ObservedWrite) { writes <- w })

	body := graphQLEnvelope(t,
		`mutation CommentCreate($input:AddCommentInput!){addComment(input:$input){commentEdge{node{url}}}}`,
		map[string]any{"input": map[string]any{"body": "good catch", "subjectId": "PR_kwAudit"}})
	req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/api/graphql", strings.NewReader(body))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case got := <-seen:
		if got != body {
			t.Errorf("upstream saw %q, want the caller's document verbatim — buffering must not alter it", got)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream never received the request")
	}

	select {
	case w := <-writes:
		if w.GraphQL == nil {
			t.Fatalf("write audit = %+v, want the request's own facts attached", w)
		}
		if got := w.GraphQL.Mutation(); got != "addComment" {
			t.Errorf("mutation = %q, want addComment", got)
		}
		if w.GraphQL.Subject != "PR_kwAudit" {
			t.Errorf("subject = %q, want the node id from the variables", w.GraphQL.Subject)
		}
		if w.URL != commentURL {
			t.Errorf("url = %q, want the created comment's link from the response", w.URL)
		}
		if w.Errored || !w.Succeeded() {
			t.Errorf("write audit = %+v, want a clean success", w)
		}
	case <-time.After(time.Second):
		t.Fatal("no write audit for a GraphQL mutation")
	}

	if extra := drainWrites(writes); extra != 0 {
		t.Errorf("%d extra audit records; one request may leave exactly one", extra)
	}
}

// TestInjector_GraphQLReadIsNotAudited is the other half of the acceptance, and
// the one that keeps the log usable: reads are nearly all of this endpoint's
// traffic and none of them may leave a row.
func TestInjector_GraphQLReadIsNotAudited(t *testing.T) {
	upstream, _ := graphQLUpstream(t, http.StatusOK, `{"data":{"repository":{"pullRequest":{"id":"PR_x"}}}}`)

	writes := make(chan ObservedWrite, 2)
	client, host := newInjectorWithWrites(t, upstream.URL, nil,
		func(_ context.Context, w ObservedWrite) { writes <- w })

	body := graphQLEnvelope(t,
		`query PullRequestByNumber($n:Int!){repository(owner:"acme",name:"widgets"){pullRequest(number:$n){id url}}}`,
		map[string]any{"n": 841})
	req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/api/graphql", strings.NewReader(body))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case w := <-writes:
		t.Errorf("a read left an audit row: %+v", w)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestInjector_GraphQLErrorsRecordAnAttempt: the endpoint refuses a mutation
// with a 200 and an errors array, so the status line alone would report a close
// that never happened as one that did.
func TestInjector_GraphQLErrorsRecordAnAttempt(t *testing.T) {
	upstream, _ := graphQLUpstream(t, http.StatusOK,
		`{"data":{"closePullRequest":null},"errors":[{"message":"Pull request is already closed"}]}`)

	writes := make(chan ObservedWrite, 2)
	client, host := newInjectorWithWrites(t, upstream.URL, nil,
		func(_ context.Context, w ObservedWrite) { writes <- w })

	body := graphQLEnvelope(t,
		`mutation($input:ClosePullRequestInput!){closePullRequest(input:$input){clientMutationId}}`,
		map[string]any{"input": map[string]any{"pullRequestId": "PR_kwRefused"}})
	req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/api/graphql", strings.NewReader(body))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case w := <-writes:
		if !w.Errored {
			t.Errorf("write audit = %+v, want the errors array recorded as a failure", w)
		}
		if w.Succeeded() {
			t.Error("a refused close reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("no write audit for a refused GraphQL mutation")
	}
}

// TestInjector_GraphQLGetBodyIsReplayable pins what the buffered body owes the
// transport: an in-memory body may be replayed, so a request the injector
// buffered is one net/http can safely retry rather than failing the agent's
// call on a stale connection.
func TestInjector_GraphQLGetBodyIsReplayable(t *testing.T) {
	const body = `{"query":"mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){clientMutationId}}"}`
	req, err := http.NewRequest(http.MethodPost, "https://example.test/api/graphql", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// A server-received request carries no GetBody; the capture is what supplies
	// one, so clear it to model the real inbound shape.
	req.GetBody = nil

	buffered, unread := bufferRequest(req)
	if unread != "" || string(buffered) != body {
		t.Fatalf("bufferRequest returned %q (unread=%q), want the whole body", buffered, unread)
	}
	if req.GetBody == nil {
		t.Fatal("GetBody was not set for a buffered body")
	}
	replay, err := req.GetBody()
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}
	first, _ := io.ReadAll(replay)
	rest, _ := io.ReadAll(req.Body)
	if string(first) != body || string(rest) != body {
		t.Errorf("replay = %q, body = %q, want both to deliver the request whole", first, rest)
	}
}

// TestInjector_RESTWritesAreUnaffectedByTheGraphQLCapture: the capture is
// scoped to one endpoint, and a REST write must still be audited from its
// response record alone with its request unread.
func TestInjector_RESTWritesAreUnaffectedByTheGraphQLCapture(t *testing.T) {
	const reqBody = `{"body":"a REST comment"}`
	gotReq := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotReq <- string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7,"html_url":"https://github.com/acme/widgets/issues/1#issuecomment-7"}`))
	}))
	defer upstream.Close()

	writes := make(chan ObservedWrite, 2)
	client, host := newInjectorWithWrites(t, upstream.URL, nil,
		func(_ context.Context, w ObservedWrite) { writes <- w })

	req, _ := http.NewRequest(http.MethodPost,
		"https://"+host+"/api/v3/repos/acme/widgets/issues/1/comments", strings.NewReader(reqBody))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case b := <-gotReq:
		if b != reqBody {
			t.Errorf("upstream saw %q, want %q verbatim", b, reqBody)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream never received the request")
	}
	select {
	case w := <-writes:
		if w.GraphQL != nil {
			t.Errorf("write audit = %+v, want no GraphQL facts on a REST write", w)
		}
		if w.ExternalID != "7" {
			t.Errorf("write audit = %+v, want the created comment's id", w)
		}
	case <-time.After(time.Second):
		t.Fatal("no write audit for a REST create")
	}
}

// drainWrites reports how many further records are already queued.
func drainWrites(writes <-chan ObservedWrite) int {
	extra := 0
	for {
		select {
		case <-writes:
			extra++
		default:
			return extra
		}
	}
}

// BenchmarkGraphQLCapture measures what the capture costs the traffic that
// pays for it without benefiting: an ordinary read, which is buffered like
// everything else and then rejected by the prescreen without being parsed.
func BenchmarkGraphQLCapture(b *testing.B) {
	body := `{"query":"query PullRequestByNumber($n:Int!){repository(owner:\"acme\",name:\"widgets\"){pullRequest(number:$n){id url title body baseRefName headRefName}}}","variables":{"n":841}}`
	srv := &Server{cfg: Config{ObserveWrite: func(context.Context, ObservedWrite) {}}}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		req, _ := http.NewRequest(http.MethodPost, "https://example.test/api/graphql", strings.NewReader(body))
		if srv.captureGraphQLWrite(req) != nil {
			b.Fatal("a read classified as a write")
		}
	}
}

// BenchmarkGraphQLCaptureMutation is the other end: a mutation pays the parse
// as well, and it is the rarer request by orders of magnitude.
func BenchmarkGraphQLCaptureMutation(b *testing.B) {
	body := `{"query":"mutation CommentCreate($input:AddCommentInput!){addComment(input:$input){commentEdge{node{url}}}}","variables":{"input":{"body":"a review reply of ordinary length, quoting the diff it answers","subjectId":"PR_kwBench"}}}`
	srv := &Server{cfg: Config{ObserveWrite: func(context.Context, ObservedWrite) {}}}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		req, _ := http.NewRequest(http.MethodPost, "https://example.test/api/graphql", strings.NewReader(body))
		if srv.captureGraphQLWrite(req) == nil {
			b.Fatal("a mutation did not classify as a write")
		}
	}
}

// TestInjector_GraphQLUnreadResponseIsNotASuccess: when the response cannot be
// read, the write is recorded as attempted rather than as a success nobody
// observed — and it says which of the two happened, since "the server refused
// it" and "we could not tell" are different admissions.
func TestInjector_GraphQLUnreadResponseIsNotASuccess(t *testing.T) {
	huge := `{"data":{"closePullRequest":{"clientMutationId":"` +
		strings.Repeat("x", maxObserveBody+4096) + `"}}}`
	upstream, _ := graphQLUpstream(t, http.StatusOK, huge)

	writes := make(chan ObservedWrite, 2)
	client, host := newInjectorWithWrites(t, upstream.URL, nil,
		func(_ context.Context, w ObservedWrite) { writes <- w })

	body := graphQLEnvelope(t,
		`mutation($input:ClosePullRequestInput!){closePullRequest(input:$input){clientMutationId}}`,
		map[string]any{"input": map[string]any{"pullRequestId": "PR_kwUnread"}})
	req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/api/graphql", strings.NewReader(body))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if len(got) != len(huge) {
		t.Errorf("caller received %d bytes, want the whole %d-byte response", len(got), len(huge))
	}

	select {
	case w := <-writes:
		if !w.ResponseUnread {
			t.Errorf("audit = %+v, want the unread response recorded as such", w)
		}
		if w.Errored {
			t.Error("an unread response was reported as a server refusal; those are different facts")
		}
		if w.Succeeded() {
			t.Error("a write whose outcome was never read reported success")
		}
		// The act is still named — it came from the request, which was read.
		if w.GraphQL == nil || w.GraphQL.Mutation() != "closePullRequest" {
			t.Errorf("audit = %+v, want the close still named from the request", w)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no audit record for a mutation with an unreadable response")
	}
}

// failingBody delivers a prefix and then fails, modelling a client that dies
// partway through sending its request.
type failingBody struct {
	prefix string
	pos    int
}

func (b *failingBody) Read(p []byte) (int, error) {
	if b.pos < len(b.prefix) {
		n := copy(p, b.prefix[b.pos:])
		b.pos += n
		return n, nil
	}
	return 0, errors.New("connection reset mid-body")
}

func (b *failingBody) Close() error { return nil }

// TestInjector_BufferRequestOnBrokenReadDropsNothing pins the weaker of the two
// promises, which is worth a test precisely because it is weaker. A body whose
// own read fails cannot be forwarded whole by anyone — but nothing this proxy
// already consumed may go missing on top of that, so the prefix has to be
// readable again through the re-presented body.
func TestInjector_BufferRequestOnBrokenReadDropsNothing(t *testing.T) {
	const prefix = `{"query":"mutation($input:MergePullRequestInput!){mergePullRequest`
	req, err := http.NewRequest(http.MethodPost, "https://example.test/api/graphql", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Body = &failingBody{prefix: prefix}
	// Unknown length, as a chunked or interrupted send has — otherwise the
	// declared-length check settles it before any read happens.
	req.ContentLength = -1

	buffered, unread := bufferRequest(req)
	if unread != ghwrite.GraphQLRequestUnread {
		t.Fatalf("unread = %q, want %q — a caller that stopped sending is not a cap this side chose",
			unread, ghwrite.GraphQLRequestUnread)
	}
	if buffered != nil {
		t.Errorf("buffered = %q, want nil — a body that did not arrive names no act", buffered)
	}

	// Everything already consumed is still there to forward. The read fails
	// again after it, which is the failure the caller brought with them.
	got, err := io.ReadAll(req.Body)
	if string(got) != prefix {
		t.Errorf("re-presented body = %q, want the consumed prefix %q back in front", got, prefix)
	}
	if err == nil {
		t.Error("reading past the prefix succeeded; the fixture no longer models a broken stream")
	}
}

// TestInjector_BufferRequestOverCapKeepsTheStrongerPromise is the other arm: no
// read error, so the stitch is exact and the body really does forward whole.
func TestInjector_BufferRequestOverCapKeepsTheStrongerPromise(t *testing.T) {
	body := strings.Repeat("x", maxRequestBody+512)
	req, err := http.NewRequest(http.MethodPost, "https://example.test/api/graphql", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.ContentLength = -1

	buffered, unread := bufferRequest(req)
	if unread != ghwrite.GraphQLOverCap || buffered != nil {
		t.Fatalf("bufferRequest = (%d bytes, unread=%q), want the over-cap refusal", len(buffered), unread)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("re-presented body: %v", err)
	}
	if string(got) != body {
		t.Errorf("re-presented body is %d bytes, want the original %d byte-for-byte", len(got), len(body))
	}
}
