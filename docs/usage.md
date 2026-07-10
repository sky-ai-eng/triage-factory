# Usage

## Prerequisites

- [Go](https://go.dev/) 1.23+
- [Node.js](https://nodejs.org/) 20+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) (for AI scoring and delegation)

## Running

```bash
# Default (port 3000, opens browser)
./triagefactory

# Custom port, no browser
./triagefactory --port 8080 --no-browser

# Bind to all interfaces (default is 127.0.0.1 — loopback only).
# Only do this on a trusted network: the HTTP API is unauthenticated
# and the server holds keychain-backed credentials.
./triagefactory --host 0.0.0.0

# Top-level help — points humans at the user commands and agents at exec.
./triagefactory --help
```

## CLI subcommands

The binary's subcommands fall into two audiences. Run `./triagefactory --help` for a one-screen summary.

### User commands

These are meant for you to invoke from a terminal.

```bash
# Symlink the binary onto PATH so the `triagefactory` CLI works from
# any directory. Defaults: /usr/local/bin on macOS, ~/.local/bin on Linux.
# Override with --dest /full/path/to/triagefactory.
./triagefactory install
```

To take over a delegated run, open it in the browser run console (`/runs/:id`)
and steer it live — send the agent a message, interrupt the current turn, or
answer a tool-permission prompt. There is no separate eject-to-terminal step.

### Agent commands

These are invoked by delegated Claude Code agents inside their worktree, not by you. Documented for completeness.

```bash
# Scoped GitHub commands the agent uses instead of touching the
# GitHub API directly — credentials stay in the OS keychain and
# every call is logged to run_artifacts.
./triagefactory exec gh pr view --owner sky-ai-eng --repo myrepo --number 42

# Scoped Jira commands, same shape.
./triagefactory exec jira issue view SKY-194

# Check a delegated run's status (used by the agent's lifecycle hooks).
./triagefactory status <run-id>
```

Run `./triagefactory exec --help` for the full subcommand list.

## Configuration

Settings are stored in the SQLite DB at `~/.triagefactory/triagefactory.db` (table `settings`, single row holding a YAML blob) and edited exclusively via the Settings page. The poll interval defaults to 5 minutes for both GitHub and Jira; configurable values are 30s, 1m, 2m, 5m.

Earlier releases used a YAML file at `~/.triagefactory/config.yaml`. On first launch after upgrading, the contents are imported into the DB and the file is removed; the poll interval is reset to the new 5m default as part of that import.

### Jira setup

Jira uses a two-stage flow in Settings:

1. Enter your Jira URL and Personal Access Token, click **Connect**. Credentials are validated and stored immediately.
2. The card expands to reveal project selection, poll interval, and status configuration. Statuses are fetched automatically from your Jira instance.
3. **Save** is disabled until you've configured projects, pickup statuses, and an in-progress status.

### Credentials

All credentials (GitHub PAT, Jira PAT, the Anthropic key, GitHub App private keys) are stored outside the database — in the OS keychain on desktop, or an encrypted file on headless installs (see [Secret storage](#secret-storage) below). Token fields in Settings show "leave blank to keep current" when a token is already stored.

### Secret storage

The secret backend is selected automatically:

- **Desktop / keychain present** (macOS, or Linux with a working Secret Service): the OS keychain, as above. No extra configuration.
- **Headless** (Docker, a server with no keychain — `go-keyring` can't reach a D-Bus Secret Service): an encrypted file at `~/.triagefactory/secrets.enc`. Secrets are encrypted app-side with AES-256-GCM; only opaque ciphertext is written to disk.

The headless file backend **requires `TF_SECRET_ENCRYPTION_KEY`** — 32 bytes, generated with `openssl rand -hex 32` (the same variable and key format the multi-mode deployment uses, so one key works for both). If the file backend is selected and the key is unset or invalid, the server refuses to start. Rotating the key makes the existing `secrets.enc` undecryptable, so plan it as a "re-enter your credentials" event. (Desktop/keychain installs don't need the key.)

`TF_SECRETS_BACKEND` overrides the auto-selection: `auto` (default), `keychain` (force the keychain; error if unavailable), or `file` (force the encrypted file). Unlike the four `TRIAGE_FACTORY_*` org-credential overlays, the file backend is writable, so credentials entered in Settings (the Anthropic key, a GitHub App, your per-user Jira token) persist across restarts on a headless box.

### Running headless on a Linux server

Local mode runs fine on a headless server (no desktop, no browser, no OS keychain). This is **single-user local mode** — *not* the multi-tenant deployment in [self-host setup](self-host-setup.md), which is the only thing that needs Postgres, GoTrue, or Docker. There are two ways to set it up: run it and reach the UI once over a tunnel, or provision everything from environment variables (zero-touch — see below).

1. **Install the binary** (Homebrew or `go build` — see the [README](../README.md)). `$HOME` must be set; state lands in `~/.triagefactory/`.

2. **Generate a secret key.** With no keychain reachable, secrets are kept in an encrypted file (see [Secret storage](#secret-storage) above), which requires `TF_SECRET_ENCRYPTION_KEY`:

   ```bash
   export TF_SECRET_ENCRYPTION_KEY=$(openssl rand -hex 32)
   ```

   Persist it where the process reads its environment (a systemd unit's `Environment=`, an `.env`, your shell profile). The server refuses to start headless without it, and losing or changing it means re-entering your credentials.

3. **Run without a browser**, reachable from where you'll use it:

   ```bash
   ./triagefactory --no-browser --host 0.0.0.0
   ```

   `--host 0.0.0.0` exposes the API on all interfaces. The HTTP API is unauthenticated, so only do that on a trusted network — otherwise keep the default loopback bind and reach it over an SSH tunnel:

   ```bash
   ssh -L 3000:localhost:3000 you@server   # then open http://localhost:3000
   ```

4. **Finish setup in the browser** (the tunneled or exposed URL) — paste your GitHub / Jira / Anthropic credentials, pick your repos, and bind your identity. Everything you enter persists (encrypted) across restarts.

#### Zero-touch provisioning (`TF_HEADLESS`)

To skip the browser entirely — for a reproducible container, a CI runner, or any unattended deploy — set `TF_HEADLESS=1`. On first start (local mode only) the server provisions itself from the environment: it creates the workspace, tracks the listed repos, optionally configures Jira (Data Center), and binds your identity — landing directly on the app with no setup wizard.

This is **not Linux-specific** — it works the same on macOS (a laptop, a mac CI runner). On any machine with a working OS keychain, secrets go to the keychain as usual, so `TF_SECRET_ENCRYPTION_KEY` (step 2 above) isn't needed — that key is only for the keychain-less file backend. The rest of the flow is identical.

| Variable | Purpose |
| --- | --- |
| `TF_HEADLESS` | Any non-empty value enables env-driven provisioning. |
| `TRIAGE_FACTORY_GITHUB_URL` | GitHub host (e.g. `https://github.com`). |
| `TRIAGE_FACTORY_GITHUB_BOT_PAT` | The org/bot access token the factory polls and runs autonomous work with. |
| `TRIAGE_FACTORY_GITHUB_USER_PAT` | *Your* identity token. Required for a no-browser boot — without it you'll be asked to connect your GitHub identity in the UI. Usually the same value as the bot PAT for a solo operator. |
| `TRIAGE_FACTORY_REPOS` | Comma-separated `owner/repo` list to track. Without at least one, the factory has nothing to poll. |
| `TRIAGE_FACTORY_CLONE_PROTOCOL` | `https` (default) or `ssh` — how repos are cloned to the box. Optional. |
| `TRIAGE_FACTORY_JIRA_URL` | Jira (Data Center) host. Optional. |
| `TRIAGE_FACTORY_JIRA_BOT_PAT` | Jira service token. Optional. |
| `TRIAGE_FACTORY_JIRA_USER_PAT` | Your Jira identity token. Required whenever Jira is configured. |
| `TRIAGE_FACTORY_JIRA_PROJECTS` | Comma-separated project keys (e.g. `SKY,TFAC`). |
| `TRIAGE_FACTORY_JIRA_PICKUP_STATUSES` | Comma-separated statuses that mean "ready to pick up". |
| `TRIAGE_FACTORY_JIRA_INPROGRESS_STATUS` | The single in-progress status. |
| `TRIAGE_FACTORY_JIRA_DONE_STATUS` | The single done status. |
| `ANTHROPIC_API_KEY` | Claude credential — local mode inherits it from the environment for scoring and delegation. |

The Jira variables apply one global status mapping to every tracked project. Jira here is optional and Data Center only; Jira Cloud onboarding stays in the UI.

Headless defaults to **HTTPS** cloning, authenticated with the bot PAT — a headless box usually has no SSH agent, so HTTPS is the credential-bearing path that works out of the box (private repos included). Set `TRIAGE_FACTORY_CLONE_PROTOCOL=ssh` only if the box has an SSH agent with a loaded key for your GitHub host; SSH clones authenticate through that agent, not the PAT.

Provisioning is a **one-time seed**: it runs only on the first start and never overwrites anything you later change in the UI, so editing repos or statuses in the app sticks across restarts. If `TF_HEADLESS` is unset but the seed variables are present, they're ignored with a warning. Missing or invalid GitHub credentials skip the whole bootstrap; an incomplete Jira block skips just Jira — both log a warning rather than failing the server.

> **Note:** `TRIAGE_FACTORY_GITHUB_BOT_PAT` / `TRIAGE_FACTORY_JIRA_BOT_PAT` were previously `TRIAGE_FACTORY_GITHUB_PAT` / `TRIAGE_FACTORY_JIRA_PAT`. The `BOT` names make the access-vs-identity split explicit (paired with the `USER_PAT` identity vars). Update any existing env files on upgrade.

### Logging

Logs are structured (Go's `log/slog`) and written to stderr. Two environment variables tune output:

- `TF_LOG_LEVEL` — minimum level: `debug`, `info` (default), `warn`, or `error`.
- `TF_LOG_FORMAT` — `text` (human-readable, the default in local mode) or `json` (machine-parseable, the default when `TF_MODE=multi`).

Every line carries a `component` field (e.g. `component=router`) naming the subsystem — the structured replacement for the old `[router]` prefixes. Verbose steady-state traces (such as per-poll credential-tier resolution) log at `debug`, so set `TF_LOG_LEVEL=debug` to surface them.

### Claude binary

By default the Agent SDK launches the Claude binary bundled with Triage Factory. To point it at a specific `claude` instead — a locally-built, pinned, or debug binary — set `TF_CLAUDE_BINARY` to its path:

```bash
export TF_CLAUDE_BINARY=/path/to/claude
```

The path is validated at spawn (it must exist and be executable), so a wrong path fails the run with a clear error rather than falling back silently. **Local mode only** — the sandboxed multi-tenant path runs the image-baked binary and ignores this variable.

### Agent engine JIT

The vendored agent engine is a Bun-compiled binary running on JavaScriptCore. By default Triage Factory disables its JIT (`BUN_JSC_useJIT=0`), which cuts peak RSS per agent process by roughly a fifth with no measurable startup cost — agent workloads are I/O-bound enough that interpreter-only execution is noise. Set `TF_AGENT_JSC_JIT=1` to restore the JIT if a compute-heavy workload regresses under the interpreter:

```bash
export TF_AGENT_JSC_JIT=1
```

Applies on both the sandbox and direct/local spawn paths.

### Privilege separation (multi mode, `TF_PRIVSEP`)

Multi mode's per-run gVisor sandbox needs root-equivalent Linux capabilities (`CAP_SYS_ADMIN`, `CAP_NET_ADMIN`) to set up netns/veth/iptables, cgroups, and the curated rootfs. Those operations run in a separate `cap-broker` process — every sandbox setup/teardown/rootfs/cgroup operation is an RPC to it over a host-only unix socket (`/run/tf/cap-broker.sock`, mode 0600) — so the main TF process (the "orchestrator": the one resolving credentials and parsing hostile input — agent output, webhook payloads) never holds those capabilities at all. The same broker also owns the run-tree ownership lifecycle — chowning a run's worktree to the sandbox uid at run start, the park-time git-delta capture (a setuid + network-namespaced child), and run-tree removal at teardown — because each of those needs a capability (`CAP_CHOWN`, `CAP_SETUID`/`CAP_SYS_ADMIN`, or unlink-through-foreign-modes) the orchestrator gives up; the broker validates every such request (path shape + current tree ownership) before touching anything.

The provided container image (`docker/entrypoint.sh`) is what makes this a real boundary rather than just a code-level split: it holds the container's granted capabilities only long enough to spawn the cap-broker, then execs the orchestrator through `setpriv`, which clears its capability bounding set, drops inheritable capabilities, sets `no_new_privs`, and switches it to a dedicated unprivileged uid — all before the orchestrator's Go runtime starts a single thread. Capabilities are per-thread in Linux and the Go runtime starts threads before `main()` runs, so dropping them from *inside* a running Go process would only ever affect the one calling thread and silently leave others privileged; `setpriv` avoids that by dropping them in a small external tool before the orchestrator binary is even loaded. The cap-broker and the orchestrator end up as different uids, so the (now capability-less) orchestrator cannot `ptrace` the broker.

```bash
export TF_PRIVSEP=1     # the default; set for clarity or to un-set a prior override
export TF_PRIVSEP=0     # rollback: single in-process, fully-privileged executor, as before
```

Boolean (case-insensitive: `0`/`false`/`no`/`off` disables it; unset or any other value is on). **Default on.** `TF_PRIVSEP=0` is a temporary rollback escape hatch to the prior single-process behavior (no cap-broker, no capability drop) for deployments that hit a regression — it is not meant to be a long-term configuration and will eventually be removed. **Multi mode + Linux only**; local mode never sandboxes and ignores the flag entirely. If the broker fails to start, boot fails with a clear error rather than silently falling back to the in-process path.

**Legibility.** Both processes are identifiable without reading source: `ps` / `/proc/<pid>/comm` shows `tf-cap-broker` and `tf-orchestrator` (rather than both sharing the binary's basename), every log line carries a `proc=cap-broker` or `proc=orchestrator` attribute (distinct from `component`, which names the subsystem within a process), and each process logs one boot line with its uid and effective capability set, e.g.:

```
level=INFO msg=boot component=cap-broker uid=0 CapEff=cap_net_admin,cap_sys_admin socket=/run/tf/cap-broker.sock proc=cap-broker
level=INFO msg=boot component=boot uid=10001 CapEff=(empty) proc=orchestrator
```

A `CapEff=(empty)` orchestrator boot line is the confirmation the drop applied; anything else there is a bug, not a variant configuration.

See the process-model table in [self-host-setup.md](self-host-setup.md#privilege-separation-process-model) for what each process holds and how to cross-check it against `/proc/<pid>/status` on a running deployment.

## GitHub polling

The poller tracks PRs across several categories:

- **Review requested** — PRs where your review is pending
- **Authored** — Your open PRs, including CI status from the check-runs API
- **Mentioned** — PRs where you were @mentioned
- **Reviewed** — PRs you've previously reviewed (tracks for follow-up)
- **Merged / Closed** — Terminal PRs tracked for dashboard statistics

All discovery queries filter to recent activity. The tracker diffs snapshots on each poll cycle and emits typed events only on state transitions — see [tracked-events.md](tracked-events.md) for the full event taxonomy.

## Repo profiling

Configured repos are automatically profiled on first run using Claude Haiku. The profiler fetches README.md, CLAUDE.md, and AGENTS.md from each repo and generates a summary used by the AI scorer and delegation agents.

Profiles are cached for 3 days. The **Re-profile** button on the Repos page forces an immediate refresh regardless of TTL.