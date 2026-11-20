package delegate

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// partsFor is the parts a native claim composes for a run whose sources are
// kinds, with nothing else in its run context.
func partsFor(kinds []string) *domain.SystemParts {
	return systemComposition{
		spec:       nativeSpec(agentprompt.SurfaceMachinist),
		mission:    "review the pull request",
		runContext: runContextLines("", "", "", "", "", "", "tfac/<ticket-id>", "", "", ""),
		tools:      sourceDocs{kinds: kinds}.toolParts(agentprompt.RuntimeNative),
	}.parts()
}

// TestSystemUpdate_ASourceTurnedOnArrivesWithItsWholeReference: a source
// whose verbs became available arrives with its full reference, and so do the
// workspace verbs the first tracker brings.
func TestSystemUpdate_ASourceTurnedOnArrivesWithItsWholeReference(t *testing.T) {
	update := systemUpdateText(partsFor([]string{"github"}), partsFor([]string{"github", "jira"}))
	for _, p := range agentprompt.ToolsReferenceParts(agentprompt.RuntimeNative, []string{"jira"}) {
		if want := updatePart(conversationPartName(partToolsPrefix+p.Key), strings.TrimSpace(p.Text), false); !strings.Contains(update, want) {
			t.Errorf("the update does not give the %s verbs in full;\n%s", p.Key, update)
		}
	}
	if n := strings.Count(update, "<part "); n != 2 {
		t.Errorf("the update gives %d parts, want the Jira and workspace verbs;\n%s", n, update)
	}
}

// TestSystemUpdate_ASourceTurnedOffIsOneLine: a source whose verbs went away
// is withdrawn in one line naming it, with none of its old reference, and the
// workspace verbs go with the last tracker.
func TestSystemUpdate_ASourceTurnedOffIsOneLine(t *testing.T) {
	update := systemUpdateText(partsFor([]string{"github", "jira"}), partsFor([]string{"github"}))
	for _, key := range []string{partToolsPrefix + "jira", partToolsPrefix + agentprompt.ToolsWorkspaceKey} {
		if want := goneLine(key, conversationPartName(key)); !strings.Contains(update, want) {
			t.Errorf("the update is missing %q;\n%s", want, update)
		}
	}
	if strings.Contains(update, "<part ") {
		t.Errorf("the update gives a part's text for a source that went away;\n%s", update)
	}
	for _, p := range agentprompt.ToolsReferenceParts(agentprompt.RuntimeNative, []string{"jira"}) {
		if line := firstLine(p.Text); strings.Contains(update, line) {
			t.Errorf("the update carries the withdrawn %s reference (%q)", p.Key, line)
		}
	}
}

// TestSystemUpdate_NothingChangedIsNoUpdate.
func TestSystemUpdate_NothingChangedIsNoUpdate(t *testing.T) {
	if got := systemUpdateText(partsFor([]string{"github", "jira"}), partsFor([]string{"jira", "github"})); got != "" {
		t.Errorf("the same parts produced an update;\n%s", got)
	}
}

// TestSystemUpdate_ALineThatNoLongerAppliesIsOneLine: a task reopened since
// the last claim has no task-state line, and the update says so in one line.
func TestSystemUpdate_ALineThatNoLongerAppliesIsOneLine(t *testing.T) {
	closed := &domain.SystemParts{Conversation: runContextLines("", "This task is closed.", "", "", "", "", "", "", "", "")}
	update := systemUpdateText(closed, &domain.SystemParts{})
	if !strings.Contains(update, goneLine(partTaskState, conversationPartName(partTaskState))) || strings.Contains(update, "This task is closed.") {
		t.Errorf("update =\n%s", update)
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// TestSystemComposition_ATextPostgresCannotStoreIsMadeStorable: a knowledge
// line or a file name in a run tree an agent owns can carry NUL or invalid
// UTF-8, which Postgres refuses in the saved blocks and parts alike, so a
// native compaction's commit would fail on every claim. The composition makes
// them storable, and the block it sends renders the parts it saves.
func TestSystemComposition_ATextPostgresCannotStoreIsMadeStorable(t *testing.T) {
	knowledge := "Team knowledge staged for this run.\n  notes\xff.md — H\x00e\x00l\x00l\x00o"
	c := systemComposition{
		spec:       nativeSpec(agentprompt.SurfaceMachinist),
		mission:    "review the pull request\x00",
		runContext: runContextLines("", "", "", "", "", "", "tfac/<ticket-id>", "", "", knowledge),
		tools:      sourceDocs{kinds: []string{"github"}}.toolParts(agentprompt.RuntimeNative),
	}
	blocks := c.blocks()

	storable := func(name, s string) {
		t.Helper()
		if strings.ContainsRune(s, 0) || !utf8.ValidString(s) {
			t.Errorf("%s holds a NUL or invalid UTF-8: %q", name, s)
		}
	}
	storable("block 2", blocks.Block)
	var listing string
	for _, p := range blocks.Parts.Conversation {
		storable(p.Key, p.Text)
		if p.Key == partKnowledge {
			listing = p.Text
		}
	}
	if listing != "Team knowledge staged for this run.\n  notes�.md — Hello" {
		t.Errorf("knowledge part = %q, want the listing with the NULs removed and the invalid byte replaced", listing)
	}
	if !strings.Contains(blocks.Block, listing) {
		t.Error("block 2 does not render the knowledge part it saves")
	}
	if !reflect.DeepEqual(blocks.Parts, c.parts()) {
		t.Error("the parts saved with the blocks differ from the parts a later claim compares")
	}
}
