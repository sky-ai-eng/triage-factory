package main

import (
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
		"/api/":                      classRaw,
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

func keysOf(n *node) []string {
	out := make([]string, 0, len(n.Children))
	for k := range n.Children {
		out = append(out, k)
	}
	return out
}
