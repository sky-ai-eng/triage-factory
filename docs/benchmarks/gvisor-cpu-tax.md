# gVisor CPU tax — why runs cost ~2.8 cores, not 0.05

Investigation (2026-07-20) prompted by a live deployment showing whole-host
CPU jump from ~1% to ~12% whenever a single agent run was active.

**RESOLVED (2026-08-06).** The lever this investigation recommended shipped
as the native agent loop: the LLM conversation moved host-side into the Go
executor and the jail's resident process became the compiled Rust tool host
— bucket A below (the ~72% JS-runtime syscall churn) left the jail
entirely, not merely compiled in place. Measured on the rebuilt
`sandbox-bench` (same host, same systrap): **0.026–0.061 cores/run** for
agent workloads and **0.48 cores/run cycle-averaged (2.1 peak)** for
CI-shaped build bursts, versus the ~2.8 cores/run measured here — a
~50–70× in-jail reduction, far past the 2.5–5× projected below. CPU no
longer binds concurrency on this host class: the 256 subnet ceiling binds
light fleets and CPU binds CI fleets at ~67 staggered runs / 32 cores,
with fair-sharing degrading gracefully at 2× oversubscription. Current
numbers: `docs/benchmarks/sandbox-bench.md`. The analysis below stands as
the evidence trail.

**Conclusion: one active run costs ~2.8 host cores under gVisor `systrap`, and
the majority is per-syscall trap-and-dispatch overhead, not the agent's compute.
CPU — not memory — is the binding concurrency constraint (~11 runs on a 32-core
host vs a ~90-run memory cap, ~8× lower). The tax scales with syscall rate, and
~72% of a real run's syscalls are JS-runtime churn (JavaScriptCore GC + Bun
event loop — the engine is a Bun binary). gVisor config is exhausted;
`--platform=kvm` cuts it ~3× but needs hardware virt TF can't mandate on
self-host, whose "runs on any Linux" portability is a hard constraint. Per-run
isolation against BOTH the host and neighbor runs is required in every
deployment tier, so hardened containers are insufficient and per-tier
relaxation is ruled out. The moat-preserving lever is a compiled agent harness
(a Go/Rust runtime removes the JS-runtime syscalls — up to ~83% of the total is
addressable), projected at ~2.5–5× CPU reduction, touching nothing in the
deployment model. See "Levers considered" at the end.**

## What the live deployment showed

Local docker multi-mode deployment on a 32-core i9-14900KF / 62 GB host.
Three sequential `Slack assistant` blueprint runs (30–52 turns, sonnet), each
running one-at-a-time (`active_runs = 1` throughout).

Two independent measurements agree on the per-run cost:

- **Executor container cgroup:** 2288 CPU-seconds accumulated over the
  deployment's uptime; measured idle draw is ~0.004 cores (negligible), so
  essentially all of it came from ~818 s of run wall-time → **~2.77 cores per
  active run**, and 97% of all TF-side CPU was in the executor container (vs
  23 CPU-s control, 48 CPU-s postgres).
- **`instance_stats` whole-host samples:** idle ~1% → ~10% of 32 cores during
  each run window → delta **~2.88 cores per run**.

### Metric caveat that started the confusion

