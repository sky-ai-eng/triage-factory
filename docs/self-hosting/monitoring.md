# Monitoring & health checks

Triage Factory exposes three health endpoints and a Prometheus metrics
endpoint. Point liveness at `/api/health`, readiness/load-balancer rotation at
`/readyz`, (in a scaled fleet) the container HEALTHCHECK for each executor at
its localhost `/healthz`, and your metrics scraper at `:9464/metrics`.

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

## Metrics — `GET /metrics`

Every pod (control and executor alike) serves Prometheus-format metrics on its
own dedicated listener — **`:9464` by default in multi mode** — instrumented
through the OpenTelemetry SDK. Nothing is bundled and nothing is required:
point whatever you already run at it (Prometheus, an OTel Collector's
`prometheus` receiver, a vendor agent), or just read it by hand:

```sh
curl -fsS http://localhost:9464/metrics | grep '^tf_'
```

`TF_METRICS_ADDR` overrides the bind (a bare port or a full `host:port`);
`TF_METRICS_ADDR=off` disables the listener. In local mode it is off unless
explicitly set, and a bare port binds loopback. The endpoint is
unauthenticated by design, same posture as `/readyz` — it carries only counts
and opaque IDs, never tenant data — and its port is deliberately separate
from the user-facing server so it stays network-internal: **don't publish or
route `9464` externally**; scrape it from inside the compose network / cluster.

Beyond the standard Go runtime and process collectors (`go_*`, `process_*`),
the TF-specific set today covers Slack message-event volume, per Slack app
(`message.channels`/`message.groups` are a firehose, unlike mentions — watch
that volume):

- `tf_slack_ingest_events_total{app_id, outcome}` — every delivery reaching
  the ingest pipeline; `outcome` is `accepted` or the drop reason
  (`duplicate`, `not_engaged`, `not_thread_reply`, `unsupported_subtype`, …),
  so the sum over outcomes is the received total.
- `tf_slack_retry_deliveries_total{app_id, transport}` — deliveries Slack
  marked as retries (`X-Slack-Retry-Num` > 0, or a Socket Mode envelope's
  `retry_attempt` > 0). **This is the alertable signal**: on the Events API,
  sustained retries are the precursor to Slack disabling the app's event
  subscription. Each occurrence is also a `WARN` log line for log-based
  alerting.
- `tf_slack_app_rate_limited_total{app_id}` — `app_rate_limited` notices:
  Slack exhausted the app's Events API budget (30k events/workspace/hour) and
  is dropping deliveries at the source (they are never retried). Also a
  `WARN` log line. If this fires, move the app to Socket Mode or reduce the
  channels the bot is in.

An example alert, in Prometheus terms (counters are per-process; `sum` across
pods in an HA topology):

```
sum(increase(tf_slack_retry_deliveries_total[15m])) > 0
```

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
