package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestSkillUpload_CreatesImportedPrompt(t *testing.T) {
	s := newTestServer(t)

	content := strings.Join([]string{
		"---",
		"name: release-notes",
		"description: Draft release notes from merged PRs",
		"allowed-tools: Read, Bash(git log:*)",
		"---",
		"",
		"# Release notes",
		"Summarize the merged PRs since the last tag.",
	}, "\n")

	rec := doJSON(t, s, http.MethodPost, "/api/skills/upload", map[string]string{"content": content})
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var created domain.Prompt
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created prompt: %v", err)
	}
	if created.Name != "release-notes" {
		t.Errorf("name = %q, want release-notes (frontmatter)", created.Name)
	}
	if created.Source != "imported" {
		t.Errorf("source = %q, want imported", created.Source)
	}
	if created.AllowedTools != "Read,Bash(git log:*)" {
		t.Errorf("allowed_tools = %q, want normalized frontmatter tools", created.AllowedTools)
	}
	if !strings.Contains(created.Body, "Summarize the merged PRs") {
		t.Errorf("body lost the markdown: %q", created.Body)
	}
	if strings.Contains(created.Body, "allowed-tools") {
		t.Errorf("body should be the post-frontmatter markdown, got %q", created.Body)
	}
}

func TestSkillUpload_RejectsUnnamedAndEmpty(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/skills/upload", map[string]string{"content": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty content = %d, want 400", rec.Code)
	}

	// No frontmatter name and no explicit name → 400 (a pasted skill has
	// no directory to derive a default name from).
	rec = doJSON(t, s, http.MethodPost, "/api/skills/upload", map[string]string{"content": "just some markdown"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unnamed content = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}

	// Explicit name rescues frontmatter-less content.
	rec = doJSON(t, s, http.MethodPost, "/api/skills/upload", map[string]string{
		"content": "just some markdown",
		"name":    "pasted-skill",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("named upload = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var created domain.Prompt
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created prompt: %v", err)
	}
	if created.Name != "pasted-skill" {
		t.Errorf("name = %q, want pasted-skill", created.Name)
	}
}
