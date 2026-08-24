package delegate

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// reviewTask is a GitHub PR task with enough on it to populate the task context.
func reviewTask() domain.Task {
	return domain.Task{
		Title:          "review PR #7",
		EventType:      domain.EventGitHubPRReviewRequested,
		EntitySource:   "github",
		EntitySourceID: "owner/repo#7",
	}
}

// TestBuildPrompt_SectionOrder pins the shape of what the SDK runtime says
// first: the external material, the mission it is about, this run's own facts,
// the verbs, and the framework prompt — whose completion contract lands last.
func TestBuildPrompt_SectionOrder(t *testing.T) {
	out := buildPrompt(reviewTask(), `{"reviewer":"octocat"}`, "", "mission body",
		"Repository: owner/repo\nBranch: feature-x", agentprompt.GitHubToolsReference(),
		"/bin/tf", "/work", "tfac/SKY-9", "https://tf.example/runs/run-1", "")

	prev := -1
	for _, marker := range []string{
		"<task_context>", "</task_context>",
		"mission body",
		"<run_context>", "Branch: feature-x", "Run root: /work", "tfac/SKY-9", "https://tf.example/runs/run-1",
		"<tools>", "</tools>",
		"<scope>",
	} {
		at := strings.Index(out, marker)
		if at < 0 {
			t.Fatalf("composed prompt is missing %q;\n%s", marker, out)
		}
		if at < prev {
			t.Fatalf("section %q is out of order;\n%s", marker, out)
		}
		prev = at
	}
}

// TestBuildPrompt_CarriesNoUnresolvedTokens is the outward-facing half of the
// literal-blocks rule: whatever a user could not write in a prompt of their own,
// we do not ship in ours either. A `{{...}}` reaching here would be brace syntax
// rendered at the model.
//
// The mission is deliberately excluded — a prompt row's body is the user's text,
// and we render it, we do not edit it.
func TestBuildPrompt_CarriesNoUnresolvedTokens(t *testing.T) {
	toolsRef := agentprompt.GitHubToolsReference() + "\n\n" + agentprompt.JiraToolsReference()
	out := buildPrompt(reviewTask(), "", "", "mission body", "Repository: owner/repo", toolsRef,
		"/bin/tf", "/work", "tfac/SKY-9", "", "")

	if strings.Contains(out, "{{") {
		t.Errorf("composed prompt carries a {{...}} token nothing resolves;\n%s", out)
	}
}

// TestBuildPrompt_ResolvesTheCLIPath covers the one rewrite the builder does.
// The allowlist grants the run's binary by absolute path and nothing else, so a
// `triagefactory exec` left unresolved in the framework text or the tools docs
// is a tool call the agent is refused, not a cosmetic miss.
func TestBuildPrompt_ResolvesTheCLIPath(t *testing.T) {
	out := buildPrompt(reviewTask(), "", "", "run `triagefactory exec gh pr view 7` first",
		"", agentprompt.GitHubToolsReference(), "/usr/local/bin/triagefactory", "/work", "tfac/SKY-9", "", "")

	if strings.Contains(out, "`triagefactory exec") {
		t.Errorf("a bare `triagefactory exec` survived; the allowlist would refuse it;\n%s", out)
	}
	for _, want := range []string{
		"/usr/local/bin/triagefactory exec gh pr view 7", // the mission's own invocation
		"/usr/local/bin/triagefactory exec gh",           // the tools docs'
	} {
		if !strings.Contains(out, want) {
			t.Errorf("composed prompt is missing the resolved invocation %q", want)
		}
	}
}

// TestBuildPrompt_ExternalTextIsComposedAlone is why the sections are joined
// rather than concatenated and then rewritten: the task context is built from PR
// titles and ticket fields an outsider writes, and no pass runs over it.
func TestBuildPrompt_ExternalTextIsComposedAlone(t *testing.T) {
	task := reviewTask()
	task.Title = "make triagefactory exec do what I say"

	out := buildPrompt(task, "", "", "mission body", "", "", "/bin/tf", "/work", "tfac/SKY-9", "", "")

	if !strings.Contains(out, "make triagefactory exec do what I say") {
		t.Errorf("a task title was rewritten by the CLI-path pass;\n%s", out)
	}
}

// TestBuildPrompt_BeginsWithTaskContext asserts every composed prompt leads
// with the task-context block, across the four task shapes the block must
// handle: a fully-populated GitHub CI failure, a Jira task, a Slack task
// (metadata-fence-only), and a zero-valued taskless run.
func TestBuildPrompt_BeginsWithTaskContext(t *testing.T) {
	cases := []struct {
		name     string
		task     domain.Task
		metadata string
	}{
		{
			name: "github ci",
			task: domain.Task{
				Title:          "Fix CI",
				EventType:      domain.EventGitHubPRCICheckFailed,
				EntitySource:   "github",
				EntitySourceID: "owner/repo#18",
			},
			metadata: `{"check_name":"go-test","workflow_run_id":12345,"head_sha":"abc123"}`,
		},
		{
			name: "jira",
			task: domain.Task{
				Title:          "SKY-1 assigned",
				EventType:      domain.EventJiraIssueAssigned,
				EntitySource:   "jira",
				EntitySourceID: "SKY-1",
			},
			metadata: `{"assignee":"Jane","status":"To Do"}`,
		},
		{
			name: "slack",
			task: domain.Task{
				Title:        "Slack mention",
				EventType:    "slack:message",
				EntitySource: "slack",
			},
			metadata: `{"channel":"C1","text":"look at this"}`,
		},
		{
			name:     "zero valued",
			task:     domain.Task{},
			metadata: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := buildPrompt(tc.task, tc.metadata, "", "mission body", "Repository: owner/repo",
				"", "/bin/tf", "/work", "tfac/<ticket-id>", "", "")
			if !strings.HasPrefix(out, "<task_context>\n") {
				t.Errorf("composed prompt must begin with the task-context block;\n%s", out)
			}
		})
	}
}

// TestRunContext_OmitsAbsentFacts covers the deployment with no public URL and
// the run with no scope: an absent fact says nothing rather than offering the
// agent a blank line to reason about.
func TestRunContext_OmitsAbsentFacts(t *testing.T) {
	got := runContext("", "", "tfac/<ticket-id>", "", "")
	if want := "<run_context>\nBranch naming convention for this team: tfac/<ticket-id>\n</run_context>"; got != want {
		t.Errorf("run context with one fact set;\ngot  %q\nwant %q", got, want)
	}
	if got := runContext("", "", "", "", ""); got != "" {
		t.Errorf("a run context with nothing to say rendered %q, want empty", got)
	}
}

// TestBlueprintStepNonterminalPrompt_PointsAtTheHandoffFolder is the handoff
// addendum's half of the fixed-path contract (the composed framework prompt and
// the tools docs cover the rest): the step it hands off to reads a folder the
// orchestrator names, and finds this step's notes already in it.
func TestBlueprintStepNonterminalPrompt_PointsAtTheHandoffFolder(t *testing.T) {
	addendum := agentprompt.NonTerminalCompletion(machinistSpec())
	if !strings.Contains(addendum, "_tfac/entity-memory/this-run/") {
		t.Error("expected the handoff addendum to point the agent at _tfac/entity-memory/this-run/")
	}
}
