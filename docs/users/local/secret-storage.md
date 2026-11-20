# Secret storage (local mode)

Credentials you enter in the app (GitHub, Jira, Linear, an Anthropic or Bedrock
key, GitHub App keys, OAuth client secrets) are kept outside the database in
one of two backends:

- **OS keychain**: macOS, or Linux with a working Secret Service. Nothing to
  configure.
- **Encrypted file** at `~/.triagefactory/secrets.enc`: any host with no
  reachable keychain, such as a server or a container. It is encrypted with
  AES-256-GCM using `TF_SECRET_ENCRYPTION_KEY`.

The encrypted file needs a key:

```bash
export TF_SECRET_ENCRYPTION_KEY=$(openssl rand -hex 32)
```

If the file backend is in use and the key is unset or invalid, the server
refuses to start.

> **Important:** Changing the key makes the existing file unreadable, so plan a
> key change as re-entering every credential.

A self-hosted deployment uses the same variable and key format; generate a
separate key for each install.

`TF_SECRETS_BACKEND` overrides the automatic choice:

| Value            | Behavior                                                        |
| ---------------- | --------------------------------------------------------------- |
| `auto` (default) | The keychain if one is reachable, otherwise the encrypted file. |
| `keychain`       | The keychain; startup fails if there isn't one.                 |
| `file`           | The encrypted file.                                             |

## A credential too large for the keychain

The macOS keychain refuses a single value larger than about 3,000 bytes. Most
credentials are far smaller, but a GitHub App private key generated at 4096
bits is about 3,200 bytes. Saving one fails with the reason `SECRET_TOO_LARGE`
and this message:

```
this secret is too large for the OS keychain; to store it in the encrypted file backend instead, set TF_SECRETS_BACKEND=file and TF_SECRET_ENCRYPTION_KEY, restart Triage Factory, and enter your credentials again
```

The encrypted file has no size limit. To switch to it, generate a key and set
both variables:

```bash
export TF_SECRET_ENCRYPTION_KEY=$(openssl rand -hex 32)
export TF_SECRETS_BACKEND=file
```

Persist both wherever Triage Factory gets its environment (a shell profile or
an env file), not only in the terminal you ran this in, and keep the key: no
other key can read the file. Then restart. The file starts empty, and nothing
is copied from the keychain, so enter each credential again.

For a host with no keychain, see [Running on a server](server.md).
