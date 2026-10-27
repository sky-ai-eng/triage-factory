# Local mode

Local mode (`TF_MODE=local`, the default) is Triage Factory as a single binary on
your own machine: SQLite for state at `~/.triagefactory/`, the OS keychain (or an
encrypted file when there is no keychain) for credentials, and **no Postgres, GoTrue, or
Docker**. It's the single-user shape — one operator, one machine. For the
multi-tenant deployment, see [self-hosting](../self-hosting/).

To install the binary, start with [INSTALLATION.md](install.md).

## Guides

- [Running](running.md) — CLI flags, `install` / `uninstall`, the exec subcommands
- [Configuration](configuration.md) — settings storage, poll intervals, Jira and Linear setup, credentials
- [Secret storage](secret-storage.md) — keychain vs. encrypted-file backend, `TF_SECRET_ENCRYPTION_KEY`
- [Running on a server](server.md) — a host with no keychain or browser: `TF_SECRET_ENCRYPTION_KEY`, reaching the UI, configuring there
- [Environment tuning](tuning.md) — run concurrency (`TF_MAX_CONCURRENT_CLAIMS`), logging, tracing (`TF_TRACES_ENDPOINT` + a one-container Tempo), `TF_CLAUDE_BINARY`, agent-engine JIT

## Product behavior (both modes)

- [Tracked events](../../../specs/tracked-events.md) — the GitHub/Jira/Linear event taxonomy
- [Commit signing](../workspace/commit-signing.md) — creating the GitHub account agents' commits are signed as, and registering it
