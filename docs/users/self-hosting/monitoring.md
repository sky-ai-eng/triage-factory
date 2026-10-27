# Monitoring

Triage Factory exposes health endpoints, Prometheus metrics, and (when you
name a backend) OTLP traces. The compose stack ships Prometheus, Tempo and
Grafana preconfigured for all three.

## Health endpoints

| Endpoint | Served by | Use it for | Fails (503) when |
| --- | --- | --- | --- |
| `GET /api/health` | control, `:3000` | Liveness | Never on a dependency. It checks nothing, so a flapping database can't trigger a restart loop. |
| `GET /readyz` | control, `:3000` | Load-balancer rotation, uptime checks | The database is unreachable, migrations are not applied, or a poller has made no progress for 90 seconds. |
| `GET /healthz` | executor, `127.0.0.1:3001` inside the container | The executor's container HEALTHCHECK | See [Executor `/healthz`](#executor-healthz). |
| `GET /metrics` | every pod, `:9464` | Prometheus scrape | — |

All four are unauthenticated and carry only counts and opaque org ids. Keep
`:9464` inside the compose network or cluster; don't publish it.

### `/readyz`

```sh
curl -fsS http://localhost:3000/readyz | jq .
```

```json
{
  "status": "ok",
  "version": "v1.12.0",
  "checks": {"db": "ok", "migrations": "ok", "poller_github": "ok", "poller_jira": "ok", "poller_linear": "ok"},
  "sources": {
    "github": {"<org_id>": {"last_success_unix": 1783300000, "age_seconds": 42, "interval_seconds": 300}}
  },
  "active_runs": 3
}
```

- `checks` are the hard checks behind the 503.
- `sources` reports each org's last successful poll per source. It never
  causes a 503. Alert on `age_seconds > 3 × interval_seconds` (the top-level
  `status` reads `degraded` at that threshold). A cycle that left the org's
  connection down does not count as a success, so `age_seconds` keeps growing
  through an outage.
- `active_runs` is the number of queued and running runs.

With several control pods, a standby hard-checks only `db` and `migrations`,
reports each poller as `"standby"`, and adds a `lease` field
(`name`, `holder_id`, `is_holder`, `term`). See [Scaling out](scaling.md).

GitHub webhook health is not on `/readyz`. Each workspace sees it under
**Workspace Settings → GitHub access**.

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
than three heartbeat intervals, or the cap-broker is not answering
(`broker_ok: false`). `draining` and `fenced` never change the status code.
`shutting_down` does: it is set on SIGTERM while the executor hands its runs
back, so a 503 with `shutting_down: true` and nonzero `active_runs` is a
normal graceful shutdown. `TF_HEALTHZ_PORT` changes the port.

## Metrics

Every pod serves Prometheus metrics on `:9464`:

```sh
curl -fsS http://localhost:9464/metrics | grep '^tf_'
```

`TF_METRICS_ADDR` changes the bind (a port or `host:port`), and `off` disables
it. Local mode serves no metrics unless `TF_METRICS_ADDR` is set.

Counters (`*_total`, histograms) are per process: `sum` them across pods.
Gauges marked "brain" below are reported only by the control pod holding the
background-brain lease, so don't sum those.

### Runs and claims

A claim is one executor's engagement with one run.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `tf_claims_expired` | — | Brain. Claims past their lease. Zero is normal; a brief nonzero is a dead engagement before another executor takes it over. |
| `tf_claims_oldest_expired_age_seconds` | — | Brain. How long the oldest expired claim has gone unreleased. Minutes means no dispatcher in the fleet is running. |
| `tf_claims_oldest_idle_seconds` | — | Brain. Longest time a live claim has gone without activity. A long tool call can hold this high without being stuck. |
| `tf_claims_oldest_checkpoint_age_seconds` | — | Brain. Longest time a live run's workspace has gone without a checkpoint: what a hard kill of its executor would lose. |
| `tf_conversations_deferred` | `org_id` | Brain. Runs waiting to retry because their model provider, or GitHub during workspace setup, was unavailable. Zero is normal. |
| `tf_engagements_stalled_total` | `op` | Runs the stall watchdog stopped, by the operation in flight (`provider`, `tool`, `clone`, `permission`, …, or `idle`). |
| `tf_conversations_handed_back_total` | `outcome`, `org_id` | Runs released mid-flight to be picked up again: `requeued` (setup failed), `requeued_credentials`, `requeued_shutdown`, `requeued_upstream`, `requeued_jail_lost`. |
| `tf_workspace_checkpoints_total` | `outcome` | Checkpoints: `written`, `skipped_unchanged`, `skipped_busy`, `failed`. |
| `tf_claims_suspend_recoveries_total` | `outcome` | Claims an engagement tried to take back after the machine slept past its lease: `reacquired`, `taken`, `failed`. |
| `tf_sidecar_frames_oversized_total` | — | Messages over 16 MiB an executor refused from a run's credential sidecar. Zero is normal; a sample means a sidecar ignored its own limit, so treat it as compromised. The `ERROR` line names the run. |

