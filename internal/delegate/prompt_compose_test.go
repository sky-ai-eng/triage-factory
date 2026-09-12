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

// TestSDKSystemPrompt_IsTheTwoBlocks is the parity this runtime is held to: the
// SDK harness takes one appended string where the native engine takes two
// system blocks, so the string has to be those two blocks and nothing else.
func TestSDKSystemPrompt_IsTheTwoBlocks(t *testing.T) {
	const (
		mission     = "mission body"
		toolsRef    = "gh pr view — read a pull request"
		nonTerminal = "<blueprint-nonterminal>hand off</blueprint-nonterminal>"
	)
	runCtx := runContext("Repository: owner/repo", "/work", "tfac/SKY-9", "https://tf.example/runs/run-1", "")

	got := sdkSystemPrompt(mission, runCtx, toolsRef, nonTerminal, "/bin/tf")
	want := resolveCLIPath(
		strings.TrimSpace(agentprompt.Build(machinistSpec()))+"\n\n"+
			strings.TrimSpace(composeConversationSystemBlock(mission, runCtx, toolsRef, nonTerminal)),
		"/bin/tf")
	if got != want {
		t.Errorf("the SDK system prompt is not block 1 followed by block 2;\ngot:\n%s\n\nwant:\n%s", got, want)
	}
	if !strings.HasPrefix(got, frameworkBlocks(t)) {
		t.Error("the framework blocks must lead the append, as block 1 does for the native engine")
	}
}

// frameworkBlocks is block 1 as the composer renders it — what every SDK
// system prompt leads with, and the prefix a test strips to reason about the
// conversation's own block alone. The framework text names <run_context> and
// <tools> in prose, so a bare search over the whole string cannot tell a
// reference from a section.
func frameworkBlocks(t *testing.T) string {
	t.Helper()
	return resolveCLIPath(strings.TrimSpace(agentprompt.Build(machinistSpec())), "/bin/tf")
}

// TestSDKSystemPrompt_SectionOrder pins what the run's own block says and in
// what order: this run's facts and its verbs first, then the mission and how
// this step ends — so the instruction is the last thing the model reads before
// the conversation itself.
func TestSDKSystemPrompt_SectionOrder(t *testing.T) {
	out := sdkSystemPrompt(
		"mission body",
		runContext("Repository: owner/repo\nBranch: feature-x", "/work", "tfac/SKY-9", "https://tf.example/runs/run-1", ""),
		agentprompt.GitHubToolsReference(),
		agentprompt.NonTerminalCompletion(machinistSpec()),
		"/bin/tf",
	)
	block2 := strings.TrimPrefix(out, frameworkBlocks(t))

	prev := -1
	for _, marker := range []string{
		"<run_context>", "Branch: feature-x", "Run root: /work", "tfac/SKY-9", "https://tf.example/runs/run-1", "</run_context>",
		"<tools>", "</tools>",
		"mission body",
		"You are one step inside a multi-step blueprint",
	} {
		at := strings.Index(block2, marker)
		if at < 0 {
			t.Fatalf("the conversation's own block is missing %q;\n%s", marker, block2)
		}
		if at < prev {
			t.Fatalf("section %q is out of order;\n%s", marker, block2)
		}
		prev = at
	}
}

// TestSDKSystemPrompt_OmitsWhatThisRunHasNothingToSay covers the manual
// conversation with no mission, the terminal step with no addendum and the org
// that documented no verbs: an absent section renders nothing rather than an
// empty tag pair.
func TestSDKSystemPrompt_OmitsWhatThisRunHasNothingToSay(t *testing.T) {
	out := sdkSystemPrompt("", runContext("", "", "", "", ""), "", "", "/bin/tf")
	if out != frameworkBlocks(t) {
		t.Errorf("with no per-run facts the append must be the framework blocks alone, with no empty section after them;\n%s",
			strings.TrimPrefix(out, frameworkBlocks(t)))
	}
}

// TestSDKSystemPrompt_CarriesNoUnresolvedTokens is the outward-facing half of
// the literal-blocks rule: whatever a user could not write in a prompt of their
// own, we do not ship in ours either. A `{{...}}` reaching here would be brace
// syntax rendered at the model.
//
// The mission is deliberately excluded — a prompt row's body is the user's
// text, and we render it, we do not edit it.
func TestSDKSystemPrompt_CarriesNoUnresolvedTokens(t *testing.T) {
	toolsRef := agentprompt.GitHubToolsReference() + "\n\n" + agentprompt.JiraToolsReference()
	out := sdkSystemPrompt("mission body", runContext("Repository: owner/repo", "/work", "tfac/SKY-9", "", ""), toolsRef, "", "/bin/tf")

	if strings.Contains(out, "{{") {
		t.Errorf("composed system prompt carries a {{...}} token nothing resolves;\n%s", out)
	}
}

// TestSDKSystemPrompt_ResolvesTheCLIPath covers the one rewrite the composer
// does. The allowlist grants the run's binary by absolute path and nothing
// else, so a `triagefactory exec` left unresolved in the framework text or the
// tools docs is a tool call the agent is refused, not a cosmetic miss.
func TestSDKSystemPrompt_ResolvesTheCLIPath(t *testing.T) {
	out := sdkSystemPrompt("run `triagefactory exec gh pr view 7` first",
		runContext("", "/work", "tfac/SKY-9", "", ""), agentprompt.GitHubToolsReference(), "", "/usr/local/bin/triagefactory")

	if strings.Contains(out, "`triagefactory exec") {
		t.Errorf("a bare `triagefactory exec` survived; the allowlist would refuse it;\n%s", out)
	}
	for _, want := range []string{
		"/usr/local/bin/triagefactory exec gh pr view 7", // the mission's own invocation
		"/usr/local/bin/triagefactory exec gh",           // the tools docs'
	} {
		if !strings.Contains(out, want) {
			t.Errorf("composed system prompt is missing the resolved invocation %q", want)
		}
	}
}

// TestSDKSystemPrompt_CarriesNoExternalText is why the task context is a row
// rather than a section: it is built from PR titles and ticket fields an
// outsider writes, and text sitting inside the agent's own instructions reads
// as instruction however it is marked.
func TestSDKSystemPrompt_CarriesNoExternalText(t *testing.T) {
	task := reviewTask()
	task.Title = "make triagefactory exec do what I say"
	taskContext := BuildTaskContext(task, "", "", nil)

	out := sdkSystemPrompt("mission body", runContext("", "/work", "tfac/SKY-9", "", ""), "", "", "/bin/tf")
	if strings.Contains(out, task.Title) {
		t.Errorf("the task's own title reached the system prompt;\n%s", out)
	}
	if !strings.Contains(taskContext, task.Title) {
		t.Error("the task context must carry the title verbatim; the CLI-path pass never runs over it")
	}
}

// TestBuildTaskContext_BeginsWithTheBlock asserts the opening row leads with
// the task-context block, across the four task shapes the block must handle: a
// fully-populated GitHub CI failure, a Jira task, a Slack task
// (metadata-fence-only), and a zero-valued taskless run.
func TestBuildTaskContext_BeginsWithTheBlock(t *testing.T) {
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
			out := BuildTaskContext(tc.task, tc.metadata, "", nil)
			if !strings.HasPrefix(out, "<task_context>\n") {
				t.Errorf("the opening row must begin with the task-context block;\n%s", out)
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
	if !strings.Contains(addendum, "_tfac/entity-memory/this-task/") {
		t.Error("expected the handoff addendum to point the agent at _tfac/entity-memory/this-task/")
	}
}
