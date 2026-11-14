package telemetry

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestNoSpanCarriesAnErrorsText keeps an error's text off every span TF's
// code writes. An error's text is tenant data (git output, repository names,
// paths, a provider's response), and a span is exported to a backend the
// operator may not run, so outside this package:
//
//   - no span.RecordError, whose exception event holds err.Error();
//   - no SetStatus(codes.Error, msg) with a message that is not a constant;
//   - no RecordSpanError(span, err, msg) with a message that is not a
//     constant.
//
// A constant is a string literal, a concatenation of constants, or a const
// declared in the same package. This package is exempt because it defines
// the helper and the scrubbed span, and _test.go files because they build
// spans to assert behavior.
func TestNoSpanCarriesAnErrorsText(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	exempt := filepath.Join(repoRoot, "internal", "telemetry")

	files := map[string][]string{}
	err = filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", "frontend", ".git", "testdata":
				return filepath.SkipDir
			}
			if path == exempt {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			dir := filepath.Dir(path)
			files[dir] = append(files[dir], path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	var offenders []string
	scanned := 0
	for _, paths := range files {
		fset := token.NewFileSet()
		var parsed []*ast.File
		for _, path := range paths {
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				// The build catches a syntax error in a production file.
				continue
			}
			parsed = append(parsed, f)
		}
		consts := packageConsts(parsed)
		for _, f := range parsed {
			scanned++
			for _, off := range spanErrorTextOffences(fset, f, consts) {
				rel, relErr := filepath.Rel(repoRoot, off.pos.Filename)
				if relErr != nil {
					rel = off.pos.Filename
				}
				offenders = append(offenders, rel+":"+strconv.Itoa(off.pos.Line)+": "+off.what)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no Go files; the walk is not reaching the repository")
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("an error's text reaches a span at:\n  %s\n\nCall telemetry.RecordSpanError(span, err, \"<what failed>\") with a constant message naming the stage, and log err with the span's ctx (log.WarnContext(ctx, ...)), where the operator finds it by the trace's id.", strings.Join(offenders, "\n  "))
	}
}

// TestSpanErrorTextOffencesCatchesEachForm checks the detector itself, so the
// guard above cannot pass by matching nothing.
func TestSpanErrorTextOffencesCatchesEachForm(t *testing.T) {
	const src = `package p

const stage = "the stage failed"

func f() {
	const local = "the local stage failed"
	var dynamic = "x"
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	span.SetStatus(codes.Error, dynamic)
	span.SetStatus(codes.Error, string(class))
	span.SetStatus(codes.Error, "fixed")
	span.SetStatus(codes.Error, stage)
	span.SetStatus(codes.Error, local)
	span.SetStatus(codes.Error, "a " + stage)
	span.SetStatus(codes.Ok, "")
	telemetry.RecordSpanError(span, err, err.Error())
	telemetry.RecordSpanError(span, err, "fixed")
	telemetry.RecordSpanError(span, err, other)
	seed.SetStatus(t, id, status)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// "other" stands for a const declared in another file of the package.
	consts := packageConsts([]*ast.File{f})
	consts["other"] = true

	var got []int
	for _, off := range spanErrorTextOffences(fset, f, consts) {
		got = append(got, off.pos.Line)
	}
	want := []int{8, 9, 10, 11, 17}
	if len(got) != len(want) {
		t.Fatalf("offences on lines %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("offences on lines %v, want %v", got, want)
		}
	}
}

type spanErrorTextOffence struct {
	pos  token.Position
	what string
}

// spanErrorTextOffences finds the calls in f that would put an error's text on
// a span. consts holds the names of the package's top-level constants, which
// the parser resolves only within the file that declares them.
func spanErrorTextOffences(fset *token.FileSet, f *ast.File, consts map[string]bool) []spanErrorTextOffence {
	var out []spanErrorTextOffence
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		report := func(what string) {
			out = append(out, spanErrorTextOffence{pos: fset.Position(call.Pos()), what: what})
		}
		switch sel.Sel.Name {
		case "RecordError":
			report("RecordError records an exception event holding the error's text")
		case "SetStatus":
			if len(call.Args) == 2 && isSelector(call.Args[0], "codes", "Error") && !isConstant(call.Args[1], consts) {
				report("SetStatus(codes.Error, …) with a message that is not a constant")
			}
		case "RecordSpanError":
			if len(call.Args) == 3 && isIdent(sel.X, "telemetry") && !isConstant(call.Args[2], consts) {
				report("RecordSpanError with a message that is not a constant")
			}
		}
		return true
	})
	return out
}

// packageConsts is the set of constant names declared at the top level of
// files.
func packageConsts(files []*ast.File) map[string]bool {
	consts := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					consts[name.Name] = true
				}
			}
		}
	}
	return consts
}

func isConstant(e ast.Expr, consts map[string]bool) bool {
	switch e := e.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.ParenExpr:
		return isConstant(e.X, consts)
	case *ast.BinaryExpr:
		return e.Op == token.ADD && isConstant(e.X, consts) && isConstant(e.Y, consts)
	case *ast.Ident:
		// A name the parser resolved is declared in this file, and is a
		// constant only if it was declared as one; an unresolved name is
		// declared elsewhere in the package.
		if e.Obj != nil {
			return e.Obj.Kind == ast.Con
		}
		return consts[e.Name]
	}
	return false
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name && isIdent(sel.X, pkg)
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
