package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNewGotrueProxy_DeniesAdmin is the TFAC-409 item-3 contract: the GoTrue
// reverse proxy refuses /auth/v1/admin/* (GoTrue's service-role user-management
// API) with a 404, while every other /auth/v1/* path forwards upstream. The
// 404 (not 403) keeps the deny indistinguishable from "no such route" so a
// prober can't confirm an admin surface exists.
func TestNewGotrueProxy_DeniesAdmin(t *testing.T) {
	// A stand-in upstream so a forwarded request is observable without a real
	// GoTrue. denyGotrueAdmin runs before the proxy's own transport, so denied
	// paths never reach here.
	var forwarded string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = r.URL.Path
		w.WriteHeader(http.StatusTeapot) // distinctive: proves we hit upstream
	}))
	defer upstream.Close()

	proxy, err := newGotrueProxy(upstream.URL)
	if err != nil {
		t.Fatalf("newGotrueProxy: %v", err)
	}

	cases := []struct {
		name       string
		path       string
		wantDenied bool
	}{
		{"admin exact", "/auth/v1/admin", true},
		{"admin users", "/auth/v1/admin/users", true},
		{"admin nested", "/auth/v1/admin/users/123/factors", true},
		{"token forwards", "/auth/v1/token", false},
		{"authorize forwards", "/auth/v1/authorize", false},
		{"jwks forwards", "/auth/v1/.well-known/jwks.json", false},
		// Not the admin subtree — a path that merely starts with "admin" as a
		// substring must still forward (no such GoTrue route, but we only gate
		// the admin/ subtree, not arbitrary prefixes).
		{"adminish forwards", "/auth/v1/administration", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forwarded = ""
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if tc.wantDenied {
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s: status = %d, want 404", tc.path, rec.Code)
				}
				if forwarded != "" {
					t.Errorf("%s: reached upstream at %q, must be blocked at the proxy", tc.path, forwarded)
				}
			} else {
				if forwarded == "" {
					t.Errorf("%s: did not reach upstream, want forwarded", tc.path)
				}
			}
		})
	}
}
