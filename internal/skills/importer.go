package skills

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
//
// Takes both *sql.DB (for the duplicate-detection raw scans that
// haven't migrated to a store method yet — wave 1 carry-over) and
// PromptStore (for the routable Get/Create/UpdateImported/Hide
// calls).
//
// ctx flows through every DB call and the per-file processing loop,
// so the request-triggered import (POST /api/skills/import) cancels
// promptly if the HTTP client disconnects or the request times out.
// Startup callers pass context.Background.
func ImportAll(ctx context.Context, database *sql.DB, prompts db.PromptStore) ImportResult {
	var result ImportResult

	// ~/.claude/skills is Claude Code user state keyed to the real HOME,
	// not TF state — it stays home-relative and does not route through
	// internal/paths.
	home, err := os.UserHomeDir() //nolint:forbidigo // Claude Code user state, not TF state (see internal/paths doc).
	if err != nil {
		result.Errors = append(result.Errors, "could not determine home dir: "+err.Error())
		return result
	}

	// Search paths: personal + project-scoped (relative to current working dir)
	searchDirs := []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(".claude", "skills"),
	}

	seenDirs := make(map[string]struct{})
	seenFiles := make(map[string]struct{})

	for _, dir := range searchDirs {
		normalizedDir, err := normalizePath(dir)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("normalize dir %s: %v", dir, err))
			continue
		}
		if _, ok := seenDirs[normalizedDir]; ok {
			// If the project-scoped dir resolves to the same location as the
			// personal dir (e.g. cwd is $HOME), only scan it once.
			continue
		}
		seenDirs[normalizedDir] = struct{}{}

		pattern := filepath.Join(dir, "*", "SKILL.md")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("glob %s: %v", pattern, err))
			continue
		}

		for _, path := range matches {
			if err := ctx.Err(); err != nil {
				result.Errors = append(result.Errors, "import cancelled: "+err.Error())
				return result
			}
			normalizedPath, err := normalizePath(path)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("normalize file %s: %v", path, err))
				continue
			}
			if _, ok := seenFiles[normalizedPath]; ok {
				result.Skipped++
				continue
			}
			seenFiles[normalizedPath] = struct{}{}

			result.Scanned++
			if err := importSkillFile(ctx, database, prompts, path, normalizedPath); err != nil {
				if err == errSkillUnchanged || err == errSkillDuplicate {
					result.Skipped++
				} else {
					result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, err))
				}
			} else {
				result.Imported++
			}
		}
	}

	hiddenDuplicates, err := hideDuplicateImportedPrompts(ctx, database, prompts)
	if err != nil {
		result.Errors = append(result.Errors, "deduplicate imported prompts: "+err.Error())
	} else if hiddenDuplicates > 0 {
		log.Printf("[skills] deduplicated %d already-imported skills (same name/body/allowed_tools)", hiddenDuplicates)
	}

	if result.Imported > 0 {
		log.Printf("[skills] imported %d skills (%d scanned, %d skipped)", result.Imported, result.Scanned, result.Skipped)
	}

	return result
}

var (
	errSkillUnchanged = fmt.Errorf("skill unchanged")
	errSkillDuplicate = fmt.Errorf("duplicate imported skill")
)

func importSkillFile(ctx context.Context, database *sql.DB, prompts db.PromptStore, path string, canonicalPath string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	content := string(data)
	meta := ParseSkillMeta(content, path)

	// Deterministic ID from the canonical file path so re-imports are idempotent
	// even when the same file is discovered as relative/absolute or via symlink.
	id := promptIDForPath(canonicalPath)

	// Check if already exists
	existing, err := prompts.Get(ctx, runmode.LocalDefaultOrg, id)
	if err != nil {
		return err
	}
	if existing != nil {
		// Update body/name/tools if the file changed
		if existing.Body == meta.Body && existing.Name == meta.Name && existing.AllowedTools == meta.AllowedTools {
			return errSkillUnchanged
		}
		if err := prompts.UpdateImported(ctx, runmode.LocalDefaultOrg, id, meta.Name, meta.Body, meta.AllowedTools); err != nil {
			return err
		}
		log.Printf("[skills] updated %q from %s", meta.Name, path)
		return nil
	}

	// Skip creating a duplicate imported prompt if the same skill body/name is
	// already present from another path. The raw-SQL scan stays here for
	// now — its "find by content fingerprint" semantics don't map cleanly
	// onto PromptStore yet, and that helper migrates in a later wave.
	duplicateID, err := findVisibleImportedPromptByContent(ctx, database, meta.Name, meta.Body, meta.AllowedTools)
	if err != nil {
		return err
	}
	if duplicateID != "" {
		log.Printf("[skills] skipped duplicate %q from %s (already imported as %s)", meta.Name, path, duplicateID)
		return errSkillDuplicate
	}

	prompt := domain.Prompt{
		ID:           id,
		Name:         meta.Name,
		Body:         meta.Body,
		Source:       "imported",
		AllowedTools: meta.AllowedTools,
	}

	if err := prompts.Create(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, prompt); err != nil {
		return err
	}

	log.Printf("[skills] imported %q from %s (description: %s)", meta.Name, path, meta.Description)
	return nil
}

