# Sandbox concurrency benchmark

`cmd/sandbox-bench` measures the host-load cost of concurrent gVisor
agent sandboxes and finds the practical (soft) concurrency cap on a
given machine. The hard cap is 256 — the subnet allocator carves
`10.42.0.0/16` into 256 per-run `/24`s (`internal/sandbox/subnet.go`) —
but most hosts run out of memory long before that.

## What one sandbox costs

Each delegated run in multi mode is a gVisor (`runsc`) sandbox inside
the TF container: a netns + veth + iptables egress rules, a chroot into
the cached alpine rootfs, running `node /sdk/wrapper.mjs` which spawns
headless Claude Code. Measured against real headless runs (Claude Code
2.1.x), the workload inside costs roughly:

- **~300MB RSS** for the claude process (grows with context; peaks
  ~370MB with tool children on short tasks, more on long ones)
- **~45–60MB** for the SDK wrapper node process
- **~0.05 cores average** — agents mostly wait on the API, with brief
  tool-call bursts

On top of that comes per-sandbox gVisor overhead (sentry + gofer),
which is one of the things the bench measures directly (run the `idle`
profile).

## Running it

```bash
./scripts/sandbox-bench.sh                          # default: agent profile, ramp to 256
./scripts/sandbox-bench.sh -profile idle -levels 8,32,64,128,256 -hold 30s
./scripts/sandbox-bench.sh -profile agent -rss-mb 400 -levels 16,32,64,96,128
```

The script builds the bench statically, builds the TF runtime image,
and runs the bench in a container with the same caps as the production
service (`SYS_ADMIN`, `NET_ADMIN`) — all netns/iptables/bundle churn
stays inside the container. Results land in `bench-results/*.csv`.

Profiles:

- `agent` (default) — allocates `-rss-mb` (default 400MB), burns
  `-duty-pct` (default 5%) of a core, trickles writes into `/work`.
  The realistic profile; use its result as the soft cap.
- `idle` — sleeps. Isolates fixed per-sandbox overhead.
- `cpu` — busy-loops one core per sandbox. Worst-case CPU pressure;
  expect the load guardrail to trip early on purpose.

## What it measures per plateau

- **spawn p50/p95** — `sandbox.Wrap` time (netns + veth + iptables +
  bundle) for the sandboxes added at this level. Spawn *rate* degrades
  before steady-state count does because the xtables lock serializes
  rule installation; `-burst` controls bring-up parallelism.
- **ready p50/p95** — Wrap start → workload resident (pages touched).
- **canary** — median of 3 full Wrap→echo→Close cycles run *while* N
  sandboxes are live: the marginal cost of "one more run" at this
  concurrency.
- **host state** — MemAvailable, swap used, load1, whole-host CPU%,
  and the summed RSS of every sandbox process tree.

## Guardrails and soft-cap definition

The ramp aborts and tears everything down when host `MemAvailable`
drops below `-mem-floor-mb` (default 6GB) or load1 exceeds `-load-max`
(default 3× cores). Hitting a guardrail is a result, not a failure —
it marks the cap for that profile.

The reported **soft cap** is the last level that completed with no
guardrail abort, no spawn-failure burst, and a canary within 2× the
first plateau's baseline.

Interpreting for capacity planning: per-run memory is
`workload RSS + gVisor overhead` (idle-profile per-sandbox RSS), so a
useful rule of thumb is

```
soft cap ≈ (host RAM − everything else − safety floor) / per-run MB
```

with the bench validating where latency knees actually appear.

## Measured results — 32-core / 62GB Linux host (2026-07)

Kernel 6.17, gVisor 20260511 (systrap), warm rootfs cache.

**Idle profile (fixed per-sandbox overhead), ramp to the 256 hard cap:**

| level | spawn p50/p95 | canary | MemAvail delta | load1 | host CPU |
|------:|--------------|-------:|---------------:|------:|---------:|
| 8     | 217/218 ms   | 343 ms | ~0.2 GB | 0.8 | 1% |
| 64    | 186/260 ms   | 320 ms | 1.4 GB  | 2.0 | 2% |
| 128   | 201/230 ms   | 331 ms | 2.9 GB  | 3.3 | 6% |
| 256   | 213/253 ms   | n/a    | 5.9 GB  | 5.2 | 21% |

All 256 ran concurrently with zero failures; teardown of 256 took
11.8s. Per-sandbox fixed overhead: **~23MB marginal memory** and
**~0.026 cores** of sentry background CPU. Spawn and canary latency
are flat across the whole range — netns/veth/iptables setup does not
degrade with live-sandbox count at this scale.

**Agent profile (400MB + 5% core + /work writes per sandbox):**

| level | spawn p50 | canary | MemAvail delta | per-run marginal |
|------:|----------:|-------:|---------------:|-----------------:|
| 8     | 232 ms | 339 ms | 3.3 GB  | 418 MB |
| 32    | 206 ms | 270 ms | 13.6 GB | 425 MB |
| 96    | 147 ms | 209 ms | 40.9 GB | 426 MB |
| 128   | — aborted: MemAvailable 7.7 GB < 8 GB floor | | | |

**Soft cap on this host: ~96 concurrent agent-shaped runs**, purely
memory-bound (~426MB marginal per run; latency never degraded — the
canary got *faster* under load). The general rule holds:

```
soft cap ≈ (host RAM − base load − safety floor) / 430 MB
```

**CPU profile (one busy-looped core per sandbox, worst case):** host
CPU saturates at 32 sandboxes (100%, load1 17.5) and holds at 64
(load1 35, 2× oversubscribed) with only mild canary degradation
(212ms → 294ms) and zero failures — the scheduler absorbs
oversubscription gracefully. Since a real agent averages ~0.05 cores,
CPU cannot bind before memory does on any sane RAM/core ratio.

