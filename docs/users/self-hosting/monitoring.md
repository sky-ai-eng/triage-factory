# Monitoring

Triage Factory exposes health endpoints, Prometheus metrics, and (when you
name a backend) OTLP traces. The compose stack ships Prometheus, Tempo and
Grafana preconfigured for all three.

## Health endpoints

| Endpoint | Served by | Use it for | Fails (503) when |
| --- | --- | --- | --- |
| `GET /api/health` | control, `:3000` | Liveness | Never on a dependency, so a flapping database can't cause a restart loop. |
| `GET /readyz` | control, `:3000` | Load-balancer rotation, uptime checks | The database is unreachable, migrations are not applied, or a poller has made no progress for 90 seconds. |
| `GET /healthz` | executor, `127.0.0.1:3001` inside the container | The executor's HEALTHCHECK | See [Executor `/healthz`](#executor-healthz). |
| `GET /metrics` | every pod, `:9464` | Prometheus scrape | — |

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
  "checks": {"db": "ok", "migrations": "ok", "poller_github": "ok", "poller_jira": "ok", "poller_linear": "ok"},
  "sources": {"github": {"<org_id>": {"last_success_unix": 1783300000, "age_seconds": 42, "interval_seconds": 300}}},
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
webhook health is shown per workspace under **Workspace Settings → GitHub
access**, not here.

### Executor `/healthz`

The executor publishes no ports, so read it through the container:

```sh
docker compose exec executor curl -fsS http://127.0.0.1:3001/healthz | jq .
```

```json
{"dispatcher_alive": true, "last_heartbeat_write_age_sec": 3, "heartbeat_ever_written": true,
 "broker_ok": true, "active_runs": 2, "draining": false, "fenced": false, "shutting_down": false}
```

It returns 503 when the dispatcher has stopped, the last heartbeat is older
than three heartbeat intervals, or the cap-broker is not answering. `draining`
and `fenced` never change the status. `shutting_down` is set on SIGTERM while
the executor hands its runs back, so a 503 with `shutting_down: true` is a
normal graceful shutdown. `TF_HEALTHZ_PORT` changes the port.

## Metrics

Every pod serves Prometheus metrics on `:9464`
(`curl -fsS http://localhost:9464/metrics | grep '^tf_'`). `TF_METRICS_ADDR`
changes the bind (a port or `host:port`), and `off` disables it. Local mode
serves none unless `TF_METRICS_ADDR` is set.

Counters and histograms are per process, so `sum` them across pods. Gauges
marked **brain** come only from the control pod holding the background-brain
lease; don't sum those.

### Runs and claims

A claim is one executor's engagement with one run.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `tf_claims_expired` | — | Brain. Claims past their lease. A brief nonzero is a dead engagement before another executor takes it over. |
| `tf_claims_oldest_expired_age_seconds` | — | Brain. How long the oldest expired claim has waited. Minutes means no dispatcher is running. |
| `tf_claims_oldest_idle_seconds` | — | Brain. Longest a live claim has gone without activity. One long tool call can hold this high. |
| `tf_claims_oldest_checkpoint_age_seconds` | — | Brain. Longest a live run's workspace has gone without a checkpoint. |
| `tf_conversations_deferred` | `org_id` | Brain. Runs waiting to retry because their model provider, or GitHub during setup, was unavailable. |
| `tf_engagements_stalled_total` | `op` | Runs the stall watchdog stopped, by operation in flight (`provider`, `tool`, `clone`, `permission`, …, `idle`). |
| `tf_conversations_handed_back_total` | `outcome`, `org_id` | Runs released mid-flight to be picked up again: `requeued`, `requeued_credentials`, `requeued_shutdown`, `requeued_upstream`, `requeued_jail_lost`. |
| `tf_workspace_checkpoints_total` | `outcome` | `written`, `skipped_unchanged`, `skipped_busy`, `failed`. |
| `tf_claims_suspend_recoveries_total` | `outcome` | Claims retaken after the machine slept: `reacquired`, `taken`, `failed`. |
| `tf_sidecar_frames_oversized_total` | — | Oversized messages refused from a run's credential sidecar. Treat any sample as a compromised sidecar; the `ERROR` line names the run. |

