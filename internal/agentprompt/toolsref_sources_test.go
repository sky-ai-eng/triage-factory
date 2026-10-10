package agentprompt_test

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
)

// jiraVerb / linearVerb / ghVerb are strings only their own family's docs
// contain, so a test can tell which references made it into a composition.
const (
	jiraVerb   = "exec jira"
	linearVerb = "exec linear"
	ghVerb     = "exec gh"
)

// TestToolsReferenceForSources_IsAboutTheOrgNotTheTrigger is the property the
// whole helper exists for: a run working a GitHub pull request in an org that
// has Jira gets the Jira verbs, because the PR may reference a ticket and the
// entity that triggered the run says nothing about what the agent will need.
func TestToolsReferenceForSources_IsAboutTheOrgNotTheTrigger(t *testing.T) {
	got := agentprompt.ToolsReferenceForSources([]string{"github", "jira"})

	if !strings.Contains(got, ghVerb) {
		t.Error("github verbs missing")
	}
	if !strings.Contains(got, jiraVerb) {
		t.Error("jira verbs missing from a composition that named jira — a PR run could not touch the ticket it references")
	}
}

// TestToolsReferenceForSources_OmitsWhatTheOrgCannotReach: the other half.
// Documenting Jira to an org with no Jira invites a wasted turn on a verb that
// cannot resolve a credential.
func TestToolsReferenceForSources_OmitsWhatTheOrgCannotReach(t *testing.T) {
	got := agentprompt.ToolsReferenceForSources([]string{"github"})

	if !strings.Contains(got, ghVerb) {
		t.Error("github verbs missing")
	}
	if strings.Contains(got, jiraVerb) {
		t.Error("jira verbs present for an org that cannot reach jira")
	}
	if strings.Contains(got, linearVerb) {
		t.Error("linear verbs present for an org that cannot reach linear")
	}
	if strings.Contains(got, "exec workspace") {
		t.Error("workspace materialization present in a set that names no issue tracker")
	}
}

// TestToolsReferenceForSources_LinearRunKeepsGitHub: a run on a Linear issue
// composes its own source with GitHub, and both families must survive — the
// agent materializes a checkout and then needs the gh verbs to open a pull
// request from it.
func TestToolsReferenceForSources_LinearRunKeepsGitHub(t *testing.T) {
	got := agentprompt.ToolsReferenceForSources([]string{"linear", "github"})

	if !strings.Contains(got, ghVerb) {
		t.Error("github verbs missing from a Linear run's composition")
	}
	if !strings.Contains(got, linearVerb) {
		t.Error("linear verbs missing from a composition that named linear")
	}
	if !strings.Contains(got, "exec workspace") {
		t.Error("workspace materialization missing from a Linear run, which starts with no checkout")
	}
	if strings.Contains(got, jiraVerb) {
		t.Error("jira verbs present in a set that did not name jira")
	}
	if strings.Index(got, ghVerb) > strings.Index(got, linearVerb) {
		t.Error("github does not lead; the canonical order is core-first, github first")
	}
}

// TestToolsReferenceForSources_IsCanonicalAndDeduplicated pins the ordering
// contract. Callers union a resolved set with the run's own source, so the same
// set arrives in different orders and with repeats — and two runs composing the
// same set must produce the same bytes, or a cached prefix stops being cached.
func TestToolsReferenceForSources_IsCanonicalAndDeduplicated(t *testing.T) {
	a := agentprompt.ToolsReferenceForSources([]string{"jira", "github", "jira"})
	b := agentprompt.ToolsReferenceForSources([]string{"github", "jira"})

	if a != b {
		t.Error("composition depends on the caller's order or repeats; a union of the same set must be one prefix")
	}
	if strings.Count(a, jiraVerb) != strings.Count(b, jiraVerb) {
		t.Error("a repeated source was emitted twice")
	}
	if strings.Index(a, ghVerb) > strings.Index(a, jiraVerb) {
		t.Error("github does not lead; the canonical order is core-first")
	}

	want := agentprompt.ToolsReferenceForSources([]string{"github", "jira", "linear"})
	for _, kinds := range [][]string{
		{"linear", "jira", "github"},
		{"jira", "linear", "github", "linear"},
		{"github", "linear", "", "jira", "github"},
	} {
		if got := agentprompt.ToolsReferenceForSources(kinds); got != want {
			t.Errorf("ToolsReferenceForSources(%q) differs from the canonical github, jira, linear composition", kinds)
		}
	}
	gh, jira, linear := strings.Index(want, ghVerb), strings.Index(want, jiraVerb), strings.Index(want, linearVerb)
	if gh >= jira || jira >= linear {
		t.Error("core order is not github, jira, linear")
	}
}

// TestToolsReferenceForSources_SkipsUnknownKinds: an unregistered source
// contributes nothing rather than an empty section or a panic. A kind can
// legitimately be outside this build (an ee source in a core-only deployment),
// and a run must not fail over a name it cannot document.
func TestToolsReferenceForSources_SkipsUnknownKinds(t *testing.T) {
	got := agentprompt.ToolsReferenceForSources([]string{"github", "gerrit", "", "phabricator"})

	if !strings.Contains(got, ghVerb) {
		t.Error("github verbs missing")
	}
	if strings.Contains(got, "gerrit") || strings.Contains(got, "phabricator") {
		t.Error("an unregistered kind reached the output")
	}
	if got != agentprompt.ToolsReferenceForSources([]string{"github"}) {
		t.Error("a skipped kind changed the composition, e.g. with a trailing separator")
	}
}

// TestToolsReferenceForSources_EmptyIsEmpty, so a caller that resolved nothing
// renders an empty <tools> section rather than a stray separator.
func TestToolsReferenceForSources_EmptyIsEmpty(t *testing.T) {
	if got := agentprompt.ToolsReferenceForSources(nil); got != "" {
		t.Errorf("ToolsReferenceForSources(nil) = %q, want empty", got)
	}
}
