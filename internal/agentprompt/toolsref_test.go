package agentprompt

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
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

// TestToolsReferenceForSources_ComposesTheSetsBlocks pins which blocks a source
// set composes and in what order. The order is canonical — core first, the
// workspace block once whenever the set names an issue tracker, then registered
// sources alphabetically — so two callers assembling the same set produce the
// same bytes, and a registered source never changes the bytes of the core
// portion. A native run leaves GitHub to its framework blocks and spells verbs
// with the applet.
func TestToolsReferenceForSources_ComposesTheSetsBlocks(t *testing.T) {
	t.Cleanup(ResetToolsReferences)
	RegisterToolsReference("fake", "FAKE VERB DOCS")
	RegisterToolsReference("zz", "ZZ VERB DOCS")

	join := func(parts ...string) string { return strings.Join(parts, "\n\n") }
	applet := func(s string) string { return strings.ReplaceAll(s, "triagefactory exec", prog.AppletName) }

	for _, tc := range []struct {
		name    string
		runtime Runtime
		kinds   []string
		want    string
	}{
		{"github alone", RuntimeSDK, []string{"github"}, githubTools},
		{"jira", RuntimeSDK, []string{"jira"}, join(jiraTools, workspaceTools)},
		{"linear", RuntimeSDK, []string{"linear"}, join(linearTools, workspaceTools)},
		{"github and a tracker", RuntimeSDK, []string{"jira", "github"}, join(githubTools, jiraTools, workspaceTools)},
		{"both trackers share one workspace block", RuntimeSDK, []string{"linear", "jira"}, join(jiraTools, linearTools, workspaceTools)},
		{"registered sources follow the core group", RuntimeSDK, []string{"zz", "fake", "linear", "github", "jira"},
			join(githubTools, jiraTools, linearTools, workspaceTools, "FAKE VERB DOCS", "ZZ VERB DOCS")},
		{"registered sources bring no workspace block", RuntimeSDK, []string{"github", "fake"}, join(githubTools, "FAKE VERB DOCS")},
		{"repeats and blanks collapse", RuntimeSDK, []string{"jira", "", "github", "jira"}, join(githubTools, jiraTools, workspaceTools)},
		{"unknown kinds contribute nothing", RuntimeSDK, []string{"github", "gerrit"}, githubTools},
		{"empty set", RuntimeSDK, nil, ""},
		{"native github alone", RuntimeNative, []string{"github"}, ""},
		{"native", RuntimeNative, []string{"github", "jira", "fake"}, applet(join(jiraTools, workspaceTools, "FAKE VERB DOCS"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToolsReferenceForSources(tc.runtime, tc.kinds); got != tc.want {
				t.Errorf("ToolsReferenceForSources(%s, %q) composed the wrong blocks;\ngot  %.300q\nwant %.300q", tc.runtime, tc.kinds, got, tc.want)
			}
		})
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
		if want := ToolsReferenceForSources(RuntimeSDK, []string{tc.source}) + "\n"; tc.got != want {
			t.Errorf("the %s accessor and ToolsReferenceForSources(%q) disagree", tc.source, tc.source)
		}
	}
}
