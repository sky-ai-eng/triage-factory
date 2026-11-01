package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestAllProtectedRoutes_RejectUnauthenticated: every route the server mounted
// through s.api / s.apiMutating answers 401 to no cookie, a non-UUID cookie and
// an unknown sid. Routes come from routeMux's record, so a new mount is covered.
func TestAllProtectedRoutes_RejectUnauthenticated(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	protected := protectedRoutes(rig.srv)
	if len(protected) == 0 {
		t.Fatalf("server mounted no protected routes")
	}

	sidName := rig.srv.sidCookieName()
	publicURL := rig.srv.deployCfg.publicURL

	variants := []struct {
		name   string
		cookie *http.Cookie
	}{
		{"no cookie", nil},
		{"garbage value", &http.Cookie{Name: sidName, Value: "not-a-uuid"}},
		{"unknown sid", &http.Cookie{Name: sidName, Value: uuid.NewString()}},
	}

	for _, route := range protected {
		method, pattern := splitMethodAndPath(route.pattern)
		concrete := substitutePathParams(pattern)
		for _, v := range variants {
			t.Run(method+" "+pattern+"/"+v.name, func(t *testing.T) {
				req := httptest.NewRequest(method, concrete, nil)
				if v.cookie != nil {
					req.AddCookie(v.cookie)
				}
				// Same-origin so withCSRFOriginCheck on mutating
				// routes passes — we're testing the auth gate, not
				// CSRF, and an unset Origin would also pass anyway.
				req.Header.Set("Origin", publicURL)

				rec := httptest.NewRecorder()
				rig.srv.mux.ServeHTTP(rec, req)

				if rec.Code != http.StatusUnauthorized {
					t.Errorf("status=%d body=%q, want 401 (variant=%s, via=%s)",
						rec.Code, strings.TrimSpace(rec.Body.String()), v.name, route.via)
				}
			})
		}
	}
}

// TestAllMutatingRoutes_RejectCrossOrigin: every s.apiMutating route answers a
// foreign Origin with 403 even without a cookie, since the CSRF check wraps the
// auth check, and apiMutating on a safe method, which CSRF passes, fails.
func TestAllMutatingRoutes_RejectCrossOrigin(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	var mutating []routeMount
	for _, r := range protectedRoutes(rig.srv) {
		if r.via == mountAPIMutating {
			mutating = append(mutating, r)
		}
	}
	if len(mutating) == 0 {
		t.Fatalf("server mounted no apiMutating routes")
	}

	for _, route := range mutating {
		method, pattern := splitMethodAndPath(route.pattern)
		// withCSRFOriginCheck pre-passes GET/HEAD/OPTIONS, which is
		// correct for those methods — but apiMutating on a safe method
		// is itself the footgun. The handler author believes they have
		// CSRF protection; CSRF silently passes through; nothing fires.
		// Fail hard so a future GET/HEAD/OPTIONS mount on apiMutating
		// gets caught here instead of in production.
		switch method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			t.Run(method+" "+pattern+"/apiMutating-on-safe-method", func(t *testing.T) {
				t.Errorf("apiMutating mounted on safe method %s — CSRF passes through, so the route has no CSRF protection. Either re-mount via s.api (read-only intent) or change the verb to a mutating one.", method)
			})
			continue
		}
		concrete := substitutePathParams(pattern)
		t.Run(method+" "+pattern, func(t *testing.T) {
			req := httptest.NewRequest(method, concrete, nil)
			req.Header.Set("Origin", "https://evil.example")
			rec := httptest.NewRecorder()
			rig.srv.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status=%d body=%q, want 403 (route registered via apiMutating but CSRF didn't fire)",
					rec.Code, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

// protectedRoutes returns every route s mounted through api or apiMutating.
// Raw mounts (the pre-auth allowlist, the SPA, the GoTrue proxy) are left
// out; routes_coverage_test.go covers those.
func protectedRoutes(s *Server) []routeMount {
	var out []routeMount
	for _, m := range s.mux.mounts {
		if m.via == mountAPI || m.via == mountAPIMutating {
			out = append(out, m)
		}
	}
	return out
}

// splitMethodAndPath splits "POST /api/foo/{id}" into ("POST",
// "/api/foo/{id}"). Patterns without a leading method (Go 1.22 mux
// permits this for any-method mounts) default to GET — none of our
// protected routes look like that today, but the fallback is safe.
func splitMethodAndPath(pattern string) (method, path string) {
	if i := strings.Index(pattern, " "); i >= 0 {
		return pattern[:i], pattern[i+1:]
	}
	return http.MethodGet, pattern
}

// substitutePathParams replaces every {name} placeholder with a
// concrete value the mux pattern will match. The substituted values
// only need to satisfy the pattern's path-segment shape (no slashes
// inside a {name}) so the mux's routing-time pattern match succeeds —
// the rejecting middleware short-circuits before the handler runs, so
// the values never have to resolve to real entities or pass any
// handler-level validation.
func substitutePathParams(path string) string {
	zeroUUID := "00000000-0000-0000-0000-000000000000"
	replacements := map[string]string{
		"{id}":             zeroUUID,
		"{conversationID}": zeroUUID,
		"{commentId}":      zeroUUID,
		"{org_id}":         zeroUUID,
		"{number}":         "0",
		"{provider}":       "github",
		"{owner}":          "octocat",
		"{repo}":           "hello-world",
		"{path}":           "readme.md",
		"{event_type}":     "github.pr.opened",
	}
	out := path
	for placeholder, value := range replacements {
		out = strings.ReplaceAll(out, placeholder, value)
	}
	// Catch-all: any remaining {name} segment gets a generic
	// alphanumeric value. Keeps the test working if a new route
	// introduces a placeholder we haven't seen before, rather than
	// silently 404ing because the unsubstituted "{newparam}" doesn't
	// match the registered pattern.
	for strings.Contains(out, "{") {
		openIdx := strings.Index(out, "{")
		closeIdx := strings.Index(out, "}")
		if closeIdx < openIdx {
			break
		}
		out = out[:openIdx] + "placeholder" + out[closeIdx+1:]
	}
	return out
}
