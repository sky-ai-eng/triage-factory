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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghwrite"
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
// trusts its per-run cert, plus the GH_HOST value (bound addr).
func newInjector(t *testing.T, upstream, incoming string) (*Server, *http.Client, string) {
	t.Helper()
	cert, certPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	srv, err := New(Config{
		Upstream:      upstream,
		IncomingToken: incoming,
		Cert:          cert,
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

	_, client, host := newInjector(t, upstream.URL, "placeholder-xyz")

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

	_, client, host := newInjector(t, upstream.URL, "correct-token")

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

	_, client, host := newInjector(t, upstream.URL, "")

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

// TestInjector_OversizedBodyReachesAgentIntact pins the delivery guarantee: a
// create response past the buffer cap reaches the caller byte-for-byte, and the
// audit is what degrades — its row still lands, without the created object's
// coordinates.
func TestInjector_OversizedBodyReachesAgentIntact(t *testing.T) {
	// A well-formed comment JSON padded past the buffer cap.
	pad := strings.Repeat("x", maxBufferedBody+4096)
	commentJSON := `{"id":777,"html_url":"https://github.com/octo/repo/pull/42#issuecomment-777","body":"` + pad + `"}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(commentJSON))
	}))
	defer upstream.Close()

	writes := make(chan ObservedWrite, 2)
	client, host := newInjectorWithWrites(t, upstream.URL, func(_ context.Context, w ObservedWrite) { writes <- w })

	req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/api/v3/repos/octo/repo/issues/42/comments",
		strings.NewReader(`{"body":"hi"}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if len(got) != len(commentJSON) {
		t.Errorf("caller received %d bytes, want the full %d — the response was truncated", len(got), len(commentJSON))
	}
	if string(got) != commentJSON {
		t.Error("caller's body differs from the upstream body")
	}
	// The response must still be parseable JSON, which truncation would break.
	var probe map[string]any
	if err := json.Unmarshal(got, &probe); err != nil {
		t.Errorf("delivered body is not valid JSON (truncated?): %v", err)
	}
	select {
	case w := <-writes:
		if w.Status != http.StatusCreated || w.ExternalID != "" || w.URL != "" {
			t.Errorf("write audit = %+v, want the 201 recorded with no created-object coordinates (body over the cap)", w)
		}
	case <-time.After(time.Second):
		t.Fatal("no write audit for the over-cap create")
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

// TestBufferBody_DeclaredOversizeReadsNothing pins the short-circuit:
// when the upstream advertises a Content-Length past the cap the outcome is
// already settled, so the body must stream to the agent untouched rather than
// being dragged through a megabyte of doomed buffering first.
func TestBufferBody_DeclaredOversizeReadsNothing(t *testing.T) {
	payload := strings.Repeat("z", maxBufferedBody+4096)
	body := &countingBody{r: strings.NewReader(payload)}
	resp := &http.Response{ContentLength: int64(len(payload)), Body: body}

	if buf, ok := bufferBody(resp); ok || buf != nil {
		t.Errorf("bufferBody = (%d bytes, %v), want (nil, false)", len(buf), ok)
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

// TestBufferBody_UnknownLengthStillBuffers is the other side of that
// short-circuit: a chunked response declares -1, which must not be read as
// "under the cap" nor as "over" — it falls through to the read.
func TestBufferBody_UnknownLengthStillBuffers(t *testing.T) {
	const payload = `{"number":1}`
	resp := &http.Response{ContentLength: -1, Body: io.NopCloser(strings.NewReader(payload))}

	buf, ok := bufferBody(resp)
	if !ok {
		t.Fatal("bufferBody skipped a chunked response")
	}
	if string(buf) != payload {
		t.Errorf("buffered %q, want %q", buf, payload)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != payload {
		t.Errorf("re-presented body = %q, want %q", got, payload)
	}
}

// TestInjector_BodyAtExactlyCapStillParses is the boundary opposite the
// oversize probe: a body of exactly maxBufferedBody is buffered and its created
// object read, not skipped.
func TestInjector_BodyAtExactlyCapStillParses(t *testing.T) {
	head := `{"id":777,"node_id":"IC_kwCap","html_url":"https://github.com/octo/repo/pull/42#issuecomment-777","body":"`
	tail := `"}`
	pad := strings.Repeat("y", maxBufferedBody-len(head)-len(tail))
	commentJSON := head + pad + tail
	if len(commentJSON) != maxBufferedBody {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(commentJSON), maxBufferedBody)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(commentJSON))
	}))
	defer upstream.Close()

	writes := make(chan ObservedWrite, 2)
	client, host := newInjectorWithWrites(t, upstream.URL, func(_ context.Context, w ObservedWrite) { writes <- w })

	req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/api/v3/repos/octo/repo/issues/42/comments",
		strings.NewReader(`{}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if len(got) != len(commentJSON) {
		t.Errorf("caller received %d bytes, want %d", len(got), len(commentJSON))
	}
	select {
	case w := <-writes:
		if w.ExternalID != "777" {
			t.Errorf("write audit = %+v, want the created object read from a body exactly at the cap", w)
		}
	case <-time.After(time.Second):
		t.Fatal("no write audit for the at-cap create")
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

// newInjectorWithWrites is newInjector with the write-audit callback wired.
func newInjectorWithWrites(t *testing.T, upstream string,
	observeWrite func(context.Context, ObservedWrite)) (*http.Client, string) {
	t.Helper()
	cert, certPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	srv, err := New(Config{
		Upstream:     upstream,
		Cert:         cert,
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
		{"merge a PR", http.MethodPut, "/api/v3/repos/octo/repo/pulls/7/merge", "", http.StatusOK, true},
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
			client, host := newInjectorWithWrites(t, upstream.URL,
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

// TestInjector_WriteAuditReadsCreatedObject pins the incident fix's injector
// half: for a shape the shared classifier calls a create, the response body is
// parsed for the new object's id and link so the audit row can name what was
// made — while the caller still receives that body whole, and the request still
// reaches the upstream untouched.
func TestInjector_WriteAuditReadsCreatedObject(t *testing.T) {
	const replyURL = "https://github.com/acme/widgets/pull/841#discussion_r777"
	const respBody = `{"id":777,"node_id":"PRRC_kwReply","in_reply_to_id":555,"html_url":"` + replyURL + `"}`
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
	client, host := newInjectorWithWrites(t, upstream.URL,
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
		if w.ExternalID != "777" || w.NodeID != "PRRC_kwReply" || w.URL != replyURL {
			t.Errorf("write audit = %+v, want the created reply's id, node id, and discussion link", w)
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
	huge := strings.Repeat("x", maxBufferedBody+2048)
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
			client, host := newInjectorWithWrites(t, upstream.URL,
				func(_ context.Context, w ObservedWrite) { writes <- w })

			req, _ := http.NewRequest(tc.method, "https://"+host+tc.path, nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			got, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if len(got) < maxBufferedBody {
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
	client, host := newInjectorWithWrites(t, upstream.URL,
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
	client, host := newInjectorWithWrites(t, upstream.URL,
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
// with a 200 and an errors array, so the status line alone would report a merge
// that never happened as one that did.
func TestInjector_GraphQLErrorsRecordAnAttempt(t *testing.T) {
	upstream, _ := graphQLUpstream(t, http.StatusOK,
		`{"data":{"mergePullRequest":null},"errors":[{"message":"Pull request is not mergeable"}]}`)

	writes := make(chan ObservedWrite, 2)
	client, host := newInjectorWithWrites(t, upstream.URL,
		func(_ context.Context, w ObservedWrite) { writes <- w })

	body := graphQLEnvelope(t,
		`mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){clientMutationId}}`,
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
			t.Error("a refused merge reported success")
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
		_, _ = w.Write([]byte(`{"id":7,"node_id":"IC_kwREST","html_url":"https://github.com/acme/widgets/issues/1#issuecomment-7"}`))
	}))
	defer upstream.Close()

	writes := make(chan ObservedWrite, 2)
	client, host := newInjectorWithWrites(t, upstream.URL,
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
		if facts, _ := srv.captureGraphQL(req); facts != nil {
			b.Fatal("a read classified as a write")
		}
	}
}

// BenchmarkGraphQLCaptureAddressedRead is the same read with the read touch
// wired, which is the production shape: the document is walked for the pull
// request it addresses.
func BenchmarkGraphQLCaptureAddressedRead(b *testing.B) {
	body := `{"query":"query PullRequestByNumber($n:Int!){repository(owner:\"acme\",name:\"widgets\"){pullRequest(number:$n){id url title body baseRefName headRefName}}}","variables":{"n":841}}`
	srv := &Server{cfg: Config{ObserveRead: func(context.Context, ObservedRead) {}}}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		req, _ := http.NewRequest(http.MethodPost, "https://example.test/api/graphql", strings.NewReader(body))
		if _, addressed := srv.captureGraphQL(req); len(addressed.Objects) != 1 {
			b.Fatalf("addressed %v, want the one pull request", addressed)
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
		if facts, _ := srv.captureGraphQL(req); facts == nil {
			b.Fatal("a mutation did not classify as a write")
		}
	}
}

// TestInjector_GraphQLUnreadResponseIsNotASuccess: when the response cannot be
// read, the write is recorded as attempted rather than as a success nobody
// observed — and it says which of the two happened, since "the server refused
// it" and "we could not tell" are different admissions.
func TestInjector_GraphQLUnreadResponseIsNotASuccess(t *testing.T) {
	huge := `{"data":{"mergePullRequest":{"clientMutationId":"` +
		strings.Repeat("x", maxBufferedBody+4096) + `"}}}`
	upstream, _ := graphQLUpstream(t, http.StatusOK, huge)

	writes := make(chan ObservedWrite, 2)
	client, host := newInjectorWithWrites(t, upstream.URL,
		func(_ context.Context, w ObservedWrite) { writes <- w })

	body := graphQLEnvelope(t,
		`mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){clientMutationId}}`,
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
		if w.GraphQL == nil || w.GraphQL.Mutation() != "mergePullRequest" {
			t.Errorf("audit = %+v, want the merge still named from the request", w)
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

// TestInjector_CreatedCommentIsNotReadBack pins that the injector makes no
// read of a comment a write created: the comment is reported with the node id
// its response carried, or none, and only the write's pull request and
// repository are read.
func TestInjector_CreatedCommentIsNotReadBack(t *testing.T) {
	const commentURL = "https://github.com/acme/widgets/pull/841#issuecomment-7"
	addComment := graphQLEnvelope(t,
		`mutation CommentCreate($input:AddCommentInput!){addComment(input:$input){commentEdge{node{url}}}}`,
		map[string]any{"input": map[string]any{"body": "pushed a fix", "subjectId": "PR_kwReadBack"}})
	merge := graphQLEnvelope(t,
		`mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){clientMutationId}}`,
		map[string]any{"input": map[string]any{"pullRequestId": "PR_kwReadBack"}})

	cases := []struct {
		name     string
		body     string
		response string
		wantNode string
	}{
		{
			name:     "a comment created without its node id",
			body:     addComment,
			response: `{"data":{"addComment":{"commentEdge":{"node":{"url":"` + commentURL + `"}}}}}`,
		},
		{
			name:     "a comment created with its node id",
			body:     addComment,
			response: `{"data":{"addComment":{"commentEdge":{"node":{"id":"IC_kwInline","url":"` + commentURL + `"}}}}}`,
			wantNode: "IC_kwInline",
		},
		{
			name:     "a refused comment",
			body:     addComment,
			response: `{"data":{"addComment":null},"errors":[{"message":"no"}]}`,
		},
		{
			name:     "a write that creates no comment",
			body:     merge,
			response: `{"data":{"mergePullRequest":{"clientMutationId":null}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reads []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					// The identity reads of the write's pull request and
					// repository, which TestInjector_AcceptedWriteCarriesItsIdentity
					// covers, answer nothing; a comment read is recorded.
					if strings.Contains(r.URL.Path, "/comments/") {
						reads = append(reads, r.URL.Path)
					}
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(tc.response))
			}))
			defer upstream.Close()

			writes := make(chan ObservedWrite, 2)
			client, host := newInjectorWithWrites(t, upstream.URL,
				func(_ context.Context, w ObservedWrite) { writes <- w })
			req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/api/graphql", strings.NewReader(tc.body))
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			_ = resp.Body.Close()

			select {
			case w := <-writes:
				if w.NodeID != tc.wantNode {
					t.Errorf("observed node id = %q, want %q", w.NodeID, tc.wantNode)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no write audit")
			}
			if len(reads) != 0 {
				t.Errorf("comment reads = %q, want none", reads)
			}
		})
	}
}

// TestInjector_AcceptedWriteCarriesItsIdentity pins the identity reads: an
// accepted write is reported with what GitHub calls its objects, read with the
// run's real credential — the pull request's or issue's node id and kind and
// its repository's id for a write that names one by number, the repository's
// alone for one that names none. A GraphQL mutation whose input is bound to
// the node id its variables name carries that node id, with only the
// repository its response locates read; one that spelled its arguments inline
// beside a variable naming something else is identified by the object its
// response's url locates, never by that variable. Each repository is read once
// per run, and a refused write reads nothing.
func TestInjector_AcceptedWriteCarriesItsIdentity(t *testing.T) {
	var mu sync.Mutex
	reads := map[string]int{}
	var readAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodGet {
			mu.Lock()
			reads[r.URL.Path]++
			readAuth = r.Header.Get("Authorization")
			mu.Unlock()
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/841":
			_, _ = w.Write([]byte(`{"number":841,"node_id":"PR_kw841","base":{"repo":{"id":4242,"full_name":"acme/widgets"}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/issues/9":
			_, _ = w.Write([]byte(`{"number":9,"node_id":"I_kw9"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets":
			_, _ = w.Write([]byte(`{"id":4242,"full_name":"acme/widgets"}`))
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/issues/77/labels"):
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/graphql" && strings.Contains(string(body), "mergePullRequest"):
			_, _ = w.Write([]byte(`{"data":{"mergePullRequest":{"pullRequest":{"url":"https://github.com/acme/widgets/pull/841"}}}}`))
		case r.URL.Path == "/graphql":
			_, _ = w.Write([]byte(`{"data":{"closePullRequest":{"pullRequest":{"id":"PR_kwClosed"}}}}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer upstream.Close()

	writes := make(chan ObservedWrite, 8)
	client, host := newInjectorWithWrites(t, upstream.URL, func(_ context.Context, w ObservedWrite) { writes <- w })
	send := func(path, body string) ObservedWrite {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "https://"+host+path, strings.NewReader(body))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		_ = resp.Body.Close()
		select {
		case w := <-writes:
			return w
		case <-time.After(5 * time.Second):
			t.Fatalf("no write audit for %s", path)
			return ObservedWrite{}
		}
	}
	repo := domain.GitHubIdentity{RepositoryID: "4242", Repository: "acme/widgets"}
	withObject := func(nodeID, kind string) domain.GitHubIdentity {
		id := repo
		id.NodeID, id.Kind = nodeID, kind
		return id
	}

	if got := send("/api/v3/repos/acme/widgets/issues/841/labels", `{"labels":["bug"]}`).Identity; got != withObject("PR_kw841", domain.EntityKindPullRequest) {
		t.Errorf("a label write on a pull request carries %+v, want its node id and repository id read back", got)
	}
	if got := send("/api/v3/repos/acme/widgets/issues/9/labels", `{"labels":["bug"]}`).Identity; got != withObject("I_kw9", domain.EntityKindIssue) {
		t.Errorf("a label write on an issue carries %+v, want the issue's node id and kind", got)
	}
	if got := send("/api/v3/repos/acme/widgets/labels", `{"name":"bug"}`).Identity; got != repo {
		t.Errorf("a repository write carries %+v, want the repository's id", got)
	}
	bound := graphQLEnvelope(t,
		`mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){pullRequest{url}}}`,
		map[string]any{"input": map[string]any{"pullRequestId": "PR_kwSubject"}})
	if got := send("/api/graphql", bound).Identity; got != withObject("PR_kwSubject", domain.EntityKindPullRequest) {
		t.Errorf("a merge whose input is bound to its pull request carries %+v, want the subject's node id and the repository's id", got)
	}
	// The variables name a node id the inline arguments do not use; the
	// response's url says which pull request was merged.
	merge := graphQLEnvelope(t,
		`mutation{mergePullRequest(input:{pullRequestId:"PR_kw841"}){pullRequest{url}}}`,
		map[string]any{"input": map[string]any{"pullRequestId": "PR_kwElsewhere"}})
	if got := send("/api/graphql", merge).Identity; got != withObject("PR_kw841", domain.EntityKindPullRequest) {
		t.Errorf("a merge whose response names its pull request carries %+v, want the pull request the url locates", got)
	}
	closed := graphQLEnvelope(t,
		`mutation($input:ClosePullRequestInput!){closePullRequest(input:$input){pullRequest{id}}}`,
		map[string]any{"input": map[string]any{"pullRequestId": "PR_kwClosed"}})
	if got := send("/api/graphql", closed).Identity; got != (domain.GitHubIdentity{NodeID: "PR_kwClosed", Kind: domain.EntityKindPullRequest}) {
		t.Errorf("a close whose response locates nothing carries %+v, want its subject's node id alone", got)
	}
	inline := graphQLEnvelope(t,
		`mutation{closePullRequest(input:{pullRequestId:"PR_kwClosed"}){pullRequest{id}}}`,
		map[string]any{"input": map[string]any{"pullRequestId": "PR_kwElsewhere"}})
	if got := send("/api/graphql", inline).Identity; got != (domain.GitHubIdentity{}) {
		t.Errorf("an inline close whose response locates nothing carries %+v, want nothing: its variables' node id is not what it closed", got)
	}
	if got := send("/api/v3/repos/acme/widgets/issues/77/labels", `{"labels":["bug"]}`).Identity; got != (domain.GitHubIdentity{}) {
		t.Errorf("a refused write carries %+v, want nothing read", got)
	}

	mu.Lock()
	defer mu.Unlock()
	// The pull request's read answered its repository, and every later write
	// in it takes the repository from the run's cache.
	if reads["/repos/acme/widgets"] != 0 {
		t.Errorf("repository reads = %d, want none after the pull request read answered it", reads["/repos/acme/widgets"])
	}
	if reads["/repos/acme/widgets/pulls/841"] != 1 || reads["/repos/acme/widgets/issues/77"] != 0 || reads["/repos/acme/widgets/pulls/77"] != 0 {
		t.Errorf("object reads = %v, want #841 read once, as the label write's object, and nothing for the refused write", reads)
	}
	if readAuth != "token ghs_realtoken" {
		t.Errorf("identity read authorization = %q, want the run's real credential", readAuth)
	}
}

// TestInjector_AddressedReadsAreReportedOnce pins the read touch without the
// gh binary: a read the upstream accepted reports the pull request or issue it
// addressed, with what GitHub calls it, the first time the run addresses it —
// over REST and GraphQL alike, under whatever case the request spelled it in.
// A read the upstream refused, a set, a write, and an object GitHub does not
// answer for report nothing, and the last is asked for again on the next read.
func TestInjector_AddressedReadsAreReportedOnce(t *testing.T) {
	var mu sync.Mutex
	gets := map[string]int{}
	missing := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			mu.Lock()
			gets[r.URL.Path]++
			mu.Unlock()
		}
		switch {
		case r.Method == http.MethodGet && strings.EqualFold(r.URL.Path, "/repos/acme/widgets/pulls/841"):
			_, _ = w.Write([]byte(`{"number":841,"node_id":"PR_kw841","base":{"repo":{"id":4242,"full_name":"acme/widgets"}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/issues/9":
			_, _ = w.Write([]byte(`{"number":9,"node_id":"I_kw9"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets":
			_, _ = w.Write([]byte(`{"id":4242,"full_name":"acme/widgets"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/78":
			mu.Lock()
			gone := missing
			mu.Unlock()
			if gone {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"number":78,"node_id":"PR_kw78","base":{"repo":{"id":4242,"full_name":"acme/widgets"}}}`))
		case r.Method == http.MethodGet && (strings.HasSuffix(r.URL.Path, "/files") || strings.HasSuffix(r.URL.Path, "/comments") || r.URL.Path == "/repos/acme/widgets/pulls"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/graphql":
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":null}}}`))
		default:
			_, _ = w.Write([]byte(`{"id":1,"html_url":"https://github.com/acme/widgets/pull/841#issuecomment-1"}`))
		}
	}))
	defer upstream.Close()

	cert, certPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	var reads []ObservedRead
	srv, err := New(Config{
		Upstream: upstream.URL,
		Cert:     cert,
		ObserveRead: func(_ context.Context, r ObservedRead) {
			mu.Lock()
			defer mu.Unlock()
			reads = append(reads, r)
		},
		ObserveWrite: func(context.Context, ObservedWrite) {},
		TokenSource:  func(context.Context) (string, error) { return "ghs_realtoken", nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	host, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}

	// send makes one request and returns what the read touch reported for it.
	// The touch is reported before the response is released, so it is in hand
	// once the request returns.
	send := func(method, path, body string) []ObservedRead {
		t.Helper()
		mu.Lock()
		before := len(reads)
		mu.Unlock()
		req, _ := http.NewRequest(method, "https://"+host+path, strings.NewReader(body))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		mu.Lock()
		defer mu.Unlock()
		return append([]ObservedRead(nil), reads[before:]...)
	}
	query := func(document string, variables map[string]any) []ObservedRead {
		t.Helper()
		return send(http.MethodPost, "/api/graphql", graphQLEnvelope(t, document, variables))
	}
	repo := domain.GitHubIdentity{RepositoryID: "4242", Repository: "acme/widgets"}
	observed := func(owner, name string, number int, nodeID, kind string) []ObservedRead {
		id := repo
		id.NodeID, id.Kind = nodeID, kind
		return []ObservedRead{{ObjectRef: ghwrite.ObjectRef{Owner: owner, Repo: name, Number: number}, Identity: id}}
	}
	none := []ObservedRead(nil)

	if got := send(http.MethodGet, "/api/v3/repos/acme/widgets/pulls/841/files", ""); !slices.Equal(got, observed("acme", "widgets", 841, "PR_kw841", domain.EntityKindPullRequest)) {
		t.Errorf("a read under a pull request reported %+v, want the pull request with its ids", got)
	}
	if got := send(http.MethodGet, "/api/v3/repos/acme/widgets/pulls/841/comments", ""); !slices.Equal(got, none) {
		t.Errorf("a second read of the pull request reported %+v, want nothing", got)
	}
	if got := query(`query($n:Int!){repository(owner:"ACME",name:"Widgets"){pullRequest(number:$n){title}}}`, map[string]any{"n": 841}); !slices.Equal(got, none) {
		t.Errorf("a GraphQL read of the pull request, spelled in another case, reported %+v, want nothing", got)
	}
	if got := query(`{repository(owner:"acme",name:"widgets"){issue(number:9){title}}}`, nil); !slices.Equal(got, observed("acme", "widgets", 9, "I_kw9", domain.EntityKindIssue)) {
		t.Errorf("a GraphQL read of an issue reported %+v, want the issue with its ids", got)
	}
	if got := send(http.MethodGet, "/api/v3/repos/acme/widgets/pulls", ""); !slices.Equal(got, none) {
		t.Errorf("a list reported %+v, want nothing", got)
	}
	if got := send(http.MethodGet, "/api/v3/repos/acme/widgets/pulls/77", ""); !slices.Equal(got, none) {
		t.Errorf("a read the upstream refused reported %+v, want nothing", got)
	}
	if got := send(http.MethodPost, "/api/v3/repos/acme/widgets/issues/55/comments", `{"body":"hi"}`); !slices.Equal(got, none) {
		t.Errorf("a write reported %+v as a read, want nothing", got)
	}
	// GraphQL answers a query for a number nothing is at with a 200.
	missingQuery := `{repository(owner:"acme",name:"widgets"){pullRequest(number:78){title}}}`
	if got := query(missingQuery, nil); !slices.Equal(got, none) {
		t.Errorf("a query for an object GitHub does not answer for reported %+v, want nothing", got)
	}
	mu.Lock()
	missing = false
	mu.Unlock()
	if got := query(missingQuery, nil); !slices.Equal(got, observed("acme", "widgets", 78, "PR_kw78", domain.EntityKindPullRequest)) {
		t.Errorf("the next read of that object reported %+v, want it asked for again and reported", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if gets["/repos/acme/widgets/pulls/841"] != 1 {
		t.Errorf("identity reads of the pull request = %d, want 1 for the run", gets["/repos/acme/widgets/pulls/841"])
	}
	// The one read of #77 is the agent's own, which the upstream refused; the
	// injector read nothing for it.
	if gets["/repos/acme/widgets/pulls/77"] != 1 || gets["/repos/acme/widgets/issues/77"] != 0 {
		t.Errorf("reads of #77 = %d pull, %d issue; want the agent's own refused read and none of the injector's",
			gets["/repos/acme/widgets/pulls/77"], gets["/repos/acme/widgets/issues/77"])
	}
}

// TestInjector_BranchLookupTouchesTheOnePullRequest pins the branch half of
// the read touch without the gh binary: a GraphQL lookup of a branch's pull
// requests reports the one pull request the injector's own lookup finds open
// on that branch, with what GitHub calls it, and nothing for a branch with none
// or several. The agent's response is not what is read: it names another pull
// request here, and that one is never reported. A branch is looked up once per
// run whatever the answer was, except a failed lookup, which is asked again;
// and a pull request is reported once, whether the run reached it by its
// number or by its branch.
func TestInjector_BranchLookupTouchesTheOnePullRequest(t *testing.T) {
	var mu sync.Mutex
	lookups := map[string]int{}
	identityReads := 0
	flaky := true
	pull := func(number int, nodeID, branch string) string {
		return `{"number":` + strconv.Itoa(number) + `,"node_id":"` + nodeID + `","head":{"ref":"` + branch + `"},` +
			`"base":{"repo":{"id":4242,"full_name":"acme/widgets"}}}`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/graphql":
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "refused") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			// What gh would read as its pull request: one the lookup below
			// does not answer.
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequests":{"nodes":[{"number":99,"id":"PR_kw99","headRefName":"feature"}]}}}}`))
		case r.URL.Path == "/repos/acme/widgets/pulls" && r.URL.Query().Has("head"):
			branch := strings.TrimPrefix(r.URL.Query().Get("head"), "acme:")
			mu.Lock()
			lookups[branch]++
			failing := branch == "flaky" && flaky
			mu.Unlock()
			switch {
			case r.URL.Query().Get("state") != "open":
				w.WriteHeader(http.StatusBadRequest)
			case failing:
				w.WriteHeader(http.StatusBadGateway)
			case branch == "feature":
				_, _ = w.Write([]byte(`[` + pull(841, "PR_kw841", branch) + `]`))
			case branch == "fifty-five":
				_, _ = w.Write([]byte(`[` + pull(55, "PR_kw55", branch) + `]`))
			case branch == "flaky":
				_, _ = w.Write([]byte(`[` + pull(78, "PR_kw78", branch) + `]`))
			case branch == "two":
				_, _ = w.Write([]byte(`[` + pull(60, "PR_kw60", branch) + `,` + pull(61, "PR_kw61", branch) + `]`))
			default:
				_, _ = w.Write([]byte(`[]`))
			}
		case r.URL.Path == "/repos/acme/widgets/pulls/55":
			mu.Lock()
			identityReads++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"number":55,"node_id":"PR_kw55","base":{"repo":{"id":4242,"full_name":"acme/widgets"}}}`))
		case r.URL.Path == "/repos/acme/widgets/pulls/841":
			mu.Lock()
			identityReads++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"number":841,"node_id":"PR_kw841","base":{"repo":{"id":4242,"full_name":"acme/widgets"}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	cert, certPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	var reads []ObservedRead
	srv, err := New(Config{
		Upstream: upstream.URL,
		Cert:     cert,
		ObserveRead: func(_ context.Context, r ObservedRead) {
			mu.Lock()
			defer mu.Unlock()
			reads = append(reads, r)
		},
		TokenSource: func(context.Context) (string, error) { return "ghs_realtoken", nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	host, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}

	send := func(method, path, body string) []ObservedRead {
		t.Helper()
		mu.Lock()
		before := len(reads)
		mu.Unlock()
		resp, err := client.Do(mustRequest(t, method, "https://"+host+path, body))
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		mu.Lock()
		defer mu.Unlock()
		return append([]ObservedRead(nil), reads[before:]...)
	}
	lookup := func(branch string) []ObservedRead {
		t.Helper()
		return send(http.MethodPost, "/api/graphql", graphQLEnvelope(t,
			`query PullRequestForBranch($owner:String!,$repo:String!,$headRefName:String!){repository(owner:$owner,name:$repo){pullRequests(headRefName:$headRefName,first:30){nodes{number}}}}`,
			map[string]any{"owner": "acme", "repo": "widgets", "headRefName": branch}))
	}
	observed := func(number int, nodeID string) []ObservedRead {
		return []ObservedRead{{
			ObjectRef: ghwrite.ObjectRef{Owner: "acme", Repo: "widgets", Number: number},
			Identity:  domain.GitHubIdentity{RepositoryID: "4242", Repository: "acme/widgets", NodeID: nodeID, Kind: domain.EntityKindPullRequest},
		}}
	}
	none := []ObservedRead(nil)

	if got := lookup("feature"); !slices.Equal(got, observed(841, "PR_kw841")) {
		t.Errorf("a lookup of a branch with one open pull request reported %+v, want that pull request with its ids", got)
	}
	if got := lookup("feature"); !slices.Equal(got, none) {
		t.Errorf("a second lookup of the branch reported %+v, want nothing", got)
	}
	if got := send(http.MethodGet, "/api/v3/repos/acme/widgets/pulls/841", ""); !slices.Equal(got, none) {
		t.Errorf("a read by number of the pull request the lookup found reported %+v, want nothing", got)
	}
	if got := send(http.MethodGet, "/api/v3/repos/acme/widgets/pulls/55", ""); !slices.Equal(got, observed(55, "PR_kw55")) {
		t.Errorf("a read by number reported %+v, want the pull request", got)
	}
	if got := lookup("fifty-five"); !slices.Equal(got, none) {
		t.Errorf("a lookup finding a pull request the run already read reported %+v, want nothing", got)
	}
	for _, branch := range []string{"none", "two", "none", "two"} {
		if got := lookup(branch); !slices.Equal(got, none) {
			t.Errorf("a lookup of %q reported %+v, want nothing", branch, got)
		}
	}
	if got := lookup("flaky"); !slices.Equal(got, none) {
		t.Errorf("a failed lookup reported %+v, want nothing", got)
	}
	mu.Lock()
	flaky = false
	mu.Unlock()
	if got := lookup("flaky"); !slices.Equal(got, observed(78, "PR_kw78")) {
		t.Errorf("the lookup after a failed one reported %+v, want it asked again and the pull request reported", got)
	}
	refused := `query PullRequestForBranch($o:String!){repository(owner:$o,name:"widgets"){pullRequests(headRefName:"refused"){nodes{number}}}}`
	if got := send(http.MethodPost, "/api/graphql", graphQLEnvelope(t, refused, map[string]any{"o": "acme"})); !slices.Equal(got, none) {
		t.Errorf("a lookup the upstream refused reported %+v, want nothing", got)
	}

	mu.Lock()
	defer mu.Unlock()
	want := map[string]int{"feature": 1, "fifty-five": 1, "none": 1, "two": 1, "flaky": 2}
	for branch, n := range want {
		if lookups[branch] != n {
			t.Errorf("lookups of %q = %d, want %d", branch, lookups[branch], n)
		}
	}
	if lookups["refused"] != 0 {
		t.Errorf("lookups of a branch the agent's own request was refused for = %d, want 0", lookups["refused"])
	}
	// The lookup answered what GitHub calls #841, so its read by number cost
	// none of the injector's own reads; #55's first read did.
	if identityReads != 3 {
		t.Errorf("reads of the pull request objects = %d, want 3: the agent's two and one identity read of #55", identityReads)
	}
}

// TestInjector_CommentAndNodeReads: a REST read of a comment by its own id
// reports the pull request or issue the comment is on, which the injector
// finds by reading the comment once; a GraphQL read by node id reports the
// object the node id names, or the one the comment or review thread it names
// is on, which the injector finds by asking GraphQL once. Both report the
// object with GitHub's ids, once per run, whichever way the run reached it. A
// refused read, and a node id that names something else or nothing GitHub
// answers for, report nothing.
func TestInjector_CommentAndNodeReads(t *testing.T) {
	var mu sync.Mutex
	gets := map[string]int{}
	nodeQueries := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			mu.Lock()
			gets[r.URL.Path]++
			mu.Unlock()
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/comments/9001":
			_, _ = w.Write([]byte(`{"id":9001,"node_id":"PRRC_kw9001","pull_request_url":"https://api.github.com/repos/acme/widgets/pulls/841"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/issues/comments/7001":
			_, _ = w.Write([]byte(`{"id":7001,"node_id":"IC_kw7001","issue_url":"https://api.github.com/repos/acme/widgets/issues/9"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/issues/comments/7002":
			_, _ = w.Write([]byte(`{"id":7002,"node_id":"IC_kw7002","issue_url":"https://api.github.com/repos/acme/widgets/issues/9"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reactions"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/841":
			_, _ = w.Write([]byte(`{"number":841,"node_id":"PR_kw841","base":{"repo":{"id":4242,"full_name":"acme/widgets"}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/50":
			_, _ = w.Write([]byte(`{"number":50,"node_id":"PR_kw50","base":{"repo":{"id":4242,"full_name":"acme/widgets"}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/60":
			_, _ = w.Write([]byte(`{"number":60,"node_id":"PR_kw60","base":{"repo":{"id":4242,"full_name":"acme/widgets"}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/issues/9":
			_, _ = w.Write([]byte(`{"number":9,"node_id":"I_kw9"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets":
			_, _ = w.Write([]byte(`{"id":4242,"full_name":"acme/widgets"}`))
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/graphql":
			body, _ := io.ReadAll(r.Body)
			var envelope struct {
				Query     string            `json:"query"`
				Variables map[string]string `json:"variables"`
			}
			_ = json.Unmarshal(body, &envelope)
			if !strings.Contains(envelope.Query, "query NodeSubject(") {
				// The agent's own read.
				if strings.Contains(envelope.Query, "PR_refused") {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				_, _ = w.Write([]byte(`{"data":{"node":null}}`))
				return
			}
			id := envelope.Variables["id"]
			mu.Lock()
			nodeQueries[id]++
			mu.Unlock()
			switch id {
			case "PR_kw50":
				_, _ = w.Write([]byte(`{"data":{"node":{"__typename":"PullRequest","number":50,"repository":{"nameWithOwner":"acme/widgets"}}}}`))
			case "IC_kwPersons":
				_, _ = w.Write([]byte(`{"data":{"node":{"__typename":"IssueComment","commentPullRequest":null,"issue":{"number":9},"repository":{"nameWithOwner":"acme/widgets"}}}}`))
			case "PR_kw841":
				_, _ = w.Write([]byte(`{"data":{"node":{"__typename":"PullRequest","number":841,"repository":{"nameWithOwner":"acme/widgets"}}}}`))
			case "PRRT_kw60":
				_, _ = w.Write([]byte(`{"data":{"node":{"__typename":"PullRequestReviewThread","pullRequest":{"number":60},"repository":{"nameWithOwner":"acme/widgets"}}}}`))
			case "U_kwUser":
				_, _ = w.Write([]byte(`{"data":{"node":{"__typename":"User"}}}`))
			case "R_kwBadRepo":
				_, _ = w.Write([]byte(`{"data":{"node":{"__typename":"PullRequest","number":1,"repository":{"nameWithOwner":"acme/../x"}}}}`))
			default:
				_, _ = w.Write([]byte(`{"data":{"node":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a node"}]}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	cert, certPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	var reads []ObservedRead
	srv, err := New(Config{
		Upstream: upstream.URL,
		Cert:     cert,
		ObserveRead: func(_ context.Context, r ObservedRead) {
			mu.Lock()
			defer mu.Unlock()
			reads = append(reads, r)
		},
		TokenSource: func(context.Context) (string, error) { return "ghs_realtoken", nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	host, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}

	send := func(method, path, body string) []ObservedRead {
		t.Helper()
		mu.Lock()
		before := len(reads)
		mu.Unlock()
		req, _ := http.NewRequest(method, "https://"+host+path, strings.NewReader(body))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		mu.Lock()
		defer mu.Unlock()
		return append([]ObservedRead(nil), reads[before:]...)
	}
	query := func(document string, variables map[string]any) []ObservedRead {
		t.Helper()
		return send(http.MethodPost, "/api/graphql", graphQLEnvelope(t, document, variables))
	}
	repo := domain.GitHubIdentity{RepositoryID: "4242", Repository: "acme/widgets"}
	observed := func(number int, nodeID, kind string) []ObservedRead {
		id := repo
		id.NodeID, id.Kind = nodeID, kind
		return []ObservedRead{{ObjectRef: ghwrite.ObjectRef{Owner: "acme", Repo: "widgets", Number: number}, Identity: id}}
	}
	none := []ObservedRead(nil)

	if got := send(http.MethodGet, "/api/v3/repos/acme/widgets/pulls/comments/9001", ""); !slices.Equal(got, observed(841, "PR_kw841", domain.EntityKindPullRequest)) {
		t.Errorf("a read of a review comment reported %+v, want the pull request it is on", got)
	}
	if got := send(http.MethodGet, "/api/v3/repos/acme/widgets/pulls/comments/9001/reactions", ""); !slices.Equal(got, none) {
		t.Errorf("a second read of the comment reported %+v, want nothing", got)
	}
	if got := send(http.MethodGet, "/api/v3/repos/acme/widgets/issues/comments/7001", ""); !slices.Equal(got, observed(9, "I_kw9", domain.EntityKindIssue)) {
		t.Errorf("a read of a conversation comment reported %+v, want the issue it is on", got)
	}
	if got := send(http.MethodGet, "/api/v3/repos/acme/widgets/issues/comments/7002", ""); !slices.Equal(got, none) {
		t.Errorf("a read of another comment on a reported issue reported %+v, want nothing", got)
	}
	if got := send(http.MethodGet, "/api/v3/repos/acme/widgets/issues/comments/7003", ""); !slices.Equal(got, none) {
		t.Errorf("a comment read the upstream refused reported %+v, want nothing", got)
	}

	if got := query(`query($id:ID!){node(id:$id){id}}`, map[string]any{"id": "PR_kw50"}); !slices.Equal(got, observed(50, "PR_kw50", domain.EntityKindPullRequest)) {
		t.Errorf("a read of a pull request by node id reported %+v, want the pull request with its ids", got)
	}
	if got := query(`{node(id:"PRRT_kw60"){... on PullRequestReviewThread{isResolved}}}`, nil); !slices.Equal(got, observed(60, "PR_kw60", domain.EntityKindPullRequest)) {
		t.Errorf("a read of a review thread by node id reported %+v, want the pull request it is on", got)
	}
	if got := query(`{a:node(id:"PR_kw50"){id} b:node(id:"PR_kw841"){id} c:node(id:"IC_kwPersons"){id}}`, nil); !slices.Equal(got, none) {
		t.Errorf("node reads of objects the run already reported reported %+v, want nothing", got)
	}
	for _, id := range []string{"U_kwUser", "R_kwBadRepo", "PR_unknown"} {
		if got := query(`query($id:ID!){node(id:$id){id}}`, map[string]any{"id": id}); !slices.Equal(got, none) {
			t.Errorf("a read of node %s reported %+v, want nothing", id, got)
		}
	}
	if got := query(`{node(id:"PR_refused"){id}}`, nil); !slices.Equal(got, none) {
		t.Errorf("a node read the upstream refused reported %+v, want nothing", got)
	}
	for _, id := range []string{"U_kwUser", "PR_unknown"} {
		_ = query(`query($id:ID!){node(id:$id){id}}`, map[string]any{"id": id})
	}

	mu.Lock()
	defer mu.Unlock()
	// The agent's two reads of comment 9001 and the injector's one.
	if n := gets["/repos/acme/widgets/pulls/comments/9001"]; n != 2 {
		t.Errorf("reads of comment 9001 = %d, want the agent's and one of the injector's", n)
	}
	if n := gets["/repos/acme/widgets/issues/comments/7002"]; n != 2 {
		t.Errorf("reads of comment 7002 = %d, want the agent's and one of the injector's", n)
	}
	if n := gets["/repos/acme/widgets/pulls/841"]; n != 1 {
		t.Errorf("identity reads of the pull request = %d, want 1", n)
	}
	if n := gets["/repos/acme/widgets/issues/comments/7003"]; n != 1 {
		t.Errorf("reads of the refused comment = %d, want the agent's alone", n)
	}
	// An answer is asked for once; a node GitHub answers null for is asked
	// again; a refused read asks nothing.
	for id, want := range map[string]int{"PR_kw50": 1, "PR_kw841": 1, "IC_kwPersons": 1, "U_kwUser": 1, "PR_unknown": 2, "PR_refused": 0} {
		if n := nodeQueries[id]; n != want {
			t.Errorf("node lookups of %s = %d, want %d", id, n, want)
		}
	}
	if n := gets["/repos/acme/../x/pulls/1"]; n != 0 {
		t.Errorf("a repository name GitHub answered in a shape no repository has was read %d times", n)
	}
}

// TestInjector_ReadsCostNothingWithoutAReadObserver: with no read touch wired,
// a read is forwarded and nothing else is asked of the upstream.
func TestInjector_ReadsCostNothingWithoutAReadObserver(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer upstream.Close()
	client, host := newInjectorWithWrites(t, upstream.URL, func(context.Context, ObservedWrite) {})

	for _, req := range []*http.Request{
		mustRequest(t, http.MethodGet, "https://"+host+"/api/v3/repos/acme/widgets/pulls/841", ""),
		mustRequest(t, http.MethodPost, "https://"+host+"/api/graphql",
			graphQLEnvelope(t, `{repository(owner:"acme",name:"widgets"){pullRequest(number:841){title}}}`, nil)),
		mustRequest(t, http.MethodPost, "https://"+host+"/api/graphql",
			graphQLEnvelope(t, `{repository(owner:"acme",name:"widgets"){pullRequests(headRefName:"feature"){nodes{number}}}}`, nil)),
		mustRequest(t, http.MethodGet, "https://"+host+"/api/v3/repos/acme/widgets/pulls/comments/9001", ""),
		mustRequest(t, http.MethodPost, "https://"+host+"/api/graphql",
			graphQLEnvelope(t, `{node(id:"PR_kw841"){id}}`, nil)),
	} {
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
		}
		_ = resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"GET /repos/acme/widgets/pulls/841", "POST /graphql", "POST /graphql", "GET /repos/acme/widgets/pulls/comments/9001", "POST /graphql"}; !slices.Equal(seen, want) {
		t.Errorf("upstream saw %v, want only the five forwarded reads %v", seen, want)
	}
}

func mustRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	return req
}
