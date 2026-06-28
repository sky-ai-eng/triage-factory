package delegate

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestBuildPrompt_InterpolatesInjectedSections guards against the single-pass
// strings.Replacer foot-gun: the tools/scope sections are injected into the
// envelope, and a section injected as a *replacement value* would keep its own
// placeholders verbatim because strings.Replacer does not re-scan replacements.
// buildPrompt inlines those sections before the placeholder pass, so the tools
// docs' {{BINARY_PATH}} and run-root memory paths must come out fully expanded.
func TestBuildPrompt_InterpolatesInjectedSections(t *testing.T) {
	task := domain.Task{
		Title:          "review PR #1",
		EventType:      domain.EventGitHubPRReviewRequested,
		EntitySource:   "github",
		EntitySourceID: "owner/repo#1",
	}
	toolsRef := ai.GHToolsTemplate + "\n\n" + ai.JiraToolsTemplate

	out := buildPrompt(task, "", "mission body", "Repository: owner/repo", toolsRef,
		"/usr/local/bin/triagefactory", "run-1", "/work", "bp-run-1", "tfac/SKY-9")

	if strings.Contains(out, "{{BINARY_PATH}}") {
		t.Error("literal {{BINARY_PATH}} survived in the composed prompt (tools section not interpolated)")
	}
	if strings.Contains(out, "{{TOOLS_REFERENCE}}") || strings.Contains(out, "{{SCOPE}}") || strings.Contains(out, "{{BRANCH_TEMPLATE}}") {
		t.Error("an injected section placeholder survived in the composed prompt")
	}
	// The branch template (already ticket-id-resolved) must surface in the
	// envelope guidance.
	if !strings.Contains(out, "tfac/SKY-9") {
		t.Error("expected the resolved branch template in the composed prompt envelope")
	}
	// The env-var-style run-root reference in the tools docs must be expanded
	// to the concrete agent-visible path, not left for (absent) shell expansion.
	if strings.Contains(out, "$TRIAGE_FACTORY_RUN_ROOT") {
		t.Error("literal $TRIAGE_FACTORY_RUN_ROOT survived in the composed prompt")
	}
	if strings.Contains(out, "$TRIAGE_FACTORY_BLUEPRINT_RUN_ID") {
		t.Error("literal $TRIAGE_FACTORY_BLUEPRINT_RUN_ID survived in the composed prompt")
	}
	// The concrete binary path from the tools docs should be present.
	if !strings.Contains(out, "/usr/local/bin/triagefactory exec gh") {
		t.Error("expected interpolated binary path in the tools section")
	}
	// The entity-memory write path must resolve to the concrete absolute path.
	if !strings.Contains(out, "/work/_scratch/entity-memory/bp-run-1/run-1.md") {
		t.Errorf("expected concrete entity-memory write path in the composed prompt;\n%s", out)
	}
}
