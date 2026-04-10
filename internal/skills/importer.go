package skills

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/todo-triage/internal/db"
	"github.com/sky-ai-eng/todo-triage/internal/domain"
)

// ImportResult summarizes what happened during an import run.
type ImportResult struct {
	Scanned  int      `json:"scanned"`
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
}

// ImportAll discovers and imports Claude Code skill files from both
// personal (~/.claude/skills/) and project-scoped (./.claude/skills/) locations.
func ImportAll(database *sql.DB) ImportResult {
	var result ImportResult

	home, err := os.UserHomeDir()
	if err != nil {
		result.Errors = append(result.Errors, "could not determine home dir: "+err.Error())
		return result
	}

	// Search paths: personal + project-scoped
	searchDirs := []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(".claude", "skills"),
	}

	for _, dir := range searchDirs {
		pattern := filepath.Join(dir, "*", "SKILL.md")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("glob %s: %v", pattern, err))
			continue
		}

		for _, path := range matches {
			result.Scanned++
			if err := importSkillFile(database, path); err != nil {
				if err == errSkillUnchanged {
					result.Skipped++
				} else {
					result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, err))
				}
			} else {
				result.Imported++
			}
		}
	}

	if result.Imported > 0 {
		log.Printf("[skills] imported %d skills (%d scanned, %d skipped)", result.Imported, result.Scanned, result.Skipped)
	}

	return result
}

var errSkillUnchanged = fmt.Errorf("skill unchanged")

func importSkillFile(database *sql.DB, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	content := string(data)
	name, description, body := parseSkillFile(content, path)

	// Deterministic ID from the file path so re-imports are idempotent
	id := fmt.Sprintf("imported-%x", sha256.Sum256([]byte(path)))[:20]

	// Check if already exists
	existing, err := db.GetPrompt(database, id)
	if err != nil {
		return err
	}
	if existing != nil {
		// Update body/name if the file changed
		if existing.Body == body && existing.Name == name {
			return errSkillUnchanged
		}
		if err := db.UpdatePrompt(database, id, name, body); err != nil {
			return err
		}
		log.Printf("[skills] updated %q from %s", name, path)
		return nil
	}

	prompt := domain.Prompt{
		ID:     id,
		Name:   name,
		Body:   body,
		Source: "imported",
	}

	if err := db.CreatePrompt(database, prompt); err != nil {
		return err
	}

	log.Printf("[skills] imported %q from %s (description: %s)", name, path, description)
	return nil
}

// parseSkillFile extracts the name, description, and body from a SKILL.md file.
// Handles YAML frontmatter between --- markers.
func parseSkillFile(content, path string) (name, description, body string) {
	// Default name from directory
	dir := filepath.Base(filepath.Dir(path))
	name = dir

	// Split frontmatter
	frontmatter, markdown := splitFrontmatter(content)

	// Parse frontmatter fields
	if frontmatter != "" {
		for _, line := range strings.Split(frontmatter, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "name:") {
				val := strings.TrimSpace(strings.TrimPrefix(line, "name:"))
				if val != "" {
					name = val
				}
			}
			if strings.HasPrefix(line, "description:") {
				description = strings.TrimSpace(strings.TrimPrefix(line, "description:"))
			}
		}
	}

	// The body is the markdown content (the actual prompt/instructions)
	body = strings.TrimSpace(markdown)
	if body == "" {
		body = content // fallback: use entire file
	}

	return name, description, body
}

// splitFrontmatter splits YAML frontmatter from markdown content.
// Returns ("", content) if no frontmatter found.
func splitFrontmatter(content string) (string, string) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "---") {
		return "", content
	}

	// Find closing ---
	rest := content[3:]
	if idx := strings.Index(rest, "\n---"); idx >= 0 {
		frontmatter := strings.TrimSpace(rest[:idx])
		body := strings.TrimSpace(rest[idx+4:])
		return frontmatter, body
	}

	return "", content
}
