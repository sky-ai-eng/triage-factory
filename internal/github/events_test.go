package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCheckRepoAccess_Classifies pins the per-repo reachability probe behind
// the SKY-409 write-time gate: 200 → reachable, 404/403 → conclusively
// unreachable, 5xx → indeterminate (fail open). The path is exactly
// /repos/{owner}/{repo}.
func TestCheckRepoAccess_Classifies(t *testing.T) {
	cases := []struct {
		name           string
		status         int
		wantReachable  bool
		wantConclusive bool
	}{
		{"200 reachable", http.StatusOK, true, true},
		{"404 conclusive unreachable", http.StatusNotFound, false, true},
		{"403 conclusive unreachable", http.StatusForbidden, false, true},
		{"500 indeterminate", http.StatusInternalServerError, false, false},
		{"502 indeterminate", http.StatusBadGateway, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/owner/repo" {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			reachable, conclusive := testClient(srv.URL).CheckRepoAccess(context.Background(), "owner", "repo")
			if reachable != tc.wantReachable || conclusive != tc.wantConclusive {
				t.Errorf("got (reachable=%v, conclusive=%v); want (%v, %v)",
					reachable, conclusive, tc.wantReachable, tc.wantConclusive)
			}
		})
	}
}

// TestCheckRepoAccess_TransportErrorFailsOpen confirms a dead endpoint
// (connection refused) is treated as indeterminate, not as a reject.
func TestCheckRepoAccess_TransportErrorFailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now → transport error

	reachable, conclusive := testClient(url).CheckRepoAccess(context.Background(), "owner", "repo")
	if reachable || conclusive {
		t.Errorf("transport error must be indeterminate; got (reachable=%v, conclusive=%v)", reachable, conclusive)
	}
}