// SkillMeta holds everything extracted from a SKILL.md file's frontmatter.
type SkillMeta struct {
	Name         string
	Description  string
	Body         string
	AllowedTools string // comma-separated tool names from allowed-tools:
}

// ParseSkillMeta extracts name, description, body, and allowed-tools
// from a SKILL.md file. Handles YAML frontmatter between --- markers.
//
// allowed-tools: accepts three shapes:
//   - Inline comma-separated: `allowed-tools: Read, Write, Glob`
//   - YAML list: `allowed-tools:\n  - Bash(git diff:*)\n  - Read`
//   - Inline with colon-containing patterns: `tools: Bash(git diff:*), Glob`
//
// All forms are normalized to a deduplicated comma-separated string.
func ParseSkillMeta(content, path string) SkillMeta {
	// Default name from directory
	dir := filepath.Base(filepath.Dir(path))
	meta := SkillMeta{Name: dir}

	// Split frontmatter
	frontmatter, markdown := splitFrontmatter(content)

	// Parse frontmatter fields
	if frontmatter != "" {
		meta.AllowedTools = parseToolsFrontmatter(frontmatter, "allowed-tools")
		if meta.AllowedTools == "" {
			meta.AllowedTools = parseToolsFrontmatter(frontmatter, "tools")
		}

		for _, line := range strings.Split(frontmatter, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "name:") {
				val := strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
				if val != "" {
					meta.Name = val
				}
			}
			if strings.HasPrefix(trimmed, "description:") {
				meta.Description = strings.TrimSpace(strings.TrimPrefix(trimmed, "description:"))
			}
		}
	}

	// The body is the markdown content (the actual prompt/instructions)
	meta.Body = strings.TrimSpace(markdown)
	if meta.Body == "" {
		meta.Body = content // fallback: use entire file
	}

	return meta
}

// parseToolsFrontmatter extracts tool entries from a frontmatter key
// that can be either inline (comma-separated) or a YAML list (dash-
// prefixed lines). Returns a comma-separated string or "".
func parseToolsFrontmatter(frontmatter, key string) string {
	lines := strings.Split(frontmatter, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, key+":") {
			continue
		}
		// Inline value after the colon.
		val := strings.TrimSpace(strings.TrimPrefix(trimmed, key+":"))
		if val != "" {
			return NormalizeToolList(val)
		}
		// YAML list: subsequent lines starting with "  -" or "- ".
		var items []string
		for j := i + 1; j < len(lines); j++ {
			entry := strings.TrimSpace(lines[j])
			if !strings.HasPrefix(entry, "- ") && !strings.HasPrefix(entry, "-\t") {
				break
			}
			item := strings.TrimSpace(strings.TrimPrefix(entry, "-"))
			if item != "" {
				items = append(items, item)
			}
		}
		if len(items) > 0 {
			return NormalizeToolList(strings.Join(items, ","))
		}
		return ""
	}
	return ""
}

// NormalizeToolList cleans a tool list string: strips YAML quotes,
// splits on commas or spaces-outside-parentheses, trims whitespace
// around each entry, drops empties, and deduplicates.
func NormalizeToolList(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, `"'`)

	// Convert spaces outside parentheses to commas so both
	// "Bash(git diff:*),Read" and "Bash(git:*) Read Glob" normalize
	// the same way. Spaces inside Bash(...) patterns are preserved.
	raw = spaceToCommaOutsideParens(raw)

	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return strings.Join(out, ",")
}

