# Secret storage (local mode)

All credentials Triage Factory uses (GitHub PAT, Jira PAT, the Anthropic key,
GitHub App private keys) are stored outside the database. The secret backend is
selected automatically:

- **Desktop / keychain present** (macOS, or Linux with a working Secret Service):
  the OS keychain. No extra configuration.
- **Headless** (Docker, a server with no keychain — `go-keyring` can't reach a
  D-Bus Secret Service): an encrypted file at `~/.triagefactory/secrets.enc`.
  Secrets are encrypted app-side with AES-256-GCM; only opaque ciphertext is
  written to disk.

The headless file backend **requires `TF_SECRET_ENCRYPTION_KEY`** — 32 bytes,
generated with `openssl rand -hex 32` (the same variable and key format the
multi-mode deployment uses, so one key works for both). If the file backend is
selected and the key is unset or invalid, the server refuses to start. Rotating
the key makes the existing `secrets.enc` undecryptable, so plan it as a "re-enter
your credentials" event. (Desktop/keychain installs don't need the key.)

`TF_SECRETS_BACKEND` overrides the auto-selection: `auto` (default), `keychain`
(force the keychain; error if unavailable), or `file` (force the encrypted file).
Unlike the four `TRIAGE_FACTORY_*` org-credential overlays, the file backend is
writable, so credentials entered in Settings (the Anthropic key, a GitHub App,
your per-user Jira token) persist across restarts on a headless box.

> The same `TF_SECRET_ENCRYPTION_KEY` also governs multi-mode deployments, where
> it encrypts `public.org_secrets` in Postgres instead of `secrets.enc` — see
> [self-hosting install](../self-hosting/install.md).
