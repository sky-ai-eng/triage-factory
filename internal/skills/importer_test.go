package skills

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite memory: %v", err)
	}
	// SQLite :memory: is per connection; pin to one connection so schema/data
	// are visible across calls.
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = database.Close() })

	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir to %s: %v", dir, err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prev)
	})
}

func writeSkillFile(t *testing.T, root, skillName, content string) string {
	t.Helper()
	path := filepath.Join(root, ".claude", "skills", skillName, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write skill file: %v", err)
	}
	return path
}

func countVisibleImportedPrompts(t *testing.T, database *sql.DB) int {
	t.Helper()
	var count int
	if err := database.QueryRow(`
		SELECT COUNT(*)
		FROM prompts
		WHERE source = 'imported' AND hidden = 0
	`).Scan(&count); err != nil {
		t.Fatalf("count visible imported prompts: %v", err)
	}
	return count
}

func countHiddenImportedPrompts(t *testing.T, database *sql.DB) int {
	t.Helper()
	var count int
	if err := database.QueryRow(`
		SELECT COUNT(*)
		FROM prompts
		WHERE source = 'imported' AND hidden = 1
	`).Scan(&count); err != nil {
		t.Fatalf("count hidden imported prompts: %v", err)
	}
	return count
}

func importedPromptName(t *testing.T, database *sql.DB) string {
	t.Helper()
	var name string
	if err := database.QueryRow(`
		SELECT name
		FROM prompts
		WHERE source = 'imported' AND hidden = 0
		ORDER BY updated_at DESC
		LIMIT 1
	`).Scan(&name); err != nil {
		t.Fatalf("load imported prompt name: %v", err)
	}
	return name
}

func promptHidden(t *testing.T, database *sql.DB, id string) bool {
	t.Helper()
	var hidden bool
	if err := database.QueryRow(`SELECT hidden FROM prompts WHERE id = ?`, id).Scan(&hidden); err != nil {
		t.Fatalf("load prompt hidden state for %s: %v", id, err)
	}
	return hidden
}

func TestImportAll_DedupesResolvedSearchDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	chdir(t, home)

	writeSkillFile(t, home, "review-pr", "Review pull requests carefully.")

	database := newTestDB(t)
	result := ImportAll(t.Context(), database, testPromptStore(database))
	if len(result.Errors) > 0 {
		t.Fatalf("ImportAll errors: %v", result.Errors)
	}
	if result.Scanned != 1 {
		t.Fatalf("expected 1 scanned skill, got %d", result.Scanned)
	}
	if result.Imported != 1 {
		t.Fatalf("expected 1 imported skill, got %d", result.Imported)
	}
	if result.Skipped != 0 {
		t.Fatalf("expected 0 skipped skills, got %d", result.Skipped)
	}
	if got := countVisibleImportedPrompts(t, database); got != 1 {
		t.Fatalf("expected 1 visible imported prompt, got %d", got)
	}
}

func TestImportAll_DedupesByNameAndBodyAcrossLocations(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)
	chdir(t, project)

	content := "Help resolve merge conflicts and keep diffs small."
	writeSkillFile(t, home, "merge-helper", content)
	writeSkillFile(t, project, "merge-helper", content)

	database := newTestDB(t)
	result := ImportAll(t.Context(), database, testPromptStore(database))
	if len(result.Errors) > 0 {
		t.Fatalf("ImportAll errors: %v", result.Errors)
	}
	if result.Scanned != 2 {
		t.Fatalf("expected 2 scanned skills, got %d", result.Scanned)
	}
	if result.Imported != 1 {
		t.Fatalf("expected 1 imported skill, got %d", result.Imported)
	}
	if result.Skipped != 1 {
		t.Fatalf("expected 1 skipped duplicate skill, got %d", result.Skipped)
	}
	if got := countVisibleImportedPrompts(t, database); got != 1 {
		t.Fatalf("expected 1 visible imported prompt, got %d", got)
	}
}

