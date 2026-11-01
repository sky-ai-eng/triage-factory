package server

import (
	"sort"
	"strings"
	"testing"
)

// preAuthAllowlist enumerates every /api/* (and adjacent non-/api/*)
// route that intentionally does NOT route through s.api / s.apiMutating.
// The list is hardcoded — adding/removing a pre-auth route requires
// editing this slice, which forces a deliberate decision rather than
// letting an accidental raw s.mux.HandleFunc slip past.
//
// Keep in sync with the doc-comment block at the top of
// internal/server/server.go::routes().
var preAuthAllowlist = []string{
	"GET /api/auth/oauth/{provider}",     // initiates OAuth dance; no session yet
	"GET /api/auth/callback",             // completes OAuth + creates session
	"POST /api/auth/logout",              // reads sid cookie directly so stale-session logout works
	"GET /api/config",                    // AuthGate reads deployment_mode pre-login
	"GET /api/health",                    // liveness probe for platform healthchecks (Fly, compose, k8s)
	"POST /api/webhooks/github/{org_id}", // GitHub App webhook receiver; pre-auth, verified by HMAC signature in-handler, IP-rate-limited at the signed-webhook tier
	"POST /api/webhooks/github",          // deployment-App webhook receiver; pre-auth, verified against the deployment secret in-handler before any store read, routed by installation binding, same limiter tier
	"GET /api/invites/preview",           // invite-token preview; recipient not yet authenticated, admin-pool, token is the bearer secret
	"GET /api/github/managed/callback",   // deployment-App bind callback; applies withSession itself to the cookie-bearing (writing) branch only — the no-cookie branch is someone who installed from GitHub's public page and has no session
	// POST /api/sso/discover is now an ee/sso route (mounted via the extension
	// seam, pre-auth + IP-rate-limited there), so it's no longer a core routes()
	// mount and isn't checked by this core coverage test.
	"/auth/v1/", // GoTrue reverse proxy; auth happens upstream
	"/api/",     // JSON 404 for unknown /api/* (TFAC-409 item 5); no identity dependency, intentionally before the SPA fallback
	"/",         // SPA fallback; no identity dependency
}

// TestRoutesCoverage asserts every /api/* route the server mounts either
// goes through s.api / s.apiMutating (so withSession is in front of the
// handler) or appears in preAuthAllowlist. It reads the server's own record
// of its mounts (routeMux), so a raw s.mux.Handle("METHOD /api/...") added in
// any file of the package, without wrapping, fails it.
func TestRoutesCoverage(t *testing.T) {
	s := newTestServer(t)
	if len(s.mux.mounts) == 0 {
		t.Fatalf("server recorded no route mounts")
	}

	allow := make(map[string]bool, len(preAuthAllowlist))
	for _, p := range preAuthAllowlist {
		allow[p] = true
	}

	var violations []string
	seenAllowlistRaw := make(map[string]bool)
	for _, m := range s.mux.mounts {
		if m.via != mountRaw {
			// s.api / s.apiMutating mounts are wrapped by construction.
			continue
		}
		if allow[m.pattern] {
			seenAllowlistRaw[m.pattern] = true
			continue
		}
		// Raw mount outside the allowlist. If it's an /api/* route,
		// that's a regression — should be using s.api/s.apiMutating.
		if strings.Contains(m.pattern, "/api/") {
			violations = append(violations,
				`raw mount "`+m.pattern+`" — wrap in s.api or s.apiMutating, or add to preAuthAllowlist with justification`,
			)
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("found %d unwrapped /api/* route(s):\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}

	// Stale-allowlist guard: every entry must correspond to an actual
	// raw mount. Prevents the allowlist from drifting (an entry that no
	// longer matches any code can mask a later regression if a future PR
	// re-introduces the same pattern with new semantics).
	for _, p := range preAuthAllowlist {
		if !seenAllowlistRaw[p] {
			t.Errorf("preAuthAllowlist entry %q has no matching raw mount — remove it from the allowlist", p)
		}
	}
}
