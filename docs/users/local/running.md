# Running (local mode)

## Launching

```bash
triagefactory                              # port 3000, opens the browser
triagefactory --port 8080 --no-browser     # another port, no browser
triagefactory --help                       # one-screen summary of commands
```

The server binds `127.0.0.1` by default. Local mode has no authentication:
every request is treated as the owner, and the server holds your credentials.
A non-loopback bind (`--host 0.0.0.0`) therefore refuses to start with
`refusing to start: local mode runs with NO authentication …` unless you also
set `TF_ALLOW_PUBLIC_LOCAL=true`:

```bash
TF_ALLOW_PUBLIC_LOCAL=true triagefactory --host 0.0.0.0
```

Only do that on a network where everyone who can reach the port is you. To
reach a remote machine's UI without exposing it, keep the loopback bind and
tunnel to it; see [Running on a server](server.md).

## CLI subcommands

```bash
# Symlink the binary onto PATH: /usr/local/bin on macOS, ~/.local/bin on Linux.
# --dest /full/path/to/triagefactory picks another location. Not needed with Homebrew.
triagefactory install

# Delete all local data. Run this before `brew uninstall triagefactory`.
triagefactory uninstall
```

`triagefactory exec …` is the set of commands delegated agents run from their
worktree. You don't need to call them; `triagefactory exec --help` lists them.
