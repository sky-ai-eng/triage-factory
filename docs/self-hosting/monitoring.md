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
disabled), not zero remaining budget.

Webhook delivery health is deliberately **not** here. Workspace Settings →
GitHub access reports it per workspace — whether the App's webhook is
configured, points at this deployment, and is being accepted — by asking GitHub
directly (`/app/hook/config` + `/app/hook/deliveries`) rather than by waiting for
a delivery. It stays off `/readyz` for two reasons: that endpoint is
unauthenticated and carries no tenant configuration, and a workspace whose App is
hookless is degraded in its **installation mirror**, not in this process's health
— polling, task creation, and content processing are unaffected, and reporting a
GitHub-side misconfiguration as a failing check would take a pod out of rotation
for something no restart can fix.

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
the TF-specific set today covers dropped audit records and Slack message-event
volume.

### Dropped audit records

- `tf_audit_records_dropped_total{stage, op}` — external-write audit records
  that never reached the log of record. **Every sample is a bug or an
  incident**: an agent wrote something under an org credential, the write
  landed upstream, and TF has no `external_actions` row for it.

  This exists because the audit pipeline is fire-and-forget by contract. The
  external act has already happened by the time TF records it, so nothing
  downstream may fail the act to save the record — which means a failure is
  swallowed, and until it was counted, a hole in the log was invisible.

  `stage` says where it was lost, and only the last of the four is reachable in
  local mode (there is no sidecar and no relay there, so the rest staying empty
  is the path not taken, not a gap):

  | `stage` | What was lost |
  | --- | --- |
  | `relay_send` | The run's credential sidecar could not put the record on the supervision channel, and said so over that same channel. |
  | `relay_decode` | The record arrived at the orchestrator and could not be decoded, or named an op this binary has no handler for. |
  | `relay_dispatch` | A provider's own notify handler (Slack, …) ran and failed. |
  | `record` | The record reached the database write and the write failed. |

  `op` names what was being recorded, in the vocabulary of its stage: a
  `<namespace>.<op>` relay pair for the relay stages (`core.record_gh_write`,
  `slack.record_thread_root`, …), the row's own domain kind (`pull_request`,
  `gh_channel_write`, `pr_merged`) for the write.

  `other` means the name arrived on the wire and matches no op this binary
  serves. The label is clamped against the ops actually registered — the core
  catalog plus whatever providers registered at startup — because the sending
  process is the one exposed to hostile text, and an unclamped label would let
  it mint unbounded series in the exporter just by naming ops that do not
  exist. Nothing here names a repository, a target, or an id; the accompanying
  `WARN` line does, and it prints the wire name verbatim even when the label
  collapsed to `other`.

  One loss is outside what this can measure, and is accepted rather than
  fixed: a record written as a run is torn down can lose to the supervision
  channel closing, which takes the report of its own loss with it. There is no
  durable spool — the decision was counters first, revisit if they ever fire.

  Alertable at the first occurrence:

  ```
  sum(increase(tf_audit_records_dropped_total[1h])) > 0
  ```

### Slack ingest

Per Slack app (`message.channels`/`message.groups` are a firehose, unlike
mentions — watch that volume):

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

#### Following an event from the poll that found it

One trace does not span the whole pipeline, deliberately. A poll cycle emits
many events; each is routed later — seconds or minutes later, on whichever
control pod holds the background brain, and up to five times if it retries —
so a poll cycle's trace ends at the emit and each event's routing is its own
trace. Joining them into one would produce a single sprawling trace per cycle
that never quite ends.

They are joined by a **span link** instead. The durable queue row carries the
trace context of its `event.enqueue` span, and the `route.event` span that
routes it links back to that enqueue. In Grafana, an event's routing trace
names the poll cycle it came from, and the poll cycle's span lists what it
caused — one link, navigable from either end. Rows enqueued while tracing was
off carry no context and route identically, with no link.

Everything past the routing is correlated by ID rather than linked: a fired
trigger's `route.delegate` span carries the `conversation.id` of the run it
started, and the run's own trace — claimed by an executor, possibly minutes
later — carries the same. A TraceQL query on that attribute finds both sides.

#### Reading a delegated run's trace

One trace per **engagement attempt**, rooted at `engagement.setup`. It starts
when the dispatcher claims the run and ends when the agent goes live, covering
the claim-validity gates, the sandbox bring-up (`sandbox.network.setup`,
`sandbox.sidecar.launch`, `sidecar.hello`, `sidecar.provision`,
`sidecar.start_proxies`), the PR fetch and clone, the workspace, the staged
skill and memory, the SDK install, and the jail launch — plus a
`capbroker.call` span for every privileged host operation, since the
cap-broker exports nothing itself.

