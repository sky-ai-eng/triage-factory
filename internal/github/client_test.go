package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHTTPError_Error(t *testing.T) {
	err := &HTTPError{StatusCode: 406, Body: `{"message":"diff too large"}`, msg: "GET /repos/o/r/pulls/1 returned 406: diff too large"}
	want := "GET /repos/o/r/pulls/1 returned 406: diff too large"
	if err.Error() != want {
		t.Errorf("HTTPError.Error() = %q, want %q", err.Error(), want)
	}
}

func TestIsHTTP406_True(t *testing.T) {
	err := &HTTPError{StatusCode: 406, Body: "diff too large", msg: "406"}
	if !IsHTTP406(err) {
		t.Error("IsHTTP406 should return true for a 406 HTTPError")
	}
}

func TestIsHTTP406_OtherStatusCodes(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 422, 500, 502} {
		err := &HTTPError{StatusCode: code, Body: "error", msg: fmt.Sprintf("%d", code)}
		if IsHTTP406(err) {
			t.Errorf("IsHTTP406 should return false for status %d", code)
		}
	}
}

func TestIsHTTP406_NonHTTPError(t *testing.T) {
	err := errors.New("network timeout")
	if IsHTTP406(err) {
		t.Error("IsHTTP406 should return false for a plain error")
	}
}

func TestIsHTTP406_Nil(t *testing.T) {
	if IsHTTP406(nil) {
		t.Error("IsHTTP406 should return false for nil")
	}
}

func TestIsHTTP406_Wrapped(t *testing.T) {
	inner := &HTTPError{StatusCode: 406, Body: "diff too large", msg: "406"}
	wrapped := fmt.Errorf("getDiffLines: %w", inner)
	if !IsHTTP406(wrapped) {
		t.Error("IsHTTP406 should unwrap and find the 406 error")
	}
}

func TestIsHTTP406_WrappedNon406(t *testing.T) {
	inner := &HTTPError{StatusCode: 500, Body: "internal server error", msg: "500"}
	wrapped := fmt.Errorf("request failed: %w", inner)
	if IsHTTP406(wrapped) {
		t.Error("IsHTTP406 should return false when wrapped error is not 406")
	}
}

// TestClient_UserID pins the bot-user-id lookup the App-registration path uses
// to build the numeric-id noreply commit email (TFAC-474): GET /users/{login}
// with the "[bot]" brackets URL-escaped on the wire, parsing {"id":N}; a 404 (or
// any non-2xx) or an idless body yields 0 + error so the caller falls back to
// the plain noreply form. It also pins that an empty-token client sends NO
// Authorization header — the registration read is genuinely unauthenticated,
// not a malformed "Bearer " GitHub would reject.
func TestClient_UserID(t *testing.T) {
	t.Run("resolves id, url-escapes [bot], unauthenticated", func(t *testing.T) {
		var gotRequestURI, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotRequestURI = r.RequestURI
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":424242,"login":"acme-bot[bot]"}`))
		}))
		defer srv.Close()

		id, err := NewClient(srv.URL, "").UserID(context.Background(), "acme-bot[bot]")
		if err != nil {
			t.Fatalf("UserID: %v", err)
		}
		if id != 424242 {
			t.Errorf("id = %d, want 424242", id)
		}
		// The brackets must be percent-encoded on the wire (GitHub rejects raw
		// brackets in a path); the decoded form must NOT appear.
		if !strings.Contains(gotRequestURI, "acme-bot%5Bbot%5D") {
			t.Errorf("request URI = %q, want it to carry the escaped %%5Bbot%%5D", gotRequestURI)
		}
		if strings.Contains(gotRequestURI, "[bot]") {
			t.Errorf("request URI = %q carried raw [bot] brackets (must be escaped)", gotRequestURI)
		}
		// Empty token → unauthenticated: no Authorization header at all.
		if gotAuth != "" {
			t.Errorf("Authorization = %q, want empty (unauthenticated registration read)", gotAuth)
		}
	})

	t.Run("404 -> 0 + typed *HTTPError (status-discriminable)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}))
		defer srv.Close()

		id, err := NewClient(srv.URL, "").UserID(context.Background(), "ghost[bot]")
		if err == nil {
			t.Fatal("UserID on 404 returned nil error; want an error so the caller falls back to the plain form")
		}
		if id != 0 {
			t.Errorf("id = %d, want 0 on error", id)
		}
		// The error wraps a *HTTPError so a caller can status-discriminate
		// (404 vs 429, …) rather than only checking err != nil.
		var he *HTTPError
		if !errors.As(err, &he) || he.StatusCode != http.StatusNotFound {
			t.Errorf("error = %v, want a wrapped *HTTPError with StatusCode 404", err)
		}
	})

	t.Run("idless body -> 0 + error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"login":"acme-bot[bot]"}`)) // no id field
		}))
		defer srv.Close()

		id, err := NewClient(srv.URL, "").UserID(context.Background(), "acme-bot[bot]")
		if err == nil || id != 0 {
			t.Fatalf("UserID on idless body = (%d, %v), want (0, error)", id, err)
		}
	})
}

