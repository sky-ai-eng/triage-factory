package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

	r := NewResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_test"}},
		&fakeApps{app: nil}, // no registered App — PAT tier
		&fakeOrgs{base: srv.URL},
		&fakeAgents{},
		nil,
	)

	reader, ok := r.(RateLimitReader)
	if !ok {
		t.Fatal("production resolver must implement RateLimitReader")
	}
	if _, ok := reader.RateLimitFor("org-1"); ok {
		t.Fatal("expected no observation before any call")
	}

	client, err := r.ClientFor(context.Background(), "org-1", "acme")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Get(context.Background(), "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}

	st, ok := reader.RateLimitFor("org-1")
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
	if _, ok := reader.RateLimitFor("org-2"); ok {
		t.Error("org-2 should have no rate-limit observation")
	}
}
