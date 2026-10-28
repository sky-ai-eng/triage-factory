# Self-host setup (multi-mode)

This is the operator-facing install flow for the multi-tenant deployment. **Local mode (default, `TF_MODE=local`) needs none of this** — install Triage Factory normally, no Postgres or GoTrue required (see [local mode](../local-mode/)).

Once the stack is up, the operational topics live in their own guides:
[Monitoring](monitoring.md), [Scaling out](scaling.md),
[Client IP & trusted proxies](networking.md), [Object storage](storage.md),
[Rotating the signing key](key-rotation.md), and [SSO with Microsoft Entra](sso-entra.md).
The security model (privilege separation, seccomp, release verification) is under
[docs/security/](../security/).

## 1. Create a GitHub OAuth app

Go to https://github.com/settings/developers → New OAuth App.

- **Homepage URL:** your public TF URL (e.g. `https://triagefactory.yourcompany.com`)
- **Authorization callback URL:** `${TF_PUBLIC_URL}/auth/v1/callback`

This is GoTrue's callback, not the TF callback handler — GitHub redirects here after the user authorizes, GoTrue exchanges the code, and then GoTrue 302s the browser back to the TF callback path (set per-request via the `redirect_to` query param on `/authorize`).

Save the **Client ID** and **Client secret**.

## 2. Populate `.env`

```sh
cp .env.example .env
```

