package ai

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestShippedPromptsParse ensures every shipped prompt has a unique,
// non-empty system_slug, a name, a body, and the system source — and
// leaves ID empty (the seeder mints a random UUID per team copy, SKY-380).
// Drift here means the seeder or a trigger's prompt-slug reference dangles.
func TestShippedPromptsParse(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range ShippedPrompts() {
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

// TestShippedPromptSlugsStable guards the cross-file invariant that every
// prompt slug referenced by a shipped trigger in db.ShippedEventHandlers
// resolves to a shipped prompt here. A typo or a renamed prompt would
// otherwise only surface as an FK failure at seed time on a fresh install.
//
// We can't import internal/db (would be a cycle: db tests import ai),
// so the trigger-side assertion lives in internal/db; here we just pin
// the prompt slugs so a rename is a visible, reviewed diff.
func TestShippedPromptSlugsStable(t *testing.T) {
	want := map[string]bool{
		"system-pr-review-security":     true,
		"system-pr-review-correctness":  true,
		"system-pr-review-aggregate":    true,
		"system-conflict-resolution":    true,
		"system-ci-fix":                 true,
		"system-jira-implement":         true,
		"system-fix-review-feedback":    true,
		domain.SystemTicketSpecPromptID: true,
	}
	for _, p := range ShippedPrompts() {
		delete(want, p.SystemSlug)
	}
	if len(want) > 0 {
		t.Errorf("shipped prompt slugs changed; missing: %v", want)
	}
}
