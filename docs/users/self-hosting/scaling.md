# Scaling out: control + N executors

A self-hosted deployment is always split into two roles. `docker compose up`
starts one of each on the same machine:

| Role | Serves the UI and API | Polls, routes, scores | Runs migrations | Runs agents (gVisor sandboxes) |
|------|:---:|:---:|:---:|:---:|
| `control` | ✅ | ✅ | ✅ | ❌ |
| `executor` | ❌ | ❌ | ❌ | ✅ |

The bundled compose file sets `TF_ROLE` per service; set it yourself only in a
custom deployment. There is no single-process option in multi mode (`TF_ROLE`
unset or `all` fails startup): keeping agents off the process that holds the
secret store is what keeps one run's credentials away from everything else.

Executors are memory-bound; budget about 0.25 GB of RAM per concurrent run. To
add executors:

```sh
docker compose up -d --scale executor=2
docker compose ps                 # 1 control + 2 executors, all healthy
docker compose logs -f executor   # delegate a run and watch an executor pick it up
```

Executors take no inbound traffic (their only listener is a localhost health
check), so adding them needs no load balancer.

## Executor disk

Each executor needs its own state-root volume (`TF_STATE_ROOT`, `/data` in the
compose file) and rootfs cache. Never share one between executors: the second
refuses to start. The compose file header describes the volumes.

An executor's state root must be on disk, not tmpfs or ramfs; an executor
pointed at one refuses to start. Agents' working trees, their `/tmp`, and
workspace snapshots all land on that volume, so a run can fill it. A single
file can't exceed 16 GiB, but there is no total limit per run. In production,
give each executor's `/data` a disk of its own: on a single machine it is
usually the disk Postgres uses, and Postgres stops when its disk is full.

Below `TF_DISPATCH_DISK_FLOOR_MB` of free space (default `10240`), an executor
stops taking new runs, so queued work goes to other executors, and deletes
parked workspaces whose snapshots are stored ([Storage](storage.md)). Neither
stops a run that is already filling the disk.

Each executor's container health check calls its localhost `GET /healthz`; see
[Monitoring](monitoring.md).

## Rolling restarts

On SIGTERM an executor stops taking new runs, its `GET /healthz` returns 503
with `shutting_down: true`, and every run on it is interrupted and handed to
another executor. Each handed-off run uploads a snapshot of its workspace
(uncommitted files included); the next executor restores it and continues the
transcript. A tool call that was cut off is reported to the agent as
interrupted. Hand-offs don't count against `TF_MAX_CLAIM_ATTEMPTS`.

Set alongside it:

- **A termination grace period of at least 30s** (the compose default). An
  executor shuts down within 25s. If its 15s hand-off window expires, it logs a
  WARN and exits anyway; a run it hadn't released is picked up by another
  executor after 75s and counts as a lost attempt.
- **Drain first to avoid interrupting runs.** Find the executor's id, drain
  it, wait until `instance list` shows its `RUNS` at 0, then stop it. Draining
  takes effect within a few seconds; live runs finish or park at the end of
  their turn and no new ones start.

  ```sh
  docker compose exec control triagefactory instance list
  docker compose exec control triagefactory instance drain <id>
  ```

An executor that dies without SIGTERM (SIGKILL, an out-of-memory kill, a lost
node) hands nothing off. Its runs move to other executors after 75s, each
counted as a lost attempt, and the workspace comes back from the last
checkpoint.

## Workspace checkpoints

A running agent's workspace is snapshotted to the object store every
`TF_SNAPSHOT_INTERVAL_SEC` (default `300`), between tool calls, so a hard kill
loses at most that interval plus the tool call in progress (tool calls can run
up to 30 minutes). Each task keeps one snapshot, overwritten each time, so the
cost is upload bandwidth and executor CPU, not storage. `0` turns checkpoints
off; a hard kill then falls back to the last park or blueprint step.

`tf_claims_oldest_checkpoint_age_seconds` and
`tf_workspace_checkpoints_total{outcome}` track them ([Monitoring](monitoring.md)),
and the fleet console shows each run's checkpoint age.

An interrupted upload leaves an **incomplete multipart upload** in the bucket,
which is billed and never cleaned up on its own. Give the bucket a lifecycle
rule that aborts incomplete multipart uploads after a day (on S3,
`AbortIncompleteMultipartUpload`; SeaweedFS and MinIO have equivalents).

## Executor database role (`tf_system`)

Executors connect to Postgres as `tf_system`, a role with no DDL and access to
only the tables running agents need, so a compromised executor never holds
superuser access. Control pods keep the `supabase_admin` connection because
they run migrations.

The bundled compose file sets this up from `TF_SYSTEM_PASSWORD`. In a custom
deployment, point each executor's `TF_DATABASE_URL` at `tf_system`; an
executor connected as a superuser logs an `ERROR` at startup naming the
problem, and keeps running.

## Database connections

Every pod opens two Postgres connection pools. Defaults per pool:

| Role | Per pool | Per pod |
|------|:---:|:---:|
| `control` | 25 | 50 |
| `executor` | 6 | 12 |

The total across pods, plus what GoTrue and admin tools use, must fit in
Postgres' `max_connections` (100 in the stock `supabase/postgres` image). One
control pod and two executors need 74. Two control pods and three executors
need 136, so either raise `max_connections` (and Postgres' memory) or lower
`TF_DB_MAX_OPEN_CONNS`, the per-pool ceiling (minimum 2).

## Multiple control pods (HA)

One control pod needs no proxy. With two or more, one holds the
`background-brain` lease and runs polling, routing and scoring; the others
stand by and take over within about 30s of the holder disappearing (the
`TF_LEASE_*` variables in `.env.example` tune this). Running agents are on
executors, so a failover doesn't interrupt them.

`GET /readyz` on each control pod includes a `lease` field
(`{name, holder_id, is_holder, term}`). A standby stays ready, so a load
balancer keeps every control pod in rotation.

Put any reverse proxy in front of the control pods. It needs:

- WebSocket upgrade passthrough (`/api/ws`).
- Long idle timeouts, since those connections stay open.
- Health checks on `GET /readyz`, removing a pod that returns 503.
- `X-Forwarded-For` set to the real client, with `TF_TRUSTED_PROXY_CIDR` set on
  the control pods to the proxy's address range
  ([Client IP & trusted proxies](networking.md)).

Caddy:

```caddy
triagefactory.yourcompany.com {
	reverse_proxy control-1:3000 control-2:3000 {
		lb_policy round_robin
		health_uri  /readyz
		health_interval 10s
		health_status 200
		transport http {
			read_timeout  1h
			write_timeout 1h
		}
	}
	# Caddy sets X-Forwarded-For by default; strip any client-supplied value
	# at the edge so only the proxy's reaches Triage Factory.
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

		proxy_http_version 1.1;
		proxy_set_header Upgrade    $http_upgrade;
		proxy_set_header Connection $connection_upgrade;

		proxy_read_timeout  1h;
		proxy_send_timeout  1h;
		proxy_buffering     off;

		# Replace (don't append) X-Forwarded-For with the real peer.
		proxy_set_header Host              $host;
		proxy_set_header X-Forwarded-For   $remote_addr;
		proxy_set_header X-Forwarded-Proto $scheme;
	}
}
```
