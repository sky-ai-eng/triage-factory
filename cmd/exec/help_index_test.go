package exec

import (
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/convident"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// registerHelpIndexFakes seeds the registry with the two shapes the index
// filter distinguishes: a family bound to an event source, and a sourceless
// one (the workspace/memory shape).
func registerHelpIndexFakes(t *testing.T) {
	t.Helper()
	t.Cleanup(ResetSubcommands)
	RegisterSubcommand("fake-src", Subcommand{
		Run:        noopSubcommandRunner,
		HelpText:   "Fake Source Commands:\n  fake-src do <thing>",
		SourceKind: "fakesrc",
	})
	RegisterSubcommand("fake-plain", Subcommand{
		Run:      noopSubcommandRunner,
		HelpText: "Fake Plain Commands:\n  fake-plain do <thing>",
	})
}

// TestHelpText_UnresolvedKindsListEverything pins the fallback direction: a
// nil kind set (no availability answer in scope) renders the full surface —
// registered families included, no availability notes. Over-inclusion is the
// safe degrade; under-advertising to an entitled run is the trial-and-error
// failure this help exists to prevent.
func TestHelpText_UnresolvedKindsListEverything(t *testing.T) {
	registerHelpIndexFakes(t)
	out := helpText("tfac", nil)

	for _, want := range []string{"Fake Source Commands:", "Fake Plain Commands:", "Jira Ticket Commands:"} {
		if !strings.Contains(out, want) {
			t.Errorf("full listing is missing %q", want)
		}
	}
	if strings.Contains(out, "not currently available") {
		t.Error("full listing carries an availability note; unresolved must render unannotated")
	}
}

// TestHelpText_FilteredIndex pins the per-state policy: a registered family
// whose source is absent from the resolved set is omitted (it may be
// unlicensed — absence is the rule for unlicensed surfaces everywhere else),
// a sourceless registered family is always listed, and Jira — core, never
// unlicensed — stays listed with the not-currently-available note.
func TestHelpText_FilteredIndex(t *testing.T) {
	registerHelpIndexFakes(t)
	out := helpText("tfac", []string{"github"})

	if strings.Contains(out, "Fake Source Commands:") {
		t.Error("a registered family whose source is unavailable must be omitted from the index")
	}
	if !strings.Contains(out, "Fake Plain Commands:") {
		t.Error("a sourceless registered family must always be listed")
	}
	if !strings.Contains(out, "Jira Ticket Commands:") {
		t.Error("the Jira section must stay listed when jira is unavailable — explained, not hidden")
	}
	if !strings.Contains(out, jiraUnavailableNote) {
		t.Error("the Jira section is missing its not-currently-available note")
	}
}

// TestHelpText_ResolvedKindsListTheirFamilies is the positive arm: a resolved
// set naming the sources renders their sections with no notes, byte-identical
// in the sections it shares with the unresolved listing.
func TestHelpText_ResolvedKindsListTheirFamilies(t *testing.T) {
	registerHelpIndexFakes(t)
	out := helpText("tfac", []string{"github", "jira", "fakesrc"})

	for _, want := range []string{"Fake Source Commands:", "Fake Plain Commands:", "Jira Ticket Commands:"} {
		if !strings.Contains(out, want) {
			t.Errorf("resolved listing is missing %q", want)
		}
	}
	if strings.Contains(out, "not currently available") {
		t.Error("resolved listing carries an availability note for an available source")
	}
	if out != helpText("tfac", nil) {
		t.Error("an all-available resolve must render the same bytes as the unresolved full listing")
	}
}

// TestHelpText_RegisteredSectionsAreNameOrdered pins deterministic output:
// two processes with the same registrations print the same help.
func TestHelpText_RegisteredSectionsAreNameOrdered(t *testing.T) {
	registerHelpIndexFakes(t)
	out := helpText("tfac", nil)
	if strings.Index(out, "Fake Plain Commands:") > strings.Index(out, "Fake Source Commands:") {
		t.Error("registered sections are not in name order (fake-plain before fake-src)")
	}
}

// TestHostHelpKinds_NoRunIdentityMeansNoStateOpen pins help's no-state
// property on the host CLI: an operator's bare terminal (no conversation env)
// gets the full listing without the DB ever being opened, and a failing open
// under run identity degrades to the full listing rather than an error.
func TestHostHelpKinds_NoRunIdentityMeansNoStateOpen(t *testing.T) {
	t.Run("no identity", func(t *testing.T) {
		t.Setenv(convident.ConversationIDEnvVar, "")
		opened := stubLocalStores(t)
		if kinds := hostHelpKinds(); kinds != nil {
			t.Errorf("kinds = %v, want nil (full listing) with no run identity", kinds)
		}
		if *opened {
			t.Error("hostHelpKinds opened local state with no run identity in scope")
		}
	})

	t.Run("open failure degrades to full listing", func(t *testing.T) {
		t.Setenv(convident.ConversationIDEnvVar, "conversation-under-test")
		saved := openLocalStores
		openLocalStores = func() (db.Stores, func() error, error) {
			return db.Stores{}, nil, errors.New("fake open failure")
		}
		t.Cleanup(func() { openLocalStores = saved })
		if kinds := hostHelpKinds(); kinds != nil {
			t.Errorf("kinds = %v, want nil (full listing) when the open fails", kinds)
		}
	})
}
