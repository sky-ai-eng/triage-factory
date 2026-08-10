package apiproxy_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/apiproxy"
)

// upstreamRecord captures what the fake upstream observed so tests can
// assert the proxy rewrote (and didn't rewrite) the right things.
type upstreamRecord struct {
	hits atomic.Int64

	mu       sync.Mutex
	method   string
	path     string
	rawQuery string
	header   http.Header
	body     string
}

// fakeUpstream stands in for api.github.com or a Jira base. Records what
// arrived, returns a canned JSON response.
func fakeUpstream(rec *upstreamRecord) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.rawQuery = r.URL.RawQuery
		rec.header = r.Header.Clone()
		rec.body = string(body)
		rec.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
}

// startProxy boots a Server via the production Start path and returns
// its base URL. Shutdown is registered on t.Cleanup.
func startProxy(t *testing.T, cfg apiproxy.Config) string {
	t.Helper()
	srv, err := apiproxy.New(cfg)
	if err != nil {
		t.Fatalf("apiproxy.New: %v", err)
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

func TestNewValidation(t *testing.T) {
	validTokenSource := func(context.Context, string, string) (string, error) { return "tok", nil }
	validHeaderSource := apiproxy.JiraBearer("pat")

	tests := []struct {
		name    string
		cfg     apiproxy.Config
		wantErr string // empty = expect success
	}{
		{
			name: "github https upstream ok",
			cfg:  apiproxy.Config{Provider: apiproxy.ProviderGitHub, Upstream: "https://api.github.com", TokenSource: validTokenSource},
		},
		{
			name: "jira https upstream ok",
			cfg:  apiproxy.Config{Provider: apiproxy.ProviderJira, Upstream: "https://jira.example.com", AuthHeaderSource: validHeaderSource},
		},
		{
			name: "loopback http upstream ok for tests",
			cfg:  apiproxy.Config{Provider: apiproxy.ProviderGitHub, Upstream: "http://127.0.0.1:9999", TokenSource: validTokenSource},
		},
		{
			// A base path IS allowed now — SetURL joins it ahead of the
			// incoming path so a GHES /api/v3 or Jira Cloud /ex/jira/{id}
			// upstream routes while the client points at the bare proxy URL.
			name: "upstream with base path accepted",
			cfg:  apiproxy.Config{Provider: apiproxy.ProviderGitHub, Upstream: "https://ghes.example.com/api/v3", TokenSource: validTokenSource},
		},
		{
			name:    "upstream with query rejected",
			cfg:     apiproxy.Config{Provider: apiproxy.ProviderGitHub, Upstream: "https://api.github.com?x=1", TokenSource: validTokenSource},
			wantErr: "query or fragment",
		},
		{
			name:    "upstream with fragment rejected",
			cfg:     apiproxy.Config{Provider: apiproxy.ProviderGitHub, Upstream: "https://api.github.com#frag", TokenSource: validTokenSource},
			wantErr: "query or fragment",
		},
		{
			name:    "non-loopback http upstream rejected",
			cfg:     apiproxy.Config{Provider: apiproxy.ProviderGitHub, Upstream: "http://api.github.com", TokenSource: validTokenSource},
			wantErr: "must use https",
		},
		{
			name:    "empty upstream rejected",
			cfg:     apiproxy.Config{Provider: apiproxy.ProviderGitHub, TokenSource: validTokenSource},
			wantErr: "Upstream is required",
		},
		{
			name:    "upstream missing scheme rejected",
			cfg:     apiproxy.Config{Provider: apiproxy.ProviderGitHub, Upstream: "api.github.com", TokenSource: validTokenSource},
			wantErr: "missing scheme or host",
		},
		{
			name:    "github without TokenSource rejected",
			cfg:     apiproxy.Config{Provider: apiproxy.ProviderGitHub, Upstream: "https://api.github.com"},
			wantErr: "TokenSource is required",
		},
		{
			name:    "jira without AuthHeaderSource rejected",
			cfg:     apiproxy.Config{Provider: apiproxy.ProviderJira, Upstream: "https://jira.example.com"},
			wantErr: "AuthHeaderSource is required",
		},
		{
			name:    "github with AuthHeaderSource rejected",
			cfg:     apiproxy.Config{Provider: apiproxy.ProviderGitHub, Upstream: "https://api.github.com", TokenSource: validTokenSource, AuthHeaderSource: validHeaderSource},
			wantErr: "AuthHeaderSource is only for ProviderJira",
		},
		{
			name:    "jira with TokenSource rejected",
			cfg:     apiproxy.Config{Provider: apiproxy.ProviderJira, Upstream: "https://jira.example.com", AuthHeaderSource: validHeaderSource, TokenSource: validTokenSource},
			wantErr: "TokenSource is only for ProviderGitHub",
		},
		{
			name:    "unknown provider rejected",
			cfg:     apiproxy.Config{Provider: "gitlab", Upstream: "https://gitlab.example.com"},
			wantErr: "unsupported provider",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := apiproxy.New(tt.cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("New() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("New() = nil error, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("New() error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestGitHubPerRepoInjection is the load-bearing assertion of the GitHub
// path: the token the upstream sees is selected by the (owner, repo) the
// request path targets, and paths that carry no repo fall back to the
// source's default via the empty pair.
func TestGitHubPerRepoInjection(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		wantOwner string
		wantRepo  string
	}{
		{name: "repo-scoped nested path", path: "/repos/octo/widgets/pulls/42/files", wantOwner: "octo", wantRepo: "widgets"},
		{name: "repo root path", path: "/repos/octo/widgets", wantOwner: "octo", wantRepo: "widgets"},
		{name: "non-repo path falls back to default", path: "/user"},
		{name: "repositories-by-id falls back to default", path: "/repositories/1296269"},
		{name: "repos prefix without repo segment falls back", path: "/repos/octo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &upstreamRecord{}
			upstream := fakeUpstream(rec)
			defer upstream.Close()

			var gotOwner, gotRepo string
			proxyURL := startProxy(t, apiproxy.Config{
				Provider: apiproxy.ProviderGitHub,
				Upstream: upstream.URL,
				TokenSource: func(_ context.Context, owner, repo string) (string, error) {
					gotOwner, gotRepo = owner, repo
					if owner == "" && repo == "" {
						return "tok-default", nil
					}
					return "tok-" + owner + "-" + repo, nil
				},
				IncomingToken: "run-placeholder",
			})

			req, _ := http.NewRequest(http.MethodGet, proxyURL+tt.path, nil)
			req.Header.Set("Authorization", "Bearer run-placeholder")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("proxy roundtrip: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			if gotOwner != tt.wantOwner || gotRepo != tt.wantRepo {
				t.Errorf("TokenSource called with (%q, %q), want (%q, %q)", gotOwner, gotRepo, tt.wantOwner, tt.wantRepo)
			}
			wantAuth := "Bearer tok-default"
			if tt.wantOwner != "" {
				wantAuth = "Bearer tok-" + tt.wantOwner + "-" + tt.wantRepo
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if got := rec.header.Get("Authorization"); got != wantAuth {
				t.Errorf("upstream Authorization = %q, want %q (placeholder must be replaced)", got, wantAuth)
			}
		})
	}
}

// TestForwardsRequestVerbatim pins the pass-through contract: method,
// path, query, body, and REST-relevant headers (Accept, Content-Type,
// If-None-Match) all reach the upstream unchanged; only Authorization is
// swapped, and proxy-chain headers never appear.
func TestForwardsRequestVerbatim(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec)
	defer upstream.Close()

	proxyURL := startProxy(t, apiproxy.Config{
		Provider: apiproxy.ProviderGitHub,
		Upstream: upstream.URL,
		TokenSource: func(context.Context, string, string) (string, error) {
			return "real-token", nil
		},
		IncomingToken: "run-placeholder",
	})

	body := strings.NewReader(`{"body":"looks good"}`)
	req, _ := http.NewRequest(http.MethodPost, proxyURL+"/repos/octo/widgets/issues/7/comments?per_page=50&page=2", body)
	req.Header.Set("Authorization", "Bearer run-placeholder")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-None-Match", `"etag-123"`)
	req.Header.Set("X-Forwarded-For", "10.0.0.1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy roundtrip: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if string(respBody) != `{"ok":true}` {
		t.Errorf("response body = %q, want upstream body passed through", respBody)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.method != http.MethodPost {
		t.Errorf("upstream method = %q, want POST", rec.method)
	}
	if rec.path != "/repos/octo/widgets/issues/7/comments" {
		t.Errorf("upstream path = %q, want the request path verbatim", rec.path)
	}
	if rec.rawQuery != "per_page=50&page=2" {
		t.Errorf("upstream query = %q, want the request query verbatim", rec.rawQuery)
	}
	if rec.body != `{"body":"looks good"}` {
		t.Errorf("upstream body = %q, want the request body verbatim", rec.body)
	}
	if got := rec.header.Get("Authorization"); got != "Bearer real-token" {
		t.Errorf("upstream Authorization = %q, want the injected credential", got)
	}
	if got := rec.header.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("upstream Accept = %q, want caller's value preserved", got)
	}
	if got := rec.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("upstream Content-Type = %q, want caller's value preserved", got)
	}
	if got := rec.header.Get("If-None-Match"); got != `"etag-123"` {
		t.Errorf("upstream If-None-Match = %q, want caller's value preserved", got)
	}
	if got := rec.header.Get("X-Forwarded-For"); got != "" {
		t.Errorf("upstream X-Forwarded-For = %q, want stripped", got)
	}
}

// TestForwardsUnderUpstreamBasePath pins the path-mounted-upstream contract
// that makes the executor's bare-proxy-URL clients reach GHES (/api/v3) and
// Jira Cloud (/ex/jira/{id}): a request the caller sends to "/repos/o/r/…"
// arrives at the upstream under its base path, "/api/v3/repos/o/r/…", with the
// caller's query preserved and only Authorization swapped.
func TestForwardsUnderUpstreamBasePath(t *testing.T) {
	rec := &upstreamRecord{}
	upstream := fakeUpstream(rec)
	defer upstream.Close()

	proxyURL := startProxy(t, apiproxy.Config{
		Provider: apiproxy.ProviderGitHub,
		Upstream: upstream.URL + "/api/v3", // GHES REST root as a base path
		TokenSource: func(_ context.Context, owner, repo string) (string, error) {
			if owner != "octo" || repo != "widgets" {
				t.Errorf("repo parsed from path = %s/%s, want octo/widgets — base path must not shift repo parsing", owner, repo)
			}
			return "real-token", nil
		},
		IncomingToken: "run-placeholder",
	})

	req, _ := http.NewRequest(http.MethodGet, proxyURL+"/repos/octo/widgets/pulls/3?state=open", nil)
	req.Header.Set("Authorization", "Bearer run-placeholder")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy roundtrip: %v", err)
	}
	resp.Body.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.path != "/api/v3/repos/octo/widgets/pulls/3" {
		t.Errorf("upstream path = %q, want the request path under the upstream base /api/v3", rec.path)
	}
	if rec.rawQuery != "state=open" {
		t.Errorf("upstream query = %q, want the caller's query preserved", rec.rawQuery)
	}
	if got := rec.header.Get("Authorization"); got != "Bearer real-token" {
		t.Errorf("upstream Authorization = %q, want the injected credential", got)
	}
}

// TestJiraInjection pins both Jira auth shapes: Cloud (Basic
// base64(email:api_token), presented by the caller as a Basic password
// placeholder) and Data Center (Bearer PAT, presented as a Bearer
// placeholder).
func TestJiraInjection(t *testing.T) {
	tests := []struct {
		name     string
		source   apiproxy.AuthHeaderSource
		path     string
		auth     func(*http.Request) // how the caller presents the placeholder
		wantAuth string
	}{
		{
			name:   "cloud basic",
			source: apiproxy.JiraBasic("dev@example.com", "cloud-api-token"),
			path:   "/rest/api/3/issue/SKY-1",
			auth: func(r *http.Request) {
				r.SetBasicAuth("dev@example.com", "run-placeholder")
			},
			wantAuth: "Basic " + base64.StdEncoding.EncodeToString([]byte("dev@example.com:cloud-api-token")),
		},
		{
			name:   "data center bearer",
			source: apiproxy.JiraBearer("dc-pat"),
			path:   "/rest/api/2/issue/SKY-1",
			auth: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer run-placeholder")
			},
			wantAuth: "Bearer dc-pat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &upstreamRecord{}
			upstream := fakeUpstream(rec)
			defer upstream.Close()

			proxyURL := startProxy(t, apiproxy.Config{
				Provider:         apiproxy.ProviderJira,
				Upstream:         upstream.URL,
				AuthHeaderSource: tt.source,
				IncomingToken:    "run-placeholder",
			})

			req, _ := http.NewRequest(http.MethodGet, proxyURL+tt.path, nil)
			tt.auth(req)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("proxy roundtrip: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			rec.mu.Lock()
			defer rec.mu.Unlock()
			if rec.path != tt.path {
				t.Errorf("upstream path = %q, want %q", rec.path, tt.path)
			}
			if got := rec.header.Get("Authorization"); got != tt.wantAuth {
				t.Errorf("upstream Authorization = %q, want %q", got, tt.wantAuth)
			}
		})
	}
}

// TestCallerAuthGate is the cross-run isolation assertion: only the
// exact per-run placeholder gets through; everything else is 401 and
// never touches the token source or the upstream.
func TestCallerAuthGate(t *testing.T) {
	tests := []struct {
		name       string
		auth       func(*http.Request)
		wantStatus int
	}{
		{
			name:       "correct bearer placeholder",
			auth:       func(r *http.Request) { r.Header.Set("Authorization", "Bearer run-placeholder") },
			wantStatus: http.StatusOK,
		},
		{
			name:       "correct basic password placeholder",
			auth:       func(r *http.Request) { r.SetBasicAuth("x-run", "run-placeholder") },
			wantStatus: http.StatusOK,
		},
		{
			name:       "wrong bearer token",
			auth:       func(r *http.Request) { r.Header.Set("Authorization", "Bearer sibling-run-token") },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong basic password",
			auth:       func(r *http.Request) { r.SetBasicAuth("x-run", "sibling-run-token") },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing authorization header",
			auth:       func(*http.Request) {},
			wantStatus: http.StatusUnauthorized,
		},
		{
			// Knowledge of the per-run secret is the property being
			// authenticated, not the scheme spelling — same latitude as
			// the llmproxy template's Bearer path.
			name:       "raw placeholder without scheme still authenticates",
			auth:       func(r *http.Request) { r.Header.Set("Authorization", "run-placeholder") },
			wantStatus: http.StatusOK,
		},
		{
			name:       "bearer scheme wrapping a non-secret value",
			auth:       func(r *http.Request) { r.Header.Set("Authorization", "Bearer Bearer run-placeholder") },
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &upstreamRecord{}
			upstream := fakeUpstream(rec)
			defer upstream.Close()

			var sourceCalls atomic.Int64
			proxyURL := startProxy(t, apiproxy.Config{
				Provider: apiproxy.ProviderGitHub,
				Upstream: upstream.URL,
				TokenSource: func(context.Context, string, string) (string, error) {
					sourceCalls.Add(1)
					return "real-token", nil
				},
				IncomingToken: "run-placeholder",
			})

			req, _ := http.NewRequest(http.MethodGet, proxyURL+"/repos/octo/widgets", nil)
			tt.auth(req)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("proxy roundtrip: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusUnauthorized {
				if rec.hits.Load() != 0 {
					t.Errorf("upstream hits = %d, want 0 (rejected request must never be forwarded)", rec.hits.Load())
				}
				if sourceCalls.Load() != 0 {
					t.Errorf("TokenSource calls = %d, want 0 (rejection must precede credential resolution)", sourceCalls.Load())
				}
			}
		})
	}
}

// TestSourceFailureIs502 pins the fail-closed contract: a broken
// credential pipeline (source error or empty result) 502s and the
// request is never forwarded — not even unauthenticated.
func TestSourceFailureIs502(t *testing.T) {
	tests := []struct {
		name string
		cfg  func(upstream string) apiproxy.Config
	}{
		{
			name: "github token source error",
			cfg: func(upstream string) apiproxy.Config {
				return apiproxy.Config{
					Provider: apiproxy.ProviderGitHub,
					Upstream: upstream,
					TokenSource: func(context.Context, string, string) (string, error) {
						return "", errors.New("mint failed")
					},
					IncomingToken: "run-placeholder",
				}
			},
		},
		{
			name: "github token source empty token",
			cfg: func(upstream string) apiproxy.Config {
				return apiproxy.Config{
					Provider: apiproxy.ProviderGitHub,
					Upstream: upstream,
					TokenSource: func(context.Context, string, string) (string, error) {
						return "", nil
					},
					IncomingToken: "run-placeholder",
				}
			},
		},
		{
			name: "jira auth header source error",
			cfg: func(upstream string) apiproxy.Config {
				return apiproxy.Config{
					Provider: apiproxy.ProviderJira,
					Upstream: upstream,
					AuthHeaderSource: func(context.Context) (string, error) {
						return "", errors.New("resolve failed")
					},
					IncomingToken: "run-placeholder",
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &upstreamRecord{}
			upstream := fakeUpstream(rec)
			defer upstream.Close()

			proxyURL := startProxy(t, tt.cfg(upstream.URL))

			req, _ := http.NewRequest(http.MethodGet, proxyURL+"/repos/octo/widgets", nil)
			req.Header.Set("Authorization", "Bearer run-placeholder")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("proxy roundtrip: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", resp.StatusCode)
			}
			if rec.hits.Load() != 0 {
				t.Errorf("upstream hits = %d, want 0 (a request without a resolved credential must never be forwarded)", rec.hits.Load())
			}
		})
	}
}

func TestStartLoopbackGuard(t *testing.T) {
	srv, err := apiproxy.New(apiproxy.Config{
		Provider: apiproxy.ProviderGitHub,
		Upstream: "https://api.github.com",
		TokenSource: func(context.Context, string, string) (string, error) {
			return "tok", nil
		},
	})
	if err != nil {
		t.Fatalf("apiproxy.New: %v", err)
	}

	if _, err := srv.Start("0.0.0.0:0"); err == nil {
		t.Fatal("Start(0.0.0.0:0) without AllowNonLoopback should error")
	}

	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Start on loopback default: %v", err)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("bound addr = %q, want a 127.0.0.1 loopback address", addr)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	if _, err := srv.Start(""); err == nil {
		t.Error("second Start on the same Server should error")
	}
}

// TestGatedShapesTransitTheVerbPipe pins that the gh channel's refusal policy
// is a property of that channel and not of the credential. This proxy is the
// pipe the scoped `exec gh` verbs use, and the shapes the injector refuses —
// a merge, a review submit, a repository create — pass through it untouched.
//
// It matters twice over. The review refusal REDIRECTS to the verbs, so a gate
// that reached this pipe would refuse the alternative it recommends and leave
// an agent with nowhere to go. And the merge shape is here because the two
// pipes must be provably distinct: a shared middleware that grew a gate would
// be caught here rather than discovered when a governed verb stopped working.
func TestGatedShapesTransitTheVerbPipe(t *testing.T) {
	gated := []struct {
		name   string
		method string
		path   string
	}{
		{"a review submit", http.MethodPost, "/repos/octo/widgets/pulls/42/reviews"},
		{"a merge", http.MethodPut, "/repos/octo/widgets/pulls/42/merge"},
		{"a repository create", http.MethodPost, "/orgs/octo/repos"},
	}
	for _, tt := range gated {
		t.Run(tt.name, func(t *testing.T) {
			rec := &upstreamRecord{}
			upstream := fakeUpstream(rec)
			defer upstream.Close()

			proxyURL := startProxy(t, apiproxy.Config{
				Provider:      apiproxy.ProviderGitHub,
				Upstream:      upstream.URL,
				TokenSource:   func(context.Context, string, string) (string, error) { return "tok", nil },
				IncomingToken: "run-placeholder",
			})

			req, _ := http.NewRequest(tt.method, proxyURL+tt.path, strings.NewReader(`{"event":"APPROVE"}`))
			req.Header.Set("Authorization", "Bearer run-placeholder")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("proxy roundtrip: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want the upstream's 200 — this pipe applies no traffic policy", resp.StatusCode)
			}
			if rec.hits.Load() != 1 {
				t.Fatalf("upstream hits = %d, want the request forwarded", rec.hits.Load())
			}
		})
	}
}
