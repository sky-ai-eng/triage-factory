package uninstall

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the chmod 0555 that this test relies on to force a remove failure")
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

func TestGitHubAppKeychainKeys_EnumeratesConfiguredApps(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "triagefactory.db")
	conn, err := db.OpenAt(dbPath)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	// A minimal stand-in for org_github_apps — the enumeration only reads
	// app_id, so the FK columns the real schema carries are irrelevant here.
	if _, err := conn.Exec(`CREATE TABLE org_github_apps (app_id TEXT NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO org_github_apps (app_id) VALUES ('123'), ('456')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	keys, err := gitHubAppKeychainKeys(dbPath)
	if err != nil {
		t.Fatalf("gitHubAppKeychainKeys: %v", err)
	}
	want := []string{
		"github_app_123_pem", "github_app_123_client_secret", "github_app_123_webhook_secret",
		"github_app_456_pem", "github_app_456_client_secret", "github_app_456_webhook_secret",
	}
	// SELECT without ORDER BY makes row order unspecified — compare as sets.
	slices.Sort(keys)
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("keys = %v, want (any order) %v", keys, want)
	}
}

func TestGitHubAppKeychainKeys_NoDBReturnsNil(t *testing.T) {
	keys, err := gitHubAppKeychainKeys(filepath.Join(t.TempDir(), "absent.db"))
	if err != nil {
		t.Fatalf("err = %v, want nil for an absent DB", err)
	}
	if keys != nil {
		t.Fatalf("keys = %v, want nil for an absent DB", keys)
	}
}

func TestGitHubAppKeychainKeys_EmptyTableReturnsNil(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "triagefactory.db")
	conn, err := db.OpenAt(dbPath)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	if _, err := conn.Exec(`CREATE TABLE org_github_apps (app_id TEXT NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	keys, err := gitHubAppKeychainKeys(dbPath)
	if err != nil {
		t.Fatalf("gitHubAppKeychainKeys: %v", err)
	}
	if keys != nil {
		t.Fatalf("keys = %v, want nil when no App is configured", keys)
	}
}
