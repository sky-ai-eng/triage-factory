package agentprompt

import (
	"strings"
	"testing"
)

func TestToolsReferenceFor_Unregistered(t *testing.T) {
	if _, ok := ToolsReferenceFor("nope"); ok {
		t.Error("expected ok=false for an unregistered source")
	}
}

func TestRegisterToolsReference_RegisterAndLookup(t *testing.T) {
	t.Cleanup(ResetToolsReferences)
	RegisterToolsReference("fake", "fake verb docs")

	got, ok := ToolsReferenceFor("fake")
	if !ok || got != "fake verb docs" {
		t.Errorf("ToolsReferenceFor(fake) = (%q, %v), want (%q, true)", got, ok, "fake verb docs")
	}
}

func TestRegisterToolsReference_PanicsOnEmptySource(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for an empty source")
		}
	}()
	RegisterToolsReference("", "text")
}

func TestRegisterToolsReference_PanicsOnEmptyText(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for empty text")
		}
	}()
	RegisterToolsReference("fake-empty-text", "")
}

func TestRegisterToolsReference_PanicsOnCoreSource(t *testing.T) {
	for _, source := range []string{"github", "jira", "linear"} {
		t.Run(source, func(t *testing.T) {
			t.Cleanup(ResetToolsReferences)
			defer func() {
				if recover() == nil {
					t.Errorf("expected panic when registering core source %q", source)
				}
			}()
			RegisterToolsReference(source, "text")
		})
	}
}

// TestToolsReferenceForSources_WorkspaceOncePerTrackerSet pins when the shared
// workspace block appears: once whenever the set names an issue tracker, never
// otherwise. A set naming both trackers that carried it twice would hand the
// agent the same instructions back to back; a GitHub-only set that carried it
// would describe a starting state a PR run does not have.
func TestToolsReferenceForSources_WorkspaceOncePerTrackerSet(t *testing.T) {
	t.Cleanup(ResetToolsReferences)
	RegisterToolsReference("fake", "FAKE VERB DOCS")

	workspace := block(blockToolsWorkspace)
	for _, tc := range []struct {
		kinds []string
		want  int
	}{
		{[]string{"jira"}, 1},
		{[]string{"linear"}, 1},
		{[]string{"github", "jira"}, 1},
		{[]string{"github", "linear"}, 1},
		{[]string{"jira", "linear"}, 1},
		{[]string{"linear", "fake", "github", "jira", "linear"}, 1},
		{[]string{"github"}, 0},
		{[]string{"fake"}, 0},
		{[]string{"github", "fake"}, 0},
	} {
		got := ToolsReferenceForSources(tc.kinds)
		if n := strings.Count(got, workspace); n != tc.want {
			t.Errorf("ToolsReferenceForSources(%q) carries the workspace block %d times, want %d", tc.kinds, n, tc.want)
		}
	}
}

// TestToolsReferenceForSources_WorkspaceClosesTheCoreGroup pins where the
// workspace block sits: after every tracker block and before every registered
// source, so a registered source never changes the bytes of the core portion.
func TestToolsReferenceForSources_WorkspaceClosesTheCoreGroup(t *testing.T) {
	t.Cleanup(ResetToolsReferences)
	RegisterToolsReference("fake", "FAKE VERB DOCS")

	with := ToolsReferenceForSources([]string{"fake", "linear", "github", "jira"})
	without := ToolsReferenceForSources([]string{"linear", "github", "jira"})

	prev := -1
	for _, section := range []string{githubTools, jiraTools, linearTools, block(blockToolsWorkspace), "FAKE VERB DOCS"} {
		at := strings.Index(with, section)
		if at < 0 {
			t.Fatalf("composition is missing a section: %.40q", section)
		}
		if at < prev {
			t.Fatalf("section %.40q is out of the canonical order", section)
		}
		prev = at
	}
	if !strings.HasPrefix(with, without+"\n\n") {
		t.Error("a registered source changed the bytes of the core portion of the composition")
	}
}

// TestTrackerReferences_MatchTheComposition holds each accessor to the
// composer: a caller reading JiraToolsReference must get exactly what a run
// whose set names Jira alone is told, workspace block included.
func TestTrackerReferences_MatchTheComposition(t *testing.T) {
	for _, tc := range []struct {
		source string
		got    string
	}{
		{"github", GitHubToolsReference()},
		{"jira", JiraToolsReference()},
		{"linear", LinearToolsReference()},
	} {
		if want := ToolsReferenceForSources([]string{tc.source}) + "\n"; tc.got != want {
			t.Errorf("the %s accessor and ToolsReferenceForSources(%q) disagree", tc.source, tc.source)
		}
	}
}

// TestLinearBlock_DocumentsEveryVerb keeps the Linear block in step with the
// verbs `exec linear issue` serves. A verb the CLI has and the prompt omits is
// one the agent improvises around instead of calling.
func TestLinearBlock_DocumentsEveryVerb(t *testing.T) {
	body := block(blockToolsLinear)
	for _, verb := range []string{
		"view", "list-states", "transition", "comment", "assign", "unassign",
		"create", "edit", "set-parent", "set-priority", "list-children", "search",
	} {
		if !strings.Contains(body, "triagefactory exec linear issue "+verb) {
			t.Errorf("the Linear tools block does not document `issue %s`", verb)
		}
	}
}

func TestRegisterToolsReference_PanicsOnDuplicate(t *testing.T) {
	t.Cleanup(ResetToolsReferences)
	RegisterToolsReference("fake-dup", "text")
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate registration")
		}
	}()
	RegisterToolsReference("fake-dup", "text")
}
