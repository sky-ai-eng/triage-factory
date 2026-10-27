# Local mode

Local mode (`TF_MODE=local`, the default) is Triage Factory as a single binary on
your own machine: SQLite for state under `~/.triagefactory/`, the OS keychain
(or an encrypted file when there is no keychain) for credentials, and no
Postgres, GoTrue or Docker. For a deployment that serves a whole organization,
see [self-hosting](../self-hosting/).

Everything you connect and configure after install (GitHub, Jira, Linear,
Claude, repositories, teams) is set up in the app.

- [Install](install.md): Homebrew, a release tarball, or a source build
- [Running](running.md): flags, network exposure, `install` / `uninstall`
- [Configuration](configuration.md): where state lives, credentials, the Linux agent sandbox, the clone protocol
- [Secret storage](secret-storage.md): keychain or encrypted file, `TF_SECRET_ENCRYPTION_KEY`
- [Running on a server](server.md): a host with no keychain or browser
- [Tuning](tuning.md): run concurrency, disk, logging, tracing, the Claude binary
- [Commit signing](../workspace/commit-signing.md): creating the GitHub account agents' commits are signed as
