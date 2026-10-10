package apiproxy_test

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

	"github.com/sky-ai-eng/triage-factory/internal/apiproxy"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// secretDetail stands in for the credential-setup detail a real source error
// can carry. It must reach neither the caller nor the log.
const secretDetail = "app 4242 installation 777"

// switchableErr is a source failure the test can change between requests.
type switchableErr struct {
	mu  sync.Mutex
	err error
}

func (s *switchableErr) set(sentinel error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = fmt.Errorf("resolve for %s: %w", secretDetail, sentinel)
}

func (s *switchableErr) get() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// TestCredentialMiss pins the answer to a failed credential lookup for every
// provider: the status says whether a retry can help, the JSON message names
// the reason and nothing more, and the operator gets one Warn per reason
// however often the caller retries.
func TestCredentialMiss(t *testing.T) {
	// Each provider's requests go to a path it forwards: Linear forwards only
	// its GraphQL endpoint, and anything else is refused before a credential
	// is looked up, so a miss could not happen there.
	missPath := map[apiproxy.Provider]string{
		apiproxy.ProviderGitHub: "/repos/acme/hidden-widgets/pulls/1",
		apiproxy.ProviderJira:   "/repos/acme/hidden-widgets/pulls/1",
		apiproxy.ProviderLinear: "/graphql",
	}
	providers := map[apiproxy.Provider]func(upstream string, src *switchableErr) apiproxy.Config{
		apiproxy.ProviderGitHub: func(upstream string, src *switchableErr) apiproxy.Config {
			return apiproxy.Config{
				Provider: apiproxy.ProviderGitHub,
				Upstream: upstream,
				TokenSource: func(context.Context, string, string) (string, error) {
					return "", src.get()
				},
				IncomingToken:  "run-placeholder",
				ConversationID: "conv-miss",
			}
		},
		apiproxy.ProviderJira: func(upstream string, src *switchableErr) apiproxy.Config {
			return apiproxy.Config{
				Provider: apiproxy.ProviderJira,
				Upstream: upstream,
				AuthHeaderSource: func(context.Context) (string, error) {
					return "", src.get()
				},
				IncomingToken:  "run-placeholder",
				ConversationID: "conv-miss",
			}
		},
		apiproxy.ProviderLinear: func(upstream string, src *switchableErr) apiproxy.Config {
			return apiproxy.Config{
				Provider: apiproxy.ProviderLinear,
				Upstream: upstream,
				AuthHeaderSource: func(context.Context) (string, error) {
					return "", src.get()
				},
				IncomingToken:  "run-placeholder",
				ConversationID: "conv-miss",
			}
		},
	}
	cases := []struct {
		sentinel error
		reason   string
		status   int
	}{
		{credbundle.ErrNoBundle, "no_bundle", http.StatusBadGateway},
		{credbundle.ErrNoRepoToken, "no_repo_token", http.StatusForbidden},
		{credbundle.ErrNoCLIToken, "no_cli_token", http.StatusForbidden},
		{credbundle.ErrNoJiraCredential, "no_jira_credential", http.StatusForbidden},
		{credbundle.ErrNoLinearCredential, "no_linear_credential", http.StatusForbidden},
		{credbundle.ErrTokenExpiring, "token_expiring", http.StatusBadGateway},
		{errors.New("resolver outage"), "other", http.StatusBadGateway},
	}
	for provider, cfg := range providers {
		for _, tc := range cases {
			t.Run(string(provider)+"/"+tc.reason, func(t *testing.T) {
				var logs bytes.Buffer
				defer logging.SetOutput(&logs)()

				rec := &upstreamRecord{}
				upstream := fakeUpstream(rec)
				defer upstream.Close()
				src := &switchableErr{}
				src.set(tc.sentinel)
				proxyURL := startProxy(t, cfg(upstream.URL, src))

				// A run holds one proxy per provider, and a missing bundle fails
				// both, so the name has to say which one answered.
				name := "apiproxy-" + string(provider)
				for range 3 {
					status, msg, raw := doMiss(t, proxyURL, missPath[provider])
					if status != tc.status {
						t.Errorf("status = %d, want %d", status, tc.status)
					}
					if !strings.HasPrefix(msg, name+": ") {
						t.Errorf("message %q does not name the proxy %q", msg, name)
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

				second := credbundle.ErrNoBundle
				if tc.sentinel == second {
					second = credbundle.ErrNoRepoToken
				}
				src.set(second)
				doMiss(t, proxyURL, missPath[provider])
				doMiss(t, proxyURL, missPath[provider])

				logged := logs.String()
				if n := strings.Count(logged, "credential lookup failed"); n != 2 {
					t.Fatalf("a second reason took the count to %d, want 2:\n%s", n, logged)
				}
				for _, want := range []string{"WARN", name, "conv-miss", tc.reason} {
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
}

// doMiss sends one request to path, which for the REST providers names a
// repository that must never reach the log, and returns the status, the
// decoded message and the raw body.
func doMiss(t *testing.T, proxyURL, path string) (int, string, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, proxyURL+path, nil)
	req.Header.Set("Authorization", "Bearer run-placeholder")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy roundtrip: %v", err)
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
