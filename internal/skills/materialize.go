package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// yamlQuoteString emits a YAML 1.2 double-quoted flow scalar by way of
// JSON encoding. YAML 1.2 is a strict superset of JSON, so a JSON-encoded
// string is always a valid YAML scalar — this handles embedded colons,
// hashes, quotes, and newlines safely without a YAML dependency.
func yamlQuoteString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// SlugForBlueprintStep produces the directory name used under
// `<wt>/.claude/skills/` for a blueprint step (historically "chain-step-...").
// Including the step index guards against two steps in one blueprint
// referencing the same prompt and overwriting each other's SKILL.md. The slug
// also doubles as the `name:` field of the generated frontmatter.
func SlugForBlueprintStep(stepIndex int, promptName string) string {
	return fmt.Sprintf("chain-step-%d-%s", stepIndex, sanitizeSlug(promptName))
}

var slugSanitizeRE = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	s = slugSanitizeRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "step"
	}
	return s
}

// MaterializeStepSkill writes a SKILL.md for one blueprint step into
// `<wt>/.claude/skills/<slug>/SKILL.md`. Branches on the prompt's
// source so an off-the-shelf imported skill is written byte-identical
// (modulo a name-rewrite when needed) and a regular user/system
// prompt gets synthetic frontmatter so Claude Code's skill discovery
// can index it.
//
//   - source = "imported": the body already carries valid SKILL.md
//     content, often with frontmatter. Write verbatim. Rewrite only
//     the `name:` field when it doesn't match the slug we materialized
//     under (otherwise discovery indexes the wrong name).
//
//   - source = "user" | "system" | other: synthesize frontmatter with
//     `name: <slug>` and `description: <brief or fallback>`, then the
//     prompt body unchanged. The wrapper user prompt names the slug
//     explicitly, so even a weak description still gets the skill
//     selected — the description is a backstop.
//
// The caller is responsible for wiping `<wt>/.claude/skills/` (or at
// least the slug subdirectory) between steps so step N+1 doesn't see
// step N's leftover skill.
//
// This is the LOCAL-mode form: it writes inside the run's worktree, which
// only stays writable to the orchestrator when no sandbox ever took
// ownership of the tree. A sandboxed (multi-mode) run stages the same
// content outside the workspace instead — see StageStepSkill.
func MaterializeStepSkill(worktree, slug string, prompt *domain.Prompt, brief string) error {
	return MaterializeStepSkillInDir(filepath.Join(worktree, ".claude", "skills"), slug, prompt, brief)
}

// StageStepSkill materializes one step's skill into an orchestrator-owned
// staging directory that a sandboxed launch bind-mounts read-only, replacing
// whatever the directory held before.
//
// This is the multi-mode counterpart of MaterializeStepSkill, and the reason
// multi-step blueprints work under privilege separation: the run tree belongs
// to the per-run sandbox uid from the first launch onward, so the
// capability-less orchestrator can neither remove the previous step's SKILL.md
// nor create the next one inside it. Staging moves both operations onto a
// directory the orchestrator owns outright — the "wipe" is a RemoveAll of its
// own dir, trivially permitted.
//
// Recreating the directory rather than writing into it is also what makes step
// isolation structural: the staged dir holds exactly one skill, so a launch
// that mounts it cannot expose a sibling step's SKILL.md even if a prior
// attempt left one behind. Content is byte-identical to the in-worktree form —
// same synthesis, different root.
func StageStepSkill(stagingDir, slug string, prompt *domain.Prompt, brief string) error {
	if prompt == nil {
		return fmt.Errorf("nil prompt")
	}
	if err := RemoveStagedSkills(stagingDir); err != nil {
		return fmt.Errorf("wipe staging dir: %w", err)
	}
	return MaterializeStepSkillInDir(stagingDir, slug, prompt, brief)
}