func TestImportAll_HidesExistingDuplicateImportedPrompts(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)
	chdir(t, project)

	database := newTestDB(t)
	body := "Triage and prioritize incoming work."
	prompts := testPromptStore(database)
	ctx := t.Context()
	if _, err := prompts.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Prompt{
		ID:     "imported-duplicate-a",
		Name:   "triage",
		Body:   body,
		Source: "imported",
	}); err != nil {
		t.Fatalf("create first duplicate prompt: %v", err)
	}
	if _, err := prompts.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Prompt{
		ID:     "imported-duplicate-b",
		Name:   "triage",
		Body:   body,
		Source: "imported",
	}); err != nil {
		t.Fatalf("create second duplicate prompt: %v", err)
	}

	result := ImportAll(t.Context(), database, testPromptStore(database))
	if len(result.Errors) > 0 {
		t.Fatalf("ImportAll errors: %v", result.Errors)
	}
	if got := countVisibleImportedPrompts(t, database); got != 1 {
		t.Fatalf("expected 1 visible imported prompt after dedupe, got %d", got)
	}
	if got := countHiddenImportedPrompts(t, database); got != 1 {
		t.Fatalf("expected 1 hidden imported prompt after dedupe, got %d", got)
	}
}

func TestImportAll_UsesDiscoveredPathForDefaultName(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	external := t.TempDir()
	t.Setenv("HOME", home)
	chdir(t, project)

	targetDir := filepath.Join(external, "real-skill")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("mkdir external skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "SKILL.md"), []byte("Keep PRs reviewable."), 0644); err != nil {
		t.Fatalf("write external skill: %v", err)
	}

	linkDir := filepath.Join(project, ".claude", "skills", "alias-skill")
	if err := os.MkdirAll(filepath.Dir(linkDir), 0755); err != nil {
		t.Fatalf("mkdir project skills dir: %v", err)
	}
	if err := os.Symlink(targetDir, linkDir); err != nil {
		t.Fatalf("symlink %s -> %s: %v", linkDir, targetDir, err)
	}

	database := newTestDB(t)
	result := ImportAll(t.Context(), database, testPromptStore(database))
	if len(result.Errors) > 0 {
		t.Fatalf("ImportAll errors: %v", result.Errors)
	}
	if result.Imported != 1 {
		t.Fatalf("expected 1 imported skill, got %d", result.Imported)
	}
	if got := importedPromptName(t, database); got != "alias-skill" {
		t.Fatalf("expected prompt name from discovered symlink dir, got %q", got)
	}
}

func TestImportAll_DoesNotHideDuplicatePromptReferencedByTrigger(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)
	chdir(t, project)

	database := newTestDB(t)
	body := "Triage and prioritize incoming work."
	keepID := "imported-duplicate-with-trigger"
	hideID := "imported-duplicate-no-trigger"

	prompts := testPromptStore(database)
	ctx := t.Context()
	if _, err := prompts.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Prompt{
		ID:     keepID,
		Name:   "triage",
		Body:   body,
		Source: "imported",
	}); err != nil {
		t.Fatalf("create triggered duplicate prompt: %v", err)
	}
	if _, err := prompts.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Prompt{
		ID:     hideID,
		Name:   "triage",
		Body:   body,
		Source: "imported",
	}); err != nil {
		t.Fatalf("create unreferenced duplicate prompt: %v", err)
	}

	trigger := domain.EventHandler{
		ID:                     "trigger-keep-imported-prompt",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            keepID,
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              domain.EventGitHubPRReviewRequested,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(0),
		Enabled:                true,
	}
	createTriggerForTestSkills(t, database, trigger)

	result := ImportAll(t.Context(), database, testPromptStore(database))
	if len(result.Errors) > 0 {
		t.Fatalf("ImportAll errors: %v", result.Errors)
	}

	storedTrigger := getTriggerForTestSkills(t, database, trigger.ID)
	if storedTrigger == nil {
		t.Fatal("expected trigger to remain after import")
	}
	// The trigger now references a blueprint wrapping keepID; resolve through
	// its first step to confirm it still points at the kept prompt.
	steps, err := sqlitestore.New(database).Blueprints.ListSteps(t.Context(), runmode.LocalDefaultOrgID, storedTrigger.BlueprintID)
	if err != nil || len(steps) == 0 {
		t.Fatalf("resolve trigger blueprint steps: steps=%v err=%v", steps, err)
	}
	if steps[0].StepPromptID != keepID {
		t.Fatalf("expected trigger blueprint to keep prompt %q, got %q", keepID, steps[0].StepPromptID)
	}
	if promptHidden(t, database, keepID) {
		t.Fatalf("prompt %q has a trigger and should not be hidden", keepID)
	}
	if !promptHidden(t, database, hideID) {
		t.Fatalf("prompt %q is duplicate without trigger and should be hidden", hideID)
	}
}
