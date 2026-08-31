# Self-host setup (multi-mode)

This is the operator-facing install flow for the multi-tenant deployment. **Local mode (default, `TF_MODE=local`) needs none of this** — install Triage Factory normally, no Postgres or GoTrue required (see [local mode](../local-mode/)).

Once the stack is up, the operational topics live in their own guides:
[Monitoring](monitoring.md), [Scaling out](scaling.md),
[Client IP & trusted proxies](networking.md), [Durable workspace storage](storage.md),
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
- `TF_SYSTEM_PASSWORD` — password for the `tf_system` role: the least-privilege database role executor pods' admin pool connects as instead of `supabase_admin` (superuser). BYPASSRLS but no DDL, and a narrow, enumerated table grant list — see the `tf_system` section of `internal/db/migrations-postgres/202605130001_pg_baseline.sql`. Executors are the most-exposed machine class (they run agent workloads), so this is the difference between a compromised executor holding a superuser DSN and one holding a role that can only touch the run-execution tables it needs. Control pods are unaffected — they keep the `supabase_admin` DSN. **Generate with `openssl rand -hex 32`.** The role ships `NOLOGIN` in the baseline; a dedicated `postgres-postinit-system` sidecar grants it `LOGIN` on every `docker compose up` — split from the `postgres-postinit` sidecar above because `tf_system` doesn't exist until the `triagefactory` container has run its migration, so this sidecar waits on `triagefactory` being healthy rather than racing it. Always required: the default stack is the co-located control + executor split, and the `executor` service connects as `tf_system`.
- `TF_PUBLIC_URL` — your public URL (no trailing slash)
- `GH_CLIENT_ID` / `GH_CLIENT_SECRET` — from step 1
- `TF_SESSION_ENCRYPTION_KEY` — 32 random bytes; AES-GCM master key for the access/refresh tokens stored at rest in `public.sessions`. **Generate with `openssl rand -hex 32`.** Rotating this key invalidates every existing session (ciphertext can't be decrypted) — plan it as a forced re-auth event.
- `TF_COOKIE_SECRET` — 32 random bytes; HMAC-SHA256 key for the short-lived OAuth state cookie (carries PKCE verifier + CSRF token). **Generate with `openssl rand -hex 32`.** Kept distinct from `TF_SESSION_ENCRYPTION_KEY` so the two rotate independently — rotating only this one invalidates in-flight OAuth handshakes (10-minute window), not active sessions.
- `TF_SECRET_ENCRYPTION_KEY` — 32 random bytes; AES-GCM master key for org + per-user **integration secrets** (GitHub App PEM / client-secret / webhook-secret, PATs, Jira tokens) stored at rest in `public.org_secrets`. **Generate with `openssl rand -hex 32`.** TF encrypts these app-side and stores only opaque ciphertext, so they are **not** in Supabase Vault / pgsodium — a routine `docker compose down && up` (or any postgres container recreate) no longer loses them. Kept distinct from `TF_SESSION_ENCRYPTION_KEY` so the two rotate independently — a session-key rotation must not nuke integration secrets. Rotating *this* key invalidates every stored secret (the ciphertext can no longer be decrypted), so plan it as a "re-enter your integration credentials" event. The same variable also governs **local-mode headless installs** (a single-binary deploy on a box with no OS keychain) — there it encrypts `~/.triagefactory/secrets.enc` instead of `public.org_secrets`; see [local mode § Secret storage](../local-mode/secret-storage.md).
- `TF_BLOB_ACCESS_KEY` / `TF_BLOB_SECRET_KEY` — credentials for the durable-workspace object store. The compose stack templates these into the bundled `seaweedfs` service's S3 identity *and* feeds them to TF's S3 client — one identity, one input, so they can't drift. **Generate each with `openssl rand -hex 32`** (access key must be ≥3 chars, secret ≥8 — a 64-char hex clears both, and keeps them JSON-safe inside SeaweedFS's `s3.json`). The endpoint (`http://seaweedfs:8333`), bucket (`tf-workspaces`), and region (`us-east-1`) all default in `docker-compose.yml`; leave them unset for self-host. See [Durable workspace storage](storage.md) for the BYO / hosted-Supabase path.
- `TF_ATLASSIAN_CLIENT_ID` / `TF_ATLASSIAN_CLIENT_SECRET` — *(optional)* the deployment's Atlassian OAuth (3LO) app, used for one-click "Connect Jira" against Cloud orgs. Deployment config in the same class as the GoTrue signing material — they live here in `.env`, **not** the keychain/`org_secrets`, because one Atlassian app is registered for the whole deployment. Leave unset to run without a deployment Atlassian app; each org can then supply its own from **Workspace Settings → Atlassian OAuth app** (a per-org override always wins over this default). Register the app in the [Atlassian developer console](https://developer.atlassian.com/console/myapps/) and set its callback (redirect) URL to `${TF_PUBLIC_URL}/api/orgs/{org}/jira/connect/callback`.
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

This brings up the full stack: Postgres, GoTrue, SeaweedFS (the durable-workspace object store), and the TF binary as the co-located control + executor pair — the `triagefactory` service (the control pod: API/WS, background brain, migrations) plus one `executor` service (the sandbox worker: run dispatcher, gVisor jails, per-run credential sidecars). Multi mode is always this split; there is no single-process option (`TF_ROLE=all` refuses to boot in multi), and only the executor container carries sandbox capabilities. The Postgres image is `supabase/postgres`, which pre-provisions the `auth` schema, the `supabase_auth_admin` role GoTrue connects as, the `authenticator` role TF's app pool uses, and the `pgvector` extension. The image also ships the `vault` / `pgsodium` extensions, but TF doesn't use them — per-org secret storage is app-encrypted (see `TF_SECRET_ENCRYPTION_KEY` above), not Supabase Vault.

On first boot, the `postgres-postinit` sidecar reconciles the `supabase_auth_admin` and `authenticator` role passwords, the `seaweedfs-postinit` sidecar creates the workspace bucket (idempotent — `head-bucket || create-bucket` via aws-cli), then the `triagefactory` container's entrypoint runs `triagefactory migrate up` against the admin DSN before starting the server. Once `triagefactory` is healthy, a third sidecar, `postgres-postinit-system`, reconciles the `tf_system` password — it has to run last because `tf_system` is itself created by that migration, so it doesn't exist yet at the point the other two sidecars run.

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

## Next steps

- [Deployment secrets](secrets.md) — supplying secrets from files (`*_FILE` / Docker & K8s secrets) and how TF handles them
- [Monitoring & health checks](monitoring.md) — `/api/health`, `/readyz`, executor `/healthz`, `:9464` metrics, and traces (`TF_TRACES_ENDPOINT` + the bundled Tempo/Grafana stack)
- [Scaling out](scaling.md) — control + N executors, per-role DB pools, HA reverse proxy
- [Client IP & trusted proxies](networking.md) — `TF_TRUSTED_PROXY_CIDR` behind a load balancer
- [Durable workspace storage](storage.md) — SeaweedFS + BYO S3/R2
- [Rotating the signing key](key-rotation.md)
- [SSO with Microsoft Entra (SAML)](sso-entra.md)
- [Security model](../security/) — privilege separation, seccomp profile, release verification