// RemoveStagedSkills removes a staging directory created by StageStepSkill.
// A missing directory is success. Safe for the capability-less orchestrator:
// the staging dir is its own, never chowned to a sandbox identity.
func RemoveStagedSkills(stagingDir string) error {
	if stagingDir == "" {
		return nil
	}
	if err := os.RemoveAll(stagingDir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// MaterializeStepSkillInDir writes `<skillsDir>/<slug>/SKILL.md` with the
// content synthesis MaterializeStepSkill documents. skillsDir is the root a
// skill-discovery pass indexes — the worktree's `.claude/skills` in local mode,
// a per-run staging dir bind-mounted into the jail in multi mode.
func MaterializeStepSkillInDir(skillsDir, slug string, prompt *domain.Prompt, brief string) error {
	if prompt == nil {
		return fmt.Errorf("nil prompt")
	}
	dir := filepath.Join(skillsDir, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}
	path := filepath.Join(dir, "SKILL.md")

	var contents string
	if prompt.Source == "imported" {
		contents = ensureFrontmatterName(prompt.Body, slug, prompt.Name, brief)
	} else {
		contents = synthesizeSkillFile(slug, prompt.Name, prompt.Body, brief)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}
	return nil
}

// ensureFrontmatterName takes an imported SKILL.md body and rewrites
// (or inserts) the `name:` field so it matches the slug we materialized
// under. Description is preserved when present; a fallback is inserted
// when the imported body has no frontmatter at all.
func ensureFrontmatterName(body, slug, promptName, brief string) string {
	frontmatter, markdown := splitFrontmatter(body)
	if frontmatter == "" {
		// No frontmatter — wrap the body in a synthesized one. This
		// handles imported prompts whose body is the markdown payload
		// only (the importer's importSkillFile may have already stripped
		// frontmatter when the caller stored the parsed body).
		return synthesizeSkillFile(slug, promptName, body, brief)
	}
	lines := strings.Split(frontmatter, "\n")
	hasName := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "name:") {
			// Fix 1: if the existing name already matches, return the body unchanged.
			existing := strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
			if existing == slug {
				return body
			}
			lines[i] = "name: " + slug
			hasName = true
			break
		}
	}
	if !hasName {
		// name: goes first in the frontmatter content; splitFrontmatter already
		// strips the surrounding "---" delimiters, so a plain prepend is correct.
		lines = append([]string{"name: " + slug}, lines...)
	}
	rebuilt := strings.Join(lines, "\n")
	return "---\n" + strings.TrimSpace(rebuilt) + "\n---\n\n" + strings.TrimLeft(markdown, "\n")
}

func synthesizeSkillFile(slug, promptName, body, brief string) string {
	desc := strings.TrimSpace(brief)
	if desc == "" {
		desc = fmt.Sprintf("Run the %q step in this blueprint.", strings.TrimSpace(promptName))
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: ")
	b.WriteString(slug)
	b.WriteString("\n")
	b.WriteString("description: ")
	b.WriteString(yamlQuoteString(desc))
	b.WriteString("\n---\n\n")
	if strings.TrimSpace(promptName) != "" {
		b.WriteString("<!-- Sourced from prompt: ")
		b.WriteString(strings.TrimSpace(promptName))
		b.WriteString(" -->\n\n")
	}
	// Fix 3: do not trim trailing whitespace; preserve body byte-identical.
	b.WriteString(body)
	return b.String()
}

// WipeBlueprintSkills removes the blueprint step skill directories so step
// N+1 doesn't see step N's SKILL.md. The whole `.claude/skills/`
// directory is wiped — blueprints don't compose with the curator skill
// materialization (blueprints run on PRs/Jira, the curator runs on
// projects), so collateral damage to other materialized skills is
// not a concern in this code path.
//
// LOCAL mode only, paired with MaterializeStepSkill. A sandboxed run's tree is
// owned by the sandbox uid after its first launch, so this unlink would fail
// with EACCES at every step boundary; that path stages outside the workspace
// and wipes by recreating its own staging dir (StageStepSkill).
func WipeBlueprintSkills(worktree string) error {
	dir := filepath.Join(worktree, ".claude", "skills")
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
