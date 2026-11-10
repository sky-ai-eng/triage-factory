# Secret storage (local mode)

Credentials you enter in the app (GitHub, Jira, Linear, an Anthropic or Bedrock
key, GitHub App keys, OAuth client secrets) are kept outside the database in
one of two backends:

- **OS keychain**: macOS, Windows (Credential Manager), or Linux with a working
  Secret Service. Nothing to configure.
- **Encrypted file** at `~/.triagefactory/secrets.enc` (on Windows,
  `%USERPROFILE%\.triagefactory\secrets.enc`): any host with no reachable
  keychain, such as a server or a container. It is encrypted with AES-256-GCM
  using `TF_SECRET_ENCRYPTION_KEY`.

Windows Credential Manager refuses a single value over 2560 bytes, which a
4096-bit GitHub App private key can exceed. Triage Factory splits such a value
across several Credential Manager entries and joins them on read, so there is
nothing to do. In Credential Manager you will see the entry itself plus extra
entries named like it with `#1`, `#2` and so on appended. Leave them alone:
deleting one makes the credential unreadable (`triagefactory uninstall` and
removing the credential in the app delete all of them together).

The encrypted file needs a key:

```bash
export TF_SECRET_ENCRYPTION_KEY=$(openssl rand -hex 32)
```

On Windows, run that in Git Bash (it ships `openssl`), or in PowerShell 7:

```powershell
$env:TF_SECRET_ENCRYPTION_KEY = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLower()
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

For a host with no keychain, see [Running on a server](server.md).
