package skills

import "testing"

func TestParseToolsFrontmatter_InlineCommaSeparated(t *testing.T) {
	fm := `name: review-code
tools: Bash(git diff:*), Glob, Read, mcp__acme-docs__search_api`
	got := parseToolsFrontmatter(fm, "tools")
	want := "Bash(git diff:*),Glob,Read,mcp__acme-docs__search_api"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseToolsFrontmatter_YAMLList(t *testing.T) {
	fm := `name: review-pr
allowed-tools:
  - Bash(git diff:*)
  - Read
  - Agent
  - mcp__widget-srv__get_schema`
	got := parseToolsFrontmatter(fm, "allowed-tools")
	want := "Bash(git diff:*),Read,Agent,mcp__widget-srv__get_schema"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseToolsFrontmatter_Empty(t *testing.T) {
	fm := `name: simple
description: no tools`
	got := parseToolsFrontmatter(fm, "allowed-tools")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestParseToolsFrontmatter_MissingKey(t *testing.T) {
	fm := `name: test
description: testing`
	got := parseToolsFrontmatter(fm, "tools")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestParseSkillMeta_AllowedTools(t *testing.T) {
	content := `---
name: review-pr
description: PR review
allowed-tools:
  - Bash(git diff:*)
  - Read
  - mcp__acme-docs__search_api
---

Review the PR carefully.`

	meta := ParseSkillMeta(content, "/skills/review-pr/SKILL.md")
	if meta.Name != "review-pr" {
		t.Errorf("name = %q, want review-pr", meta.Name)
	}
	want := "Bash(git diff:*),Read,mcp__acme-docs__search_api"
	if meta.AllowedTools != want {
		t.Errorf("AllowedTools = %q, want %q", meta.AllowedTools, want)
	}
	if meta.Body != "Review the PR carefully." {
		t.Errorf("Body = %q", meta.Body)
	}
}

func TestParseSkillMeta_FallsBackToToolsKey(t *testing.T) {
	content := `---
name: agent-def
tools: Glob, Grep, Read, mcp__widget-srv__list_schemas
---

Do the thing.`

	meta := ParseSkillMeta(content, "/agents/test/SKILL.md")
	want := "Glob,Grep,Read,mcp__widget-srv__list_schemas"
	if meta.AllowedTools != want {
		t.Errorf("AllowedTools = %q, want %q", meta.AllowedTools, want)
	}
}

func TestParseSkillMeta_NoFrontmatter(t *testing.T) {
	content := "Just a plain skill body with no frontmatter."
	meta := ParseSkillMeta(content, "/skills/plain/SKILL.md")
	if meta.AllowedTools != "" {
		t.Errorf("expected empty AllowedTools, got %q", meta.AllowedTools)
	}
	if meta.Name != "plain" {
		t.Errorf("name = %q, want plain", meta.Name)
	}
}

func TestNormalizeToolList(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Read, Write, Glob", "Read,Write,Glob"},
		{"Read, Read, Glob", "Read,Glob"},
		{" ", ""},
		{",,Read,,", "Read"},
		// YAML-quoted value
		{`"Read, Grep, Glob, Bash(git:*)"`, "Read,Grep,Glob,Bash(git:*)"},
		// Space-delimited (no commas), spaces inside parens preserved
		{"Bash(git:*) Bash(gh:*) Read Grep Glob", "Bash(git:*),Bash(gh:*),Read,Grep,Glob"},
		// Mixed: commas + spaces-inside-parens
		{"Bash(git diff:*), Read, Agent", "Bash(git diff:*),Read,Agent"},
		// Space-delimited with spaces inside parens
		{"Bash(git diff:*) Read Agent", "Bash(git diff:*),Read,Agent"},
		// Single-quoted YAML value
		{"'Read, Write'", "Read,Write"},
	}
	for _, tt := range tests {
		got := NormalizeToolList(tt.input)
		if got != tt.want {
			t.Errorf("NormalizeToolList(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestImportAll_PersistsAllowedTools(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	chdir(t, t.TempDir())

	content := `---
name: mcp-skill
allowed-tools:
  - mcp__acme-docs__search_api
  - mcp__widget-srv__get_schema
---

Use MCP tools to look things up.`

	writeSkillFile(t, home, "mcp-skill", content)

	database := newTestDB(t)
	result := ImportAll(t.Context(), database, testPromptStore(database))
	if len(result.Errors) > 0 {
		t.Fatalf("ImportAll errors: %v", result.Errors)
	}
	if result.Imported != 1 {
		t.Fatalf("expected 1 imported, got %d", result.Imported)
	}

	var allowedTools string
	if err := database.QueryRow(`
		SELECT allowed_tools FROM prompts
		WHERE source = 'imported' AND hidden = 0
		LIMIT 1
	`).Scan(&allowedTools); err != nil {
		t.Fatalf("query allowed_tools: %v", err)
	}
	want := "mcp__acme-docs__search_api,mcp__widget-srv__get_schema"
	if allowedTools != want {
		t.Errorf("persisted allowed_tools = %q, want %q", allowedTools, want)
	}
}
