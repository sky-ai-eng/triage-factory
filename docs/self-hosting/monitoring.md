# Monitoring & health checks

Triage Factory exposes three health endpoints. Point liveness at `/api/health`,
readiness/load-balancer rotation at `/readyz`, and (in a scaled fleet) the
container HEALTHCHECK for each executor at its localhost `/healthz`.

## Readiness — `GET /readyz`

Point your load balancer / uptime check at `GET /readyz` (a bare path, not under
`/api/`) rather than `/api/health`. `/api/health` is a liveness probe only — no DB
or integration check, by design, so a flapping dependency can't trip a platform's
auto-restart-on-liveness-failure. `/readyz` is the readiness signal: it 503s on a
**hard** failure (DB unreachable, migrations not applied, or a poller's base-tick
loop stalled/crashed) and reports, without ever 503ing on them, **soft** signals —
each org's last-successful-poll age vs its configured poll interval, each org's
last-observed GitHub primary rate-limit budget, and a count of in-flight
(`queued`/`running`) agent runs:

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

An org absent from `rate_limit.github` has no observation yet this process (never
polled, or its host omits rate-limit headers — e.g. GHES with rate limiting
disabled), not zero remaining budget. Webhook-delivery freshness has no equivalent
section yet — webhook ingestion invariants (dedup, reconciliation linkage) haven't
landed, so there is no state to report.

Alert on `age_seconds > 3 × interval_seconds` for whichever source/org you care
about — that threshold is yours to pick; `/readyz` reports the raw numbers rather
than guessing at your alerting policy (its own top-level `status` field applies
that same 3x default and surfaces it as `"degraded"`, purely as a quick-glance
dashboard signal, never as a 503). `/readyz` is unauthenticated by design, same as
`/api/health` — the response carries only opaque org IDs, never repo names,
usernames, or other tenant data, so there is no `?verbose=` mode and none is
needed.

In an HA (multiple control pods) topology `/readyz` also carries a `lease` field
and a standby hard-checks only DB + migrations — see
[Scaling out](scaling.md#multiple-control-pods-ha).

## Executor health — `GET /healthz`

Each executor exposes a localhost-only `GET /healthz` (default `127.0.0.1:3001`,
`TF_HEALTHZ_PORT`) — the container HEALTHCHECK target. It reports:

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

It returns **503** when the dispatcher loop has stopped, the last fleet-registry
heartbeat write is older than 3× the heartbeat interval, **or** the cap-broker
stops answering (`broker_ok:false` — an executor that can't launch sandboxes is
useless). `draining` and `fenced` are informational and never flip the code: a
fenced executor stays `200`-but-`fenced:true` so the HEALTHCHECK doesn't kill it
before it can quiesce. The instance registry (`instances` table) carries each
pod's role and build version, so version skew across a rolling deploy is visible
there.
