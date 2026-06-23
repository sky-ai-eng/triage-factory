package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
// every level and keeps records in order. Only used from non-parallel tests, so
// it needs no locking.
type captureHandler struct {
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	data, err := clientAgainst(srv.URL).PostGraphQL(map[string]any{"query": "{ __typename }"})
	if err != nil {
		t.Fatalf("PostGraphQL with partial data should not error, got %v", err)
	}
	if !strings.Contains(string(data), `"number":1`) {
		t.Errorf("expected the usable data to pass through; got %s", string(data))
	}

	if len(logs.records) != 1 {
		t.Fatalf("expected exactly one log record, got %d", len(logs.records))
	}
	const wantMsg = "GraphQL partial error; using partial data"
	if got := logs.records[0]; got.Level != slog.LevelWarn || got.Message != wantMsg {
		t.Errorf("log = %v %q; want WARN %q", got.Level, got.Message, wantMsg)
	}
}

// TestPostGraphQL_TotalError_Errors pins the total-failure path: a response with
// null `data` alongside `errors[]` (the genuine-failure shape — bad query, cost
// ceiling, auth) returns an error rather than handing callers an empty result.
func TestPostGraphQL_TotalError_Errors(t *testing.T) {
	body := `{"data":null,"errors":[{"type":"INSUFFICIENT_SCOPES","path":["viewer"],"message":"insufficient scopes"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	_, err := clientAgainst(srv.URL).PostGraphQL(map[string]any{"query": "{ __typename }"})
	if err == nil {
		t.Fatal("PostGraphQL with null data and errors should return an error, got nil")
	}
	if !strings.Contains(err.Error(), "insufficient scopes") {
		t.Errorf("expected the error to surface the GraphQL message; got %q", err.Error())
	}
}
