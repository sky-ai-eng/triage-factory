# Monitoring

Triage Factory exposes health endpoints, Prometheus metrics, and (when you
name a backend) OTLP traces. The compose stack ships Prometheus, Tempo, and
Grafana preconfigured for all three.

## Health endpoints

| Endpoint          | Served by                                       | Use it for                            | Fails (503) when                                                                                          |
| ----------------- | ----------------------------------------------- | ------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `GET /api/health` | control, `:3000`                                | Liveness                              | Never on a dependency, so a flapping database can't cause a restart loop.                                 |
| `GET /readyz`     | control, `:3000`                                | Load-balancer rotation, uptime checks | The database is unreachable, migrations are not applied, or a poller has made no progress for 90 seconds. |
| `GET /healthz`    | executor, `127.0.0.1:3001` inside the container | The executor's HEALTHCHECK            | See [Executor `/healthz`](#executor-healthz).                                                             |
| `GET /metrics`    | every pod, `:9464`                              | Prometheus scrape                     | —                                                                                                         |

All four are unauthenticated and carry only counts and opaque org ids. Keep
`:9464` inside the compose network or cluster.

### `/readyz`

```sh
curl -fsS http://localhost:3000/readyz | jq .
```

```json
{
  "status": "ok",
  "version": "v1.12.0",
  "checks": {
    "db": "ok",
    "migrations": "ok",
    "poller_github": "ok",
    "poller_jira": "ok",
    "poller_linear": "ok"
  },
  "sources": {
    "github": {
      "<org_id>": {
        "last_success_unix": 1783300000,
        "age_seconds": 42,
        "interval_seconds": 300
      }
    }
  },
  "active_runs": 3
}
```

`checks` are the hard checks behind the 503. `sources` is each org's last
successful poll per source and never causes a 503: alert on
`age_seconds > 3 × interval_seconds` (the top-level `status` reads `degraded`
at that point). A cycle that couldn't reach the source is not a success, so
`age_seconds` keeps growing through an outage.

With several control pods, a standby checks only `db` and `migrations`,
reports each poller as `"standby"`, and adds a `lease` field (`name`,
`holder_id`, `is_holder`, `term`). See [Scaling out](scaling.md). GitHub
webhook health is shown per organization under **Organization → Settings →
GitHub access**, not here.

### Executor `/healthz`

The executor’s container health check calls this endpoint, so
`docker compose ps` and `triagefactory deploy doctor` already show whether it
passes. It listens on the container’s loopback interface only, so to read the
full response, run `curl` inside the container:

```sh
docker compose exec executor curl -fsS http://127.0.0.1:3001/healthz | jq .
```

```json
{
  "dispatcher_alive": true,
  "last_heartbeat_write_age_sec": 3,
  "heartbeat_ever_written": true,
  "broker_ok": true,
  "active_runs": 2,
  "draining": false,
  "fenced": false,
  "shutting_down": false
}
```

It returns 503 when the dispatcher has stopped, the last heartbeat is older
than three heartbeat intervals, or the cap-broker is not answering. `draining`
and `fenced` never change the status. `shutting_down` is set on SIGTERM while
the executor hands its runs back, so a 503 with `shutting_down: true` is a
normal graceful shutdown. `TF_HEALTHZ_PORT` changes the port.

## Metrics

Every pod serves Prometheus metrics on `:9464`. `TF_METRICS_ADDR` changes the
bind (a port or `host:port`), and `off` disables it. Local mode serves none
unless `TF_METRICS_ADDR` is set.

Every metric carries a description of what it measures. To list them:

```sh
docker compose exec control curl -fsS http://localhost:9464/metrics | grep '^# HELP tf_'
```

A counter appears only after its first event, so a quiet pod lists fewer.

When you combine pods in a query:

- **Counters and histograms** are per process, so `sum` them.
- **Gauges about the deployment's queues and claims** (`tf_claims_*`,
  `tf_work_*`, `tf_conversations_deferred`, `tf_upstream_up`,
  `tf_commit_signing_oldest_pending_age_seconds`) come only from the control
  pod holding the background-brain lease. Don't sum them.
- **Rate-limit readings** (`tf_upstream_rate_limit_*`) are the upstream's own
  count, so take `min`. Slack's `tf_upstream_rate_limit_used` is the
  exception: `sum` it.

### Mergeability probes

