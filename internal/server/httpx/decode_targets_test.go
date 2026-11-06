package httpx

import (
	"fmt"
	"go/ast"
	"go/types"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// A request body can only declare fields whose faults the strict decode can
// report. encoding/json names a field only on its own UnmarshalTypeError, and
// these shapes fail with some other error, which reaches the client as a
// field-less INVALID_BODY:
//
//   - a type that decodes itself through UnmarshalJSON (time.Time given a
//     number) or UnmarshalText (any text type refusing its string), whose
//     error is returned unchanged;
//   - a field with the ,string option given an unquoted value, a []byte
//     given a string that is not base64, and a json.Number given a string
//     that is not a number, which fail with encoding/json's own plain errors;
//   - an embedded pointer to an unexported struct, which encoding/json cannot
//     allocate.
//
// The test type-checks every package that imports httpx, finds each value
// passed to a decode door, and refuses those shapes anywhere in its type. A
// timestamp or an id a body carries is a string the handler parses, with a
// fault that names its field.

// decodeDoors are the httpx functions that decode a request body, and the
// index of the argument each decodes into.
var decodeDoors = map[string]int{
	"DecodeJSONStrict":      2,
	"DecodeJSONStrictLimit": 2,
	"DecodeJSONStrictBytes": 2,
}

const httpxPath = "github.com/sky-ai-eng/triage-factory/internal/server/httpx"

// decodeFinding is one refused shape: the decode target, the JSON path to
// the field ("" for the body itself), and why.
type decodeFinding struct {
	target, path, reason string
}

func TestDecodeTargets_EveryFaultIsReportable(t *testing.T) {
	const repoRoot = "../../.."
	imports, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedImports, Dir: repoRoot}, "./...")
	if err != nil {
		t.Fatalf("list packages: %v", err)
	}
	var paths []string
	for _, p := range imports {
		// A package that failed to list has no imports to read, and would
		// drop out of the scan unnoticed.
		for _, e := range p.Errors {
			t.Fatalf("list %s: %v", p.PkgPath, e)
		}
		if _, ok := p.Imports[httpxPath]; ok {
			paths = append(paths, p.PkgPath)
		}
	}
	targets, findings := scanDecodeTargets(t, repoRoot, paths...)
	if targets == 0 {
		t.Fatal("found no decode targets; the scan no longer matches the decode doors")
	}
	for _, f := range findings {
		t.Errorf("%s: %s, so a bad value there reaches the client as an INVALID_BODY naming no field; "+
			"take it as a plain JSON type and parse it in the handler", f.target, f.describe())
	}
}

// TestDecodeTargets_FixtureFindings pins that the scan finds every refused
// shape in the fixture, through each decode door, and nothing it allows.
func TestDecodeTargets_FixtureFindings(t *testing.T) {
	_, findings := scanDecodeTargets(t, "../../..", httpxPath+"/testdata/decodetargets")
	var got []string
	for _, f := range findings {
		got = append(got, f.describe())
	}
	want := []string{
		"again.when is a time.Time, which decodes itself through UnmarshalJSON",
		"at is a time.Time, which decodes itself through UnmarshalJSON",
		"blob is a byte slice, which refuses a string that is not base64 without naming its field",
		"by_ref has keys of type decodetargets.textRef, which decodes itself through UnmarshalText",
		"count takes the ,string option",
		"items is a type parameter, whose fields cannot be checked here; decode into a concrete type",
		"items.when is a time.Time, which decodes itself through UnmarshalJSON",
		"n is a json.Number, which refuses a string that is not a number without naming its field",
		"ref is a decodetargets.textRef, which decodes itself through UnmarshalText",
		"the body embeds *decodetargets.hidden, a pointer to an unexported struct, which encoding/json cannot allocate",
		"the body is a decodetargets.stamped, which decodes itself through UnmarshalJSON",
		"the body is a time.Time, which decodes itself through UnmarshalJSON",
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("findings =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func (f decodeFinding) describe() string {
	if f.path == "" {
		return "the body " + f.reason
	}
	return f.path + " " + f.reason
}

// scanDecodeTargets type-checks the packages at paths and returns how many
// decode targets they pass to a decode door, and the refused shapes in them.
func scanDecodeTargets(t *testing.T, dir string, paths ...string) (int, []decodeFinding) {
	t.Helper()
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  dir,
	}, paths...)
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}
	var (
		targets  int
		findings []decodeFinding
	)
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			t.Fatalf("load %s: %v", pkg.PkgPath, e)
		}
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				fn, ok := pkg.TypesInfo.Uses[sel.Sel].(*types.Func)
				if !ok || fn.Pkg() == nil || fn.Pkg().Path() != httpxPath {
					return true
				}
				arg, ok := decodeDoors[fn.Name()]
				if !ok {
					return true
				}
				pos := pkg.Fset.Position(call.Pos())
				ptr, ok := pkg.TypesInfo.TypeOf(call.Args[arg]).(*types.Pointer)
				if !ok {
					t.Errorf("%s: %s decodes into a %s; pass a pointer to the body's own type, so its fields can be checked",
						pos, fn.Name(), pkg.TypesInfo.TypeOf(call.Args[arg]))
					return true
				}
				targets++
				target := fmt.Sprintf("%s (%s)", pos, types.TypeString(ptr.Elem(), types.RelativeTo(pkg.Types)))
				walkDecodeTarget(ptr.Elem(), "", map[types.Type]bool{}, func(path, reason string) {
					findings = append(findings, decodeFinding{target: target, path: path, reason: reason})
				})
				return true
			})
		}
	}
	return targets, findings
}

