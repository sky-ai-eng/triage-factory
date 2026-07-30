package sandbox

import "os"

// CaptureChildEnv is the minimal environment for the capture child. It
// deliberately does NOT inherit this process's env — the orchestrator's holds
// DB passwords and service tokens the child (running attacker-influenceable
// git) must never see. It carries only what git needs to run locally.
//
// It also refuses to read any user/global/system git config: GIT_CONFIG_GLOBAL
// and GIT_CONFIG_SYSTEM point at /dev/null so git ignores ~/.gitconfig, the XDG
// global config, and /etc/gitconfig, and HOME is a non-existent path so nothing
// resolves through it either. Without this, HOME pointing at a shared writable
// directory (/tmp) would let anyone plant /tmp/.gitconfig — a filter, an
// include.path — and make the capture attacker-influenceable and
// non-deterministic across runs. Only the run root's own repo config is read.
// On top of that, config overrides neuter the two attribute-free code-exec keys
// (core.fsmonitor, diff.external) as defense in depth; the uid drop, not these,
// is the actual boundary.
//
// Exported, and platform-neutral even though only the Linux capture path spawns
// the child, because this env is the contract between the two halves of the
// capture — the privileged spawner here and the unprivileged body it re-execs
// (cmd/snapshotcapture, via worktree.CaptureWorkspaceGit) — so that body's own
// tests can run git under the exact environment production hands it rather than
// a stand-in that drifts from it.
func CaptureChildEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"), // locate the git binary
		"HOME=/nonexistent",         // no user config from a shared/writable HOME
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=",
		// diff.external is a TRIPWIRE, not the protection. git has no value
		// meaning "disabled" for it — any non-empty value is a command git runs,
		// and an empty one it reads as the command "", which it tries to exec for
		// every changed path — so a `git diff` that reaches this key dies loudly
		// instead of quietly diffing through an attacker's program. Unsetting the
		// key rather than emptying it would just hand a flag-less diff whatever
		// diff.external the agent wrote into the run root's own (agent-writable)
		// config, which IS read. What actually protects the capture's diffs is
		// --no-ext-diff --no-textconv at the invocation, which no config can
		// shadow; this entry is here so a future diff added WITHOUT those flags
		// fails visibly rather than silently trusting repo config.
		"GIT_CONFIG_KEY_1=diff.external", "GIT_CONFIG_VALUE_1=",
	}
}