Triage Factory learns whether a pull request conflicts by asking GitHub only
when the answer can have changed: a new pull request, a new head, a new base
branch (the pull request was retargeted), a move of its base branch that
touches the files it changes, a fresh round of reads after GitHub kept
answering `UNKNOWN`, or a sweep, which the first poll more than 6 hours after
the last read admits. Each poll checks the tip of
every base branch an open pull request targets when the repository's open
pull requests changed, and at least every 10 minutes otherwise. These metrics
show what that costs and how it is going:

| Metric                                | What it counts                                                                                                                                                    |
| ------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `tf_mergeability_probes_total`        | Pull requests asked about, by `reason` (`new`, `head_moved`, `base_changed`, `base_moved`, `base_reset`, `unknown_retry`, `sweep`). Each read makes GitHub run a test merge, which on GitHub Enterprise Server is your server's work. |
| `tf_mergeability_answers_total`       | GitHub's answers, by `value` (`MERGEABLE`, `CONFLICTING`, `UNKNOWN`). A high share of `UNKNOWN` means GitHub is slow to compute mergeability.                       |
| `tf_mergeability_unknown_giveups_total` | Pull requests GitHub answered `UNKNOWN` for ten times in a row. Each is asked about again 10 minutes later, counted under `reason="unknown_retry"`; the pause doubles with each give-up in a row, up to 6 hours. A steady rate means GitHub is persistently slow to compute mergeability. |
| `tf_mergeability_sweep_changes_total` | Sweep reads that found the answer changed. A nonzero rate means mergeability changed without Triage Factory reading it after the push, retarget or base-branch move that caused it, so the change was seen up to 6 hours late. |
| `tf_mergeability_base_moves_total`    | Base-branch moves processed, by `outcome`: `overlap` (only the pull requests whose changed files overlap the move were asked about), `identical`, or a reset that asked about every open pull request on the branch: `reset_first_sight` (a branch seen for the first time), `reset_truncated` (a move too large to list), `reset_rewritten` (a force-push or rewind), `reset_missing_tip` (the previous tip no longer exists), `reset_unreadable` (GitHub kept failing to compare the previous tip with the new one for 20 minutes). A steady reset rate on a busy branch costs a test merge per open pull request each time. |
| `tf_mergeability_overlap_set_size`    | A histogram, per `overlap` move, of the share of the branch's open pull requests that were asked about. About 1% on average in large repositories.                                       |
| `tf_mergeability_path_fetches_total`  | Requests sent to read pull requests' changed files, by `source`: `graphql` (the files) or `rest_renames` (the previous names of renamed and copied files). A pull request's files are read only when its base branch moves after its head or base changed. |
| `tf_mergeability_dropped_total`       | Probes given up without an answer, by `org_id` and `outcome`: `dependency_down`, `transient` or `deadline` after five failed reads, or `no_credential` at once when no GitHub credential the organization has connected can read the repository. The next poll queues the pull request again. A steady `no_credential` rate is a tracked repository outside the GitHub App's grant, which the organization's GitHub access settings list; any other steady rate means TF cannot reach GitHub. |

The probe queue is `kind="pr_mergeability_probes"` on the `tf_work_*` gauges
and alerts. A probe is not read until 15 seconds after it is queued, so its
objective for the oldest ready probe is two minutes. A probe that hits a
GitHub rate limit waits for the limit to reset and counts as deferred, not
failed. A probe whose reads keep failing is given up after five attempts and
queued again by the next poll, so an outage delays conflict detection rather
than stopping it. A probe parks only when it reaches ten attempts, which takes
repeated crashes or failures inside Triage Factory itself, since a failed read
from the fifth attempt on drops it instead; a parked probe stops that pull
request's conflict detection until an admin redrives it under **Parked work**.

### Alerts

The bundled Prometheus loads the rules in
[`docker/observability/rules/`](../../../docker/observability/rules/). Each
alert's description, shown beside it in Grafana, says what to check.

