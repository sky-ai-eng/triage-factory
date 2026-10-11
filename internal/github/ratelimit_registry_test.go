package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// TestResolver_RateLimitFor_RecordsPerOrgFromLiveCalls pins the
// end-to-end path GET /readyz's soft rate-limit signal depends on
// (TFAC-573): the resolver observes rate-limit headers from every
// *Client it mints — here, the PAT tier, via ClientFor — and makes the
// latest values readable per org through RateLimitReader without a
// fresh API call.
func TestResolver_RateLimitFor_RecordsPerOrgFromLiveCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Reset", "1783303600")
		w.Header().Set("X-RateLimit-Used", "679")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)

	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: nil}, // no registered App — PAT tier
		&fakeOrgs{base: srv.URL},
		&fakeAgents{},
		nil,
	)

	host := domain.GitHubHost(srv.URL)
	reader, ok := r.(RateLimitReader)
	if !ok {
		t.Fatal("production resolver must implement RateLimitReader")
	}
	if _, ok := reader.RateLimitFor("org-1", host); ok {
		t.Fatal("expected no observation before any call")
	}

	client, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Get(context.Background(), "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}

	st, ok := reader.RateLimitFor("org-1", host)
	if !ok {
		t.Fatal("expected an observation after one call")
	}
	if st.Remaining != 4321 {
		t.Errorf("remaining = %d, want 4321", st.Remaining)
	}
	if st.Used != 679 {
		t.Errorf("used = %d, want 679", st.Used)
	}
	if want := time.Unix(1783303600, 0); !st.Reset.Equal(want) {
		t.Errorf("reset = %v, want %v", st.Reset, want)
	}

	// A distinct org must not see org-1's observation — the registry is
	// per-org, not process-global.
	if _, ok := reader.RateLimitFor("org-2", host); ok {
		t.Error("org-2 should have no rate-limit observation")
	}
}

// TestResolver_RateLimitFor_KeyedByHost pins that a budget observed on one
// GitHub host is never reported for another: each deployment meters its own,
// so an org repointed from host A to host B reads B's budget (none yet), not
// A's last answer, and each host's observation stays under its own key.
func TestResolver_RateLimitFor_KeyedByHost(t *testing.T) {
	serve := func(remaining string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", remaining)
			w.Header().Set("X-RateLimit-Reset", "1783303600")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("[]"))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	srvA := serve("4000")
	srvB := serve("17")
	onA, onB := domain.GitHubHost(srvA.URL), domain.GitHubHost(srvB.URL)

	orgs := &fakeOrgs{base: srvA.URL}
	r := newTestResolver(
		// No github_url secret: the PAT is used on whatever host the org
		// resolves to, so the same resolver can observe both hosts.
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: nil},
		orgs,
		&fakeAgents{},
		nil,
	)
	reader := r.(RateLimitReader)

	probe := func() {
		t.Helper()
		client, err := r.ClientFor(context.Background(), "org-1", "acme")
		if err != nil {
			t.Fatalf("ClientFor: %v", err)
		}
		if _, err := client.Get(context.Background(), "/probe"); err != nil {
			t.Fatalf("probe: %v", err)
		}
	}

	probe()
	if st, ok := reader.RateLimitFor("org-1", onA); !ok || st.Remaining != 4000 {
		t.Fatalf("host A = (%+v, %v), want remaining 4000", st, ok)
	}
	if st, ok := reader.RateLimitFor("org-1", onB); ok {
		t.Fatalf("host B reported %+v before any call reached it; host A's budget must not answer for it", st)
	}

	orgs.base = srvB.URL
	probe()
	if st, ok := reader.RateLimitFor("org-1", onB); !ok || st.Remaining != 17 {
		t.Fatalf("host B = (%+v, %v), want remaining 17", st, ok)
	}
	if st, ok := reader.RateLimitFor("org-1", onA); !ok || st.Remaining != 4000 {
		t.Fatalf("host A = (%+v, %v) after a call on host B, want its own remaining 4000", st, ok)
	}
}
