# Environment tuning (local mode)

Environment-variable knobs for run concurrency, logging, tracing, the Claude
binary, and the agent engine's runtime. The concurrency, logging, tracing, and
JIT settings apply in multi mode too; the `TF_CLAUDE_BINARY` override is
local-mode only.

## Run concurrency

Delegated runs execute through a process-wide dispatcher that runs at most
`TF_MAX_CONCURRENT_CLAIMS` agents at once — **default 8, and the cap applies in
local mode too**, not just multi (concurrency is an API-spend throttle as much
as a memory guard). Delegating more work than the cap is normal: the extra runs
wait in a durable queue — the board card and the run page show **QUEUED** with
the time spent waiting — and each starts automatically as a slot frees. Nothing
is stuck, and queued runs survive a restart (the dispatcher re-claims them on
boot). Queue time is tracked separately from working time: elapsed/duration
readouts measure from when the run actually started executing, and a run that
waited meaningfully shows its dwell as its own `queued` readout on the card
footer and the run page's telemetry rail.

```bash
export TF_MAX_CONCURRENT_CLAIMS=16   # each concurrent run costs ~256 MB RAM plus API spend
```

The effective cap is logged at boot (`run concurrency cap`); when a burst
saturates it the dispatcher logs `run concurrency cap reached; queued runs
start as slots free`, and logs again once slots open up. Values above 256 clamp
to 256 (a sandbox-allocator structural limit shared with multi mode).

One companion guardrail also defers queued runs on a loaded host:
`TF_DISPATCH_MEM_FLOOR_MB` (default 4096) stops the dispatcher from claiming
new runs while the host's available memory is below the floor — runs stay
queued and dispatch resumes when memory recovers. It fails open where memory
isn't reportable (for example macOS). The per-run memory ceiling
(`TF_CLAIM_MEMORY_LIMIT_MB`) is a multi-mode sandbox control and does not apply
locally.

## Parked workspace disk

A stopped run keeps its worktree on disk so resuming it is instant — a resume
reuses the tree rather than rebuilding it from the snapshot TF also wrote. Those
trees are full checkouts, and nothing reclaims them while the process is
running; a restart sweeps the ones no parked run still wants.

If parked runs pile up faster than you restart and the disk matters more than
warm resumes, `TF_WORKSPACE_EVICT_AFTER_SEC` turns on an hourly sweep that
reclaims trees idle longer than the given number of seconds:

```bash
export TF_WORKSPACE_EVICT_AFTER_SEC=21600   # reclaim trees parked >6h ago
```

It is **off unless you set it** (multi mode defaults it to 6h — there the disk
belongs to shared infrastructure rather than to you). A tree is only reclaimed
once its snapshot is verifiably written and no run sharing it is live, and
resuming afterwards rebuilds the workspace from that snapshot — slower than a
warm resume, never lost work.

## Logging

