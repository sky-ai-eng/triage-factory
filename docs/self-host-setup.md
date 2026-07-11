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
- `TF_SYSTEM_PASSWORD` — password for the `tf_system` role: the least-privilege database role executor pods' admin pool connects as instead of `supabase_admin` (superuser). BYPASSRLS but no DDL, and a narrow, enumerated table grant list — see the `tf_system` section of `internal/db/migrations-postgres/202605130001_pg_baseline.sql`. Executors are the most-exposed machine class (they run agent workloads), so this is the difference between a compromised executor holding a superuser DSN and one holding a role that can only touch the run-execution tables it needs. Control/all pods are unaffected — they keep the `supabase_admin` DSN. **Generate with `openssl rand -hex 32`.** The role ships `NOLOGIN` in the baseline; a dedicated `postgres-postinit-system` sidecar grants it `LOGIN` on every `docker compose up` — split from the `postgres-postinit` sidecar above because `tf_system` doesn't exist until the `triagefactory` container has run its migration, so this sidecar waits on `triagefactory` being healthy rather than racing it. Required even on a single-box (`TF_ROLE` unset) deployment — `docker-compose.yml`'s `${VAR:?}` guard fails boot without it (Compose interpolates every service's env up front, so there's no way to make it conditional on `--profile scale`), even though only the `executor` service actually connects as `tf_system`.
- `TF_PUBLIC_URL` — your public URL (no trailing slash)
- `GH_CLIENT_ID` / `GH_CLIENT_SECRET` — from step 1
- `TF_SESSION_ENCRYPTION_KEY` — 32 random bytes; AES-GCM master key for the access/refresh tokens stored at rest in `public.sessions`. **Generate with `openssl rand -hex 32`.** Rotating this key invalidates every existing session (ciphertext can't be decrypted) — plan it as a forced re-auth event.
- `TF_COOKIE_SECRET` — 32 random bytes; HMAC-SHA256 key for the short-lived OAuth state cookie (carries PKCE verifier + CSRF token). **Generate with `openssl rand -hex 32`.** Kept distinct from `TF_SESSION_ENCRYPTION_KEY` so the two rotate independently — rotating only this one invalidates in-flight OAuth handshakes (10-minute window), not active sessions.
- `TF_SECRET_ENCRYPTION_KEY` — 32 random bytes; AES-GCM master key for org + per-user **integration secrets** (GitHub App PEM / client-secret / webhook-secret, PATs, Jira tokens) stored at rest in `public.org_secrets`. **Generate with `openssl rand -hex 32`.** TF encrypts these app-side and stores only opaque ciphertext, so they are **not** in Supabase Vault / pgsodium — a routine `docker compose down && up` (or any postgres container recreate) no longer loses them. Kept distinct from `TF_SESSION_ENCRYPTION_KEY` so the two rotate independently — a session-key rotation must not nuke integration secrets. Rotating *this* key invalidates every stored secret (the ciphertext can no longer be decrypted), so plan it as a "re-enter your integration credentials" event. The same variable also governs **local-mode headless installs** (a single-binary deploy on a box with no OS keychain) — there it encrypts `~/.triagefactory/secrets.enc` instead of `public.org_secrets`; see `docs/usage.md` § Secret storage.
- `TF_BLOB_ACCESS_KEY` / `TF_BLOB_SECRET_KEY` — credentials for the durable-workspace object store. The compose stack templates these into the bundled `seaweedfs` service's S3 identity *and* feeds them to TF's S3 client — one identity, one input, so they can't drift. **Generate each with `openssl rand -hex 32`** (access key must be ≥3 chars, secret ≥8 — a 64-char hex clears both, and keeps them JSON-safe inside SeaweedFS's `s3.json`). The endpoint (`http://seaweedfs:8333`), bucket (`tf-workspaces`), and region (`us-east-1`) all default in `docker-compose.yml`; leave them unset for self-host. See [Durable workspace storage](#durable-workspace-storage-seaweedfs) below for the BYO / hosted-Supabase path.
- `TF_ATLASSIAN_CLIENT_ID` / `TF_ATLASSIAN_CLIENT_SECRET` — *(optional)* the first-party Atlassian OAuth (3LO) app TF uses for one-click "Connect Jira" against Cloud orgs. Deployment config in the same class as the GoTrue signing material — they live here in `.env`, **not** the keychain/vault, because TF registers one first-party Atlassian app for the whole deployment. Leave unset to run without a hosted Atlassian app; each org can then supply its own from **Workspace Settings → Atlassian OAuth app** (a per-org override always wins over this default). Register the app in the [Atlassian developer console](https://developer.atlassian.com/console/myapps/) and set its callback (redirect) URL to `${TF_PUBLIC_URL}/api/orgs/{org}/jira/connect/callback`.
- `TF_TRUSTED_PROXY_CIDR` / `TF_CAPTURE_CLIENT_IP` — *(recommended when behind a proxy)* govern how TF derives the client IP it records on sessions, on the SOC2 auth audit log, and keys the pre-auth rate limiter by. **If TF sits behind a load balancer or CDN, set `TF_TRUSTED_PROXY_CIDR`** — otherwise `X-Forwarded-For` is ignored and every request is attributed to the LB. See [Client IP & trusted proxies](#client-ip--trusted-proxies) below for the full rationale and per-topology guidance.

> **Rotating passwords:** edit `.env` and re-run `docker compose up -d`. Two short-lived sidecars run on every boot and reapply `ALTER USER`/`ALTER ROLE` for the non-superuser roles — `postgres-postinit` for `supabase_auth_admin` and `authenticator`, `postgres-postinit-system` for `tf_system` — so password changes propagate without wiping the data volume. Rotating `POSTGRES_PASSWORD` itself requires more care — that's the superuser's password and Postgres only honors the env var on first init, so changing it means `ALTER USER postgres WITH PASSWORD '...'` by hand inside the running container.

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

On first boot, the `postgres-postinit` sidecar reconciles the `supabase_auth_admin` and `authenticator` role passwords, the `seaweedfs-postinit` sidecar creates the workspace bucket (idempotent — `head-bucket || create-bucket` via aws-cli), then the `triagefactory` container's entrypoint runs `triagefactory migrate up` against the admin DSN before starting the server. Once `triagefactory` is healthy, a third sidecar, `postgres-postinit-system`, reconciles the `tf_system` password — it has to run last because `tf_system` is itself created by that migration, so it doesn't exist yet at the point the other two sidecars run.

> **The TF data volume is an identity, not just a cache.** The `tf-data` volume holds `instance-id` — the persistent identity this process registers into the fleet registry and stamps onto the runs it executes; crash recovery keys on it. Keep the volume persistent across restarts (the compose file already does), and **never run two TF containers off copies of one data volume** (a cloned volume, a restored snapshot started alongside the original): the second boot supersedes the first's identity, and the superseded process fences itself — stops claiming new runs, kills its own live sandboxes, then **exits with code 3**. Your restart policy (compose/systemd) brings it straight back up, which re-registers, bumps the epoch, and fences whichever OTHER copy is still running off the duplicated volume — so two processes sharing one data volume **crash-loop each other indefinitely**. That crash-loop is the intended alarm, not a bug: it's loud, visible in `docker compose ps`/logs, and impossible to miss, unlike a silently-fenced-but-still-running process. The fix is always the same — stop one of the duplicates. If you ever need to duplicate a machine on purpose, delete `instance-id` from the copy first so it mints a fresh identity instead of colliding.

Smoke-check the stack came up:

```sh
docker compose ps                         # all services should be "running"/"healthy" (the three postinit sidecars show "exited(0)")
curl -fsS http://localhost:3000/api/health   # 200 OK
curl -s http://localhost:9999/.well-known/jwks.json | jq .   # JWKS with one public RSA key
```

### Monitoring

Point your load balancer / uptime check at `GET /readyz` (a bare path, not under `/api/`) rather than `/api/health` above. `/api/health` is a liveness probe only — no DB or integration check, by design, so a flapping dependency can't trip a platform's auto-restart-on-liveness-failure. `/readyz` is the readiness signal: it 503s on a **hard** failure (DB unreachable, migrations not applied, or a poller's base-tick loop stalled/crashed) and reports, without ever 503ing on them, **soft** signals — each org's last-successful-poll age vs its configured poll interval, each org's last-observed GitHub primary rate-limit budget, and a count of in-flight (`queued`/`running`) agent runs:

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
  "rate_limit": {
    "github": {"<org_id>": {"remaining": 4321, "reset_unix": 1783303600, "used": 679}}
  },
  "active_runs": 3
}
```

An org absent from `rate_limit.github` has no observation yet this process (never polled, or its host omits rate-limit headers — e.g. GHES with rate limiting disabled), not zero remaining budget. Webhook-delivery freshness has no equivalent section yet — webhook ingestion invariants (dedup, reconciliation linkage) haven't landed, so there is no state to report.

Alert on `age_seconds > 3 × interval_seconds` for whichever source/org you care about — that threshold is yours to pick; `/readyz` reports the raw numbers rather than guessing at your alerting policy (its own top-level `status` field applies that same 3x default and surfaces it as `"degraded"`, purely as a quick-glance dashboard signal, never as a 503). `/readyz` is unauthenticated by design, same as `/api/health` — the response carries only opaque org IDs, never repo names, usernames, or other tenant data, so there is no `?verbose=` mode and none is needed.

## Scaling out: control + N executors

The default `docker compose up` runs one all-in-one `triagefactory` container (`TF_ROLE` unset → `all`) — HTTP + pollers + brain + the delegated-run dispatcher in a single process. That goes a long way (memory-bound; budget ~0.25 GB RAM per concurrent run). When you outgrow one box, or want the sandbox fleet to fail independently of the API, split the process into a **control** pod and **N executor** pods with the `scale` compose profile.

`TF_ROLE` names the role each container plays:

| Role | Serves user HTTP/WS | Pollers + router + AI brain | Runs migrations | Claims + executes delegated runs | Sandboxes |
|------|:---:|:---:|:---:|:---:|:---:|
| `all` (default) | ✅ | ✅ | ✅ | ✅ | ✅ |
| `control` | ✅ | ✅ | ✅ | ❌ | ✅ (curator chat) |
| `executor` | ❌ | ❌ | ❌ (asserts schema) | ✅ | ✅ (delegated runs) |

Bring up 1 control + 2 executors:

```sh
# In .env, flip the main service to the control role:
echo 'TF_ROLE=control' >> .env

# Start the control pod (the published entrypoint) plus 2 executor pods:
docker compose --profile scale up -d --scale executor=2
```

That's the whole topology — **there is no load balancer.** Executors accept no inbound traffic (no published ports; their only listener is a localhost-only healthz), so the single control pod stays the single entrypoint exactly as the all-in-one did. Verify a run flows end-to-end:

```sh
docker compose --profile scale ps                 # 1 triagefactory (control) + 2 executor, all healthy
# Enqueue a manual delegation through the control pod's API, then watch an
# executor claim it (run_messages land, the run completes):
docker compose --profile scale logs -f executor
```

Both roles run the **same image and entrypoint** (the broker-then-drop privilege separation), and **both carry the sandbox caps** — control pods sandbox curator chat sessions on the request path, executors sandbox delegated runs. Each executor replica gets its **own** `TF_STATE_ROOT` (`/data`) and rootfs-cache volume: the fleet is shared-nothing, and two processes sharing one state root would collide on the instance-identity flock (the second refuses to boot). See the header of `docker-compose.yml` for the volume details.

### Executor health

Each executor exposes a localhost-only `GET /healthz` (default `127.0.0.1:3001`, `TF_HEALTHZ_PORT`) — the container HEALTHCHECK target. It reports:

```json
{
  "dispatcher_alive": true,
  "last_heartbeat_write_age_sec": 3,
  "heartbeat_ever_written": true,
  "broker_ok": true,
  "active_runs": 2,
  "draining": false,
  "fenced": false
}
```

It returns **503** when the dispatcher loop has stopped, the last fleet-registry heartbeat write is older than 3× the heartbeat interval, **or** the cap-broker stops answering (`broker_ok:false` — an executor that can't launch sandboxes is useless). `draining` and `fenced` are informational and never flip the code: a fenced executor stays `200`-but-`fenced:true` so the HEALTHCHECK doesn't kill it before it can quiesce. The instance registry (`instances` table) carries each pod's role and build version, so version skew across a rolling deploy is visible there.

### Executor database role (`tf_system`)

An executor's admin database pool connects as `tf_system`, not `supabase_admin` — a least-privilege role (`BYPASSRLS`, no DDL, an enumerated grant list covering only the run-execution tables the dispatcher touches). Executors are the most-exposed machine class in the fleet (they run agent workloads and parse agent output), so a compromised executor holding `tf_system` never has superuser reach — no other org's rows outside the granted tables, no DDL, no server-wide surface. Control/`all` pods are unaffected; they keep the `supabase_admin` DSN because they run migrations.

This is wired automatically by `docker compose --profile scale` (the `executor` service's `TF_DATABASE_URL` is templated from `TF_SYSTEM_PASSWORD`) — nothing to configure beyond filling in `.env`. If you're running an executor outside the shipped compose file (bare-metal, a custom orchestrator) and it ends up pointed at a superuser DSN — e.g. a dev box reusing the control pod's connection string — TF logs one `ERROR` at boot naming the misconfiguration and continues (it does not refuse to boot): compose, the supported deployment path, ships this correctly, so a hard failure there would be disproportionate; the warning is for exactly this kind of bare-metal drift. Fix it by pointing the executor's `TF_DATABASE_URL` at `tf_system` instead.

### Per-role DB pools and `max_connections`

Every pod opens two Postgres pools (an admin pool and an app/RLS pool). The API tier (`control`/`all`) keeps 25 connections per pool; an **executor ships much smaller ceilings** — its dispatcher, heartbeat, and run sinks need a fraction of the API tier's request concurrency, and its app pool is effectively idle (no RLS request handlers run on an executor). At the defaults:

| Role | Per-pool ceiling | Pools | Per-pod worst case |
|------|:---:|:---:|:---:|
| `control` / `all` | 25 | 2 | 50 |
| `executor` | 6 | 2 | 12 |

So the 1-control + 2-executor profile demands at most **25×2 + 2×(6×2) = 74** connections — comfortably inside a stock `supabase/postgres` `max_connections=100` with headroom. Sizing your own fleet: `Σ pods (per-pool ceiling × 2)` must fit `max_connections` minus what GoTrue and any admin tooling reserve. Three executors + two control pods at the defaults is `2×50 + 3×12 = 136` — past a default 100, so either raise `max_connections` (and Postgres' memory) or lower `TF_DB_MAX_OPEN_CONNS`. Override the per-pool ceiling with `TF_DB_MAX_OPEN_CONNS` (floored at 2). A dedicated transaction-mode pooler in front of Postgres stays a large-fleet option (TFAC-307), not a requirement for this profile.

### Multiple control pods (HA) — reference reverse-proxy config

> **One control pod needs no proxy.** The single-control shape above already works with zero extra infrastructure. Running **M ≥ 2 control pods** for HA works today: exactly one control pod holds the `background-brain` lease (a Postgres-backed election, `TF_LEASE_*` env knobs — see `.env.example`) and runs the pollers/router/AI managers/sweepers at a time; the rest stay standbys, ready to take over within roughly a TTL + one renewal interval (~30s at the defaults) of the holder going away. `GET /readyz` reflects this per pod — a `lease` field reports `{name, holder_id, is_holder, term}`, and a standby hard-checks only DB + migrations (never poller-alive) so the proxy below keeps every standby in rotation. One caveat until curator homing (TFAC-588) lands: curator chat sessions are pod-local (the serving control pod sandboxes them on the request path), so a mid-conversation failover or an unlucky proxy re-route can land a follow-up message on a different pod than the one holding the session — retry from the client recovers it, but it's not yet seamless. The config below is the reverse-proxy half you put in front of M control pods.

We ship configuration as a worked example, never proxy software. Any stock reverse proxy works; the load-bearing requirements are: **WebSocket upgrade passthrough** (TF streams run activity over `/api/ws`), **generous idle timeouts** (those connections are long-lived), **readiness-aware rotation** (poll `GET /readyz` and pull a pod that 503s out of rotation), and **`X-Forwarded-For` set to the real client** paired with `TF_TRUSTED_PROXY_CIDR` (see the *Client IP & trusted proxies* section below — TF only trusts XFF from a proxy in that allowlist).

Caddy:

```caddy
triagefactory.yourcompany.com {
	reverse_proxy control-1:3000 control-2:3000 {
		lb_policy round_robin
		# Readiness-aware rotation: a control pod that 503s /readyz is
		# pulled until it recovers.
		health_uri  /readyz
		health_interval 10s
		health_status 200
		# Long-lived WS + streaming responses.
		transport http {
			read_timeout  1h
			write_timeout 1h
		}
	}
	# Caddy forwards X-Forwarded-For by default; keep the edge stripping any
	# client-supplied XFF so only the proxy's value reaches TF.
}
```

nginx:

```nginx
map $http_upgrade $connection_upgrade { default upgrade; '' close; }

upstream tf_control {
	server control-1:3000 max_fails=3 fail_timeout=10s;
	server control-2:3000 max_fails=3 fail_timeout=10s;
}

server {
	listen 443 ssl;
	server_name triagefactory.yourcompany.com;

	location / {
		proxy_pass http://tf_control;

		# WebSocket upgrade passthrough (/api/ws).
		proxy_http_version 1.1;
		proxy_set_header Upgrade    $http_upgrade;
		proxy_set_header Connection $connection_upgrade;

		# Long-lived streams: don't cut the connection mid-run.
		proxy_read_timeout  1h;
		proxy_send_timeout  1h;
		proxy_buffering     off;

		# Set (don't append) XFF to just the real peer, then set
		# TF_TRUSTED_PROXY_CIDR to this proxy's egress CIDR so TF honors it.
		proxy_set_header Host              $host;
		proxy_set_header X-Forwarded-For   $remote_addr;
		proxy_set_header X-Forwarded-Proto $scheme;
	}
}
```

Then set `TF_TRUSTED_PROXY_CIDR` on the control pods to the proxy's egress CIDR(s) so `X-Forwarded-For` is honored (see the next section).

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

## Privilege separation: process model

The `triagefactory` container grants `CAP_SYS_ADMIN` + `CAP_NET_ADMIN` (see `docker-compose.yml`'s `cap_add` comment) because the per-run gVisor sandbox needs them to set up netns/veth/iptables, cgroups, and the curated rootfs. That grant is a *ceiling* on the container, not a promise that the whole TF process wields it: as of TFAC-605, `docker/entrypoint.sh` splits the container into two processes at boot, and only one of them ever touches those capabilities.

This split applies **only when `TF_MODE=multi`** — the same condition that decides whether this host sandboxes runs at all. Local mode (`TF_MODE=local`, the image's own default — the point of `docker-compose.yml`'s "a `docker run` without any env vars boots into a working single-tenant binary") has no sandbox and no broker to protect anything from, so the entrypoint skips the split outright there: no uid switch, no capability drop, no `$HOME` change. A bare `docker run` of the published image is completely unaffected by this section.

| Process | Holds capabilities | Holds credentials | Parses hostile input | Listens for connections |
| --- | --- | --- | --- | --- |
| **cap-broker** (`tf-cap-broker`) | **Yes** — `CAP_SYS_ADMIN`, `CAP_NET_ADMIN` | No | No | Yes — but only a host-only unix socket (`/run/tf/cap-broker.sock`, mode 0600), reachable only by the orchestrator |
| **orchestrator** (`tf-orchestrator`) | **No** — empty effective set, capability bounding set cleared | Yes — GitHub/Jira tokens, the GitHub App key, DB credentials | Yes — webhook payloads, agent output, the HTTP API | Yes — the public HTTP API / websocket |
| **sandbox** (the delegated agent) | No | No | Yes — it's the source of the hostile input | No — outbound only, through the orchestrator's egress proxy |

The broker serves two families of operations over that socket, both behind boundary validation (`internal/sandbox`'s `ValidateLaunchParams` and `validateRunTreeRoot`) so a compromised orchestrator cannot steer them at arbitrary host state: the sandbox infrastructure itself (netns/veth/iptables, cgroups, the curated rootfs, the `runsc` launch), and the **run-tree ownership lifecycle** — handing a freshly-cloned run tree to the sandbox identity (uid 10000) at run start, running the park-time git-delta capture in a dropped-privilege child, and destroying the tree at teardown. The lifecycle family exists because all three are privileged once the orchestrator's capabilities are gone: changing a file's owner needs `CAP_CHOWN`, the capture child's setuid/netns need `CAP_SETUID`/`CAP_SYS_ADMIN`, and removing a tree the sandbox wrote into means unlinking through modes the orchestrator can't. The chown/remove ops additionally require the target tree to already be owned by the orchestrator or the sandbox identity — a validly-shaped path pointing at `/etc` is refused by ownership, not just by shape. One deliberate exception stays orchestrator-side with no capability at all: the per-run agenthost socket is *chgrp'd* (not chowned) to the sandbox group — an owner-legal group grant, possible because the image makes `tf-orchestrator` a member of `tf-sandbox` (gid 10000) and the entrypoint's `setpriv` carries exactly that one supplementary group through the drop.

**Broker lifetime.** After the entrypoint's `exec`, the broker is a child of the orchestrator, but the orchestrator neither supervises nor restarts it (post-drop it lacks the privilege to manage a root process anyway) — if the broker ever dies, subsequent sandbox operations fail with a clear dial error and the fix is a container restart; a crashed broker may also linger as one `<defunct>` zombie entry in `ps` until then, which is cosmetic. Shutdown needs no supervision either way: tini's `-g` signal fan-out reaches both processes directly.

How to read this against a running deployment:

- `docker compose exec triagefactory ps aux` (or `ps -ef` inside the container's PID namespace) shows two `triagefactory`-derived processes named `tf-cap-broker` and `tf-orchestrator` — not two processes both named `triagefactory`.
- `cat /proc/<cap-broker pid>/status | grep ^Cap` and the same for the orchestrator's pid: the broker's `CapEff` decodes to `cap_sys_admin,cap_net_admin` (via `capsh --decode=<hex>` or by eye against `include/uapi/linux/capability.h`'s bit numbers); the orchestrator's `CapEff` is all zeroes, and — the stronger property — its `CapBnd` (bounding set) is all zeroes too, meaning it cannot regain a capability even by executing a setuid-root helper later.
- `cat /proc/<cap-broker pid>/status | grep ^Uid` vs. the orchestrator's: different real uids (broker is `0`, orchestrator is `10001` by default — see `TF_ORCHESTRATOR_UID` in `.env.example`). Different uids is what makes the orchestrator's zero capabilities meaningful: even a `CAP_SYS_PTRACE`-holding attacker (which the orchestrator isn't, but hypothetically) can't `ptrace` a different-uid process without it.
- Every log line the container emits carries a `proc=cap-broker` or `proc=orchestrator` attribute, and each process logs exactly one boot line reporting its own uid and effective capability set — see the example in [usage.md](usage.md#privilege-separation-multi-mode).

**`$HOME`.** `setpriv` switches uid/gid/capabilities but never touches environment variables, so the entrypoint explicitly sets `HOME` to the orchestrator's own directory (`/home/tf-orchestrator` by default, owned by uid `10001`) right before the exec — the container's inherited `HOME=/root` would otherwise persist and become unreadable/unwritable once the process is no longer root. This matters beyond the obvious: a few paths (curator session state, skills import, project-bundle export/import) deliberately resolve Claude Code SDK session state from the real `$HOME` even in multi mode (see `internal/paths.go`), so getting this wrong breaks those, not just an edge case.

**No rollback flag.** The cap-broker is the only sandbox launch path in multi mode — it is spawned unconditionally, with no switch to disable it and no in-process fallback. If the broker can't start, boot fails with a clear error rather than silently running the sandbox from a less-isolated, fully-privileged process. (Local mode never sandboxes, so it never spawns a broker and is unaffected.)

**Upgrading an existing deployment.** The orchestrator now runs as uid `10001` instead of root. Files created under the `tf-data` (`/data`) and `tf-rootfs` (`/opt/triagefactory/sandbox`) volumes by a pre-TFAC-605 (root) orchestrator stay root-owned — the entrypoint only `chown`s the top-level mount points, not existing content recursively, to keep every boot fast. If the orchestrator logs permission errors reading or writing under `/data` after upgrading, run a one-time recursive fix from the host:

```sh
docker compose run --rm --user root triagefactory chown -R 10001:10001 /data /opt/triagefactory/sandbox
```

A fresh deployment (empty volumes) needs no such step.

One more upgrade wrinkle, only if you persisted the container's `/root` across upgrades (no stock volume does): Claude Code SDK session state a pre-TFAC-605 deployment wrote under `/root/.claude` (curator session resume, primarily) is orphaned once `$HOME` moves to `/home/tf-orchestrator` — the orchestrator won't find it, and parked curator sessions from before the upgrade rehydrate from their snapshots instead of resuming warm. If keeping warm resume matters, copy `/root/.claude` into `/home/tf-orchestrator/.claude` (and `chown -R 10001:10001` it) before the first post-upgrade boot; otherwise nothing needs doing — the state regenerates.

## Tailored seccomp profile

The `triagefactory` service in `docker-compose.yml` runs with `security_opt: seccomp=docker/seccomp-profile.json` — a default-deny allowlist, not `seccomp=unconfined`. This is TFAC-608 (part of the privilege-separation epic, `docs/specs/privsep/README.md` §9 hardening track; see `docs/sandbox-security-architecture.md` §3/§6 vector 2 for the threat-model framing).

**Scope — self-host `docker-compose` only.** Fly.io-hosted production (`fly.toml`) runs with unconfined seccomp inside its Fly Machine and is not covered by this profile — the Fly Machine config format has no `security_opt`-equivalent field to attach one to. Each Fly Machine is already its own Firecracker microVM, a per-tenant hardware isolation boundary rather than a kernel shared with other tenants, which is a different property from what this profile hardens on self-host's shared Docker engine.

**Why a profile is needed at all.** Docker's own default seccomp profile blocks several syscalls that `runsc` (gVisor, `--platform=systrap`) needs to construct its own, far stricter, per-run sandbox — that's why the container previously ran fully unconfined. `unconfined` reads as "no syscall filtering at all" to a security reviewer, which overstates what's actually required: the gap turned out to be a single syscall.

**Scope — this is the host-level profile, not the in-sandbox one.** `docker/seccomp-profile.json` constrains the `triagefactory` **container** as seen by the host kernel — i.e., the TF binary itself, `ip`/`iptables`/`sysctl`, the `chroot`+`apk` rootfs bake, and `runsc` (and everything `runsc` forks). It is unrelated to the OCI seccomp profile `internal/sandbox/spec.go` attaches to the *sandboxed agent's* OCI spec (`internal/sandbox/syscalls.go`'s `defaultAllowedSyscalls`) — that one is inert under gVisor (`runsc` doesn't enforce the app-facing OCI seccomp list at all; see TFAC-299 and the validation in `docs/specs/playwright-chromium-sandbox/README.md` §5.4/§8). Don't conflate the two: this profile is real host-kernel enforcement, the other is a no-op today.

**What it allows.** `docker/seccomp-profile.json` starts from [Docker/Moby's own default seccomp profile](https://github.com/moby/moby/blob/master/profiles/seccomp/default.json) — a widely audited, default-deny (`SCMP_ACT_ERRNO`) allowlist, not a hand-rolled one — and applies two changes:

1. **Removes** syscalls the executor has no legitimate use for and that are meaningfully dangerous if a compromised process ever reached them: `reboot`, `init_module`, `delete_module`, `iopl`, `ioperm`, `acct`, `quotactl`, `quotactl_fd`, `bpf`, `perf_event_open`, the fanotify family (`fanotify_init`, `fanotify_mark`), `landlock_create_ruleset`, `landlock_add_rule`, `landlock_restrict_self`, `lookup_dcookie`, `syslog`, `vhangup`. None of these are exercised anywhere in the netns/veth/iptables/cgroup/chroot+apk/runsc lifecycle. Removing them is defense-in-depth on top of the capability gate: the container's `CAP_SYS_ADMIN`/`CAP_NET_ADMIN` grant would otherwise let the kernel's own capability check wave some of these through if seccomp didn't block them first.
2. **Adds** `pivot_root` — the one syscall empirical validation found Docker's default profile missing for `runsc` to construct the per-run sandbox's mount namespace. Folded into the profile's existing `mount`/`umount2`/`unshare`/`setns` rule group (`includes.caps: [CAP_SYS_ADMIN]`) rather than allowed unconditionally — `pivot_root(2)` itself requires `CAP_SYS_ADMIN`, same as its neighbors in that group, so gating it the same way keeps the profile internally consistent and gives defense-in-depth if this container's capability grant is ever narrowed later (the PS-P0..P4 privsep split). Everything else `runsc`'s systrap platform needs (`mount`, `umount2`, `chroot`, `ptrace`, `unshare`, `setns`, `clone`/`clone3`, `capset`/`capget`, `process_vm_readv`/`process_vm_writev`, `seccomp`, `prctl`, `personality`, `memfd_create`, …) was already present in Docker's default allowlist — enforced there via the same capability checks, same as production.

The result is ~400 allowed syscalls (vs. Docker's default ~415, vs. effectively all ~450 under `unconfined`) — a real reduction, though the primary privilege boundary remains the two capabilities the container holds, not the syscall count. `userfaultfd` (a syscall we speculated `runsc` might also need) was tested and found **not** required — it stays out of the allowlist.

**Compatibility note — older Docker/libseccomp.** Because the base profile is copied from a recent Moby release, it names a handful of newer syscalls (`cachestat`, `mount_setattr`, `move_mount`, `fsopen`/`fsconfig`/`fsmount`/`fspick`/`open_tree` — the new mount API, plus a few others). A sufficiently old Docker Engine / libseccomp doesn't recognize those names when parsing the profile, which fails the **whole container's startup** (not just that one rule) with an error naming the unrecognized syscall — this is a real, historically-seen Docker/libseccomp compatibility class, distinct from anything in this profile's own design. If `docker compose up` fails this way: upgrade Docker Engine (and the bundled libseccomp) to a current release, or — if you can't upgrade — delete the unrecognized syscall name(s) from the offending rule's `names` array in `docker/seccomp-profile.json` and re-run the validation procedure below; a kernel old enough to ship a Docker/libseccomp that predates these names almost certainly predates the syscalls themselves too, so removing the name costs you nothing on that host.

### Validation

Validated across the full run lifecycle on real `runsc` (release-20260511, matching the pin in `docker/Dockerfile`): sandbox spawn, agent execution (including real network egress from inside the gVisor sandbox through the netns/veth/NAT/egress-allowlist path — `internal/sandbox/netns_linux.go`, `iptables_linux.go`), teardown, and orphan reap (`internal/sandbox/reaper_linux.go`), plus the `chroot`+`apk` rootfs toolchain bake (`internal/sandbox/rootfs_linux.go`) and the agent-host IPC socket path (`cmd/exec/agenthost/`). No browser sandbox profile exists in the codebase yet (`docs/specs/playwright-chromium-sandbox/` is still proposal-stage — no `Config.Profile` field), so there was nothing browser-specific to validate.

Harness: `go test -tags integration ./internal/sandbox/...` (the same suite `scripts/test-sandbox-linux.sh` runs) built as a static binary and run inside a container mirroring the compose service's exact privilege shape (`--cap-add SYS_ADMIN --cap-add NET_ADMIN --security-opt apparmor=unconfined --cgroupns=private`, no `--privileged`), with `--security-opt seccomp=docker/seccomp-profile.json` in place of `unconfined`. Run repeatedly (5+ iterations per candidate) to rule out flakiness before treating a pass as signal.

### Regenerating / extending the profile

A future `runsc` release, a new packaged tool, or a new privileged-operation code path in `internal/sandbox` may need an additional syscall. Two approaches, in order of preference:

**1. Direct validation (what TFAC-608 used, and the more reliable method).** Apply a candidate profile via Docker's real enforcement and run the actual lifecycle against it — don't guess, and don't trust `strace` here (see the gotcha below):

```sh
# From the repo root, with runsc on PATH and a container runtime available:
go test -tags integration -c -o /tmp/sandboxtest ./internal/sandbox/
docker run --rm \
  --cap-add SYS_ADMIN --cap-add NET_ADMIN \
  --security-opt apparmor=unconfined --security-opt seccomp=docker/seccomp-profile.json \
  --cgroupns=private \
  -v /tmp/sandboxtest:/work/sandboxtest:ro \
  -v "$(command -v runsc)":/usr/local/bin/runsc:ro \
  -w /tmp nicolaka/netshoot:latest \
  sh -c 'cp /work/sandboxtest /tmp/t && chmod +x /tmp/t && /tmp/t -test.v -test.timeout=180s'
```

(`nicolaka/netshoot` is a convenient off-the-shelf image that already bundles `ip`/`iptables`/`chroot`/`sysctl` — swap in anything else with the same tools, e.g. the `triagefactory` runtime image itself.)

If a test fails, the error is usually specific enough to point at the missing syscall (a `mount`/`chroot`/`ptrace`-shaped "operation not permitted" from `ip`, `iptables`, `chroot`, or `runsc`'s own stderr). Add the syscall to `docker/seccomp-profile.json`'s `syscalls` array with a `comment` explaining why, then re-run to confirm — including several repeats, since a nested/virtualized test host can have its own unrelated startup flakiness (a `runsc` sentry that occasionally fails with "cannot read client sync file: waiting for sandbox to start: EOF" on a cold-started nested test container, unrelated to seccomp) that's easy to misattribute to the profile if you only run once.

**Gotcha: don't wrap this in `strace -f`.** `runsc`'s systrap platform uses `ptrace` internally to attach to its own stub processes. `strace -f` auto-attaches to every forked child too, and Linux allows only one tracer per process — so `runsc`'s own attach loses the race and the sandbox fails to start (the same "waiting for sandbox to start: EOF" symptom, this time *caused* by the tracer, not by seccomp). This reproduces regardless of the seccomp profile in effect, including under `unconfined`, which is the tell that it's a tracer conflict, not a permissions problem. Diagnose failures via the error output and (if needed) `runsc --debug --debug-log=<path>`, not `strace -f` around the whole tree.

**2. Audit-mode capture**, for a broader/from-scratch resurvey (e.g., a `runsc` platform change): apply a profile with `defaultAction: SCMP_ACT_LOG` (allow + log every syscall) instead of `SCMP_ACT_ERRNO`, run the lifecycle, and collect the `type=SECCOMP` entries from the kernel audit log (`ausearch -m SECCOMP`, or `dmesg` when no `auditd` is listening). This is non-intrusive (no ptrace, no conflict with `runsc`), but at real workload syscall volume the kernel's `printk` rate limiter can silently drop the majority of entries — treat it as a rough survey to generate hypotheses, then confirm each candidate syscall with approach 1 above.

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