// walkDecodeTarget reports each refused shape in t, which decodes at path,
// following encoding/json's rules for which fields it decodes.
func walkDecodeTarget(t types.Type, path string, seen map[types.Type]bool, report func(path, reason string)) {
	if isJSONType(t, "RawMessage") {
		return
	}
	if isJSONType(t, "Number") {
		report(path, "is a json.Number, which refuses a string that is not a number without naming its field")
		return
	}
	// A type parameter's underlying type is its constraint, an interface,
	// so it is told apart before one.
	if _, generic := t.(*types.TypeParam); generic {
		report(path, "is a type parameter, whose fields cannot be checked here; decode into a concrete type")
		return
	}
	if _, iface := t.Underlying().(*types.Interface); iface {
		return
	}
	if method := selfDecoding(t); method != "" {
		report(path, fmt.Sprintf("is a %s, which decodes itself through %s", qualifiedName(t), method))
		return
	}
	// seen holds the types on the current path, so a recursive type ends
	// while a type reached at two paths is checked at both.
	if seen[t] {
		return
	}
	seen[t] = true
	defer delete(seen, t)
	switch u := t.Underlying().(type) {
	case *types.Pointer:
		walkDecodeTarget(u.Elem(), path, seen, report)
	case *types.Slice:
		if b, ok := u.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Uint8 {
			report(path, "is a byte slice, which refuses a string that is not base64 without naming its field")
			return
		}
		walkDecodeTarget(u.Elem(), path, seen, report)
	case *types.Array:
		walkDecodeTarget(u.Elem(), path, seen, report)
	case *types.Map:
		if method := selfDecoding(u.Key()); method != "" {
			report(path, fmt.Sprintf("has keys of type %s, which decodes itself through %s", qualifiedName(u.Key()), method))
		}
		walkDecodeTarget(u.Elem(), path, seen, report)
	case *types.Struct:
		for i := range u.NumFields() {
			f := u.Field(i)
			tag := reflect.StructTag(u.Tag(i)).Get("json")
			if tag == "-" {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			structLike := isStructType(f.Type())
			// encoding/json decodes no unexported field but an embedded
			// struct, whose exported fields it can still set.
			if !f.Exported() && (!f.Embedded() || !structLike) {
				continue
			}
			if f.Embedded() && !f.Exported() && name == "" {
				if _, ptr := f.Type().Underlying().(*types.Pointer); ptr {
					report(path, fmt.Sprintf("embeds %s, a pointer to an unexported struct, which encoding/json cannot allocate", qualifiedName(f.Type())))
					continue
				}
			}
			if f.Embedded() && name == "" && structLike {
				walkDecodeTarget(f.Type(), path, seen, report)
				continue
			}
			if name == "" {
				name = f.Name()
			}
			fieldPath := name
			if path != "" {
				fieldPath = path + "." + name
			}
			if slices.Contains(strings.Split(opts, ","), "string") {
				report(fieldPath, "takes the ,string option")
			}
			walkDecodeTarget(f.Type(), fieldPath, seen, report)
		}
	}
}

// selfDecoding names the method t decodes itself through, UnmarshalJSON
// before UnmarshalText as encoding/json prefers it, or "" for neither. A
// pointer receiver counts, since encoding/json decodes into an addressable
// value.
func selfDecoding(t types.Type) string {
	for _, method := range []string{"UnmarshalJSON", "UnmarshalText"} {
		if obj, _, _ := types.LookupFieldOrMethod(t, true, nil, method); obj != nil {
			if _, ok := obj.(*types.Func); ok {
				return method
			}
		}
	}
	return ""
}

// isJSONType reports whether t is encoding/json's type of that name.
// json.RawMessage is allowed though it decodes itself: its UnmarshalJSON
// keeps the bytes and refuses nothing.
func isJSONType(t types.Type, name string) bool {
	named, ok := types.Unalias(t).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "encoding/json" && named.Obj().Name() == name
}

// isStructType reports whether t is a struct or a pointer to one.
func isStructType(t types.Type) bool {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	_, ok := t.Underlying().(*types.Struct)
	return ok
}

// qualifiedName spells t with its package's name ("time.Time").
func qualifiedName(t types.Type) string {
	return types.TypeString(t, func(p *types.Package) string { return p.Name() })
}
