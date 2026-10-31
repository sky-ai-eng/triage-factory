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

// This file records the decision for the top-level helpers whose declarations
// are byte-identical in the SQLite and Postgres stores. Each entry in
// identicalHelperRatchet carries its tier: the reason the helper is duplicated
// rather than shared, given on the tier's constant. Every tier is guarded the
// same way. An entry that drifts between the dialects fails, and a newly copied
// helper with no entry fails, so a one-sided edit or a silent copy is caught
// immediately. Only tierPure is expected to shrink.
//
// The boundary between the tiers is what may be shared. Go code over values
// that are already scanned can be: PendingRow and its consumers live in
// pending_rows.go. SQL text, placeholder syntax, casts, RLS parameters and
// driver NULL scanning stay per-dialect.

// helperTier is why a byte-identical dialect helper is duplicated.
type helperTier int

const (
	// tierPure is a small pure-Go helper that touches no SQL text and no driver
	// row (derefFloat, nullIfEmpty, normalizeEmail). It could move to shared
	// code and is duplicated only because each one is low-value to hoist. This
	// tier is the shrinking set: moving a helper to shared code deletes its
	// entry, and a new pure helper is better shared than added here.
	tierPure helperTier = iota + 1

	// tierScanner reads driver rows into domain values: a scan function, or a
	// loop that runs a query through the dialect's queryer and scans each row
	// (queryTasksCtx, collectURLRewrites). Scanners are per-dialect by
	// decision, so one can take driver-specific NULL handling without first
	// undoing a shared abstraction. Scanners that differ between the dialects
	// already do so for that reason: Postgres scanDeliverable scans straight
	// into strings, while SQLite's scans sql.NullString and unwraps it. An
	// identical scanner is expected to stay listed; it is not part of the
	// shrinking set.
	tierScanner

	// tierSQLFragment composes SQL text (liveTopLevelConversationSQL,
	// task*SQL, workspaceKey*SQL, Episode*SQL). Fragments are per-dialect by
	// decision. Each is identical only because the predicate it spells needs no
	// placeholder, cast or json guard yet, and sharing one would mean undoing
	// the abstraction the first time one dialect needs its own spelling. What
	// must not drift is the predicate's meaning, and the dual-dialect
	// conformance suites pin that by asserting both backends answer alike.
	//
	// The workspaceKey*SQL family is the case in evidence: its two listed
	// members match, while workspaceKeyIdleSinceSQL is unlisted because it
	// already needs its own spelling — SQLite wraps every timestamp in
	// datetime() to normalize the mixed on-disk formats a bare MAX would order
	// lexically.
	//
	// EpisodeSetupFailuresSQL, EpisodeLostEngagementsSQL,
	// EpisodeUpstreamHandBacksSQL and EpisodeJailLossesSQL are each a one-line
	// selection over their dialect's episodeHandBacksSQL, which already differs
	// (Postgres casts its count), so they are identical only because the
	// outcomes each selects need no dialect spelling. The claim-predicate and
	// hand-back conformance suites pin their meaning by reading the counts off
	// a claim in both backends.
	//
	// liveTopLevelConversationSQL is also a base that other fragment builders
	// in its dialect compose over (taskLiveConversationSQL, and the
	// live-conversation readers beside it), so a shared version would pull
	// their composition across the dialect boundary with it. The conformance
	// suites assert both backends answer alike about a concluded row, an ended
	// row and a subagent row.
	tierSQLFragment
)

func (t helperTier) String() string {
	switch t {
	case tierPure:
		return "tierPure"
	case tierScanner:
		return "tierScanner"
	case tierSQLFragment:
		return "tierSQLFragment"
	}
	return fmt.Sprintf("helperTier(%d)", int(t))
}

func (t helperTier) known() bool {
	return t == tierPure || t == tierScanner || t == tierSQLFragment
}

