# Scaling out: control + N executors

Multi mode is **always** a control + executor split — there is no fused
single-process option (`TF_ROLE=all`, or leaving it unset, refuses to boot in
multi mode; `all` is local mode's shape). The default `docker compose up`
brings up the smallest split: one `triagefactory` **control** pod (API/WS, the
background brain, migrations, the secret store) co-located with one
**executor** (the run dispatcher, gVisor sandboxes, per-run credential
sidecars) on the same box. That goes a long way (executors are memory-bound;
budget ~0.25 GB RAM per concurrent run). When you outgrow one box, or want the
sandbox fleet to fail independently of the API, add executors.

`TF_ROLE` names the role each container plays (the bundled compose file sets
it per service; only a custom deployment sets it by hand):

| Role | Serves user HTTP/WS | Pollers + router + AI brain | Runs migrations | Claims + executes delegated runs | Sandboxes |
|------|:---:|:---:|:---:|:---:|:---:|
| `control` | ✅ | ✅ | ✅ | ❌ | ❌ |
| `executor` | ❌ | ❌ | ❌ (asserts schema) | ✅ | ✅ (delegated runs) |

Scale to 1 control + 2 executors:

```sh
docker compose up -d --scale executor=2
```

> **Why there is no single-process multi shape.** The split is what carries
> the credential-isolation guarantee: the control pod holds the secret store
> but launches no sandbox (and carries no sandbox caps — it schedules on any
> substrate that forbids privileged containers, managed Kubernetes included),
> while an executor sandboxes hostile agent workloads but never holds the
> secret-decryption key — every run's credentials arrive as a sealed per-run
> bundle that only that run's sidecar can open. A fused process would hold
> both powers at once, so multi mode makes it unbootable rather than
> discouraged: the isolation boundary hangs on `TF_MODE`, not on a deployment
> knob an operator could forget.

That's the whole topology — **there is no load balancer.** Executors accept no
inbound traffic (no published ports; their only listener is a localhost-only
healthz), so the single control pod stays the single entrypoint. Verify a run
flows end-to-end:

```sh
docker compose ps                 # 1 triagefactory (control) + N executor, all healthy
# Enqueue a manual delegation through the control pod's API, then watch an
# executor claim it (messages land, the conversation completes):
docker compose logs -f executor
```

Both roles run the **same image and entrypoint**, but **only executors carry the
sandbox caps** (and the broker-then-drop privilege separation that contains them)
— every sandboxed workload, every delegated run, executes
on executors. A control pod is an ordinary unprivileged web service: its own
background LLM work (task scoring, repo profiling) is
toolless direct API calls that never spawn a sandbox. Each executor
replica gets its **own** `TF_STATE_ROOT` (`/data`) and rootfs-cache volume: the
fleet is shared-nothing, and two processes sharing one state root would collide on
the instance-identity flock (the second refuses to boot). See the header of
`docker-compose.yml` for the volume details.

Each executor's container HEALTHCHECK hits its localhost `GET /healthz` — see
[Monitoring & health checks](monitoring.md#executor-health--get-healthz).

## Executor database role (`tf_system`)

An executor's admin database pool connects as `tf_system`, not `supabase_admin` — a least-privilege role (`BYPASSRLS`, no DDL, an enumerated grant list covering only the run-execution tables the dispatcher touches). Executors are the most-exposed machine class in the fleet (they run agent workloads and parse agent output), so a compromised executor holding `tf_system` never has superuser reach — no other org's rows outside the granted tables, no DDL, no server-wide surface. Control pods are unaffected; they keep the `supabase_admin` DSN because they run migrations.

This is wired automatically by the bundled compose file (the `executor` service's `TF_DATABASE_URL` is templated from `TF_SYSTEM_PASSWORD`) — nothing to configure beyond filling in `.env`. If you're running an executor outside the shipped compose file (bare-metal, a custom orchestrator) and it ends up pointed at a superuser DSN — e.g. a dev box reusing the control pod's connection string — TF logs one `ERROR` at boot naming the misconfiguration and continues (it does not refuse to boot): compose, the supported deployment path, ships this correctly, so a hard failure there would be disproportionate; the warning is for exactly this kind of bare-metal drift. Fix it by pointing the executor's `TF_DATABASE_URL` at `tf_system` instead.

## Per-role DB pools and `max_connections`

Every pod opens two Postgres pools (an admin pool and an app/RLS pool). The API
tier (`control`) keeps 25 connections per pool; an **executor ships much
smaller ceilings** — its dispatcher, heartbeat, and run sinks need a fraction of
the API tier's request concurrency, and its app pool is effectively idle (no RLS
request handlers run on an executor). At the defaults:

| Role | Per-pool ceiling | Pools | Per-pod worst case |
|------|:---:|:---:|:---:|
| `control` | 25 | 2 | 50 |
| `executor` | 6 | 2 | 12 |

So the 1-control + 2-executor profile demands at most **25×2 + 2×(6×2) = 74**
connections — comfortably inside a stock `supabase/postgres` `max_connections=100`
with headroom. Sizing your own fleet: `Σ pods (per-pool ceiling × 2)` must fit
`max_connections` minus what GoTrue and any admin tooling reserve. Three executors
+ two control pods at the defaults is `2×50 + 3×12 = 136` — past a default 100, so
either raise `max_connections` (and Postgres' memory) or lower
`TF_DB_MAX_OPEN_CONNS`. Override the per-pool ceiling with `TF_DB_MAX_OPEN_CONNS`
(floored at 2). A dedicated transaction-mode pooler in front of Postgres stays a
large-fleet option, not a requirement for this profile.

## Multiple control pods (HA)

> **One control pod needs no proxy.** The single-control shape above already works
> with zero extra infrastructure. Running **M ≥ 2 control pods** for HA works
> today: exactly one control pod holds the `background-brain` lease (a
> Postgres-backed election, `TF_LEASE_*` env knobs — see `.env.example`) and runs
> the pollers/router/AI managers/sweepers at a time; the rest stay standbys, ready
> to take over within roughly a TTL + one renewal interval (~30s at the defaults)
> of the holder going away. `GET /readyz` reflects this per pod — a `lease` field
> reports `{name, holder_id, is_holder, term}`, and a standby hard-checks only DB +
> migrations (never poller-alive) so the proxy below keeps every standby in
> rotation. A delegated run is executor-claimed, not pod-local, so a
> mid-run failover or a proxy re-route to a different control pod is
> seamless. The config below is the reverse-proxy half you put in front of M
> control pods.

We ship configuration as a worked example, never proxy software. Any stock reverse
proxy works; the load-bearing requirements are: **WebSocket upgrade passthrough**
(TF streams run activity over `/api/ws`), **generous idle timeouts** (those
connections are long-lived), **readiness-aware rotation** (poll `GET /readyz` and
pull a pod that 503s out of rotation), and **`X-Forwarded-For` set to the real
client** paired with `TF_TRUSTED_PROXY_CIDR` (see
[Client IP & trusted proxies](networking.md) — TF only trusts XFF from a proxy in
that allowlist).

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

Then set `TF_TRUSTED_PROXY_CIDR` on the control pods to the proxy's egress CIDR(s)
so `X-Forwarded-For` is honored — see [Client IP & trusted proxies](networking.md).
