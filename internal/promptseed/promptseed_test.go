package promptseed

import (
	"regexp"
	"strings"
	"testing"
)

// TestShippedPromptsParse ensures every shipped prompt has a unique,
// non-empty system_slug, a name, a body, and the system source — and
// leaves ID empty (the seeder mints a random UUID per team copy).
// Drift here means the seeder or a trigger's prompt-slug reference dangles.
func TestShippedPromptsParse(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Prompts() {
		if p.SystemSlug == "" {
			t.Errorf("shipped prompt has empty SystemSlug: %+v", p)
		}
		if p.ID != "" {
			t.Errorf("shipped prompt %s should leave ID empty (seeder mints it); got %q", p.SystemSlug, p.ID)
		}
		if seen[p.SystemSlug] {
			t.Errorf("duplicate shipped prompt system_slug: %s", p.SystemSlug)
		}
		seen[p.SystemSlug] = true
		if strings.TrimSpace(p.Body) == "" {
			t.Errorf("shipped prompt %s has empty body", p.SystemSlug)
		}
		if p.Source != "system" {
			t.Errorf("shipped prompt %s has non-system source %q", p.SystemSlug, p.Source)
		}
	}
}

// promptHole is the `{{SCREAMING_CASE}}` shape that reads as a slot something
// is expected to fill. Braces around lowercase are left alone because they are
// content elsewhere — the Jira formatting guidance teaches `{{monospace}}`,
// which is Jira's own markup and belongs in front of the model verbatim.
var promptHole = regexp.MustCompile(`\{\{[A-Z][A-Z0-9_]*\}\}`)

// TestShippedPromptsAreLiteral holds the defaults to what a user can write.
// A prompt row is user-editable, and what a user types is what the model reads,
// so a shipped body that left a slot for something to fill would be an example
// of a capability nobody could reproduce — and, with nothing filling it, brace
// syntax rendered at the model.
//
// Everything a body needs is already addressable in prose: the task context
// names the pull request and the event's own fields, the run context names the
// run root and the branch convention, and `triagefactory exec …` resolves to the
// run's binary.
func TestShippedPromptsAreLiteral(t *testing.T) {
	for _, p := range Prompts() {
		if hole := promptHole.FindString(p.Body); hole != "" {
			t.Errorf("shipped prompt %s leaves %s for something to fill; nothing does", p.SystemSlug, hole)
		}
	}
}

// TestShippedPromptSlugsStable guards the cross-file invariant that every
// prompt slug referenced by a shipped trigger in db.ShippedEventHandlers
// resolves to a shipped prompt here. A typo or a renamed prompt would
// otherwise only surface as an FK failure at seed time on a fresh install.
//
// We can't import internal/db (would be a cycle: db tests import this
// package), so the trigger-side assertion lives in internal/db; here we
// just pin the prompt slugs so a rename is a visible, reviewed diff.
func TestShippedPromptSlugsStable(t *testing.T) {
	want := map[string]bool{
		"system-pr-review-security":    true,
		"system-pr-review-correctness": true,
		"system-pr-review-aggregate":   true,
		"system-conflict-resolution":   true,
		"system-ci-fix":                true,
		"system-jira-implement":        true,
		"system-fix-review-feedback":   true,
	}
	for _, p := range Prompts() {
		delete(want, p.SystemSlug)
	}
	if len(want) > 0 {
		t.Errorf("shipped prompt slugs changed; missing: %v", want)
	}
}
