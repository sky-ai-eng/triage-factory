# Scaling out: control + N executors

The default `docker compose up` runs one all-in-one `triagefactory` container
(`TF_ROLE` unset → `all`) — HTTP + pollers + brain + the delegated-run dispatcher
in a single process. That goes a long way (memory-bound; budget ~0.25 GB RAM per
concurrent run). When you outgrow one box, or want the sandbox fleet to fail
independently of the API, split the process into a **control** pod and **N
executor** pods with the `scale` compose profile.

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

That's the whole topology — **there is no load balancer.** Executors accept no
inbound traffic (no published ports; their only listener is a localhost-only
healthz), so the single control pod stays the single entrypoint exactly as the
all-in-one did. Verify a run flows end-to-end:

```sh
docker compose --profile scale ps                 # 1 triagefactory (control) + 2 executor, all healthy
# Enqueue a manual delegation through the control pod's API, then watch an
# executor claim it (run_messages land, the run completes):
docker compose --profile scale logs -f executor
```

Both roles run the **same image and entrypoint** (the broker-then-drop privilege
separation), and **both carry the sandbox caps** — control pods sandbox curator
chat sessions on the request path, executors sandbox delegated runs. Each executor
replica gets its **own** `TF_STATE_ROOT` (`/data`) and rootfs-cache volume: the
fleet is shared-nothing, and two processes sharing one state root would collide on
the instance-identity flock (the second refuses to boot). See the header of
`docker-compose.yml` for the volume details.

Each executor's container HEALTHCHECK hits its localhost `GET /healthz` — see
[Monitoring & health checks](monitoring.md#executor-health--get-healthz).

## Per-role DB pools and `max_connections`

Every pod opens two Postgres pools (an admin pool and an app/RLS pool). The API
tier (`control`/`all`) keeps 25 connections per pool; an **executor ships much
smaller ceilings** — its dispatcher, heartbeat, and run sinks need a fraction of
the API tier's request concurrency, and its app pool is effectively idle (no RLS
request handlers run on an executor). At the defaults:

| Role | Per-pool ceiling | Pools | Per-pod worst case |
|------|:---:|:---:|:---:|
| `control` / `all` | 25 | 2 | 50 |
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
> rotation. One caveat until curator homing lands: curator chat sessions
> are pod-local (the serving control pod sandboxes them on the request path), so a
> mid-conversation failover or an unlucky proxy re-route can land a follow-up
> message on a different pod than the one holding the session — retry from the
> client recovers it, but it's not yet seamless. The config below is the
> reverse-proxy half you put in front of M control pods.

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
