package skills

import (
	"os"
	"path/filepath"
	"strings"
)

// ScanAgentTools reads all agent definitions from ~/.claude/agents/*.md
// and collects their declared tools. Returns a deduplicated, comma-
// separated string suitable for merging into --allowedTools — or ""
// if no agent files exist or none declare tools.
func ScanAgentTools() string {
	// ~/.claude/agents is Claude Code user state keyed to the real HOME,
	// not TF state — it stays home-relative and does not route through
	// internal/paths.
	home, err := os.UserHomeDir() //nolint:forbidigo // Claude Code user state, not TF state (see internal/paths doc).
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".claude", "agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	var parts []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		frontmatter, _ := splitFrontmatter(string(data))
		if frontmatter == "" {
			continue
		}
		tools := parseToolsFrontmatter(frontmatter, "tools")
		if tools != "" {
			parts = append(parts, tools)
		}
	}
	return NormalizeToolList(strings.Join(parts, ","))
}
