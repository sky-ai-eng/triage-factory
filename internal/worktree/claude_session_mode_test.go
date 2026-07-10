package worktree

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestClaudeProjectDir_LocalModeUsesHome(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir, err := ClaudeProjectDir("/tmp/triagefactory-runs/run-1")
	if err != nil {
		t.Fatalf("ClaudeProjectDir: %v", err)
	}
	want := filepath.Join(home, ".claude", "projects", "-tmp-triagefactory-runs-run-1")
	if dir != want {
		t.Fatalf("ClaudeProjectDir = %q, want %q", dir, want)
	}
}

func TestClaudeProjectDir_SandboxedMultiUsesRunRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sandbox branch is linux-only")
	}
	runmode.SetForTest(t, runmode.ModeMulti)
	// Must NOT consult HOME at all: the sandboxed agent ran with
	// HOME=/work, so its session tree is inside the run root on the host.
	t.Setenv("HOME", "")

	dir, err := ClaudeProjectDir("/data/orgs/org-1/projects/p-1")
	if err != nil {
		t.Fatalf("ClaudeProjectDir: %v", err)
	}
	want := filepath.Join("/data/orgs/org-1/projects/p-1", ".claude", "projects", "-work")
	if dir != want {
		t.Fatalf("ClaudeProjectDir = %q, want %q", dir, want)
	}

	p, err := ClaudeSessionPath("/data/orgs/org-1/projects/p-1", "sess-1")
	if err != nil {
		t.Fatalf("ClaudeSessionPath: %v", err)
	}
	if p != filepath.Join(want, "sess-1.jsonl") {
		t.Fatalf("ClaudeSessionPath = %q, want under %q", p, want)
	}
}
