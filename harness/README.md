# harness

Compiled (Rust) implementations of the agent-harness tools that run inside
Triage Factory's gVisor sandboxes.

## Why

`docs/benchmarks/gvisor-cpu-tax.md`: under gVisor `systrap`, CPU cost is
~linear in guest syscall rate, and ~72% of a real run's syscalls are
JS-runtime churn (GC, event loop) with another ~12% tool fork/exec. A
compiled harness that also services common tools natively removes up to ~83%
of the syscall volume. This workspace is the first piece: the in-sandbox
compiled coding tools.

## What

`tf-harness-tools/` — the seven coding-agent tools from
[pi](https://github.com/earendil-works/pi) (`read`, `bash`, `edit`, `write`,
`grep`, `find`, `ls`), ported from
`pi/packages/coding-agent/src/core/tools/*` to Rust with **behavioral parity:
same JSON tool definitions, same output text (truncation notices, continuation
hints), same error strings, same edge-case semantics (fuzzy edit matching,
CRLF/BOM preservation, tail truncation, gitignore handling).

Two deliberate architectural differences, both syscall-motivated:

- `grep` embeds the ripgrep crates (`grep`, `ignore`, `globset`) instead of
  fork/exec'ing an `rg` binary; `find` reimplements the `fd` invocation pi
  uses on top of the same `ignore` walker. No subprocess, no JSON pipe.
- Everything else that pi does through the Bun/JSC runtime happens here in
  ahead-of-time-compiled code with no GC and no event loop.

Behavioral parity is enforced two ways (see `tf-harness-tools/tests/`):

- Rust ports of pi's own `tools.test.ts` / fuzzy / CRLF suites (74 tests).
- A differential harness (`parity/parity.ts`) that executes pi's actual
  TypeScript tools (via bun, from a local pi checkout) and the Rust CLI
  against identical fixture workspaces and deep-diffs the JSON results —
  81 cases, all passing, including byte-identical unified patches (the
  jsdiff 8.0.4 Myers walk is ported so tie-breaking matches), truncation
  notices, fuzzy-edit fixups, and error strings.

Validated baseline (2026-07-21): pi @ `959cc189`, semantics probed against
ripgrep 14.1.1 and fd 10.2.0 source. If the parity suite is re-run against a
newer pi and diverges, upstream moved — decide deliberately whether to follow
before touching the port.

```bash
# unit + ported pi tests
cd harness && cargo test

# differential parity vs the real pi tools (needs `npm ci` in the pi repo;
# defaults to a sibling ../pi checkout, override with PI_ROOT or --pi)
cargo build && bun parity/parity.ts [--pi /path/to/pi] [case-filter]

# same corpus, driven through the resident tool-host socket, asserted
# byte-identical to direct invocation (no pi checkout needed)
cargo build && bun parity/parity.ts --mode socket [case-filter]

# poke a tool by hand
./target/release/tf-harness-tools grep --cwd ~/code/repo '{"pattern":"func ","glob":"*.go"}'
./target/release/tf-harness-tools --definitions
```

## Resident tool-host (`serve`)

The native multi-mode agent loop runs *outside* the gVisor jail and dispatches
each tool call *in*. `tf-harness-tools serve --connect <path> [--cwd <dir>]` is
the in-jail counterpart: a long-lived process that answers length-prefixed JSON
tool-call frames, one at a time, using the exact same `run_tool` entry point as
the one-shot CLI — so a call over the socket returns byte-identical bytes to a
direct invocation. The loop binds the socket and `serve` dials in (the jail runs
with gVisor's `--host-uds=open`, which permits `connect` but not `bind`); only
the connection direction inverts, and the peer requests while `serve` answers as
usual.

The wire protocol is specified by the rustdoc on the frame types in
`src/serve.rs` (`Request` / `Response` / `ErrorKind`) — the schema comment *is*
the spec. In brief: a 4-byte big-endian length prefix then that many JSON bytes;
request `{id, tool, args}`; response `{id, ok, result}` or `{id, ok:false,
error}`, where `error` is the tool's message string for a tool failure or a
`{kind, message}` object for a protocol failure (`unknown_tool`,
`malformed_request`, `request_too_large`, `response_too_large`). A protocol
error doesn't imply the engagement is over: all kinds are survivable — read the
next frame — except `request_too_large`, after which the server always closes
because the stream can't be resynchronized past a body it refused to read
(`ErrorKind::is_fatal` is the authoritative classification). A clean socket EOF
ends the engagement. Keeping tool
execution in-jail (rather than on the host side of a bind mount) keeps
symlink/path-traversal resolution over hostile worktree content inside the
Sentry; the resident-behind-a-socket shape keeps the capability-holding
cap-broker out of the per-call hot path.

One frame is not a tool call: `{"tool":"_configure","args":{…}}`, which the peer
sends once before its first tool call to set policy for the session (today, only
`bash_mem_budget_mb` — see the divergence below). It is dispatched ahead of the
tool registry, so it never appears in `--definitions` and the model never sees
it; unknown keys are ignored and answered `ok` so a newer peer degrades to
"policy not applied", and a host predating the verb answers the ordinary
non-fatal `unknown_tool`. Session state dies with the connection and is
persisted nowhere.

Launching `serve` as the sandbox's main process (the OCI-spec/mount wiring) and
the Go client that drives it belong to the native-loop ticket; this crate ships
`serve` standalone and fully tested:

- `tests/serve_test.rs` — soak/robustness: rapid sequential calls, oversized
  (truncated) output parity, a client killed mid-request, a server killed
  mid-request, unknown tool, and malformed/oversized frames (the server answers
  a protocol error or closes — it never panics on hostile bytes).
- `parity/parity.ts --mode socket` — the full corpus over the socket, asserted
  byte-identical to direct invocation.
- `tests/bash_budget.rs` — the per-command memory budget: the tree-RSS sampler,
  a real breach, and an under-budget command asserted byte-identical to an
  unwatched one. Linux-only.
- `tests/injail_smoke.rs` — an env-gated smoke that runs `serve` under real
  `runsc` and drives it from the host: tool calls, `/proc/<pid>/statm` and the
  children lists the sampler reads, and one real breach killed inside the jail.
  Skips cleanly where runsc is absent (set `TF_HARNESS_INJAIL_SMOKE=1` and
  `TF_HARNESS_INJAIL_ROOTFS=<dir>` on the runsc host to run it).

## Measured syscall reduction

`strace -f -c` totals on this repo's working tree (release binary vs bun
driving pi's tools; pi's grep/find spawn rg/fd subprocesses, counted via
`-f`). "Warm marginal" is (11 calls − 1 call)/10 within one process — the
number that matters for a long-lived compiled agent loop; "cold" includes
process + runtime startup:

