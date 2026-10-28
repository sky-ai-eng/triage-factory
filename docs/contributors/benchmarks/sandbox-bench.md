# sandbox-bench — capacity, latency, and containment of native sandboxes

`cmd/sandbox-bench` measures what concurrent gVisor sandboxes cost on a host
— memory **and per-run CPU** — and verifies the memory-containment contract,
using the production launch path end to end. Its output is the capacity
model the public sizing page consumes. Run it with
`./scripts/sandbox-bench.sh` (see [Running it](#running-it)).

## What it launches

Exactly what the executor launches for a native engagement. The bench brings
up its own cap-broker (it answers the `cap-broker` subcommand itself, so
`capbroker.Start`'s bare-metal fallback spawns the broker from the bench
binary and installs the IPC client as `internal/sandbox`'s privileged ops),
then per sandbox: seeds a worktree at `RunTreeRoot(rootKey)`, stands up the
per-run network via `SetupRunNetwork`, binds an (unserved) agenthost socket
at the broker's trusted per-run path, and launches the resident
`tf-harness-tools serve` jail through `gvisor.LaunchOnNetwork`, the gVisor
cell's own launch on a network the bench built. Every
launch therefore passes the broker's argv pin, env allowlist, worktree
scope, and mount validation — the bench measures the launch users run, not
a lookalike.

What it deliberately does **not** include: a per-run credential sidecar
(~24 MB RSS + proxy CPU in production, measured separately during the
sidecar-isolation work) and the executor-side Go loop (LLM streaming, DB
writes — host-side, untaxed by gVisor). Jail figures are the gVisor-taxed
part; the platform reserve covers the rest.

## Running it

```bash
./scripts/sandbox-bench.sh                          # native ramp (defaults)
./scripts/sandbox-bench.sh -profile native-idle
./scripts/sandbox-bench.sh -profile native-heavy -levels 8,16,32 -hold 150s
./scripts/sandbox-bench.sh -profile native-heavy -sync-bursts -levels 8,16,32 -hold 150s
./scripts/sandbox-bench.sh -profile oomtest
```

The script builds a static bench binary, builds the runtime image, and runs
the bench in a container with `SYS_ADMIN`/`NET_ADMIN` (rootfs cache kept
warm in a named volume; the first ever run pays the alpine bake once).
Results — a per-level CSV plus a capacity-model JSON — land in
`./bench-results/`. Ramps warm up with one throwaway cycle, hold each
plateau while sampling, run three "one more full run" canaries per level,
and abort on guardrails (`-mem-floor-mb`, `-load-max`) — hitting a guardrail
IS a result.

## Profiles

- **`native`** (default) — the resident tool host driven over the serve
  socket with an agent-shaped tool cycle (bash-heavy on purpose: fork/exec
  is gVisor's most expensive pattern) against a seeded worktree, one call
  per `-tool-interval`.
- **`native-idle`** — resident tool host, no traffic: the pure isolation
  floor (sentry + gofer + netns + tool host).
- **`native-heavy`** — the CI shape: each jail cycles an offline
  build-shaped burst (`node` compute — JSON/regex/hash churn — plus child
  process spawns), a browser-sized memory hold (`-hold-mb`), and idle, with
  `-duty` the active fraction. Staggered offsets by default (a real fleet);
  `-sync-bursts` phase-locks every jail to the same wall-clock grid for the
  worst case. Offline by construction — bench jails have no egress.
- **`oomtest`** — not a ramp: the memory blast-radius scenario (below).

## What it measures

Per plateau, host CPU and **every jail's cgroup** are series-sampled
together across the whole hold (2 s ticks), yielding cycle averages *and*
per-run burst peaks — a single end-of-hold window would catch each jail in
a random phase. The per-jail read is the production
`sandbox.SampleRunCgroup` (`memory.current` + `cpu.stat` `usage_usec` from
the run's own cgroup), so the systrap tax is inside every number; teardown
peaks ride `RecordSandboxActuals`, the same billing-grade read production
stamps on claims. Spawn (network + jail bring-up), ready (spawn → the tool
host answering a real call), and the canary (a full
network+jail+tool+teardown cycle started while the plateau stays live) are
wall-clock.

## Results (2026-08-06)

32-core i9-14900KF / 62 GB, kernel 6.17, gVisor `release-20260511`
(systrap), warm rootfs cache. Every run below: zero tool errors, zero
failed spawns, no guardrail tripped, all levels healthy to the last.

| profile | jail mem/run | mem peak | cores/run | cores peak | levels |
|---|---:|---:|---:|---:|---|
| `native-idle` | 24 MB | — | 0.026 | — | 4→128 |
| `native` @ 3 s cadence | 34 MB | — | 0.038 | — | 4→128 |
| `native` @ 1 s cadence | 36 MB | — | 0.061 | — | 16→48 |
| `native-heavy` staggered | 101 MB | 354 MB | 0.48 | 2.11 | 8→32 |
| `native-heavy` sync (worst case) | 92 MB | 352 MB | 0.26 | 1.03 | 8→32 |

- **Memory is dead linear and cross-validated**: the host-level MemAvailable
  slope (33.6 MB/run, least squares over the native ramp) independently
  agrees with the per-jail cgroup mean (34 MB/run).
- **CPU is linear in tool cadence** above the 0.026-core idle floor
  (~+0.012 cores per 20 calls/min), and linear in fleet size: the staggered
  heavy ramp held 0.47–0.48 cores/run from 8 to 32 jails.
- **Latency is flat under load**: spawn p50 ~171–292 ms and one-more-run
  ~330–490 ms across everything from 4 idle jails to 128 busy ones; the
  canary at 128 live (402 ms) beat the canary at 4 (477 ms). Teardown
  reclaims ~47 ms/sandbox (idle) — jails killed mid-build take longer.
- **Saturation degrades gracefully**: the sync run at 32 drove demand to
  ~67 cores of 32 (host peak 97%). Per-build burst peaks compressed
  2.07 → 1.72 → 1.03 cores across 8/16/32 phase-locked builds — kernel
  fair-sharing — while cycle averages stretched. Zero failures, zero tool
  errors, canary flat (~307 ms). The penalty for oversubscription is
  throughput, never correctness.

### Capacity model

The JSON artifact carries the fitted coefficients and the formula:

```
concurrent_runs = min((host_mem_mb − platform_reserve_mb) / mem_mb_per_run,
                      host_cores / cores_per_run,
                      256)
```

Which limit binds flips with workload. Light/triage fleets hit the
**256-per-host subnet ceiling** long before any resource (CPU would allow
~500+ runs on this host, memory ~1,500). CI-heavy fleets are **CPU-bound**
(~67 staggered runs on 32 cores at the measured cycle average) — and the
CPU line is pacing, not admission: past it, builds fair-share and stretch,
measured healthy to 2× oversubscription. Memory and the ceiling are the
admission limits. Size burst headroom against the peaks (2.1 cores /
354 MB per simultaneously-building run), not the averages.

## oomtest — the memory blast-radius scenario

Victims get a small cgroup ceiling (`-oom-victim-limit-mb`, default 512)
and grow past it in touched 32 MB chunks while neighbor jails keep running
the agent workload; canaries bracket the breach. The contract under test is
**containment**, not a specific death: the ceiling must hold and the
failure must land inside the offending run — a kernel OOM kill of that
jail or of the allocating process inside it, or gVisor surfacing ENOMEM
to that process — with zero neighbor errors and a flat canary. Whether
the ceiling held is judged on each victim cgroup's own `memory.peak`,
which teardown reads: a victim can climb to its ceiling and be killed
between two samples of `memory.current`.

Measured outcome (2026-10-22, kernel 7.0, PASS 2/2): **the kernel holds
the ceiling by killing the allocating process, and the jail survives.**
Each victim's cgroup peaked at exactly its 512 MB ceiling with 1024 MB
attempted, within half a second of the allocation starting. The kernel's
memory cgroup OOM killer then killed one host process in that cgroup: a
systrap stub, the host process that runs the application's address space,
not the sentry or the gofer. The growing `node` died of SIGKILL inside
the jail, and its `bash` call came back as an error carrying what it had
printed, `Command was killed by SIGKILL (signal 9)`, and the native loop's
notice that the sandbox reached its 512 MB limit and the kernel killed a
process to stay under it. The bench wraps each victim's tool host the way
the loop does, prints that text, and records the victim as
`process-killed`. Neighbors took zero errors, and the canary went
342 → 405 ms across the breach.

On kernel 6.17 (2026-08-06) the same scenario ended in allocation failure
instead: the allocation failed inside the victim's own process at a
483 MB high-water, and the tool returned an ordinary error. Either way, a
leaking build costs its own run at worst — never a neighbor, never the
host.

## Caveats

- Jail memory excludes a real repository checkout (the seeded tree is
  small); a run's `/work` is repo-sized.
- The per-run credential sidecar and executor-side loop costs are outside
  the jail cgroup (see "What it launches").
- The heavy profile is build-*shaped* (real node compute and process
  churn, no network); production telemetry (`sandbox_stats`, claim actuals)
  is the source of truth for real customer workload distributions.
- Sum-of-jail figures are cgroup truth per run; whole-host `/proc/stat`
  readings in the CSV include everything else on the box.
