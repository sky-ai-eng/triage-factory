package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestAllProtectedRoutes_RejectUnauthenticated probes every /api/* route
// registered via s.api / s.apiMutating with three flavors of unauth and
// asserts each comes back as 401. Routes are discovered by AST-parsing
// server.go so coverage can't drift behind new mounts — if someone adds
// a route via s.api/s.apiMutating, the test picks it up on the next run.
//
// The variants:
//
//	no cookie       — most basic gate; requireAuth not wired at all
//	garbage value   — cookie present but value isn't a UUID
//	unknown sid     — well-formed UUID with no matching session row
//
// All three short-circuit at auth middleware and never reach the route
// handler. The no-cookie and garbage-value variants reject before any DB
// access; the unknown-sid variant still performs a session lookup against
// the sessions table to confirm the row doesn't exist, but it still
// returns 401 without needing session-row fixtures. The mux extracts
// path values during routing (before middleware runs), but the handler
// never gets the chance to consume them, so the substituted values
// only need to match the pattern's segment shape.
//
// What this test does NOT cover:
//
//   - Expired-but-extant sid: separate test; needs a real session row.
//     Existing TestAuthFlow_ForceExpiryReturns401 covers the mechanism.
//   - Cross-org access (sid for user A, query org B's resource): RLS
//     concern, covered by cross_org_http_test.go.
//   - CSRF rejection paths: covered by TestAuthFlow_Logout_CSRFOriginCheck.
//
// The static "is this route wrapped at all" guard lives in
// routes_coverage_test.go; this is the runtime complement that proves
// wrapping actually rejects bad input.
func TestAllProtectedRoutes_RejectUnauthenticated(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	protected := discoverProtectedRoutes(t)
	if len(protected) == 0 {
		t.Fatalf("AST discovered 0 protected routes — parser broken")
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

// TestAllMutatingRoutes_RejectCrossOrigin asserts that every route
// registered via s.apiMutating returns 403 when the Origin header
// points at a foreign site, regardless of cookie state. This proves
// withCSRFOriginCheck is wired in front of every mutating handler —
// the runtime complement to routes_coverage_test.go's static guarantee
// that mutating routes go through apiMutating.
//
// Without this test, a future change that strips the CSRF wrap from
// any individual route would slip through: the unauth test would
// still pass (auth still 401s on no cookie), and routes_coverage_test
// would still pass (the route is still registered via apiMutating).
// The misconfig is only detectable by actually firing a cross-origin
// request and watching for the rejection.
//
// We use Origin: https://evil.example and no cookie. CSRF fires
// outside the auth wrap, so the no-cookie case still produces a 403
// (not a 401) — that's the assertion. Path-param substitutions reuse
// the unauth-test helper since CSRF, like auth, rejects before the
// handler runs — the mux extracts path values during routing, but
// no handler-level parsing or DB query happens past the rejection.
func TestAllMutatingRoutes_RejectCrossOrigin(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	all := discoverProtectedRoutes(t)
	var mutating []protectedRoute
	for _, r := range all {
		if r.via == "apiMutating" {
			mutating = append(mutating, r)
		}
	}
	if len(mutating) == 0 {
		t.Fatalf("AST discovered 0 apiMutating routes — parser broken")
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

// protectedRoute is one route discovered by AST inspection of
// (s *Server).routes().
type protectedRoute struct {
	pattern string // e.g. "POST /api/tasks/{id}/swipe"
	via     string // "api" or "apiMutating"
}

// discoverProtectedRoutes parses server.go, walks (s *Server).routes(),
// and returns every mount registered via s.api / s.apiMutating. Raw
// s.mux.Handle* calls (pre-auth allowlist + the SPA / GoTrue proxy) are
// excluded — those are already covered by routes_coverage_test.go.
//
// Reuses the same parsing approach so the two tests can't disagree
// about what counts as "a route mount."
func discoverProtectedRoutes(t *testing.T) []protectedRoute {
	t.Helper()
	const sourceFile = "server.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, sourceFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", sourceFile, err)
	}

	var routesFn *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != "routes" {
			continue
		}
		routesFn = fn
		break
	}
	if routesFn == nil {
		t.Fatalf("could not find (s *Server).routes() in %s", sourceFile)
	}

	var out []protectedRoute
	ast.Inspect(routesFn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "s" {
			return true
		}
		switch sel.Sel.Name {
		case "api", "apiMutating":
		default:
			return true
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		out = append(out, protectedRoute{pattern: pattern, via: sel.Sel.Name})
		return true
	})
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
		"{id}":         zeroUUID,
		"{runID}":      zeroUUID,
		"{commentId}":  zeroUUID,
		"{org_id}":     zeroUUID,
		"{number}":     "0",
		"{provider}":   "github",
		"{owner}":      "octocat",
		"{repo}":       "hello-world",
		"{path}":       "readme.md",
		"{event_type}": "github.pr.opened",
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