`internal/hoststat` samples `/proc/stat`, which inside a container reflects
the **whole host**, not the container (verified: container `/proc/stat` is
byte-identical to the host's). So the dashboard's `cpu_pct` is whole-machine
utilization, and control + executor pods report near-identical values because
they're both reading the same host-wide figure — not because control is doing
the work. "12% CPU" = 12% of 32 cores ≈ 3.8 cores.

## Controlled native-vs-gVisor microbench

To attribute the 2.8 cores, an identical fixed-work node workload (loopback
HTTP-over-UDS streaming + JSON parse + transcript file I/O — an agent-shaped
event-loop/syscall mix) was run three ways in a dedicated privileged container
using the production rootfs and runsc build (`release-20260511.0`). True host
CPU = the container's own cgroup `cpu.stat` delta; guest-perceived CPU =
`process.cpuUsage()`.

**Streaming workload (2000 req × 60 chunks, 3 reps averaged):**

| platform | host CPU | vs native |
|---|---:|---:|
| native (chroot, no sandbox) | 1.68 s | 1.0× |
| gVisor **systrap** (`--network=sandbox`, prod config) | 24.9 s | **14.8×** |
| gVisor systrap (`--network=none`) | 24.7 s | 14.7× |
| gVisor **kvm** (`--network=sandbox`) | 7.5 s | **4.5×** |
| gVisor kvm (`--network=none`) | 7.6 s | 4.5× |

**Fork/exec-heavy workload (400 `/bin/true` spawns — the Bash/git tool-call
pattern, 3 reps averaged):**

| platform | host CPU | vs native |
|---|---:|---:|
| native | 0.53 s | 1.0× |
| **systrap** | 7.6 s | **14.3×** |
| **kvm** | 3.6 s | **6.7×** |

Findings:

- **`systrap` costs ~14× native CPU**, stable across both workload shapes.
  Even the guest's *self-perceived* CPU is inflated ~7× because systrap's
  SIGSYS trap handlers execute in guest context; the sentry adds ~2× more on
  top.
- **The netstack is not the driver** — `--network=sandbox` vs `--network=none`
  is within noise for these workloads (real egress-heavy runs will add some
  netstack cost, but the platform dominates).
- **`kvm` is ~3× cheaper than systrap** on the streaming mix (4.5× vs 14.8×),
  ~2× cheaper on fork/exec (6.7× vs 14.3×). Requires `/dev/kvm`.

Reconciling with production: 2.8 cores/run ÷ ~14× systrap ≈ **0.2 native-core
of real agent work per run** — plausible for a multi-turn run doing real tool
work, and well above the 0.05-core idle-wait lower bound. Heavier code
blueprints (CI-fix, PR-review: more git/bash spawns) will exceed 2.8 cores.

## Capacity implication (32-core / 62 GB host)

| constraint | per-run cost | concurrent runs |
|---|---:|---:|
| memory (the sandbox-bench doc as then written) | ~0.5–0.6 GB | ~90 |
| **CPU, systrap** | ~2.8 cores | **~11** |
| CPU, kvm | ~0.9 cores | ~35 |

Under systrap, CPU saturates at ~11 concurrent runs — **~8× below** the memory
cap the bench of the time treated as the binding constraint. That era's
"CPU cannot bind before memory" conclusion was an artifact of its synthetic
5%-duty-cycle profile; it did not hold for the SDK engine under systrap.

## Why systrap, and the lever

`internal/sandbox/runsc.go` hardcodes `--platform=systrap`. The choice is
deliberate and documented (`docs/security/security-overview.md`: "no KVM /
nested virtualization … gVisor runs on the systrap platform") — it buys
deployment portability (runs on any cloud VM without nested virt) at the cost
of CPU. The in-code rationale ("27% faster than ptrace") compares the two
*software* platforms; KVM was off the table for the original Fly Machines
target, not evaluated against.

**Primary lever: `--platform=kvm` on KVM-capable hosts** (bare metal, or cloud
instances with nested virt / `.metal` shapes). ~3× CPU reduction, one-flag
change, roughly triples CPU-bound concurrency and brings the CPU and memory
ceilings into the same range. Caveat: needs `/dev/kvm` mapped into the
executor container and a host that exposes it; where that's unavailable the
tax is structural to software syscall interception and only a different
isolation technology (e.g. a microVM) or a lower syscall volume would move it.

## Reproducing

Scripts in this investigation's scratchpad (`work.js`, `forkwork.js`,
`sweep.sh`): copy the production rootfs out of a running executor
(`docker cp <executor>:/opt/triagefactory/sandbox/rootfs-* ./rootfs`), launch
a container from the executor image with `--cap-add SYS_ADMIN --cap-add
NET_ADMIN --device /dev/kvm --security-opt seccomp=unconfined`, mount a fresh
writable `cgroup2` for accounting, and run the workload native (chroot) vs
`runsc --platform={systrap,kvm} run`, diffing the container's `cpu.stat`.

## Where the CPU actually goes (sentry profile, 2026-07-20)

A `runsc --profile --profile-cpu` capture of the sentry during a 32s streaming
run, analyzed with `go tool pprof`, decomposes the systrap cost by self-time:

- **~43% is trap / context-switch / schedule machinery** — the cost of
  *intercepting* syscalls, not doing them. Go scheduler churn (`findRunnable`,
  `schedule`, `stealWork`, `goyield_m`, `futex`, `runq*`) is ~23% alone;
  systrap dispatch (`fastPathDispatcher.loop`, `cputicks`, `switchToApp`,
  `waitOnState`, `contextQueue.add`) ~15%; mutex ~5%.
- **~19% is the real host syscalls** the sentry must re-issue
  (`hostsyscall.RawSyscall*`, `Syscall6`) — the irreducible part.
- **remainder (~35%)** is the Go-implemented syscall emulation, arg/buffer
  copying (`memmove`/`duffcopy`), and netstack.

So the single most CPU-intensive part is **the per-syscall trap-and-dispatch
overhead**, which is fixed per syscall and independent of what the syscall
does. Cost scales with **syscall rate**. The fast-path dispatcher *spins*
(cputicks-timed) to cut latency at the cost of CPU; it is internal and
adaptive, with no external tuning knob.

### Guest syscall histogram (strace, 150 requests)

| syscall | count | ~per req | note |
|---|---:|---:|---|
| mmap | 24546 | 164 | **largest source; priciest class to emulate** |
| munmap | 24226 | 162 | paired with mmap — transient large allocations |
| futex | 19054 | 127 | thread sync (V8/libuv) |
| read | 18486 | 123 | streaming I/O |
| epoll_pwait | 18306 | 122 | event loop |
| writev | 18000 | 120 | streaming I/O |
| clock_gettime | 11934 | 80 | timers |

`mmap`/`munmap` churn dominates by volume and is the most expensive syscall
class for gVisor (the sentry maintains its own address-space model). This is
allocator/GC-driven.

### Levers tested and ruled out (portable, no KVM)

- gVisor config: directfs (already on), syscall-patching (already on),
  `--overlay2` (worse), `--network=host` (worse), `--iouring` (test-only),
  `GOMAXPROCS` env (no effect — sentry sizes itself), CPU quota / affinity
  (not applied under `--ignore-cgroups`; guest still sees 32 CPUs).
- **V8 `--max-semi-space-size=128`: no change to mmap/munmap count** for the
  synthetic workload (deterministic strace) — so no CPU win here. The mmap
  source in that proxy is not young-gen scavenge.

### The open lever — measured on the real engine below

The synthetic proxy's syscall mix may not match a real delegated run: it opens
a fresh connection per request (a real run keeps a connection alive to the LLM proxy) and
never fork/execs (a real run spawns Bash/git constantly — gVisor's worst case).
The real-engine decomposition below answered this, and the native loop's
shipped outcome (see the resolution at the top) settled it in production.

## Real-engine syscall decomposition (2026-07-20)

The synthetic node proxy above over-counts connection churn and has no
fork/exec. This is the **real vendored `claude` engine** (the musl
`claude-agent-sdk` binary — a Bun/JavaScriptCore executable, not Node/V8)
run under gVisor+`--strace` against the bench's mock LLM, driving a real
6-turn Bash tool loop, in a self-contained harness (manual netns+veth, mock
on the host veth IP, uid 10000 as production). Trace: 51,388 guest syscalls.

**Three buckets — what each engine-replacement lever can remove:**

| bucket | syscalls | share | removed by |
|---|---:|---:|---|
| **A** runtime churn (GC/JIT/event-loop) | 36,811 | **71.6%** | a compiled Go/Rust harness |
| **B** tool fork/exec + child procs | 5,980 | 11.6% | native in-harness tools |
| **C** irreducible model + file I/O | 8,597 | 16.7% | nothing (the floor) |

Top engine syscalls: `futex` 17,033 · `clock_gettime` 11,104 · `mremap`
6,144 · `madvise` 6,009 · `rt_sigaction` 1,060 · `epoll_pwait` 1,004 ·
`read` 830. **`futex` + `clock_gettime` alone are 55% of ALL syscalls** —
JSC's multi-threaded GC synchronization + Bun's per-tick clock reads. A
Go/Rust runtime generates a tiny fraction of both.

**Implication for a compiled harness.** Up to **83% of syscalls (A+B) are
addressable** by replacing the JS engine with a compiled harness that also
services common tools natively. Since CPU is ~linear in syscall count (the
trap machinery dominates — see the sentry profile above), that maps to a
**~2.5–5× CPU reduction** (14× → ~3–6×; 2.8 → ~0.6–1.1 cores/run; density
~11 → ~25–50 on a 32-core host). This is *larger* than the ~2× the synthetic
proxy projected, because the real engine is more runtime-dominated.

**Honest caveats.** (1) "Removable" ≠ removed: a Go/Rust runtime has its own
`futex`/`clock_gettime`/GC syscalls — bucket A shrinks a lot, not to zero.
(2) Count ≠ CPU exactly: `clock_gettime` (22% of count) may be gVisor
vDSO-served (cheap, no trap), lowering its CPU weight — but `futex` (33%) and
the memory-management calls (`mremap`/`madvise`/`mmap`, ~23%) always trap and
are expensive, so the CPU-weighted removable fraction stays high. (3) Measured
on 6 short Bash tool calls; a heavy code run (git, larger contexts) shifts
bucket B up. (4) The floor is C + the new harness's own runtime + gVisor's
per-syscall tax on the remainder — a compiled harness gets *near* the
non-JS-runtime floor, not to native.

Harness scripts (self-contained, no broker; reusable on any runsc host):
`mock.mjs` (standalone mock LLM), `engine-run.sh` (netns + real engine +
strace), `decompose.sh` (3-bucket classifier). The bench-integration
blocker noted at the time — `sandbox-bench` lacking broker wiring — was
resolved when the bench was rebuilt broker-wired against the native
runtime; a syscall-profile mode was never needed, because the engine whose
syscalls were being profiled left the jail entirely.

## Levers considered, and where each lands

Ranked against the binding constraint (self-host must "run on any Linux" — the
product moat) and the security requirement (per-run isolation against BOTH the
host and neighbor runs, in EVERY tier — a jailbroken agent must not reach the
host or other concurrent runs, independent of tenancy):

| lever | CPU effect | verdict |
|---|---|---|
| gVisor config (directfs, iouring, overlay2, GOMAXPROCS, network mode) | ~0 | **exhausted** — directfs/patching already on; overlay2/host-net worse; iouring test-only; sentry sizes its own threads |
| `--platform=kvm` | ~3× | **self-host: no** — needs `/dev/kvm`; breaks "runs on any Linux". Ship as opt-in auto-detected on bare-metal self-host only |
| microVM per run (Firecracker) | ~native, stronger isolation | **self-host: no** — needs KVM/nested-virt; forces node provisioning, guest-kernel supply chain; excludes locked-down/air-gapped customers (the isolation-hungry segment). Viable only where the platform IS the microVM (Fly SaaS) |
| hardened containers (ns+seccomp+Landlock) | ~native | **no** — insufficient: one kernel LPE via an allowed syscall breaks host AND neighbor isolation at once. Only acceptable with per-tenant hosts, which self-host density can't assume |
| syscall reduction within the JS engine (allocator, buffering) | ~20–40% | modest; unproven on the real engine; keeps the moat intact |
| **compiled agent harness** (replace the Bun/JSC engine with Go/Rust) | **~2.5–5× projected — shipped, measured ~50–70×** | **the moat-preserving lever, taken** — shipped as the native loop (Go loop host-side + Rust tool host in-jail), which removed bucket A from the jail entirely rather than compiling it in place; see the resolution at the top |

Note the current vendored `claude` engine is itself Bun/JavaScriptCore, so
forking another JS harness is CPU-neutral — the density win requires leaving
the JS runtime for a compiled one.
