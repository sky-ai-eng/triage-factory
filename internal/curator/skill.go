package curator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
)

// specSkillDirName is the per-session subdirectory Claude Code scans for
// project-scoped skills. Claude Code globs `<cwd>/.claude/skills/*/SKILL.md`
// at session start and (best-effort) on resume, so writing into this
// path before each dispatch is what makes the per-project ticket-spec
// guidance available to the model as a discoverable skill.
const specSkillDirName = "ticket-spec"

const jiraFormattingSkillDirName = "jira-formatting"

// jiraFormattingDescription is the trigger sentence Claude Code reads to decide
// whether the skill applies to the current turn. It stays in code rather than
// in the prompt row for the same reason the ticket-spec description does: the
// user edits *what* good Jira markup means, not the Claude-Code-flavored
// descriptor that gets the skill selected.
const jiraFormattingDescription = "Format Jira descriptions and comments with Jira wiki markup whenever you draft or edit Jira content."

// materializeSpecSkill writes <cwd>/.claude/skills/<specSkillDirName>/SKILL.md
// containing the body of the project's effective spec-authorship prompt.
// Resolution order: project's `spec_authorship_blueprint_id` → its single
// step → that step's prompt, then the seeded `domain.SystemTicketSpecPromptID`
// blueprint. Either path falling through to a missing/empty prompt logs a
// warning and removes any prior
// SKILL.md rather than failing the dispatch — the Curator should still
// answer the user's message even without the skill, and a stale file
// from a previous resolution would otherwise keep feeding the agent
// out-of-date guidance.
//
// We always overwrite. The prompt body can change between turns (the
// user edits it on the Prompts page or swaps which prompt the project
// points at) and the user expects the Curator's next turn to pick up
// the change without a session reset. Writing fresh on every dispatch
// is the cheapest way to honor that.
//
// orgID + creatorUserID identify the requesting user — the prompt
// lookup runs inside SyntheticClaimsWithTx so multi-mode RLS reads
// stay attributed to the user that triggered the turn.
func materializeSpecSkill(ctx context.Context, stores db.Stores, orgID, creatorUserID string, project *domain.Project, cwd string) error {
	if project == nil {
		return nil
	}
	prompt, err := resolveSpecPrompt(ctx, stores, orgID, creatorUserID, project)
	if err != nil {
		return err
	}
	dir := filepath.Join(cwd, ".claude", "skills", specSkillDirName)
	path := filepath.Join(dir, "SKILL.md")

	if prompt == nil || strings.TrimSpace(prompt.Body) == "" {
		// No usable prompt — clear any stale SKILL.md from a previous
		// dispatch so the agent doesn't keep applying guidance that no
		// longer matches the project's current configuration.
		curatorLog.Warn("no spec-authorship prompt resolved; clearing stale skill if any", "project", project.ID)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale SKILL.md: %w", err)
		}
		return nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}
	contents := renderSkillFile(prompt.Name, prompt.Body)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}
	return nil
}

// materializeJiraFormattingSkill writes an always-on skill that teaches the
// Curator Jira-flavored markup (h2., inline code, {code} blocks, etc.),
// resolved from the team's seeded `system-jira-formatting` prompt row.
//
// It is a row rather than an embedded file so a team whose Jira conventions
// differ can fix it on the Prompts page — shipping a default nobody can see or
// change is how guidance quietly goes stale. Unlike ticket-spec no project
// points at it, so resolution is a direct slug lookup with no blueprint hop.
//
// A missing or empty row clears any stale SKILL.md rather than failing the
// dispatch, matching materializeSpecSkill: the Curator should still answer the
// user even when its guidance can't be resolved.
func materializeJiraFormattingSkill(ctx context.Context, stores db.Stores, orgID, creatorUserID string, project *domain.Project, cwd string) error {
	if project == nil {
		return nil
	}
	prompt, err := resolveJiraFormattingPrompt(ctx, stores, orgID, creatorUserID, project)
	if err != nil {
		return err
	}
	dir := filepath.Join(cwd, ".claude", "skills", jiraFormattingSkillDirName)
	path := filepath.Join(dir, "SKILL.md")

	if prompt == nil || strings.TrimSpace(prompt.Body) == "" {
		curatorLog.Warn("no jira-formatting prompt resolved; clearing stale skill if any", "project", project.ID)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale jira formatting SKILL.md: %w", err)
		}
		return nil
	}

	if err := skills.MaterializeStepSkillInDir(
		filepath.Join(cwd, ".claude", "skills"),
		jiraFormattingSkillDirName,
		prompt,
		jiraFormattingDescription,
	); err != nil {
		return fmt.Errorf("write jira formatting SKILL.md: %w", err)
	}
	return nil
}

