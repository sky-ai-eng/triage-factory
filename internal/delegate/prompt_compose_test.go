package delegate

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestBuildPrompt_InterpolatesInjectedSections guards against the single-pass
// strings.Replacer foot-gun: the tools/scope sections are injected into the
// envelope, and a section injected as a *replacement value* would keep its own
// placeholders verbatim because strings.Replacer does not re-scan replacements.
// buildPrompt inlines those sections before the placeholder pass, so the tools
// docs' {{BINARY_PATH}} and run-root memory paths must come out fully expanded.
func TestBuildPrompt_InterpolatesInjectedSections(t *testing.T) {
	task := domain.Task{
		Title:          "review PR #1",
		EventType:      domain.EventGitHubPRReviewRequested,
		EntitySource:   "github",
		EntitySourceID: "owner/repo#1",
	}
	toolsRef := agentprompt.GitHubToolsReference() + "\n\n" + agentprompt.JiraToolsReference()

	out := buildPrompt(task, "", "", "mission body", "Repository: owner/repo", toolsRef,
		"/usr/local/bin/triagefactory", "run-1", "/work", "bp-run-1", "tfac/SKY-9", "")

	if strings.Contains(out, "{{BINARY_PATH}}") {
		t.Error("literal {{BINARY_PATH}} survived in the composed prompt (tools section not interpolated)")
	}
	if strings.Contains(out, "{{TOOLS_REFERENCE}}") || strings.Contains(out, "{{SCOPE}}") || strings.Contains(out, "{{BRANCH_TEMPLATE}}") {
		t.Error("an injected section placeholder survived in the composed prompt")
	}
	// The branch template (already ticket-id-resolved) must surface in the
	// envelope guidance.
	if !strings.Contains(out, "tfac/SKY-9") {
		t.Error("expected the resolved branch template in the composed prompt envelope")
	}
	// The env-var-style run-root reference in the tools docs must be expanded
	// to the concrete agent-visible path, not left for (absent) shell expansion.
	if strings.Contains(out, "$TRIAGE_FACTORY_CONVERSATION_ROOT") {
		t.Error("literal $TRIAGE_FACTORY_CONVERSATION_ROOT survived in the composed prompt")
	}
	if strings.Contains(out, "$TRIAGE_FACTORY_BLUEPRINT_RUN_ID") {
		t.Error("literal $TRIAGE_FACTORY_BLUEPRINT_RUN_ID survived in the composed prompt")
	}
	// The concrete binary path from the tools docs should be present.
	if !strings.Contains(out, "/usr/local/bin/triagefactory exec gh") {
		t.Error("expected interpolated binary path in the tools section")
	}
	// The memory write path must resolve to the concrete absolute path.
	if !strings.Contains(out, "/work/_tfac/memory.md") {
		t.Errorf("expected concrete memory write path in the composed prompt;\n%s", out)
	}
}

// TestBlueprintStepNonterminalPrompt_MemoryPathCarriesNoIDs is the handoff
// addendum's half of the fixed-path contract (the composed framework prompt and
// the tools docs cover the rest): the step it hands off to reads a folder the
// orchestrator names, never a path composed from run ids.
func TestBlueprintStepNonterminalPrompt_MemoryPathCarriesNoIDs(t *testing.T) {
	addendum := agentprompt.NonTerminalCompletion(machinistSpec())
	for _, bad := range []string{"entity-memory/{{", "entity-memory/$"} {
		if strings.Contains(addendum, bad) {
			t.Errorf("the handoff addendum composes an entity-memory path from placeholders (%q)", bad)
		}
	}
	if !strings.Contains(addendum, "_tfac/entity-memory/this-run/") {
		t.Error("expected the handoff addendum to point the agent at _tfac/entity-memory/this-run/")
	}
}

// TestBuildPrompt_BeginsWithTaskContext asserts every composed prompt leads
// with the injected task-context block, across the four task shapes the block
// must handle: a fully-populated GitHub CI failure, a Jira task, a Slack task
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
				"", "/bin/tf", "run-1", "/work", "bp-run-1", "tfac/<ticket-id>", "")
			if !strings.HasPrefix(out, "<task_context>\n") {
				t.Errorf("composed prompt must begin with the task-context block;\n%s", out)
			}
		})
	}
}

// TestBuildPrompt_PrependOnly locks the composition contract: for any existing
// prompt, the composed output differs from the pre-injection output by exactly
// the prepended block plus its separator — nothing else changes.
func TestBuildPrompt_PrependOnly(t *testing.T) {
	task := domain.Task{
		Title:          "review PR #7",
		EventType:      domain.EventGitHubPRReviewRequested,
		EntitySource:   "github",
		EntitySourceID: "owner/repo#7",
	}
	metadata := `{"reviewer":"octocat","review_type":"changes_requested"}`
	mission := "mission body with {{PR_NUMBER}}"
	scope := "Repository: owner/repo"
	toolsRef := agentprompt.GitHubToolsReference()

	out := buildPrompt(task, metadata, "", mission, scope, toolsRef,
		"/bin/tf", "run-1", "/work", "bp-run-1", "tfac/SKY-9", "")

	// Reconstruct the pre-injection return value: the same shim + section
	// pre-pass + replacer pass buildPrompt runs, without the prepended block.
	body := strings.ReplaceAll(mission, "triagefactory exec", "/bin/tf exec")
	full := body + "\n\n" + agentprompt.Build(machinistSpec(), agentprompt.Parts{})
	full = strings.NewReplacer("{{TOOLS_REFERENCE}}", toolsRef, "{{SCOPE}}", scope).Replace(full)
	prev := BuildPromptReplacer(task, metadata, "run-1", "/bin/tf", "/work", "bp-run-1", "tfac/SKY-9", "").Replace(full)

	want := BuildTaskContext(task, metadata, "") + "\n\n" + prev
	if out != want {
		t.Errorf("composed output must be exactly the block + separator + prior output;\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

// TestBuildPrompt_ExternalTextNotInterpolated proves the ordering is what makes
// external placeholder-shaped text inert: a task title carrying literal
// {{RUN_ID}} survives verbatim in the block, while a real body placeholder in
// the mission still interpolates.
func TestBuildPrompt_ExternalTextNotInterpolated(t *testing.T) {
	task := domain.Task{
		Title:          "handle {{RUN_ID}} in the retry path",
		EventType:      domain.EventGitHubPRCICheckFailed,
		EntitySource:   "github",
		EntitySourceID: "owner/repo#5",
	}

	out := buildPrompt(task, "", "", "mission for run {{RUN_ID}}", "", "",
		"/bin/tf", "the-real-run-id", "/work", "bp-run-1", "tfac/<ticket-id>", "")

	if !strings.Contains(out, "handle {{RUN_ID}} in the retry path") {
		t.Errorf("external {{RUN_ID}} in the title must survive uninterpolated;\n%s", out)
	}
	if !strings.Contains(out, "mission for run the-real-run-id") {
		t.Errorf("a genuine body placeholder must still interpolate;\n%s", out)
	}
}