// TestClient_EmptyToken_UnauthenticatedAcrossMethods pins the invariant behind
// the setAuth seam (TFAC-474): an empty-token client omits the Authorization
// header on EVERY request builder — do/Get, GetConditional, GetRaw,
// DownloadArtifact, PostGraphQL — not just one. A regression that re-adds an
// unconditional "Bearer " on any of them would resurface the malformed-
// credential 401 on the unauthenticated registration read.
func TestClient_EmptyToken_UnauthenticatedAcrossMethods(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "") // unauthenticated client

	checks := []struct {
		name string
		call func() error
	}{
		{"do/Get", func() error { _, err := c.Get(context.Background(), "/x"); return err }},
		{"GetConditional", func() error { _, _, _, err := c.GetConditional(context.Background(), "/x", ""); return err }},
		{"GetRaw", func() error { _, err := c.GetRaw(context.Background(), "/x", "application/json"); return err }},
		{"DownloadArtifact", func() error {
			_, err := c.DownloadArtifact(context.Background(), "/x", &strings.Builder{}, 1<<20)
			return err
		}},
		{"PostGraphQL", func() error {
			_, err := c.PostGraphQL(context.Background(), map[string]any{"query": "{__typename}"})
			return err
		}},
	}
	for _, ck := range checks {
		gotAuth = "sentinel"
		if err := ck.call(); err != nil {
			t.Fatalf("%s: %v", ck.name, err)
		}
		if gotAuth != "" {
			t.Errorf("%s sent Authorization=%q, want none (empty-token client must be unauthenticated)", ck.name, gotAuth)
		}
	}
}

func TestGraphQLURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "github.com",
			baseURL: "https://api.github.com",
			want:    "https://api.github.com/graphql",
		},
		{
			name:    "GHES",
			baseURL: "https://git.corp.example.com/api/v3",
			want:    "https://git.corp.example.com/api/graphql",
		},
		{
			name:    "GHES trailing slash stripped",
			baseURL: "https://github.example.com/api/v3",
			want:    "https://github.example.com/api/graphql",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := graphqlURL(tt.baseURL)
			if got != tt.want {
				t.Errorf("graphqlURL(%q) = %q, want %q", tt.baseURL, got, tt.want)
			}
		})
	}
}

// captureHandler is a minimal slog.Handler that records emitted entries so a
// test can assert exactly what — and how often — a code path logged. It enables
// every level and keeps records in order. Handle is safe for concurrent use, as
// the slog.Handler contract requires.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// recorded returns a snapshot of the captured records under lock.
func (h *captureHandler) recorded() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

