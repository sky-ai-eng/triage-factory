package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
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
	"GET /api/invites/preview",           // invite-token preview; recipient not yet authenticated, admin-pool, token is the bearer secret
	"GET /api/github/managed/callback",   // deployment-App bind callback; applies withSession itself to the cookie-bearing (writing) branch only — the no-cookie branch is someone who installed from GitHub's public page and has no session
	// POST /api/sso/discover is now an ee/sso route (mounted via the extension
	// seam, pre-auth + IP-rate-limited there), so it's no longer a core routes()
	// mount and isn't checked by this core coverage test.
	"/auth/v1/", // GoTrue reverse proxy; auth happens upstream
	"/api/",     // JSON 404 for unknown /api/* (TFAC-409 item 5); no identity dependency, intentionally before the SPA fallback
	"/",         // SPA fallback; no identity dependency
}

// TestRoutesCoverage parses server.go and asserts every /api/* mount
// either goes through s.api / s.apiMutating (so withSession is in front
// of the handler) or appears in preAuthAllowlist. Catches regressions
// where a future contributor adds a raw s.mux.HandleFunc("METHOD
// /api/...") without wrapping it.
func TestRoutesCoverage(t *testing.T) {
	const sourceFile = "server.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, sourceFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", sourceFile, err)
	}

	// Find the routes() method body. Other functions in server.go
	// (handleFrontend etc.) don't register routes, but scoping to
	// routes() means a stray s.mux.HandleFunc elsewhere wouldn't
	// confuse the audit.
	var routesFn *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name.Name != "routes" || fn.Recv == nil {
			continue
		}
		routesFn = fn
		break
	}
	if routesFn == nil {
		t.Fatalf("could not find (s *Server).routes() in %s", sourceFile)
	}

	type mount struct {
		pattern string // e.g. "POST /api/tasks/{id}/swipe"
		via     string // helper used: "api", "apiMutating", "mux.HandleFunc", "mux.Handle"
		pos     token.Position
	}
	var mounts []mount

	allow := make(map[string]bool, len(preAuthAllowlist))
	for _, p := range preAuthAllowlist {
		allow[p] = true
	}

	ast.Inspect(routesFn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if len(call.Args) == 0 {
			return true
		}
		// First arg must be a string literal route pattern.
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}

		// Identify the call shape.
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		methodName := sel.Sel.Name

		var via string
		switch methodName {
		case "api", "apiMutating":
			// s.api(...) / s.apiMutating(...) — direct method on Server.
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "s" {
				return true
			}
			via = methodName
		case "HandleFunc", "Handle":
			// s.mux.HandleFunc / s.mux.Handle
			inner, ok := sel.X.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if inner.Sel.Name != "mux" {
				return true
			}
			if ident, ok := inner.X.(*ast.Ident); !ok || ident.Name != "s" {
				return true
			}
			via = "mux." + methodName
		default:
			return true
		}

		mounts = append(mounts, mount{
			pattern: pattern,
			via:     via,
			pos:     fset.Position(call.Pos()),
		})
		return true
	})

	if len(mounts) == 0 {
		t.Fatalf("no route mounts found in routes() — parser likely broken")
	}

	// Deduplicate by (pattern, via) so PUT+PATCH aliases counted once
	// per call site. The test cares about each *call site*, not each
	// HTTP-verb combo, so we don't dedup by pattern alone.

	var violations []string
	seenAllowlistRaw := make(map[string]bool)

	for _, m := range mounts {
		if m.via != "mux.HandleFunc" && m.via != "mux.Handle" {
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
				m.pos.String()+": raw "+m.via+`("`+m.pattern+`", ...) — wrap in s.api or s.apiMutating, or add to preAuthAllowlist with justification`,
			)
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("found %d unwrapped /api/* route(s):\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}

	// Stale-allowlist guard: every entry must correspond to an actual
	// raw mount in routes(). Prevents the allowlist from drifting (an
	// entry that no longer matches any code can mask a later regression
	// if a future PR re-introduces the same pattern with new semantics).
	for _, p := range preAuthAllowlist {
		if !seenAllowlistRaw[p] {
			t.Errorf("preAuthAllowlist entry %q has no matching raw mount in routes() — remove it from the allowlist", p)
		}
	}
}
