// Command apitree prints the HTTP surface as a path tree.
//
// It exists because the route table is registered in one 800-line function and
// read in URL order by everyone who consumes it — the SPA, an agent holding a
// token, anyone auditing what is reachable. Source order answers "where is this
// handler"; this answers "what does the surface look like", which is the
// question the API-design rules in CLAUDE.md are actually about. A resource
// missing its `GET /{id}` or its `POST /list` is visible here and invisible in
// server.go.
//
// Routes are read from the AST rather than by grepping, so a registration
// wrapped across lines is still found and a route named inside a comment or a
// string is not. Test files are skipped: a route registered in a test is not
// part of the surface.
//
// The four registration idioms, and what each one means for the caller:
//
//	s.api / api.API                   session required, read-shaped
//	s.apiMutating / api.APIMutating   session required + CSRF same-origin check
//	s.mux.Handle / s.mux.HandleFunc   raw mount — no session wrapper of its own
//
// The third is the interesting one: every route on it either authenticates by
// another means (a webhook HMAC, an OAuth state parameter) or is deliberately
// open (health, config, the SPA fallback). It is marked in the output for that
// reason rather than for completeness.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// authClass is how a route was mounted — which is to say what a caller has to
// present before the handler runs.
type authClass int

const (
	classSession authClass = iota
	classMutating
	classRaw
)

func (c authClass) mark() string {
	switch c {
	case classMutating:
		return "*"
	case classRaw:
		return "!"
	}
	return " "
}

// route is one registered pattern. Method is empty for a prefix mount
// ("/api/", "/") that answers every method.
type route struct {
	Method string
	Path   string
	Class  authClass
	// Pkg is the directory the registration lives in, relative to the repo
	// root — "internal/server", "ee/fleet". The ee surfaces install
	// themselves, so this is how a reader tells core from an EE feature.
	Pkg string
}

// registrars maps a call's selector text to what mounting through it means.
// Anything not named here is not a route registration.
var registrars = map[string]authClass{
	"s.api":              classSession,
	"api.API":            classSession,
	"s.apiMutating":      classMutating,
	"api.APIMutating":    classMutating,
	"s.mux.Handle":       classRaw,
	"s.mux.HandleFunc":   classRaw,
	"mux.Handle":         classRaw,
	"mux.HandleFunc":     classRaw,
	"s.public":           classRaw,
	"api.Public":         classRaw,
	"api.PublicMutating": classRaw,
}

func main() {
	root := flag.String("root", ".", "repository root to scan")
	flat := flag.String("flat", "", "instead of a tree, print a sorted flat list (`all` or a path prefix)")
	flag.Parse()

	routes, err := collect(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apitree:", err)
		os.Exit(1)
	}
	if len(routes) == 0 {
		fmt.Fprintln(os.Stderr, "apitree: no routes found — is -root the repository root?")
		os.Exit(1)
	}

	if *flat != "" {
		printFlat(routes, *flat)
		return
	}
	printTree(routes)
}

// collect walks the repo for route registrations. Directories that cannot
// contain Go source we care about are skipped wholesale — node_modules is large
// enough that walking it is the difference between instant and not.
func collect(root string) ([]route, error) {
	var out []route
	skip := map[string]bool{
		".git": true, "node_modules": true, "dist": true, "vendor": true,
		"target": true, "testdata": true,
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		found, ferr := parseFile(path, filepath.ToSlash(filepath.Dir(rel)))
		if ferr != nil {
			// A file that will not parse is a compile error someone else is
			// already looking at; it must not stop the rest of the surface
			// from being printed.
			fmt.Fprintf(os.Stderr, "apitree: skipping %s: %v\n", rel, ferr)
			return nil
		}
		out = append(out, found...)
		return nil
	})
	return out, err
}

func parseFile(path, pkgDir string) ([]route, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var out []route
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		class, ok := registrars[selectorText(call.Fun)]
		if !ok {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		method, p := splitPattern(pattern)
		if !strings.HasPrefix(p, "/") {
			// Not a mux pattern — some other call that happens to share a
			// name and take a string first.
			return true
		}
		out = append(out, route{Method: method, Path: p, Class: class, Pkg: pkgDir})
		return true
	})
	return out, nil
}

// selectorText renders a call's function expression as the dotted source text
// ("s.mux.HandleFunc"), so the registrar table can be written the way the calls
// are written.
func selectorText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		base := selectorText(v.X)
		if base == "" {
			return ""
		}
		return base + "." + v.Sel.Name
	}
	return ""
}

// splitPattern separates Go 1.22's "METHOD /path" mux pattern. A pattern with
// no method matches every method, and is reported as such.
func splitPattern(pattern string) (method, path string) {
	if i := strings.IndexByte(pattern, ' '); i > 0 {
		return pattern[:i], strings.TrimSpace(pattern[i+1:])
	}
	return "", pattern
}

// node is one path segment. Methods is what is registered at exactly this
// path — empty for a segment that only exists to hold children.
type node struct {
	Seg      string
	Methods  []route
	Children map[string]*node
}

func newNode(seg string) *node {
	return &node{Seg: seg, Children: map[string]*node{}}
}

func (n *node) child(seg string) *node {
	c, ok := n.Children[seg]
	if !ok {
		c = newNode(seg)
		n.Children[seg] = c
	}
	return c
}