Logs are structured (Go's `log/slog`) and written to stderr. Two environment
variables tune output:

- `TF_LOG_LEVEL` — minimum level: `debug`, `info` (default), `warn`, or `error`.
- `TF_LOG_FORMAT` — `text` (human-readable, the default in local mode) or `json`
  (machine-parseable, the default when `TF_MODE=multi`).

Every line carries a `component` field (e.g. `component=router`) naming the
subsystem — the structured replacement for the old `[router]` prefixes. Verbose
steady-state traces (such as per-poll credential-tier resolution) log at `debug`,
so set `TF_LOG_LEVEL=debug` to surface them.

## Tracing

`TF_TRACES_ENDPOINT` names an OTLP/HTTP backend and is the whole switch — same
variable in both modes, no `runmode` gate. Unset (local mode's default) installs
no tracer provider at all. The compose stack defaults it to its bundled Tempo;
locally you point it at your own.

Spans are **pushed**, so something has to be listening before any of them is
visible. For the local dev loop that something is one container.

### Tempo in one `docker run`

From the repo root (the `-v` paths are relative to it):

```bash
docker run -d --name tf-tempo \
  -p 127.0.0.1:4318:4318 -p 127.0.0.1:3200:3200 \
  -v "$PWD/docker/observability/tempo-standalone.yaml:/etc/tempo/tempo.yaml:ro" \
  grafana/tempo:2.10.7 -config.file=/etc/tempo/tempo.yaml

TF_TRACES_ENDPOINT=http://localhost:4318 ./triagefactory
```

`4318` is OTLP ingest (where TF pushes) and `3200` is Tempo's query API (where
you read); both bind loopback. The config is checked in — local-disk storage, a
day of retention, no cluster, nothing to operate. TF logs `tracing enabled` with
the resolved address at boot; if it doesn't, the variable didn't reach the
process.

Read what arrived straight off the query API — no UI required, which is exactly
what you want when the question is "did the span I just added export, with the
attributes I expect?":

```bash
# every trace Tempo has seen recently
curl -sS -G http://localhost:3200/api/search --data-urlencode 'q={}' | jq '.traces[].rootTraceName'

# one pipeline's traces, by the attribute that identifies it
curl -sS -G http://localhost:3200/api/search --data-urlencode 'q={ .conversation.id = "<id>" }' | jq

# the whole trace, spans and attributes included
curl -sS http://localhost:3200/api/traces/<trace-id> | jq

# an aggregate over spans rather than a search for them
curl -sS -G http://localhost:3200/api/metrics/query_range \
  --data-urlencode 'q={} | count_over_time() by (name)' \
  --data-urlencode "start=$(($(date +%s) - 900))" --data-urlencode "end=$(date +%s)" \
  --data-urlencode 'step=60s' | jq
```

That last one is a **TraceQL metrics** query, and it runs on the checked-in
config's `local-blocks` processor — the one that stores the spans it receives so
a query can group them by an attribute nobody declared in advance. Without it in
`metrics_generator`, those queries and every panel of Grafana's Traces Drilldown
fail with `localblocks processor not found` rather than coming back empty. Two of
Tempo's defaults bound them: a range wider than 3h is rejected outright, and
anything past the last 30 minutes is read from storage rather than the generator.

A trace is searchable a beat after it finishes (Tempo cuts it once it has been
idle ~10s); `/api/traces/<id>` finds it immediately. If nothing shows up at all,
push a span by hand to tell a broken exporter from a quiet TF:

```bash
s=$(date +%s)
curl -sS -X POST http://localhost:4318/v1/traces -H 'content-type: application/json' -d "{
  \"resourceSpans\":[{\"resource\":{\"attributes\":[{\"key\":\"service.name\",\"value\":{\"stringValue\":\"smoke\"}}]},
  \"scopeSpans\":[{\"spans\":[{\"traceId\":\"1234567890abcdef1234567890abcdef\",\"spanId\":\"1234567890abcdef\",
  \"name\":\"smoke.test\",\"kind\":1,
  \"startTimeUnixNano\":\"$((s-1))000000000\",\"endTimeUnixNano\":\"${s}000000000\",
  \"attributes\":[{\"key\":\"conversation.id\",\"value\":{\"stringValue\":\"smoke-1\"}}]}]}]}]}"
```

Second-precision timestamps on purpose: `date +%s%N` exists on neither macOS nor
busybox, and a span accidentally stamped in 1970 is stored happily and then
never appears in any search.

### Adding the trace view

When reading JSON stops being enough — waterfalls, span attributes side by side,
the attribute correlations that jump from a span to every other trace touching
the same conversation — add Grafana next to it. Same provisioning file the
compose stack uses, so the correlations are identical:

```bash
docker network create tf-traces
docker network connect --alias tempo tf-traces tf-tempo

docker run -d --name tf-grafana --network tf-traces -p 127.0.0.1:3030:3000 \
  -e GF_AUTH_ANONYMOUS_ENABLED=true -e GF_AUTH_ANONYMOUS_ORG_ROLE=Editor \
  -e GF_AUTH_DISABLE_LOGIN_FORM=true \
  -v "$PWD/docker/observability/grafana-datasources.yaml:/etc/grafana/provisioning/datasources/triage-factory.yaml:ro" \
  grafana/grafana:13.1.2
```

Then open <http://localhost:3030> → **Explore** → **Tempo**, or **Drilldown →
Traces** for the aggregate view over the same spans — Grafana fetches that app
from `grafana.com` when it first starts, so with no outbound access the section
is absent rather than broken. The file also provisions a
Prometheus data source, which has nothing behind it in this two-container shape;
the only things that notice are the trace view's Service Graph tab and its
trace-to-metrics links — Drilldown reads Tempo directly and works here. No login
form, and both containers publish to `127.0.0.1` only.

Tear the whole thing down with `docker rm -f tf-tempo tf-grafana && docker
network rm tf-traces` — no state outside the containers, and TF's own SQLite
never learns any of this happened.
In multi mode the compose stack ships these same pieces already wired: see
[Monitoring → The bundled trace
stack](../self-hosting/monitoring.md#the-bundled-trace-stack).

## Claude binary

By default the Agent SDK launches the Claude binary bundled with Triage Factory.
To point it at a specific `claude` instead — a locally-built, pinned, or debug
binary — set `TF_CLAUDE_BINARY` to its path:

```bash
export TF_CLAUDE_BINARY=/path/to/claude
```

The path is validated at spawn (it must exist and be executable), so a wrong path
fails the run with a clear error rather than falling back silently. **Local mode
only** — the sandboxed multi-tenant path runs the image-baked binary and ignores
this variable.

## Agent engine JIT

The vendored agent engine is a Bun-compiled binary running on JavaScriptCore. By
default Triage Factory disables its JIT (`BUN_JSC_useJIT=0`), which cuts peak RSS
per agent process by roughly a fifth with no measurable startup cost — agent
workloads are I/O-bound enough that interpreter-only execution is noise. Set
`TF_AGENT_JSC_JIT=1` to restore the JIT if a compute-heavy workload regresses
under the interpreter:

```bash
export TF_AGENT_JSC_JIT=1
```

Applies on both the sandbox and direct/local spawn paths.
