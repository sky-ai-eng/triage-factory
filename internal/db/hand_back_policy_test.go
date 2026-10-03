package db

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestHandBackPolicies_RenderTheVocabularyTheSQLUsed pins the rendered
// fragments: the four outcomes the dialects spelled by hand before the table,
// in the order they spelled them, plus requeued_upstream. A reordered or
// renamed row changes SQL every claim runs, so it has to change here too.
func TestHandBackPolicies_RenderTheVocabularyTheSQLUsed(t *testing.T) {
	if got, want := HandBackOutcomesSQL(), `'requeued','requeued_credentials','reaped','requeued_shutdown','requeued_upstream'`; got != want {
		t.Errorf("HandBackOutcomesSQL() = %s, want %s", got, want)
	}
	for budget, want := range map[HandBackBudget]string{
		BudgetSetup:    `'requeued'`,
		BudgetLoss:     `'reaped'`,
		BudgetUpstream: `'requeued_upstream'`,
		BudgetNone:     `'requeued_credentials','requeued_shutdown'`,
	} {
		if got := HandBackBudgetOutcomesSQL(budget); got != want {
			t.Errorf("HandBackBudgetOutcomesSQL(%s) = %s, want %s", budget, got, want)
		}
	}
	if got := HandBackBudgetOutcomesSQL("unspent"); got != "NULL" {
		t.Errorf("HandBackBudgetOutcomesSQL of a budget nothing spends = %s, want NULL", got)
	}
}

func TestHandBackPolicies_OutcomesAreUniqueAndKnown(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range HandBackPolicies {
		if seen[p.Outcome] {
			t.Errorf("outcome %q is declared twice", p.Outcome)
		}
		seen[p.Outcome] = true
		got, ok := HandBackPolicyFor(p.Outcome)
		if !ok || got.Outcome != p.Outcome {
			t.Errorf("HandBackPolicyFor(%q) = (%+v, %v)", p.Outcome, got, ok)
		}
	}
	for _, notHandBack := range []string{"", "completed", "failed", "parked", "cancelled"} {
		if _, ok := HandBackPolicyFor(notHandBack); ok {
			t.Errorf("HandBackPolicyFor(%q) found a policy; it is not a hand-back", notHandBack)
		}
	}
}

func TestHandBackPolicy_DelayWalksTheScheduleAndRepeatsItsLastEntry(t *testing.T) {
	upstream, ok := HandBackPolicyFor(HandBackUpstream)
	if !ok {
		t.Fatal("no requeued_upstream policy")
	}
	for prior, want := range map[int]time.Duration{
		-1: 30 * time.Second,
		0:  30 * time.Second,
		1:  time.Minute,
		2:  2 * time.Minute,
		3:  5 * time.Minute,
		4:  10 * time.Minute,
		26: 10 * time.Minute,
	} {
		if got := upstream.Delay(prior); got != want {
			t.Errorf("upstream Delay(%d) = %v, want %v", prior, got, want)
		}
	}
	shutdown, _ := HandBackPolicyFor(HandBackShutdown)
	if got := shutdown.Delay(3); got != 0 {
		t.Errorf("a policy with no schedule waits %v, want 0", got)
	}
}

// TestHandBackPolicies_CoverEveryHandBackOutcomeTheDialectsName reads both
// dialects' store source for every hand-back outcome they spell — a requeued
// variant or reaped, quoted as SQL or as Go — and requires each to be a row of
// HandBackPolicies. An outcome a writer releases with that the table does not
// declare would end the queue episode it should have kept open, and spend no
// budget.
func TestHandBackPolicies_CoverEveryHandBackOutcomeTheDialectsName(t *testing.T) {
	outcome := regexp.MustCompile(`['"]((?:requeued[a-z_]*)|reaped)['"]`)
	found := map[string][]string{}
	for _, dir := range []string{"postgres", "sqlite"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range outcome.FindAllStringSubmatch(string(src), -1) {
				found[m[1]] = append(found[m[1]], file)
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("found no hand-back outcome in either dialect; the scan is reading the wrong files")
	}
	var names []string
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := HandBackPolicyFor(name); !ok {
			t.Errorf("%q is named in %v but is not in HandBackPolicies", name, found[name])
		}
	}
}
