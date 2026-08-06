# Monitoring & health checks

Triage Factory exposes three health endpoints, a Prometheus metrics endpoint,
and — when you configure a backend — an OTLP trace stream. Point liveness at
`/api/health`, readiness/load-balancer rotation at `/readyz`, (in a scaled
fleet) the container HEALTHCHECK for each executor at its localhost
`/healthz`, and your metrics scraper at `:9464/metrics`. Traces work the
other way around: nothing scrapes them, the process pushes them to the
endpoint you name in `TF_TRACES_ENDPOINT`.

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

## Traces — `TF_TRACES_ENDPOINT`

Tracing is **off in every mode until you set `TF_TRACES_ENDPOINT`**, and that
one variable is the whole switch:

```sh
TF_TRACES_ENDPOINT=http://tempo:4318
```

Point it at anything that speaks **OTLP over HTTP** — Grafana Tempo, Jaeger,
an OpenTelemetry Collector, a vendor endpoint. The compose stack ships a Tempo
and defaults this variable to it (see [below](#the-bundled-trace-stack)); local
mode leaves it unset, so tracing there is off until you name a backend. A value
with no scheme is read as
plaintext (`tempo:4318` → `http://tempo:4318`); spell `https://` out for a
TLS backend. A path is used as given and defaults to `/v1/traces`.
`TF_TRACES_ENDPOINT=off` disables tracing, which is how you override an
inherited value.

**Traces are pushed, not scraped — and that difference is the thing to
internalize.** Prometheus cannot receive or store them: there is no port to
scrape and no endpoint to curl. Each process batches finished spans and POSTs
them to the endpoint above, which means the backend has to be reachable *from
the pod*, and it means an unreachable backend costs you spans rather than
raising an alert. Watch the process's own logs for `opentelemetry sdk error`
records if spans never show up.

Every process traces itself the same way — control pods and executors alike,
local mode and multi mode alike. Two processes deliberately never emit spans
at all: the **cap-broker** and the per-run **credential sidecar**. Giving the
most privileged process on the host a new outbound network dependency, or
punching an OTLP destination through the sidecar's fail-closed egress
allowlist, is not worth broker-internal timing. Their work is visible as
client-side spans in the executor around each IPC call.

Standard `OTEL_EXPORTER_OTLP_*` variables still reach the exporter for the
knobs TF does not wrap — headers (`OTEL_EXPORTER_OTLP_HEADERS`, for a
vendor's auth token), timeout, compression, TLS. The exceptions are the two
endpoint variables, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` and
`OTEL_EXPORTER_OTLP_ENDPOINT`: `TF_TRACES_ENDPOINT` overrides both, and
neither enables tracing on its own. An OTLP address inherited from a compose
file or a sidecar shouldn't quietly start a trace pipeline nobody asked this
process for.

If you set one of them and TF is not tracing, you get a warning naming the
variable rather than silence:

```
WARN an OTLP endpoint is configured but TF_TRACES_ENDPOINT is not; tracing
     stays disabled (set TF_TRACES_ENDPOINT to enable it)
     ignored=[OTEL_EXPORTER_OTLP_TRACES_ENDPOINT]
```

Set `TF_TRACES_ENDPOINT` to the same address to turn it on, or
`TF_TRACES_ENDPOINT=off` to say you meant it and silence the warning.

Same don't-publish-externally posture as `:9464`: spans carry opaque IDs
(`org.id`, `conversation.id`, `task.id`) and never repo names, usernames, PR
titles, or message text — but they are unauthenticated on the wire unless you
configure TLS, so keep the collector inside the compose network / cluster.

Two joins tie the three signals together, and neither needs a database
column:

- **Logs → traces.** Any log line emitted while a span is active carries
  `trace_id` and `span_id`. Grep a trace ID out of the logs, paste it into
  your trace UI.
- **Metrics → traces (exemplars).** `/metrics` serves OpenMetrics to any
  scraper that negotiates for it, which is what carries **exemplars** —
  individual samples annotated with the trace ID of the request that produced
  them. Enable them on your scraper (`--enable-feature=exemplar-storage` on
  Prometheus) and a spike in a latency histogram becomes a click through to
  a trace of one slow request. Scrapers that don't negotiate OpenMetrics see
  the same plain text as before.

Shutdown flushes: a batch of finished spans that hasn't been exported yet is
written out on SIGTERM before the process exits, so the traces covering a
restart survive it.

### The bundled trace stack

`docker compose up -d` brings up a trace backend alongside TF, and
`TF_TRACES_ENDPOINT` defaults to it, so a fresh deployment traces itself. Open
Grafana at <http://localhost:3030>.

| Service | Role | Published |
| --- | --- | --- |
| `tempo` | trace backend — OTLP/HTTP on `:4318`, query API on `:3200` | nothing |
| `prometheus` | scrapes every pod's `:9464`; receives Tempo's generated span metrics | nothing |
| `grafana` | the UI that joins them; data sources and correlations provisioned from disk | `127.0.0.1:3030` |

Grafana is the only one reachable from the host, and only on loopback — reach
it on a remote box over an SSH tunnel (`ssh -L 3030:localhost:3030 …`). It runs
with anonymous Editor access and no login form, which suits a loopback UI whose
entire contents come from provisioning. **None of it is meant to be
internet-facing**: to expose the trace UI, put Grafana behind the same reverse
proxy and auth as everything else and give it accounts. The same applies to
pointing TF at a Tempo off the compose network — spans are unauthenticated on
the wire unless you configure TLS.

Config lives in [`docker/observability/`](../../docker/observability/) — Tempo's
config, Prometheus's scrape config, and Grafana's data sources. One detail worth
knowing: Prometheus finds TF's `:9464` by DNS service discovery rather than
static targets, so `--scale executor=3` is scraped as three targets instead of
whichever replica DNS happened to answer with.

#### Swapping pieces out

The compose file is yours to edit, and these three services are the parts most
likely to duplicate something you already run:

- **Already have Prometheus?** Delete the `prometheus` service, scrape each
  pod's `:9464` with yours, and update the Prometheus URL in
  `docker/observability/grafana-datasources.yaml`. Tempo's span-metrics
  `remote_write` needs a `--web.enable-remote-write-receiver` target — repoint
  it or drop that block from `docker/observability/tempo.yaml`.
- **Already have Grafana?** Delete the `grafana` service and copy the two data
  sources and three correlations out of that same file.
- **Already have a trace backend?** Keep Grafana and Prometheus if you want
  them, delete `tempo`, and set `TF_TRACES_ENDPOINT` to your collector.
- **Want no tracing?** Delete `tempo` and set `TF_TRACES_ENDPOINT=off`. Without
  the second half TF keeps posting spans to a name that no longer resolves and
  logs an export error every batch interval.

#### Is the pipe working?

Spans are pushed, so an unreachable backend costs you spans rather than raising
an alert — and "nothing in Grafana" looks identical whether TF never exported or
Tempo never stored. Pushing one span by hand separates the two:

```sh
docker compose exec grafana sh -c '
  s=$(date +%s)
  curl -sS -X POST http://tempo:4318/v1/traces -H "content-type: application/json" -d "{
    \"resourceSpans\":[{\"resource\":{\"attributes\":[{\"key\":\"service.name\",\"value\":{\"stringValue\":\"smoke\"}}]},
    \"scopeSpans\":[{\"spans\":[{\"traceId\":\"1234567890abcdef1234567890abcdef\",\"spanId\":\"1234567890abcdef\",
    \"name\":\"smoke.test\",\"kind\":1,
    \"startTimeUnixNano\":\"$((s-1))000000000\",\"endTimeUnixNano\":\"${s}000000000\",
    \"attributes\":[{\"key\":\"conversation.id\",\"value\":{\"stringValue\":\"smoke-1\"}}]}]}]}]}"'
```

(Second-precision timestamps because `date +%s%N` — nanoseconds — exists on
neither the container's busybox nor macOS, and a span stamped in 1970 is stored
happily and then never appears in any search.)

Then look for it in Grafana (Explore → Tempo → `{ .conversation.id = "smoke-1" }`;
allow ~10s, a trace is searchable once the ingester has cut it). If the hand-sent
span shows up and TF's don't, the problem is on TF's side — check the process
logs for `tracing enabled` at boot and `opentelemetry sdk error` after it. If
neither shows up, it's Tempo: `tempo_distributor_spans_received_total` in
Prometheus counts what actually arrived.

#### Clicking from a span to everything related

Domain IDs ride on spans as attributes — `conversation.id`, `event.id`,
`task.id` — and never the other way around: no TF table stores a trace ID,
because traces age out on a much shorter clock than the rows would, and the
column would mostly point at deleted traces. The way back is therefore a search
on the attribute, and Grafana's provisioning turns that search into a click.
Open any span with a `conversation.id` and its links menu offers **Traces for
this conversation**, which runs `{ .conversation.id = "…" }` — every other trace
that touched the same conversation, across pods and across pipeline stages that
deliberately do not share a trace. Same for `event.id` and `task.id`.

Adding another attribute to that list is a `correlations:` entry in
`docker/observability/grafana-datasources.yaml` plus a restart of the Grafana
container; the file's comments walk through the shape.

#### Metrics ↔ traces

Two things wire the metrics half in, both provisioned:

- **Exemplars.** Prometheus runs with `--enable-feature=exemplar-storage`, so
  the trace IDs TF attaches to histogram samples are stored, and the Prometheus
  data source turns each into a link into Tempo. A spike in a latency panel
  becomes a click through to one slow request.
- **Span metrics and the service graph.** Tempo's metrics-generator derives RED
  metrics (`traces_spanmetrics_*`) and service-graph edges from the spans it
  receives and remote-writes them to Prometheus. That is what fills the trace
  view's **Service Graph** tab and the trace-to-metrics links on a span, and it
  works regardless of what TF itself instruments.

Trace-to-**logs** is deliberately not configured: it needs a log store to point
at and this stack ships none. The logs↔traces join is still there — every line
emitted under a span carries `trace_id` — it is just `docker compose logs | grep
<trace_id>` rather than a click. Adding a Loki container and a `tracesToLogsV2`
block to the Tempo data source is the upgrade path.

#### Retention

Tempo keeps blocks for 7 days and Prometheus keeps samples for 7 days, matched
on purpose so an exemplar never outlives the trace it links to. All three
volumes (`tempo-data`, `prometheus-data`, `grafana-data`) are caches: losing
them loses observability history, never TF state.

For the local-mode dev loop — a single `docker run` of Tempo against a TF binary
running on the host — see [Environment tuning →
Tracing](../local-mode/tuning.md#tracing).

## Executor health — `GET /healthz`

Each executor exposes a localhost-only `GET /healthz` (default `127.0.0.1:3001`,
`TF_HEALTHZ_PORT`) — the container HEALTHCHECK target. **That loopback address is
the executor container's own**, not the host's: the executor publishes no ports
at all, so nothing you curl from the host reaches it. Read it through the
container:

```sh
docker compose exec executor curl -fsS http://127.0.0.1:3001/healthz | jq .
```

It reports:

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
