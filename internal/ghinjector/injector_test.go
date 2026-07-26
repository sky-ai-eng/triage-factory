package ghinjector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
