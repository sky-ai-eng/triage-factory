//go:build windows

package worktree

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The expected names are the directories Claude Code created under
// ~/.claude/projects for these cwds on a Windows machine.
func TestEncodeClaudeProjectDir_Windows(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`C:\Tanium_Stuff\Projects\Tools\triage-factory`, `C--Tanium-Stuff-Projects-Tools-triage-factory`},
		{`C:\Tanium_Stuff\Projects\Tools\triage-factory\.claude\worktrees\tf-initial-port-547abf`,
			`C--Tanium-Stuff-Projects-Tools-triage-factory--claude-worktrees-tf-initial-port-547abf`},
		{`C:\Users\ERIC~1.ARN\AppData`, `C--Users-ERIC-1-ARN-AppData`},
		{`C:\Users\Eric Arnst\x`, `C--Users-Eric-Arnst-x`},
		// A rune outside the basic plane is two UTF-16 units, so two dashes.
		{"C:\\a\U0001F600b", "C--a--b"},
		// A rune inside the basic plane but outside ASCII is one unit.
		{"C:\\caf\u00e9", "C--caf-"},
	} {
		if got := encodeClaudeProjectDir(tc.in); got != tc.want {
			t.Errorf("encodeClaudeProjectDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestClaudeProjectDir_NameTooLongOnWindows(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	atLimit := `C:\` + strings.Repeat("a", claudeEncodedNameMax-3) // encodes to exactly 200
	if got := len(encodeClaudeProjectDir(atLimit)); got != claudeEncodedNameMax {
		t.Fatalf("test setup: encoded length %d, want %d", got, claudeEncodedNameMax)
	}
	if _, err := ClaudeProjectDir(atLimit); err != nil {
		t.Errorf("a name of exactly %d characters must resolve, got %v", claudeEncodedNameMax, err)
	}

	tooLong := atLimit + "a"
	_, err := ClaudeProjectDir(tooLong)
	if !errors.Is(err, ErrClaudeSessionNameTooLong) {
		t.Fatalf("ClaudeProjectDir(201 chars) err = %v, want ErrClaudeSessionNameTooLong", err)
	}
	if _, err := ClaudeSessionPath(tooLong, "sess"); !errors.Is(err, ErrClaudeSessionNameTooLong) {
		t.Errorf("ClaudeSessionPath err = %v, want ErrClaudeSessionNameTooLong", err)
	}
	if WarnIfClaudeSessionNameTooLong(tooLong) != true {
		t.Error("WarnIfClaudeSessionNameTooLong(201 chars) = false, want true")
	}
	if WarnIfClaudeSessionNameTooLong(atLimit) {
		t.Error("WarnIfClaudeSessionNameTooLong(200 chars) = true, want false")
	}
}

func TestClaudeProjectDir_LocalModeUsesUserProfileOnWindows(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)

	dir, err := ClaudeProjectDir(`C:\Users\me\.triagefactory\run-1`)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".claude", "projects", "C--Users-me--triagefactory-run-1")
	if dir != want {
		t.Fatalf("ClaudeProjectDir = %q, want %q", dir, want)
	}
}
