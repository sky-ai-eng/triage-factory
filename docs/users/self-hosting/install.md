# Self-host setup (multi-mode)

This sets up the multi-tenant deployment: Postgres, GoTrue (sign-in), an object
store, and Triage Factory as a `control` + `executor` pair, all from
`docker-compose.yml`.

## 0. Requirements

- A Linux VM or bare-metal host with Docker Engine and Compose v2. The
  `executor` container needs `CAP_SYS_ADMIN`, `CAP_NET_ADMIN`,
  `CAP_SYS_RESOURCE` and a private cgroup namespace (the compose file sets all
  of these). Managed platforms that block those (managed Kubernetes, Railway,
  Render) can host `control` but not an executor. Details are in the
  [security overview](../security/security-overview.md).
- A checkout of this repository. The compose file builds the image from source;
  to run a published image instead, swap each service's `build:` block for the
  commented `image: ghcr.io/sky-ai-eng/triage-factory:<tag>` line
  ([verifying releases](../security/verifying-releases.md)).
- A `triagefactory` binary on the host for the one-time key generation in step 3
  ([download or build one](../local/install.md)).
- A public HTTPS URL that reaches port `3000` on the host, through your TLS
  terminator or load balancer. Everything, sign-in included, is served there.

## 1. Create the GitHub OAuth app (sign-in)

People sign in to Triage Factory with GitHub. At
<https://github.com/settings/developers> → **New OAuth App**:

- **Homepage URL:** your public URL, e.g. `https://tfac.example.com`
- **Authorization callback URL:** `<public URL>/auth/v1/callback`

Keep the **Client ID** and a new **Client secret** for step 2.

## 2. Fill in `.env`

```sh
cp .env.example .env
```

Generate every key and password with `openssl rand -hex 32`. Use hex, not
base64: some of these values are written into database connection URLs, where
base64's `/`, `+` and `=` break parsing.

| Variable                                   | What it is                                                                                                                                                                            |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POSTGRES_PASSWORD`                        | Postgres superuser, used for migrations.                                                                                                                                              |
| `SUPABASE_AUTH_ADMIN_PASSWORD`             | The database role GoTrue connects as.                                                                                                                                                 |
| `TF_AUTHENTICATOR_PASSWORD`                | The database role `control` serves requests as.                                                                                                                                       |
| `TF_SYSTEM_PASSWORD`                       | The least-privilege database role executors connect as.                                                                                                                               |
| `TF_PUBLIC_URL`                            | Your public URL, no trailing slash.                                                                                                                                                   |
| `GH_CLIENT_ID`, `GH_CLIENT_SECRET`         | The OAuth app from step 1.                                                                                                                                                            |
| `TF_SESSION_ENCRYPTION_KEY`                | Encrypts sign-in sessions. Changing it signs everyone out.                                                                                                                            |
| `TF_COOKIE_SECRET`                         | Signs the short-lived sign-in state cookie. Changing it only breaks sign-ins in progress.                                                                                             |
| `TF_SECRET_ENCRYPTION_KEY`                 | Encrypts every stored integration credential (GitHub App keys, Jira and Linear tokens). Changing or losing it makes them all unreadable, and every workspace has to enter them again. |
| `TF_BLOB_ACCESS_KEY`, `TF_BLOB_SECRET_KEY` | Credentials for the bundled SeaweedFS object store. To use S3, R2, or Supabase Storage instead, see [Durable workspace storage](storage.md).                                          |

`docker compose up` refuses to start while a required value is blank. Every
other variable in `.env.example` is optional and documented there.

**On GitHub Enterprise Server**, also set `TF_DEFAULT_GITHUB_HOST` to your GHES
URL (e.g. `https://ghe.example.com`) **before the first workspace is created**.
Records are keyed under the host they were created with and are never migrated,
so changing it later means a fresh install.

To change a database password later, edit `.env` and run
`docker compose up -d`; the password sidecars reapply it on every boot.
`POSTGRES_PASSWORD` is the exception: Postgres reads it only when the volume is
first created, so also run `ALTER USER postgres WITH PASSWORD '…'` inside the
running `postgres` container.

### Optional, deployment-wide

- Invite-only deployment: `TF_PREVENT_ORG_CREATION=true`.
- A GitHub App every workspace can connect through: [Deployment GitHub App](deployment-github-app.md).
- A signer for those workspaces' commits: [Deployment commit signer](deployment-commit-signer.md).
- One-click Jira Cloud connect: [Deployment Jira app](deployment-jira-app.md).
- One-click Linear install: [Deployment Linear app](deployment-linear-app.md).
- SAML sign-in: [SSO](sso.md).
- Bedrock through a customer IAM role: [Bedrock role mode](bedrock-role-mode.md).
- Behind a load balancer or CDN: set `TF_TRUSTED_PROXY_CIDR` ([Client IP & trusted proxies](networking.md)).
- Secrets from mounted files instead of `.env`: [Deployment secrets](secrets.md).

## 3. Generate the sign-in signing key

```sh
./triagefactory jwk-init --write-env .env
```

This writes `GOTRUE_JWT_KEYS`, `GOTRUE_JWT_SECRET` and
`TF_GOTRUE_SERVICE_ROLE_TOKEN` into `.env`. Run it after `TF_PUBLIC_URL` is set.
Re-running it is safe: it keeps an existing keypair and rewrites the lines in
place. To replace the key on purpose, see
[Rotating the JWT key](jwt-key-rotation.md).

## 4. Start the stack

```sh
docker compose up -d
```

This starts Postgres, GoTrue, SeaweedFS, `control` (the API and background
work; it runs database migrations at boot) and one `executor` (runs agents in
gVisor sandboxes). Check it came up:

```sh
docker compose ps                              # services running/healthy; the three *-postinit sidecars exited(0)
curl -fsS http://localhost:3000/api/health     # 200
curl -s http://localhost:9999/.well-known/jwks.json | jq .   # one public RSA key
```

> **Never run two Triage Factory containers off copies of one data volume.**
> The `tf-data` volume holds the container's identity (`instance-id`). Two
> processes sharing one identity stop each other: the older one kills its own
> sandboxes and exits with code 3, the restart policy brings it back, and the
> two crash-loop indefinitely. Stop one of them to fix it. If you duplicate a
> host or volume on purpose, delete `instance-id` from the copy first so it
> creates a new identity.

## 5. Sign in

Open your public URL and sign in with GitHub. The first person creates the
organization (unless `TF_PREVENT_ORG_CREATION` is set), and the app walks them
through connecting GitHub, an issue tracker and a model provider. Workspace
setup from here on happens in the app.

To give someone the deployment-wide admin view, see
[Fleet console & operators](fleet-console.md).

## 6. (Optional) Enable Enterprise features

SSO, Slack, governance and the fleet console need a license token from your
vendor. Add it to `.env` and restart:

```sh
TF_LICENSE=<signed-token>
```

`control` logs the result at boot:

```
Enterprise license: Acme Corp (features: [sso slack governance fleet]) valid until 2026-09-21
```

A missing, invalid or expired token leaves every Enterprise feature off.
`GET /api/entitlements` shows which features are on.

## Next steps

- [Monitoring & health checks](monitoring.md)
- [Scaling out](scaling.md): more executors, more control pods
- [Durable workspace storage](storage.md)
- [Deployment secrets](secrets.md)
- [Rotating the JWT key](jwt-key-rotation.md)
- [Security model](../security/)
