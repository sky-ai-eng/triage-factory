# Environment tuning (local mode)

Environment-variable knobs for logging, the Claude binary, and the agent
engine's runtime. The logging and JIT settings apply in multi mode too; the
`TF_CLAUDE_BINARY` override is local-mode only.

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