What happens to a stopped run:

- A run idle for 10 minutes, or stuck in one operation past its deadline (a
  tool call over 30 minutes, a clone over 10, a permission prompt unanswered
  for 150 seconds), parks with reason `stalled`. It waits for someone to
  send it a message, which resumes it.
- A run whose model provider is down, or that can't reach GitHub while
  setting up, retries on a backoff for about 4 hours, then parks as
  "Paused: provider unavailable" (`upstream_unavailable`).
- A run whose sandbox dies 3 times in a row parks as `sandbox_lost`.
- A run whose setup fails 5 times in a row parks.

```
tf_claims_oldest_expired_age_seconds > 120                              # for 5m: a dead engagement nobody released
sum(rate(tf_engagements_stalled_total{op!="provider"}[15m])) > 0        # a run is parked waiting for a person
sum(max_over_time(tf_conversations_deferred[15m])) > 0                  # for 30m, warning: runs waiting on an unavailable upstream
tf_claims_oldest_checkpoint_age_seconds > 3 * 300                       # for 15m; use 3 × your TF_SNAPSHOT_INTERVAL_SEC
sum(rate(tf_workspace_checkpoints_total{outcome="failed"}[1h])) > 0     # checkpoints failing
increase(tf_sidecar_frames_oversized_total[1h]) > 0                     # a sidecar to investigate
```

### Work queues

The router's event queue (`event_queue`), queued auto-delegations
(`pending_firings`) and post-scoring trigger re-evaluation
(`task_rederive_queue`) share one set of metrics, labelled `work_kind` and
`org_id`. Work that fails past its retry budget, or fails permanently, is
**parked**: it will not run until an org admin redrives or cancels it from the
**Parked work** section of the org's settings.

| Metric | Meaning |
| --- | --- |
| `tf_work_ready`, `tf_work_leased`, `tf_work_deferred` | Brain. Items claimable now, being worked on, and waiting for a retry time or a busy task. |
| `tf_work_parked` | Brain. Items waiting for a person. Zero is normal. |
| `tf_work_oldest_ready_age_seconds` | Brain. Age of the oldest claimable item. |
| `tf_work_oldest_ready_age_objective_seconds` | The bound that age should stay under (60s for every kind). |
| `tf_work_oldest_deferred_age_seconds` | Brain. Age of the oldest deferred item. For `pending_firings`, a value far past a run's length points at a run that never finished. |
| `tf_work_{claims,reclaims,completions,deferrals,cancellations,redrives,supersedes}_total` | Item counts per disposition. A reclaim is an item replayed after its holder died. |
| `tf_work_requeues_total` | Failed attempts returned to the queue, by `outcome`. |
| `tf_work_parks_total` | Items parked, by `reason`. |
| `tf_work_lease_lost_total` | Writes refused because a worker had lost its lease, by `op`. |

```
tf_work_parked > 0                                                                                        # for 15m: work waiting for a person
tf_work_oldest_ready_age_seconds > on (work_kind) group_left tf_work_oldest_ready_age_objective_seconds    # for 5m: the queue is behind
tf_work_ready > 0 and rate(tf_work_claims_total[5m]) == 0                                                 # for 5m: stopped, not idle
```

### Commit signing

Signing requests and budgets. Two settings cap them on every control pod (and
a local process); an invalid value refuses the boot:

| Setting | Default | Limit |
| --- | --- | --- |
| `TF_MAX_COMMIT_SIGNATURES_PER_ORG_MINUTE` | 600 | Signatures per org in any 60 seconds. A rebase re-signs every commit it rewrites. |
| `TF_MAX_COMMIT_SIGNATURES_PER_CLAIM` | 5000 | Signatures per run engagement. |