| Alert                           | Severity | Fires when                                                                                                                          |
| ------------------------------- | -------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| `UpstreamFailingFleetWide`      | critical | More than half of the orgs using an upstream are failing against it, for 10 minutes.                                                |
| `UpstreamDownFleetWide`         | critical | More than half of the orgs' last polls got no answer from GitHub or Jira, for 5 minutes.                                            |
| `ConversationsDeferred`         | warning  | Runs have waited 30 minutes on an unavailable model provider, or on GitHub during setup.                                            |
| `BackgroundBrainNotRunning`     | critical | No control pod has run polling, routing and scoring for 5 minutes.                                                                  |
| `WorkQueueStopped`              | critical | A queue has ready work that nothing has claimed for about 10 minutes.                                                               |
| `WorkQueueBehind`               | warning  | An org's oldest ready item has waited past the queue's objective, for 5 minutes.                                                    |
| `WorkParked`                    | warning  | An org has work parked for 15 minutes, waiting for an admin under **Parked work** in the org settings.                              |
| `ClaimsExpiredUnreleased`       | critical | A run's claim has stayed expired with no executor taking it over.                                                                   |
| `WorkspaceCheckpointsStale`     | warning  | A live run has gone 45 minutes without a workspace checkpoint.                                                                      |
| `WorkspaceCheckpointsFailing`   | warning  | A workspace checkpoint failed in the last hour.                                                                                     |
| `SidecarFrameOversized`         | critical | A run's credential sidecar sent a frame over the size limit. Treat that sidecar as compromised.                                     |
| `CommitSigningStalled`          | critical | A commit signature request has waited over 5 seconds for a minute.                                                                  |
| `CommitSignaturesUnanswered`    | warning  | Signature requests went unanswered in the last 15 minutes.                                                                          |
| `CommitSignaturesRefusedForRun` | warning  | A run asked to sign past its budget, after its engagement ended, or as someone other than the signer.                               |
| `DeploymentCommitSignerDown`    | critical | A control pod's [deployment commit signer](deployment-integrations/deployment-commit-signer.md) has not been active for 15 minutes. |
| `AuditRecordsDropped`           | warning  | An agent's write landed without its audit record.                                                                                   |
| `TasksOpenOnClosedEntity`       | warning  | An org has tasks open on a pull request or issue that Triage Factory has closed, which is a bug.                                    |
| `SlackDeliveriesRetried`        | warning  | Slack retried deliveries to an app in the last 15 minutes.                                                                          |
| `JiraDeploymentAppQuotaLow`     | warning  | The deployment's shared Atlassian app has reported its Jira quota near its limit, for 10 minutes.                                   |

The two fleet-wide connection alerts need at least two orgs. One org's
failures are usually its own host, VPN or credential, and show on the
dashboard's Connections row instead. `WorkspaceCheckpointsStale` assumes the
default `TF_SNAPSHOT_INTERVAL_SEC`; with a longer interval it fires on healthy
runs.

`JiraDeploymentAppQuotaLow` is the only alert on rate-limit readings. On
Atlassian's default tier, every org that connects Jira through the deployment's
app shares one hourly quota, and once it runs out, people's Jira actions from
the board (claim, done, requeue) wait and then fail in all of those orgs at
once. Polling and delegated runs use each org's own service credential and are
not affected. Only you can fix it: ask Atlassian to move the app to a higher
tier, or have the orgs that use it most add an Atlassian OAuth app of their own
or connect with API tokens, after which their people connect Jira again. A near
reading counts until a later Jira response for the same org replaces it, for at
most an hour, so once people stop using Jira the alert can keep firing for up
to an hour. On Atlassian's per-tenant tier each org has its own quota, and the
alert fires for any one org's.

### Delivering alerts

The bundled stack runs **no Alertmanager**, so a firing alert goes nowhere.
Read it as the `ALERTS` series in Grafana, or with
`docker compose exec prometheus wget -qO- http://localhost:9090/api/v1/alerts`.
To deliver alerts, add an `alerting:` block to
`docker/observability/prometheus.yml` or load the rules into your own
Prometheus. Add a rule as another file in the rules directory, then
`docker compose restart prometheus`.

## Traces

Tracing is off until `TF_TRACES_ENDPOINT` names an OTLP/HTTP endpoint (Tempo,
Jaeger, an OpenTelemetry Collector, a vendor). The compose stack sets it to the
bundled Tempo; `off` disables it. A value with no scheme is plain HTTP
(`tempo:4318`); write `https://` for TLS. The path defaults to `/v1/traces`.
`OTEL_EXPORTER_OTLP_*` variables still set headers, timeout, compression and
TLS, but the endpoint variables don't turn tracing on (TF logs
`an OTLP endpoint is configured but TF_TRACES_ENDPOINT is not`).

