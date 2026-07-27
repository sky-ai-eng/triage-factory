// Package githooks owns the TF-controlled git hooks directory and the
// plumbing that installs it as git's core.hooksPath for every delegated
// agent run — process-scoped, so the hooks fire on every git operation
// the agent performs, in both run modes, any cwd, including repos the
// agent clones into subdirs itself (F2, TFAC-456).
//
// This package delivers the install mechanism, the directory, and the
// run-context convention only. It ships no hooks: the first one
// (pre-push branch capture) lands with A·3, and core.hooksPath pointing
// at a directory with no matching hook is a git no-op, so the wiring is
// safe to land on its own.
//
// Two install paths, one convention:
//
//   - Sandbox (multi): the host owns HostDir(), TF bind-mounts it
//     read-only into the rootfs at SandboxDir, and the agent's
//     GIT_CONFIG_* env carries core.hooksPath=SandboxDir. See
//     internal/agentproc.
//   - Local: the agent subprocess's env carries
//     core.hooksPath=HostDir() directly (DirectAgentEnv), scoped to that
//     process — never written to the operator's ~/.gitconfig. See
//     internal/agentproc.newDirectCommand.
package githooks

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// SandboxDir is the fixed in-sandbox path the host hooks dir is
// bind-mounted at (read-only). Fixed because the hooks are generic and
// read run context from env, so the location never varies per run.
// Mirrors the sandbox's other host-owned mounts (/run/tf.sock,
// /usr/local/bin/triagefactory).
const SandboxDir = "/run/tf-hooks"

// ConfigKey is the git config key that points git at a hooks directory.
const ConfigKey = "core.hooksPath"

// BinEnvVar names the env entry carrying the absolute path to the
// triagefactory binary the hooks invoke. The hooks are generic shell
// scripts with no compiled-in path, and in local mode the binary is
// wherever the operator ran it from (not necessarily on PATH), so the
// spawner exports this in the agent process env in both run modes; the
// hooks read it with a PATH fallback. Part of the F2 run-context
// convention, alongside TRIAGE_FACTORY_CONVERSATION_ID and the agenthost socket.
const BinEnvVar = "TRIAGE_FACTORY_BIN"

// PushCaptureEnvVar names the env entry that tells the pre-push hook who owns
// branch-push capture — and base-branch push enforcement — for this run. When
// set to PushCaptureProxy (the sandbox env, written whenever the per-run git
// proxy is wired), the hook stands down entirely: the proxy's ref gate already
// adjudicates the team's base-branch push policy, so a second judgment there
// would be redundant; and every push transits the proxy, whose receive-pack
// capture observes the upstream's actual outcome — so artifacts are written
// only for pushes that landed, and refused pushes leave an audit failure row.
// The hook fires BEFORE the transfer and cannot know the outcome; letting it
// record under a proxy would mint branch artifacts for pushes GitHub refused.
// Unset (local mode — no proxy exists), the hook is the only capture point
// AND the only policy gate: it records at pre-push time, accepting that it
// cannot observe the outcome, and refuses a protected-ref push itself.
const PushCaptureEnvVar = "TF_GIT_PUSH_CAPTURE"

// PushCaptureProxy is the PushCaptureEnvVar value that stands the pre-push
// hook down in favor of the git proxy's outcome capture.
const PushCaptureProxy = "proxy"

//go:embed README.md
var readmeContent []byte

// prePushHook is the embedded pre-push hook (A·3, TFAC-460) Ensure writes
// into the hooks dir. Two passes: it enforces the team's base-branch push
// policy via `triagefactory hook check-push` (local mode's enforcement point
// — a safety guard against a mistaken agent, aborting only on that verb's
// dedicated "refused by policy" status, so every operational failure allows
// the push), then records each pushed branch as a durable artifact via
// `triagefactory hook record-push`. Recording stays best-effort and can never
// change the exit status.
//
//go:embed pre-push
var prePushHook []byte

// prepareCommitMsgHook is the embedded prepare-commit-msg hook (TFAC-452)
// Ensure writes into the hooks dir. It appends a Co-authored-by: trailer
// crediting the delegating human on manual runs, gated on the
// TRIAGE_FACTORY_GIT_COAUTHOR_TRAILER run env var. Generic + idempotent: a
// no-op when the var is unset (autonomous/event runs), so shipping it
// unconditionally never affects those runs or the pre-push behavior.
//
//go:embed prepare-commit-msg
var prepareCommitMsgHook []byte

// HostDir returns the host-side TF-controlled hooks directory. In local
// mode this is what the agent's core.hooksPath points at directly; in
// multi mode it is the bind-mount source for SandboxDir.
func HostDir() string {
	return paths.HooksDir()
}

