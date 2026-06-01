package uninstall

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

func TestResolvedTakeoversDir_Default(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, ".triagefactory")
	got, err := resolvedTakeoversDir(filepath.Join(dataDir, "triagefactory.db"), filepath.Join(dataDir, "takeovers"))
	if err != nil {
		t.Fatalf("resolvedTakeoversDir() error: %v", err)
	}

	want := filepath.Join(dataDir, "takeovers")
	if got != want {
		t.Fatalf("resolvedTakeoversDir() = %q, want %q", got, want)
	}
}

func TestResolvedTakeoversDir_ConfigOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, ".triagefactory")

	// Settings live in the DB now — open + migrate, then write the
	// instance_config override directly so resolvedTakeoversDir's
	// downstream read sees it.
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open(): %v", err)
	}
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		conn.Close()
		t.Fatalf("db.Migrate(): %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO instance_config (id, server_port, server_takeover_dir, updated_at)
		VALUES (1, 3000, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET server_takeover_dir = excluded.server_takeover_dir
	`, "~/custom-takeovers"); err != nil {
		conn.Close()
		t.Fatalf("write instance_config: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close(): %v", err)
	}

	got, err := resolvedTakeoversDir(filepath.Join(dataDir, "triagefactory.db"), filepath.Join(dataDir, "takeovers"))
	if err != nil {
		t.Fatalf("resolvedTakeoversDir() error: %v", err)
	}

	want := filepath.Join(home, "custom-takeovers")
	if got != want {
		t.Fatalf("resolvedTakeoversDir() = %q, want %q", got, want)
	}
}

func TestBuildPlan_DetectsTakeoversOutsideDataDir(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, ".triagefactory")
	takeoversDir := filepath.Join(root, "custom-takeovers")
	if err := os.MkdirAll(takeoversDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", takeoversDir, err)
	}

	plan := buildPlan(dataDir, takeoversDir, filepath.Join(dataDir, "projects"), "")
	if !plan.hasTakeovers {
		t.Fatalf("plan.hasTakeovers = false, want true")
	}
	if plan.empty() {
		t.Fatalf("plan.empty() = true, want false")
	}
}

func TestRemoveClaudeProjectsForTakeovers_CountsOnlyExistingDirs(t *testing.T) {
	home := t.TempDir()
	takeoversDir := filepath.Join(t.TempDir(), "takeovers")
	if err := os.MkdirAll(takeoversDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", takeoversDir, err)
	}

	runA := filepath.Join(takeoversDir, "run-a")
	runB := filepath.Join(takeoversDir, "run-b")
	for _, runDir := range []string{runA, runB} {
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", runDir, err)
		}
	}

	projectA := claudeProjectDirForRun(t, home, runA)
	if err := os.MkdirAll(projectA, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", projectA, err)
	}

	n, err := removeClaudeProjectsForTakeovers(takeoversDir, home)
	if err != nil {
		t.Fatalf("removeClaudeProjectsForTakeovers() error: %v", err)
	}
	if n != 1 {
		t.Fatalf("removeClaudeProjectsForTakeovers() removed %d dirs, want 1", n)
	}
	if _, err := os.Stat(projectA); !os.IsNotExist(err) {
		t.Fatalf("projectA still exists or unexpected stat error: %v", err)
	}
}

func TestRemoveClaudeProjectsForTakeovers_ReturnsRemoveErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based permission test is not reliable on Windows")
	}

	home := t.TempDir()
	takeoversDir := filepath.Join(t.TempDir(), "takeovers")
	if err := os.MkdirAll(takeoversDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", takeoversDir, err)
	}
	runDir := filepath.Join(takeoversDir, "run-perm")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", runDir, err)
	}

	projectDir := claudeProjectDirForRun(t, home, runDir)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", projectDir, err)
	}

	projectsRoot := filepath.Join(home, ".claude", "projects")
	if err := os.Chmod(projectsRoot, 0o555); err != nil {
		t.Fatalf("Chmod(%q): %v", projectsRoot, err)
	}
	defer func() {
		_ = os.Chmod(projectsRoot, 0o755)
	}()

	n, err := removeClaudeProjectsForTakeovers(takeoversDir, home)
	if err == nil {
		t.Fatalf("removeClaudeProjectsForTakeovers() error = nil, want non-nil")
	}
	if n != 0 {
		t.Fatalf("removeClaudeProjectsForTakeovers() removed %d dirs, want 0", n)
	}
}

func TestBuildPlan_DetectsCuratorProjects(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, ".triagefactory")
	projectsDir := filepath.Join(dataDir, "projects")
	if err := os.MkdirAll(filepath.Join(projectsDir, "proj-a"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	plan := buildPlan(dataDir, filepath.Join(dataDir, "takeovers"), projectsDir, "")
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
