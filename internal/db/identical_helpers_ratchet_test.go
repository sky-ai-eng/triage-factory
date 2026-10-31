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

// This file records the decision for the helpers duplicated between the SQLite
// and Postgres stores. Each entry carries its tier, the reason it is duplicated
// rather than shared. Every tier gets both checks: an entry that drifts between
// the dialects fails, and a new byte-identical helper with no entry fails. No
// name exempts a helper or decides its tier, and only tierPure is expected to
// shrink. Go over already-scanned values can be shared (PendingRow and its
// consumers in pending_rows.go); SQL text, placeholder syntax, casts, RLS
// parameters and driver NULL scanning stay per-dialect.

// helperTier is why a byte-identical dialect helper is duplicated.
type helperTier int

const (
	// tierPure is a small pure-Go helper with no SQL text and no driver row
	// (derefFloat, nullIfEmpty). It could be shared and is duplicated only
	// because each is low-value to hoist. This is the shrinking set.
	tierPure helperTier = iota

	// tierScanner reads driver rows: a scan function, or a loop that queries
	// and scans (queryTasksCtx, collectURLRewrites). Per-dialect by decision,
	// so a scanner can take driver-specific NULL handling without undoing a
	// shared abstraction; the two scanDeliverable copies already differ that
	// way.
	tierScanner

	// tierSQLFragment composes SQL text (liveTopLevelConversationSQL,
	// task*SQL, workspaceKey*SQL, Episode*SQL). Per-dialect by decision: each
	// is identical only because its predicate needs no placeholder, cast or
	// json guard yet, and siblings in the same families already diverge.
	// liveTopLevelConversationSQL is also a base other fragments in its
	// dialect compose over, so sharing it would pull their composition across
	// the dialect boundary. The dual-dialect conformance suites pin what the
	// predicates mean.
	tierSQLFragment

	// tierConstructor builds a dialect's own store type (newEventStore). The
	// text matches, but each copy names its own package's type, so there is
	// nothing to share.
	tierConstructor
)

// helperTierChoices is what the new-helper failure says about where to file a
// helper.
const helperTierChoices = "tierPure (pure Go; the shrinking set, so sharing it is the expected fix), " +
	"tierScanner (reads driver rows; per-dialect by decision), " +
	"tierSQLFragment (composes SQL text; per-dialect by decision), " +
	"tierConstructor (builds the dialect's own store type; nothing to share)"

// identicalHelperRatchet maps every top-level helper whose complete func
// declaration (excluding its doc comment) is currently byte-identical in sqlite
// and postgres to its tier. Moving a helper to shared code deletes its entry.
// Diverging one requires an explicit decision that it acquired dialect content;
// merely deleting the entry to make this test pass defeats the ratchet.
var identicalHelperRatchet = map[string]helperTier{
	"buildDashboardTimeline": tierPure,
	"claimOutcomeForStatus":  tierPure,
	"derefFloat":             tierPure,
	"derefInt":               tierPure,
	"floatPtrFromNull":       tierPure,
	"int64PtrFromNull":       tierPure,
	"intPtrFromNull":         tierPure,
	"normalizeEmail":         tierPure,
	"normalizeReachable":     tierPure,
	"nullBotUserID":          tierPure,
	"nullIfEmpty":            tierPure,
	"nullString":             tierPure,
	"nullStringToPtr":        tierPure,
	"observedAt":             tierPure,
	"refreshInstant":         tierPure,
	"reverseTaskMemories":    tierPure,

	"collectURLRewrites":     tierScanner,
	"queryListedTasksCtx":    tierScanner,
	"queryTasksCtx":          tierScanner,
	"scanAuthEvent":          tierScanner,
	"scanAuthEventRows":      tierScanner,
	"scanConnectionStatus":   tierScanner,
	"scanDeliverableRows":    tierScanner,
	"scanEntityRow":          tierScanner,
	"scanEvent":              tierScanner,
	"scanExternalAction":     tierScanner,
	"scanExternalActionRows": tierScanner,
	"scanIntIDs":             tierScanner,
	"scanJiraStatusRules":    tierScanner,
	"scanMessageRow":         tierScanner,
	"scanMessageRows":        tierScanner,
	"scanOneExecutorClaim":   tierScanner,
	"scanOperator":           tierScanner,
	"scanOrgEventSourceRow":  tierScanner,
	"scanPendingBind":        tierScanner,
	"scanReachableRepos":     tierScanner,
	"scanSpendRow":           tierScanner,
	"scanTaskBareRow":        tierScanner,
	"scanTaskCloseDeferral":  tierScanner,
	"scanTaskFields":         tierScanner,
	"scanTaskFromRow":        tierScanner,
	"scanTaskMemories":       tierScanner,
	"scanTaskMemory":         tierScanner,
	"scanTeamRow":            tierScanner,
	"scanTeamSettings":       tierScanner,
	"scanWorktree":           tierScanner,
	"scanWrittenEntity":      tierScanner,

	"EpisodeJailLossesSQL":        tierSQLFragment,
	"EpisodeLostEngagementsSQL":   tierSQLFragment,
	"EpisodeSetupFailuresSQL":     tierSQLFragment,
	"EpisodeUpstreamHandBacksSQL": tierSQLFragment,
	"liveTopLevelConversationSQL": tierSQLFragment,
	"taskLiveConversationSQL":     tierSQLFragment,
	"taskMemoryAttemptSQL":        tierSQLFragment,
	"taskMemoryOwedRowSQL":        tierSQLFragment,
	"taskMemoryPendingSQL":        tierSQLFragment,
	"workspaceKeyMembersSQL":      tierSQLFragment,
	"workspaceKeyUnclaimedSQL":    tierSQLFragment,

	"newCommitSignatureStore":   tierConstructor,
	"newConversationQueueStore": tierConstructor,
	"newDashboardStore":         tierConstructor,
	"newEventStore":             tierConstructor,
	"newFactoryReadStore":       tierConstructor,
	"newModelAvailabilityStore": tierConstructor,
	"newScoreStore":             tierConstructor,
	"newTeamActivityStore":      tierConstructor,
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

	var violations []string
	for name := range identicalHelperRatchet {
		sqliteFunc, sqliteOK := sqlite[name]
		postgresFunc, postgresOK := postgres[name]
		if !sqliteOK || !postgresOK {
			violations = append(violations, fmt.Sprintf(
				"stale ratchet entry %s: sqlite present=%t, postgres present=%t — if it moved to shared code or was renamed, update the entry; if one dialect lost it, restore it there or record why before removing the entry",
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
		if _, listed := identicalHelperRatchet[name]; listed {
			continue
		}
		postgresFunc, ok := postgres[name]
		if ok && sqliteFunc.source == postgresFunc.source {
			violations = append(violations, fmt.Sprintf(
				"new byte-identical dialect helper %s in %s and %s — share it, or add it to identicalHelperRatchet under its tier: %s",
				name, sqliteFunc.path, postgresFunc.path, helperTierChoices))
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
