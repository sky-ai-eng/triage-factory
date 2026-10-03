package gitproxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// switchableSource fails with whatever error the test last set.
type switchableSource struct {
	mu  sync.Mutex
	err error
}

func (s *switchableSource) set(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *switchableSource) source(context.Context, string, string) (gitproxy.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return gitproxy.Token{}, s.err
}

// secretDetail stands in for the App or credential-setup detail a real source
// error can carry. It must reach neither the agent nor the log.
const secretDetail = "app 4242 installation 777"

func missError(sentinel error) error {
	return fmt.Errorf("mint for %s: %w", secretDetail, sentinel)
}

// getMiss sends one git request for a repository whose name must never reach
// the log, and returns the status and the decoded message.
func getMiss(t *testing.T, proxyURL string) (int, string, string) {
	t.Helper()
	resp, err := http.Get(proxyURL + "/acme/hidden-widgets.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body is not a JSON object: %v (%q)", err, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	return resp.StatusCode, body.Message, string(raw)
}

// TestProxyCredentialMiss pins the answer to a failed credential lookup: the
// status says whether a retry can help (git prints a 502 as an outage and a 403
// as a refusal), the JSON message names the reason and nothing more, and the
// operator gets one Warn per reason however often the agent retries.
func TestProxyCredentialMiss(t *testing.T) {
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

			rec := &fakeUpstreamRecord{}
			upstream := fakeGitHub(rec)
			defer upstream.Close()
			src := &switchableSource{}
			src.set(missError(tc.sentinel))
			proxyURL := startMissProxy(t, src.source, upstream.URL)

			for range 3 {
				status, msg, raw := getMiss(t, proxyURL)
				if status != tc.status {
					t.Errorf("status = %d, want %d", status, tc.status)
				}
				if !strings.Contains(msg, "("+tc.reason+")") {
					t.Errorf("message %q does not name reason %q", msg, tc.reason)
				}
				if strings.Contains(raw, secretDetail) {
					t.Errorf("body leaks the source error: %s", raw)
				}
			}
			if n := strings.Count(logs.String(), "credential lookup failed"); n != 1 {
				t.Fatalf("three identical misses logged %d lines, want 1:\n%s", n, logs.String())
			}

			// A different reason is news; it logs once more.
			second := credbundle.ErrNoBundle
			if tc.sentinel == second {
				second = credbundle.ErrNoRepoToken
			}
			src.set(missError(second))
			getMiss(t, proxyURL)
			getMiss(t, proxyURL)

			logged := logs.String()
			if n := strings.Count(logged, "credential lookup failed"); n != 2 {
				t.Fatalf("a second reason took the count to %d, want 2:\n%s", n, logged)
			}
			for _, want := range []string{"WARN", "gitproxy", "conv-miss", tc.reason} {
				if !strings.Contains(logged, want) {
					t.Errorf("log missing %q:\n%s", want, logged)
				}
			}
			for _, leak := range []string{secretDetail, "acme", "hidden-widgets"} {
				if strings.Contains(logged, leak) {
					t.Errorf("log leaks %q:\n%s", leak, logged)
				}
			}
			if rec.hits.Load() != 0 {
				t.Errorf("upstream hits = %d, want 0", rec.hits.Load())
			}
		})
	}
}

// TestProxyNearExpiryTokenIsTokenExpiring pins the proxy's own refusal of a
// token too close to expiry: it is a miss a refresh can still answer, so it
// keeps the 502 and names itself.
func TestProxyNearExpiryTokenIsTokenExpiring(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()
	ts := &constantTokenSource{value: "ghs_NEARLY", expiresAt: time.Now().Add(time.Minute)}
	proxyURL := startMissProxy(t, ts.source, upstream.URL)

	status, msg, _ := getMiss(t, proxyURL)
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
	if !strings.Contains(msg, "(token_expiring)") {
		t.Errorf("message %q does not name token_expiring", msg)
	}
	if rec.hits.Load() != 0 {
		t.Errorf("upstream hits = %d, want 0", rec.hits.Load())
	}
}

func startMissProxy(t *testing.T, ts gitproxy.TokenSource, upstream string) string {
	t.Helper()
	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource:    ts,
		Upstream:       upstream,
		ConversationID: "conv-miss",
	})
	if err != nil {
		t.Fatalf("gitproxy.New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Server.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return "http://" + addr
}
