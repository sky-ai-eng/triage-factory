# Running Triage Factory (local mode)

Local mode (`TF_MODE=local`, the default) runs Triage Factory as a single binary
on your own machine — SQLite for state, the OS keychain for credentials, no
Postgres, GoTrue, or Docker. This page covers launching the binary and the CLI
subcommands. To install it first, see [INSTALLATION.md](../INSTALLATION.md).

## Launching

```bash
# Default (port 3000, opens browser)
triagefactory

# Custom port, no browser
triagefactory --port 8080 --no-browser

# Bind to all interfaces (default is 127.0.0.1 — loopback only).
# Only do this on a trusted network: the HTTP API is unauthenticated
# and the server holds keychain-backed credentials.
triagefactory --host 0.0.0.0

# Top-level help — points humans at the user commands and agents at exec.
triagefactory --help
```

To reach a headless box's UI without exposing the API, keep the default loopback
bind and tunnel to it — see [Running headless](headless.md).

## CLI subcommands

The binary's subcommands fall into two audiences. Run `triagefactory --help` for
a one-screen summary.

### User commands

These are meant for you to invoke from a terminal.

```bash
# Symlink the binary onto PATH so the `triagefactory` CLI works from
# any directory. Defaults: /usr/local/bin on macOS, ~/.local/bin on Linux.
# Override with --dest /full/path/to/triagefactory. Only relevant if you didn't
# install through homebrew.
triagefactory install

# Removes all local data. Run this before running `brew uninstall triagefactory`
triagefactory uninstall
```

### Agent commands

These are invoked by delegated Claude Code agents inside their worktree, not by
you. Run `triagefactory exec --help` for the full subcommand list.

## The `gh` channel

A delegated agent talks to GitHub through the real `gh` CLI, behind a per-run
credential proxy. Local mode runs the same channel multi-mode deployments do —
the difference is only how the pieces get on the machine.

### The binary

Triage Factory pins one `gh` release (`internal/ghpin`) and fetches it on the
first run that needs it, into:

```
~/.triagefactory/bin/gh          # the binary
~/.triagefactory/bin/gh.pin      # what was installed, and from where
```

The download is checked against the pinned SHA256 before anything is installed,
and the binary is re-verified on every run — a stale pin or a rewritten file is
re-fetched. `~/.triagefactory/bin` leads the agent subprocess's `PATH`, so `gh`
resolves there.

**Your own `gh` is never used.** It may be any version, and — the real problem —
it is logged in as *you*: an agent invoking it would act under your full personal
GitHub scope, outside the per-run credential channel entirely. The agent also
gets its own `GH_CONFIG_DIR`, so `~/.config/gh` (including the account and token
in your `hosts.yml`) is never read.

### The credential

Each run gets a loopback proxy on `127.0.0.1`. The agent's `gh` holds only a
random per-run placeholder; the proxy strips it and attaches the org's real
GitHub credential on the way upstream, reading it fresh per request so a
credential you rotate mid-run is picked up. The real token never enters the
agent's environment, arguments, or any file it can read. A repo the credential
can't reach returns GitHub's own 404, unchanged.

The agent gets an enumerated set of subcommands — `gh pr` (view / list / diff /
checkout / create / comment / ready / close), `gh issue`, `gh run`, `gh repo
view`, `gh search`. Not the whole binary: `gh api` would let a prompt-injected
agent POST anything anywhere with the org's credential attached, `gh auth` /
`config` / `alias` / `extension` are credential and configuration surfaces, and
`gh pr merge` has no local equivalent of the merge-intent gate. Those are
refused, and the refusal is visible to the agent as an ordinary permission
denial.

### When the fetch fails

Nothing breaks. An unreachable release, an unsupported platform, or a checksum
mismatch is logged, the run starts without the gh channel, and the agent works
through the scoped `triagefactory exec gh` verbs exactly as it does today. `gh`
is also left out of the agent's allowed commands entirely in that case — which
is what keeps it from falling through to your own installation.
