# Running multi mode by hand

[`triagefactory deploy`](../users/self-hosting/install.md) is the supported
way to run a self-hosted deployment. This page covers running the same compose
files yourself: from a source checkout, to build your own image, or from the
release's compose bundle on a host where you manage the files.

This replaces running `deploy init` and `deploy up`, steps 3 and 4 of the
[install guide](../users/self-hosting/install.md). The rest of that guide
applies as written, and so do three parts of those two steps: the GitHub
Enterprise Server setting, the optional settings, and the warning about
copying a data volume.

## 1. Get the compose files

- **A source checkout** of this repository, or
- **The compose bundle** from a release, `triagefactory-compose_<version>.tar.gz`:
  `docker-compose.yml`, `.env.example` and the files the compose file mounts.
  Check it against the release's `checksums.txt`
  ([verifying releases](../users/security/verifying-releases.md)).

`docker-compose.yml` runs the signed release image it pins. Nothing here
checks that image's signature for you, as `deploy` does; `cosign verify` does
it by hand ([verifying releases](../users/security/verifying-releases.md)).

## 2. Fill in `.env`

```sh
cp .env.example .env
```

Generate every key and password with `openssl rand -hex 32`. Use hex, not
base64: some of these values are written into database connection URLs, where
base64's `/`, `+` and `=` break parsing.

| Variable                                                                         | What it is                                                                                                                                                                                                               |
| -------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `POSTGRES_PASSWORD`                                                              | Postgres superuser, used for migrations.                                                                                                                                                                                 |
| `SUPABASE_AUTH_ADMIN_PASSWORD`                                                   | The database role GoTrue connects as.                                                                                                                                                                                    |
| `TF_AUTHENTICATOR_PASSWORD`                                                      | The database role `control` serves requests as.                                                                                                                                                                          |
| `TF_SYSTEM_PASSWORD`                                                             | The least-privilege database role executors connect as.                                                                                                                                                                  |
| `TF_PUBLIC_URL`                                                                  | Your public URL, no trailing slash.                                                                                                                                                                                      |
| `GH_CLIENT_ID`, `GH_CLIENT_SECRET`                                               | The GitHub OAuth app.                                                                                                                                                                                                    |
| `TF_SESSION_ENCRYPTION_KEY`                                                      | Encrypts sign-in sessions. Changing it signs everyone out.                                                                                                                                                               |
| `TF_COOKIE_SECRET`                                                               | Signs the short-lived sign-in state cookie. Changing it only breaks sign-ins in progress.                                                                                                                                |
| `TF_SECRET_ENCRYPTION_KEY`                                                       | Encrypts every stored integration credential (GitHub App keys, Jira and Linear tokens). Changing or losing it makes them all unreadable, and every workspace has to enter them again.                                    |
| `TF_WORKSPACE_*`, `TF_KNOWLEDGE_*`, `TF_KNOWLEDGE_READER_*`, `SEAWEEDFS_ADMIN_*` | The four `ACCESS_KEY` / `SECRET_KEY` pairs for the bundled SeaweedFS object store; the four access keys must differ. To use S3, R2, or Supabase Storage instead, see [Object storage](../users/self-hosting/storage.md). |

`docker compose up` refuses to start while a required value is blank. Every
other variable in `.env.example` is optional and documented there.

## 3. Generate the sign-in signing key

```sh
triagefactory jwk-init --write-env .env
```

This writes `GOTRUE_JWT_KEYS`, `GOTRUE_JWT_SECRET` and
`TF_GOTRUE_SERVICE_ROLE_TOKEN` into `.env`. Run it after `TF_PUBLIC_URL` is set.
Re-running it is safe: it keeps an existing keypair and rewrites the lines in
place. To replace the key on purpose, see
[Rotating the JWT key](../users/self-hosting/jwt-key-rotation.md).

## 4. Start the stack

```sh
docker compose up -d
```

Check it came up:

```sh
docker compose ps                              # services running/healthy; the three *-postinit sidecars exited(0)
curl -fsS http://localhost:3000/api/health     # 200
curl -s http://localhost:9999/.well-known/jwks.json | jq .   # one public RSA key
```

Add executors with `docker compose up -d --scale executor=N`. A plain
`docker compose up -d` goes back to one.

## Building the image from your checkout

Add this line to `.env`:

```dotenv
COMPOSE_FILE=docker-compose.yml:compose.dev.yml
```

`compose.dev.yml` layers a build of the working tree, tagged
`triagefactory:dev`, over the release pin. Then:

```sh
docker compose up -d --build
```

`triagefactory deploy init --build-from <checkout>` renders a deployment that
builds the same way, so you can try `deploy` itself against your changes.

To run the binary on the host instead of in a container, against the compose
stack's Postgres, GoTrue and SeaweedFS, use `./scripts/multi-mode-dev.sh` (see
[CLAUDE.md](../../CLAUDE.md)).