Fill in:
- `POSTGRES_PASSWORD` — superuser password. Used for migrations and admin tasks. **Generate with `openssl rand -hex 32`** — `docker-compose.yml` interpolates this directly into the URL-form `TF_DATABASE_URL` DSN, so the same URL-safety constraint as the other DB-role passwords applies. Do *not* use `openssl rand -base64 32` — base64 includes `/`, `+`, and `=`, which are URL-reserved and break pgx's connection-string parser.
- `SUPABASE_AUTH_ADMIN_PASSWORD` — distinct password for the role GoTrue connects as. Keeping it separate from the superuser means a GoTrue compromise doesn't surrender full DB access. **Generate with `openssl rand -hex 32`** — GoTrue's DB library only accepts URL-form connection strings, so the password is interpolated into a `postgres://user:pass@host/...` URL. Plain hex avoids every URL-reserved character (`/`, `?`, `#`, `@`, `+`, `=`) by construction. Do *not* use `openssl rand -base64 32` — base64 includes `/` and `+` which break URL parsing.
- `TF_AUTHENTICATOR_PASSWORD` — password for the `authenticator` role TF's app pool connects as for RLS-active request handling. Kept distinct from the other two so a compromise of one pool doesn't surrender the others. **Generate with `openssl rand -hex 32`** for consistency with the other role passwords. (The TF binary builds the app DSN via `net/url.UserPassword`, which percent-encodes safely, so this password isn't strictly URL-constrained — but uniform rotation runbooks beat one-off exceptions.) The `postgres-postinit` sidecar reapplies this on every `docker compose up`, same as the auth-admin password.
- `TF_SYSTEM_PASSWORD` — password for the `tf_system` role: the least-privilege database role executor pods' admin pool connects as instead of `supabase_admin` (superuser). BYPASSRLS but no DDL, and a narrow, enumerated table grant list — see the `tf_system` section of `internal/db/migrations-postgres/202605130001_pg_baseline.sql`. Executors are the most-exposed machine class (they run agent workloads), so this is the difference between a compromised executor holding a superuser DSN and one holding a role that can only touch the run-execution tables it needs. Control pods are unaffected — they keep the `supabase_admin` DSN. **Generate with `openssl rand -hex 32`.** The role ships `NOLOGIN` in the baseline; a dedicated `postgres-postinit-system` sidecar grants it `LOGIN` on every `docker compose up` — split from the `postgres-postinit` sidecar above because `tf_system` doesn't exist until the `control` container has run its migration, so this sidecar waits on `control` being healthy rather than racing it. Always required: the default stack is the co-located control + executor split, and the `executor` service connects as `tf_system`.
- `TF_PUBLIC_URL` — your public URL (no trailing slash)
- `GH_CLIENT_ID` / `GH_CLIENT_SECRET` — from step 1
- `TF_SESSION_ENCRYPTION_KEY` — 32 random bytes; AES-GCM master key for the access/refresh tokens stored at rest in `public.sessions`. **Generate with `openssl rand -hex 32`.** Rotating this key invalidates every existing session (ciphertext can't be decrypted) — plan it as a forced re-auth event.
- `TF_COOKIE_SECRET` — 32 random bytes; HMAC-SHA256 key for the short-lived OAuth state cookie (carries PKCE verifier + CSRF token). **Generate with `openssl rand -hex 32`.** Kept distinct from `TF_SESSION_ENCRYPTION_KEY` so the two rotate independently — rotating only this one invalidates in-flight OAuth handshakes (10-minute window), not active sessions.
- `TF_SECRET_ENCRYPTION_KEY` — 32 random bytes; AES-GCM master key for org + per-user **integration secrets** (GitHub App PEM / client-secret / webhook-secret, PATs, Jira tokens) stored at rest in `public.org_secrets`. **Generate with `openssl rand -hex 32`.** TF encrypts these app-side and stores only opaque ciphertext, so they are **not** in Supabase Vault / pgsodium — a routine `docker compose down && up` (or any postgres container recreate) no longer loses them. Kept distinct from `TF_SESSION_ENCRYPTION_KEY` so the two rotate independently — a session-key rotation must not nuke integration secrets. Rotating *this* key invalidates every stored secret (the ciphertext can no longer be decrypted), so plan it as a "re-enter your integration credentials" event. The same variable also governs **local-mode installs with no OS keychain** (a single-binary deploy on a server or container) — there it encrypts `~/.triagefactory/secrets.enc` instead of `public.org_secrets`; see [local mode § Secret storage](../local-mode/secret-storage.md).
- `TF_WORKSPACE_ACCESS_KEY` / `TF_WORKSPACE_SECRET_KEY`, `TF_KNOWLEDGE_ACCESS_KEY` / `TF_KNOWLEDGE_SECRET_KEY`, `TF_KNOWLEDGE_READER_ACCESS_KEY` / `TF_KNOWLEDGE_READER_SECRET_KEY`, `SEAWEEDFS_ADMIN_ACCESS_KEY` / `SEAWEEDFS_ADMIN_SECRET_KEY` — the object store's four key pairs. Multi mode keeps workspace snapshots and team knowledge in two buckets, and each pair is one identity in the bundled `seaweedfs` service, limited to its bucket: the workspace pair (read-write on `tf-workspaces`, given to the executor), the knowledge writer (read-write on `tf-knowledge`, given to control), the knowledge reader (read-only on `tf-knowledge`, given to the executor as its `TF_KNOWLEDGE_*`), and the admin pair, which only the `seaweedfs-postinit` sidecar holds, to create both buckets and turn versioning on for `tf-knowledge`. **Generate each with `openssl rand -hex 32`** (access key must be ≥3 chars, secret ≥8 — a 64-char hex clears both, and keeps them JSON-safe inside SeaweedFS's `s3.json`); the four access keys must differ. The endpoint (`http://seaweedfs:8333`), buckets (`tf-workspaces`, `tf-knowledge`), and region (`us-east-1`) all default in `docker-compose.yml`; leave them unset for self-host. See [Object storage](storage.md) for what each role needs and the BYO / hosted path.
- `TF_ATLASSIAN_CLIENT_ID` / `TF_ATLASSIAN_CLIENT_SECRET` — *(optional)* the deployment's Atlassian OAuth (3LO) app, used for one-click "Connect Jira" against Cloud orgs. Deployment config in the same class as the GoTrue signing material — they live here in `.env`, **not** the keychain/`org_secrets`, because one Atlassian app is registered for the whole deployment. Leave unset to run without a deployment Atlassian app; each org can then supply its own from **Workspace Settings → Atlassian OAuth app** (a per-org override always wins over this default for new connections). A member's Jira connection is refreshed only through the app it was made through, so changing this pair to a different client ID sends everyone who connected through the old app back to **Connect Jira**; rotating the secret of the same app does not. Register the app in the [Atlassian developer console](https://developer.atlassian.com/console/myapps/) and set its callback (redirect) URL to `${TF_PUBLIC_URL}/api/orgs/{org}/jira/connect/callback`.
- `TF_LINEAR_CLIENT_ID` / `TF_LINEAR_CLIENT_SECRET` — *(optional)* the deployment's Linear OAuth app: the Linear counterpart of the Atlassian pair above, in the same class of config (`.env`, not the keychain/`org_secrets`). One app, registered once for the whole deployment, so a workspace with no app of its own installs Triage Factory in its Linear workspace with one click and never sees a client ID. **Leave both unset to run without one.** Set both or neither; one without the other is logged at boot and treated as neither. Register the app by following [step 9](#9-optional-register-a-deployment-linear-app). Either way a workspace connects Linear from **Workspace Settings**, one of two ways. It can install Triage Factory as an app in its Linear workspace: a Linear workspace admin clicks **Install Triage Factory** under **Linear connection**, which runs through the deployment app when there is one, or through an app the workspace creates itself from the pre-filled link under **Linear OAuth app** (which registers `${TF_PUBLIC_URL}/api/linear/install/callback` and `${TF_PUBLIC_URL}/api/linear/connect/callback` as redirect URIs) and pastes the client ID and secret of. A workspace's own app always wins over the deployment app. Triage Factory then acts as the app's user, which holds no person's key and takes no Linear seat. Private Linear teams have to be shared with the app on its page in Linear. A Linear workspace can be installed in only one Triage Factory workspace. Or it pastes a personal Linear API key under **Linear connection**. Both are stored in `org_secrets`. Once a workspace has Linear, every member connects their own Linear account before the app opens, because Linear writes a person starts are made as that person: **Connect Linear** authorizes through the same OAuth app, or they paste a personal API key from the same Linear workspace. Each member's credential is stored in `org_secrets` under their own user, and removing a member from the workspace deletes it and revokes a Connect grant.
- `TF_GITHUB_APP_ID` / `TF_GITHUB_APP_PRIVATE_KEY` / `TF_GITHUB_APP_WEBHOOK_SECRET` / `TF_GITHUB_APP_CLIENT_SECRET` — *(optional)* the deployment's GitHub App: the GitHub counterpart of the Atlassian pair above. One App, registered by hand once for the whole deployment, that any workspace with no GitHub credential of its own can connect to from **Workspace Settings → GitHub access → Connect GitHub…**. Same class of config as the Atlassian pair — `.env`, not the keychain/`org_secrets`. **Leave all four unset to run without one**: every workspace brings its own App instead, and a workspace that has its own App keeps using it either way — the deployment App is what a workspace with *no* credential gets, never an override. Set all four or none; a strict subset is logged at boot and treated as none. The values come from the App registration, which needs the stack up, so register it afterwards by following [step 8](#8-optional-register-a-deployment-github-app).
- `TF_DEFAULT_GITHUB_HOST` — *(GitHub Enterprise Server deployments only)* the deployment's default GitHub: the web base every workspace without a GitHub base URL of its own resolves to, **and** the GitHub the deployment App is registered on — one variable because they are one fact. Unset means `https://github.com`, which is right unless your GitHub is a GHES. **Set it before the first workspace exists**: installation, reachability and identity rows are keyed under the host they were written with and nothing migrates them, so changing it later is a fresh-install decision. Plain config, not a secret; a value that is not an `http(s)` web base fails boot.
- `TF_TRUSTED_PROXY_CIDR` / `TF_CAPTURE_CLIENT_IP` — *(recommended when behind a proxy)* govern how TF derives the client IP it records on sessions, on the SOC2 auth audit log, and keys the pre-auth rate limiter by. **If TF sits behind a load balancer or CDN, set `TF_TRUSTED_PROXY_CIDR`** — otherwise `X-Forwarded-For` is ignored and every request is attributed to the LB. See [Client IP & trusted proxies](networking.md) for the full rationale and per-topology guidance.
- `TF_LLM_CRED_TTL_SEC` / `TF_EXECUTOR_EGRESS_CIDRS` / `TF_EXECUTOR_VPCE_IDS` — *(Bedrock role mode only)* govern the short-lived STS session credentials the control process mints when an org authenticates to Bedrock by assuming a customer IAM role instead of storing a static key. Leave unset unless you offer that auth method — it also requires the control service to carry an ambient AWS identity. See [Bedrock role mode](bedrock-role-mode.md) for the prerequisite, the connect probe, and what each knob does.

> **Rotating passwords:** edit `.env` and re-run `docker compose up -d`. Two short-lived sidecars run on every boot and reapply `ALTER USER`/`ALTER ROLE` for the non-superuser roles — `postgres-postinit` for `supabase_auth_admin` and `authenticator`, `postgres-postinit-system` for `tf_system` — so password changes propagate without wiping the data volume. Rotating `POSTGRES_PASSWORD` itself requires more care — that's the superuser's password and Postgres only honors the env var on first init, so changing it means `ALTER USER postgres WITH PASSWORD '...'` by hand inside the running container.

## 3. Generate the JWT signing key

```sh
./triagefactory jwk-init --write-env .env
```

This generates a fresh RS256 keypair, formats it as GoTrue’s `GOTRUE_JWT_KEYS` JSON array of JWK objects (including both private and public material), and writes `GOTRUE_JWT_KEYS=<json>`, `GOTRUE_JWT_SECRET=...`, and `TF_GOTRUE_SERVICE_ROLE_TOKEN=...` to `.env`. The private side stays in `.env` (read only by GoTrue); only the public side is published at GoTrue's `/.well-known/jwks.json` endpoint. The generated `GOTRUE_JWT_SECRET` is also required by the compose stack, so if you manage these values manually, do not omit it. (`TF_GOTRUE_SERVICE_ROLE_TOKEN` is only needed once you enable SSO — see [the SSO guide](sso-entra.md) — but it's harmless to mint up front.)

`jwk-init --write-env` is **idempotent and upserts in place** — it replaces each variable's line rather than appending, collapsing any stale duplicates, so re-running is clean. Crucially, if `.env` already has a `GOTRUE_JWT_KEYS`, jwk-init **reuses that keypair** and writes only the service-role token; it does **not** rotate the signing key. To deliberately rotate the keypair, pass `--rotate` (see [Rotating the signing key](key-rotation.md)).

## 4. Bring up the stack

```sh
docker compose up -d
```

This brings up the full stack: Postgres, GoTrue, SeaweedFS (the durable-workspace object store), and the TF binary as the co-located control + executor pair — the `control` service (API/WS, background brain, migrations) plus one `executor` service (the sandbox worker: run dispatcher, gVisor jails, per-run credential sidecars). Multi mode is always this split; there is no single-process option (`TF_ROLE=all` refuses to boot in multi), and only the executor container carries sandbox capabilities. The Postgres image is `supabase/postgres`, which pre-provisions the `auth` schema, the `supabase_auth_admin` role GoTrue connects as, the `authenticator` role TF's app pool uses, and the `pgvector` extension. The image also ships the `vault` / `pgsodium` extensions, but TF doesn't use them — per-org secret storage is app-encrypted (see `TF_SECRET_ENCRYPTION_KEY` above), not Supabase Vault.

On first boot, the `postgres-postinit` sidecar reconciles the `supabase_auth_admin` and `authenticator` role passwords, the `seaweedfs-postinit` sidecar creates the workspace bucket (idempotent — `head-bucket || create-bucket` via aws-cli), then the `control` container's entrypoint runs `triagefactory migrate up` against the admin DSN before starting the server. That order is load-bearing for the event queue: its stored vocabulary and its worker ship together, so a worker on the previous binary cannot drain the migrated table and there is no flag that would let it — a rollback is a forward fix. Once `control` is healthy, a third sidecar, `postgres-postinit-system`, reconciles the `tf_system` password — it has to run last because `tf_system` is itself created by that migration, so it doesn't exist yet at the point the other two sidecars run.

> **The TF data volume is an identity, not just a cache.** The `tf-data` volume holds `instance-id` — the persistent identity this process registers into the fleet registry and stamps onto the runs it executes; crash recovery keys on it. Keep the volume persistent across restarts (the compose file already does), and **never run two TF containers off copies of one data volume** (a cloned volume, a restored snapshot started alongside the original): the second boot supersedes the first's identity, and the superseded process fences itself — stops claiming new runs, kills its own live sandboxes, then **exits with code 3**. Your restart policy (compose/systemd) brings it straight back up, which re-registers, bumps the epoch, and fences whichever OTHER copy is still running off the duplicated volume — so two processes sharing one data volume **crash-loop each other indefinitely**. That crash-loop is the intended alarm, not a bug: it's loud, visible in `docker compose ps`/logs, and impossible to miss, unlike a silently-fenced-but-still-running process. The fix is always the same — stop one of the duplicates. If you ever need to duplicate a machine on purpose, delete `instance-id` from the copy first so it mints a fresh identity instead of colliding.

Smoke-check the stack came up:

```sh
docker compose ps                         # all services should be "running"/"healthy" (the three postinit sidecars show "exited(0)")
curl -fsS http://localhost:3000/api/health   # 200 OK
curl -s http://localhost:9999/.well-known/jwks.json | jq .   # JWKS with one public RSA key
```

For ongoing liveness/readiness monitoring (and what to point a load balancer at),
see [Monitoring & health checks](monitoring.md). To add executors (or control
pods for HA), see [Scaling out](scaling.md).

## 5. Verify the GitHub OAuth flow

Drive the full OAuth roundtrip end-to-end in a browser:

```sh
# 1. Open the browser at /api/auth/oauth/github (substitute your TF_PUBLIC_URL)
open "https://triagefactory.yourcompany.com/api/auth/oauth/github?return_to=/"
# 2. Authorize in the GitHub UI
# 3. Browser lands back on / with an `sid` HttpOnly cookie
# 4. Confirm session is live
curl -b "sid=<value>" https://triagefactory.yourcompany.com/api/me
# 5. Logout (revokes server-side, clears cookie)
curl -b "sid=<value>" -X POST https://triagefactory.yourcompany.com/api/auth/logout
```

The same flow is also covered by the integration test suite (`go test ./internal/server/ -run TestAuthFlow`), which drives the callback handler against a testcontainer Postgres + in-process JWKS — handy for diagnosing whether a problem is in the auth wiring or in the GitHub OAuth app config.

## 6. (Optional) Smoke-test the Verifier directly

Useful when diagnosing whether the issue is in the Verifier wiring vs. GoTrue itself. Mint a test token via GoTrue's signup endpoint (no GitHub dance required):

```sh
TOKEN=$(curl -s -X POST http://localhost:9999/signup \
  -H 'Content-Type: application/json' \
  -d '{"email":"smoke@example.com","password":"smoketest123"}' \
  | jq -r .access_token)
```

Round-trip through the Verifier. **Note:** `.env` is read by Docker Compose, not your shell, so substitute your actual `TF_PUBLIC_URL` value here (the shell won't pick it up from `.env` unless you `set -a; source .env; set +a` first):

```sh
echo "$TOKEN" | TF_GOTRUE_JWKS_URL=http://localhost:9999/.well-known/jwks.json \
  TF_GOTRUE_ISSUER=https://triagefactory.yourcompany.com/auth/v1 \
  ./triagefactory jwk-init --verify
```

You should see the parsed claims printed as JSON (`Subject`, `Email`, `Provider`, etc.). This requires a local TF binary on the host — useful when the in-container TF service is misbehaving and you want to isolate the Verifier path from the rest of the server.

## 7. (Optional) Enable Enterprise features

Enterprise features — SSO, Slack, governance, the fleet console — are gated by a signed license token, verified offline at boot. To enable them, set the token your vendor gave you in `.env`:

```bash
TF_LICENSE=<signed-token>
```

then `docker compose up -d`. On boot the control pod logs the verdict to stderr:

```
Enterprise license: Acme Corp (features: [sso slack governance fleet]) valid until 2026-09-21
```

No token, a token signed by the wrong key, or an expired token → community default (every enterprise feature off, no crash). Confirm which features are live at any time with `GET /api/entitlements`. To supply the token as a mounted file instead of `.env`, see [Deployment secrets](secrets.md).

## 8. (Optional) Register a deployment GitHub App

A workspace's GitHub access is always a GitHub App, never a personal access token: an App's installation tokens last an hour and cover only the repositories it is installed on, and its client secret is what lets members link their GitHub accounts, which is why importing an App asks for it. A workspace normally brings its own — an App it registers from **Workspace Settings → GitHub access** (GitHub's manifest flow does the registration), or an App it already owns and imports. A **deployment GitHub App** is the alternative for a self-host that would rather register one App, once, and let every workspace use it: a workspace admin presses **Connect GitHub…**, names a GitHub account, confirms on GitHub, and that account's installation is bound to their workspace. It is to GitHub what the deployment Atlassian app in step 2 is to Jira, with more moving parts because a GitHub App has more of them.

Three things to hold onto before starting:

- **Precedence is the same as Jira's.** A workspace that has its own App keeps using it. The deployment App is what a workspace with no credential gets — a default, never an override. (Connect refuses a workspace that already holds a credential and says which one to disconnect first.)
- **Leaving the four variables unset changes nothing.** Every workspace continues to bring its own App, exactly as today.
- **Multi mode only.** Local mode ignores the variables; a distributed binary cannot ship a shared App key.

The App is registered by hand — no manifest, no automation — so the steps below name every field. They run top to bottom: each step uses only values produced by an earlier one. A reviewer who has never registered a GitHub App should not need GitHub's documentation open.

### 8.1 Decide which GitHub the App lives on

The App is registered on exactly one GitHub, and it is the deployment's default GitHub: `TF_DEFAULT_GITHUB_HOST`, unset meaning `https://github.com`. Nothing to do if your GitHub is github.com. If it is a GitHub Enterprise Server, `TF_DEFAULT_GITHUB_HOST=https://ghe.example.com` must already be in `.env` from step 2, **before the first workspace was created**, and the App is registered on that GHES. A workspace whose own GitHub base URL names a different host cannot use the deployment App — Connect refuses with a message naming both hosts (`This workspace is pointed at <workspace host>; the deployment's GitHub App is on <deployment host>`), and that workspace brings its own App. Changing the default later is a fresh-install decision: rows are keyed under the host they were written with.

### 8.2 Create the App on GitHub

Open **Settings → Developer settings → GitHub Apps → New GitHub App** on the account that should own it. Owning it under an organization you control (rather than a person's account) means it survives that person leaving: `https://github.com/organizations/<org>/settings/apps/new`, or `https://github.com/settings/apps/new` for a personal account. On a GHES, the same paths on your GHES host.

Fill the form in top to bottom. Anything not listed keeps its default.

| Field | Value |
|---|---|
| **GitHub App name** | Any name that is unique on that GitHub. Workspace admins see it on GitHub's install page, so name it after your deployment. |
| **Homepage URL** | Your `TF_PUBLIC_URL`. |
| **Callback URL** (under *Identifying and authorizing users*) | `${TF_PUBLIC_URL}/api/github/managed/callback`, absolute — for example `https://triagefactory.yourcompany.com/api/github/managed/callback`. One entry. It carries no workspace id on purpose: an App has one callback list fixed at registration, and the workspace is recovered from the connect ceremony's own record instead. The same URL also serves members linking their GitHub accounts through the App (8.5). |
| **Expire user authorization tokens** | Leave as is. |
| **Request user authorization (OAuth) during installation** | **Check it.** This is what lets TF learn *who* completed the installation, which is how a workspace proves an installation is theirs. GitHub greys out **Setup URL** the moment this is on — the two are mutually exclusive — so leave Setup URL blank and **Redirect on update** unchecked. With this box off, every connection fails with `The deployment's GitHub App is missing the “Request user authorization (OAuth) during installation” setting`. |
| **Enable Device Flow** | Leave unchecked. |
| **Webhook → Active** | Checked. |
| **Webhook URL** | `${TF_PUBLIC_URL}/api/webhooks/github` — the deployment receiver, again with no workspace id in it. Deliveries are verified against the secret below and then routed by the installation they name. |
| **Webhook secret** | Generate one now — `openssl rand -hex 32` — paste it here, and keep it: it becomes `TF_GITHUB_APP_WEBHOOK_SECRET` in step 8.4. GitHub never shows it again. |
| **SSL verification** | Enable. |

**Permissions.** Expand each group and set exactly these. Everything else stays *No access*.

| Repository permissions | Access |
|---|---|
| Actions | Read-only |
| Checks | Read-only |
| Commit statuses | Read-only |
| Contents | Read and write |
| Issues | Read and write |
| Metadata | Read-only (GitHub sets this itself) |
| Pull requests | Read and write |

| Organization permissions | Access |
|---|---|
| **Members** | **Read-only — required.** |

> **Organization → Members: Read-only is load-bearing. Do not trim it.** It is what lets TF confirm that the person connecting an *organization* is an owner of it: the connect ceremony asks GitHub for that person's role in the organization, and anything short of a definite owner refuses the connection. Without this permission, connecting an organization fails — TF reads the App's granted permissions from GitHub when a workspace connects and refuses with `the deployment App is not granted the organization members permission` — and, separately, GitHub only restricts installing an App to organization owners when the App requests an organization permission, so dropping it would also let any repository admin install your App. If you later change the App's permissions, each account that already installed it must accept the new permission set on GitHub before it takes effect.

| Account permissions | Access |
|---|---|
| **Email addresses** | **Read-only — required.** |

> **Account → Email addresses: Read-only is required too.** In a workspace with no App of its own, members link their GitHub account through this App, and linking reads their verified primary email. Without it, linking fails before anyone is sent to GitHub, and the control pod logs `the deployment App is not granted the account email addresses permission`. Connecting a workspace refuses the same way.

**Subscribe to events.** Tick **Check run**, **Check suite**, **Issue comment**, **Pull request**, **Pull request review**, **Pull request review comment**, and **Push**. The installation events (`installation`, `installation_repositories`) are delivered to every App's webhook without being asked for.

**Where can this GitHub App be installed?** → **Any account.** The whole point is that workspaces owned by other GitHub organizations can connect; *Only on this account* would restrict installation to the owning account.

Press **Create GitHub App**.

### 8.3 Collect the four values

You land on the App's **General** page. Everything TF needs is here or was generated a moment ago:

- **App ID** — the number under *About* near the top. It is the App's numeric id, **not** the *Client ID* next to it (TF reads the client id, the slug and the owner back from GitHub itself). → `TF_GITHUB_APP_ID`
- **Client secrets → Generate a new client secret** — shown once; copy it now. It authenticates the exchange that identifies the person completing an installation. → `TF_GITHUB_APP_CLIENT_SECRET`
- **Private keys → Generate a private key** — GitHub downloads `<app-name>.<date>.private-key.pem` to your machine. Keep the whole file, `BEGIN`/`END` lines included. → `TF_GITHUB_APP_PRIVATE_KEY`
- The webhook secret you generated in 8.2. → `TF_GITHUB_APP_WEBHOOK_SECRET`

### 8.4 Put them in `.env` and restart the control pod

All four must be set — a strict subset is logged at boot and the deployment runs as if none were set, so **Connect GitHub…** would answer `This deployment isn't set up to connect GitHub accounts`. Each of the three secrets can be a plain value or a mounted file via `<NAME>_FILE` ([Deployment secrets](secrets.md)); use the file form for the PEM, which is multi-line and does not survive a `.env` well.

```yaml
# compose.override.yml  (auto-merged by `docker compose`)
secrets:
  tf_gh_app_key: { file: ./secrets/github-app.private-key.pem }
services:
  control:
    secrets: [tf_gh_app_key]
```

```dotenv
# .env
TF_GITHUB_APP_ID=123456
TF_GITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/tf_gh_app_key
TF_GITHUB_APP_WEBHOOK_SECRET=<the value you pasted into the webhook form>
TF_GITHUB_APP_CLIENT_SECRET=<the client secret GitHub showed once>
```

Then:

```sh
docker compose up -d
```

Only the `control` service carries these variables — it serves the connect ceremony and the webhook receiver, and its background brain mints a connected workspace's installation tokens and seals them into the per-run bundles executors receive. An executor never holds the App key, so nothing forwards it there.

Keep the PEM `chmod 600` and owned by you; the pod makes its own copy readable to the uid it runs as ([file ownership](secrets.md#file-ownership)). In a source checkout, `secrets/` and `compose.override.yml` are gitignored.

Check it took:

1. `docker compose logs control | grep -i "deployment github app"` prints nothing. A line there names the variable that is wrong (a non-numeric App ID, a PEM that will not parse, a missing member of the four).
2. On GitHub, open the App → **Advanced → Recent Deliveries**. The `ping` GitHub sent when you created the App failed with `401`, because TF did not have the webhook secret yet. Press **Redeliver**: it now returns `204`. That proves the URL, the secret, and TLS in one go. A `302` here means something in front of TF answered instead — an identity-aware proxy such as Cloudflare Access, IAP or oauth2-proxy sends GitHub to its login page. Exempt `POST ${TF_PUBLIC_URL}/api/webhooks/github` from it (and `/api/webhooks/github/{org_id}`, which workspaces' own Apps deliver to). TF verifies every delivery's signature itself, so the exemption exposes nothing. Until then everything still works: GitHub is polled, and a webhook only makes a change land sooner.
3. In any workspace with no GitHub credential, **Workspace Settings → GitHub access** now offers **Connect GitHub…**.

### 8.5 What to expect afterwards

- **Connecting.** A workspace admin presses **Connect GitHub…**, types the login of the GitHub organization or user to connect, and confirms on GitHub. If the App is already installed there, that is the whole trip and they land back in Workspace Settings with the account connected. If it is not, GitHub's install page opens with that account preselected; they choose which repositories to grant, and the same thing happens on the way back. **Connect another account…** adds more; each is its own connect. On every one TF confirms that the person is an owner of the organization they are connecting, and that they confirmed on GitHub as the GitHub account linked to their Triage Factory user — an admin who has not linked one is told to do that first. Nothing is ever listed; the admin names the account.
- **Linking members' GitHub accounts.** Every member links the GitHub account Triage Factory matches their pull requests and reviews to. In a workspace with no App of its own, that link runs through this App: the member presses **Connect GitHub**, authorizes the App on GitHub, and comes back through the callback URL from 8.2. Nothing is installed, and an admin can link theirs before the workspace has connected an account, which the connect ceremony then requires. Multi mode has no token paste for this.
- **Tenancy.** The App being deployment-wide does not make one workspace able to see another's repositories. Each workspace sees only the accounts it connected, and an installation belongs to at most one workspace — connecting an account that another workspace already holds is refused.
- **Installed but not yet connected.** If an organization *member* without owner rights names that organization, GitHub sends an install request to an owner instead of installing, and TF shows `Install request sent`; once the owner approves, the admin connects it as above. Someone who installs from the App's public page on GitHub, outside any workspace, lands on TF's `/github/installed` page, which asks them to sign in, pick the workspace, and name the account. Both are ordinary states, not faults.
- **Moving a workspace that already has its own App.** Its GitHub access card offers **Switch to the deployment's App…**, which asks for the account first: the workspace's own App is then disconnected (the registration, its installations and its key are removed from TF; the App itself stays registered on GitHub) and the admin goes straight through the Connect flow above for that account. The workspace has no GitHub access for the length of that trip; an admin who leaves GitHub without finishing is taken back to the setup wizard.
- **Leaving.** A workspace can disconnect from the deployment App in Workspace Settings and then register its own App or import one. Details of installations, grants and what an admin's change on GitHub does are in [GitHub App installations](../concepts/github-app-installations.md).

If a connection is refused, the message names the cause. The ones that point back at this section:

| Message | Fix |
|---|---|
| `This deployment isn't set up to connect GitHub accounts.` | One or more of the four variables is missing or malformed on the control pod — see the boot log in 8.4. |
| `…missing the “Request user authorization (OAuth) during installation” setting.` | Tick that box on the App (8.2). |
| `…not granted the organization members permission` | Add Organization → Members: Read-only on the App (8.2), then have each installed account accept the updated permissions on GitHub. |
| `…not granted the account email addresses permission` (log) | Add Account → Email addresses: Read-only on the App (8.2). Members are asked to approve it on GitHub the next time they link. |
| `This workspace is pointed at <host>; the deployment's GitHub App is on <host>.` | That workspace's GitHub is not the deployment default (8.1). It brings its own App. |
| `GitHub rejected the deployment App's ID and private key` (log) | `TF_GITHUB_APP_ID` names a different App than the PEM belongs to. Re-check the App ID (not the client id) and the key. |

### 8.6 (Optional) Configure the deployment commit signer

A workspace on the deployment App commits as the App's bot, and GitHub never marks a commit Verified as a bot unless GitHub made the commit itself. The **deployment commit signer** fixes that for every workspace on the deployment App at once: a GitHub **user** account whose name and address its agents' commits carry, and whose SSH signing key Triage Factory signs them with, so GitHub marks them Verified and a repository that requires signed commits accepts them. A workspace that registers a signer of its own in **Workspace Settings → Commit signing** uses that one instead, and a workspace whose own signer is broken commits unsigned rather than falling back to the deployment's. The App still pushes and opens the pull requests; the account only lends its identity and key.

What a signer account is, and why it should be one made for the purpose rather than a person's, is in [Commit signing: creating the signer account](../concepts/commit-signing.md). In short: create a dedicated user account on the deployment's GitHub (8.1), give it a profile name, since that is the name every commit carries, and verify an email on it.

1. **Generate the key** on a machine you trust, with no passphrase (Triage Factory cannot unlock one):

   ```sh
   ssh-keygen -t ed25519 -N '' -C 'Triage Factory commit signing' -f ./secrets/commit-signer
   ```

2. **Add the public half to the account.** Signed in as the signer account: **Settings → SSH and GPG keys → New SSH key**, **Key type: Signing key**, paste `./secrets/commit-signer.pub`. Key titles are public, so name it after the product, not a customer.

3. **Configure the control pod** and restart it. The key is multi-line, so mount it as a file:

   ```yaml
   # compose.override.yml
   secrets:
     tf_commit_signer_key: { file: ./secrets/commit-signer }
   services:
     control:
       secrets: [tf_commit_signer_key]
   ```

   ```dotenv
   # .env
   TF_GITHUB_COMMIT_SIGNER_LOGIN=TriageFactory
   TF_GITHUB_COMMIT_SIGNER_KEY_FILE=/run/secrets/tf_commit_signer_key
   # Optional on github.com, where unset means the account's noreply address
   # (<id>+<login>@users.noreply.github.com, which survives a rename of the
   # login). Required on GitHub Enterprise Server, which has no noreply address:
   # an address verified on the account.
   # TF_GITHUB_COMMIT_SIGNER_EMAIL=agent@yourcompany.com
   ```

| Setting | Required | Meaning |
|---|---|---|
| `TF_GITHUB_COMMIT_SIGNER_LOGIN` | with the key | The signer account's login. The first check reads the account it names; from then on the account is followed by its id, so renaming the login changes nothing. |
| `TF_GITHUB_COMMIT_SIGNER_KEY` | with the login | The unencrypted ed25519 private key in OpenSSH format. A secret: it honours `TF_GITHUB_COMMIT_SIGNER_KEY_FILE` ([Deployment secrets](secrets.md)). |
| `TF_GITHUB_COMMIT_SIGNER_EMAIL` | off github.com | The committer address. It must be verified on the account, or GitHub reports the commits `unverified_email`. Don't use the address that recovers the account: it is on every commit. |

A login without a key, a key without a login, an email alone, a key that is not an unencrypted ed25519 key, or no email off github.com refuses the boot with the variable named. Only the `control` service carries these, and every control pod should carry the same three: each pod signs whichever managed workspace's signature request it claims, and a sandboxed run's sidecar sends it the commit's digest, so **no executor ever holds the key**. Local mode ignores them; a local org registers its own signer.

Check it took. Within a minute of the restart a control pod checks the account and, finding the key, makes the signer **active** and logs `deployment commit signer is active; managed workspaces sign as it`. A `deployment commit signer is pending` line followed by `the deployment commit signer's key is not among the account's signing keys` means step 2 is missing; GitHub's listing can also take a minute to show a key just added. Then `triagefactory commit-signer status`, run on a control container (`docker compose exec control triagefactory commit-signer status`), shows the signer, `triagefactory commit-signer verify` checks it again on demand, and a managed workspace's **Workspace Settings → Commit signing** reads `Commits are signed as TriageFactory, this deployment's signing account`. Runs pick it up at their next start.

To **rotate the key**, add the new public half to the account first, then change the key and restart the control pods. A new key starts a new status row, pending until a check finds it on the account, so a key not yet added stops managed workspaces signing until it is. A control pod waits up to 5 seconds at boot for its first check before it signs anything, so with the key already on the account the first pod on the new key has activated it before it answers a run. To sign as a **different account**, generate a new key the same way: a key stays with the account a check first found it on, so changing only `TF_GITHUB_COMMIT_SIGNER_LOGIN` leaves the key bound to the old account, and each control pod logs a WARN saying so. Leave the old public key on the account unless it may have leaked: on a GHES with persistent verification off, deleting it turns the commits it signed Unverified.

When the signer breaks (its key removed from the account, or GitHub failing a pushed commit for its address) every managed workspace commits unsigned from its next signature on, each control pod logs `deployment commit signer is broken; managed workspaces commit unsigned until an operator fixes it`, and `tf_deployment_commit_signer_up` reads 0 ([Monitoring](monitoring.md#commit-signer-health)). Fix the cause on the account, then run `triagefactory commit-signer verify` on a control container. A key put back on the account restores it on the next check without one.

Add the signer's login to your repositories' CLA allowlist, if they run one, before enabling it: its commits are authored as that account.

## 9. (Optional) Register a deployment Linear app

A workspace that installs Triage Factory in Linear normally registers a Linear OAuth app of its own first, from **Workspace Settings → Linear OAuth app**. A **deployment Linear app** is the alternative: one app, registered once, that every workspace with no app of its own installs through. A workspace admin clicks **Install Triage Factory** and approves on Linear, and there is nothing to create or paste.

- **Precedence is the same as Jira's.** A workspace that registered its own app keeps using it. The deployment app is what a workspace with no app gets — a default, never an override.
- **Leaving both variables unset changes nothing.** Every workspace keeps registering its own app or pasting an API key.
- **Multi mode only.** Local mode ignores the variables; a distributed binary cannot ship a shared client secret.

### 9.1 Create the app in Linear

Linear recommends owning an app from a dedicated Linear workspace, so every admin of that workspace can manage it rather than only the person who created it. Sign in to that workspace as an admin and open this link, with `https://tf.example.com` replaced by your `TF_PUBLIC_URL` (three places) and `Example%20Corp` by the name workspace admins should see as the app's developer (2–80 characters, a space written `%20`):

```text
https://linear.app/settings/api/applications/new?distribution=public&developer.name=Example%20Corp&oauth.client_name=Triage%20Factory&oauth.client_uri=https://tf.example.com&oauth.redirect_uris=https://tf.example.com/api/linear/install/callback&oauth.redirect_uris=https://tf.example.com/api/linear/connect/callback&oauth.grant_types=authorization_code
```

The link pre-fills Linear's new-application form. Check it against this list before creating the app:

| Parameter | Value |
|---|---|
| `distribution` | `public`, so any Linear workspace can authorize the app. A private app can be installed only in the workspace that owns it. |
| `developer.name` | Your name, as the app's developer. |
| `oauth.client_name` | `Triage Factory`. Workspace admins see it on Linear's install page, and it is the name of the app user Triage Factory acts as. Linear refuses a name that contains "Linear". |
| `oauth.client_uri` | Your `TF_PUBLIC_URL`. |
| `oauth.redirect_uris` | Both `${TF_PUBLIC_URL}/api/linear/install/callback` (the workspace install) and `${TF_PUBLIC_URL}/api/linear/connect/callback` (members connecting their own accounts). They are absolute and carry no workspace id: one app serves every workspace, and Linear matches redirect URIs exactly. Linear's documentation requires them to be `https`, so the deployment needs a TLS `TF_PUBLIC_URL`; Linear also accepts `http://localhost` for a development stack. |
| `oauth.grant_types` | `authorization_code` only. Triage Factory does not use client credentials: they mint tokens only for the workspace that owns the app. |

Leave webhooks off; Triage Factory polls Linear. Create the app, then copy its **client ID** and **client secret** from the app's page — Linear does not hand them back to the link that created it.

### 9.2 Put the pair in `.env` and restart the control pod

```dotenv
# .env
TF_LINEAR_CLIENT_ID=<the app's client ID>
TF_LINEAR_CLIENT_SECRET=<the app's client secret>
```

The secret can also be a mounted file via `TF_LINEAR_CLIENT_SECRET_FILE` ([Deployment secrets](secrets.md)). Then:

```sh
docker compose up -d
```

Only the `control` service carries these variables. It serves the install and Connect ceremonies, and its background brain refreshes each install's tokens and seals them into the per-run bundles executors receive, so an executor never holds the client secret.

Check it took:

1. `docker compose logs control | grep -i "deployment linear app"` prints nothing. A line there names the variable that is missing.
2. In any workspace with no Linear app of its own, **Workspace Settings → Linear OAuth app** reads *Using this deployment's Triage Factory app*, and **Linear connection** offers **Install Triage Factory**.

### 9.3 What to expect afterwards

- **Installing.** A Linear workspace admin installs the app; Linear does not let a member install an app for the workspace. Triage Factory then polls and writes as the app's user. Private Linear teams have to be shared with the app on its page in Linear.
- **Switching from a key to the app.** A personal key reads the private teams its owner is a member of; the app reads only the ones shared with it. Before installing over a key, **Linear connection** lists the tracked teams that are private. Afterwards it names any tracked team the app can't read. The tracker leaves those teams' issues tracked and unrefreshed rather than retiring them, and they refresh once the team is shared.
- **Tenancy.** The app being deployment-wide does not let one Triage Factory workspace see another's Linear. A Linear workspace can be installed in only one Triage Factory workspace; a second is refused with *That Linear workspace is already connected to another Triage Factory organization*, and the tokens Linear minted for it are revoked.
- **Third-party application approvals.** On a paid Linear plan an admin can require approval before members authorize a third-party app. The workspace install is itself that approval. A member who later clicks **Connect Linear** may still be asked to request approval; this has not been verified. A member who is refused can ask a Linear admin to approve Triage Factory, or paste a personal API key instead.
- **Rotating the client secret.** In Linear, open the app under **Settings → Administration → API** and rotate its secret, then put the new one in `.env` and run `docker compose up -d`. Tokens already issued keep working: each refreshes with the new secret the next time it expires, so no workspace has to install again. Triage Factory refreshes with the secret it booted with, so restart soon after rotating.
- **Removing the app.** Every install and Connect grant made through the deployment app refreshes only with its secret. Unsetting the pair leaves those workspaces' Linear connections working until their access tokens expire, within a day, and then failing until each workspace disconnects Linear and connects again through an app of its own or a personal API key. Their settings offer registering an app of their own as soon as the control pod restarts without the pair. To change the secret, rotate it rather than removing the app.

## Next steps

- [Deployment secrets](secrets.md) — supplying secrets from files (`*_FILE` / Docker & K8s secrets) and how TF handles them
- [GitHub App installations](../concepts/github-app-installations.md) — how a connected account's installation and grants propagate, for both a workspace's own App and the deployment App
- [Monitoring & health checks](monitoring.md) — `/api/health`, `/readyz`, executor `/healthz`, `:9464` metrics, and traces (`TF_TRACES_ENDPOINT` + the bundled Tempo/Grafana stack)
- [Scaling out](scaling.md) — control + N executors, per-role DB pools, HA reverse proxy
- [Client IP & trusted proxies](networking.md) — `TF_TRUSTED_PROXY_CIDR` behind a load balancer
- [Object storage](storage.md) — workspace snapshots and team knowledge; SeaweedFS + BYO S3/R2
- [Rotating the signing key](key-rotation.md)
- [SSO with Microsoft Entra (SAML)](sso-entra.md)
- [Security model](../security/) — privilege separation, seccomp profile, release verification
