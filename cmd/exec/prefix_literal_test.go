package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// hardcodedPrefixAllowlist names the files still permitted to contain the
// literal command prefix in a string. Shrinking only — an entry may be removed,
// never added.
//
// prog.go owns the one legitimate occurrence: the fallback used when argv0 is
// empty, which is where the canonical spelling is defined for everyone else.
var hardcodedPrefixAllowlist = map[string]bool{
	filepath.Join("prog", "prog.go"): true,
}

// TestNoHardcodedInvocationPrefix is the guard that keeps the CLI's help output
// honest. `triagefactory exec` is the wrong spelling in every environment we
// ship: the native jail teaches `tfac`, the SDK sandbox teaches an absolute
// per-run binary path, and in local mode the binary is wherever the operator
// built it and often isn't on PATH under that name at all. A usage line naming a
// command the reader cannot run costs the agent turns of blind retrying, which
// is exactly what happened in the first native-loop dogfood run.
//
// So every user-facing site derives its prefix from argv0 via prog.Prefix(),
// and this test asserts no string literal re-introduces the constant.
// Test files are exempt: pinning the output requires naming both spellings.
func TestNoHardcodedInvocationPrefix(t *testing.T) {
	const forbidden = "triagefactory exec"
	fset := token.NewFileSet()

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if hardcodedPrefixAllowlist[path] {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		// String literals only — a comment may (and does) discuss the canonical
		// spelling; only what reaches a user's terminal matters here.
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			if strings.Contains(val, forbidden) {
				t.Errorf("%s: string literal hardcodes %q — use prog.Prefix() so usage text names the invoked command",
					fset.Position(lit.Pos()), forbidden)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking cmd/exec: %v", err)
	}
}
