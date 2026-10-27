# Deployment secrets

Every secret the deployment reads is listed, with what it's for, in
[`.env.example`](../../../.env.example). This page covers supplying them as
files and how Triage Factory handles them at boot. For rotation, see the notes
on each variable in `.env.example` and [JWT key rotation](jwt-key-rotation.md).

## Plain variable or file

- **Plain variable:** set `NAME` in `.env`.
- **File:** set `NAME_FILE` to the mounted path and leave `NAME` blank. This is
  the Docker and Kubernetes secrets convention, and it keeps the value out of
  the compose file, `docker inspect` and the process environment.

If both are set, the file wins and a warning is logged. A `NAME_FILE` that
can't be read fails boot with an error naming it. A required key supplied
neither way fails boot with `<NAME> is empty`.

At boot Triage Factory reads each secret into memory and removes it from its
environment, so child processes (git, `gh`, the agent SDK installer) never
inherit it.

| Where the value can be seen | Plain `NAME` | `NAME_FILE` |
|---|---|---|
| Child processes | no | no |
| `docker inspect`, the compose file | yes | no |
| The process's `/proc/<pid>/environ` | yes | no |
| The process's memory (root or same uid) | yes | yes |

Prefer the file form for the sensitive keys, above all
`TF_SECRET_ENCRYPTION_KEY`, and for anything multi-line, such as a PEM.

## Which secrets accept `NAME_FILE`

| Secret | Forwarded by the bundled compose |
|---|---|
| `TF_LICENSE` | control and executor |
| `TF_SECRET_ENCRYPTION_KEY`, `TF_SESSION_ENCRYPTION_KEY`, `TF_COOKIE_SECRET`, `TF_GOTRUE_SERVICE_ROLE_TOKEN` | control |
| `TF_ATLASSIAN_CLIENT_SECRET`, `TF_LINEAR_CLIENT_SECRET` | control |
| `TF_GITHUB_APP_PRIVATE_KEY`, `TF_GITHUB_APP_WEBHOOK_SECRET`, `TF_GITHUB_APP_CLIENT_SECRET` | control |
| `TF_GITHUB_COMMIT_SIGNER_KEY` | control |
| `TF_DATABASE_URL`, `TF_DATABASE_DIRECT_URL`, `TF_AUTHENTICATOR_PASSWORD`, `TF_BLOB_ACCESS_KEY`, `TF_BLOB_SECRET_KEY` | no |

With the bundled compose, set `NAME_FILE` in `.env` and mount the file; the
variable is already forwarded. The last row is not forwarded because the
compose file builds those values itself or shares them with a sidecar that
needs the plain value. Use the file form for them only in a deployment where
you set the container environment yourself.

## Example

Compose mounts secrets under `/run/secrets/<name>`:

```yaml
# compose.override.yml (merged automatically by docker compose)
secrets:
  tf_enc_key: { file: ./secrets/enc_key }
  tf_license: { file: ./secrets/license }
  tf_gh_app_key: { file: ./secrets/github-app.private-key.pem }
services:
  control:
    secrets: [tf_enc_key, tf_license, tf_gh_app_key]
  executor:
    secrets: [tf_license]
```

```dotenv
# .env: leave the plain variables blank
TF_SECRET_ENCRYPTION_KEY_FILE=/run/secrets/tf_enc_key
TF_LICENSE_FILE=/run/secrets/tf_license
TF_GITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/tf_gh_app_key
```

In a source checkout, `secrets/` and `compose.override.yml` are gitignored.

## File ownership

Keep each host file `chmod 600` and owned by you. Compose ignores a secret's
`uid`, `gid` and `mode`, so the mounted file keeps your ownership. The
container's entrypoint copies each `NAME_FILE` it can read into
`/run/tf-secrets/`, readable only by the uid Triage Factory runs as, and
points the variable at the copy. Your mounted file is never modified.