It ends at agent-live rather than at the run's end, and that is deliberate: a
span only exports once it ends, so one held open across an hours-long
streaming period would be lost entirely in a crash — exactly the runs you most
want the data for. A run requeued five times therefore produces five traces
sharing `conversation.id`, told apart by `claim.attempt`.

The root's `outcome` says how the attempt ended, and the distinctions are the
ones you actually filter on: `agent_live` reached the agent; `cancelled` was
stopped by someone; `shutdown` stood down because its executor was going away,
leaving the claim for the boot reconcile; `fenced` lost the conversation to a
successor; `setup_failed` is the only one that also carries an error status, so
"traces with errors" stays a list of things that are wrong rather than a list
of runs that were stopped.

What happens after agent-live appears as separate, punctual traces that **link
back** to the engagement root: `permission.prompt` (the human wait, which the
run's own duration accounting deliberately discounts), `relay.call` /
`relay.notify` (each exec verb the sandboxed agent relayed out),
`git.authorize` and `git.record_push`, `artifact.record`,
`workspace.snapshot`, and `engagement.complete`, which carries the run's cost,
duration and turn count. Open any of them and the links menu walks to the
setup trace, and vice versa.

`workspace.snapshot` gets read more closely than the others, because it is
the unbounded piece of a park: a git bundle, a tar of the whole scratch tree,
and a blob upload. Nothing a person is watching waits on it — the park records
that a persist is owed, flips the conversation, and captures afterwards — so a
slow one costs storage lag and a resume's wait rather than a run that reports
WORKING after you pressed stop. It splits into three children naming where:
`workspace.snapshot.capture` (the git delta plus
the session-transcript read, with `snapshot.bundle_bytes` /
`snapshot.patch_bytes` / `snapshot.transcript_bytes`),
`workspace.snapshot.archive` (the tar walk plus gzip, with
`snapshot.raw_bytes` in against `size_bytes` out — the compression ratio),
and `workspace.snapshot.put` (the blob write). Every span in the family
carries `runtime`, and it is load-bearing: only a delegated SDK run snapshots
a transcript, so transcript sizes are SDK-run data, not a property of all
snapshots. The family is identical in both modes save for what `capture`
covers — local runs the capture in-process, multi routes it through the
privileged capture child, which is span-free like every privileged process,
so the executor-side `capture` span is its measurement.

The read against it is `engagement.workspace.ensure`, on the setup trace of
whatever engagement picks the conversation back up. Its `workspace.provenance`
says how that engagement's tree came to be — `warm` (the parked tree was still
on this host), `rehydrated` (rebuilt from the blob), or `fresh` (there was no
blob to rebuild from, so the tree holds only what reached the remote, and the
agent is told so). `snapshot.waited_ms` is how long it spent waiting on a
persist that was still in flight: zero on nearly every ensure, and the thing
to look at when a resume is slow for no visible reason. A wait that runs to
the `TF_SNAPSHOT_WAIT_SEC` bound (default 60s) and then reads `fresh` means the
executor that owed the snapshot never produced one — pair it with that
executor's `workspace.snapshot` span, or its absence.

Two spans on that path measure someone else's work.
`engagement.credentials.await` is the executor sitting in
`awaiting_credentials` waiting for a bundle sealed to its sidecar's key, and
`credentials.provision` is the control pod producing it. They are **not**
linked, because the doorbell between them is lossy by design and the sweep is
the real completion path — a link would promise a handoff neither side can
guarantee. Query `conversation.id` to see both.

A local-mode run traces a shorter version of the same thing: with no sandbox
there is no broker, sidecar, or credential span at all, and the trace is
fetch → clone → staging → SDK → spawn. Missing spans there are the path not
taken, not a failure.

### The bundled trace stack

`docker compose up -d` brings up a trace backend alongside TF, and
`TF_TRACES_ENDPOINT` defaults to it, so a fresh deployment traces itself. Open
Grafana at <http://localhost:3030> — it lands directly on the bundled overview
dashboard, which needs no queries typed to be useful.

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
config, Prometheus's scrape config, and Grafana's data sources and dashboards.
One detail worth knowing: Prometheus finds TF's `:9464` by DNS service discovery
rather than static targets, so `--scale executor=3` is scraped as three targets
instead of whichever replica DNS happened to answer with.

#### Bundled dashboards

Grafana's home page is the **Triage Factory — Overview** dashboard, provisioned
from [`docker/observability/dashboards/`](../../docker/observability/dashboards/)
the same way the data sources are. Seven rows — three answering "is TF healthy,
and what is slow", then one each for the three pipelines whose spans need
reading rather than aggregating, and one that should stay empty:

