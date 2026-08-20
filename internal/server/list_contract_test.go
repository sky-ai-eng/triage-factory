package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The read surface has two invariants that no individual route test can catch,
// because both are about the SET of routes rather than any one of them. They
// are checked here by parsing the package, in the same style as
// TestRoutesCoverage — a new route that breaks either fails the build rather
// than shipping and being found later by whoever paginates the next surface.
//
//  1. Every `POST /…/list` route actually speaks the list contract: it resolves
//     a page and writes one of the list envelopes. A route can otherwise be
//     named `/list`, decode a body, and answer a bare array — which is worse
//     than not converting it, because the name now promises paging that isn't
//     there.
//  2. No handler code constructs a bare `db.ListOpts{}`. An unwindowed store
//     read is legitimate when the answer is a decision rather than a page, and
//     `db.Unwindowed` is how a call site says that. The bare literal reads
//     identically at both kinds of call site, which is how an unbounded
//     fetch-the-world lands in a response body unnoticed.

// listRouteExceptions names `POST /…/list` routes that deliberately do not use
// httpx.WriteList. Each still resolves a page and still answers the paging
// keys — the exception is only about which writer produces the envelope.
var listRouteExceptions = map[string]string{
	// Groups its rows into a map keyed by task id (a map is not a slice), so it
	// writes its own envelope with httpx.NextPageToken. See
	// conversationListResponse.
	"POST /api/agent/conversations/list": "grouped response shape",
	// Carries a readiness discriminator beside the list, because a first-ever
	// open has nothing mirrored yet and an empty array there reads as "you have
	// no repositories" rather than "we have not looked yet". Still resolves a
	// page, still answers every paging key (with a real total_count on both
	// credential classes), and mints its next token with httpx.NextPageToken.
	// See githubRepoListResponse.
	"POST /api/github/repos/list": "readiness discriminator beside the list",
	// Carries the team's bot beside the list, because the bot is a workload
	// identity and not a member: paging a list whose last element is not a
	// member makes total_count a lie. Still resolves a page, still answers
	// every paging key, and mints its next token with httpx.NextPageToken.
	// See teamRosterListResponse.
	"POST /api/teams/{team_id}/members/list": "team bot beside the list",
}

func TestListRoutesResolveAPage(t *testing.T) {
	files := parsePackageFiles(t, ".")
	bodies := handlerBodies(files)
	for pattern, handler := range listRouteHandlers(t, files) {
		body, ok := bodies[handler]
		if !ok {
			// The handler lives outside this package (an ee surface mounted
			// through the extension seam); its own package tests it.
			continue
		}
		if !strings.Contains(body, "httpx.ResolvePage") {
			t.Errorf("%s (%s) does not call httpx.ResolvePage — a /list route that "+
				"doesn't resolve a page is a bare array wearing the list contract's name",
				pattern, handler)
		}
		if _, excepted := listRouteExceptions[pattern]; excepted {
			if !strings.Contains(body, "httpx.NextPageToken") {
				t.Errorf("%s (%s) is a declared WriteList exception but never mints a "+
					"next token — an exception owes the same paging keys", pattern, handler)
			}
			continue
		}
		if !strings.Contains(body, "httpx.WriteList") && !strings.Contains(body, "httpx.WriteProxyList") {
			t.Errorf("%s (%s) resolves a page but writes no list envelope; if its shape "+
				"genuinely cannot be the flat envelope, add it to listRouteExceptions with a reason",
				pattern, handler)
		}
	}
}

// TestNoBareListOptsLiteral is the second invariant. It scans source text
// rather than the AST because the thing being forbidden is a spelling: both
// forms produce the identical value, and the point is that one of them states
// the intent and the other hides it.
//
// It walks the whole module rather than this package. Scoping it to the
// handlers was the obvious reading — a bare literal only becomes an unbounded
// response body here — but it is the wrong one twice over: a store's own
// `...System` variant and an importer reaching for every row are the same
// judgment call, and a rule that stops at a directory boundary is a rule
// somebody lands on the wrong side of. One did, in the commit that added this.
func TestNoBareListOptsLiteral(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	walked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Everything outside the Go tree, plus the build outputs that would
			// make this walk slow and noisy.
			switch d.Name() {
			case ".git", "node_modules", "frontend", "target", "dist", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		walked++
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "db.ListOpts{}") {
				t.Errorf("%s:%d constructs a bare db.ListOpts{}. If the read genuinely "+
					"wants every row, say db.Unwindowed; if it feeds a response, pass the "+
					"window httpx.ResolvePage returned.", rel, i+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if walked == 0 {
		t.Fatal("walked no Go files — the scan is broken, not the tree")
	}
}

// parsePackageFiles parses every non-test .go file in dir. An explicit walk
// rather than parser.ParseDir, which is deprecated for not honoring build tags
// — this package has none, but the deprecation is the linter's business and a
// ReadDir loop is the same six lines.
func parsePackageFiles(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		out[path] = f
	}
	return out
}

// listRouteHandlers returns pattern → handler-function name for every
// `POST /…/list` registered in routes().
func listRouteHandlers(t *testing.T, files map[string]*ast.File) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "api" && sel.Sel.Name != "apiMutating") {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pattern, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.HasSuffix(pattern, "/list") {
				return true
			}
			if name := handlerName(call.Args[1]); name != "" {
				out[pattern] = name
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatal("found no POST /…/list routes — the scan is broken, not the surface")
	}
	return out
}

// handlerName renders the handler argument of an api()/apiMutating() call as
// the bare function name, for both `x.handleFoo` and `handleFoo` forms.
func handlerName(arg ast.Expr) string {
	switch h := arg.(type) {
	case *ast.SelectorExpr:
		return h.Sel.Name
	case *ast.Ident:
		return h.Name
	}
	return ""
}

// handlerBodies renders every function and method in the package to source
// text, keyed by bare name. Text rather than AST because the assertions are
// "does this handler call X", and a substring is both sufficient and immune to
// how the call is spelled.
func handlerBodies(files map[string]*ast.File) map[string]string {
	out := map[string]string{}
	for name, f := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			start, end := int(fn.Body.Pos()), int(fn.Body.End())
			// Positions are file-set offsets; both are within this file since
			// the decl came from it. Convert by subtracting the file base.
			base := int(f.FileStart)
			if start-base < 0 || end-base > len(src) {
				continue
			}
			out[fn.Name.Name] = string(src[start-base : end-base])
		}
	}
	return out
}