// helperTierChoices is the part of the new-helper failure that names where to
// file the helper.
const helperTierChoices = "tierPure (pure Go; the shrinking set, so sharing it is the expected fix), " +
	"tierScanner (reads driver rows; per-dialect by decision), " +
	"tierSQLFragment (composes SQL text; per-dialect by decision)"

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
	for _, violation := range identicalHelperViolations(identicalHelperRatchet, sqlite, postgres) {
		t.Error(violation)
	}
}

// TestIdenticalHelperViolations runs the ratchet's checks over synthetic
// dialect sources, so each failure the real ratchet can report is known to
// fire.
func TestIdenticalHelperViolations(t *testing.T) {
	src := func(path, body string) topLevelFuncSource {
		return topLevelFuncSource{path: path, source: body}
	}
	sqlite := map[string]topLevelFuncSource{
		"pureHelper":  src("sqlite/a.go", "func pureHelper() {}"),
		"scanThing":   src("sqlite/a.go", "func scanThing() { sqlite }"),
		"thingSQL":    src("sqlite/a.go", "func thingSQL() { sqlite }"),
		"copiedAgain": src("sqlite/a.go", "func copiedAgain() {}"),
		"newStore":    src("sqlite/a.go", "func newStore() {}"),
	}
	postgres := map[string]topLevelFuncSource{
		"pureHelper":  src("postgres/a.go", "func pureHelper() {}"),
		"scanThing":   src("postgres/a.go", "func scanThing() { postgres }"),
		"thingSQL":    src("postgres/a.go", "func thingSQL() { postgres }"),
		"copiedAgain": src("postgres/a.go", "func copiedAgain() {}"),
		"newStore":    src("postgres/a.go", "func newStore() {}"),
	}
	ratchet := map[string]helperTier{
		"pureHelper": tierPure,
		"scanThing":  tierScanner,
		"thingSQL":   tierSQLFragment,
		"goneHelper": tierPure,
		"untiered":   0,
	}

	got := strings.Join(identicalHelperViolations(ratchet, sqlite, postgres), "\n")
	for _, want := range []string{
		"tierScanner helper scanThing drifted",
		"tierSQLFragment helper thingSQL drifted",
		"stale ratchet entry goneHelper (tierPure)",
		"ratchet entry untiered has helperTier(0)",
		"new byte-identical dialect helper copiedAgain",
		"tierPure (pure Go; the shrinking set",
		"tierScanner (reads driver rows",
		"tierSQLFragment (composes SQL text",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("violations missing %q; got:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"pureHelper", "newStore"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("violations name %s, which is converged or exempt; got:\n%s", unwanted, got)
		}
	}
}

// identicalHelperViolations checks ratchet against the scanned sqlite and
// postgres sources and returns every failure, sorted.
func identicalHelperViolations(ratchet map[string]helperTier, sqlite, postgres map[string]topLevelFuncSource) []string {
	var violations []string
	for name, tier := range ratchet {
		if !tier.known() {
			violations = append(violations, fmt.Sprintf(
				"ratchet entry %s has %s — file it under one of %s",
				name, tier, helperTierChoices))
			continue
		}
		sqliteFunc, sqliteOK := sqlite[name]
		postgresFunc, postgresOK := postgres[name]
		if !sqliteOK || !postgresOK {
			violations = append(violations, fmt.Sprintf(
				"stale ratchet entry %s (%s): sqlite present=%t, postgres present=%t — if it moved to shared code or was deleted, delete the entry; if it was renamed, rename the entry",
				name, tier, sqliteOK, postgresOK))
			continue
		}
		if sqliteFunc.source != postgresFunc.source {
			violations = append(violations, fmt.Sprintf(
				"identical %s helper %s drifted between %s and %s — converge the edit, or record why the helper acquired real dialect content before removing it from the ratchet",
				tier, name, sqliteFunc.path, postgresFunc.path))
		}
	}

	for name, sqliteFunc := range sqlite {
		// Same-named constructors wire package-local implementations; they are
		// not candidates for sharing even when their one-line bodies happen to
		// match.
		if _, listed := ratchet[name]; listed || strings.HasPrefix(name, "new") {
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
	return violations
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
