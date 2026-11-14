package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// TestClient_DependabotAlertsAccess covers each answer the list request can
// give, and the responses that give none.
func TestClient_DependabotAlertsAccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   domain.DependabotAlertsAccess
	}{
		{"readable, no alerts", http.StatusOK, `[]`, domain.DependabotAlertsReadable},
		{"readable, alerts", http.StatusOK, `[{"number":1,"state":"open"}]`, domain.DependabotAlertsReadable},
		{"alerts turned off", http.StatusForbidden,
			`{"message":"Dependabot alerts are disabled for this repository.","documentation_url":"https://docs.github.com/rest/dependabot/alerts#list-dependabot-alerts-for-a-repository","status":"403"}`,
			domain.DependabotAlertsDisabled},
		{"archived repository", http.StatusForbidden,
			`{"message":"Dependabot alerts are not available for archived repositories.","status":"403"}`,
			domain.DependabotAlertsDisabled},
		{"App without the permission", http.StatusForbidden,
			`{"message":"Resource not accessible by integration","status":"403"}`,
			domain.DependabotAlertsNoPermission},
		{"fine-grained token without the permission", http.StatusForbidden,
			`{"message":"Resource not accessible by personal access token","status":"403"}`,
			domain.DependabotAlertsNoPermission},
		{"not found", http.StatusNotFound, `{"message":"Not Found","status":"404"}`, domain.DependabotAlertsNoPermission},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotURI string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotURI = r.URL.RequestURI()
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			got, err := NewClient(srv.URL, "tok").DependabotAlertsAccess(context.Background(), "octo", "api")
			if err != nil {
				t.Fatalf("DependabotAlertsAccess: %v", err)
			}
			if got != tc.want {
				t.Errorf("access = %q, want %q", got, tc.want)
			}
			if want := "/repos/octo/api/dependabot/alerts?state=open&per_page=1"; !strings.HasSuffix(gotURI, want) {
				t.Errorf("request = %q, want it to end %q", gotURI, want)
			}
		})
	}

	t.Run("a 403 that is not GitHub's is no answer", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<html>Access denied</html>`))
		}))
		t.Cleanup(srv.Close)
		got, err := NewClient(srv.URL, "tok").DependabotAlertsAccess(context.Background(), "octo", "api")
		if err == nil || got != "" {
			t.Errorf("access = %q, err = %v; want no answer and an error", got, err)
		}
	})

	t.Run("a server error is no answer", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		t.Cleanup(srv.Close)
		got, err := NewClient(srv.URL, "tok").DependabotAlertsAccess(context.Background(), "octo", "api")
		if err == nil || got != "" {
			t.Errorf("access = %q, err = %v; want no answer and an error", got, err)
		}
	})

	t.Run("a rate limit is no answer", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		}))
		t.Cleanup(srv.Close)
		got, err := NewClient(srv.URL, "tok").DependabotAlertsAccess(context.Background(), "octo", "api")
		var rl *ErrRateLimited
		if !errors.As(err, &rl) || got != "" {
			t.Errorf("access = %q, err = %v; want no answer and *ErrRateLimited", got, err)
		}
	})
}

// TestClient_DependabotAlertsAccess_CountsARefusalAsAnswered: GitHub's 403 to
// the probe is counted as an answered request, not a refused credential, and
// the same 403 to any other request still is one.
func TestClient_DependabotAlertsAccess_CountsARefusalAsAnswered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration","status":"403"}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "tok")
	ctx, tally := upstream.WithTally(context.Background())

	if _, err := c.DependabotAlertsAccess(ctx, "octo", "api"); err != nil {
		t.Fatalf("DependabotAlertsAccess: %v", err)
	}
	if auth, rejected := tally.Count(upstream.Auth), tally.Count(upstream.Rejected); auth != 0 || rejected != 1 {
		t.Errorf("probe counted auth=%d rejected=%d, want auth=0 rejected=1", auth, rejected)
	}

	if _, err := c.Get(ctx, "/repos/octo/api"); err == nil {
		t.Fatal("Get: want the 403 as an error")
	}
	if auth := tally.Count(upstream.Auth); auth != 1 {
		t.Errorf("another request's 403 counted auth=%d, want 1", auth)
	}
}
