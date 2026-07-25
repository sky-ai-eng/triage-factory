package sandbox

import (
	"os"
	"path/filepath"
)

// The trusted-mount classification for a blueprint step's SKILL.md.
//
// A step skill is TF-side content the agent only ever reads, so it follows the
// standing rule for everything TF hands a jail: it arrives as a read-only bind
// mount from an orchestrator-owned directory, not as a file TF writes into the
// agent's workspace. That is what makes a multi-step blueprint work at all
// under privilege separation — the run tree belongs to the per-run sandbox uid
// from the first launch onward, and the capability-less orchestrator cannot
// write inside it at the next step boundary.
//
// Unlike the SDK dir or the git-hooks dir, the broker does not CREATE this
// source (the orchestrator stages it before the launch RPC), so — exactly like
// the agenthost socket and the gh-injector cert — the broker VALIDATES a
// launch's claimed source against its own derivation rather than overriding it.
// Both sides share the derivation below: the staging producer
// (internal/delegate) imports this package, so unlike the RunTreeRoot /
// agenthost-socket cases there is no reverse-import cycle forcing a duplicated
// literal, and therefore no drift to guard against.

// TrustedSkillsDestination is the fixed in-sandbox path the per-launch step
// skill directory is bind-mounted at, read-only. It joins /opt/tf/bin as a
// rootfs-absolute TF-owned location, and deliberately does NOT live under
// /work: the worktree's contents are agent-authored between blueprint steps,
// and an agent-planted symlink at an in-worktree mount destination could
// redirect the mount elsewhere in the jail (e.g. shadowing /opt/tf/bin and
// defeating the pinned-gh PATH property). Mount-point synthesis on the
// read-only rootfs is already proven by the /opt/tf/bin mounts.
//
// The agent discovers what is mounted here through its ordinary personal-skill
// path: in-jail HOME is /work, so <worktree>/.claude/skills is ~/.claude/skills,
// and the worktree carries a symlink there pointing at this path (planted at
// worktree build / snapshot restore, while the tree is still
// orchestrator-owned — see internal/worktree.EnsureSandboxSkillsLink).
const TrustedSkillsDestination = "/opt/tf/skills"

// skillStagingBasename is the directory per-run step-skill staging dirs live
// under inside os.TempDir(): os.TempDir()/triagefactory-skills/<runID>. A
// deliberate sibling of runTreeBasename's triagefactory-runs, with the opposite
// ownership contract: a run tree is handed to the sandbox uid at launch, a
// staging dir is NEVER chowned and never appears on the host inside anything
// the agent can reach.
const skillStagingBasename = "triagefactory-skills"

// SkillStagingBase returns the parent of every per-run staging dir. The startup
// orphan sweep reads it to reclaim staging dirs whose runs are gone.
func SkillStagingBase() string {
	return filepath.Join(os.TempDir(), skillStagingBasename)
}

// TrustedSkillsSourcePath returns the host path of runID's own step-skill
// staging dir — both what the orchestrator stages into and what the broker
// requires a TrustedSkillsDestination mount's source to resolve to. (The broker
// re-derives it from the symlink-resolved SkillStagingBase so the per-run
// component must be a real directory; see validateWorktreeAndMounts.) Keying on
// the run id (a blueprint step's own conversation id, which is distinct per
// step) is what makes "step N sees only its own SKILL.md" true by construction:
// each launch mounts a directory holding exactly one skill.
func TrustedSkillsSourcePath(runID string) string {
	return filepath.Join(SkillStagingBase(), runID)
}
