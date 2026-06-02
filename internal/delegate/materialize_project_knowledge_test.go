package delegate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestMaterializeProjectKnowledge_NilProjectID_CreatesEmptyDir guards
// the same `ls`-without-ENOENT invariant materializePriorMemories
// guards: the agent's pre-flight scan of ./_scratch/project-knowledge/
// must succeed even when the entity has no project assigned.
func TestMaterializeProjectKnowledge_NilProjectID_CreatesEmptyDir(t *testing.T) {
	cwd := t.TempDir()

	materializeProjectKnowledge(runmode.LocalDefaultOrgID, cwd, nil)

	dir := filepath.Join(cwd, "_scratch", "project-knowledge")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("project-knowledge dir not created at %s: %v", dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("project-knowledge exists but is not a directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read project-knowledge: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty project-knowledge dir, found %d entries", len(entries))
	}
}

// TestMaterializeProjectKnowledge_CopiesAllMarkdown verifies the
// happy path: every .md file under <home>/.triagefactory/projects/<id>/
// knowledge-base/ lands in _scratch/project-knowledge/ flat, preserving
// filenames.
func TestMaterializeProjectKnowledge_CopiesAllMarkdown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projectID := "proj-1"
	kbDir := filepath.Join(home, ".triagefactory", "projects", projectID, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "architecture.md"), []byte("# Arch\n"), 0644); err != nil {
		t.Fatalf("write architecture.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "conventions.md"), []byte("# Conv\n"), 0644); err != nil {
		t.Fatalf("write conventions.md: %v", err)
	}
	// A non-markdown sibling — should not be copied.
	if err := os.WriteFile(filepath.Join(kbDir, "ignored.txt"), []byte("nope"), 0644); err != nil {
		t.Fatalf("write ignored.txt: %v", err)
	}

	cwd := t.TempDir()
	materializeProjectKnowledge(runmode.LocalDefaultOrgID, cwd, &projectID)

	dst := filepath.Join(cwd, "_scratch", "project-knowledge")
	for _, name := range []string{"architecture.md", "conventions.md"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Errorf("expected %s in project-knowledge: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "ignored.txt")); !os.IsNotExist(err) {
		t.Errorf("ignored.txt should not have been copied (err=%v)", err)
	}
}

// TestMaterializeProjectKnowledge_OversizedLogs verifies the soft cap:
// >500KB total still copies, just logs a warning. We don't assert on the
// log line — the load-bearing invariant is "files reach the destination."
func TestMaterializeProjectKnowledge_OversizedLogs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projectID := "proj-big"
	kbDir := filepath.Join(home, ".triagefactory", "projects", projectID, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	big := strings.Repeat("x", 600*1024)
	if err := os.WriteFile(filepath.Join(kbDir, "huge.md"), []byte(big), 0644); err != nil {
		t.Fatalf("write huge: %v", err)
	}

	cwd := t.TempDir()
	materializeProjectKnowledge(runmode.LocalDefaultOrgID, cwd, &projectID)

	dst := filepath.Join(cwd, "_scratch", "project-knowledge", "huge.md")
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("oversized file should still copy: %v", err)
	}
	if info.Size() != int64(len(big)) {
		t.Errorf("copied size = %d, want %d", info.Size(), len(big))
	}
}

// TestMaterializeProjectKnowledge_MissingKnowledgeDir_NoOp covers the
// projectID-set-but-no-KB-on-disk case: assigned project that hasn't
// had any knowledge files curated yet. Expect the destination dir to
// exist (for `ls`-without-ENOENT) and be empty, no error.
func TestMaterializeProjectKnowledge_MissingKnowledgeDir_NoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projectID := "proj-empty"
	cwd := t.TempDir()
	materializeProjectKnowledge(runmode.LocalDefaultOrgID, cwd, &projectID)

	dst := filepath.Join(cwd, "_scratch", "project-knowledge")
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("project-knowledge dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("project-knowledge exists but is not a directory")
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatalf("read project-knowledge: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty project-knowledge dir, found %d entries", len(entries))
	}
}

// TestLookupEntityProjectID_RoundTrips guards against the silent
// regression where db.GetEntity's SELECT omits project_id and
// e.ProjectID always reads as nil — making materializeProjectKnowledge
// a no-op for every assigned entity.
func TestLookupEntityProjectID_RoundTrips(t *testing.T) {
	database := newTakeoverTestDB(t)

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#1", "pr", "T", "https://x/1")
	if err != nil {
		t.Fatalf("entity: %v", err)
	}

	if got := lookupEntityProjectID(sqlitestore.New(database).Entities, runmode.LocalDefaultOrgID, entity.ID); got != nil {
		t.Errorf("expected nil for unassigned entity, got %q", *got)
	}

	if _, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{ID: "proj-rt", Name: "Roundtrip"}); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := database.Exec(`UPDATE entities SET project_id = ? WHERE id = ?`, "proj-rt", entity.ID); err != nil {
		t.Fatalf("assign project: %v", err)
	}

	got := lookupEntityProjectID(sqlitestore.New(database).Entities, runmode.LocalDefaultOrgID, entity.ID)
	if got == nil {
		t.Fatal("expected project id after assignment, got nil")
	}
	if *got != "proj-rt" {
		t.Errorf("project id = %q, want %q", *got, "proj-rt")
	}
}
