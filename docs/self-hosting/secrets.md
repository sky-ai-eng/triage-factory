# Deployment secrets

The multi-mode stack reads several secrets at boot: the at-rest encryption keys (`TF_SECRET_ENCRYPTION_KEY`, `TF_SESSION_ENCRYPTION_KEY`, `TF_COOKIE_SECRET`), the database DSN and role passwords, the object-store keys (`TF_BLOB_*`), the GoTrue admin bearer (`TF_GOTRUE_SERVICE_ROLE_TOKEN`), and — optionally — the Enterprise license token (`TF_LICENSE`), the deployment Atlassian app secret (`TF_ATLASSIAN_CLIENT_SECRET`), and the deployment GitHub App's private key, webhook secret, and client secret (`TF_GITHUB_APP_PRIVATE_KEY`, `TF_GITHUB_APP_WEBHOOK_SECRET`, `TF_GITHUB_APP_CLIENT_SECRET`). Every one is documented inline in [`.env.example`](../../.env.example). This page is about how to *supply* them and how TF handles them.

For **rotating** a secret, see [key rotation](key-rotation.md) (the JWT signing key) and the per-variable notes in `.env.example` (each key's rotation impact). For the deployment's overall threat model, see [docs/security/](../security/).

## Two ways to supply a secret

**Plain environment variable** (the default). Set `NAME` in `.env`; the bundled compose forwards it into the container.

**A file** (`NAME_FILE`) — the standard convention for Docker and Kubernetes secrets, which are mounted as files. Set `NAME_FILE` to the mount path and leave the plain `NAME` blank. This keeps the secret's *value* out of the compose file, out of `docker inspect`, and out of the container's environment entirely — only the path is an env var.

## How TF handles a secret at boot

At startup TF captures each deployment secret into process memory (from `NAME_FILE` if set, else the plain `NAME`) and then **unsets it from the environment**. A set-but-unreadable `NAME_FILE` fails boot with a clear error; if both `NAME` and `NAME_FILE` are set, the file wins (with a warning).

What that buys you, and where the value can still be observed:

| Surface | Plain `NAME=` | Via `NAME_FILE` |
|---|---|---|
| Child processes (git, `gh`, the SDK installer) inheriting it | **no** — unset from `os.Environ()` before any child spawns | **no** |
| `docker inspect` / the compose file | yes | **no** — never a container-create env var |
| This process's own `/proc/<pid>/environ` | **yes** — the kernel's exec-time snapshot; TF can't rewrite it in-process | **no** — only the path was ever in env |
| Process heap (`/proc/<pid>/mem`, root/same-uid) | yes | yes — unavoidable: TF must hold the value to decrypt/sign/connect |

So `NAME_FILE` is strictly better for the sensitive keys — above all `TF_SECRET_ENCRYPTION_KEY`, the master key for the org-secrets vault. Credentials handed to *sandboxed agents* are a separate, stronger boundary — see [docs/security/](../security/).

## Which secrets support `NAME_FILE` in the bundled compose

The **binary** honors `NAME_FILE` for `TF_SECRET_ENCRYPTION_KEY`, `TF_SESSION_ENCRYPTION_KEY`, `TF_COOKIE_SECRET`, `TF_LICENSE`, `TF_GOTRUE_SERVICE_ROLE_TOKEN`, `TF_ATLASSIAN_CLIENT_SECRET`, `TF_GITHUB_APP_PRIVATE_KEY`, `TF_GITHUB_APP_WEBHOOK_SECRET`, `TF_GITHUB_APP_CLIENT_SECRET`, `TF_DATABASE_URL`, `TF_DATABASE_DIRECT_URL`, `TF_AUTHENTICATOR_PASSWORD`, `TF_BLOB_ACCESS_KEY`, and `TF_BLOB_SECRET_KEY` — but only if the container's environment actually carries the `NAME_FILE` variable.

The bundled `docker-compose.yml` **forwards `NAME_FILE` for the TF-only secrets**: `TF_LICENSE`, `TF_GOTRUE_SERVICE_ROLE_TOKEN`, `TF_SESSION_ENCRYPTION_KEY`, `TF_COOKIE_SECRET`, `TF_SECRET_ENCRYPTION_KEY` (all pods), and — the control pod only — `TF_ATLASSIAN_CLIENT_SECRET` (control serves the Jira connect flow) plus `TF_GITHUB_APP_PRIVATE_KEY`, `TF_GITHUB_APP_WEBHOOK_SECRET`, and `TF_GITHUB_APP_CLIENT_SECRET` (control serves the deployment App's connect ceremony and webhook receiver, and its background brain mints the installation tokens that reach executors already sealed into per-run bundles, so an executor never holds the key). For those, set `NAME_FILE` in `.env`, leave the plain `NAME` blank, and mount the file. The private key is the one to reach for first: a PEM is multi-line, which a `.env` carries badly and a mounted file carries natively.

The rest are **not** `_FILE`-wired in the bundled stack, because the compose file either **constructs** them (`TF_DATABASE_URL` is built from `POSTGRES_PASSWORD`) or **shares** them with a sidecar that needs the plain value (`TF_BLOB_*` are templated into SeaweedFS's S3 identity; `TF_AUTHENTICATOR_PASSWORD` is applied to the DB role by `postgres-postinit`). Use `NAME_FILE` for those only in a custom deployment (Kubernetes, your own compose) where you set the container environment yourself.

## Example: mount secrets with Docker Compose

Docker Compose secrets mount under `/run/secrets/<name>`. Because the bundled compose already forwards the `_FILE` vars, an override only needs the **mount**; point `_FILE` at it from `.env`:

```yaml
# compose.override.yml  (auto-merged by `docker compose`)
secrets:
  tf_enc_key: { file: ./secrets/enc_key }
  tf_license: { file: ./secrets/license }
  tf_gh_app_key: { file: ./secrets/github-app.private-key.pem }
services:
  triagefactory:
    secrets: [tf_enc_key, tf_license, tf_gh_app_key]
  executor:
    secrets: [tf_license]
```

```dotenv
# .env — leave the plain TF_SECRET_ENCRYPTION_KEY blank; point _FILE at the mount
TF_SECRET_ENCRYPTION_KEY_FILE=/run/secrets/tf_enc_key
TF_LICENSE_FILE=/run/secrets/tf_license
# the deployment GitHub App's PEM, mounted whole (BEGIN/END lines included)
TF_GITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/tf_gh_app_key
```

### File ownership

Keep the host file private to you (`chmod 600`); nothing else needs to read it. Compose bind-mounts a `file:` secret with the host file's owner and mode, and ignores a secret's `uid`/`gid`/`mode` (it warns that they are unsupported), so inside the container the file still belongs to your host uid. The TF process runs as uid `10001` once the entrypoint drops privileges, so the entrypoint, while still root, copies every `NAME_FILE` it finds into `/run/tf-secrets/`, owned by that uid and mode `0400`, and points the variable at the copy before the drop. The mount itself is never chmod'd or chown'd. A `NAME_FILE` naming a path root cannot read is left alone, so boot fails with the binary's own error naming it.

When deploying from a source checkout, `secrets/` and `compose.override.yml` are gitignored: the files in the example above cannot land in a commit through a careless `git add`.

> The three crypto keys are optional (`:-`) in the bundled compose specifically so the file form can leave the plain var blank — if you supply *neither* the plain value nor a readable file, the binary fails fast at boot with a clear `"<NAME> is empty"`. The other required secrets keep compose's parse-time `${VAR:?}` check, so an unfilled `.env` still fails at `docker compose up`.