// resolveJiraFormattingPrompt loads the team's copy of the seeded
// `system-jira-formatting` prompt. Read inside SyntheticClaimsWithTx so
// multi-mode RLS attributes it to the user whose turn triggered the dispatch,
// same as resolveSpecPrompt.
func resolveJiraFormattingPrompt(ctx context.Context, stores db.Stores, orgID, creatorUserID string, project *domain.Project) (*domain.Prompt, error) {
	var result *domain.Prompt
	err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, creatorUserID, func(ts db.TxStores) error {
		p, err := ts.Prompts.GetBySystemSlug(ctx, orgID, project.TeamID, domain.SystemJiraFormattingPromptID)
		if err != nil {
			return fmt.Errorf("load jira formatting prompt: %w", err)
		}
		result = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// resolveSpecPrompt returns the prompt backing the project's chosen spec
// blueprint (or the seeded default blueprint). The extra hop vs. the
// pre-blueprint code: resolve the blueprint, take its first step, then load
// that step's prompt. A NULL/empty pointer on the project, or a pointer to a
// deleted blueprint, both fall through to the default — the schema's
// ON DELETE SET NULL drops dangling FKs but a stale in-memory project row
// could still carry the deleted id, so we re-check.
//
// Reads happen inside SyntheticClaimsWithTx so multi-mode RLS gates on
// (org_id = tf.current_org_id() AND tf.user_has_org_access). The lookups are
// read-only; the tx exists purely to set claims for those calls.
func resolveSpecPrompt(ctx context.Context, stores db.Stores, orgID, creatorUserID string, project *domain.Project) (*domain.Prompt, error) {
	var result *domain.Prompt
	err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, creatorUserID, func(ts db.TxStores) error {
		// Resolve the project's spec blueprint: the configured id first, else
		// the team's seeded default by system_slug (its id is a random UUID
		// per team copy).
		var bp *domain.Blueprint
		if project.SpecAuthorshipBlueprintID != "" {
			b, err := ts.Blueprints.Get(ctx, orgID, project.SpecAuthorshipBlueprintID)
			if err != nil {
				return fmt.Errorf("load configured spec blueprint: %w", err)
			}
			if b != nil {
				bp = b
			} else {
				curatorLog.Warn("project references missing spec blueprint; falling back to system default",
					"project", project.ID, "blueprint", project.SpecAuthorshipBlueprintID)
			}
		}
		if bp == nil {
			b, err := ts.Blueprints.GetBySystemSlug(ctx, orgID, project.TeamID, domain.SystemTicketSpecPromptID)
			if err != nil {
				return fmt.Errorf("load default spec blueprint: %w", err)
			}
			bp = b
		}
		if bp == nil {
			return nil // no blueprint resolved; caller clears any stale skill
		}

		// First step's prompt is the spec-authorship content.
		steps, err := ts.Blueprints.ListSteps(ctx, orgID, bp.ID)
		if err != nil {
			return fmt.Errorf("load spec blueprint steps: %w", err)
		}
		if len(steps) == 0 {
			curatorLog.Warn("spec blueprint has no steps; clearing stale skill if any", "blueprint", bp.ID)
			return nil
		}
		p, err := ts.Prompts.Get(ctx, orgID, steps[0].StepPromptID)
		if err != nil {
			return fmt.Errorf("load spec step prompt: %w", err)
		}
		result = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// renderSkillFile wraps the prompt body in the YAML frontmatter shape
// Claude Code expects (`name:` + `description:`). The description is a
// short trigger sentence — it's what the model reads to decide whether
// the skill is relevant to the current turn, not the full guidance.
//
// We keep the description constant rather than deriving it from the
// prompt body so swapping prompts doesn't break the model's discovery
// heuristic — the user can edit *what* a well-specced ticket means
// without also having to write a Claude-Code-flavored skill descriptor.
func renderSkillFile(promptName, body string) string {
	const description = "Author a software ticket / spec that a human reviewer can skim and an autonomous coding agent can execute. Use whenever the user asks to draft a ticket, issue, spec, or work item for engineering work."
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: ")
	b.WriteString(specSkillDirName)
	b.WriteString("\n")
	b.WriteString("description: ")
	b.WriteString(description)
	b.WriteString("\n---\n\n")
	if strings.TrimSpace(promptName) != "" {
		b.WriteString("<!-- Sourced from prompt: ")
		b.WriteString(strings.TrimSpace(promptName))
		b.WriteString(" -->\n\n")
	}
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n")
	return b.String()
}
