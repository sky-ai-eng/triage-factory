package ghinjector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The repository gate as an agent meets it: the pinned gh against the
// conformance fixture, behind a front that answers the gate's lookups of the
// repositories outside the run. The run works in octo/repo; acme/payments is
// private and acme/lib public, and octo/old-name is octo/repo's former name.
func TestGHWire_RepositoryGate(t *testing.T) {
	fixture := newFakeUpstream(t)
	target, err := url.Parse(fixture.server.URL)
	if err != nil {
		t.Fatalf("parse fixture url: %v", err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	lookups := map[string]string{
		"/repos/acme/payments": `{"id": 2, "full_name": "acme/payments", "private": true}`,
		"/repos/acme/lib":      `{"id": 3, "full_name": "acme/lib", "private": false}`,
		"/repos/octo/old-name": `{"id": 4, "full_name": "octo/repo", "private": true}`,
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := lookups[strings.TrimPrefix(r.URL.Path, restPrefix)]; ok && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)

	var (
		mu      sync.Mutex
		set     = []AuthorizedRepository{{Name: "octo/repo", ID: "4"}}
		denials []string
	)
	env := ghEnvWithConfig(t, front.URL, Config{
		Repositories: func(context.Context) ([]AuthorizedRepository, bool, error) {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(set), true, nil
		},
		DenyRepository: func(_ context.Context, d DeniedRepository) {
			mu.Lock()
			defer mu.Unlock()
			denials = append(denials, d.Name())
		},
	})
	denied := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := denials
		denials = nil
		return out
	}

	refusedOutside := [][]string{
		{"pr", "view", "42", "-R", "acme/payments"},
		{"api", "repos/acme/payments/pulls/42"},
		{"api", "-X", "POST", "repos/acme/lib/issues", "-f", "title=x"},
	}
	for _, args := range refusedOutside {
		out := runGHRefused(t, env, args...)
		name := "acme/payments"
		if strings.Contains(strings.Join(args, " "), "acme/lib") {
			name = "acme/lib"
		}
		if !strings.Contains(out, "workspace add "+name) {
			t.Errorf("gh %s printed %q, want the refusal naming %s and the way to add it", strings.Join(args, " "), out, name)
		}
		if got := denied(); !slices.Equal(got, []string{name}) {
			t.Errorf("gh %s left denials %v, want one for %s", strings.Join(args, " "), got, name)
		}
	}

	for _, args := range [][]string{
		{"pr", "view", "42", "-R", "octo/repo"},
		{"api", "repos/octo/repo/pulls/42"},
		// Under its former name, GitHub answers with the run's repository.
		{"api", "repos/octo/old-name/pulls/42"},
		// A public repository outside the run reads.
		{"api", "repos/acme/lib/issues"},
		{"repo", "view", "acme/lib"},
	} {
		runGH(t, env, args...)
	}
	if got := denied(); len(got) != 0 {
		t.Errorf("denials = %v, want none for the run's own repository and a public read", got)
	}

	// `workspace add` widens the set, and the next request sees it.
	mu.Lock()
	set = append(set, AuthorizedRepository{Name: "acme/payments", ID: "2"})
	mu.Unlock()
	runGH(t, env, "pr", "view", "42", "-R", "acme/payments")
	runGH(t, env, "api", "repos/acme/payments/pulls/42")
	if got := denied(); len(got) != 0 {
		t.Errorf("denials = %v after the repository joined the set, want none", got)
	}
}
