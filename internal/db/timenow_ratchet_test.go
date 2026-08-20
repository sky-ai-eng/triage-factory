package db

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This file is the stored-timestamp ratchet: a source scan over both store
// dialects that fails on a time.Now() the caller did not immediately convert
// to UTC. It is modelled on internal/server/ratchet_test.go and has the same
// one-way mechanics:
//
//   - A new bare time.Now() in a file with no entry fails immediately. The fix
//     is .UTC(), not an entry — reviewers treat a new entry as a red flag.
//   - A stale entry — one whose file no longer has a bare time.Now() — fails
//     the test, so entries can only be deleted, never accumulate.
//
// Why a ratchet rather than a comment. The SQLite DSN sets
// _time_format=sqlite, so the driver serializes a bound time.Time as
// "2006-01-02 15:04:05.999999999-07:00", preserving the WRITER's offset —
// while SQLite's own datetime('now') and CURRENT_TIMESTAMP are always UTC. A
// single bare time.Now() therefore writes a row that compares and sorts as the
// process's wall clock against everything around it: west of UTC the task
// queue's snooze filter treats a fresh snooze as already expired, so the
// gesture does nothing at all. The convention was already UTC at the
// overwhelming majority of sites before this file existed; it was just
// unenforced, and the minority that wasn't is what shipped the bug.
//
// Postgres is in scope for symmetry rather than for a live defect: every
// timestamp column there is timestamptz, so pgx sends the instant and the
// offset is irrelevant. The two dialect files are read side by side and copied
// from each other constantly, so a bare time.Now() surviving in the pg twin is
// how this comes back to SQLite.
//
// Scope: production .go files under internal/db/sqlite and internal/db/postgres.
// _test.go files are skipped — a test that stages a local-zone value on purpose
// is exercising this rule, not breaking it.

// timeNowRatchetAllowlist is the SHRINKING section: files with a bare
// time.Now() not yet converted. It is EMPTY, and shipped empty — the sweep
// that introduced this ratchet converted every site in both packages. It stays
// here as the place a regression would have to argue for itself.
var timeNowRatchetAllowlist = []string{}

func TestRatchet_StoreTimestampsAreUTC(t *testing.T) {
	const repoRoot = "../.."
	roots := []string{"internal/db/sqlite", "internal/db/postgres"}

	allowed := map[string]bool{}
	for _, f := range timeNowRatchetAllowlist {
		if allowed[f] {
			t.Fatalf("duplicate ratchet entry: %s", f)
		}
		allowed[f] = true
	}

	hits := map[string][]int{}
	for _, root := range roots {
		rootPath := filepath.Join(repoRoot, root)
		if _, err := os.Stat(rootPath); err != nil {
			t.Fatalf("scan root %s missing: %v", root, err)
		}
		err := filepath.WalkDir(rootPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			lines, err := bareTimeNowLines(path)
			if err != nil {
				return err
			}
			if len(lines) > 0 {
				hits[rel] = lines
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	var violations []string
	for file, lines := range hits {
		if !allowed[file] {
			violations = append(violations, fmt.Sprintf(
				"%s: bare time.Now() at line(s) %v — write time.Now().UTC() instead of adding an allowlist entry; "+
					"the driver stores the writer's offset and SQLite compares against UTC",
				file, lines))
		}
	}
	for _, file := range timeNowRatchetAllowlist {
		if len(hits[file]) == 0 {
			violations = append(violations, fmt.Sprintf(
				"stale allowlist entry: %s no longer has a bare time.Now() — delete the entry so the ratchet tightens",
				file))
		}
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}
}

// bareTimeNowLines reports the lines of every time.Now() call in the file that
// is not immediately converted with .UTC(). The scan is over the parsed AST
// rather than the text: a mention in a comment or a string is not a call, and
// spelling the check as a regex would have to re-derive that.
func bareTimeNowLines(path string) ([]int, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// First pass: every time.Now() that is the receiver of a .UTC() call. The
	// receiver is matched by node identity, so time.Now().UTC().Add(d) counts
	// as converted while time.Now().Add(d) does not.
	converted := map[ast.Node]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "UTC" {
			return true
		}
		if inner, ok := sel.X.(*ast.CallExpr); ok && isTimeNowCall(inner) {
			converted[inner] = true
		}
		return true
	})

	var lines []int
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isTimeNowCall(call) || converted[call] {
			return true
		}
		lines = append(lines, fset.Position(call.Pos()).Line)
		return true
	})
	sort.Ints(lines)
	return lines, nil
}

func isTimeNowCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Now" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "time"
}