// Ensure creates the host hooks directory and (re)writes its README,
// .keep marker, and managed hooks. Idempotent: safe to call on every
// startup. Called before any run can spawn so core.hooksPath always
// resolves to a real directory with the current hook set.
//
// The managed hooks (A·3, TFAC-460 pre-push; TFAC-452 prepare-commit-msg) are
// rewritten every call so an upgraded binary refreshes a stale on-disk copy —
// the hooks dir lives under the state root and persists across upgrades. They
// are written 0755 because git skips non-executable hooks.
func Ensure() error {
	dir := HostDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("githooks: create hooks dir %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), readmeContent, 0o644); err != nil {
		return fmt.Errorf("githooks: write hooks README: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".keep"), nil, 0o644); err != nil {
		return fmt.Errorf("githooks: write hooks .keep: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pre-push"), prePushHook, 0o755); err != nil {
		return fmt.Errorf("githooks: write pre-push hook: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prepare-commit-msg"), prepareCommitMsgHook, 0o755); err != nil {
		return fmt.Errorf("githooks: write prepare-commit-msg hook: %w", err)
	}
	return nil
}

// DirectAgentEnv returns env extended with core.hooksPath=HostDir() — plus any
// extraPairs (the org commit identity's user.name/user.email, via
// IdentityConfigPairs) — as git env-config entries, for a non-sandboxed (local)
// agent subprocess. The returned slice REPLACES the process env (it is env plus
// our entries), not a fragment to append. extraPairs is variadic so the
// hooks-only callers (and tests) stay unchanged; an empty list reproduces the
// original single-entry behavior exactly.
//
// Our entries land at successive next-free GIT_CONFIG indices — starting one
// past the inherited GIT_CONFIG_COUNT — so a pre-existing operator GIT_CONFIG_*
// set (a custom CA, say) is preserved and our entries compose alongside it.
// Crucially, the inherited GIT_CONFIG_COUNT line is dropped and re-emitted
// bumped to cover all our entries, so git reads a single, correct count
// regardless of how duplicate env keys resolve (getenv is first-wins on glibc,
// last-wins elsewhere — depending on either is a portability bug). This mirrors
// internal/worktree's gitConfigEnviron exactly.
//
// Git's env-config form (GIT_CONFIG_COUNT + GIT_CONFIG_KEY_n/_VALUE_n) is
// used — not a `git config` write — so nothing touches on-disk config
// (the operator's ~/.gitconfig stays untouched) and the install is scoped
// to this one agent process. core.hooksPath is always entry 0 of our block so
// its index is stable for any test asserting it.
func DirectAgentEnv(env []string, extraPairs ...[2]string) []string {
	pairs := append([][2]string{{ConfigKey, HostDir()}}, extraPairs...)
	base := gitConfigCount(env)
	out := make([]string, 0, len(env)+1+2*len(pairs))
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") {
			continue // re-emitted below, bumped to include our entries
		}
		out = append(out, kv)
	}
	for i, kv := range pairs {
		idx := base + i
		out = append(out,
			"GIT_CONFIG_KEY_"+strconv.Itoa(idx)+"="+kv[0],
			"GIT_CONFIG_VALUE_"+strconv.Itoa(idx)+"="+kv[1],
		)
	}
	return append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(base+len(pairs)))
}

// gitConfigCount parses GIT_CONFIG_COUNT (the number of git's indexed
// env-config entries) from env, or 0 when it is absent, malformed, or
// negative. Scans last-to-first so a duplicated COUNT resolves the way
// git and Go's exec env-dedup would (last value wins).
//
// Intentionally a byte-for-byte copy of internal/worktree's unexported
// gitConfigCount rather than a shared import. Reusing worktree's would
// mean either exporting it from there (widening that package's API for a
// 12-line helper) or githooks importing worktree — the wrong dependency
// direction, since worktree is a heavy clone/fetch package and githooks
// is a leaf. The only clean DRY fix is a new shared leaf package, which
// isn't worth standing up for one trivial function that mirrors git's
// fixed, documented GIT_CONFIG_COUNT format (so it won't drift) and is
// independently unit-tested on both sides. If a third copy ever appears,
// that's the rule-of-three trigger to extract — along with the
// drop-and-re-emit layering this and worktree.gitConfigEnviron also
// share, which is the more valuable thing to consolidate.
func gitConfigCount(env []string) int {
	for i := len(env) - 1; i >= 0; i-- {
		v, ok := strings.CutPrefix(env[i], "GIT_CONFIG_COUNT=")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return n
		}
		return 0
	}
	return 0
}