| Metric | Labels | Meaning |
| --- | --- | --- |
| `tf_commit_signatures_total` | `signer` (`org`, `deployment`), `outcome` (`signed`, `refused`, `unanswered`), `reason`, `org_id` | Signature requests. `reason` names a refusal: `committer_mismatch`, `not_a_commit`, `signer_inactive`, `org_minute_budget`, `claim_budget`, `claim_inactive`. |
| `tf_commit_signing_duration_seconds` | `signer`, `mode` | Histogram of request-to-signature time. |
| `tf_commit_signing_requests_pending`, `tf_commit_signing_oldest_pending_age_seconds` | — | Brain. Requests no control pod has picked up, and how long the oldest has waited. Zero is normal. |
| `tf_commit_signing_swept_total` | — | Requests a control pod found late because a notification was dropped. A steady rate adds up to 1.5s per signature. |
| `tf_commit_signing_limit` | `budget` | The two configured limits, for comparison in rules. |
| `tf_commit_signing_pushes_total` | `signed`, `org_id` | Pushes from runs with an active signer, by whether the pushed tip was signed. |

**Signer health.** TF checks each signer about every 15 minutes (is its key
still on the account?) and after each push (did GitHub mark the commit
Verified?). A failure marks the signer `broken`, and commits go out unsigned
until it is fixed.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `tf_commit_signer_checks_total` | `check` (`verify`, `periodic`, `post_push`), `result` | Checks of org and deployment signers. `result` is `valid`, `unknown_key`, `no_user`, `unverified_email`, `key_missing`, `other`, or `error` (GitHub could not be read). |
| `tf_commit_signers` | `status` | Brain. Org signers by `pending`, `active`, `broken`. |
| `tf_commit_signer_broken` | `org_id`, `reason` | Brain. 1 for each broken org signer. |
| `tf_deployment_commit_signer_up` | — | 1 while the [deployment commit signer](deployment-commit-signer.md) is `active`, 0 while `pending` or `broken`. Reported by every control pod configured with one. |

Log lines to search for:

- `commit signer is broken; the org's runs commit unsigned until an admin fixes it` (WARN, with `org`, `check`, `reason`), and `commit signer recovered`.
- `deployment commit signer is broken; managed workspaces commit unsigned until an operator fixes it` (WARN, with `login`, `host`, `reason`, `since`), and `deployment commit signer is active` once it recovers.

The deployment signer's periodic check reads GitHub without a credential. On
a GitHub Enterprise Server in private mode that read is refused, so the
deployment signer stays `pending` there. A post-push failure breaks it for
every managed workspace until you fix the account and run `triagefactory
commit-signer verify` on a control container.

```
tf_commit_signing_oldest_pending_age_seconds > 5                                        # control pods are not signing
increase(tf_commit_signatures_total{outcome="unanswered"}[15m]) > 0                     # signatures timing out
increase(tf_commit_signatures_total{reason=~"claim_budget|claim_inactive|committer_mismatch"}[1h]) > 0
tf_commit_signer_broken == 1                                                            # an org's commits are going out unsigned
tf_deployment_commit_signer_up == 0                                                     # every managed workspace is committing unsigned
increase(tf_commit_signer_checks_total{result="error"}[1h]) > 0                          # checks cannot read GitHub
```

`tf_deployment_commit_signer_up` comes from every control pod, so group by job
for one page per deployment. It reads 0 for a few seconds after a fresh
configuration, until the first check finds the key.

### Source connections

Every request TF makes to GitHub, Jira, Linear, Slack, Anthropic or Bedrock is
counted by how it ended. Requests a delegated run makes from inside its
sandbox (its `gh` calls and exec verbs) are not counted.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `tf_upstream_requests_total` | `upstream`, `outcome`, `org_id` | Attempts. A retried call counts once per attempt. |
| `tf_upstream_retries_total` | `upstream`, `outcome`, `org_id` | Retries, by the outcome that caused them. |
| `tf_upstream_up` | `upstream` (`github`, `jira`), `org_id` | Brain. 1 if the org's last poll cycle reached the source, 0 if it did not. No series while the source is off, unconfigured, or tracks nothing. |

| `outcome` | Meaning |
| --- | --- |
| `ok` | Success. |
| `rate_limited` | The upstream asked TF to wait. |
| `transient` | A 5xx, a timeout, a transport failure, or a non-JSON 403 from a proxy in front of the upstream (such as a VPN gateway in front of a GitHub Enterprise Server). |
| `auth` | The upstream refused the credential or its permissions. |
| `rejected` | Any other 4xx: the request was wrong and a retry won't help. |

