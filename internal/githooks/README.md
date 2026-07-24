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

- `TRIAGE_FACTORY_CONVERSATION_ID` — the run the git op belongs to. A hook records
  through the `triagefactory hook ...` callback (an internal namespace,
  kept off `exec` so the agent can't invoke it), which resolves this into
  the `(org, user, run)` identity (local mode) or hands it to the agenthost
  daemon (sandbox mode).
- `TRIAGE_FACTORY_BIN` — the absolute path to the `triagefactory` binary
  the hook invokes. The hooks are generic scripts with no compiled-in
  path, and in local mode the binary lives wherever the operator ran it
  from (not necessarily on `PATH`), so the spawner exports this in both
  modes. Hooks read it with a `PATH` fallback (`${TRIAGE_FACTORY_BIN:-triagefactory}`).
- The **agenthost socket** — present at `/run/tf.sock` inside the sandbox
  (multi). In local mode there is no socket; `triagefactory exec`
  auto-detects its absence and routes writes through a `LocalClient`
  keyed by `TRIAGE_FACTORY_CONVERSATION_ID` instead. Either way a hook just runs
  `triagefactory exec ...` and the choke point does the right thing.

## Contract for hooks added here

- **Best-effort, non-blocking.** A hook failure must never fail the
  agent's git op. Exit `0` regardless of what the recording side-channel
  does (e.g. `triagefactory exec ... || true`).
- **Executable.** Files must be `0755`; git skips non-executable hooks.
- **Named after the git hook** they implement (`pre-push`,
  `prepare-commit-msg`, ...). A hooks dir with no file matching the hook
  git is firing is simply a no-op.

## Shipped hooks

- `pre-push` (A·3, TFAC-456→TFAC-460) — records each pushed branch as a
  durable `branch` artifact via `triagefactory hook record-push`. git
  feeds it the pushed refs on stdin; it skips deletes, marks new branches,
  and always exits `0`. Rewritten by `Ensure` on every startup so an
  upgraded binary refreshes a stale on-disk copy.

  **Stands down when `TF_GIT_PUSH_CAPTURE=proxy`** (the sandbox env, set
  whenever the per-run git proxy is wired): pre-push fires *before* the
  transfer, so it cannot know whether the push will land — recording there
  would mint artifacts for pushes GitHub refuses. Under a proxy, the
  proxy's receive-pack capture owns the record instead: artifact +
  `branch_pushed` audit row on a 2xx, a `branch_push_failed` audit row on
  anything else. Local mode has no proxy, so the hook keeps its
  record-at-pre-push role there (outcome unobservable — accepted).
