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

Local mode runs fine on a headless server (no desktop, no browser, no OS keychain). This is **single-user local mode** — *not* the multi-tenant deployment in [self-host setup](self-host-setup.md), which is the only thing that needs Postgres, GoTrue, or Docker. All you need here is the binary and one extra environment variable.

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

4. **Finish setup in the browser.** Open the tunneled (or exposed) URL and complete the setup wizard normally — paste your GitHub / Jira / Anthropic credentials once and they persist (encrypted) across restarts.

That's the whole difference from a desktop install: one env key plus `--no-browser`/`--host` or a tunnel.

### Logging

Logs are structured (Go's `log/slog`) and written to stderr. Two environment variables tune output:

- `TF_LOG_LEVEL` — minimum level: `debug`, `info` (default), `warn`, or `error`.
- `TF_LOG_FORMAT` — `text` (human-readable, the default in local mode) or `json` (machine-parseable, the default when `TF_MODE=multi`).

Every line carries a `component` field (e.g. `component=router`) naming the subsystem — the structured replacement for the old `[router]` prefixes. Verbose steady-state traces (such as per-poll credential-tier resolution) log at `debug`, so set `TF_LOG_LEVEL=debug` to surface them.

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