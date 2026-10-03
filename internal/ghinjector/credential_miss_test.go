package ghinjector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// missSecretDetail stands in for the credential-setup detail a real source
// error can carry. It must reach neither the agent nor the log.
const missSecretDetail = "app 4242 installation 777"

// TestInjector_CredentialMiss pins the answer to a failed credential lookup:
// the status says whether a retry can help, the JSON message — the member gh
// prints — names the reason and nothing more, and the operator gets one Warn
// per reason however often the agent retries.
func TestInjector_CredentialMiss(t *testing.T) {
	cases := []struct {
		sentinel error
		reason   string
		status   int
	}{
		{credbundle.ErrNoBundle, "no_bundle", http.StatusBadGateway},
		{credbundle.ErrNoRepoToken, "no_repo_token", http.StatusForbidden},
		{credbundle.ErrNoCLIToken, "no_cli_token", http.StatusForbidden},
		{credbundle.ErrNoJiraCredential, "no_jira_credential", http.StatusForbidden},
		{credbundle.ErrTokenExpiring, "token_expiring", http.StatusBadGateway},
		{errors.New("resolver outage"), "other", http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			var logs bytes.Buffer
			defer logging.SetOutput(&logs)()

			reached := false
			upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
			defer upstream.Close()

			var mu sync.Mutex
			srcErr := fmt.Errorf("resolve for %s: %w", missSecretDetail, tc.sentinel)
			srv, err := New(Config{
				Upstream:       upstream.URL,
				ConversationID: "conv-miss",
				TokenSource: func(context.Context) (string, error) {
					mu.Lock()
					defer mu.Unlock()
					return "", srcErr
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			for range 3 {
				status, msg, raw := serveMiss(t, srv)
				if status != tc.status {
					t.Errorf("status = %d, want %d", status, tc.status)
				}
				if !strings.Contains(msg, "("+tc.reason+")") {
					t.Errorf("message %q does not name reason %q", msg, tc.reason)
				}
				if strings.Contains(raw, missSecretDetail) {
					t.Errorf("body leaks the source error: %s", raw)
				}
			}
			if n := strings.Count(logs.String(), "credential lookup failed"); n != 1 {
				t.Fatalf("three identical misses logged %d lines, want 1:\n%s", n, logs.String())
			}

			second := credbundle.ErrNoBundle
			if tc.sentinel == second {
				second = credbundle.ErrNoCLIToken
			}
			mu.Lock()
			srcErr = fmt.Errorf("resolve for %s: %w", missSecretDetail, second)
			mu.Unlock()
			serveMiss(t, srv)
			serveMiss(t, srv)

			logged := logs.String()
			if n := strings.Count(logged, "credential lookup failed"); n != 2 {
				t.Fatalf("a second reason took the count to %d, want 2:\n%s", n, logged)
			}
			for _, want := range []string{"WARN", "ghinjector", "conv-miss", tc.reason} {
				if !strings.Contains(logged, want) {
					t.Errorf("log missing %q:\n%s", want, logged)
				}
			}
			for _, leak := range []string{missSecretDetail, "acme", "hidden-widgets"} {
				if strings.Contains(logged, leak) {
					t.Errorf("log leaks %q:\n%s", leak, logged)
				}
			}
			if reached {
				t.Error("forwarded upstream despite no resolvable credential")
			}
		})
	}
}

// TestInjector_EmptyTokenIsAMiss pins that a source answering with an empty
// token and no error is still refused, under the reason no sentinel names.
func TestInjector_EmptyTokenIsAMiss(t *testing.T) {
	srv, err := New(Config{
		Upstream:    "https://api.github.com",
		TokenSource: func(context.Context) (string, error) { return "", nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	status, msg, _ := serveMiss(t, srv)
	if status != http.StatusBadGateway || !strings.Contains(msg, "(other)") {
		t.Errorf("got %d %q, want 502 naming reason other", status, msg)
	}
}

// serveMiss sends one request, naming a repository that must never reach the
// log, straight to the handler, and returns the status, the decoded message
// and the raw body.
func serveMiss(t *testing.T, srv *Server) (int, string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v3/repos/acme/hidden-widgets", nil))
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not a JSON object: %v (%q)", err, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	return rec.Code, body.Message, rec.Body.String()
}