A run idle for 10 minutes, or stuck past an operation's deadline (a tool call
over 30 minutes, a clone over 10), parks as `stalled` until someone sends it a
message. A run whose model provider is down retries for about 4 hours, then
parks as "Paused: provider unavailable". A run whose sandbox dies 3 times in a
row parks as `sandbox_lost`.

```
tf_claims_oldest_expired_age_seconds > 120                              # for 5m: a dead engagement nobody released
sum(rate(tf_engagements_stalled_total{op!="provider"}[15m])) > 0        # a run is parked waiting for a person
sum(max_over_time(tf_conversations_deferred[15m])) > 0                  # for 30m: runs waiting on an unavailable upstream
tf_claims_oldest_checkpoint_age_seconds > 3 * 300                       # for 15m; use 3 × your TF_SNAPSHOT_INTERVAL_SEC
sum(rate(tf_workspace_checkpoints_total{outcome="failed"}[1h])) > 0
increase(tf_sidecar_frames_oversized_total[1h]) > 0
```

### Work queues

The event queue (`event_queue`), queued auto-delegations (`pending_firings`)
and trigger re-evaluation after scoring (`task_rederive_queue`) share one set
of metrics, labelled `work_kind` and `org_id`. Work that fails permanently or
exhausts its retries is **parked** until an org admin redrives or cancels it
under **Parked work** in the org's settings.

| Metric | Meaning |
| --- | --- |
| `tf_work_ready`, `tf_work_leased`, `tf_work_deferred`, `tf_work_parked` | Brain. Items claimable, in progress, waiting for a retry time or a busy task, and waiting for a person. |
| `tf_work_oldest_ready_age_seconds`, `tf_work_oldest_deferred_age_seconds` | Brain. Age of the oldest ready and deferred item. |
| `tf_work_oldest_ready_age_objective_seconds` | The bound the oldest ready age should stay under. |
| `tf_work_{claims,reclaims,completions,requeues,parks,deferrals,cancellations,redrives,supersedes,lease_lost}_total` | Counts per disposition. |

```
tf_work_parked > 0                                                                                        # for 15m: work waiting for a person
tf_work_oldest_ready_age_seconds > on (work_kind) group_left tf_work_oldest_ready_age_objective_seconds    # for 5m: the queue is behind
tf_work_ready > 0 and rate(tf_work_claims_total[5m]) == 0                                                 # for 5m: stopped, not idle
```

### Commit signing

| Metric | Labels | Meaning |
| --- | --- | --- |
| `tf_commit_signatures_total` | `signer` (`org`, `deployment`), `outcome` (`signed`, `refused`, `unanswered`), `reason`, `org_id` | Signature requests. A refusal's `reason`: `committer_mismatch`, `not_a_commit`, `signer_inactive`, `org_minute_budget`, `claim_budget`, `claim_inactive`. |
| `tf_commit_signing_oldest_pending_age_seconds` | — | Brain. How long the oldest request no control pod has picked up has waited. |
| `tf_commit_signing_limit` | `budget` | The configured limits: `TF_MAX_COMMIT_SIGNATURES_PER_ORG_MINUTE` (default 600) and `TF_MAX_COMMIT_SIGNATURES_PER_CLAIM` (default 5000). |
| `tf_commit_signer_checks_total` | `check` (`verify`, `periodic`, `post_push`), `result` | Health checks of a signer: `valid`, `unknown_key`, `no_user`, `unverified_email`, `key_missing`, `other`, or `error` (GitHub could not be read). |
| `tf_commit_signer_broken` | `org_id`, `reason` | Brain. 1 for each broken org signer. |
| `tf_deployment_commit_signer_up` | — | 1 while the [deployment commit signer](deployment-commit-signer.md) is `active`. Reported by every control pod configured with one. |

