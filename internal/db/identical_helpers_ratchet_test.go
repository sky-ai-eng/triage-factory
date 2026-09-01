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

// This file records the decision for the helpers duplicated between
// the SQLite and Postgres stores.
//
// Tier A stays duplicated because its small pure-Go helpers are safe but
// low-value to hoist. Tier B stays duplicated so a scanner can acquire real
// driver-specific SQL/NULL behavior without first undoing a shared
// abstraction. Both tiers are guarded here: an accidental one-sided edit
// fails immediately, and a newly copied helper cannot silently enlarge the
// set.
//
// Tier C is split on its structural boundary. PendingRow and the four pure
// consumers of an already-scanned row live in pending_rows.go. Helpers that
// still accept a dialect-local queryer or another local implementation type
// remain beside their SQL and are guarded here. SQL text, placeholder syntax,
// casts, RLS parameters, and driver NULL scanning are deliberately out of
// scope for sharing.

// identicalHelperRatchet is the SHRINKING section. Every name is a top-level
// helper whose complete func declaration (excluding its doc comment) is
// currently byte-identical in sqlite and postgres. Moving a helper to shared
// code deletes its entry. Diverging one requires an explicit decision that it
// acquired dialect content; merely deleting the entry to make this test pass
// defeats the ratchet.
var identicalHelperRatchet = []string{
	"buildDashboardTimeline",
	"claimOutcomeForStatus",
	"collectURLRewrites",
	"derefFloat",
	"derefInt",
	"floatPtrFromNull",
	"int64PtrFromNull",
	"intPtrFromNull",
	"normalizeEmail",
	"normalizeReachable",
	"nullBotUserID",
	"nullIfEmpty",
	"nullString",
	"nullStringToPtr",
	"observedAt",
	"queryTasksCtx",
	"refreshInstant",
	"reverseTaskMemories",
	"scanArtifactRows",
	"scanAuthEvent",
	"scanAuthEventRows",
	"scanEntityRow",
	"scanExternalAction",
	"scanExternalActionRows",
	"scanFailedEvent",
	"scanIntIDs",
	"scanJiraStatusRules",
	"scanMessageRow",
	"scanMessageRows",
	"scanOneExecutorClaim",
	"scanOrgEventSourceRow",
	"scanPendingBind",
	"scanReachableRepos",
	"scanSpendRow",
	"scanTaskBareRow",
	"scanTaskFields",
	"scanTaskFromRow",
	"scanTaskMemories",
	"scanTaskMemory",
	"scanTeamRow",
	"scanTeamSettings",
	"scanWrittenEntity",
	"splitRepoSlug",
}

func TestRatchet_IdenticalDialectHelpersStayConverged(t *testing.T) {
	const repoRoot = "../.."
	sqlite, err := topLevelFuncSources(filepath.Join(repoRoot, "internal/db/sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	postgres, err := topLevelFuncSources(filepath.Join(repoRoot, "internal/db/postgres"))
	if err != nil {
		t.Fatal(err)
	}

	if !sort.StringsAreSorted(identicalHelperRatchet) {
		t.Fatal("identicalHelperRatchet must stay sorted")
	}
	allowed := make(map[string]bool, len(identicalHelperRatchet))
	for _, name := range identicalHelperRatchet {
		if allowed[name] {
			t.Fatalf("duplicate ratchet entry: %s", name)
		}
		allowed[name] = true
	}

	var violations []string
	for _, name := range identicalHelperRatchet {
		sqliteFunc, sqliteOK := sqlite[name]
		postgresFunc, postgresOK := postgres[name]
		if !sqliteOK || !postgresOK {
			violations = append(violations, fmt.Sprintf(
				"stale ratchet entry %s: sqlite present=%t, postgres present=%t — if it moved to shared code, delete the entry so the ratchet shrinks",
				name, sqliteOK, postgresOK))
			continue
		}
		if sqliteFunc.source != postgresFunc.source {
			violations = append(violations, fmt.Sprintf(
				"identical helper %s drifted between %s and %s — converge the edit, or record why the helper acquired real dialect content before removing it from the ratchet",
				name, sqliteFunc.path, postgresFunc.path))
		}
	}

	for name, sqliteFunc := range sqlite {
		// Same-named constructors wire package-local implementations. They were
		// excluded from TFAC-874's 46-helper measurement and are not candidates
		// for sharing even when their one-line bodies happen to match.
		if strings.HasPrefix(name, "new") || allowed[name] {
			continue
		}
		postgresFunc, ok := postgres[name]
		if ok && sqliteFunc.source == postgresFunc.source {
			violations = append(violations, fmt.Sprintf(
				"new byte-identical dialect helper %s in %s and %s — share it or make the duplication an explicit ratchet decision",
				name, sqliteFunc.path, postgresFunc.path))
		}
	}

	sort.Strings(violations)
	for _, violation := range violations {
		t.Error(violation)
	}
}

type topLevelFuncSource struct {
	path   string
	source string
}

func topLevelFuncSources(root string) (map[string]topLevelFuncSource, error) {
	if _, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("scan root %s missing: %w", root, err)
	}

	funcs := map[string]topLevelFuncSource{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			if _, duplicate := funcs[fn.Name.Name]; duplicate {
				return fmt.Errorf("duplicate top-level func %s while scanning %s", fn.Name.Name, root)
			}
			start := fset.Position(fn.Pos()).Offset
			end := fset.Position(fn.End()).Offset
			funcs[fn.Name.Name] = topLevelFuncSource{
				path:   filepath.ToSlash(path),
				source: string(source[start:end]),
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", root, err)
	}
	return funcs, nil
}