A connection change is logged once, not per request:

| Level | Message |
| --- | --- |
| WARN | `github connection lost` / `jira connection lost` (with `org`, `class`) |
| WARN | `<source> connection still down, failure changed` |
| INFO | `<source> connection restored` (with `down_for`) |
| INFO | `<source> connection no longer checked` (the source was turned off or unconfigured) |

Individual failures log at DEBUG (`TF_LOG_LEVEL=debug`). A repository GitHub
answers 404 for logs `discovery: repo unreachable — skipping` at WARN every
cycle.

One org failing is usually its own host, VPN or credential, which an operator
can't fix, so the bundled rules alert only fleet-wide:

- `UpstreamFailingFleetWide` (critical, 10m): for one upstream, more than half
  of the orgs using it see more than half of their attempts end `transient` or
  `auth`, with at least two orgs.
- `UpstreamDownFleetWide` (critical, 5m): for one upstream, more than half of
  the orgs with a recorded state have `tf_upstream_up == 0`, with at least two
  orgs.

Per-org failures show on the dashboard's Connections row and in:

```
sum by (org_id, outcome) (rate(tf_upstream_requests_total{upstream="jira",org_id!="",outcome=~"transient|auth"}[15m]))
```

### Rate-limit budgets

TF exports the latest rate-limit reading for each upstream, org, credential
and bucket. Org members can read the same numbers at
`GET /api/orgs/{org_id}/integrations/rate-limits`.

| Metric | Meaning |
| --- | --- |
| `tf_upstream_rate_limit_remaining`, `_limit`, `_remaining_ratio`, `_until_reset_seconds` | What the latest response said is left, the bucket size, the share left, and time to refill. No series when the response didn't say. |
| `tf_upstream_rate_limit_used` | Slack only: calls this pod made against the bucket in the last minute. |
| `tf_upstream_rate_limit_near_limit` | Jira Cloud's hourly `quota` only: 1 when Jira says less than 20% is left. |
| `tf_upstream_rate_limit_wait_seconds_total` | Time spent waiting on a rate limit before sending a request. |
| `tf_upstream_rate_limit_refused_total` | Slack and Jira: rate-limited responses, by the bucket that refused them. |
| `tf_upstream_query_cost` | Histogram of GraphQL points per query (GitHub `graphql`, Linear `complexity`), by `op`. |

Labels:

| `upstream` | `credential` | `bucket` |
| --- | --- | --- |
| `github` | `installation:<id>`, `pat` | `core`, `graphql`, `search`, … |
| `linear` | `api_key`, `app_install` | `requests`, `complexity`, `endpoint:<name>` |
| `jira` | `cloud_api_token`, `dc_pat`, `cloud_oauth`, `cloud_oauth:deployment`, `cloud_oauth:org` | Cloud: `quota`, `burst:<endpoint>`; Data Center: `requests` (only when an admin turned rate limiting on) |
| `slack` | `bot:<workspace id>:<app id>`, `app:<app id>` | the API method, such as `chat.update` |

Don't sum the gauges across pods: each is the upstream's own count as of that
pod's latest response, so use `min`. Slack is the exception, because each pod
counts only its own calls: `sum` its `used` across pods. On Atlassian's default
tier, every org using the [deployment Jira app](deployment-jira-app.md) shares
one quota, so take `max` over `org_id` for `cloud_oauth:deployment`.

Without metrics, the log carries the same signal: `rate-limit budget low` (WARN,
once per window, below 20% left) and `waiting on a rate limit` (WARN).

```
min by (upstream, org_id, credential, bucket) (tf_upstream_rate_limit_remaining_ratio)
max(tf_upstream_rate_limit_near_limit{upstream="jira", credential="cloud_oauth:deployment"})
```

### Slack ingest

| Metric | Labels | Meaning |
| --- | --- | --- |
| `tf_slack_ingest_events_total` | `app_id`, `outcome` | Every delivery, by `accepted`, `channel_moved`, or the reason it was dropped. |
| `tf_slack_retry_deliveries_total` | `app_id`, `transport` | Deliveries Slack marked as retries. Sustained retries on the Events API come before Slack disables the app's event subscription. Also a WARN line. |
| `tf_slack_app_rate_limited_total` | `app_id` | Slack hit the app's Events API budget (30,000 events per workspace per hour) and is dropping events. Move the app to Socket Mode or remove the bot from busy channels. |

