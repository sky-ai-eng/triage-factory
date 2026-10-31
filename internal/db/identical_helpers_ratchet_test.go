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
// immediately. Only tierPure is expected to shrink. No name exempts a helper
// from either check, and a name does not decide its tier; what the function
// does decides it.
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
	// undoing a shared abstraction. Some scanners already differ for that
	// reason: Postgres scanDeliverable scans straight into strings, while
	// SQLite's scans sql.NullString and unwraps it. An identical scanner is
	// expected to stay listed; it is not part of the shrinking set.
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

	// tierConstructor builds a dialect's own store type (newEventStore,
	// newScoreStore). Its text matches only because both dialects spell the
	// type and its fields alike; each copy names a different package-local
	// type, so there is nothing to share. It is not part of the shrinking set,
	// and it leaves the ratchet once one dialect's store gains a field the
	// other's lacks.
	tierConstructor
)

// helperTiers is every tier with its name and the one-line guidance the
// failures give for filing a helper under it. String, known and
// helperTierChoices all read this table.
var helperTiers = []struct {
	tier     helperTier
	name     string
	guidance string
}{
	{tierPure, "tierPure", "pure Go; the shrinking set, so sharing it is the expected fix"},
	{tierScanner, "tierScanner", "reads driver rows; per-dialect by decision"},
	{tierSQLFragment, "tierSQLFragment", "composes SQL text; per-dialect by decision"},
	{tierConstructor, "tierConstructor", "builds the dialect's own store type; nothing to share"},
}

func (t helperTier) String() string {
	for _, d := range helperTiers {
		if d.tier == t {
			return d.name
		}
	}
	return fmt.Sprintf("helperTier(%d)", int(t))
}

func (t helperTier) known() bool {
	for _, d := range helperTiers {
		if d.tier == t {
			return true
		}
	}
	return false
}

// helperTierChoices names every tier with its guidance, for the failures that
// ask where to file a helper.
func helperTierChoices() string {
	choices := make([]string, len(helperTiers))
	for i, d := range helperTiers {
		choices[i] = d.name + " (" + d.guidance + ")"
	}
	return strings.Join(choices, ", ")
}

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
		"convergedHelper": src("sqlite/a.go", "func convergedHelper() {}"),
		"pureThing":       src("sqlite/a.go", "func pureThing() { sqlite }"),
		"scanThing":       src("sqlite/a.go", "func scanThing() { sqlite }"),
		"thingSQL":        src("sqlite/a.go", "func thingSQL() { sqlite }"),
		"newThingStore":   src("sqlite/a.go", "func newThingStore() { sqlite }"),
		"oneSided":        src("sqlite/a.go", "func oneSided() {}"),
		"copiedAgain":     src("sqlite/a.go", "func copiedAgain() {}"),
		"newStore":        src("sqlite/a.go", "func newStore() {}"),
	}
	postgres := map[string]topLevelFuncSource{
		"convergedHelper": src("postgres/a.go", "func convergedHelper() {}"),
		"pureThing":       src("postgres/a.go", "func pureThing() { postgres }"),
		"scanThing":       src("postgres/a.go", "func scanThing() { postgres }"),
		"thingSQL":        src("postgres/a.go", "func thingSQL() { postgres }"),
		"newThingStore":   src("postgres/a.go", "func newThingStore() { postgres }"),
		"copiedAgain":     src("postgres/a.go", "func copiedAgain() {}"),
		"newStore":        src("postgres/a.go", "func newStore() {}"),
	}
	ratchet := map[string]helperTier{
		"convergedHelper": tierPure,
		"pureThing":       tierPure,
		"scanThing":       tierScanner,
		"thingSQL":        tierSQLFragment,
		"newThingStore":   tierConstructor,
		"goneHelper":      tierPure,
		"oneSided":        tierScanner,
		"untiered":        0,
	}

	got := strings.Join(identicalHelperViolations(ratchet, sqlite, postgres), "\n")
	for _, want := range []string{
		"identical tierPure helper pureThing drifted",
		"identical tierScanner helper scanThing drifted",
		"identical tierSQLFragment helper thingSQL drifted",
		"identical tierConstructor helper newThingStore drifted",
		"stale ratchet entry goneHelper (tierPure): in neither dialect",
		"stale ratchet entry oneSided (tierScanner): only sqlite has it — restore it in postgres",
		"ratchet entry untiered has helperTier(0)",
		// A name prefix exempts nothing: an identical newStore with no entry
		// fails like any other helper.
		"new byte-identical dialect helper newStore",
		"new byte-identical dialect helper copiedAgain",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("violations missing %q; got:\n%s", want, got)
		}
	}
	for _, d := range helperTiers {
		if want := d.name + " (" + d.guidance + ")"; !strings.Contains(got, want) {
			t.Errorf("new-helper failure does not name %s; got:\n%s", d.name, got)
		}
	}
	if strings.Contains(got, "convergedHelper") {
		t.Errorf("violations name convergedHelper, which is listed and converged; got:\n%s", got)
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
				name, tier, helperTierChoices()))
			continue
		}
		sqliteFunc, sqliteOK := sqlite[name]
		postgresFunc, postgresOK := postgres[name]
		switch {
		case !sqliteOK && !postgresOK:
			violations = append(violations, fmt.Sprintf(
				"stale ratchet entry %s (%s): in neither dialect — delete the entry if the helper moved to shared code or was removed, or rename it to follow the helper",
				name, tier))
			continue
		case !sqliteOK || !postgresOK:
			present, missing := "sqlite", "postgres"
			if !sqliteOK {
				present, missing = missing, present
			}
			violations = append(violations, fmt.Sprintf(
				"stale ratchet entry %s (%s): only %s has it — restore it in %s, or record why the dialects stopped sharing it before removing it from the ratchet",
				name, tier, present, missing))
			continue
		}
		if sqliteFunc.source != postgresFunc.source {
			violations = append(violations, fmt.Sprintf(
				"identical %s helper %s drifted between %s and %s — converge the edit, or record why the helper acquired real dialect content before removing it from the ratchet",
				tier, name, sqliteFunc.path, postgresFunc.path))
		}
	}

	for name, sqliteFunc := range sqlite {
		if _, listed := ratchet[name]; listed {
			continue
		}
		postgresFunc, ok := postgres[name]
		if ok && sqliteFunc.source == postgresFunc.source {
			violations = append(violations, fmt.Sprintf(
				"new byte-identical dialect helper %s in %s and %s — share it, or add it to identicalHelperRatchet under its tier: %s",
				name, sqliteFunc.path, postgresFunc.path, helperTierChoices()))
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
