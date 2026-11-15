package agentprompt

import (
	"strings"
	"testing"
)

// The parts are what a conversation compares claim to claim, and the joined
// composition is what it was sent. The two have to be the same text.
func TestBuildParts_JoinToBuild(t *testing.T) {
	for _, spec := range allSpecs() {
		parts := BuildParts(spec)
		texts := make([]string, len(parts))
		keys := map[string]bool{}
		for i, p := range parts {
			texts[i] = p.Text
			if keys[p.Key] {
				t.Errorf("%+v: key %q appears twice", spec, p.Key)
			}
			keys[p.Key] = true
			if p.Name == "" {
				t.Errorf("%+v: part %q has no name", spec, p.Key)
			}
		}
		if got, want := strings.Join(texts, "\n\n")+"\n", Build(spec); got != want {
			t.Errorf("%+v: joined parts differ from Build", spec)
		}
	}
}

func TestToolsReferenceParts_JoinToTheReference(t *testing.T) {
	ResetToolsReferences()
	t.Cleanup(ResetToolsReferences)
	RegisterToolsReference("slack", "Slack interactions use `triagefactory exec slack`.\n")
	for _, runtime := range []Runtime{RuntimeSDK, RuntimeNative} {
		for _, kinds := range [][]string{nil, {"github"}, {"jira"}, {"linear", "jira", "github"}, {"slack", "jira"}} {
			parts := ToolsReferenceParts(runtime, kinds)
			texts := make([]string, len(parts))
			for i, p := range parts {
				texts[i] = p.Text
			}
			if got, want := strings.Join(texts, "\n\n"), ToolsReferenceForSources(runtime, kinds); got != want {
				t.Errorf("%s %v: joined parts differ from the reference", runtime, kinds)
			}
		}
	}
}

func TestOpeningTag(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"<team_knowledge>\nbody", "team_knowledge"},
		{"<autonomous-coding-practices>\nbody", "autonomous-coding-practices"},
		{"# Heading\nbody", "x/y.txt"},
		{"<a b>\nbody", "x/y.txt"},
		{"</closing>\nbody", "x/y.txt"},
	} {
		if got := openingTag(tc.text, "x/y.txt"); got != tc.want {
			t.Errorf("openingTag(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestInstructionsUpdate_Composition(t *testing.T) {
	if got := InstructionsUpdate(nil, nil); got != "" {
		t.Fatalf("an empty update = %q, want nothing", got)
	}
	got := InstructionsUpdate(
		[]Part{{Key: "a", Name: "first", Text: "new text\n"}, {Key: "b", Name: "second", Text: "more"}},
		[]Part{{Key: "c", Name: "the Linear verbs"}},
	)
	if !strings.HasPrefix(got, "<instructions_update>\n") || !strings.HasSuffix(got, "\n</instructions_update>") {
		t.Fatalf("update is not enclosed in its tag:\n%s", got)
	}
	first := strings.Index(got, "<part name=\"first\">\nnew text\n</part>")
	second := strings.Index(got, "<part name=\"second\">\nmore\n</part>")
	withdrawn := strings.Index(got, "the Linear verbs")
	if first < 0 || second < first || withdrawn < second {
		t.Fatalf("update does not carry each changed part in order, then each withdrawal:\n%s", got)
	}
	if strings.Count(got[withdrawn:], "\n") != 1 {
		t.Fatalf("a withdrawal takes more than its own line:\n%s", got)
	}
}
