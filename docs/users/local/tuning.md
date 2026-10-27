# Tuning (local mode)

Environment variables for run concurrency, disk use, logging, tracing, and the
agent runtime. Except for `TF_CLAUDE_BINARY`, each also applies in a
self-hosted deployment.

## Run concurrency

| Variable                   | Default | Effect                                                                                                       |
| -------------------------- | ------- | ------------------------------------------------------------------------------------------------------------ |
| `TF_MAX_CONCURRENT_CLAIMS` | `8`     | Most agent runs at once. Each costs about 256 MB of RAM plus API spend. Values above 256 are clamped to 256. |
| `TF_DISPATCH_MEM_FLOOR_MB` | `4096`  | No new run starts while available memory is below this. Ignored where memory can't be read (macOS).          |

Runs past the cap wait in a queue for slots to free up. Their card and run
page show **QUEUED** with the time spent waiting. The startup log prints
the cap (`run concurrency cap`), and a full queue logs
`run concurrency cap reached; queued runs start as slots free`.

## Parked workspace disk

A stopped run keeps its worktree on disk so resuming it is instant. Nothing
removes those worktrees while the process runs; a restart clears the ones no
parked run still needs. Two variables, both off unless set, reclaim them
sooner:

| Variable                       | Effect                                                                                                                   |
| ------------------------------ | ------------------------------------------------------------------------------------------------------------------------ |
| `TF_WORKSPACE_EVICT_AFTER_SEC` | Hourly, delete parked worktrees idle longer than this many seconds (for example `21600` for 6h).                         |
| `TF_DISPATCH_DISK_FLOOR_MB`    | While free disk is below this many MiB, start no new runs and delete parked worktrees without waiting for the idle time. |

A worktree is only deleted once its snapshot is saved and no live run is using
it. Resuming afterwards rebuilds the workspace from the snapshot: slower, but
nothing is lost.

## Logging

Logs go to stderr.

| Variable        | Values                                                                      |
| --------------- | --------------------------------------------------------------------------- |
| `TF_LOG_LEVEL`  | `debug`, `info` (default), `warn`, `error`                                  |
| `TF_LOG_FORMAT` | `text` (default in local mode), `json` (default in self-hosted deployments) |

Every line carries a `component` field naming the subsystem.

## Tracing

Set `TF_TRACES_ENDPOINT` to an OTLP/HTTP endpoint to export traces. Unset, no
traces are produced. If correctly configured, at startup Triage Factory will
log `tracing enabled` with the address.

> I don't know why anyone would want to do this when using it day-to-day, but
> I use it for debugging and figured it should be documented.

The quickest backend is a single Tempo container, run from the repository
root:

```bash
docker run -d --name tf-tempo \
  -p 127.0.0.1:4318:4318 -p 127.0.0.1:3200:3200 \
  -v "$PWD/docker/observability/tempo-standalone.yaml:/etc/tempo/tempo.yaml:ro" \
  grafana/tempo:2.10.7 -config.file=/etc/tempo/tempo.yaml

TF_TRACES_ENDPOINT=http://localhost:4318 ./triagefactory
```

Port 4318 receives traces and 3200 answers queries. To list recent traces:

```bash
curl -sS -G http://localhost:3200/api/search --data-urlencode 'q={}' | jq '.traces[].rootTraceName'
```

For a UI, add Grafana with the repository's provisioning file:

```bash
docker network create tf-traces
docker network connect --alias tempo tf-traces tf-tempo

docker run -d --name tf-grafana --network tf-traces -p 127.0.0.1:3030:3000 \
  -e GF_AUTH_ANONYMOUS_ENABLED=true -e GF_AUTH_ANONYMOUS_ORG_ROLE=Editor \
  -e GF_AUTH_DISABLE_LOGIN_FORM=true \
  -v "$PWD/docker/observability/grafana-datasources.yaml:/etc/grafana/provisioning/datasources/triage-factory.yaml:ro" \
  grafana/grafana:13.1.2
```

Open <http://localhost:3030>, then **Explore → Tempo** or **Drilldown →
Traces**. Remove both with
`docker rm -f tf-tempo tf-grafana && docker network rm tf-traces`.

A self-hosted deployment ships the same pieces already wired; see
[Monitoring](../self-hosting/monitoring.md).

## Claude binary

Agents run the Claude binary bundled with Triage Factory. To use another one
(a pinned or debug build), point `TF_CLAUDE_BINARY` at it:

```bash
export TF_CLAUDE_BINARY=/path/to/claude
```

A path that doesn't exist or isn't executable fails the run with an error.
Local mode only.

## Agent engine JIT

Triage Factory turns off the JavaScript JIT in the agent engine, which lowers
each agent's memory use by about a fifth. If a compute-heavy workload gets
slower, turn it back on:

```bash
export TF_AGENT_JSC_JIT=1
```

On macOS the JIT is always on and this variable has no effect.
