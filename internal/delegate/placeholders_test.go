package delegate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// interpolate runs the replacer against a template and returns the result.
// Convenience wrapper so tests read as "replace these placeholders in this
// template" instead of "build a replacer and also remember to call it".
func interpolate(r *strings.Replacer, template string) string {
	return r.Replace(template)
}

func TestBuildPromptReplacer_CICheckFailed(t *testing.T) {
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:        "alice",
		AuthorIsSelf:  true,
		CheckRunID:    42,
		CheckName:     "test",
		CheckURL:      "https://github.com/owner/repo/runs/42",
		WorkflowRunID: 12345,
		HeadSHA:       "abc123",
		Repo:          "owner/repo",
		PRNumber:      18,
	}
	metaJSON, _ := json.Marshal(meta)

	task := domain.Task{
		ID:             "task-1",
		Title:          "test failed on PR #18",
		EventType:      domain.EventGitHubPRCICheckFailed,
		EntitySource:   "github",
		EntitySourceID: "owner/repo#18",
	}

	r := BuildPromptReplacer(task, string(metaJSON), "run-xyz", "/bin/triagefactory", "scope text", "tools ref")

	template := `Download logs: {{BINARY_PATH}} exec gh actions download-logs {{WORKFLOW_RUN_ID}}
Run ID: {{RUN_ID}}  Check: {{CHECK_NAME}}
Repo: {{OWNER}}/{{REPO}} PR #{{PR_NUMBER}} at {{HEAD_SHA}}
Title: {{TASK_TITLE}}
Event: {{EVENT_TYPE}}`

	got := interpolate(r, template)
	want := `Download logs: /bin/triagefactory exec gh actions download-logs 12345
Run ID: run-xyz  Check: test
Repo: owner/repo PR #18 at abc123
Title: test failed on PR #18
Event: github:pr:ci_check_failed`

	if got != want {
		t.Errorf("interpolation mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestBuildPromptReplacer_ThirdPartyCI_ZeroWorkflowRun(t *testing.T) {
	// Third-party CI (Supabase, Circle): workflow_run_id is absent from
	// the metadata JSON because the struct field has `omitempty`. The
	// placeholder should render empty, NOT "0" — a prompt that runs
	// `download-logs {{WORKFLOW_RUN_ID}}` needs an empty string so the
	// command fails cleanly with a usage error rather than attempting
	// a download for run ID 0.
	meta := events.GitHubPRCICheckFailedMetadata{
		Author:     "alice",
		CheckRunID: 42,
		CheckName:  "supabase-ci",
		// WorkflowRunID intentionally zero (omitempty drops it from JSON)
		HeadSHA:  "abc123",
		Repo:     "owner/repo",
		PRNumber: 18,
	}
	metaJSON, _ := json.Marshal(meta)

	task := domain.Task{
		EventType:      domain.EventGitHubPRCICheckFailed,
		EntitySource:   "github",
		EntitySourceID: "owner/repo#18",
	}

	r := BuildPromptReplacer(task, string(metaJSON), "run-xyz", "/bin/triagefactory", "", "")

	if got := interpolate(r, "wf={{WORKFLOW_RUN_ID}}"); got != "wf=" {
		t.Errorf("expected empty workflow_run_id placeholder for third-party CI, got %q", got)
	}
}

func TestBuildPromptReplacer_JiraAssigned(t *testing.T) {
	meta := events.JiraIssueAssignedMetadata{
		Assignee:       "Aidan",
		AssigneeIsSelf: true,
		IssueKey:       "SKY-123",
		Project:        "SKY",
		IssueType:      "Task",
		Priority:       "High",
		Status:         "To Do",
		Summary:        "Fix the thing",
	}
	metaJSON, _ := json.Marshal(meta)

	task := domain.Task{
		Title:          "SKY-123 assigned to you",
		EventType:      domain.EventJiraIssueAssigned,
		EntitySource:   "jira",
		EntitySourceID: "SKY-123",
	}

	r := BuildPromptReplacer(task, string(metaJSON), "run-xyz", "/bin/tf", "", "")

	got := interpolate(r, "{{ISSUE_KEY}} ({{PROJECT}}): {{SUMMARY}} [{{PRIORITY}}/{{STATUS}}]")
	want := "SKY-123 (SKY): Fix the thing [High/To Do]"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}

	// GitHub-specific placeholders must render empty on a Jira task —
	// prompts authored for mixed triggers shouldn't leak stale PR numbers.
	if strings.Contains(interpolate(r, "x{{OWNER}}x{{REPO}}x{{PR_NUMBER}}x"), "owner") {
		t.Error("expected GitHub placeholders to be empty on a Jira task")
	}
}

func TestBuildPromptReplacer_EmptyMetadata(t *testing.T) {
	// No event metadata loaded (DB fetch failed, or event had empty
	// metadata). Identity placeholders still resolve; event-specific ones
	// render empty. Run must not crash.
	task := domain.Task{
		Title:          "some task",
		EventType:      "some:event:type",
		EntitySource:   "github",
		EntitySourceID: "owner/repo#7",
	}

	r := BuildPromptReplacer(task, "", "run-xyz", "/bin/tf", "", "")

	got := interpolate(r, "{{OWNER}}/{{REPO}}#{{PR_NUMBER}} run={{WORKFLOW_RUN_ID}} head={{HEAD_SHA}}")
	want := "owner/repo#7 run= head="
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestBuildPromptReplacer_EventMetadataJSONEscapeHatch(t *testing.T) {
	// Prompts can request the full metadata blob via {{EVENT_METADATA_JSON}}
	// — lets custom prompts reference any field we haven't flattened into
	// a named placeholder.
	metaJSON := `{"assignee":"Alice","issue_key":"SKY-1","custom_field":"value"}`

	task := domain.Task{
		EventType:      domain.EventJiraIssueAssigned,
		EntitySource:   "jira",
		EntitySourceID: "SKY-1",
	}

	r := BuildPromptReplacer(task, metaJSON, "", "", "", "")

	got := interpolate(r, "meta={{EVENT_METADATA_JSON}}")
	want := "meta=" + metaJSON
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestParseGitHubEntitySourceID(t *testing.T) {
	cases := []struct {
		in              string
		owner, repo, pr string
	}{
		{"owner/repo#42", "owner", "repo", "42"},
		{"a-b/c.d#1", "a-b", "c.d", "1"},
		{"no-hash", "", "", ""},
		{"#42", "", "", "42"},
		{"owner/repo/sub#5", "owner/repo", "sub", "5"},
	}
	for _, tc := range cases {
		o, r, p := parseGitHubEntitySourceID(tc.in)
		if o != tc.owner || r != tc.repo || p != tc.pr {
			t.Errorf("parse %q: got (%q,%q,%q) want (%q,%q,%q)", tc.in, o, r, p, tc.owner, tc.repo, tc.pr)
		}
	}
}