func build(routes []route) *node {
	root := newNode("")
	for _, r := range routes {
		cur := root
		for _, seg := range strings.Split(strings.Trim(r.Path, "/"), "/") {
			if seg == "" {
				continue
			}
			cur = cur.child(seg)
		}
		// A trailing slash is a PREFIX mount: "/api/" catches everything under
		// /api that no other pattern claimed, and "/" is the SPA fallback.
		// It gets its own child so it cannot be misread as a route at the path
		// itself — "GET /api" is not a thing, and printing it there would say
		// it was.
		if strings.HasSuffix(r.Path, "/") {
			cur = cur.child("(everything else)")
		}
		cur.Methods = append(cur.Methods, r)
	}
	return root
}

// methodOrder sorts a node's methods the way a reader scans a resource: read
// first, then the writes in increasing order of consequence.
var methodOrder = map[string]int{"GET": 0, "POST": 1, "PUT": 2, "PATCH": 3, "DELETE": 4, "": 5}

// sortKey orders siblings so a resource's own sub-paths come before its
// parameterized child — "/teams/list" reads before "/teams/{team_id}" — and
// literal segments group above templates rather than interleaving with them.
func sortKey(seg string) (int, string) {
	if strings.HasPrefix(seg, "{") {
		return 1, seg
	}
	if seg == "(everything else)" {
		return 2, seg
	}
	return 0, seg
}

func printTree(routes []route) {
	root := build(routes)

	fmt.Println("Triage Factory — HTTP surface")
	fmt.Println()
	fmt.Println("  *  CSRF same-origin check (registered mutating)")
	fmt.Println("  !  raw mount — no session wrapper; authenticates by other means or is public")
	fmt.Println("  ~  registered outside internal/server (the package is named)")
	fmt.Println()

	// The tree is printed twice over: once to measure, once to emit, so the
	// method column lands in the same place on every line however deep.
	// tree(1) names the root it walked before drawing anything under it.
	root.Seg = "/"

	width := 0
	walk(root, "", false, true, func(line string, _ *node) {
		if len(line) > width {
			width = len(line)
		}
	})
	width += 2

	walk(root, "", false, false, func(line string, n *node) {
		if len(n.Methods) == 0 {
			fmt.Println(line)
			return
		}
		fmt.Printf("%-*s%s\n", width, line, describe(n.Methods))
	})

	fmt.Println()
	fmt.Printf("%d routes\n", len(routes))
}

// walk renders each node's prefixed line and hands it to emit. measuring
// suppresses nothing — it exists only so the caller can tell the two passes
// apart if it ever needs to.
func walk(n *node, prefix string, isRoot, measuring bool, emit func(string, *node)) {
	if !isRoot {
		emit(prefix+n.Seg, n)
	}
	segs := make([]string, 0, len(n.Children))
	for s := range n.Children {
		segs = append(segs, s)
	}
	sort.Slice(segs, func(i, j int) bool {
		ai, as := sortKey(segs[i])
		bi, bs := sortKey(segs[j])
		if ai != bi {
			return ai < bi
		}
		return as < bs
	})

	// The root's children start flush; everything below is drawn under its
	// parent's continuation bar.
	childPrefix := ""
	if !isRoot {
		childPrefix = strings.TrimSuffix(prefix, "├── ")
		childPrefix = strings.TrimSuffix(childPrefix, "└── ")
		if strings.HasSuffix(prefix, "├── ") {
			childPrefix += "│   "
		} else if strings.HasSuffix(prefix, "└── ") {
			childPrefix += "    "
		}
	}
	for i, s := range segs {
		branch := "├── "
		if i == len(segs)-1 {
			branch = "└── "
		}
		walk(n.Children[s], childPrefix+branch, false, measuring, emit)
	}
}

// describe renders one node's registrations: the methods, their auth marks,
// and — only when it is not the core server — where the route was installed.
func describe(rs []route) string {
	sort.Slice(rs, func(i, j int) bool {
		return methodOrder[rs[i].Method] < methodOrder[rs[j].Method]
	})
	parts := make([]string, 0, len(rs))
	pkgs := map[string]bool{}
	for _, r := range rs {
		m := r.Method
		if m == "" {
			m = "ANY"
		}
		parts = append(parts, strings.TrimSpace(m+r.Class.mark()))
		if r.Pkg != "internal/server" {
			pkgs[r.Pkg] = true
		}
	}
	out := strings.Join(parts, " ")
	if len(pkgs) > 0 {
		names := make([]string, 0, len(pkgs))
		for p := range pkgs {
			names = append(names, p)
		}
		sort.Strings(names)
		out += "   ~ " + strings.Join(names, ", ")
	}
	return out
}

func printFlat(routes []route, filter string) {
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return methodOrder[routes[i].Method] < methodOrder[routes[j].Method]
	})
	n := 0
	for _, r := range routes {
		if filter != "all" && !strings.HasPrefix(r.Path, filter) {
			continue
		}
		m := r.Method
		if m == "" {
			m = "ANY"
		}
		fmt.Printf("%-7s %s%s\n", m+r.Class.mark(), r.Path, pkgSuffix(r))
		n++
	}
	fmt.Fprintf(os.Stderr, "%d routes\n", n)
}

func pkgSuffix(r route) string {
	if r.Pkg == "internal/server" {
		return ""
	}
	return "   ~ " + r.Pkg
}