Spans are pushed, so the backend must be reachable from each pod, and an
unreachable one loses spans silently; look for `opentelemetry sdk error` in the
logs. Spans carry opaque ids, not repo names or message text, but are
unauthenticated without TLS, so keep the backend internal. Log lines written
inside a span carry `trace_id` and `span_id`, and `/metrics` serves exemplars
to a scraper that negotiates OpenMetrics.

### The bundled trace stack

| Service      | Role                                                                  | Published        |
| ------------ | --------------------------------------------------------------------- | ---------------- |
| `tempo`      | Trace backend (OTLP/HTTP `:4318`, query `:3200`)                      | nothing          |
| `prometheus` | Scrapes every pod, stores Tempo's span metrics, evaluates alert rules | nothing          |
| `grafana`    | The UI                                                                | `127.0.0.1:3030` |

Open Grafana at <http://localhost:3030> (`ssh -L 3030:localhost:3030` on a
remote host). It allows anonymous editing with no login, so don't expose it
without putting your own proxy and auth in front. Prometheus labels each target
`tf_service="control"` or `"executor"`. Config is in
[`docker/observability/`](../../../docker/observability/).

Grafana opens on the **Triage Factory — Overview** dashboard: recent GitHub,
Jira and Linear poll cycles, slow API requests and errored spans; request rate,
errors and p95 latency by span (click an exemplar dot to open its trace);
per-pod runtime; event routing, deferred firings and score re-evaluations; run
setup phases; run completions and in-run agent activity; workspace snapshots;
dropped audit records; and upstream connections and rate-limit waits.
Dashboards are read-only, provisioned from
`docker/observability/dashboards/`. Use **Export → Save as copy** to
experiment, or edit the JSON to change one.

Tempo and Prometheus keep 7 days. `tempo-data` stores each span twice, so size
it for about double the raw traces. The volumes are caches; losing them loses
no TF state.

To use your own pieces: for Prometheus, scrape each pod's `:9464`, load
`docker/observability/rules/`, and repoint `grafana-datasources.yaml` and
Tempo's `remote_write`; for Grafana, copy the data sources, correlations and
dashboards; for a trace backend, delete `tempo` and set `TF_TRACES_ENDPOINT`.
To drop tracing, delete `tempo` **and** set `TF_TRACES_ENDPOINT=off`, or TF
logs an export error every batch.

**Checking the pipeline.** Push a span by hand, then search Explore → Tempo for
`{ .conversation.id = "smoke-1" }` after about 10 seconds:

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

If it appears and TF's spans don't, check TF's logs for `tracing enabled` and
`opentelemetry sdk error`. If it doesn't, check Tempo
(`tempo_distributor_spans_received_total`). Traces Drilldown is downloaded by
Grafana at startup, so it is missing without outbound access.

For a local-mode binary, see [Environment tuning → Tracing](../local/tuning.md#tracing).

### Following a run or an event

Search by `{ .conversation.id = "…" }`, `event.id` or `task.id`; in Grafana a
span with one of those offers **Traces for this conversation** in its links
menu.

- **An event.** A poll cycle's trace ends when it emits events. Each event's
  routing is its own `route.event` trace, linked back to the poll. A trigger
  that delegates carries the run's `conversation.id` on `route.delegate`.
- **A run's setup.** Each attempt is one trace rooted at `engagement.setup`,
  from claim to agent start, numbered by `claim.attempt`. Its `outcome` is
  `agent_live`, `cancelled`, `shutdown`, `fenced`, `loss_budget_spent`, or
  `setup_failed` (the only error).
- **After the agent starts.** Short traces link back to the setup:
  `permission.prompt`, `relay.call`, `git.record_push`, `artifact.record`,
  `workspace.snapshot`, and `engagement.complete` (cost, duration, turns).
- **A slow resume.** `engagement.workspace.ensure` carries
  `workspace.provenance` (`warm`, `rehydrated`, `fresh`) and
  `snapshot.waited_ms`. Waiting the full `TF_SNAPSHOT_WAIT_SEC` and then
  `fresh` means the previous executor never wrote its snapshot.
- **Credentials and signatures** are made on a control pod while the executor
  waits (`credentials.provision`, `commit_signing.decide`). They aren't linked
  to the run's trace; search by `conversation.id`.
