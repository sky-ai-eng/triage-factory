package worktree

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// skillsLinkRel is the path, relative to a run tree, where the agent's engine
// looks for personal skills. In-jail HOME is /work — the run tree's bind-mount
// destination — so the engine's ~/.claude/skills IS this path inside the tree.
var skillsLinkRel = filepath.Join(".claude", "skills")

// EnsureSandboxSkillsLink plants `<dir>/.claude/skills` as a symlink to the
// jail's read-only step-skill mount point, so a sandboxed agent discovers its
// step's SKILL.md through the engine's ordinary personal-skills path without TF
// ever writing a skill into the workspace.
//
// Called at the two moments a run tree is orchestrator-owned: worktree BUILD and
// snapshot RESTORE, both strictly before the launch-time chown hands the tree to
// the per-run sandbox uid. After that hand-off the capability-less orchestrator
// cannot write inside the tree at all, which is precisely why the step skill
// itself no longer lives there — planting once, up front, is what buys every
// later step boundary a zero-write setup.
//
// Force-replaces whatever occupies the path (a real directory from a pre-fix
// snapshot's patch, a stale symlink with a different target), and no-ops when
// the correct symlink is already there — so a warm tree reused across steps
// takes no write at all. TF owns `<tree>/.claude/skills` outright, the same
// contract WipeBlueprintSkills has always had for the local path.
//
// No-op unless this host actually sandboxes: local mode keeps the step skill in
// the workspace (the orchestrator owns the tree there, and released local
// behavior is deliberately untouched), and there is no mount to point at.
//
// An agent that later deletes or replaces the symlink only sabotages its own
// skill discovery — it could already delete its SKILL.md — and cannot affect
// mount placement, which is fixed at spec time regardless of anything in the
// tree.
func EnsureSandboxSkillsLink(dir string) error {
	if dir == "" || !agentproc.WillSandbox() {
		return nil
	}
	link := filepath.Join(dir, skillsLinkRel)
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			if target, rerr := os.Readlink(link); rerr == nil && target == sandbox.TrustedSkillsDestination {
				return nil // already correct — take no write on a warm tree
			}
		}
		if err := os.RemoveAll(link); err != nil {
			return fmt.Errorf("clear %s: %w", skillsLinkRel, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", skillsLinkRel, err)
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}
	if err := os.Symlink(sandbox.TrustedSkillsDestination, link); err != nil {
		return fmt.Errorf("link %s: %w", skillsLinkRel, err)
	}
	return nil
}

// plantSandboxSkillsLink is the best-effort wrapper every tree-materializing
// path calls. A failure is logged rather than failing the run: the link only
// affects skill discovery, and the wrapper prompt names the skill's in-jail path
// explicitly, so a blueprint step degrades to reading the file by path rather
// than dying at setup. The log line is the diagnostic for the systemic case
// (e.g. `.claude` present as a regular file).
func plantSandboxSkillsLink(dir string) {
	if err := EnsureSandboxSkillsLink(dir); err != nil {
		worktreeLog.Warn("plant sandbox skills symlink failed; a blueprint step in this tree will not discover its skill through ~/.claude/skills", "dir", dir, "error", err)
	}
}
