package uninstall

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildPlan_DetectsCuratorProjects(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, ".triagefactory")
	projectsDir := filepath.Join(dataDir, "projects")
	if err := os.MkdirAll(filepath.Join(projectsDir, "proj-a"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	plan := buildPlan(dataDir, projectsDir, "")
	if !plan.hasProjects {
		t.Fatalf("plan.hasProjects = false, want true")
	}
	if plan.empty() {
		t.Fatalf("plan.empty() = true, want false")
	}
}

func TestRemoveClaudeProjectsForCurator_CountsOnlyExistingDirs(t *testing.T) {
	home := t.TempDir()
	projectsDir := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", projectsDir, err)
	}

	projA := filepath.Join(projectsDir, "proj-a")
	projB := filepath.Join(projectsDir, "proj-b")
	for _, d := range []string{projA, projB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", d, err)
		}
	}

	// Pre-create the Claude session dir for proj-a only — proj-b's
	// encoded path does not exist on disk and must be skipped silently.
	encodedA := claudeProjectDirForRun(t, home, projA)
	if err := os.MkdirAll(encodedA, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", encodedA, err)
	}

	n, err := removeClaudeProjectsForCurator(projectsDir, home)
	if err != nil {
		t.Fatalf("removeClaudeProjectsForCurator() error: %v", err)
	}
	if n != 1 {
		t.Fatalf("removeClaudeProjectsForCurator() removed %d dirs, want 1", n)
	}
	if _, err := os.Stat(encodedA); !os.IsNotExist(err) {
		t.Fatalf("encodedA still exists or unexpected stat error: %v", err)
	}
}

func TestRemoveClaudeProjectsForCurator_ReturnsRemoveErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based permission test is not reliable on Windows")
	}

	home := t.TempDir()
	projectsDir := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", projectsDir, err)
	}
	projDir := filepath.Join(projectsDir, "proj-perm")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", projDir, err)
	}

	encoded := claudeProjectDirForRun(t, home, projDir)
	if err := os.MkdirAll(encoded, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", encoded, err)
	}

	projectsRoot := filepath.Join(home, ".claude", "projects")
	if err := os.Chmod(projectsRoot, 0o555); err != nil {
		t.Fatalf("Chmod(%q): %v", projectsRoot, err)
	}
	defer func() {
		_ = os.Chmod(projectsRoot, 0o755)
	}()

	n, err := removeClaudeProjectsForCurator(projectsDir, home)
	if err == nil {
		t.Fatalf("removeClaudeProjectsForCurator() error = nil, want non-nil")
	}
	if n != 0 {
		t.Fatalf("removeClaudeProjectsForCurator() removed %d dirs, want 0", n)
	}
}

func claudeProjectDirForRun(t *testing.T, home, runDir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(runDir)
	if err != nil {
		resolved = runDir
	}
	encoded := strings.NewReplacer("/", "-", ".", "-").Replace(resolved)
	return filepath.Join(home, ".claude", "projects", encoded)
}
