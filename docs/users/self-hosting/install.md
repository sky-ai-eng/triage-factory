# Self-host setup (multi-mode)

This sets up the multi-tenant deployment: Postgres, GoTrue (sign-in), an object
store, and Triage Factory as a `control` + `executor` pair, run with Docker
Compose. The `triagefactory` binary renders the deployment, generates its
secrets, and starts it, pinned to the signed release image of its own version.

For airgapped deployments, to run the compose files yourself, or to run a build
of your own checkout, see [Running multi mode by hand](../../contributors/multi-mode-by-hand.md).

## 0. Requirements

- A Linux host with Docker Engine, Docker Compose 2 or later, and cgroup v2.
  The `executor` container needs `CAP_SYS_ADMIN`, `CAP_NET_ADMIN`,
  `CAP_SYS_RESOURCE` and a private cgroup namespace (the compose file sets all
  of these). Managed platforms that block those (managed Kubernetes, Railway,
  Render) can host `control` but not an executor. Details are in the
  [security overview](../security/security-overview.md).
- Ports `3000`, `9999` and `3030` free on the host.
- Outbound HTTPS to `ghcr.io`, `pkg-containers.githubusercontent.com` and
  `tuf-repo-cdn.sigstore.dev`, to pull the image and check its signature.
- A public HTTPS URL that reaches port `3000` on the host, through your TLS
  terminator or load balancer. Everything, sign-in included, is served there.

`triagefactory deploy` checks the host before it writes anything, and says
which requirement failed.

## 1. Install the binary

```sh
brew tap sky-ai-eng/tap
brew install triagefactory
```

Or download a release tarball ([local install](../local/install.md) covers
both). The binary's version is the version of the deployment it renders.

## 2. Create the GitHub OAuth app (sign-in)

People sign in to Triage Factory with GitHub, on the deployment's GitHub:
github.com, or your GitHub Enterprise Server when you set
`TF_DEFAULT_GITHUB_HOST` (step 3). Register the app on that GitHub, at
<https://github.com/settings/developers> or
`https://<your GitHub Enterprise Server>/settings/developers` → **New OAuth App**:

- **Homepage URL:** your public URL, e.g. `https://tfac.example.com`
- **Authorization callback URL:** `<public URL>/auth/v1/callback`

Keep the **Client ID**, and save a new **Client secret** to a file for step 3.

## 3. Render the deployment

```sh
triagefactory deploy init --dir /srv/triagefactory \
  --public-url https://tfac.example.com \
  --github-client-id <client id> \
  --github-client-secret-file ./gh-client-secret
```

On a terminal, `init` asks for any of the three values you leave off. It
checks the release image's signature, then writes the compose files, the
executor's seccomp profile and a `.env` (mode `0600`) with every password and
key generated. The rest of this guide runs from that directory; every `deploy`
command also takes `--dir`.

Optional settings are documented in the rendered `.env`. Put changes to the
rendered compose files in `compose.override.yml`, which compose merges on its
own and `deploy` never writes.

**On GitHub Enterprise Server**, set `TF_DEFAULT_GITHUB_HOST` in `.env` to your
GHES URL (e.g. `https://ghe.example.com`) **before the first organization is
created**. Sign-in uses that server too, so the OAuth app from step 2 must be
registered there. Records are keyed under the host they were created with and
are never migrated, so if you change it later you need a completely fresh
install. On github.com, leave it unset: multi mode refuses to boot with it set
to `https://github.com`.

Back up `.env`. Losing `TF_SECRET_ENCRYPTION_KEY` makes every stored
integration credential unreadable, and losing `POSTGRES_PASSWORD` locks
Triage Factory out of its database. Re-running `init` won't overwrite
anything, though; it only adds items `.env` is missing.

### Optional, deployment-wide

- Invite-only deployment: `TF_PREVENT_ORG_CREATION=true`.
- A GitHub App every organization can connect through: [Deployment GitHub App](deployment-integrations/deployment-github-app.md).
- A signer for those organizations' commits: [Deployment commit signer](deployment-integrations/deployment-commit-signer.md).
- One-click Jira Cloud connect: [Deployment Jira app](deployment-integrations/deployment-jira-app.md).
- One-click Linear install: [Deployment Linear app](deployment-integrations/deployment-linear-app.md).
- SAML sign-in: [SSO](sso.md).
- Bedrock through a customer IAM role: [Bedrock role mode](bedrock-role-mode.md).
- Behind a load balancer or CDN: set `TF_TRUSTED_PROXY_CIDR` ([Client IP & trusted proxies](networking.md)).
- Secrets from mounted files instead of `.env`: [Deployment secrets](secrets.md).

## 4. Start the stack

```sh
triagefactory deploy up
```

This starts Postgres, GoTrue, SeaweedFS, `control` (the API and background
work; it runs database migrations at boot) and one `executor` (runs agents in
gVisor sandboxes), then waits until every service is healthy. To check it
again later:

```sh
triagefactory deploy doctor
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
first organization (unless `TF_PREVENT_ORG_CREATION` is set), but org creation
is free and self-hosters are not limited in number of organizations configured.
On org creation, the app walks you through connecting GitHub, an issue tracker,
and a model provider. Organization setup from here on happens in the app.

To give someone the deployment-wide admin view, see
[Fleet console & operators](fleet-console.md).

## 6. (Optional) Enable Enterprise features

SSO, Slack, governance and the fleet console need a license token as per the
product [LICENSE](../../../LICENSE). Once purchased, add it to `.env` and
restart:

```sh
TF_LICENSE=<signed-token>
```

`control` logs the result at boot:

```
Enterprise license: Acme Corp (features: [sso slack governance fleet]) valid until 2026-09-21
```

A missing, invalid or expired token leaves every Enterprise feature off.
`GET /api/entitlements` shows which features are on.

## Changing a database password

Edit `.env` and run `triagefactory deploy up`; the password sidecars reapply
the passwords on every boot. `POSTGRES_PASSWORD` is the exception: Postgres
reads it only when the volume is first created, so also run
`ALTER USER postgres WITH PASSWORD '…'` inside the running `postgres`
container.

## Next steps

- [Upgrading](upgrading.md)
- [Monitoring & health checks](monitoring.md)
- [Scaling out](scaling.md): more executors, more control pods
- [Object storage](storage.md)
- [Deployment secrets](secrets.md)
- [Rotating the JWT key](jwt-key-rotation.md)
- [Security model](../security/)
