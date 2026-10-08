# Secret storage (local mode)

Credentials Triage Factory stores (the GitHub PAT, the Jira credential, the
Linear API key, an Anthropic or Bedrock key, GitHub App private keys) are entered
in the UI and kept outside the database. Local mode does not read GitHub, Jira or
Linear credentials from environment variables; Claude can instead run on the
host's own credentials, which are never stored (see
[configuration](configuration.md#credentials)). The secret backend is selected
automatically:

- **Desktop / keychain present** (macOS, or Linux with a working Secret Service):
  the OS keychain. No extra configuration.
- **No keychain** (Docker, a server — `go-keyring` can't reach a D-Bus Secret
  Service): an encrypted file at `~/.triagefactory/secrets.enc`.
  Secrets are encrypted app-side with AES-256-GCM; only opaque ciphertext is
  written to disk.

The file backend **requires `TF_SECRET_ENCRYPTION_KEY`** — 32 bytes,
generated with `openssl rand -hex 32` (the same variable and key format the
multi-mode deployment uses, so one key works for both). If the file backend is
selected and the key is unset or invalid, the server refuses to start. Rotating
the key makes the existing `secrets.enc` undecryptable, so plan it as a "re-enter
your credentials" event. (Desktop/keychain installs don't need the key.)

`TF_SECRETS_BACKEND` overrides the auto-selection: `auto` (default), `keychain`
(force the keychain; error if unavailable), or `file` (force the encrypted file).
Either way, credentials entered in Settings persist across restarts. See
[Running on a server](headless.md) for setting up a host with no keychain.

> The same `TF_SECRET_ENCRYPTION_KEY` also governs multi-mode deployments, where
> it encrypts `public.org_secrets` in Postgres instead of `secrets.enc` — see
> [self-hosting install](../self-hosting/install.md).

## Linear

The org's Linear connection lives under these keys:

- `linear_api_key` — the personal API key the workspace connected with.
- `linear_auth_method` — which shape the connection takes. `api_key` is the
  only one Settings binds today.
- `linear_app_install` — reserved for connecting Linear by installing an app
  instead of pasting a key. Nothing writes it yet.
- `linear_bound_as` — who the key belonged to and its workspace's name, recorded
  when it was connected so Settings can show them. Not a secret; it is stored
  here so it is written and removed with the key.

Disconnecting Linear in Settings removes all four.