// graphqlServing starts an httptest server that returns body verbatim for any
// request, registers its teardown, and returns the base URL for clientAgainst.
func graphqlServing(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestPostGraphQL_PartialError_ReturnsData pins the partial-error path: a 200
// carrying both a usable `data` block and an `errors[]` entry (the FORBIDDEN
// statusCheckRollup shape an App without statuses:read produces) returns the
// data with no error, so the batch's other PRs survive one forbidden field
// instead of the whole refresh aborting. It also asserts the degradation is
// announced exactly once — a single WARN per response, not one per node.
func TestPostGraphQL_PartialError_ReturnsData(t *testing.T) {
	logs := &captureHandler{}
	prev := githubLog
	githubLog = slog.New(logs)
	t.Cleanup(func() { githubLog = prev })

	body := `{"data":{"nodes":[{"number":1}]},"errors":[{"type":"FORBIDDEN","path":["nodes",0,"commits","nodes",0,"commit","statusCheckRollup"],"message":"Resource not accessible by integration"}]}`
	data, err := clientAgainst(graphqlServing(t, body)).PostGraphQL(context.Background(), map[string]any{"query": "{ __typename }"})
	if err != nil {
		t.Fatalf("PostGraphQL with partial data should not error, got %v", err)
	}
	if !strings.Contains(string(data), `"number":1`) {
		t.Errorf("expected the usable data to pass through; got %s", string(data))
	}

	recs := logs.recorded()
	if len(recs) != 1 {
		t.Fatalf("expected exactly one log record, got %d", len(recs))
	}
	const wantMsg = "GraphQL partial error; using partial data"
	if got := recs[0]; got.Level != slog.LevelWarn || got.Message != wantMsg {
		t.Errorf("log = %v %q; want WARN %q", got.Level, got.Message, wantMsg)
	}
}

// TestPostGraphQL_TotalError_Errors pins the total-failure path: with no usable
// `data` alongside `errors[]` (the genuine-failure shape — bad query, cost
// ceiling, auth), PostGraphQL returns an error rather than handing callers an
// empty result. Covers null `data`, an absent `data` key, and that a multi-error
// response notes the dropped tail instead of silently showing only the first.
func TestPostGraphQL_TotalError_Errors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string // substrings the returned error must contain
	}{
		{
			name: "null data",
			body: `{"data":null,"errors":[{"type":"INSUFFICIENT_SCOPES","path":["viewer"],"message":"insufficient scopes"}]}`,
			want: []string{"insufficient scopes"},
		},
		{
			name: "absent data key",
			body: `{"errors":[{"type":"FORBIDDEN","path":["viewer"],"message":"forbidden field"}]}`,
			want: []string{"forbidden field"},
		},
		{
			name: "multiple errors note the dropped tail",
			body: `{"data":null,"errors":[{"message":"first"},{"message":"second"},{"message":"third"}]}`,
			want: []string{"first", "and 2 more"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := clientAgainst(graphqlServing(t, tt.body)).PostGraphQL(context.Background(), map[string]any{"query": "{ __typename }"})
			if err == nil {
				t.Fatalf("expected an error for %s, got nil", tt.name)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q; want it to contain %q", err.Error(), want)
				}
			}
		})
	}
}

// TestRequestCore_Table exercises the unified ctx-aware request core (TFAC-475):
// a 2xx returns the body; any non-2xx returns a *HTTPError carrying the exact
// status, body, and rendered message; and the Accept header defaults to the v3
// JSON media type but is overridable via GetRaw. Empty-token (no Authorization)
// is pinned separately by TestClient_EmptyToken_UnauthenticatedAcrossMethods.
func TestRequestCore_Table(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		accept     string // "" → Get (default Accept); else GetRaw with this Accept
		wantErr    bool
		wantStatus int
	}{
		{name: "2xx returns body", status: 200, body: `{"ok":true}`},
		{name: "404 returns typed error", status: 404, body: `{"message":"nope"}`, wantErr: true, wantStatus: 404},
		{name: "500 returns typed error", status: 500, body: "boom", wantErr: true, wantStatus: 500},
		{name: "GetRaw honors custom Accept", status: 200, body: "diff", accept: "application/vnd.github.v3.diff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAccept string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAccept = r.Header.Get("Accept")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			c := clientAgainst(srv.URL)

			var (
				data       []byte
				err        error
				wantAccept = "application/vnd.github.v3+json"
			)
			if tt.accept != "" {
				data, err = c.GetRaw(context.Background(), "/x", tt.accept)
				wantAccept = tt.accept
			} else {
				data, err = c.Get(context.Background(), "/x")
			}

			if gotAccept != wantAccept {
				t.Errorf("Accept header = %q, want %q", gotAccept, wantAccept)
			}
			if tt.wantErr {
				var he *HTTPError
				if !errors.As(err, &he) {
					t.Fatalf("want *HTTPError, got %T: %v", err, err)
				}
				if he.StatusCode != tt.wantStatus {
					t.Errorf("StatusCode = %d, want %d", he.StatusCode, tt.wantStatus)
				}
				if he.Body != tt.body {
					t.Errorf("Body = %q, want %q", he.Body, tt.body)
				}
				if want := fmt.Sprintf("GET /x returned %d: %s", tt.status, tt.body); he.Error() != want {
					t.Errorf("Error() = %q, want %q", he.Error(), want)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(data) != tt.body {
				t.Errorf("body = %q, want %q", string(data), tt.body)
			}
		})
	}
}