TF checks each signer's key on its account about every 15 minutes and checks
each pushed commit's verification. A failure marks the signer `broken`, and
its commits go out unsigned until it is fixed. The logs say so:
`commit signer is broken; the org's runs commit unsigned until an admin fixes it`,
or for the deployment signer
`deployment commit signer is broken; managed workspaces commit unsigned until an operator fixes it`.
The deployment signer's periodic check reads GitHub without a credential, so on
a GitHub Enterprise Server in private mode it stays `pending`.

```
tf_commit_signing_oldest_pending_age_seconds > 5                                         # control pods are not signing
increase(tf_commit_signatures_total{outcome="unanswered"}[15m]) > 0
increase(tf_commit_signatures_total{reason=~"claim_budget|claim_inactive|committer_mismatch"}[1h]) > 0
tf_commit_signer_broken == 1                                                             # an org's commits go out unsigned
tf_deployment_commit_signer_up == 0                                                      # every managed workspace commits unsigned; group by job
increase(tf_commit_signer_checks_total{result="error"}[1h]) > 0                          # checks cannot read GitHub
```

### Source connections

Every request TF makes to GitHub, Jira, Linear, Slack, Anthropic or Bedrock is
counted by how it ended. Requests a delegated run makes from inside its
sandbox are not.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `tf_upstream_requests_total` | `upstream`, `outcome`, `org_id` | Attempts. A retried call counts each attempt. |
| `tf_upstream_retries_total` | `upstream`, `outcome`, `org_id` | Retries, by the outcome that caused them. |
| `tf_upstream_up` | `upstream` (`github`, `jira`), `org_id` | Brain. 1 if the org's last poll reached the source, 0 if not. No series while the source is off or unconfigured. |

`outcome` is `ok`, `rate_limited`, `transient` (a 5xx, timeout, transport
failure, or a non-JSON 403 from a proxy such as a VPN gateway), `auth` (the
credential was refused), or `rejected` (any other 4xx).

The logs report a connection once per change: `github connection lost` /
`jira connection lost` (WARN, with `org` and `class`), `<source> connection
restored` (INFO), and `<source> connection no longer checked`. Individual
failures log at DEBUG.

One org failing is usually its own host, VPN or credential, so the bundled
rules page only when it's widespread: `UpstreamFailingFleetWide` (more than
half of the orgs using an upstream are failing, for 10 minutes) and
`UpstreamDownFleetWide` (more than half have `tf_upstream_up == 0`, for 5
minutes). Both need at least two orgs. One org's failures show on the
dashboard's Connections row.

### Rate-limit budgets

TF exports the latest rate-limit reading per `upstream`, `org_id`,
`credential` and `bucket`. Org members can read the same numbers at
`GET /api/orgs/{org_id}/integrations/rate-limits`.

| Metric | Meaning |
| --- | --- |
| `tf_upstream_rate_limit_remaining`, `_limit`, `_remaining_ratio`, `_until_reset_seconds` | What is left, the bucket size, the share left, and time to refill. |
| `tf_upstream_rate_limit_used` | Slack: calls this pod made in the last minute. |
| `tf_upstream_rate_limit_near_limit` | Jira Cloud's hourly `quota`: 1 below 20% left. |
| `tf_upstream_rate_limit_wait_seconds_total` | Time spent waiting on a rate limit. |
| `tf_upstream_rate_limit_refused_total` | Slack and Jira rate-limited responses, by bucket. |
| `tf_upstream_query_cost` | Histogram of GraphQL points per query, by `op`. |

`credential` is `installation:<id>` or `pat` (GitHub), `api_key` or
`app_install` (Linear), `cloud_api_token`, `dc_pat`, `cloud_oauth`,
`cloud_oauth:deployment` or `cloud_oauth:org` (Jira), and
`bot:<workspace id>:<app id>` or `app:<app id>` (Slack). `bucket` is
GitHub's resource (`core`, `graphql`, `search`), Linear's `requests`,
`complexity` or `endpoint:<name>`, Jira's `quota`, `burst:<endpoint>` or Data
Center's `requests`, and the Slack API method.