**Claude profile (the real vendored engine, fleet-scale, 2026-07):**
each sandbox ran the actual musl `claude` binary over stream-json
against a per-run mock Anthropic endpoint bound on the sandbox gateway
IP (`ConfigureProxies`, mirroring the production LLM proxy), driving a
6-turn Bash tool-use conversation, then idling resident:

| live engines | MemAvail delta (JIT off) | per-run | delta (JIT on) | per-run |
|---:|---:|---:|---:|---:|
| 4   | 0.57 GB | 142 MB | 0.66 GB | 165 MB |
| 16  | 2.4 GB  | 152 MB | 2.7 GB  | 170 MB |
| 32  | 5.0 GB  | 155 MB | 5.5 GB  | 173 MB |
| 64  | 9.9 GB  | **155 MB** | 11.1 GB | 173 MB |

**Marginal cost of a real agent: ~155 MB/run** (JSC JIT off — the
production default) — dead linear 4→64, canary flat ~330 ms, ready
p95 ~1.4 s, zero failures. This confirms the pageshare result with
the real binary: a single engine process measures ~193 MB RSS alone,
but at fleet scale the ~90–110 MB of file-backed binary pages are
shared across sandboxes and amortize to ~0. The JIT costs ~18 MB/run
resident when enabled.

Planning guidance from this: **budget ~256 MB per concurrent run**
(engine marginal + wrapper + transcript headroom; long-context runs
grow further), i.e. `runs ≈ (RAM − 12 GB) ÷ 0.25 GB`, capped at 256 —
a 64 GB host sustains ~200 runs, a 16 GB host ~16.

Accounting note: the CSV's `sandbox_pss_mb`/`sandbox_rss_mb` columns
overstate at this scale — RSS double-counts the static runsc binary's
text pages across every sentry/gofer, and PSS roughly doubles workload
memory through gVisor's memfd stub mappings. `mem_delta_mb` (host
MemAvailable against the pre-ramp baseline) is the authoritative
capacity-planning number; the per-tree columns are for relative
attribution only.

Real delegated runs also carry a node SDK wrapper (~50MB), a blobless
git clone on disk, and context growth in the claude process on long
tasks — budget **~500–600MB per run** for planning, i.e. ~80–90 runs
on a 62GB host, comfortably above any realistic dispatcher setting
and well below the 256 hard cap.

## Credential-sidecar footprint (post-relocation)

Every multi-mode delegated run now also carries a **credential sidecar** — the
capless per-run process that holds the run's sealed bundle and hosts its
LLM/git/API proxies plus the relocated agent-host socket server (the
per-run-credential-isolation epic). The sidecar is a plain Go host process, *not*
a gVisor sandbox, so its footprint reads straight out of `/proc/<pid>/status`
without the sandbox-tree accounting caveats above.

Measured standalone on a 6.17 host (single process, no fleet-scale page sharing):

| state | VmRSS | threads |
|---|---:|---:|
| idle (X25519 key minted, no proxies bound) | ~22.4 MB | 10 |
| proxies bound (LLM + GitHub-API + Jira-API + git) | ~24.5 MB | 10 |

So the four bound proxies add ~2 MB of resident heap on top of the Go-runtime
floor; the agent-host socket server (a single unix listener, measured separately
because its `/run/tf` bind needs root) adds well under a further MB. `VmData` sits
at ~150 MB but is the reserved Go heap arena, **not** resident — the same
RSS-vs-reservation distinction the accounting note above draws for the sandbox
tree.

Two things this changes, and one it doesn't:

- **The spec's ~10 MB estimate predates the full relocation.** It was written when
  the proxies and agent-host ran as goroutines in the shared orchestrator; the
  measured ~24 MB is the cost of standing them (plus the relay runtime) up as a
  standalone Go process per run. The delta is the Go runtime baseline, not the
  proxy logic — attribute it to the relocation, not to new work the sidecar does.
- **Marginal cost at fleet scale is lower than the standalone number.** Every
  sidecar execs the *same* `triagefactory` binary, so its file-backed text pages
  are shared across all live sidecars and amortize toward zero — the same
  mechanism that took the real engine from ~193 MB measured alone to ~155 MB
  marginal at fleet scale (see the Claude profile above). The true marginal per
  run is the private (heap/stack) portion, a few MB. Against the ~500–600 MB/run
  engine-plus-wrapper planning budget, the sidecar is a rounding error: **the
  per-run planning budget is unchanged in practice** (still ~500–600 MB/run).
- **The 256 hard cap is structurally unchanged.** The sidecar adds no new
  ceiling: its per-run uid comes from the reserved `SidecarUID` band, which is
  sized to `MaxSandboxes` (256) — one slot per run, matching the subnet
  allocator's own `[0, 256)` pool exactly. Concurrency is still bound by the
  subnet allocator (and, well before it, host memory), not by the sidecar.

Reproducing the standalone figure needs no runsc host: launch `triagefactory
run-sidecar` with its stdio on a socketpair, drive the sealed-bundle +
start-proxies handshake (`internal/agentproc.BringUpRunSidecar`, `HostVethIP`
`127.0.0.1`), and read `VmRSS` from `/proc/<pid>/status`. The authoritative
*fleet-scale* marginal — after page sharing, alongside live sandboxes — rides the
`sandbox-bench` harness on a runsc host, where the sidecar process joins each
run's tree.

## Relationship to the dispatcher cap

The run dispatcher semaphore (`internal/delegate/process_registry.go`,
`DefaultMaxConcurrentRuns = 4`) is the *operational* concurrency
limit; nothing currently wires a config knob to raise it. The number
this bench produces is the evidence for what that knob may safely be
set to on a given host class.
