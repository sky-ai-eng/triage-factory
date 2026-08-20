package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture exercises every registration idiom plus the three shapes that
// must NOT be picked up: a path in a comment, a path in an unrelated call, and
// a registration in a test file (which the walker skips, not parseFile).
const fixture = `package server

// Not a route: GET /api/from-a-comment
func routes() {
	s.api("GET /api/things/{id}", nil)
	s.apiMutating("POST /api/things/list", nil)
	s.mux.HandleFunc("GET /health", nil)
	api.API("GET /api/fleet/overview", nil)
	// An ee installer's pre-auth mount: no session wrapper, authenticated by
	// a signature or not at all. The class of route the "!" mark is for.
	api.Raw("POST /api/webhooks/thing/{org_id}", api.SignedWebhookRateLimit(nil))
	// A registration wrapped across lines is still a registration.
	s.api(
		"GET /api/things/by-name/{name}",
		nil,
	)
	// A prefix mount, and the catch-all under it.
	s.mux.HandleFunc("/api/", nil)
	// Same name, different receiver — and a string first arg that is not a
	// mux pattern. Neither is a route.
	log.Printf("GET /api/not-a-route")
	s.api(somePattern, nil)
}
`

func writeGo(t *testing.T, dir, name, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseFileFindsEveryIdiomAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "routes.go", fixture)

	got, err := parseFile(filepath.Join(dir, "routes.go"), "internal/server")
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}

	want := map[string]authClass{
		"/api/things/{id}":           classSession,
		"/api/things/list":           classMutating,
		"/health":                    classRaw,
		"/api/fleet/overview":        classSession,
		"/api/things/by-name/{name}": classSession,
		// Mounted through ExtensionAPI.Raw — no session wrapper, which is the
		// whole reason it has to be found.
		"/api/webhooks/thing/{org_id}": classRaw,
		"/api/":                        classRaw,
	}
	if len(got) != len(want) {
		t.Fatalf("found %d routes, want %d: %+v", len(got), len(want), got)
	}
	for _, r := range got {
		class, ok := want[r.Path]
		if !ok {
			t.Errorf("unexpected route %q — a comment or a non-mux call was read as a registration", r.Path)
			continue
		}
		if r.Class != class {
			t.Errorf("%s: class %v, want %v", r.Path, r.Class, class)
		}
	}
}

func TestCollectSkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "routes.go", "package p\nfunc f() { s.api(\"GET /api/real\", nil) }\n")
	writeGo(t, dir, "routes_test.go", "package p\nfunc g() { s.api(\"GET /api/only-in-a-test\", nil) }\n")

	got, err := collect(dir)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	// A route registered in a test is not part of the surface.
	if len(got) != 1 || got[0].Path != "/api/real" {
		t.Fatalf("got %+v, want just /api/real", got)
	}
}

func TestSplitPattern(t *testing.T) {
	for _, c := range []struct{ in, method, path string }{
		{"GET /api/x", "GET", "/api/x"},
		{"DELETE /api/x/{id}", "DELETE", "/api/x/{id}"},
		// No method: a prefix mount answers every method.
		{"/api/", "", "/api/"},
		{"/", "", "/"},
	} {
		m, p := splitPattern(c.in)
		if m != c.method || p != c.path {
			t.Errorf("splitPattern(%q) = (%q, %q), want (%q, %q)", c.in, m, p, c.method, c.path)
		}
	}
}

func TestPrefixMountDoesNotCollapseOntoItsParent(t *testing.T) {
	root := build([]route{
		{Method: "GET", Path: "/api/things/{id}"},
		{Method: "", Path: "/api/"},
	})
	api := root.Children["api"]
	if api == nil {
		t.Fatal("no /api node")
	}
	// "GET /api" is not a route; printing the prefix mount there would say it was.
	if len(api.Methods) != 0 {
		t.Errorf("/api carries %+v — the prefix mount collapsed onto it", api.Methods)
	}
	if _, ok := api.Children["(everything else)"]; !ok {
		t.Errorf("prefix mount lost; children are %v", keysOf(api))
	}
}

func TestDescribeReadsBeforeWritesAndNamesOnlyForeignPackages(t *testing.T) {
	core := describe([]route{
		{Method: "DELETE", Class: classMutating, Pkg: "internal/server"},
		{Method: "GET", Class: classSession, Pkg: "internal/server"},
	})
	if core != "GET DELETE*" {
		t.Errorf("describe = %q, want %q", core, "GET DELETE*")
	}
	ee := describe([]route{{Method: "GET", Class: classSession, Pkg: "ee/fleet"}})
	if !strings.Contains(ee, "~ ee/fleet") {
		t.Errorf("describe = %q, want it to name the installing package", ee)
	}
}

// TestRegistrarsCoverExtensionAPI is the drift guard. `registrars` is a hand-
// written list, and the first thing it got wrong was omitting ExtensionAPI.Raw
// — so two ee routes (SSO discovery, the Slack webhook receiver) were missing
// from the surface with nothing to say so. Read the interface itself and
// require every mounting method to be listed: a method whose first parameter
// is the mux pattern and whose second is an http handler mounts routes, and if
// this tool cannot see it the output is a quiet lie rather than an error.
func TestRegistrarsCoverExtensionAPI(t *testing.T) {
	const decl = "../../internal/server/extension.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, decl, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", decl, err)
	}

	var iface *ast.InterfaceType
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "ExtensionAPI" {
			return true
		}
		iface, _ = ts.Type.(*ast.InterfaceType)
		return false
	})
	if iface == nil {
		t.Fatalf("no ExtensionAPI interface in %s — did it move?", decl)
	}

	found := 0
	for _, m := range iface.Methods.List {
		fn, ok := m.Type.(*ast.FuncType)
		if !ok || len(m.Names) != 1 || fn.Params == nil || len(fn.Params.List) != 2 {
			continue
		}
		if !isIdent(fn.Params.List[0].Type, "string") {
			continue
		}
		if !isHTTPHandler(fn.Params.List[1].Type) {
			continue
		}
		found++
		name := "api." + m.Names[0].Name
		if _, ok := registrars[name]; !ok {
			t.Errorf("ExtensionAPI.%s mounts routes but %q is not in registrars — every route it registers is silently absent from the output",
				m.Names[0].Name, name)
		}
	}
	// If the shape check ever stops matching, the loop above passes vacuously.
	if found < 3 {
		t.Fatalf("matched %d mounting methods, expected at least 3 (API, APIMutating, Raw) — the signature check has drifted", found)
	}
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// isHTTPHandler matches http.Handler and http.HandlerFunc, the two second
// parameters a mux mount takes.
func isHTTPHandler(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || !isIdent(sel.X, "http") {
		return false
	}
	return sel.Sel.Name == "Handler" || sel.Sel.Name == "HandlerFunc"
}

func keysOf(n *node) []string {
	out := make([]string, 0, len(n.Children))
	for k := range n.Children {
		out = append(out, k)
	}
	return out
}