```
sum(increase(tf_slack_retry_deliveries_total[15m])) > 0
```

### Data integrity

Each of these should stay at zero. A sample is a bug or an incident.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `tf_audit_records_dropped_total` | `stage`, `op` | An agent's write under an org credential landed upstream but its audit record was lost. `stage`: `relay_send`, `relay_decode`, `relay_dispatch`, `record`. The matching WARN line names the target. |
| `tf_tasks_open_on_closed_entity` | `org_id` | Brain. Open tasks on a merged or done entity. |
| `tf_entity_terminal_active` | `org_id` | Brain. Merged or done entities still active after 15 minutes. Usually an untracked repo or project whose tasks are waiting to be dismissed. |
| `tf_linear_mirror_failures_total` | `reason`, `org_id` | Delegated Linear issues TF failed to move to IN PROGRESS ★. `transition_failed` usually means the team's starred state was deleted in Linear; star another on the team's Linear source page. |
| `tf_memory_entity_links_skipped_total` | `role`, `source`, `reason`, `org_id` | Memory links not written; a later run on that entity won't see this run's memory. Not an alert. |

```
sum(increase(tf_audit_records_dropped_total[1h])) > 0
tf_tasks_open_on_closed_entity > 0
max_over_time(tf_entity_terminal_active[30m]) > 0
sum by (org_id) (increase(tf_linear_mirror_failures_total{reason="transition_failed"}[1h])) > 0
```

### Delivering alerts

The bundled Prometheus loads every `*.yml` in
[`docker/observability/rules/`](../../../docker/observability/rules/)
(`UpstreamFailingFleetWide`, `UpstreamDownFleetWide`, `ConversationsDeferred`)
but runs **no Alertmanager**, so a firing alert goes nowhere. Read it as the
`ALERTS` series in Grafana's Explore, or:

```sh
docker compose exec prometheus wget -qO- http://localhost:9090/api/v1/alerts
```

To deliver alerts, add an `alerting:` block to
`docker/observability/prometheus.yml`, or load the rules into the Prometheus
you already run. Add your own rules as another file in that directory, then
`docker compose restart prometheus`.

## Traces

Tracing is off until `TF_TRACES_ENDPOINT` names an OTLP/HTTP endpoint (Tempo,
Jaeger, an OpenTelemetry Collector, a vendor). The compose stack sets it to the
bundled Tempo.

- A value with no scheme is plain HTTP (`tempo:4318` → `http://tempo:4318`);
  write `https://` for TLS. The path defaults to `/v1/traces`.
- `TF_TRACES_ENDPOINT=off` disables tracing.
- `OTEL_EXPORTER_OTLP_*` variables still set headers, timeout, compression and
  TLS. The two endpoint variables do not turn tracing on; TF logs
  `an OTLP endpoint is configured but TF_TRACES_ENDPOINT is not` instead.

Spans are pushed, so the backend must be reachable from each pod, and an
unreachable one loses spans without alerting. Look for
`opentelemetry sdk error` in the logs. The cap-broker and the per-run
credential sidecar emit no spans. Spans carry opaque ids, not repo names or
message text, but they are unauthenticated on the wire without TLS, so keep the
backend on the internal network.

Every log line written inside a span carries `trace_id` and `span_id`, and
`/metrics` serves exemplars (trace ids on histogram samples) to a scraper that
negotiates OpenMetrics.

### The bundled trace stack

| Service | Role | Published |
| --- | --- | --- |
| `tempo` | Trace backend (OTLP/HTTP `:4318`, query `:3200`) | nothing |
| `prometheus` | Scrapes every pod's `:9464`, stores Tempo's span metrics, evaluates the alert rules | nothing |
| `grafana` | The UI | `127.0.0.1:3030` |

Open Grafana at <http://localhost:3030> (over `ssh -L 3030:localhost:3030` on a
remote host). It allows anonymous editing and has no login, so don't expose it;
to share it, put it behind your reverse proxy and auth. Prometheus labels each
target with `tf_service` (`control` or `executor`). Config is in
[`docker/observability/`](../../../docker/observability/).

Grafana opens on the **Triage Factory — Overview** dashboard:

- **Traces**: GitHub and Jira poll cycles, system job cycles, API requests
  slower than 500 ms, and errored spans. The last two should be empty.
- **RED**: request rate, error rate and p95 latency by span name. Click an
  exemplar dot to open the trace behind it.
