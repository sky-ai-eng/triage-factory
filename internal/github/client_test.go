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
		{"do/Get", func() error { _, err := c.Get("/x"); return err }},
		{"GetConditional", func() error { _, _, _, err := c.GetConditional("/x", ""); return err }},
		{"GetRaw", func() error { _, err := c.GetRaw("/x", "application/json"); return err }},
		{"DownloadArtifact", func() error {
			_, err := c.DownloadArtifact(context.Background(), "/x", &strings.Builder{}, 1<<20)
			return err
		}},
		{"PostGraphQL", func() error { _, err := c.PostGraphQL(map[string]any{"query": "{__typename}"}); return err }},
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
	data, err := clientAgainst(graphqlServing(t, body)).PostGraphQL(map[string]any{"query": "{ __typename }"})
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
			_, err := clientAgainst(graphqlServing(t, tt.body)).PostGraphQL(map[string]any{"query": "{ __typename }"})
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
