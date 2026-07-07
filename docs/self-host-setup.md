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
- `TF_SECRET_ENCRYPTION_KEY` — 32 random bytes; AES-GCM master key for org + per-user **integration secrets** (GitHub App PEM / client-secret / webhook-secret, PATs, Jira tokens) stored at rest in `public.org_secrets`. **Generate with `openssl rand -hex 32`.** TF encrypts these app-side and stores only opaque ciphertext, so they are **not** in Supabase Vault / pgsodium — a routine `docker compose down && up` (or any postgres container recreate) no longer loses them. Kept distinct from `TF_SESSION_ENCRYPTION_KEY` so the two rotate independently — a session-key rotation must not nuke integration secrets. Rotating *this* key invalidates every stored secret (the ciphertext can no longer be decrypted), so plan it as a "re-enter your integration credentials" event. The same variable also governs **local-mode headless installs** (a single-binary deploy on a box with no OS keychain) — there it encrypts `~/.triagefactory/secrets.enc` instead of `public.org_secrets`; see `docs/usage.md` § Secret storage.
- `TF_BLOB_ACCESS_KEY` / `TF_BLOB_SECRET_KEY` — credentials for the durable-workspace object store. The compose stack templates these into the bundled `seaweedfs` service's S3 identity *and* feeds them to TF's S3 client — one identity, one input, so they can't drift. **Generate each with `openssl rand -hex 32`** (access key must be ≥3 chars, secret ≥8 — a 64-char hex clears both, and keeps them JSON-safe inside SeaweedFS's `s3.json`). The endpoint (`http://seaweedfs:8333`), bucket (`tf-workspaces`), and region (`us-east-1`) all default in `docker-compose.yml`; leave them unset for self-host. See [Durable workspace storage](#durable-workspace-storage-seaweedfs) below for the BYO / hosted-Supabase path.
- `TF_ATLASSIAN_CLIENT_ID` / `TF_ATLASSIAN_CLIENT_SECRET` — *(optional)* the first-party Atlassian OAuth (3LO) app TF uses for one-click "Connect Jira" against Cloud orgs. Deployment config in the same class as the GoTrue signing material — they live here in `.env`, **not** the keychain/vault, because TF registers one first-party Atlassian app for the whole deployment. Leave unset to run without a hosted Atlassian app; each org can then supply its own from **Workspace Settings → Atlassian OAuth app** (a per-org override always wins over this default). Register the app in the [Atlassian developer console](https://developer.atlassian.com/console/myapps/) and set its callback (redirect) URL to `${TF_PUBLIC_URL}/api/orgs/{org}/jira/connect/callback`.
- `TF_TRUSTED_PROXY_CIDR` / `TF_CAPTURE_CLIENT_IP` — *(recommended when behind a proxy)* govern how TF derives the client IP it records on sessions, on the SOC2 auth audit log, and keys the pre-auth rate limiter by. **If TF sits behind a load balancer or CDN, set `TF_TRUSTED_PROXY_CIDR`** — otherwise `X-Forwarded-For` is ignored and every request is attributed to the LB. See [Client IP & trusted proxies](#client-ip--trusted-proxies) below for the full rationale and per-topology guidance.

> **Rotating passwords:** edit `.env` and re-run `docker compose up -d`. A short-lived `postgres-postinit` sidecar runs on every boot and reapplies `ALTER USER` for the non-superuser roles (`supabase_auth_admin`, `authenticator`), so password changes propagate without wiping the data volume. Rotating `POSTGRES_PASSWORD` itself requires more care — that's the superuser's password and Postgres only honors the env var on first init, so changing it means `ALTER USER postgres WITH PASSWORD '...'` by hand inside the running container.

## 3. Generate the JWT signing key

```sh
./triagefactory jwk-init --write-env .env
```

This generates a fresh RS256 keypair, formats it as GoTrue’s `GOTRUE_JWT_KEYS` JSON array of JWK objects (including both private and public material), and writes `GOTRUE_JWT_KEYS=<json>`, `GOTRUE_JWT_SECRET=...`, and `TF_GOTRUE_SERVICE_ROLE_TOKEN=...` to `.env`. The private side stays in `.env` (read only by GoTrue); only the public side is published at GoTrue's `/.well-known/jwks.json` endpoint. The generated `GOTRUE_JWT_SECRET` is also required by the compose stack, so if you manage these values manually, do not omit it. (`TF_GOTRUE_SERVICE_ROLE_TOKEN` is only needed once you enable SSO — see [the SSO guide](sso-entra.md) — but it's harmless to mint up front.)

`jwk-init --write-env` is **idempotent and upserts in place** — it replaces each variable's line rather than appending, collapsing any stale duplicates, so re-running is clean. Crucially, if `.env` already has a `GOTRUE_JWT_KEYS`, jwk-init **reuses that keypair** and writes only the service-role token; it does **not** rotate the signing key. To deliberately rotate the keypair, pass `--rotate` (see [Rotating the signing key](#rotating-the-signing-key)).

## 4. Bring up the stack

```sh
docker compose up -d
```

This brings up the full stack: Postgres, GoTrue, SeaweedFS (the durable-workspace object store), and the `triagefactory` service running the TF binary in a container. The Postgres image is `supabase/postgres`, which pre-provisions the `auth` schema, the `supabase_auth_admin` role GoTrue connects as, the `authenticator` role TF's app pool uses, and the vault / pgsodium / pgvector extensions for per-org secret storage.

On first boot, the `postgres-postinit` sidecar reconciles the `supabase_auth_admin` and `authenticator` role passwords, the `seaweedfs-postinit` sidecar creates the workspace bucket (idempotent — `head-bucket || create-bucket` via aws-cli), then the `triagefactory` container's entrypoint runs `triagefactory migrate up` against the admin DSN before starting the server.

Smoke-check the stack came up:

```sh
docker compose ps                         # all services should be "running"/"healthy" (postgres-postinit and seaweedfs-postinit show "exited(0)")
curl -fsS http://localhost:3000/api/health   # 200 OK
curl -s http://localhost:9999/.well-known/jwks.json | jq .   # JWKS with one public RSA key
```

### Monitoring

Point your load balancer / uptime check at `GET /readyz` (a bare path, not under `/api/`) rather than `/api/health` above. `/api/health` is a liveness probe only — no DB or integration check, by design, so a flapping dependency can't trip a platform's auto-restart-on-liveness-failure. `/readyz` is the readiness signal: it 503s on a **hard** failure (DB unreachable, migrations not applied, or a poller's base-tick loop stalled/crashed) and reports, without ever 503ing on them, **soft** signals — each org's last-successful-poll age vs its configured poll interval, and a count of in-flight (`queued`/`running`) agent runs:

```sh
curl -fsS http://localhost:3000/readyz | jq .
```

```json
{
  "status": "ok",
  "checked_at": 1783300042,
  "version": "v1.12.0",
  "checks": {"db": "ok", "migrations": "ok", "poller_github": "ok", "poller_jira": "ok"},
  "sources": {
    "github": {"<org_id>": {"last_success_unix": 1783300000, "age_seconds": 42, "interval_seconds": 300}},
    "jira":   {"<org_id>": {"last_success_unix": 1783299900, "age_seconds": 142, "interval_seconds": 300}}
  },
  "active_runs": 3
}
```

Alert on `age_seconds > 3 × interval_seconds` for whichever source/org you care about — that threshold is yours to pick; `/readyz` reports the raw numbers rather than guessing at your alerting policy (its own top-level `status` field applies that same 3x default and surfaces it as `"degraded"`, purely as a quick-glance dashboard signal, never as a 503). `/readyz` is unauthenticated by design, same as `/api/health` — the response carries only opaque org IDs, never repo names, usernames, or other tenant data, so there is no `?verbose=` mode and none is needed.

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

## Client IP & trusted proxies

TF records the client IP in three places, all fed by one extractor:

1. `sessions.ip_addr` — session forensics.
2. `auth_events.ip_address` — the SOC2 authentication audit log.
3. The pre-auth **per-IP rate limiter** — a brute-force / abuse control. A spoofable key is a real security hole: rotate fake values to evade the per-IP cap, or stamp a victim's IP to get them throttled.

`X-Forwarded-For` is *appended* by each proxy in the path, never overwritten — so its leftmost entry is whatever the original caller typed, i.e. attacker-controlled. TF therefore trusts `X-Forwarded-For` **only** when the request's direct peer is a proxy you've declared trusted, and then walks the header right-to-left (newest hop first), skipping trusted hops, to the first untrusted address — the real client.

Configure it with two env vars (multi mode only — local mode is single-user and ignores them):

- **`TF_TRUSTED_PROXY_CIDR`** — comma-separated CIDRs of your trusted upstream proxies (a bare IP is accepted, treated as a `/32` or `/128`). Determines which direct peers unlock `X-Forwarded-For`. An IPv4 CIDR also matches a dual-stack proxy whose connections arrive as IPv4-mapped IPv6 (`::ffff:10.0.0.1`, common with nginx on Linux and AWS ALB); add IPv6 CIDRs only for proxies that connect over native IPv6.
- **`TF_CAPTURE_CLIENT_IP`** — boolean, default `true`. Set `false` to capture no IP at all (store `NULL`), for data-minimization-conscious deployments.

| Deployment | Config | Result |
| -- | -- | -- |
| **SaaS / behind a stable edge** | `TF_TRUSTED_PROXY_CIDR` = LB/CDN egress range | accurate, unspoofable |
| **Self-host, directly exposed** | leave unset | `RemoteAddr` = real peer, unspoofable |
| **Self-host, behind your own LB** | their proxy CIDR(s) | accurate, unspoofable |
| **Privacy-sensitive** | `TF_CAPTURE_CLIENT_IP=false` (or `TF_TRUSTED_PROXY_CIDR=none`) | `NULL` IP |

**Secure default:** with `TF_TRUSTED_PROXY_CIDR` unset, `X-Forwarded-For` is ignored entirely and the client IP is the direct peer (`RemoteAddr`). That's never spoofable — but if TF actually *is* behind a proxy, every request collapses onto the proxy's IP: the per-IP rate limiter becomes one global bucket (throttling everyone or no one) and the audit log records the LB, not the client. So **any proxied deployment must set `TF_TRUSTED_PROXY_CIDR`** for the limiter and audit IPs to work per-client. Multi mode logs a loud warning at boot when it's unset.

**Edge header hygiene (complement).** Where you control the edge, configure the outermost proxy to *strip* any inbound `X-Forwarded-For` before appending its own, so a client can't pre-seed the chain. Examples:

```nginx
# nginx: replace (don't append) — sets XFF to just the real peer
proxy_set_header X-Forwarded-For $remote_addr;
```

```
# HAProxy: option forwardfor already overwrites by default;
# be explicit if you've customized it:
http-request set-header X-Forwarded-For %[src]
```

The right-to-left walk is sound even without the strip (a pre-seeded value sits to the left of the address your first trusted proxy appends, so it's never reached) — but stripping at the edge is defense-in-depth and keeps the header honest for anything else that reads it. This mirrors the outbound `X-Forwarded-For` stripping TF already does in its git/LLM proxies.

## Durable workspace storage (SeaweedFS)

A blueprint's workspace — the git worktree plus the scratch space its steps hand off through — must survive the executor that created it (an open run can outlast the process; an executor can scale down mid-run). The TF binary snapshots that workspace to an **S3-compatible object store** and rehydrates it on resume; the host worktree is only a warm cache.

Self-host runs **SeaweedFS** for this: one self-contained S3 container (Apache-2.0, Go), no Postgres or JWT coupling. The workspace snapshots are opaque server-internal tarballs — they need a dumb bucket, not a storage API's RLS / resumable-upload / CDN layer. The `seaweedfs` service in `docker-compose.yml` runs `weed server -s3` (the all-in-one master+volume+filer+S3 process), and a one-shot `seaweedfs-postinit` sidecar creates the bucket on every `up` (`head-bucket || create-bucket` via aws-cli — idempotent).

The TF binary talks to it through the `TF_BLOB_*` env (read only in multi mode; local mode writes blobs to `~/.triagefactory/blobs` and runs none of this). The compose stack templates a single-identity `s3.json` from the same `TF_BLOB_ACCESS_KEY` / `TF_BLOB_SECRET_KEY` pair you set in `.env`, so server, bucket sidecar, and client all share one credential — there's no second input to drift. Supplying that identity is also what takes SeaweedFS **out of its open-by-default "allow-all" mode**: the bundled store always requires the creds, never serving anonymously on the compose network. The S3 port (`8333`) is deliberately **not** published to the host — TF and the sidecar reach it only over the compose network.

Smoke-test the round-trip through the aws-cli sidecar (exercises the same bucket and creds TF's S3 client uses; the stack must already be up):

```sh
docker compose run --rm --no-deps --entrypoint sh seaweedfs-postinit -c '
  EP=http://seaweedfs:8333; B=${TF_BLOB_BUCKET:-tf-workspaces}
  echo hello | aws --endpoint-url "$EP" s3 cp - "s3://$B/smoke.txt" &&
  aws --endpoint-url "$EP" s3 cp "s3://$B/smoke.txt" - &&            # -> hello
  aws --endpoint-url "$EP" s3 rm "s3://$B/smoke.txt"'
```

To eyeball stored snapshot objects there's no published browser console; list them through the same sidecar: `docker compose run --rm --no-deps --entrypoint sh seaweedfs-postinit -c 'aws --endpoint-url http://seaweedfs:8333 s3 ls "s3://${TF_BLOB_BUCKET:-tf-workspaces}/"'`.

### Hosted Supabase Storage, S3, or R2 (SaaS / BYO)

The same `aws-sdk-go-v2` client (path-style addressing, configurable `BaseEndpoint`) drives **any** S3-protocol backend — there's no compose change to point it elsewhere. Set `TF_BLOB_ACCESS_KEY` / `TF_BLOB_SECRET_KEY` to that backend's keys and override the endpoint (and bucket / region as needed) in `.env`:

```sh
TF_BLOB_ENDPOINT=https://<ref>.supabase.co/storage/v1/s3   # base path IS preserved
TF_BLOB_BUCKET=tf-workspaces
TF_BLOB_REGION=us-east-1
```

`TF_BLOB_ENDPOINT` is a full URL — its scheme selects TLS and any base path (Supabase Storage's `/storage/v1/s3`) is kept intact. The bundled `seaweedfs` service still starts in this configuration but goes unused; drop it via a compose override (and remove `seaweedfs-postinit` from `triagefactory`'s `depends_on` in that override) if you don't want it running. Pre-create the bucket on the hosted side (the `seaweedfs-postinit` sidecar only ensures the bucket on the bundled SeaweedFS).

## Rotating the signing key

The current tooling supports **single-key replacement** only. Because the default `jwk-init --write-env` reuses an existing keypair, rotation is an explicit opt-in via `--rotate`:

1. `./triagefactory jwk-init --write-env .env --rotate` — regenerates the keypair, secret, and service-role token, replacing the old lines in place (no manual deletion needed)
2. Recreate GoTrue so it picks up the new env: `docker compose up -d gotrue`

Any `jwk-init --write-env` run (rotate or not) rewrites `.env` atomically and normalizes its mode to `0600`, since the file holds private RSA material. If you'd deliberately set a looser mode (e.g. `0640` for a `docker` group), re-apply it after running.

`docker compose up -d` (without `stop`/`start`) detects the env diff against the existing container and recreates it. `docker compose start gotrue` would reuse the cached env from container creation and the new key would NOT be loaded — this is a common foot-gun. The Verifier picks up the new key automatically on the next unknown-`kid` lookup — no TF restart needed.

**Caveat:** any access tokens still in flight that were signed by the old key will fail verification as soon as GoTrue restarts. GoTrue's default access-token lifetime is 1 hour, so the practical impact is "users with active sessions need to re-authenticate." For zero-downtime overlap rotation (publish both old and new keys, switch the signing kid, wait for the old to expire, drop the old) you'd need to maintain a multi-key `GOTRUE_JWT_KEYS` array by hand — our `jwk-init` doesn't currently support merge semantics. Planned for a future ticket; for now, rotate during low-traffic windows or treat each rotation as a forced re-auth event.

## Verifying releases

Every release artifact is signed keylessly via [cosign](https://docs.sigstore.dev/) and GitHub OIDC — there's no signing key for us to manage, rotate, or leak, and no key for you to fetch and pin before verifying. Each tagged release also carries an SPDX SBOM per archive, and the GHCR image carries an SBOM + SLSA provenance attestation alongside its signature.

The `--certificate-identity-regexp` below pins the exact workflow file *and* the tag-push trigger that ran it — not just the repo. A bare repo-name match would also accept a signature from any other workflow in this repo that happened to hold `id-token: write` (e.g. one added on a PR branch), which defeats the point of checking provenance at all.

**Release tarball / checksums** — `checksums.txt` is signed as a blob; verifying its signature transitively verifies every archive it lists (each line is a sha256 of one archive):

```sh
cosign verify-blob --certificate checksums.txt.pem --signature checksums.txt.sig \
  --certificate-identity-regexp '^https://github\.com/sky-ai-eng/triage-factory/\.github/workflows/release\.yml@refs/tags/.+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
```

Download `checksums.txt`, `checksums.txt.sig`, and `checksums.txt.pem` from the release page alongside the archive, then `sha256sum -c checksums.txt` to confirm the archive you downloaded matches.

**Docker image** — verify by digest or tag:

```sh
cosign verify ghcr.io/sky-ai-eng/triage-factory:vX.Y.Z \
  --certificate-identity-regexp '^https://github\.com/sky-ai-eng/triage-factory/\.github/workflows/docker-publish\.yml@refs/tags/.+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Inspect the attached SBOM + provenance attestations with `docker buildx imagetools inspect ghcr.io/sky-ai-eng/triage-factory:vX.Y.Z`.

The regex above only matches tag-push signatures, scoped to what this "Verifying releases" section covers. `docker-publish.yml` also signs on every push to `main` (`:edge`, `:sha-<short>`) — those are legitimately signed too, but with `@refs/heads/main` in place of `@refs/tags/...`, so verifying one of those images needs `--certificate-identity-regexp '^https://github\.com/sky-ai-eng/triage-factory/\.github/workflows/docker-publish\.yml@refs/heads/main$'` instead.

Each `*.sbom.json` (SPDX) attached to the release is a plain downloadable asset — no verification tooling needed, just fetch it over HTTPS.

**Air-gapped / no Rekor-Fulcio reachability:** keyless verification needs the signer to reach GitHub's OIDC issuer at sign time (already true — it runs in Actions) and the *verifier* to reach the public Rekor transparency log and Fulcio CA at verify time. If your environment can't reach those, fall back to the plain `sha256sum -c checksums.txt` check against a `checksums.txt` you fetched over authenticated HTTPS from the GitHub release page — that gives you integrity (the file wasn't corrupted/tampered in transit) without the provenance guarantee (that it was GitHub Actions, specifically, that produced it).
