# Triage Factory git hooks

This directory is **TF-controlled**. Triage Factory installs it as git's
`core.hooksPath` for every delegated-agent run — process-scoped, via the
agent subprocess's `GIT_CONFIG_*` environment (never a per-worktree
`git config` write and never the operator's `~/.gitconfig`). Because the
config is process-scoped, these hooks fire on **every** git operation the
agent performs: in both run modes (local + sandboxed multi), under any
working directory, including repositories the agent clones into subdirs
itself.

Do not add or edit files here by hand. On startup TF ensures the
directory exists and rewrites the files it manages (this README, and the
hooks themselves once they ship). It does **not** delete files it doesn't
recognize, so a stray file you drop here will persist — but git only runs
files named exactly after a hook (`pre-push`, ...), so an unrecognized
file is inert rather than dangerous.

## Run-context convention

Hooks are **generic** — they carry no per-run state. They read the run
context from the environment TF guarantees is present in the agent
process in both modes:

- `TRIAGE_FACTORY_RUN_ID` — the run the git op belongs to. A hook records
  through the `triagefactory exec ...` choke point, which resolves this
  into the `(org, user, run)` identity (local mode) or hands it to the
  agenthost daemon (sandbox mode).
- The **agenthost socket** — present at `/run/tf.sock` inside the sandbox
  (multi). In local mode there is no socket; `triagefactory exec`
  auto-detects its absence and routes writes through a `LocalClient`
  keyed by `TRIAGE_FACTORY_RUN_ID` instead. Either way a hook just runs
  `triagefactory exec ...` and the choke point does the right thing.

## Contract for hooks added here

- **Best-effort, non-blocking.** A hook failure must never fail the
  agent's git op. Exit `0` regardless of what the recording side-channel
  does (e.g. `triagefactory exec ... || true`).
- **Executable.** Files must be `0755`; git skips non-executable hooks.
- **Named after the git hook** they implement (`pre-push`,
  `prepare-commit-msg`, ...). A hooks dir with no file matching the hook
  git is firing is simply a no-op.

No hooks ship yet — this is the install mechanism only (F2, TFAC-456).
The first hook (`pre-push`, branch-artifact capture) lands with A·3.
