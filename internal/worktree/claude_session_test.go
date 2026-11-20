package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEncodeClaudeProjectDir_MatchesClaudeCode checks the encoder against
// names Claude Code itself created. Each want was captured by running Claude
// Code 2.1.329 in that cwd and listing the directory it created. To recapture
// after a Claude Code upgrade, run it from the cwd with nothing inherited, so
// no CLAUDE_CONFIG_DIR or CLAUDE_CODE_PROJECT_DIR_NAME moves or renames the
// directory. The CLI creates it before its API call fails:
//
//	h=$(mktemp -d)
//	timeout 15 env -i PATH="$PATH" HOME="$h" ANTHROPIC_API_KEY=x ANTHROPIC_BASE_URL=http://127.0.0.1:9 claude -p hi
//	ls "$h/.claude/projects"
//
// The Windows-shaped case is one Linux path component, which works because
// Claude Code's rule has no per-platform branch.
func TestEncodeClaudeProjectDir_MatchesClaudeCode(t *testing.T) {
	const long = "/tmp/triagefactory-runs/rk-"
	const longName = "-tmp-triagefactory-runs-rk-"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"slashes and dashes", "/tmp/triagefactory-runs/rk-1", "-tmp-triagefactory-runs-rk-1"},
		{"dots", "/home/dev/.triagefactory/takeovers/run-x", "-home-dev--triagefactory-takeovers-run-x"},
		{"underscores", "/var/folders/x_/3k_9w2/T/triagefactory-runs/rk-1", "-var-folders-x--3k-9w2-T-triagefactory-runs-rk-1"},
		{"space", "/tmp/my tmp/triagefactory-runs/rk-1", "-tmp-my-tmp-triagefactory-runs-rk-1"},
		{"backslashes and colon", `/tmp/C:\Users\dev\AppData\Local\Temp\triagefactory-runs\rk-1`, "-tmp-C--Users-dev-AppData-Local-Temp-triagefactory-runs-rk-1"},
		{"precomposed accent", "/tmp/triagefactory-runs/caf\u00e9", "-tmp-triagefactory-runs-caf-"},
		{"decomposed accent", "/tmp/triagefactory-runs/cafe\u0301", "-tmp-triagefactory-runs-cafe-"},
		{"outside the basic plane", "/tmp/triagefactory-runs/rk-\U0001F600", "-tmp-triagefactory-runs-rk---"},
		{"200 units, kept whole", long + strings.Repeat("a", 173), longName + strings.Repeat("a", 173)},
		{"201 units, negative hash", long + strings.Repeat("a", 173) + "b", longName + strings.Repeat("a", 173) + "-tebivo"},
		{"hash covers a surrogate pair past the cut", long + strings.Repeat("a", 173) + "b/\U0001F600", longName + strings.Repeat("a", 173) + "-lt33c6"},
		{"hash of math.MinInt32", long + strings.Repeat("m", 180) + "ahoijqxe", longName + strings.Repeat("m", 173) + "-zik0zk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := encodeClaudeProjectDir(tc.in); got != tc.want {
				t.Errorf("encodeClaudeProjectDir(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRemoveClaudeProjectDir_OnlyUnderTempDir pins the safety rail: a cwd that
// merely shares the temp dir's leading characters is outside it, and its
// project dir must survive.
func TestRemoveClaudeProjectDir_OnlyUnderTempDir(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	base := t.TempDir()
	tmp := filepath.Join(base, "tmp")
	inside := filepath.Join(tmp, "triagefactory-runs", "rk-1")
	sibling := filepath.Join(base, "tmpwork", "project")
	for _, d := range []string{inside, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", tmp)
	home := t.TempDir()
	t.Setenv("HOME", home)

	projectDir := func(cwd string) string {
		resolved, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(home, claudeProjectsDir, encodeClaudeProjectDir(resolved))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	insideDir, siblingDir := projectDir(inside), projectDir(sibling)

	RemoveClaudeProjectDir(sibling)
	if _, err := os.Stat(siblingDir); err != nil {
		t.Errorf("project dir for %s, outside the temp dir, was removed: %v", sibling, err)
	}
	RemoveClaudeProjectDir(inside)
	if _, err := os.Stat(insideDir); !os.IsNotExist(err) {
		t.Errorf("project dir for %s, inside the temp dir, was kept (err=%v)", inside, err)
	}
}