// blockingServer backs the cancellation tests: its handler signals `started`
// once, then holds the response open on a test-owned release channel that
// cleanup always closes. So the only way a client call returns is via its own
// ctx cancellation, never a server response — and crucially the handler is
// unblocked before srv.Close(), which otherwise hangs waiting for the in-flight
// connection (a client cancel does not reliably fire the server's r.Context()
// for a request whose body the handler never read).
func blockingServer(t *testing.T) (url string, started <-chan struct{}) {
	t.Helper()
	s := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		once.Do(func() { close(s) })
		<-release
	}))
	t.Cleanup(func() {
		close(release) // unblock the handler first, then Close() can drain
		srv.Close()
	})
	return srv.URL, s
}

// TestRequestCore_ContextCancellation proves the core actually honors ctx: the
// server holds the response open, and a mid-flight cancel makes the call return
// promptly with a context error rather than hanging to the 30s client timeout.
// This is the whole point of the ticket — before unification the do() family
// ignored ctx entirely.
func TestRequestCore_ContextCancellation(t *testing.T) {
	url, started := blockingServer(t)
	c := clientAgainst(url)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, "/slow")
		errc <- err
	}()

	<-started
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get did not return after ctx cancellation — the request is not ctx-aware")
	}
}

// TestDo_PostSurfacesHTTPError pins the error-typing half of the unification: a
// do-family call (Post) now returns a status-discriminable *HTTPError, where it
// used to return a plain fmt.Errorf. The rendered message is byte-identical to
// the old "%s %s returned %d: %s", so string-matching callers are unaffected
// and errors.As callers only gain accuracy.
func TestDo_PostSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"validation failed"}`))
	}))
	defer srv.Close()
	c := clientAgainst(srv.URL)

	_, err := c.Post(context.Background(), "/repos/o/r/pulls", map[string]any{"title": "x"})
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("Post must now surface a *HTTPError (do-family folded onto the typed core); got %T: %v", err, err)
	}
	if he.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("StatusCode = %d, want 422", he.StatusCode)
	}
	if want := `POST /repos/o/r/pulls returned 422: {"message":"validation failed"}`; he.Error() != want {
		t.Errorf("Error() = %q, want %q", he.Error(), want)
	}
}

// TestPostGraphQL_ContextCancellation pins the GraphQL path specifically: it has
// its own c.http.Do call rather than threading through request(), so it gets its
// own cancellation guard. A mid-flight cancel aborts it promptly instead of
// hanging to the 30s client timeout — the do-family-didn't-honor-ctx motivation
// applies to PostGraphQL too.
func TestPostGraphQL_ContextCancellation(t *testing.T) {
	url, started := blockingServer(t)
	c := clientAgainst(url)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := c.PostGraphQL(ctx, map[string]any{"query": "{ __typename }"})
		errc <- err
	}()

	<-started
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PostGraphQL did not return after ctx cancellation — the GraphQL path is not ctx-aware")
	}
}

// TestNewProxyClient_GraphQLFailsClosed verifies a credential-proxy client
// refuses GraphQL up front rather than silently misrouting it: the REST proxy
// its baseURL points at does not front the sibling GraphQL endpoint, so
// PostGraphQL must fail closed with no network attempt. A normal NewClient is
// unaffected — only the viaProxy flag trips the guard.
func TestNewProxyClient_GraphQLFailsClosed(t *testing.T) {
	c := NewProxyClient("http://10.42.5.1:34567", "per-run-placeholder")
	_, err := c.PostGraphQL(context.Background(), map[string]any{"query": "{ viewer { login } }"})
	if err == nil {
		t.Fatal("PostGraphQL on a proxy client must fail closed, got nil error")
	}
	if !strings.Contains(err.Error(), "credential proxy") {
		t.Fatalf("want the proxy-guard error, got %v", err)
	}
}