- **Runtime**: per-pod DB pool use, HTTP status codes, goroutines, memory,
  Slack ingest.
- **Event pipeline**: each routed event's queue wait and disposition.
- **Delegated runs**: run setup from claim to agent start, with each phase's
  duration.
- **Workspace snapshots**: duration, size and phase breakdown of each snapshot.
- **Audit pipeline**: `tf_audit_records_dropped_total`; should be flat zero.
- **Connections**: upstream requests and retries by outcome, and orgs
  currently failing.

Dashboards are provisioned read-only from
[`docker/observability/dashboards/`](../../../docker/observability/dashboards/).
To experiment, use **Export → Save as copy**. To change one, edit its JSON; to
add one, drop a `.json` file in the directory, referencing the data sources by
UID (`tf-tempo`, `tf-prometheus`).

**Retention** is 7 days for both Tempo and Prometheus. `tempo-data` stores each
span twice, so size it for about double the raw traces. All three volumes are
caches; losing them loses no TF state.

**Replacing a piece:**

- **Your own Prometheus:** delete the `prometheus` service, scrape each pod's
  `:9464`, load the rules from `docker/observability/rules/`, and update the
  URL in `docker/observability/grafana-datasources.yaml`. Tempo's span-metrics
  `remote_write` in `docker/observability/tempo.yaml` needs a target that
  accepts remote write, or remove that block.
- **Your own Grafana:** delete the `grafana` service and copy the data
  sources, correlations and dashboards from `docker/observability/`.
- **Your own trace backend:** delete `tempo` and point `TF_TRACES_ENDPOINT` at
  your collector.
- **No tracing:** delete `tempo` and set `TF_TRACES_ENDPOINT=off`, or TF logs
  an export error every batch.

**Checking the pipeline.** Push one span by hand:

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

Search for it in Explore → Tempo with `{ .conversation.id = "smoke-1" }` after
about 10 seconds. If it appears and TF's spans don't, check TF's logs for
`tracing enabled` and `opentelemetry sdk error`. If it doesn't appear, check
Tempo; `tempo_distributor_spans_received_total` counts what arrived. Neither
Tempo nor Prometheus publishes a port; reach them with
`docker compose exec prometheus wget -qO- 'http://localhost:9090/api/v1/targets?state=any'`
and `docker compose exec grafana curl -sS http://tempo:3200/status/services`.

Traces Drilldown is installed by Grafana from grafana.com at startup, so it is
missing on a host with no outbound access. TraceQL metrics queries are limited
to a 3-hour range.

For tracing a local-mode binary, see
[Environment tuning → Tracing](../local/tuning.md#tracing).

### Following a run or an event

Search by id: `{ .conversation.id = "…" }`, `event.id` or `task.id`. In
Grafana, a span with one of those attributes offers **Traces for this
conversation** (or event, or task) in its links menu.

- **An event.** A poll cycle's trace ends when it emits events. Each event's
  routing is its own trace (`route.event`), linked back to the poll that
  emitted it. A trigger that delegates carries the new run's
  `conversation.id` on its `route.delegate` span.
- **A run's setup.** Each attempt is one trace rooted at `engagement.setup`,
  from claim to agent start. A run picked up five times has five traces with
  the same `conversation.id`, told apart by `claim.attempt`. The root's
  `outcome` is `agent_live`, `cancelled`, `shutdown`, `fenced`,
  `loss_budget_spent`, or `setup_failed`; only `setup_failed` is marked as an
  error.
- **After the agent starts.** Separate short traces link back to the setup
  trace: `permission.prompt`, `relay.call`, `git.record_push`,
  `artifact.record`, `workspace.snapshot`, and `engagement.complete` (cost,
  duration, turns).
- **A slow resume.** On the setup trace, `engagement.workspace.ensure` carries
  `workspace.provenance` (`warm`, `rehydrated`, `fresh`) and
  `snapshot.waited_ms`. A wait that runs to `TF_SNAPSHOT_WAIT_SEC` and ends
  `fresh` means the executor that owed the snapshot never wrote one.
- **Credentials and commit signatures** are made on a control pod while the
  executor waits: `engagement.credentials.await` and `credentials.provision`,
  `commit_signing.sign` and `commit_signing.decide`. They are not linked;
  search by `conversation.id` to see both sides.

A local-mode run has no sandbox, so its setup trace has no broker, sidecar or
credential spans.
