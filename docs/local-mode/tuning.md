# Environment tuning (local mode)

Environment-variable knobs for run concurrency, logging, the Claude binary, and
the agent engine's runtime. The concurrency, logging, and JIT settings apply in
multi mode too; the `TF_CLAUDE_BINARY` override is local-mode only.

## Run concurrency

Delegated runs execute through a process-wide dispatcher that runs at most
`TF_MAX_CONCURRENT_RUNS` agents at once — **default 8, and the cap applies in
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
export TF_MAX_CONCURRENT_RUNS=16   # each concurrent run costs ~256 MB RAM plus API spend
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
(`TF_RUN_MEMORY_LIMIT_MB`) is a multi-mode sandbox control and does not apply
locally.

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