Use `min`, not `sum`, across pods: each reading is the upstream's own count.
Slack is the exception; `sum` its `used`. Every org on the
[deployment Jira app](deployment-jira-app.md) shares one quota on Atlassian's
default tier, so take `max` over `org_id` for `cloud_oauth:deployment`. The log
reports `rate-limit budget low` (below 20%) and `waiting on a rate limit`.

### Slack ingest

| Metric | Labels | Meaning |
| --- | --- | --- |
| `tf_slack_ingest_events_total` | `app_id`, `outcome` | Every delivery: `accepted`, `channel_moved`, or the reason it was dropped. |
| `tf_slack_retry_deliveries_total` | `app_id`, `transport` | Deliveries Slack marked as retries. Sustained retries come before Slack disables the app's event subscription. |
| `tf_slack_app_rate_limited_total` | `app_id` | Slack is dropping events past the app's budget (30,000 per workspace per hour). Move to Socket Mode or remove the bot from busy channels. |

Alert on `sum(increase(tf_slack_retry_deliveries_total[15m])) > 0`.

### Data integrity

These should stay at zero; a sample is a bug or an incident.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `tf_audit_records_dropped_total` | `stage`, `op` | An agent's write under an org credential landed, but its audit record was lost. The WARN line names the target. |
| `tf_tasks_open_on_closed_entity` | `org_id` | Brain. Open tasks on a merged or done entity. |
| `tf_entity_terminal_active` | `org_id` | Brain. Merged or done entities still active after 15 minutes, usually in an untracked repo or project whose tasks need dismissing. |
| `tf_linear_mirror_failures_total` | `reason`, `org_id` | Delegated Linear issues TF couldn't move to IN PROGRESS ★. `transition_failed` usually means that state was deleted; star another on the team's Linear source page. |

`tf_memory_entity_links_skipped_total{role, source, reason, org_id}` counts
memory links not written, so a later run on that entity won't see this run's
memory. It is not an alert.

```
sum(increase(tf_audit_records_dropped_total[1h])) > 0
tf_tasks_open_on_closed_entity > 0
max_over_time(tf_entity_terminal_active[30m]) > 0
sum by (org_id) (increase(tf_linear_mirror_failures_total{reason="transition_failed"}[1h])) > 0
```

### Delivering alerts

The bundled Prometheus loads every `*.yml` in
[`docker/observability/rules/`](../../../docker/observability/rules/) but runs
**no Alertmanager**, so a firing alert goes nowhere. Read it as the `ALERTS`
series in Grafana, or with
`docker compose exec prometheus wget -qO- http://localhost:9090/api/v1/alerts`.
To deliver alerts, add an `alerting:` block to
`docker/observability/prometheus.yml` or load the rules into your own
Prometheus. Add a rule as another file in that directory, then
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

| Service | Role | Published |
| --- | --- | --- |
| `tempo` | Trace backend (OTLP/HTTP `:4318`, query `:3200`) | nothing |
| `prometheus` | Scrapes every pod, stores Tempo's span metrics, evaluates alert rules | nothing |
| `grafana` | The UI | `127.0.0.1:3030` |

Open Grafana at <http://localhost:3030> (`ssh -L 3030:localhost:3030` on a
remote host). It allows anonymous editing with no login, so don't expose it
without putting your own proxy and auth in front. Prometheus labels each target
`tf_service="control"` or `"executor"`. Config is in
[`docker/observability/`](../../../docker/observability/).

Grafana opens on the **Triage Factory — Overview** dashboard: recent poll
cycles, slow API requests and errored spans; request rate, errors and p95
latency by span (click an exemplar dot to open its trace); per-pod runtime;
event routing; run setup phases; workspace snapshots; dropped audit records;
and upstream connections. Dashboards are read-only, provisioned from
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