| tool | cold rust | cold pi (bun) | marginal rust | marginal pi | marginal ratio |
|---|---:|---:|---:|---:|---:|
| grep (glob `*.go`) | 613 | 11,837 | 521 | 1,691 | 3.2× |
| find (`**/*.go`) | 1,515 | 14,111 | 1,385 | 4,061 | 2.9× |
| read (CLAUDE.md) | 121 | 10,931 | 10 | 46 | 4.6× |
| ls | 127 | 11,407 | 45 | 300 | 6.7× |

The ~11k-syscall bun baseline is the per-process JS-runtime tax; the
per-call GC/event-loop churn that dominates real runs (bucket A in
`docs/benchmarks/gvisor-cpu-tax.md`) disappears with the runtime itself.

## Known intentional divergences

All model-visible strings match pi. The differences that exist:

- **Image pixel payloads** — the `image` crate encodes PNG/JPEG bytes
  differently than pi's Photon/WASM; dimensions, notes, and mime types
  match.
- **ls sort for non-ASCII names** — pi uses the host locale's ICU
  collation; this port implements the CLDR root primary order for ASCII
  (verified against pi: `_` < `.` < digits < letters) and falls back to
  code points for non-ASCII (pi's own order is host-locale-dependent
  there).
- **Multi-file match ordering** — rg/fd walk in parallel, so pi's ordering
  is nondeterministic; this port walks deterministically. Within a file,
  ordering matches.
- **grep/find engine error text** — regex/glob parse errors come from the
  same upstream crates rg/fd use, but without their stderr framing (no
  `rg:` / `[fd error]:` prefixes).
- **grep `--glob` rooting** — glob filters are matched relative to the
  *search path*, not the process cwd. rg (and therefore pi) roots them at
  the cwd, so anchored globs like `src/*.ts` silently match nothing when
  the searched directory isn't the cwd itself; here they work uniformly.
  Basename-style globs (`*.ts`, `**/*.spec.ts`) behave identically in
  both.
- **fd's global ignore file** (`~/.config/fd/ignore`) is not read; the
  sandbox has none.
- **`bash` per-command memory budget** — pi has no such limit. Behavior parity
  holds for every command under the budget (byte-identical output, truncation
  and timeout semantics), and the budget is *off* unless a `serve` session was
  configured with one, so the one-shot CLI and the differential parity corpus
  never see it. What it adds is an error path pi does not have: a command whose
  process tree exceeds the budget is SIGKILLed and comes back as a tool error
  naming the observed figure and the limit, with whatever it printed first. It
  samples every 300 ms, so a breach can overshoot between ticks — the jail's own
  memory ceiling remains the hard backstop, and the budget's job is attributing
  a breach to the command that caused it instead of to the next innocent
  allocation in the session.
- **TF branding on agent-facing strings** — the bash spill file is
  `/tmp/tf-bash-<hex>.log` where pi writes `pi-bash-…`. The path is
  model-visible in the truncation notice (and read back by the agent), so
  it carries our name. All other model-visible strings are brand-neutral
  in pi and unchanged here.

## Licensing and attribution

This directory is covered by the repository's Triage Factory License
(source-available). Two permissively licensed upstreams are embedded as
ports, which their licenses expressly allow in a proprietary work provided
the notices are retained:

- Tool semantics, descriptions, and much of the logic are ported from
  [pi](https://github.com/earendil-works/pi) — MIT, Copyright (c) 2025
  Mario Zechner. Notice: `tf-harness-tools/LICENSE-pi`.
- `tf-harness-tools/src/jsdiff.rs` is a port of
  [jsdiff](https://github.com/kpdecker/jsdiff) 8.0.4's line diff and patch
  formatting — BSD-3-Clause, Copyright (c) 2009-2015 Kevin Decker. Notice:
  `tf-harness-tools/LICENSE-jsdiff`.

Neither license is copyleft; they impose notice retention only. When
distributing binaries built from this workspace, include both notice files
in the third-party attributions alongside the Go/npm ones.
