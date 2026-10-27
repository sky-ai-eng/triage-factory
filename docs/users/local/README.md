# Local mode

Local mode (`TF_MODE=local`, the default) is Triage Factory as a single binary on
your own machine: SQLite for state under `~/.triagefactory/`, the OS keychain
(or an encrypted file when there is no keychain) for credentials, and no
Postgres, GoTrue or Docker. Agents in this mode cannot be securely sandboxed to
the same extent as a multi-tenant deployment.

Local mode is intended for single developers or users without access to GitHub
Apps through their enterprise. Many features from the multi-tenant mode are
missing, but setup is intentionally lightweight and the full triage engine is
included. To deploy Triage Factory for an organization, see
[self-hosting](../self-hosting/install.md) instead.

Everything you connect and configure after install (GitHub, Jira, Linear,
Claude, repositories, teams) is set up in the app.

## Pages

- [Install](install.md): Homebrew, a release tarball, or a source build
- [Running](running.md): flags, network exposure, `install` / `uninstall`
- [Configuration](configuration.md): where state lives, credentials, the Linux agent sandbox, the clone protocol
- [Secret storage](secret-storage.md): keychain or encrypted file, `TF_SECRET_ENCRYPTION_KEY`
- [Running on a server](server.md): a host with no keychain or browser
- [Tuning](tuning.md): run concurrency, disk, logging, tracing, the Claude binary
