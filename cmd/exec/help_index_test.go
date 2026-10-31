package exec

import (
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/convident"
	jiraexec "github.com/sky-ai-eng/triage-factory/cmd/exec/jira"
	linearexec "github.com/sky-ai-eng/triage-factory/cmd/exec/linear"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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

// TestHelpText_UnresolvedSourcesListEverything pins the fallback direction: a
// nil manifest (no answer in scope) renders the full surface — registered
// families included, no notes. Over-inclusion is the safe degrade;
// under-advertising to an entitled run is the trial-and-error failure this
// help exists to prevent.
func TestHelpText_UnresolvedSourcesListEverything(t *testing.T) {
	registerHelpIndexFakes(t)
	out := helpText("tfac", nil)

	for _, want := range []string{"Fake Source Commands:", "Fake Plain Commands:", jiraexec.HelpText, linearexec.HelpText} {
		if !strings.Contains(out, want) {
			t.Errorf("full listing is missing %q", want)
		}
	}
	if strings.Contains(out, "NOTE:") {
		t.Error("full listing carries a note; unresolved must render unannotated")
	}
}

// TestHelpText_NotesFollowTheManifest pins the per-state policy: Jira and
// Linear — core, never unlicensed — stay listed with the refusal table's note
// for their state, a registered family whose source is unlicensed or absent
// is omitted, one in any other state is listed with its note, and a
// sourceless family is always listed.
func TestHelpText_NotesFollowTheManifest(t *testing.T) {
	registerHelpIndexFakes(t)
	sources := domain.SourceManifest{
		"github": {State: domain.SourceSealed},
		"jira":   {State: domain.SourceDisabled},
		"linear": {State: domain.SourceUnresolved, Reason: domain.SourceReasonTokenEndpointUnavailable},
	}
	out := helpText("tfac", sources)

	if strings.Contains(out, "Fake Source Commands:") {
		t.Error("a registered family absent from the manifest must be omitted from the index")
	}
	if !strings.Contains(out, "Fake Plain Commands:") {
		t.Error("a sourceless registered family must always be listed")
	}
	for kind, section := range map[string]string{"jira": jiraexec.HelpText, "linear": linearexec.HelpText} {
		if !strings.Contains(out, section) {
			t.Errorf("the %s section must stay listed — explained, not hidden", kind)
		}
		if note := agenthost.SourceNote(kind, sources[kind]); !strings.Contains(out, "NOTE: "+note) {
			t.Errorf("the %s section is missing its note %q", kind, note)
		}
	}
	if n := strings.Count(out, "NOTE: "); n != 2 {
		t.Errorf("the index carries %d notes, want one each for jira and linear and none for a sealed GitHub", n)
	}

	sources["fakesrc"] = domain.SourceEntry{State: domain.SourceUnlicensed}
	if strings.Contains(helpText("tfac", sources), "Fake Source Commands:") {
		t.Error("a registered family whose source is unlicensed must be omitted")
	}
	sources["fakesrc"] = domain.SourceEntry{State: domain.SourceUnconfigured}
	out = helpText("tfac", sources)
	if !strings.Contains(out, "Fake Source Commands:") || !strings.Contains(out, "NOTE: "+agenthost.SourceNote("fakesrc", sources["fakesrc"])) {
		t.Error("a registered family whose source is unconfigured must be listed with its note")
	}
}

// TestHelpText_SealedSourcesListTheirFamilies is the positive arm: a manifest
// sealing every source renders their sections with no notes, byte-identical to
// the unresolved listing.
func TestHelpText_SealedSourcesListTheirFamilies(t *testing.T) {
	registerHelpIndexFakes(t)
	sealed := domain.SourceEntry{State: domain.SourceSealed}
	out := helpText("tfac", domain.SourceManifest{"github": sealed, "jira": sealed, "linear": sealed, "fakesrc": sealed})

	for _, want := range []string{"Fake Source Commands:", "Fake Plain Commands:", jiraexec.HelpText, linearexec.HelpText} {
		if !strings.Contains(out, want) {
			t.Errorf("resolved listing is missing %q", want)
		}
	}
	if out != helpText("tfac", nil) {
		t.Error("an all-sealed manifest must render the same bytes as the unresolved full listing")
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

// TestHostHelpSources_NoRunIdentityMeansNoStateOpen pins help's no-state
// property on the host CLI: an operator's bare terminal (no conversation env)
// gets the full listing without the DB ever being opened, and a failing open
// under run identity degrades to the full listing rather than an error.
func TestHostHelpSources_NoRunIdentityMeansNoStateOpen(t *testing.T) {
	t.Run("no identity", func(t *testing.T) {
		t.Setenv(convident.ConversationIDEnvVar, "")
		opened := stubLocalStores(t)
		if sources := hostHelpSources(); sources != nil {
			t.Errorf("sources = %v, want nil (full listing) with no run identity", sources)
		}
		if *opened {
			t.Error("hostHelpSources opened local state with no run identity in scope")
		}
	})

	t.Run("open failure degrades to full listing", func(t *testing.T) {
		t.Setenv(convident.ConversationIDEnvVar, "conversation-under-test")
		saved := openLocalStores
		openLocalStores = func() (db.Stores, func() error, error) {
			return db.Stores{}, nil, errors.New("fake open failure")
		}
		t.Cleanup(func() { openLocalStores = saved })
		if sources := hostHelpSources(); sources != nil {
			t.Errorf("sources = %v, want nil (full listing) when the open fails", sources)
		}
	})
}
