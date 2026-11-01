package delegate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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

// launchAppend is the append a launch sends: the two calls runAgent makes, in
// its order. The production seam takes block 2 already composed, because a
// resume reads that block off the row rather than rebuilding it — so these
// tests compose it the way the launch does and hand it over the same door.
func launchAppend(mission, runCtxBlock, toolsRef, nonTerminal, binaryPath string) string {
	return sdkSystemPrompt(composeConversationSystemBlock(mission, runCtxBlock, toolsRef, nonTerminal), binaryPath)
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
	runCtx := runContext("Repository: owner/repo", "/work", "", "tfac/SKY-9", "https://tf.example/runs/run-1", "", "", "")

	got := launchAppend(mission, runCtx, toolsRef, nonTerminal, "/bin/tf")
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

// TestSDKSystemPrompt_OmitsWhatThisRunHasNothingToSay covers the manual
// conversation with no mission, the terminal step with no addendum and the org
// that documented no verbs: an absent section renders nothing rather than an
// empty tag pair.
func TestSDKSystemPrompt_OmitsWhatThisRunHasNothingToSay(t *testing.T) {
	out := launchAppend("", runContext("", "", "", "", "", "", "", ""), "", "", "/bin/tf")
	if out != frameworkBlocks(t) {
		t.Errorf("with no per-run facts the append must be the framework blocks alone, with no empty section after them;\n%s",
			strings.TrimPrefix(out, frameworkBlocks(t)))
	}
}

// TestSDKSystemPrompt_ResolvesTheCLIPath covers the one rewrite the composer
// does. The allowlist grants the run's binary by absolute path and nothing
// else, so a `triagefactory exec` left unresolved in the framework text or the
// tools docs is a tool call the agent is refused, not a cosmetic miss.
func TestSDKSystemPrompt_ResolvesTheCLIPath(t *testing.T) {
	out := launchAppend("run `triagefactory exec gh pr view 7` first",
		runContext("", "/work", "", "tfac/SKY-9", "", "", "", ""), "use `triagefactory exec fake verb`", "", "/usr/local/bin/triagefactory")

	if strings.Contains(out, "`triagefactory exec") {
		t.Errorf("a bare `triagefactory exec` survived; the allowlist would refuse it;\n%s", out)
	}
	for _, want := range []string{
		"/usr/local/bin/triagefactory exec gh pr view 7", // the mission's own invocation
		"/usr/local/bin/triagefactory exec fake verb",    // the tools section's
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
	taskContext := BuildTaskContext(task, "", "", "", nil, nil)

	out := launchAppend("mission body", runContext("", "/work", "", "tfac/SKY-9", "", "", "", ""), "", "", "/bin/tf")
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
			out := BuildTaskContext(tc.task, "", tc.metadata, "", nil, nil)
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
	got := runContext("", "", "", "tfac/<ticket-id>", "", "", "", "")
	if want := "<run_context>\nBranch naming convention for this team: tfac/<ticket-id>\n</run_context>"; got != want {
		t.Errorf("run context with one fact set;\ngot  %q\nwant %q", got, want)
	}
	if got := runContext("", "", "", "", "", "", "", ""); got != "" {
		t.Errorf("a run context with nothing to say rendered %q, want empty", got)
	}
}

// TestHeldCloseLine_NamesTheEventThatResolvedTheTask: a launch on a task
// whose close is held is told which event resolved it and when, and a launch
// on a task holding none is told nothing.
func TestHeldCloseLine_NamesTheEventThatResolvedTheTask(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "run-held", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()
	var taskID, entityID string
	if err := database.QueryRow(`SELECT t.id, t.entity_id FROM conversations c JOIN tasks t ON t.id = c.task_id WHERE c.id = 'run-held'`).Scan(&taskID, &entityID); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if got := s.heldCloseLine(ctx, runmode.LocalDefaultOrgID, taskID); got != "" {
		t.Errorf("line for a task holding no close = %q, want none", got)
	}

	at := time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)
	eventID, err := sqlitestore.New(database).Events.Record(ctx, runmode.LocalDefaultOrgID, domain.Event{
		EventType: domain.EventGitHubPRCICheckPassed, EntityID: &entityID, OccurredAt: at,
	})
	if err != nil {
		t.Fatalf("record the closing event: %v", err)
	}
	if _, err := database.Exec(`UPDATE tasks SET close_deferred_event_id = ? WHERE id = ?`, eventID, taskID); err != nil {
		t.Fatalf("hold the task's close: %v", err)
	}
	want := domain.TaskClosingContextLine(domain.EventGitHubPRCICheckPassed, at)
	if got := s.heldCloseLine(ctx, runmode.LocalDefaultOrgID, taskID); got != want {
		t.Errorf("line for a held task = %q, want %q", got, want)
	}
}
