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
