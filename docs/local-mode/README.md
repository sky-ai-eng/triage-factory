# Local mode

Local mode (`TF_MODE=local`, the default) is Triage Factory as a single binary on
your own machine: SQLite for state at `~/.triagefactory/`, the OS keychain (or an
encrypted file when headless) for credentials, and **no Postgres, GoTrue, or
Docker**. It's the single-user shape — one operator, one machine. For the
multi-tenant deployment, see [self-hosting](../self-hosting/).

To install the binary, start with [INSTALLATION.md](../INSTALLATION.md).

## Guides

- [Running](running.md) — CLI flags, `install` / `uninstall`, the exec subcommands
- [Configuration](configuration.md) — settings storage, poll intervals, Jira setup, credentials
- [Secret storage](secret-storage.md) — keychain vs. encrypted-file backend, `TF_SECRET_ENCRYPTION_KEY`
- [Running headless](headless.md) — no browser / keychain-less hosts + `TF_HEADLESS` zero-touch provisioning (cross-platform)
- [Environment tuning](tuning.md) — logging, `TF_CLAUDE_BINARY`, agent-engine JIT

## Product behavior (both modes)

- [GitHub polling](../concepts/polling.md) — what the poller tracks
- [Tracked events](../concepts/tracked-events.md) — the GitHub/Jira event taxonomy
- [Repo profiling](../concepts/repo-profiling.md) — how repos get summarized for scoring