// spaceToCommaOutsideParens replaces spaces that are not inside
// parentheses with commas. This lets "Bash(git diff:*) Read" become
// "Bash(git diff:*),Read" while preserving "git diff" inside the parens.
func spaceToCommaOutsideParens(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
			b.WriteByte(s[i])
		case ')':
			if depth > 0 {
				depth--
			}
			b.WriteByte(s[i])
		case ' ', '\t':
			if depth > 0 {
				b.WriteByte(s[i])
			} else {
				b.WriteByte(',')
			}
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
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

func normalizePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	// Fall back to absolute clean path for non-existent paths, broken links,
	// or permission errors; we still want deterministic identity.
	return filepath.Clean(abs), nil
}

func promptIDForPath(path string) string {
	return fmt.Sprintf("imported-%x", sha256.Sum256([]byte(path)))[:20]
}

func promptFingerprint(name, body, allowedTools string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + body + "\x00" + allowedTools))
	return fmt.Sprintf("%x", sum)
}

func findVisibleImportedPromptByContent(ctx context.Context, database *sql.DB, name, body, allowedTools string) (string, error) {
	var id string
	err := database.QueryRowContext(ctx, `
		SELECT id
		FROM prompts
		WHERE source = 'imported' AND hidden = 0 AND name = ? AND body = ? AND allowed_tools = ?
		ORDER BY updated_at DESC
		LIMIT 1
	`, name, body, allowedTools).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

func hideDuplicateImportedPrompts(ctx context.Context, database *sql.DB, prompts db.PromptStore) (int, error) {
	// Post-SKY-259: prompt_triggers folded into event_handlers with
	// kind='trigger'. Post-SKY-416 a trigger references a BLUEPRINT, not a
	// prompt directly, so "is this prompt referenced by a trigger" routes
	// event_handlers.blueprint_id → blueprint_steps.step_prompt_id = p.id.
	// The joins also match on org_id — every table carries org_id post-
	// SKY-269, and matching it keeps a same-id prompt in another org from
	// being counted as referencing each other.
	//
	// The outer query is also scoped to the local org explicitly:
	// ImportAll is local-mode only today (the skills runner reads
	// ~/.claude/skills which is a per-machine concept), so pinning the
	// SELECT to LocalDefaultOrg keeps the trigger_count tenant-correct
	// here and avoids leaking imported prompts from any other org_id
	// that might exist in the same DB during tests.
	rows, err := database.QueryContext(ctx, `
		SELECT p.id, p.name, p.body, p.allowed_tools, COUNT(t.id) AS trigger_count
		FROM prompts p
		LEFT JOIN blueprint_steps bs
		       ON bs.step_prompt_id = p.id
		LEFT JOIN event_handlers t
		       ON t.blueprint_id = bs.blueprint_id
		      AND t.org_id       = p.org_id
		      AND t.kind         = 'trigger'
		WHERE p.source = 'imported' AND p.hidden = 0
		  AND p.org_id = ?
		GROUP BY p.id, p.name, p.body, p.allowed_tools, p.updated_at, p.created_at
		ORDER BY trigger_count DESC, p.updated_at DESC, p.created_at DESC, p.id ASC
	`, runmode.LocalDefaultOrg)
	if err != nil {
		return 0, err
	}

	seen := make(map[string]struct{})
	var idsToHide []string

	for rows.Next() {
		var (
			id           string
			name         string
			body         string
			allowedTools string
			refs         int
		)
		if err := rows.Scan(&id, &name, &body, &allowedTools, &refs); err != nil {
			_ = rows.Close()
			return 0, err
		}

		fingerprint := promptFingerprint(name, body, allowedTools)
		if _, ok := seen[fingerprint]; !ok {
			seen[fingerprint] = struct{}{}
			continue
		}
		// Keep prompts that are still bound to triggers visible so bindings
		// don't point at hidden prompts. Prompt cleanup stays conservative:
		// we only hide unreferenced duplicates.
		if refs > 0 {
			continue
		}
		idsToHide = append(idsToHide, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	// Close rows before UPDATEs so single-connection SQLite test DBs don't
	// deadlock while this scan is still holding the only open connection.
	if err := rows.Close(); err != nil {
		return 0, err
	}

	hiddenCount := 0
	for _, id := range idsToHide {
		if err := prompts.Hide(ctx, runmode.LocalDefaultOrg, id); err != nil {
			return hiddenCount, err
		}
		hiddenCount++
	}

	return hiddenCount, nil
}
