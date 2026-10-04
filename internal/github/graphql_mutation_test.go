package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestGraphQLMutations_AreSentOnce: a GraphQL mutation that meets a 503 may
// have been applied before the failure, so it is returned to the caller after
// one attempt rather than replayed, exactly like a REST write. The server
// would answer the second attempt, which is how a replay would show.
func TestGraphQLMutations_AreSentOnce(t *testing.T) {
	SetTransientBackoffForTest(t, time.Millisecond)
	for _, tc := range []struct {
		name string
		call func(context.Context, *Client) error
	}{
		{name: "MarkPRReady", call: func(ctx context.Context, c *Client) error { return c.MarkPRReady(ctx, "o", "r", 7) }},
		{name: "ConvertPRToDraft", call: func(ctx context.Context, c *Client) error { return c.ConvertPRToDraft(ctx, "o", "r", 7) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/graphql" {
					_, _ = w.Write([]byte(`{"node_id":"PR_x"}`))
					return
				}
				if posts.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"message":"Service Unavailable"}`))
					return
				}
				_, _ = w.Write([]byte(`{"data":{"pr":{"pullRequest":{"isDraft":false}}}}`))
			}))
			t.Cleanup(srv.Close)

			err := tc.call(context.Background(), clientAgainst(srv.URL))
			var he *HTTPError
			if !errors.As(err, &he) || he.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("err = %v, want the 503 returned as an *HTTPError", err)
			}
			if got := posts.Load(); got != 1 {
				t.Errorf("the mutation was sent %d times, want 1", got)
			}
		})
	}
}

// TestPostGraphQL_QueryRetriesTransient is the other half: a query is a read,
// so it keeps the GET retry policy and recovers from the same 503.
func TestPostGraphQL_QueryRetriesTransient(t *testing.T) {
	SetTransientBackoffForTest(t, time.Millisecond)
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if posts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"x"}}}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := clientAgainst(srv.URL).PostGraphQL(context.Background(), map[string]any{"query": "{ viewer { login } }"}); err != nil {
		t.Fatalf("PostGraphQL: %v", err)
	}
	if got := posts.Load(); got != 2 {
		t.Errorf("the query was sent %d times, want 2", got)
	}
}
