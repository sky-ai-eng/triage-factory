package skills

import (
	"os"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func importedPrompt(name, body string) *domain.Prompt {
	return &domain.Prompt{Name: name, Body: body, Source: "imported"}
}

func userPrompt(name, body string) *domain.Prompt {
	return &domain.Prompt{Name: name, Body: body, Source: "user"}
}

// imported skill with matching name: → output bytes identical to input.
func TestEnsureFrontmatterName_MatchingName(t *testing.T) {
	slug := "chain-step-0-foo-bar"
	body := "---\nname: chain-step-0-foo-bar\ndescription: does stuff\n---\n\nSome content.\n"
	got := ensureFrontmatterName(body, slug, "Foo Bar", "")
	if got != body {
		t.Errorf("expected body unchanged\ngot:  %q\nwant: %q", got, body)
	}
}

// imported skill with mismatched name: → output has new name: and original key/value preserved.
func TestEnsureFrontmatterName_MismatchedName(t *testing.T) {
	slug := "chain-step-1-my-skill"
	body := "---\nname: old-skill-name\ndescription: does stuff\n---\n\nContent here.\n"
	got := ensureFrontmatterName(body, slug, "My Skill", "")
	if !strings.Contains(got, "name: "+slug) {
		t.Errorf("expected rewritten name %q; got: %q", slug, got)
	}
	if !strings.Contains(got, "description: does stuff") {
		t.Errorf("expected original description preserved; got: %q", got)
	}
	if strings.Contains(got, "name: old-skill-name") {
		t.Errorf("expected old name removed; got: %q", got)
	}
}

// imported skill with frontmatter missing name: → output has valid frontmatter.
func TestEnsureFrontmatterName_MissingName(t *testing.T) {
	slug := "chain-step-2-review"
	body := "---\ndescription: reviews code\n---\n\nDo the review.\n"
	got := ensureFrontmatterName(body, slug, "Review", "")

	if !strings.HasPrefix(got, "---\n") {
		t.Errorf("output must start with ---\\n; got: %q", got)
	}
	if !strings.Contains(got, "name: "+slug) {
		t.Errorf("missing name: %q in output; got: %q", slug, got)
	}
	if !strings.Contains(got, "description: reviews code") {
		t.Errorf("existing key/value not preserved; got: %q", got)
	}
	// Must have a closing --- after the opening one.
	if !strings.Contains(got[4:], "\n---") {
		t.Errorf("no closing --- delimiter found; got: %q", got)
	}
}

// imported skill with NO frontmatter at all → synthesize path; produces ---\nname: ...\n---\n block.
func TestEnsureFrontmatterName_NoFrontmatter(t *testing.T) {
	slug := "chain-step-3-analyze"
	body := "Just plain markdown body.\n"
	got := ensureFrontmatterName(body, slug, "Analyze", "brief desc")

	if !strings.HasPrefix(got, "---\n") {
		t.Errorf("output must start with ---\\n; got: %q", got)
	}
	if !strings.Contains(got, "name: "+slug) {
		t.Errorf("missing name: %q; got: %q", slug, got)
	}
	if !strings.Contains(got, "Just plain markdown body.") {
		t.Errorf("original body not present in output; got: %q", got)
	}
}

// user-sourced prompt → synthesized frontmatter wraps body; body trailing newlines preserved.
func TestSynthesizeSkillFile_TrailingNewlines(t *testing.T) {
	p := userPrompt("Deploy Step", "Do the deployment.\n\n")
	got := synthesizeSkillFile("chain-step-4-deploy-step", p.Name, p.Body, "deploy the thing")

	if !strings.HasSuffix(got, "Do the deployment.\n\n") {
		t.Errorf("trailing newlines stripped; tail: %q", got[max(0, len(got)-40):])
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// SlugForBlueprintStep produces distinct slugs for prompts named "Foo Bar" at index 0 vs "foo-bar" at index 1.
func TestSlugForBlueprintStep_Collision(t *testing.T) {
	slug0 := SlugForBlueprintStep(0, "Foo Bar") // chain-step-0-foo-bar
	slug1 := SlugForBlueprintStep(1, "foo-bar") // chain-step-1-foo-bar
	if slug0 == slug1 {
		t.Errorf("expected distinct slugs; both are %q", slug0)
	}
}

// Full write path: user-sourced prompt materializes a SKILL.md with synthesized frontmatter.
func TestMaterializeStepSkill_UserPrompt(t *testing.T) {
	wt := t.TempDir()
	p := userPrompt("Test Step", "Run the test suite.\n")
	slug := SlugForBlueprintStep(0, p.Name)

	if err := MaterializeStepSkill(wt, slug, p, "run tests"); err != nil {
		t.Fatalf("MaterializeStepSkill: %v", err)
	}

	skillPath := wt + "/.claude/skills/" + slug + "/SKILL.md"
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "---\n") {
		t.Errorf("expected frontmatter block; got: %q", got[:min(len(got), 50)])
	}
	if !strings.Contains(got, "name: "+slug) {
		t.Errorf("name missing from SKILL.md; got: %q", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
