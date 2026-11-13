package delegate

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The paths the prompt shows the agent are paths its shell and file tools
// will use. On Windows that means forward slashes, because a backslash path
// pasted into a bash command is read as escapes.
func TestAgentVisibleCheckout_IsTheAgentSpelling(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs", "task1")
	checkout := filepath.Join(root, "owner", "repo", "slug")
	elsewhere := filepath.Join(t.TempDir(), "other", "repo")

	for name, tc := range map[string]struct{ checkout, want string }{
		"checkout under the run root": {checkout, filepath.ToSlash(checkout)},
		"checkout outside the root":   {elsewhere, filepath.ToSlash(elsewhere)},
		"no checkout":                 {"", ""},
	} {
		got := agentVisibleCheckout(root, tc.checkout)
		if got != tc.want {
			t.Errorf("%s: agentVisibleCheckout = %q, want %q", name, got, tc.want)
		}
		if runtime.GOOS == "windows" && strings.Contains(got, `\`) {
			t.Errorf("%s: %q carries a backslash", name, got)
		}
	}
}

func TestBuildBlueprintStepWrapperPrompt_SkillPathIsTheAgentSpelling(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs", "task1")
	prompt := buildBlueprintStepWrapperPrompt(
		domain.Task{Title: "t"},
		domain.BlueprintStep{StepIndex: 0, Brief: "do it"},
		&domain.Prompt{Name: "p"},
		"my-skill", agentproc.AgentVisibleRoot(root), 1, "",
	)
	want := filepath.ToSlash(filepath.Join(root, ".claude", "skills", "my-skill", "SKILL.md"))
	if !strings.Contains(prompt, want) {
		t.Errorf("prompt does not name the skill at %q:\n%s", want, prompt)
	}
	if runtime.GOOS == "windows" && strings.Contains(prompt, `\`) {
		t.Errorf("prompt carries a backslash path:\n%s", prompt)
	}
}

// On Windows the agent runs `triagefactory exec …` by its bare name, so the
// substitution leaves the prompt's own spelling alone and the allowlist rule
// names the same string.
func TestLocalAgentBinary_PromptAndAllowlistAgree(t *testing.T) {
	host := filepath.Join(t.TempDir(), "triagefactory.exe")
	name := agentproc.LocalAgentBinary(host)

	got := resolveCLIPath("run `triagefactory exec gh pr view 1`", name)
	if want := "run `" + name + " exec gh pr view 1`"; got != want {
		t.Errorf("resolveCLIPath = %q, want %q", got, want)
	}
	allow := agentproc.BuildAllowedToolsFor(agentproc.AllowedToolsOptions{SelfBin: name})
	if !strings.Contains(allow, "Bash("+name+" exec *)") {
		t.Errorf("allowlist does not grant %q", name+" exec")
	}
	if runtime.GOOS == "windows" {
		if !strings.Contains(allow, "Bash(triagefactory exec *)") {
			t.Errorf("Windows allowlist does not grant the bare-name rule")
		}
		if got != "run `triagefactory exec gh pr view 1`" {
			t.Errorf("Windows prompt was rewritten to %q, want the bare name left alone", got)
		}
	}
}
