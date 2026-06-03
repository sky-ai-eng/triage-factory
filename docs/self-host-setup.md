# Self-host setup (multi-mode)

This is the operator-facing install flow for the multi-tenant deployment. **Local mode (default, `TF_MODE=local`) needs none of this** — install Triage Factory normally, no Postgres or GoTrue required.

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
- `TF_PUBLIC_URL` — your public URL (no trailing slash)
- `GH_CLIENT_ID` / `GH_CLIENT_SECRET` — from step 1
- `TF_SESSION_ENCRYPTION_KEY` — 32 random bytes; AES-GCM master key for the access/refresh tokens stored at rest in `public.sessions`. **Generate with `openssl rand -hex 32`.** Rotating this key invalidates every existing session (ciphertext can't be decrypted) — plan it as a forced re-auth event.
- `TF_COOKIE_SECRET` — 32 random bytes; HMAC-SHA256 key for the short-lived OAuth state cookie (carries PKCE verifier + CSRF token). **Generate with `openssl rand -hex 32`.** Kept distinct from `TF_SESSION_ENCRYPTION_KEY` so the two rotate independently — rotating only this one invalidates in-flight OAuth handshakes (10-minute window), not active sessions.
- `TF_BLOB_ACCESS_KEY` / `TF_BLOB_SECRET_KEY` — credentials for the durable-workspace object store. The compose stack feeds these to the bundled `minio` service's root user *and* to TF's S3 client — one identity, one input, so they can't drift. **Generate each with `openssl rand -hex 32`** (access key must be ≥3 chars, secret ≥8 — a 64-char hex clears both). The endpoint (`http://minio:9000`), bucket (`tf-workspaces`), and region (`us-east-1`) all default in `docker-compose.yml`; leave them unset for self-host. See [Durable workspace storage](#durable-workspace-storage-minio) below for the BYO / hosted-Supabase path.

> **Rotating passwords:** edit `.env` and re-run `docker compose up -d`. A short-lived `postgres-postinit` sidecar runs on every boot and reapplies `ALTER USER` for the non-superuser roles (`supabase_auth_admin`, `authenticator`), so password changes propagate without wiping the data volume. Rotating `POSTGRES_PASSWORD` itself requires more care — that's the superuser's password and Postgres only honors the env var on first init, so changing it means `ALTER USER postgres WITH PASSWORD '...'` by hand inside the running container.

## 3. Generate the JWT signing key

```sh
./triagefactory jwk-init --write-env .env
```

This generates a fresh RS256 keypair, formats it as a JWKS containing both private and public material, and appends both `GOTRUE_JWT_KEYS=<json>` and `GOTRUE_JWT_SECRET=...` to `.env`. The private side stays in `.env` (read only by GoTrue); only the public side is published at GoTrue's `/.well-known/jwks.json` endpoint. The generated `GOTRUE_JWT_SECRET` is also required by the compose stack, so if you manage these values manually, do not omit it.

Re-running `jwk-init --write-env .env` appends a *second* line, which works (GoTrue picks the last one) but is messy — clear the existing line first if you're rotating.

## 4. Bring up the stack

```sh
docker compose up -d
```

This brings up the full stack: Postgres, GoTrue, MinIO (the durable-workspace object store), and the `triagefactory` service running the TF binary in a container. The Postgres image is `supabase/postgres`, which pre-provisions the `auth` schema, the `supabase_auth_admin` role GoTrue connects as, the `authenticator` role TF's app pool uses, and the vault / pgsodium / pgvector extensions for per-org secret storage.

On first boot, the `postgres-postinit` sidecar reconciles the `supabase_auth_admin` and `authenticator` role passwords, the `minio-postinit` sidecar creates the workspace bucket (idempotent — `mc mb --ignore-existing`), then the `triagefactory` container's entrypoint runs `triagefactory migrate up` against the admin DSN before starting the server.

Smoke-check the stack came up:

```sh
docker compose ps                         # all services should be "running"/"healthy" (postgres-postinit and minio-postinit show "exited(0)")
curl -fsS http://localhost:3000/api/health   # 200 OK
curl -s http://localhost:9999/.well-known/jwks.json | jq .   # JWKS with one public RSA key
curl -fsS http://localhost:9000/minio/health/live   # MinIO S3 API is live
```

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

## Durable workspace storage (MinIO)

A blueprint's workspace — the git worktree plus the scratch space its steps hand off through — must survive the executor that created it (a yield to a human can outlast the process; an executor can scale down mid-run). The TF binary snapshots that workspace to an **S3-compatible object store** and rehydrates it on resume; the host worktree is only a warm cache.

Self-host runs **MinIO** for this: one self-contained S3 container, no Postgres or JWT coupling. The workspace snapshots are opaque server-internal tarballs — they need a dumb bucket, not a storage API's RLS / resumable-upload / CDN layer. The `minio` service in `docker-compose.yml` provides exactly that, and a one-shot `minio-postinit` sidecar creates the bucket on every `up` (`mc mb --ignore-existing` — idempotent).

The TF binary talks to it through the `TF_BLOB_*` env (read only in multi mode; local mode writes blobs to `~/.triagefactory/blobs` and runs none of this). The compose stack derives MinIO's root user/password from the single `TF_BLOB_ACCESS_KEY` / `TF_BLOB_SECRET_KEY` pair you set in `.env`, so server, bucket sidecar, and client all share one credential — there's no second `MINIO_ROOT_*` input to drift.

Smoke-test the round-trip against the composed MinIO (exercises the same bucket and creds TF's S3 client uses):

```sh
# set -a; source .env; set +a   # load TF_BLOB_* into your shell first
docker compose run --rm --no-deps minio-postinit '
  mc alias set tf http://minio:9000 "$TF_BLOB_ACCESS_KEY" "$TF_BLOB_SECRET_KEY" &&
  echo hello | mc pipe tf/"${TF_BLOB_BUCKET:-tf-workspaces}"/smoke.txt &&
  mc cat tf/"${TF_BLOB_BUCKET:-tf-workspaces}"/smoke.txt &&        # -> hello
  mc rm tf/"${TF_BLOB_BUCKET:-tf-workspaces}"/smoke.txt'
```

The console (port `9001`, `http://localhost:9001`) is handy for eyeballing snapshot objects; log in with the same access key / secret.

### Hosted Supabase Storage, S3, or R2 (SaaS / BYO)

The same `aws-sdk-go-v2` client (path-style addressing, configurable `BaseEndpoint`) drives **any** S3-protocol backend — there's no compose change to point it elsewhere. Set `TF_BLOB_ACCESS_KEY` / `TF_BLOB_SECRET_KEY` to that backend's keys and override the endpoint (and bucket / region as needed) in `.env`:

```sh
TF_BLOB_ENDPOINT=https://<ref>.supabase.co/storage/v1/s3   # base path IS preserved
TF_BLOB_BUCKET=tf-workspaces
TF_BLOB_REGION=us-east-1
```

`TF_BLOB_ENDPOINT` is a full URL — its scheme selects TLS and any base path (Supabase Storage's `/storage/v1/s3`) is kept intact. The bundled `minio` service still starts in this configuration but goes unused; drop it via a compose override (and remove `minio-postinit` from `triagefactory`'s `depends_on` in that override) if you don't want it running. Pre-create the bucket on the hosted side (the `minio-postinit` sidecar only ensures the bucket on the bundled MinIO).

## Rotating the signing key

The current tooling supports **single-key replacement** only:

1. Remove the existing `GOTRUE_JWT_KEYS=` and `GOTRUE_JWT_SECRET=` lines from `.env`
2. `./triagefactory jwk-init --write-env .env`
3. Recreate GoTrue so it picks up the new env: `docker compose up -d gotrue`

`docker compose up -d` (without `stop`/`start`) detects the env diff against the existing container and recreates it. `docker compose start gotrue` would reuse the cached env from container creation and the new key would NOT be loaded — this is a common foot-gun. The Verifier picks up the new key automatically on the next unknown-`kid` lookup — no TF restart needed.

**Caveat:** any access tokens still in flight that were signed by the old key will fail verification as soon as GoTrue restarts. GoTrue's default access-token lifetime is 1 hour, so the practical impact is "users with active sessions need to re-authenticate." For zero-downtime overlap rotation (publish both old and new keys, switch the signing kid, wait for the old to expire, drop the old) you'd need to maintain a multi-key `GOTRUE_JWT_KEYS` array by hand — our `jwk-init` doesn't currently support merge semantics. Planned for a future ticket; for now, rotate during low-traffic windows or treat each rotation as a forced re-auth event.
