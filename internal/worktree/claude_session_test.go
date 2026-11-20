package worktree

import (
	"strings"
	"testing"
)

// TestEncodeClaudeProjectDir_MatchesClaudeCode checks the encoder against
// names Claude Code itself created. Each want was captured by running Claude
// Code 2.1.329 in that cwd under a fresh HOME and listing
// $HOME/.claude/projects; the 2.1.325 binary the pinned SDK ships carries the
// same function. To recapture after a Claude Code upgrade, do the same: a
// `claude -p` that never reaches the API still creates the directory.
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