- **Traces.** Five fixed TraceQL searches — GitHub and Jira poll cycles, system
  job cycles (scorer / profiler / classifier), API requests slower than 500 ms,
  and every span that ended with an error status. Each row is one trace and
  clicking it opens that trace's waterfall. Two of these are *supposed* to be
  empty on a healthy deployment; the slow-request and errored-span lists filling
  up is the signal.
- **RED.** Request rate, error rate, and p95 latency by span name, over the
  `traces_spanmetrics_*` series Tempo's metrics generator derives from the spans
  it receives. This covers every instrumented subsystem at once and grows on its
  own as new span families land — nothing here enumerates subsystems by hand.
  The latency panel has **exemplars on**: each dot is one sample annotated with
  the trace ID that produced it, so clicking one jumps straight to the trace
  behind an outlier.
- **Runtime.** Per-pod signals from `:9464`: DB pool connections and contention
  (per pool — multi mode's `admin` and `app` behave nothing alike), HTTP
  requests by response status, goroutines, RSS, and the Slack ingest counters.
  Deliberately short: things you would act on, not a runtime-metrics museum.
  The Slack panel stays empty unless a Slack app is connected.
- **Event pipeline.** One row per routed event, carrying the two things the RED
  row structurally cannot show: `queue.wait_ms` (the enqueue→claim interval,
  over before the consumer span starts) and the routing `disposition`. Both are
  span *attributes*, and the `traces_spanmetrics_*` series carry only service,
  span name, kind and status — so this is a span list you sort and filter rather
  than a series you graph, which is what reading "which event went which way"
  actually wants. Graphing them is still available, one
  [TraceQL metrics](#traceql-metrics-and-traces-drilldown) query away
  (`{ name = "route.event" } | count_over_time() by (span.disposition)`), it is
  just not what this row is for. Alongside it, the `route.delegate` rate: how much
  auto-delegation a trigger's configuration is actually causing, which has no
  other display anywhere. Note `error` there is a disposition, not a span error
  status — the store call that failed carries the status on its own span.
- **Delegated runs.** The engagement setup, claim to agent-live: a list of
  attempts with their `outcome` and `claim.attempt`, the phase breakdown that
  answers "why did that run take four minutes to start", the credential park
  next to the control pod's seal, and every `capbroker.call` by `op` — which is
  the *only* record the cap-broker's work has anywhere, since it exports no
  spans of its own. An error rate over engagements has to read the `outcome`
  attribute rather than the span status: `setup_failed` is the one outcome that
  is also an error, so counting statuses would count every stopped run as a
  failure. A local-mode run populates a subset of this row — there is no broker,
  sidecar, or credential span there at all — and those empty panels are the path
  not taken, not a fault.
- **Workspace snapshots.** The `workspace.snapshot` family described above: one
  row per snapshot with its duration and stored footprint, the
  capture/archive/put phase breakdown, the capture member sizes, and the
  archive's raw-in/compressed-out pair. Unlike the setup row, this one is
  **fully populated in local mode** — the snapshot pipeline is the same code in
  both modes, and tracing is gated on `TF_TRACES_ENDPOINT` alone. The one
  mode difference is inside `capture`: multi runs it in a privileged,
  span-free child, so the executor-side span is that child's only timing.
  Filter the member-size panels on `runtime` before reading transcript sizes —
  the transcript member exists only for delegated SDK runs.
- **Audit pipeline.** `tf_audit_records_dropped_total` three ways: the total
  over the range, the rate by `stage`, and a filterable table by `op`. Unlike
  the trace rows, this one is read for its *absence* — flat zero is the
  expected shape and the only good one, since a single sample means the
  `external_actions` log is missing a write that really happened. See
  [dropped audit records](#dropped-audit-records) for what each stage means,
  which of them local mode can reach, and the one loss the counters
  deliberately cannot see.

Everything the dashboard displays is span names, opaque IDs, and closed enums —
never a repo name, username, or PR title — the same rule the spans and metrics
themselves follow.

The dashboards are **read-only**, and that is the point of checking the JSON in:
the file is the source of truth, so deleting the `grafana-data` volume and
re-upping reproduces them exactly. Grafana refuses a save-in-place with "Cannot
save provisioned dashboard". To experiment, use **Export → Save as copy** in the
dashboard menu — that lands a mutable duplicate in Grafana's own database, which
you can edit freely and which the provisioner will not touch. To change the
checked-in one, edit the JSON: the provisioner re-reads the directory on a timer,
so a save shows up without restarting the container.

Adding a dashboard is a file — drop another `.json` next to `tf-overview.json`
and it is picked up. Reference data sources by the pinned UIDs (`tf-tempo`,
`tf-prometheus`) rather than by name; that pinning is what lets checked-in JSON
survive a fresh volume.

#### Swapping pieces out

The compose file is yours to edit, and these three services are the parts most
likely to duplicate something you already run:

- **Already have Prometheus?** Delete the `prometheus` service, scrape each
  pod's `:9464` with yours, and update the Prometheus URL in
  `docker/observability/grafana-datasources.yaml`. Tempo's span-metrics
  `remote_write` needs a `--web.enable-remote-write-receiver` target — repoint
  it or drop that block from `docker/observability/tempo.yaml`.
- **Already have Grafana?** Delete the `grafana` service and copy the two data
  sources and three correlations out of that same file, plus the dashboards in
  `docker/observability/dashboards/`. They address data sources by UID, so keep
  `tf-tempo` / `tf-prometheus` or rewrite the references.
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

#### Reaching Tempo and Prometheus directly

Two things Grafana can't show you: Prometheus's scrape-status page (whether it
is actually hitting every executor — `up{job="triagefactory"}` answers most of
it as a query) and Tempo's operational endpoints. Neither publishes a port, so
go through `exec`, and mind that the two images differ:

```sh
# Prometheus ships busybox wget, no curl
docker compose exec prometheus wget -qO- 'http://localhost:9090/api/v1/targets?state=any'

# Tempo's image is distroless — no shell to exec into, so use a neighbour's curl
docker compose exec grafana curl -sS http://tempo:3200/status/services
```

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
  the trace IDs attached to histogram samples are stored, and the Prometheus
  data source turns each into a link into Tempo. A spike in a latency panel
  becomes a click through to one slow request — that is what the overview's p95
  panel does. Two producers feed it and they spell the label differently: TF's
  own OTel SDK writes `trace_id` on the `tf_*` histograms, Tempo's
  metrics-generator writes `traceID` on `traces_spanmetrics_*`. The data source
  declares both, because Grafana only renders the link when a declared name
  matches a label the exemplar actually carries — with the spelling missing you
  get a dot you can hover but not click.
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

#### TraceQL metrics and Traces Drilldown

Span metrics are derived ahead of time and carry four labels — service, span
name, kind, status — so they answer "how many, how slow" and nothing about what
a span was *about*. **TraceQL metrics** are the other half of the question:
`{ name = "route.event" } | count_over_time() by (span.disposition)` aggregates
the stored spans when the query runs, so any attribute TF stamps is a grouping
key without anyone having declared it in advance. Grafana's **Traces Drilldown**
is a UI over exactly these queries, and `| rate()` typed into Explore is the
same machinery.

Drilldown itself is Grafana's, not something this stack provisions: Grafana
installs that app at startup by fetching it from `grafana.com`, so on a host
with no outbound access the Drilldown section is simply missing — a different
symptom from the one below, where the section is there and every panel in it
errors.

What answers them is the `local-blocks` processor in
`docker/observability/tempo.yaml`, which keeps the spans themselves rather than
a summary of them. It is not optional, and its absence is loud rather than
empty: with the processor off, every metrics query and every Drilldown panel
fails with *localblocks processor not found*. Three settings there are
load-bearing, all checked in:

- `metrics_generator.traces_storage.path` — where the processor keeps those
  spans. Required whenever it is enabled, and the blast radius is wider than
  the feature: with no path the generator refuses to initialize at all, taking
  span metrics and the service graph down with it.
- `filter_server_spans: false` — the default admits server-kind spans and trace
  roots, which on TF's traces means the HTTP surface and the outside of every
  pipeline. The interior spans (`engagement.clone`, `sidecar.provision`,
  `capbroker.call`) are the ones worth aggregating.
- `flush_to_storage: true` — completed blocks land in the trace storage the
  ingester also writes to, which is what lets a query reach past the
  generator's own window, at the cost of a second copy of every span.

Two of Tempo's defaults shape what you can ask, and neither is written into the
checked-in config: a metrics query's range is capped at
`query_frontend.metrics.max_duration` (3h — a wider time picker is rejected
rather than truncated), and anything older than
`query_frontend.metrics.query_backend_after` (30m) is answered from backend
blocks rather than from the generator. Set them in `tempo.yaml` if a three-hour
ceiling is shorter than what you are chasing.

#### Retention

Tempo keeps blocks for 7 days and Prometheus keeps samples for 7 days, matched
on purpose so an exemplar never outlives the trace it links to. `tempo-data`
holds each span twice inside that window — the ingester's block and the
metrics-generator's flushed copy, the one TraceQL metrics read — so size it for
roughly double what the traces alone would take. All three volumes
(`tempo-data`, `prometheus-data`, `grafana-data`) are caches: losing them loses
observability history, never TF state.

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
