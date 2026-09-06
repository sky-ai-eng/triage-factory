# Horizontal scaling — execution-plane split

Design of record for scaling Triage Factory past one machine: a small
**control plane** (the org-brain singletons) plus N **shared-nothing
executors** (the gVisor sandbox hosts), coordinated entirely through
Postgres. Same binary, same mechanism from N=1 self-host to an N-pod
fleet; no k8s, no service mesh, no pod-to-pod RPC, no shared filesystem.

Status: **accepted design** (this is the "dedicated design session" the
epic called for). Tracked as **TFAC-71**. Builds on the conversation-queue /
live-run / steering line (TFAC-13 → TFAC-305 → TFAC-309), the memory
guardrail (TFAC-552), the worktree cache design (TFAC-60), and the
sandbox-fleet profiles spec (`docs/for-agents/specs/sandbox-fleet/`).

Scope note: multi-mode only. Local mode (Tier 4, one user, SQLite) is
structurally N=1: it always runs `TF_ROLE=all` and every mechanism below
degrades to a no-op there. Local behavior does not change.

---

## 0. When this matters, and how much

A single multi-mode host already goes far. Measured on the benches
(`docs/benchmarks/sandbox-bench.md`, `docs/benchmarks/poll-bench.md`):

- **Sandbox capacity is memory-bound**: ~155 MB marginal per live agent
  engine, ~500–600 MB/run planning budget with wrapper + clone + context
  growth → **~80–90 concurrent runs on a 62 GB host**, hard-capped at 256
  (the `/24`-per-run subnet allocator, `internal/sandbox/subnet.go`).
  Spawn latency is flat (~200 ms p50) all the way up.
- **Polling is cheap**: a large-org GHES shape (300 repos / ~4.6k open
  PRs) costs ~18 s cold and ~10 s warm per cycle at 30 ms RTT, ~120 MB
  RSS. One process can poll many orgs.

So this epic is the **growth + HA** path: outgrow one box's RAM, want
zero-downtime deploys, or want the sandbox fleet to fail independently
of the API. It is not a launch blocker for single-executor multi-mode —
but several of its substrate pieces (ownership-scoped recovery, the
executor registry, the capacity read-out) pay for themselves at N=1 and
ship first.

Fleet capacity once built:

```
fleet concurrent runs ≈ Σ over executors of
    min( TF_MAX_CONCURRENT_CLAIMS,  (RAM_MB − 12288) / 512,  256 )
```

e.g. three 64 GB executors ≈ ~300 concurrent runs, with the control
plane on a 2–4 GB pod.

Three precisions on the formula's terms, now that the substrate
shipped:

- **`RAM_MB` is cgroup-truthful** (TFAC-581): the instance's effective
  memory is the most restrictive `memory.max` in its own cgroup
  ancestry when confined — cross-clamped against `/proc/meminfo` so a
  limit above physical RAM can't overstate capacity — and host MemTotal
  only when genuinely unconfined.
- **`/512` is the conservative planning budget** (wrapper + clone +
  context growth). The shipped boot-log advisory
  (`DerivedRunCapacity`) divides by the 256 MB *marginal-engine*
  budget instead — an upper advisory bound, roughly 2× this planning
  figure. Both cite the same bench; they answer different questions.
  Plan fleets with 512; the dispatch memory floor defends the
  difference at runtime (the derived figure is a log line and an
  over-provision warning, never admission).
- **The 12288 reserve models a co-resident platform stack** (TF +
  Postgres + GoTrue + object store on one box). Under cgroup-truthful
  figures it derives capacity 0 for any dedicated executor pod under
  ~12.5 GB — a per-role reserve default ships with `TF_ROLE`
  (TFAC-582).

---

## 1. What already exists (the substrate is queue-shaped)

The codebase has been deliberately pre-seamed for this split. Inventory
of what this design **reuses rather than builds**:

| Piece | Where | State |
| --- | --- | --- |
| Durable conversation queue, `FOR UPDATE SKIP LOCKED` claim | `internal/db/postgres/conversation_queue.go` (`ClaimNextConversation`), `internal/delegate/dispatch.go` | Built (TFAC-13). Claim is already N-worker-safe; the dispatcher loop is per-process. |
| `Delegate()` = pure DB enqueue | `internal/delegate/delegate.go` → `EnqueueConversation` | Built. Manual + auto delegation already work cross-machine unchanged — no spawner needed at the enqueue site. |
| Durable event queue for router-bound events | `internal/ingest/ingest.go`, `internal/db/postgres/event_queue.go` | Built. Single drain worker; `SKIP LOCKED` claim exists. |
| Executor-ownership stamp (the active claim's `claims.executor_id`/`boot_epoch`) | pg baseline (`claims`), stamped by `stampExecutor` (`process_registry.go`) | Written, consumed by nothing — the intended lease hook. |
| `RunController` indirection for cancel/steer/interrupt/permission | `internal/delegate/process_registry.go` | Built (TFAC-305). In-process impl resolves `s.procs`/`s.cancels`/`s.permPending`; the seam exists precisely so a DB-signaling impl can slot in. |
| Durable workspace snapshots + cross-host rehydrate | `internal/storage` (S3: `<org>/<blueprint_run>/workspace.tar`), `ensureWorkspace`/`workspaceRecoverable` (`resume.go`) | Built. A parked run can resume on a different machine today. |
| Self-contained multi-mode runs | TFAC-545: per-run clone with App installation token; per-run `llmproxy`/`gitproxy`/`egressproxy` on the veth IP; agenthost socket per run | Built. **Credentials and toolchain co-locate with whichever host executes the run** — nothing about a run's execution needs the API box. |
| Local admission control | `internal/hostmem` + `TF_DISPATCH_MEM_FLOOR_MB` (TFAC-552); `TF_MAX_CONCURRENT_CLAIMS` clamped to 256 | Built, per-process — exactly the right shape per-executor. Contract to preserve: **gating never mutates queue state**; a tight host simply stops claiming and work flows to headroom with zero coordination. |
| Replica-safe auth | RS256 JWKS verify (`internal/auth/verify`) + DB-backed sessions (`internal/sessions`) | Built. Any API replica can serve any browser; no sticky sessions. |
| Replica-safe delegation fences | `blueprint_runs_event_trigger_fence` UNIQUE `(triggering_event_id, trigger_id)`; task dedup partial index; `pending_firings` dedup index | Built. Two processes firing the same event/trigger produce exactly one run. |
| Advisory-lock precedents | `auth_provision.go`, `team_github_repos.go`, ee/slack | Built for single-operation correctness (not leadership). |
| Poll cursors + conditional-request state in DB | `poller_state`, repo pulls-poll-state (ETags) | Built. Poll position survives a process swap; a cold handoff re-lists as free 304s. |
| Cost/usage accounting + quotas | `llm_spend` view, the `…/usage` reads under `/api/me`, `/api/orgs/{org_id}` and `/api/teams/{team_id}`, daily + per-team caps, `system_llm_runs` | Built (TFAC-449). The **spend** layer of the dashboard exists; the **infrastructure** layer (this spec §8) does not. |
| Re-seedable per-org disk state | `internal/paths` under `TF_STATE_ROOT`; bounded evictable bare/worktree cache (TFAC-60) | Built per-pod. Durable copies live in Postgres + S3; everything on executor disk is cache. |
| Readiness probe | `GET /readyz` (`internal/server/readyz_handler.go`, TFAC-573) | Built. Hard-fails DB/migrations/poller-alive → 503 for LB rotation; soft-reports poll staleness, GitHub rate budget, active runs. The split makes the poller hard-check **lease-conditional** (§8.3) — as shipped it would 503 every standby control pod. |
| Budgeted, resumable poll cycles | TFAC-571 (GitHub) | Built, with an **in-process** resume point. A cycle cut short by `ErrRateLimited` saves a round-robin repo cursor on the poller (`Manager.ghCursor`, `internal/poller/manager.go`), so the next cycle **on that same process** resumes at the first unrefreshed repo instead of starving the tail of a large tracked set. The cursor is in-memory by TFAC-571's explicit v1 allowance: a leader handoff (or a restart) loses it and the successor starts at the head. What makes that benign is the row above, not this one — the durable per-repo ETag state means a cold re-list is mostly free 304s (§3). |

And the inverse — what at design time **assumed exactly one process**
(the gap list this design closes; ✅ = closed by the shipped P0s,
2026-07-08):

1. ✅ **Boot recovery was global, not ownership-scoped.** Any process
   boot ran `ResetProcessingConversations` and `event_queue.ResetProcessing`,
   flipping **all** in-flight rows back to queued — a second replica
   booting re-queued work the first was still running. Closed by
   TFAC-578 (+ #624's resume-path stamp and the one-time upgrade
   normalization); was the highest-severity hazard.
2. ✅ (mitigated) **The tracker's snapshot diff was single-writer by
   silent assumption.** Closed at the write layer by the `poll_seq`
   CAS + emission suppression (TFAC-579 + #624): a concurrent writer
   now degrades to dropped no-op cycles, not duplicated events or lost
   transitions. Leadership (P1) remains the primary guarantee — the
   CAS is the belt-and-suspenders, and N pollers still waste N× the
   API budget until the lease lands.
3. **Per-entity event ordering is a single-worker property.** Close
   checks and route ordering rely on the event queue's one-worker global
   FIFO; `SKIP LOCKED` across N drainers would break same-entity order.
   (Open — leadership, P1.)
4. **The WS hub, permission broker, process registry, poll schedule,
   one-shot announce toasts, presence, and user-kick are all
   process-local.** A browser on pod A never sees events produced on pod
   B; `CloseUserConnections` (session revoke) only closes local sockets;
   cancel/steer/permission answers 409 unless they land on the owning
   process. (Open — P1, §5.1/§5.2.)
5. ✅ **`pending_firings` drain was only serialized in-memory.** Closed
   by TFAC-579 + #624: claiming pop under `SKIP LOCKED` with
   staleness-based crash recovery, the one-active-per-entity partial
   unique index with a distinct entity-busy outcome, and `'draining'`
   visible to the dedup index and the busy gate.
6. **Every replica would run every background job.** Pollers, scorer,
   classifier, profiler, reconciler, reapers all start unconditionally
   (`app.Run`), so N replicas = N× GitHub/Jira polling, N× Haiku spend,
   and the `syslimit` "global" cap silently becomes 8×N. (Open —
   leadership, P1. The double-*accounting* half is closed: TFAC-579's
   `system_llm_runs` idempotency key means overlap can no longer
   double-count spend.)
7. ✅ **Migrations race**: closed by TFAC-580 — `goose.Up` under a
   session advisory lock, executors refuse behind/ahead schemas at
   `migrate up` time (in-process server assert → TFAC-582).
8. Assorted per-process caches degrade quietly (token caches, reachable-
   repo cache, ip rate limiter ×N) — acceptable, documented below.

---

## 2. Topology

### 2.1 Roles

One binary, one new startup flag alongside `TF_MODE`:

```
TF_ROLE = control | executor        (multi-mode-only input)
  multi mode: REQUIRED — unset, "all", or a typo refuses to boot
  local mode: IGNORED — the single-process shape is gated on TF_MODE
```

- **`all`** — the single-process shape: HTTP + WS + background brain +
  dispatcher. LOCAL-ONLY (TFAC-637 retired it as a multi role): multi
  mode is always the control+executor split, so the credential-isolation
  boundary hangs on the mode, never on a deployment knob. In local mode
  all coordination mechanisms below still run, they just trivially
  self-resolve (the process holds every lease, placement always picks
  itself).
- **`control`** — serves HTTP/API/WS and *competes for leadership* of
  the background brain (§3). Spawns no sandboxes at all: its system
  jobs are toolless direct LLM calls (§7), so it carries no privileged
  container caps.
- **`executor`** — runs the dispatcher, sandboxes, agenthost daemons,
  per-run proxies, and its own disk reapers against its own
  `TF_STATE_ROOT`. Serves no user HTTP (a local health endpoint only).
  Registers and heartbeats in the executor registry (§4).

Deployment floor stays compose: `postgres + gotrue + seaweedfs + 1..M
control + 1..N executor` behind any plain round-robin LB (control pods
only). An executor needs a **privileged-capable container substrate**
(`SYS_ADMIN`+`NET_ADMIN`+unconfined seccomp+cgroup-v2 private
namespace — the same caps the single container needs today; systrap
gVisor, so no KVM/nested-virt requirement) or a plain VM/bare-metal
host with root+runsc. Being precise about where that line falls,
because "no k8s" is *not* it:

- **Impossible:** platforms that forbid privileged pods — PaaS
  (Railway/Render-class) and locked-down managed-k8s modes (GKE
  Autopilot, EKS Fargate). This is about who controls pod security
  policy, not about Kubernetes the technology.
- **Viable:** raw Docker/compose, Fly Machines, VMs — and
  **self-managed k8s** (or managed k8s with self-controlled node
  groups), where an executor is just a privileged pod. Executors are
  in fact the k8s-*friendly* half of the system: outbound-only (no
  Service, no Ingress, no mesh), ephemeral-volume-tolerant (§4.1
  degradation), pull-based (any pod autoscaler composes, §4.5).
  Validate on the integration suite before blessing a given
  cluster/runtime combo (cgroupns behavior varies by containerd
  config) — same validate-early posture as hardened hosts.
- **Non-target** means we don't *build for* k8s (no operator, no
  Helm chart, no manifests shipped) — not that it's forbidden.

Control pods pose no substrate constraint at all: unprivileged (no
sandbox caps, no Node invocation — §7), they run anywhere, Autopilot
included. The privileged-substrate requirement above is
executors-only.

### 2.2 The unit of scale

Unchanged from the epic's durable conclusions: **the executor pod, not
the sandbox.** Each executor keeps the sandbox a tight host-local unit
(own /24 pool → 256 hard cap, own rootfs cache, own worktree cache
shard, own admission control) and the fleet scales by adding pods.
Shared-nothing: two executors never talk to each other, never share a
filesystem, and coordinate only through Postgres rows.

### 2.3 What lives where

| Concern | Home | Why |
| --- | --- | --- |
| HTTP API, WS termination, auth/sessions | every control pod | already replica-safe (JWKS + DB sessions) |
| Pollers (GitHub/Jira), tracker | **leader** control pod | tracker snapshot RMW must be single-writer per (org, source) |
| Event-queue drainer + router (tasks, triggers, close checks) | **leader** | preserves per-entity FIFO ordering; co-located with the poll sentinels |
| Scorer / classifier / profiler / reconciler / marketplace-stats managers | **leader** | driven by in-process `system:poll:*` sentinels; keeping the whole brain on one pod means the sentinels never need to become durable, and `syslimit`'s global cap stays globally true |
| Drain sweeper, snapshot reaper (S3), announce/poll-ready gates | **leader** | singletons today, stay singletons |
| Run dispatcher, sandboxes, agenthost, per-run proxies, worktree/sandbox reapers | every executor | the thing being scaled |
| Delegation enqueue (`Delegate`), run control endpoints | any control pod | enqueue is a DB write; control ops route via signals (§5) |
| Migrations (`goose.Up`) | control pods only, under an advisory lock | executors assert schema version and wait |

### 2.4 Recommended shapes

| Shape | Control | Executors | What you get |
| --- | --- | --- | --- |
| **Local** | `TF_ROLE=all`, one process | in-process | The laptop box (SQLite, no sandbox). Every mechanism self-resolves; nothing to operate. Not a multi shape. |
| **Co-located split** (multi default; TFAC-637) | 1 | 1, same box | The smallest multi deployment: the shipped compose brings up control + one executor together. Per-run credential isolation by construction; the control container carries no sandbox caps. |
| **First fleet** | 1 | N | The main win: sandbox capacity scales independently and executor deploys never touch the API. The lone control pod trivially holds the brain lease. **No load balancer** — one entrypoint (§2.6). A control restart is a brief API blip + brain restart whose poll catch-up is mostly free 304s. |
| **HA** | 2–3 behind the LB | N | Every control pod serves API/WS (replica-safe already) and competes for the one brain lease; exactly one holds it, the rest are **working API replicas + warm standbys, not idle spares**. Leader loss → takeover ≤ ~TTL. Zero-downtime rolls both tiers: control one-at-a-time behind readiness, executors drain-and-replace. Postgres HA remains the floor — control HA cannot exceed the database's. |

Start at the smallest shape that meets the requirement and move down
the table only on evidence; the mechanisms are identical in all three,
so moving is a compose edit, not a migration.

The load-bearing move is the first five "leader" rows of §2.3's Home
column: **the entire org brain moves as one unit** with a single leader
lease. That one decision preserves four invariants at once — tracker
single-writer, event-queue FIFO, bus-local sentinels, process-true
`syslimit` — and makes leadership *coarse* (one lease), which is the
cheapest thing that can possibly work. Sharding the brain per-org
across control pods is a later refinement (§9 P5) that reuses the same
lease table at a finer key; the poll bench says one leader carries
realistic multi-org load for a long time.

### 2.5 Where requests land

Inbound traffic never routes *to* anything specific. The LB picks a
control pod per REST request and per WS connection (plain round-robin,
no stickiness), and the design's job is to make every control pod
equally able to answer everything — including pods that are not the
leader. **The leader is invisible to the request path**: user traffic
never routes to it, waits on it, or knows it exists; the lease governs
background work only.

What a request might need, and how each need is location-independent:

| The request needs… | Served from any pod because… |
| --- | --- |
| authentication | JWKS verify is stateless; sessions are DB rows (§1) |
| state reads/writes | Postgres is the one truth; RLS context is per-tx |
| to create work (delegate, wake a parked run) | it's an enqueue — executors claim it; the receiving pod is irrelevant (§4.2, §5.2) |
| to control a live run (steer/cancel/interrupt/permission) | resolved via the active claim's `claims.executor_id` → `conversation_signals` to the owner; local short-circuit only if this pod happens to own it (§5.2) |
| to poke a brain singleton (PollSoon, re-profile, trigger) | `tf_ctl` trigger relay → whichever pod holds the lease (§5.3) |
| live events pushed to the browser | the pod holding *your* socket fans in everything via `tf_ws`, filtered per `(org, user)` — producers don't matter (§5.1) |
| inbound WS frames (presence) | the socket-owning pod writes `ws_presence` rows — globally visible (§5.1) |
| session revoke to bite everywhere | kick broadcast on `tf_ctl` (§5.1) |

A WS reconnect may land on a *different* pod than before; that is
fine — nothing session-shaped lives in pod memory, and missed live
events during the gap are the §5 lossy-by-contract window over
durable state.

Residual per-pod request state: the in-process **request serializers**
(`projectMutexes` around project-config autosave RMW, `githubAppRegMu`
around GitHub-App registration) serialize only within one pod — two
pods can interleave the same RMW. Fix is the existing advisory-lock
precedent (`pg_advisory_xact_lock` keyed on the project/org, as
`team_github_repos.go:131` already does), folded into the P0
replica-correctness bundle. The pre-auth **ip rate limiter** stays
per-pod (effective limit ≈ ×M, LB-dependent — accepted and
documented), and the TTL caches (reachable-repo, token caches) merely
lose cross-pod hit-rate, never correctness.

### 2.6 The load balancer (operator-provided)

**When one exists at all.** Executors accept no inbound traffic —
they are outbound-only participants (claims, heartbeats, and LISTEN
toward Postgres; GitHub/LLM egress through their per-run proxies), so
there is never anything to balance *toward executors* at any N. The
LB enters only at $M \geq 2$ control pods (the HA row of §2.4). The
all-in-one and first-fleet shapes have exactly today's single
entrypoint: one published port on the one control process.

**We do not write or ship one — any reverse proxy works.** The
properties that usually make LB selection fraught were removed on
purpose: no sticky sessions (DB sessions + the `tf_ws` backplane), no
path-based routing (any pod serves any route, §2.5), no server
affinity, no LB-level auth. What remains is the boring minimum any
stock proxy (nginx, Caddy, HAProxy, Traefik, a cloud ALB, Fly's
proxy) provides:

1. HTTP/1.1 with WebSocket upgrade passthrough.
2. Idle/read timeouts generous enough for long-lived WS connections.
3. Health-aware rotation against `GET /readyz` (already shipped,
   TFAC-573; P1 makes its poller hard-check lease-conditional so
   standbys stay in rotation, §8.3) — this is what makes
   one-at-a-time control deploys zero-downtime; a dropped WS is
   absorbed by the frontend's auto-reconnect.
4. `X-Forwarded-For` passthrough, with `TF_TRUSTED_PROXY_CIDR` /
   `TF_CAPTURE_CLIENT_IP` set so the per-pod rate limiter and
   client-IP capture see real client addresses — **plumbing that
   already exists**, because the single-pod deployment already
   anticipates a fronting proxy today. TLS terminates wherever the
   operator prefers, exactly as now.

What the repo ships is a **reference configuration, not software**: the
$M \geq 2$ compose profile includes an optional stock-proxy service (a
few dozen lines of Caddy/nginx config) as a worked example
(TFAC-582's docs scope). On the managed side, the platform's own
proxy (e.g. Fly's) plays this role with zero additions. Building or
forking a proxy would recreate the §5.0 second-service argument in
its strongest form — a bespoke component in the hot path of every
request, carrying no logic of ours.

---

## 3. Leadership

A `leases` table, not a bare advisory lock — inspectable, feeds the
dashboard, and survives connection churn deliberately:

```sql
leases (
  name          text PRIMARY KEY,      -- 'background-brain' (later: per-org rows)
  holder_id     text NOT NULL,         -- instance id (§4.1)
  term          bigint NOT NULL,       -- fencing token, +1 per acquisition
  acquired_at   timestamptz NOT NULL,
  renewed_at    timestamptz NOT NULL
)
```

- Control pods attempt acquisition on boot and on a watch interval:
  `UPDATE ... SET holder_id=me, term=term+1, acquired_at=now(),
  renewed_at=now() WHERE renewed_at < now() - TTL` (plus the
  insert-if-absent bootstrap). Renewal every ~5 s, TTL ~20 s.
- The holder starts the brain (pollers, drainer, managers, sweepers)
  and **self-demotes on renewal failure, on its own monotonic clock**:
  the brain runs only while `now_mono < last_successful_renewal +
  demote_deadline`, with `demote_deadline` strictly less than the
  takeover TTL — so the old brain is provably stopping before a
  successor can acquire. All *cross-node* comparisons (`renewed_at <
  now() - TTL`) use DB time only; no wall-clock agreement between
  nodes is ever assumed. Long-running brain components bound the
  residual overlap window themselves: the event-queue drainer
  re-verifies the lease term between batches, and the tracker's
  snapshot writes carry the `poll_seq` CAS (§5.4) — so a straggler
  ex-leader mid-cycle degrades to no-ops, not corruption.
- Failover cost is bounded and benign: the new leader's poll schedule
  starts cold (every org due), and it starts cold in the stronger sense
  too — TFAC-571's round-robin repo cursor lives on the poller process,
  so a handoff mid-cycle drops the resume point and the successor
  re-enumerates from the head of the repo list. The ETag state that
  makes that cheap *is* durable, so the catch-up cycle is mostly free
  304s (`docs/benchmarks/poll-bench.md`). Making the cursor itself
  survive a handoff is the remaining work (§9, item 22) and is only a
  fairness improvement: without it, a tracked set large enough to
  exhaust the rate budget every cycle can keep re-refreshing its head
  and starving its tail across a flapping leader.
- **The brain includes EE background workers.** `ExtensionAPI.OnReady`
  fires unconditionally in every process today; under the split those
  workers (connection managers, adapters — e.g. the Slack liveness
  consumer of TFAC-592's run sentinels) are brain components and gate
  on the lease like the core set, unless one is explicitly marked
  replica-safe. Ungated, every control pod runs every EE worker —
  §1's hazard 6 wearing EE clothes, with external writes (Slack
  posts) duplicating ×M.
- **Fencing where it counts, tolerance elsewhere.** Full fencing (every
  brain write carries the term) is overkill; the one genuinely dangerous
  overlap is the tracker snapshot RMW, and it gets its own guard
  (✅ shipped, TFAC-579 + #624): an `entities.poll_seq` CAS (`... SET
  snapshot_json=$1, poll_seq=poll_seq+1 WHERE id=$2 AND poll_seq=$3`),
  **and a lost CAS suppresses that cycle's diffed events too** — the
  no-op has to cover emission, not just the write, or the straggler's
  transitions re-fire under fresh event ids (§5.4). Everything else the
  brain writes is either fence-protected already (delegation),
  last-writer-wins-safe (scores, profiles), or idempotent (task dedup
  index).
- `goose.Up` wraps in `pg_advisory_lock` so M control pods can boot
  concurrently (✅ shipped, TFAC-580; the wait is announced in the log
  so a pod queued behind a peer is legible). Executors don't migrate;
  they compare the schema version and refuse if behind or ahead —
  precision on what shipped: the compare runs at `migrate up` time,
  i.e. in the **container entrypoint**, and "wait if behind" is the
  entrypoint's bounded retry + restart policy, not an in-process poll.
  A multi-mode server started WITHOUT the entrypoint (k8s `command:`
  override, systemd unit) currently boots against an unchecked schema —
  the in-process assert at executor server boot belongs to TFAC-582
  (drain-first deploys remain the rule for schema changes, §5.5).

At `TF_ROLE=all` the single process always wins the lease — zero
behavior change.

---

## 4. The executor fleet

### 4.1 Identity and registry

Instance identity is **stable across restarts**: an id minted once and
persisted under `TF_STATE_ROOT` (so a rebooted executor recognizes *its
own* in-flight rows), plus a `boot_epoch` that increments per boot.
Mechanically:

1. **The id is a file; the file is the identity.** (✅ shipped) First
   boot mints a random id into `<TF_STATE_ROOT>/instance-id`; every
   boot re-reads it under an **exclusive flock held for the process
   lifetime**, so two processes pointed at one state root fail fast
   instead of silently sharing an identity. (Platform caveat: the
   flock is unix-only — on Windows builds the fail-fast guarantee
   doesn't exist; multi-node is a Linux deployment, so this is a
   documented gap, not a fix target.) Content is validated as a UUID:
   a torn write or stray edit is a loud boot error, never a silently
   adopted new identity that orphans the real id's rows. The id
   deliberately identifies the *state root*, not the machine or the
   process (hostnames are recycled in container platforms; PIDs are
   meaningless): ownership of rows is really ownership of the disk
   state — worktrees, caches — those rows reference, so identity must
   travel with the volume.
2. **The epoch is minted by the registry, not the file.** (✅ shipped)
   Boot registration is one statement — `INSERT … ON CONFLICT (id) DO
   UPDATE SET boot_epoch = instances.boot_epoch + 1, … RETURNING
   boot_epoch` — atomic and monotonic, immune to the volume
   snapshot/restore/clone weirdness that corrupts a file-local
   counter. Registration and epoch-mint are the same write; it also
   clears the dead boot's capacity snapshot (no fresh epoch wearing
   stale admission data) while **preserving operator intent** —
   `draining` and `labels` survive restarts, and the heartbeat never
   writes them (a 4s renewal loop that reset `draining=false` would
   un-drain an instance within one tick of the operator draining it).
3. **Claims stamp the pair.** (✅ shipped — the column is
   `claims.boot_epoch` / `event_queue.boot_epoch`, not the
   `claimed_epoch` name earlier drafts used.) Conversation claims and
   event-queue claims record the epoch next to `executor_id`, atomically in the
   claim statement; **resumes re-stamp in the same statement that
   flips the row to 'running'** (a parked run resumed on instance A
   must not spend its rehydrate+spawn window wearing instance B's
   identity — B's next boot would sweep a live resume); requeues and
   resets clear the stamp (a queued row has no owner). The boot
   self-sweep (§4.2) is then a pure predicate — reset `WHERE
   executor_id = me AND boot_epoch < my epoch` — with no ordering
   dependence: rows from the current life can't match by
   construction, so the sweep is safe to run (or re-run) at any time.
4. **The heartbeat doubles as a split-identity fence.** (✅ shipped in
   two halves.) Renewal is `UPDATE … WHERE id = me AND boot_epoch =
   mine`; matching zero rows means another process has re-registered
   this identity (a cloned volume, a duplicated state root across
   hosts — the case the flock can't see). Shipped reaction: a sticky
   fence latch — the dispatcher stops claiming, resumes are refused,
   the heartbeat loop exits, one loud ERROR names the remediation
   (restart to re-register). The remaining half — kill live sandboxes
   and exit, so a zombie's in-flight work can't double external
   writes — is the reaper phase's self-fence (§4.3, TFAC-586's
   scope): sandbox teardown belongs to the machinery that owns it.
5. **Ephemeral state roots degrade to the reaper, never to
   corruption.** A pod with no persistent volume mints a fresh id
   each boot; its prior lives' rows are never self-swept, but the
   leader reaper collects them by heartbeat staleness like any dead
   executor's (§4.3), and a registry GC tombstones rows whose
   heartbeat is 7+ days stale (default; the GC is TFAC-586 scope
   alongside the reaper). Persistent volumes are the recommendation
   for executors regardless (they carry the caches); correctness
   never depends on them.

At `TF_ROLE=all`/local the same mechanism runs against
`~/.triagefactory` and trivially yields one row, epoch bumping per
restart.

The registry is deliberately named for what it holds — **every TF
process in the deployment**, not just executors ("instance" is already
the shipped vocabulary: `claims.executor_id` stamps "a constant instance
id"). "Executor" stays what it is everywhere else: a *role*. The
capacity/admission columns are meaningful only for executor-capable
roles and stay NULL/zero on pure-control rows.

```sql
instances (
  id               text PRIMARY KEY,
  role             text NOT NULL,             -- 'executor' | 'all' | 'control'
  version          text NOT NULL,             -- build version, for skew visibility
  boot_epoch       bigint NOT NULL,
  started_at       timestamptz NOT NULL,
  last_heartbeat_at timestamptz NOT NULL,
  draining         boolean NOT NULL DEFAULT false,
  -- capacity + admission state, executor-capable roles only
  -- (TFAC-552's follow-up lands here)
  max_runs         int,
  active_runs      int,
  mem_total_mb     int,
  mem_available_mb int,
  dispatch_gated   boolean,
  labels           jsonb                      -- future: sandbox-fleet profile classes
)
```

Heartbeat every ~3–5 s (one UPDATE). Membership = rows with a fresh
heartbeat; everything that needs "the live fleet" (placement §6,
reaper §4.3, dashboard §8) reads this table, filtering on `role` where
it wants only claimants. Control pods register too (so the dashboard
sees the whole deployment — versions, lease holder, health), they just
never claim runs.

This registry is deliberately also **the dashboard's data source** —
the scheduler's bookkeeping *is* the fleet view (§8). No parallel
metrics plumbing.

### 4.2 Claiming work

The dispatcher loop is unchanged in shape (mem-gate → semaphore →
claim → execute); three additions to the claim:

1. **Ownership-scoped recovery** (✅ shipped — TFAC-578, fixes hazard
   #1 even at N=1): `ResetProcessingConversations` / `ResetProcessing` are
   "reset rows where `executor_id = me AND boot_epoch < my current
   epoch`" — a booting process only sweeps *its own* orphans, and the
   reset clears the stamp on the way out. The released-upgrade
   boundary needed one extra piece: pre-registry rows carry per-boot
   random uuids (or NULL) that no persistent id ever matches, so a
   one-time SQLite migration replays the old global reset once —
   without it, a run mid-flight at upgrade shutdown stayed `running`
   forever and a mid-processing event was lost permanently. One
   sanctioned exception stays global: `ReconcileOrphanedRuns` (park
   children under terminal blueprints; fail `running` blueprints that
   hold no child at all past a grace) is a cross-instance heal whose
   arms are each guarded by state no live owner can be in — a terminal
   parent, or a mint older than any mint takes. Fleet-wide orphan recovery
   moves to the **leader reaper**: runs whose executor's heartbeat is
   stale past a threshold are requeued (`attempts`-capped, then failed
   with `failure_kind='executor_lost'`).
2. **Cross-process wake**: `EnqueueConversation` also `NOTIFY tf_wake` so idle
   executors claim within milliseconds instead of the poll interval
   (which remains as backstop).
3. **Affinity preference** (§6) and later **per-org fairness** (§9 P3)
   shape *which* queued row a claim takes — never *whether* state
   mutates. The TFAC-552 contract generalizes to the fleet: admission
   is executor-local, queue state is global, and a gated/full/draining
   executor simply doesn't claim.

### 4.3 Run death and recovery

- **Executor restarts**: sweeps only its own rows (above); its parked
  runs stay parked (snapshots are in S3, worktrees preserved where
  warm).
- **Executor dies**: leader reaper requeues its claimed/running rows.
  A re-claimed *mid-flight* run restarts its step from the beginning on
  the new executor (fresh self-contained clone; the crashed attempt's
  session transcript died with the host). `attempts` caps this
  (`TF_MAX_CLAIM_ATTEMPTS`, default 2 — consecutive losses, not
  lifetime claims; decision 4). Residual risk, accepted and
  documented: a run that had already
  performed external writes (pushed a branch, posted a comment) before
  the crash will redo them — artifact upserts and branch-push semantics
  absorb most of it, and `failure_kind='executor_lost'` makes the rest
  auditable. A *parked* run has no owner at all and rehydrates anywhere
  from its S3 snapshot — already true today.
- **A stale heartbeat means *fenced*, not just dead.** The reaper
  cannot distinguish a crashed executor from one partitioned away
  from Postgres, so the protocol makes them equivalent: an executor
  that fails to renew its heartbeat past a self-fence deadline (own
  monotonic clock, strictly shorter than the reaper's staleness
  threshold — same discipline as leader demotion, §3) **kills its
  live sandboxes, stops its sinks, and stops claiming** before the
  reaper may requeue its runs. It forfeits nothing by doing so — a
  partitioned executor can't write `messages` or heartbeats
  anyway — and without it, a requeued run and a zombie sandbox could
  both finish and double their external writes.
- **The DB refuses a slipped zombie's writes.** The self-fence above is
  process-level and clock-based, so the failure modes that void it (a
  stalled process, a clock bug) would otherwise leave nothing between a
  zombie and a conversation it no longer owns. Every executor engagement
  write — transcript rows, the terminal status flip, `claims.phase` —
  therefore names its own claim id and validates it with a locking read
  (`FOR SHARE`) in the same transaction, which conflicts with the
  reaper's release: a concurrent release either waits for the write to
  commit or the write is refused with `db.ErrClaimReleased`. An
  engagement that sees that refusal kills its sandbox and writes
  nothing more — no terminal, no result, no release — because the
  successor owns the conversation's disposition. Postgres only; local
  is single-process and has no such race. A refusal in production means
  the self-fence failed and is logged at error level as an incident
  signal.
- **Graceful drain** (deploys, scale-down): set `draining=true` → the
  executor stops claiming; live runs finish or park at their turn end
  (TFAC-305) — the fleet's natural quiesce; when `active_runs=0` the
  operator retires it. Dashboard badge + one CLI/API verb.

### 4.4 Executor DB access (least privilege)

Background stores currently ride the **admin pool (superuser)**. On a
one-box deployment that's moot; on a fleet, executors are the
most-exposed machines (they run agent workloads), and they should not
hold superuser DSNs. Introduce a `tf_system` role — `BYPASSRLS` (or
explicit system policies), narrow grants (the tables the executor path
actually touches), no DDL — and point executor pods' "admin" pool at
it. Control keeps a migration-capable role. Hardening ticket, not a
blocker for the split itself.

### 4.5 Elasticity (autoscaling-ready, autoscaling-agnostic)

Executor scaling has nothing to do with a load balancer, because
executors receive no traffic — **the queue is the load balancer for
the execution tier.** That makes both directions trivially safe:

- **Scale-out is "boot a pod."** A new executor registers, heartbeats,
  and starts claiming — no routing change, no scheduler registration,
  no rebalance step. Time-to-first-claim ≈ container boot: the rootfs
  ships baked into the image, and repo caches warm lazily per claim.
- **Scale-in is "drain, then retire"** (§4.3) — and even ungraceful
  loss is just the reaper path. Scale-to-*zero* executors merely
  queues work; the control plane and every admission gate hold.
- **Churn is absorbed, not resisted.** Membership changes reshuffle
  only the affected rendezvous keys, and a wrong/stale preference
  costs cache warmth, never correctness (§6). Ephemeral instances
  tombstone out via the registry GC.

What the product deliberately does **not** ship is the control loop
that decides *when* to add or remove machines. Three reasons:

1. **Actuation is 100 % platform-specific.** "Add an executor" means
   a compose command, a Fly Machines API call, an ASG desired-count
   bump, or an enterprise infra team racking a hardened host — the
   §5.0 second-service argument applies to owning N platform
   integrations in the deploy path.
2. **Self-hosters who can autoscale, can — with their own scaler.**
   Anyone on programmable compute already has the loop half: an ASG /
   MIG with a launch template running the executor container, or
   executors as privileged pods on self-managed k8s (§2.1) where
   KEDA can scale the Deployment straight off a queue-depth SQL query
   and the cluster-autoscaler provisions nodes — zero code from us,
   because the contract below is the whole integration surface. The
   one segment that structurally won't autoscale is the
   isolated-network deployment: a fixed, security-reviewed host pool,
   where "autoscaling" is a capacity-planning conversation served by
   the §0 arithmetic and the fleet dashboard. That segment's
   constraint is policy, not software.
3. **Under-capacity degrades gracefully by construction.** Because
   gating never mutates queue state (TFAC-552 → §4.2), a saturated
   fleet means longer queue waits — visible as dashboard percentiles —
   not dropped work. Autoscalers are urgent where under-capacity
   drops requests; here it just queues minutes-long, dollars-costing
   jobs a little longer.

What the product *does* ship is the *contract an autoscaler needs*:
instant safe join, drain-or-die-safe leave, and the decision signal
(queue depth, wait percentiles, headroom, gated flags) queryable from
the registry/stats tables and `/api/fleet`. A SaaS autoscaler is then
a ~hundred-line ops-side loop — read the signal, call the platform's
machines API, respect drain — living in the private ops tooling, not
in this binary. Give it hysteresis: rendezvous cache warmth rewards
calm fleets, and the aging claim already covers the burst in the
meantime.

---

## 5. Coordination fabric (Postgres is the bus)

Three LISTEN/NOTIFY channels plus small outbox tables where payloads
can exceed NOTIFY's ~8 KB limit. Each pod holds 1–2 **dedicated direct
connections** for LISTEN (session-scoped — they must bypass any
transaction-mode pooler; the query pools can move behind
PgBouncer/Supavisor independently, per TFAC-307 §2, with the RLS
`SET LOCAL` pattern validated under transaction pooling).

Today's defaults are $25 \text{ open per pool} \times \text{ two pools} \approx 50 \text{ conns}$ 
per process (`applyPGPoolDefaults`, `internal/app/stores.go`) —
sized for one all-in-one binary. A fleet multiplies that: 
$3 \text{ executors} + 2 \text{ control} \approx 250 \text{ conns}$, past a default 
`max_connections` long before any workload pressure. So pool ceilings
become **per-role** (an executor's dispatcher + sinks need a fraction
 of what the API tier does), env-tunable, and the compose profile 
documents the `max_connections` arithmetic; a transaction-mode pooler 
(TFAC-307 §2) is the lever for large fleets, never a requirement for small
ones.

| Channel | Producer → consumer | Payload |
| --- | --- | --- |
| `tf_wake` | control (enqueue) → executors | `{kind: run\|event, org}` — claim nudges |
| `tf_ws` | anyone → every control pod | WS event envelope `{origin_pod, event}`; over the inline threshold (default 6 KB; NOTIFY caps at 8 KB and agent messages regularly exceed it), a `ws_outbox` row ref instead — 60 s TTL reaper on the outbox |
| `tf_ctl` | control ↔ executors, control ↔ control | `{signal_id}` / `{kick_user}` / `{trigger: subsystem, org}` — see below |
| `tf_bus` | executors → **the brain only** | brain-bound bus-relay envelopes (run sentinels, §5.3); the LISTEN starts/stops with the lease; lossy-by-contract |

One rule governs every channel: **NOTIFY is a doorbell — never the
data, and never the only path.** A notification means "scan your
backing table now": `tf_ctl` consumers scan `conversation_signals` for unacked
rows on every (re)connect and on a slow backstop poll; `tf_wake`
consumers keep their claim-poll interval as the backstop; so a dropped
LISTEN connection *delays* work, it cannot *lose* it. The one
deliberate exception is `tf_ws`: live UI events are lossy by contract
(durable state is in Postgres and the frontend refetches on
navigation), so a missed window degrades liveness, never state.

### 5.0 Why Postgres, and not Redis/NATS

Doorbells, TTL state, and queues are a broker's home turf, so the
choice needs a recorded argument. Four reasons, in decreasing
order of weight:

1. **Postgres is already mandatory; a broker is a second service.**
   Isolated-network self-host is a first-class target, and every
   additional service is operator burden compounded: another image to
   mirror, another auth/TLS surface, another persistence + HA decision
   tree (RDB/AOF? Sentinel? JetStream retention?), another thing to
   monitor and upgrade. The fabric riding the store that must exist
   anyway adds **zero** marginal operational surface. (NATS is the
   strongest broker candidate here — a single small Go binary — but it
   only softens this point; it doesn't touch the next three.)
2. **Every signal we send is *about* a row, and co-commit deletes a
   whole race class.** A conversation-message NOTIFY rides the same commit as
   its `messages` INSERT; a `conversation_signals` doorbell rides the
   signal row's insert — the listener can never observe a doorbell
   whose row isn't visible, and a crash can never emit one without the
   other. With an external broker, every one of these becomes a
   two-system ordering problem (publish-then-commit vs
   commit-then-publish, both racy), whose standard fix is… an outbox
   table in Postgres with the broker demoted to a doorbell. That
   endgame is strictly more moving parts than starting and ending in
   Postgres.
3. **One liveness SPOF instead of two.** Postgres down = everything
   down, obviously and totally. Adding a broker creates the
   half-broken states ("API up, live streams dead, cancels dead")
   that are the worst kind of page for a self-host operator — partial,
   confusing, and only diagnosable with knowledge of our internals.
4. **Mode parity.** Local mode is SQLite; a broker could never be
   required there, so every mechanism would need a second (in-process)
   implementation regardless. Multi-mode has Postgres unconditionally
   — so the Postgres implementation is the *only* multi implementation
   needed, and local degrades to the in-process no-ops it already has.

The load numbers say the ceilings don't bind. Heartbeats: 50 executors
at ~4 s ≈ 12 tiny UPDATEs/s. Control signals: human-initiated,
~zero/s baseline. The WS backplane is the only real-volume channel:
~300 concurrent runs at agent message rates is low hundreds of
NOTIFYs/s, each riding a commit that was happening anyway. Postgres's
genuine NOTIFY limit — the global notification-queue lock serializes
committing notifiers, and slow listeners grow the (default 8 GB)
queue — becomes measurable in the sustained thousands-per-second range
with many concurrent committers: one to two orders of magnitude above
target scale, and a deployment big enough to approach it (thousands of
concurrent runs, each costing real LLM spend) gets re-benched long
before then. Queue-table hygiene is the other classic Postgres-queue
failure mode (dead-tuple bloat under high churn); our rows churn in
minutes, not milliseconds — autovacuum health on `conversations`/`event_queue`
goes in the P1 ops docs, and that's all it needs. This is also the
well-trodden boring path now (River/Oban/pgmq/Graphile Worker;
Rails 8 shipped DB-backed Solid Queue/Cache as its default,
*retiring* its Redis dependency).

What a broker would actually buy — no 8 KB payload limit (we pay the
`ws_outbox` ref pattern), native TTL/eviction (we pay small reapers),
native cross-pod counters (a fleet-wide rate limiter), and a higher
raw fan-out ceiling. None of these bind at target scale, and the two
that ever could have **contained, per-channel escape hatches**: the
Hub backplane is one interface, so `tf_ws` alone could move to a
broker without touching queues, leases, or signals; and a fleet-wide
GitHub-budget limiter would be a narrow *additive* adoption, not a
fabric change. Decision rule: adopt a broker on measured evidence,
per channel, never as the substrate.

**The same replaces-what-exactly test bounds Kubernetes as a design
basis.** The tempting swap — "let k8s be the fleet" — founders on the
unit of scale: a run is not a pod (§2.2 — one executor pod hosts
~a hundred gVisor sandboxes with warm caches, a subnet pool, and
per-run proxies; run-per-pod would forfeit all three and trade a
200 ms sandbox spawn for pod-startup seconds). So k8s could supply
process supervision, node heartbeats, and a drain-ish verb — the
trivial fifth of this spec — while the application-semantic
four-fifths (queue claims, cross-pod run control, WS fan-out,
per-`(org,repo)` affinity, single-writer and FIFO invariants,
ownership-scoped recovery) remain ours to build on any substrate.
Requiring it would also invert the deployment floor for exactly the
segments the product exists to serve (§2.1's ladder) and fork local
mode off the shared mechanism. Hence the settled posture: k8s is
**permitted as an executor substrate** where it genuinely earns its
keep — node management and autoscaling underneath privileged executor
pods (§2.1, §4.5) — and never designed against.

### 5.1 WS fan-out

`websocket.Hub.Broadcast` gains a backplane: publish to `tf_ws` (insert
outbox row first when large, NOTIFY inside the same tx as the
underlying write so a reader can always fetch what the ref points to);
every control pod LISTENs and fans-in remote events to its local
sockets through the existing per-`(org,user)` scope filter. Origin-pod
id in the envelope prevents double-delivery to the producer's own
clients. Per-connection NOTIFY ordering keeps per-conversation message order.

(Run sentinels deliberately do **not** ride this channel — they are
brain-bound, not socket-bound; see §5.3 and the `tf_bus` channel.)

Same channel fixes the two security/UX locals:

- **`CloseUserConnections`** (logout, membership removal) broadcasts a
  kick on `tf_ctl`; every control pod closes that user's local sockets.
  Closes the current cross-replica revoke gap.
- **Presence** moves to a `ws_presence` table (upsert on
  viewing/visibility change + TTL heartbeat): `PresentFor` reads rows,
  not local sockets — the unattended-prompt fast-deny works regardless
  of which pod holds the socket, and the dashboard gets live-viewer
  counts for free.

The in-process `eventbus` itself does **not** become distributed — all
its subscribers are brain components co-located on the leader by
construction (§2.3). The ws-broadcast subscriber is the one bridge
between worlds.

### 5.2 Cross-pod run control

`RunController` gets its intended second implementation. New outbox:

```sql
conversation_signals (
  id           bigserial PRIMARY KEY,
  org_id       uuid NOT NULL,
  conversation_id text NOT NULL,
  kind         text NOT NULL,      -- cancel | interrupt | steer | permission | inject
  payload      jsonb,              -- steer text, permission decision, injection body, ...
  target       text NOT NULL,      -- executor id owning the run
  created_at   timestamptz NOT NULL,
  acked_at     timestamptz
)
```

The fifth kind, `inject`, exists because TFAC-594's additive-event
injection is a routed operation wearing a queued API: its live path is
`getProc(conversationID)` — a process-local map hit — and its fallback durably
stages for the next resume. On one process that's correct. Across the
split, the router (brain) misses `getProc` for every run executing on
an executor, so every additive event would silently degrade to
staged-and-probably-never-delivered (active auto-runs usually
terminate without another resume). Under `conversation_signals`, the owner
delivers into its live process; the staged fallback remains for
genuinely parked runs.

Control-pod handler logic (`message` / `interrupt` / `cancel` /
`permissions/{id}`):

1. Local process registry hit? → in-process controller, exactly today's
   path (keeps `TF_ROLE=all` latency identical).
2. Else resolve the active claim's `claims.executor_id` + executor
   liveness: live → insert
   signal + `NOTIFY tf_ctl`; owner LISTENs, applies via *its* local
   registry (including resolving `permPending`), acks.
3. No live owner → today's DB-only paths (`MarkCancelledIfActive`, 409
   for steer/interrupt), unchanged semantics.

For the record, this round-trip is a named pattern, not an invention:
asynchronous [Request–Reply](https://www.enterpriseintegrationpatterns.com/patterns/messaging/RequestReply.html)
with a [Correlation Identifier](https://www.enterpriseintegrationpatterns.com/patterns/messaging/CorrelationIdentifier.html)
(Hohpe & Woolf, *Enterprise Integration Patterns*) carried over
pub/sub — the same shape as CI approval gates, Temporal signals, and
multi-server websocket backplanes. The permission `tool_call_id` and
`conversation_signals.id` are the correlation identifiers.

**Parked/open runs stop being a control-plane special case entirely**:
`ResumeOpenRun` becomes *resume-by-enqueue* — a message to a hibernated
run enqueues its continuation as ordinary claimable work (preferred to
its last executor for the warm worktree, rehydratable anywhere from
S3). This retires the in-process resume goroutine **in every mode,
`TF_ROLE=all` included** — deliberately unlike the signal handlers
above, which do keep a local short-circuit. The rule that separates
them: **operations on live processes are routed; creation of work is
queued.** Steer/interrupt/permission act on an in-memory process
handle that exists in exactly one process, so they are deliverable
from any pod at any N — `conversation_signals` finds the owner and delivers,
one NOTIFY hop (~ms) each way, no leader involvement — but they can
never be *claimed* by an arbitrary worker the way queue work can; the
local short-circuit is merely the degenerate route when the caller
already owns the handle (`TF_ROLE=all`). A resume has no live
process; it *mints work*,
and work that bypasses the queue bypasses every admission gate built
in this design — the memory floor, the per-org concurrency cap, fair
ordering, budget admission (§6.4) — exactly the storm path (wake 20
parked runs at once) where those gates matter. Routing it through the
queue costs ~ms at `all` (the wake is the in-process dispatcher
nudge; the spawn it precedes costs seconds), makes a crash between
"message recorded" and "process spawned" recoverable by the standard
reconcile instead of an ad-hoc path, and keeps "the queue is the
truth" true at N=1 — no class of running work exists that never
appeared as a queued row.

State-machine shape, settled here: the user message is recorded first
(`messages` + pending-input on the conversation), then the **same
`conversations` row** transitions back to queued and is claimed like any other (its
original enqueue time makes it oldest-first, so wakes claim
promptly); the claiming executor's resume path rehydrates, spawns
with `--resume <session_id>`, and delivers the recorded message as
the turn input. No second runs row, and — keeping the standing
invariant — no new conversation status.

Acked signal rows are purged after 24 h (audit convenience window);
stale unacked signals expire harmlessly (the reaper owns the run's fate if the
owner died mid-signal).

### 5.3 Brain-bound relays (triggers + run sentinels)

`Manager.Trigger(orgID)` (scorer/classifier/profiler/reconciler) and
`PollSoon(source, orgID)` grow a cross-process path: non-leader callers
publish `{trigger}` on `tf_ctl`; the leader routes to the in-process
manager. Callers that need this: config-save handlers on non-leader
control pods, the delegation spawner's classifier wait on executors,
the re-profile button. At `all`, loopback.

The same brain-bound motion carries the run sentinels
(`system:conversation:status` / `system:conversation:activity`, TFAC-592): they are
published on the bus of the process *running the run* — an executor,
after the split — while their consumers (EE bus subscribers such as
the Slack liveness adapter) are brain components (§3). They ride a
dedicated **`tf_bus`** channel: not `tf_ctl`, because their volume
tracks agent activity and a burst must never queue ahead of a cancel
on the control channel's per-connection FIFO; and not `tf_ws`, whose
contract is "every pod fans to sockets" — M−1 pods would receive a
high-volume stream only to discard it. **Only the brain LISTENs on
`tf_bus`.** The subscription starts and stops with the lease like any
other brain subsystem, so subscription *scope* replaces per-message
routing: there is no relay tag and nothing for standbys to remember
to drop, because they never receive the traffic. The holder
re-publishes envelopes onto its local bus, skipping self-origin ones
(at `TF_ROLE=all` the sentinel is already on the only bus there is).
Delivery is lossy-by-contract like `tf_ws` — sentinels are an
observability seam (liveness indicators), never load-bearing state —
so a leader-handoff gap costs a blip, not correctness.

One more sentinel joined the brain-local family since this section was
written: `system:routing:disposition` (TFAC-593), published by
`HandleEvent` — i.e. by the router, a brain component — with no
consumer yet (the Slack ack-reaction work will be the first). As long
as its consumers are brain-domain they share the router's process and
the in-process bus suffices, no relay needed; a consumer that ever
lives elsewhere makes it a `tf_bus` case like the run sentinels.

### 5.4 Correctness bundle (needed regardless of N) — ✅ shipped

Small fixes the fleet exposes but that were wrong-by-inspection
(TFAC-579 #622, review fixes #624). What shipped, including where the
implementation deliberately diverges from the original text:

- `PopForEntity` is a claiming pop (`UPDATE ... SET status='draining',
  claimed_at=now() ... RETURNING` under `SKIP LOCKED`), retiring the
  process-local `drainLocks` as the only serialization. The new
  in-flight state comes with its own crash recovery: the drain sweeper
  requeues `'draining'` rows whose claim is stale (~2 min) —
  **staleness-based, not ownership-scoped**, on purpose (decision 9,
  §11): a firing claim is a milliseconds-scale DB transaction, not
  long-lived owned work, and a duplicate drain is absorbed by the
  fences below. `'draining'` counts as queued intent everywhere it
  matters: the dedup index and the router's busy gate both see it, so
  a duplicate enqueue mid-drain collapses and a fresh event can't jump
  the queue inside the drain window.
- The "one active auto-run per entity" gate is DB-enforced: a partial
  unique index on `blueprint_runs (org_id, entity_id) WHERE
  trigger_type='event' AND status='running'`, with `entity_id`
  denormalized at insert. The in-memory check survives as a fast-path
  only. **The index loser is a distinct outcome from the replay
  fence** (`ErrEntityBusy` vs `inserted=false`): a replay is
  permanently satisfied; entity-busy is a deferral the router queues
  onto `pending_firings` — conflating them silently dropped intent on
  a routine gate race.
- The `became_atomic` suppression is serialized under the per-entity
  advisory lock shared with task creation rather than a unique index —
  the invariant is cross-row/cross-event-type, which no partial index
  can express. Mechanism drift from this spec's original "unique
  backing" wording, correctness equivalent.
- `system_llm_runs` idempotency keys on the Claude Code **session id**,
  not the `agentproc` TraceID this spec originally named — the TraceID
  is a per-job label ("scorer-batch") and would have collapsed all of a
  job's spend rows into one. Partial unique index excludes NULL, so
  trace-less rows never collide.
- `entities.poll_seq` CAS (§3) — **and the tracker suppresses the
  cycle's diffed events when the CAS loses or errors.** The snapshot
  write is the sole re-emit prevention; publishing transitions off a
  snapshot that didn't win re-derives them next cycle under fresh event
  ids the (event, trigger) fence can't collapse. The winner's next
  cycle re-diffs and emits, so suppression loses nothing.

### 5.5 Deploys and version skew

Heartbeats carry `version`; the dashboard surfaces skew. Rolling rule:
control pods first (they migrate, under the goose lock — ✅ shipped,
TFAC-580), then executors drain-and-replace. Schema changes require
drain-first executor deploys — cheap to enforce because executors
refuse to start against a newer schema than their build.

Two shipped details worth knowing (§3 has the enforcement-point
precision):

- **An executor performs zero boot-time DB writes** — no DDL, and it
  skips the `SeedEventTypes` catalog reconciliation too, so a stale
  executor build can never stomp catalog rows a newer control build
  wrote. That skip is itself a version-skew protection, not an
  optimization.
- **"Wait if behind" is the entrypoint's bounded retry** (30×1s, then
  the restart policy), and today it retries the never-self-resolving
  "ahead" refusal identically — both exit 1. Distinguishing them (and
  the in-process assert for entrypoint-less starts) rides with
  TFAC-582.

---

## 6. Placement

### 6.1 The settled decision: hash for the map, table for the exceptions

The epic left "deterministic hash vs routing table" open. The design
dissolves the dilemma rather than picking a side, via one doctrine:

> **The queue is the truth; placement is a preference.**

Placement never carries correctness. Multi-mode runs are self-contained
(TFAC-545) and workspaces rehydrate from S3, so *any* executor can
execute *any* run — affinity only buys warm caches (bare clones, shared
worktrees, rootfs variants). Once placement is advisory, a stale, wrong,
or absent placement map cannot break anything — which means the map
should be the cheapest self-healing thing available:

- **Default map = capacity-weighted rendezvous hashing** over live
  registry members, keyed `(org, repo)` for delegation. No state to
  maintain, any pod computes it identically, joins/leaves reshuffle
  only the affected keys, and weights come from `instances.max_runs`.
  Top-K candidates fall out for free (hot-repo bounded replication: the
  first K candidates all count as "preferred", bounding a hot
  monorepo's cache to K pods on a cost dial — no hard ceiling).
- **`placement_overrides` table only for human intent**: manual pins,
  hot-key `replicas=K`, and nothing else (drain lives on the executor
  row). Checked before the hash; expected to stay nearly empty.
- **Explainability closes the gap** that made a routing table tempting:
  a `placement` read endpoint + CLI verb that shows, for any key, the
  computed candidate order and why (weights, overrides, liveness), plus
  a dashboard column. Determinism plus an explainer beats a mutable
  table plus a rebalancer.

### 6.2 Mechanics

- Control stamps `conversations.preferred_executor_id` at enqueue: the
  enqueuing pod computes the rendezvous winner over live registry
  members and writes it onto the row, so tier-1 claims are an indexed
  equality instead of per-claim hash evaluation. The stamp is a
  snapshot of membership and can go stale if the fleet changes while
  the run waits — but a run waits seconds-to-minutes while membership
  changes hours-to-days apart, and the row classes that *do* outlive
  their stamp are **re-stamped as they re-enter the queue**: a reaper
  requeue clears it (its stamped executor is likely the dead one, and
  nothing about the corpse is worth chasing), while a **resume stamps
  the executor of the conversation's newest claim** — at resume time
  the last engagement's executor is better than a fresh hash, because
  that machine still holds the workspace tree `worktree_path` names.
  Net invariant: no stamp is ever older than one queue dwell, and a
  stale one costs a warm cache via the aging tier, never correctness.
- Claim is two-tier: **(1)** my queued runs (`preferred = me`), **(2)**
  anyone's queued runs older than an aging threshold (~15–30 s) or
  whose preferred executor is dead/gated/draining. Tier 2 is the
  spillover that makes affinity lose to capacity: a saturated or gated
  owner never head-of-line-blocks its shard — work ages briefly, then
  flows to headroom, exactly the TFAC-552 doctrine extended fleet-wide.
- At N=1 the hash always returns self; tier 1 always hits. No-op.

### 6.3 Curator homing — removed (TFAC-894)

Curator sessions were, at this design's writing, the one *stateful*
placement client: sticky cwd + shared-RO worktrees per
`(org, project)`, with a `curator_homes` row mapping
`(org, project) → executor`, minted on first turn from the same
rendezvous hash and held sticky — via a hard preference in the ordinary
queue — until the home executor died, at which point the next turn
re-homed the row and the new executor re-materialized worktrees through
TFAC-60's seed-on-demand.

The curator feature has since been removed entirely (TFAC-894): no
more curator sessions, no `curator_homes` table, no homing logic. This
section stays only as the record that the mechanism existed and why it
leaves nothing behind — it was scoped to curator's own sticky-cwd
requirement alone, never a dependency of delegation's placement.
Delegation's `(org, repo)` rendezvous hash and `preferred_executor_id`
stamping (§6.2) are unaffected; they never routed through this.

### 6.4 Heterogeneous sandboxes (the TFAC-408 interplay)

The sandbox-fleet spec (`docs/for-agents/specs/sandbox-fleet/`) makes the sandbox
configurable per org: named **profiles** with an image (rootfs
variant), an egress policy, and a resource class. Two of those
dimensions are placement-inert: egress and rlimits resolve per-run
from the profile at spawn time — any executor enforces any policy,
zero executor state. The image and the memory class are not, and they
are exactly what "counts + memory" fails to capture:

- **Admission becomes budget-based the day two resource classes
  exist.** Today's `max_runs` and the ~512 MB/run rule assume uniform
  runs; a browser profile is several × that. The profile carries a
  `mem_budget_mb`, the conversation row is stamped with it at enqueue, the
  claim admits on `reserved_mb + budget ≤ capacity` (keeping
  `max_runs` as an absolute ceiling and the TFAC-552 floor as the
  actual-memory backstop), and the heartbeat reports `reserved_mb`
  alongside `active_runs`.
- **The image dimension adds a third category to the claim doctrine:**
  *the queue is the truth; placement is a preference; **eligibility is
  a constraint**.* Variants are baked artifacts: an executor has one
  cached, can bake it on demand (assumes alpine-CDN egress — on
  isolated networks variants must instead be pre-baked into the
  shipped image or served from an internal mirror), or cannot run the
  profile at all. Warm-variant locality is a *preference* (folds into
  the rendezvous tiers); bake-capability is a *constraint* — a WHERE
  clause on the claim, because an aged run must never spill to an
  executor that cannot execute it, and a key no live executor can
  serve fails fast with a legible `failure_kind` instead of queueing
  forever. Track warm variants in a small
  `instance_variants(instance_id, rootfs_key, baked_at, last_used_at,
  bytes)` table — which also gives the rootfs cache the LRU/disk
  budget it currently lacks (variants run 400–700 MB and org-authored
  recipes make the set unbounded) and feeds the dashboard. Operator
  pool segmentation (big-memory executors for heavy profiles,
  sandbox-fleet §5) is a static `instances.labels` match — same
  family as `placement_overrides`, human intent only.

  Composition with §6.2's claim, stated precisely: a claim decision
  has three nested layers — **eligibility** (could this executor
  *ever* run this run: variant availability/bakeability, pool
  labels), **admission** (can it right now: budget headroom, mem
  floor, gated/draining, `max_runs`), and **preference** (should it,
  for cache warmth: the stamp, tier 1 vs tier 2). **Aging erodes
  only the third.** Tier-2 "claimable by anyone" means anyone
  *eligible and admissible*; an empty eligibility set fails fast
  (above); an eligible-but-saturated set queues as ordinary
  under-capacity for that profile class. And the enqueue-time stamp
  rendezvouses over the **eligible subset**, not all members —
  otherwise a run whose global winner lacks its variant is born with
  a useless stamp and eats the aging delay on every single run of
  that profile.
- **Cross-org sharing is safe by construction — for recipes.** A
  variant is not an uploaded artifact; it is a *recipe*,
  content-addressed by `rootfsCacheKey` (alpine sha + package set),
  and the key is deliberately org-blind: two orgs with identical
  recipes share one baked rootfs. The tenancy invariant that makes
  this fine: **a baked rootfs may be shared cross-tenant iff it
  derives only from trusted public, sha-pinned inputs and is mounted
  read-only** — both hold today (the base rootfs already shares
  cross-tenant; same argument as the shared-RO worktree precedent).
  The org boundary lives on the *profile object* (RLS-scoped, per
  sandbox-fleet §6), never on the artifact. Anything org-*supplied*
  (uploaded blobs, private packages) breaks the invariant and would
  have to live as org-scoped artifacts with per-org disk accounting —
  out of sandbox-fleet v1's scope; credentials and egress policy stay
  per-run in the proxies, never baked into an image. And when
  org-authored recipes do ship, the privilege-separation epic
  (`docs/security/security-overview.md`) adds a hard constraint:
  such a build executes customer-influenced package scripts, so it must
  run in an isolated/unprivileged builder — never in the
  capability-holding `cap-broker` — which then only mounts the
  resulting rootfs read-only by verified hash.
- **The per-tenant compute quota is the same budget, summed per org —
  not a run-count cap.** Fleet-capacity admission (first bullet) answers
  *can this executor hold the run*; the SaaS compute quota answers *has
  this tenant used up the compute its plan includes*, and it is the
  **same `mem_budget_mb` unit** — one meter end to end. The claim gains a
  second budget term alongside the capacity check: `Σ(org's active runs'
  mem_budget_mb) + this run's budget ≤ org_budget_mb`. Memory, not run
  count, is the denomination on purpose: once profiles are heterogeneous,
  a browser-heavy tenant and a CI-triage tenant at the *same run count*
  consume wildly different compute, and memory (not runs) is what the
  vendor is billed for. Image/variant **size is deliberately *not* the
  meter** — it is a shared, content-addressed disk artifact
  (`instance_variants.bytes`, cross-tenant by hash — the image bullet
  above), so metering on it bills the cache, not the run. `org_budget_mb` is **plan-derived,
  never an operator-set row**: it rides the license entitlement (§11
  decision 1, `entitlements.Active()`), so it is un-raisable by the org
  admin by construction and scales with tier × seats (e.g.
  `base(tier) + per_seat(tier)·seats`, community < professional <
  enterprise) — the billing formula only parameterizes the number, it
  does not touch the mechanism, so the mechanism can land before the
  formula is settled. Self-host is unlimited (the operator sizes their
  own pools, sandbox-fleet §5); a manual per-tenant override can sit on
  top later, but the default is the plan. This is a **distinct axis from
  TFAC-590's** `org_settings.max_concurrent_runs`, which stays the
  *org's own* self-limit — a count-shaped knob the org sets to protect
  *its own* GitHub/CI/API from *its own* agents ("no more than N open PRs
  at once") — orthogonal to the operator's memory-denominated compute
  quota; the claim composes both (plus fairness ordering and the capacity
  floor). Lands with budget admission below (needs `mem_budget_mb` on the
  profile + run), and is console-EE / SaaS-only.

None of this is needed while only the "base" profile exists — P1–P3
deliberately assume one variant and a uniform budget. The gate:
before TFAC-408's image or resource dimensions ship on a
multi-executor deployment, land budget admission, the
eligibility-constrained claim, and `instance_variants` with an
eviction budget (the §9 P5 item).

Keying, settled 2026-07-07: **two-layer, name → recipe → hash.** A
profile binds a *named* catalog entry; the entry is a recipe (base +
package set) resolving to a `rootfsCacheKey` content hash; executors
bake, cache, dedupe, and advertise by **hash**
(`instance_variants.rootfs_key`), while authoring, rule-binding, and
pool labels speak **names**. Editing a recipe under a name mints a
new hash, so the physical layer never holds ambiguous bytes. The v1
catalog is curated ("base", "browser"); org-authored entries are
later catalog rows with authorship — same mechanism, more authors.

These are deliberately **not** pre-added to the registry schema, and
the reasons are specific, not reflexive YAGNI: `reserved_mb` is
identically `active_runs ×` the uniform budget until budgets are
heterogeneous, so pre-adding it ships a writer that cannot be
exercised in anger; `instance_variants`' key shape hangs on
TFAC-408's unresolved catalog-vs-recipe decision (variant *name* vs
`rootfsCacheKey` content hash), so freezing it early converts a cheap
future ADD into a semantic ALTER on populated data; and the
fresh-install forward-migration model makes late additive schema on
these tiny tables ~free — the classic front-loading justification
(painful online ALTERs at scale) never applies. `instances.labels`
is the deliberate forward-compat room in the meantime, per the
`executor_id` precedent: pre-ship only the shape-certain thing.

### 6.5 The locality model (what placement never looks at)

Everything in this section shares one inversion that is easy to miss and
load-bearing everywhere: **locality is manufactured, never observed.** No
component anywhere inventories which repos, bares, or worktrees sit on
which executor's disk. Routing consults exactly two things: the pure
rendezvous hash (stateless) and the per-run `preferred_executor_id`
stamp (lives one queue dwell, cleared on requeue). The warm cache is
the *consequence* of always routing a key to the same winner — never an
*input* to the routing decision.

Two corollaries, each of which looks like a bug or a missing feature
until you see the doctrine:

- **Cache eviction changes no routing state.** When the TFAC-60 reaper
  evicts a cold bare, nothing is "unmarked" anywhere — the next run for
  that key routes to the same winner and pays one blobless clone to
  re-seed. Eviction is a cache concern, routing is a hashing concern;
  coupling them would require fleet-visible disk inventory.
- **Feeding observed cache/disk state back into placement is a
  correctness bug, not an optimization.** Determinism is the whole
  contract: every pod computes the same winner for a key with zero
  coordination. A "prefer whoever is warm" shortcut evaluated on one
  pod's local knowledge forks the map.

Capacity, by contrast, *is* observed — but only through the registry
heartbeat (weights, liveness, the memory gate), never per-key. And the
capacity envelope is per-host and work-source-agnostic: every
dispatched run is admitted through the same memory guardrail and
dispatch concurrency limit, so heartbeat occupancy reports the host's
true sandbox load, not just the conversation queue's share of it.

---

## 7. Background jobs at N (and the unprivileged control plane)

System jobs (scorer/classifier/profiler batches — Haiku) stay **on the
leader**: sentinels stay in-process, `syslimit`'s cap stays globally
true because exactly one process runs them, and nothing about their
(already replica-tolerant: last-writer-wins, delegation-fenced) writes
changes.

The endgame (settled 2026-07-11, superseding the original job-class
plan): **the brain's jobs stop needing a sandbox at all, instead of
moving to somewhere privileged enough to host one.** The observation
is that the five brain-side LLM paths split by capability surface,
not by subsystem:

- **Pure completions** — the scorer (`internal/ai/scorer.go`),
  classifier stage 1 (`internal/projectclassify`), and the repo
  profiler (`internal/repoprofile`) are toolless: no `AllowedTools`,
  no `Cwd` (an empty scratch dir), no git proxy, no agenthost. Prompt
  in, JSON out. The gVisor jail exists to contain T3 — an SDK RCE
  reaching tools, files, or network — a surface these jobs never had.
  They become **direct LLM API calls from Go** (same credential
  resolution, same spend accounting), dropping the Node SDK
  subprocess entirely — which also removes the one
  untrusted-model-output parser that isn't memory-safe Go from the
  control plane.
- **Agentic — none remain.** Curator turns (full tool allowlist, real
  worktrees, agenthost `exec` surface) were the one brain job that
  genuinely needed the jail. Curator no longer exists (TFAC-894), so
  every brain job left is a pure toolless completion — the
  unprivileged-control-plane argument this section makes isn't a
  target to reach anymore, it's already true.
- **Classifier stage 2 is removed, not migrated.** The agentic
  KB-reading disambiguation pass was the one job both brain-resident
  and genuinely agentic; rather than home it to executors, delete it.
  An exact stage-1 tie resolves to **unassigned** — the existing
  classified-but-unassigned state (`classified_at` set, `project_id`
  NULL), surfaced by the project backfill UI for a human to place.
  Rationale: a tie means "can't tell", and "can't tell" must never
  widen anything — projects are team-scoped, and `OwningTeamForEntitySystem`
  derives team visibility from `project_id`, so resolving a tie by
  assigning *both* projects would derive team visibility from
  classifier uncertainty. Unassigned entities keep flowing: the
  factory belt keys off tracked repos / Jira project keys, never
  `entities.project_id`.

Net effect: control pods drop `SYS_ADMIN`/`NET_ADMIN`/runsc **and any
Node invocation** — a plain Go web tier that runs anywhere,
managed-k8s included. `syslimit` survives unchanged (the jobs stay
leader-only and in-process; exactly one process still runs them);
what disappears is the per-job sandbox cost, not their placement. The
original alternative — generalize the conversation queue with a job class so
system jobs become claimable executor work — is retired: it moved the
sandbox dependency around instead of deleting it, and its secondary
payoffs (fleet-wide accounting, fairness for system jobs) don't bind
at target scale.

Also inherited into this epic from TFAC-307 (they bind at fan-out, not
at N=1): the short-TTL `ForSystem` credential-resolution cache, pool
ceilings behind a transaction-mode pooler, and the agenthost-level
per-upstream throttle + 429/`Retry-After` backoff (the Jira client has
none today; all runs share one org bot identity, so the fleet
multiplies pressure on a per-account limit we don't control — degrade
gracefully at the choke point rather than erroring into agents).

---

## 8. Monitoring & the shared fleet dashboard

### 8.1 Principles

- **Product-grade observability is DB-backed and ships in the binary.**
  It must work on a bare compose install with no additional infra. No
  Prometheus dependency for the product surface; a `/metrics` exporter
  is an optional add-on for operators who already run Grafana.
- **One dashboard, both audiences — parity by identity.** There is no
  separate SaaS admin portal for infrastructure (TFOPS covers
  billing/licenses via Stripe + Supabase Studio). The fleet dashboard
  ships in-product, gated on a deployment-**operator** identity:
  self-host operators see their own fleet; SkyAI staff see the SaaS
  fleet by being operators of that deployment. Same code, same API,
  zero drift.
- **The scheduler's bookkeeping is the telemetry.** The registry
  (§4.1), the existing timing columns on `conversations` and
  `claims`, and one small sample table are the entire data model —
  no parallel pipeline.

### 8.2 Data model

Already present on `conversations`: `started_at` (enqueue), `queued_at`,
`completed_at`, `parked_at`, `status/outcome/failure_kind`. `claimed_at`,
`duration_ms`, `num_turns`, `executor_id`, and `attempts` live on (or derive
from) `claims`, and cost + the four token totals are derived from `messages`.
Queue wait = `claimed_at − started_at`.
`event_queue`/`pending_firings` carry full timing. `llm_spend` covers
money. Net-new:

```sql
instance_stats (          -- 1-minute samples, written by each pod, ~30d retention
  instance_id  text,
  at           timestamptz,
  active_runs  int,
  queued_visible int,      -- queue depth this executor could claim
  mem_available_mb int,
  cpu_pct      real,       -- sampler promoted from cmd/sandbox-bench/hostmetrics_linux.go
  load1        real,
  claims       int,        -- claims since last sample
  spawn_p50_ms int,
  oom_kills    int
)
```

One row per instance per minute is negligible write load and answers
every time-series question the dashboard has (utilization history,
claim rates, headroom trends). A retention reaper trims it. One
constraint on the sampler when it's promoted from
`cmd/sandbox-bench/hostmetrics_linux.go`: its memory fields must come
from `hostmem`, not the bench's own `/proc/meminfo` reader — the bench
deliberately reads host-wide (it benchmarks hosts); the dashboard must
report instance truth.

Prerequisite fix: ✅ shipped (TFAC-581 + #624). `hostmem` is
**cgroup-aware** — and slightly stronger than this section originally
asked for: it resolves the process's own cgroup from
`/proc/self/cgroup` and takes the most restrictive `memory.max` across
the whole ancestry (with `memory.stat` inactive_file as the
reclaimable credit), so it is truthful under shared cgroup namespaces
and under bare-metal systemd slices with `MemoryMax` set, not just
"when containerized". Figures cross-clamp against `/proc/meminfo`, so
a limit above physical RAM can't fabricate headroom; ambiguous cgroup
reads fail open to Unknown with a logged warning (Unknown disarms the
dispatch floor — silence there was itself a hazard).

### 8.3 Surfaces

- **`GET /api/fleet/*`** (operator-gated): `overview` (fleet totals,
  capacity vs active, queue depth + wait percentiles, version skew),
  `instances` (registry + live stats + drain control), `timeseries`
  (instance_stats windows), `placement` (the §6.1 explainer),
  `queue` (oldest-waiting, per-org shares).
- **Fleet page** in the SPA (or an Infrastructure tab on `/usage`):
  machines with capacity bars + gated/draining/version badges, live
  run counts, queue depth/wait sparklines, duration percentiles,
  failure_kind rates, spend overlay from `llm_spend`. 10 s poll first
  (factory-snapshot precedent); a coalesced `fleet_updated` WS event
  later if wanted.
- **Org-scoped subset** on `/usage` for org admins (their queue waits,
  run durations, active runs) — visible on SaaS without exposing other
  tenants' machine truth.
- **Operator identity (settled)**: a CLI-managed user flag —
  `triagefactory operator add|remove|list <email>` — recorded in the
  access-change log. Bootstrap is shell access to the deployment,
  which is already the operator trust boundary (same as `jwk-init`);
  the flag merely reflects it into the product. Rejected: env
  allowlists (rotation = redeploy, no audit entry, drifts under SSO
  renames) and org-owner auto-grant (wrong on shared SaaS).
- `GET /api/system/capacity` (TFAC-555) ships early and reads the same
  registry — at N=1 it's the one-row special case of the fleet view.
- Per-role health: `GET /readyz` **already exists** (TFAC-573) — DB +
  migrations + poller-alive as hard 503s, poll staleness / GitHub rate
  budget / active-runs as soft signals. The split's remaining work is
  conditionality, not construction: the poller hard-check applies
  **only on the lease holder** (a standby runs no pollers and must
  stay in LB rotation — as shipped, `hardOK && pollerAlive` would 503
  every non-leader and collapse the HA shape to leader-only serving);
  standbys hard-check DB + migrations and report lease state
  informationally; executors serve a separate local healthz
  (dispatcher + heartbeat-write liveness — they have no user HTTP).
  All roles already log structured JSON in multi-mode.

### 8.4 Packaging (settled 2026-07-07)

**Console EE, operability core.** Core: the registry, the capacity
read-out (TFAC-555), drain + operator CLI verbs, and the org-scoped
usage additions — an unlicensed N-executor deployment stays fully
operable from the shell. EE: the **fleet console** (the Fleet page
and the rich `/api/fleet` surfaces — timeseries, placement explainer,
drain controls in the UI) as the "sandbox-fleet administration"
feature the entitlements package already names, module shape per
`docs/ee-feature-packaging.md`. Never gate operability; gate the
console.

One scoping rule the split surfaced: **fleet administration is a
deployment-scoped feature, so it resolves through the
deployment-scoped entitlement path** — the license-backed
`entitlements.Active()` — **never the per-org resolver.** The per-org
path (SaaS Stripe entitlements, `entitlements.For(orgID)`) governs
org-facing surfaces only; it could not even name an org for a
cross-org console. The two gates compose: `is_operator` (identity,
§8.3) AND `Active().Has(FeatureFleet)` (deployment entitlement). On
the shared SaaS deployment the operator is the vendor, which simply
runs its own licensed deployment — no special case.

---

## 9. Rollout

Each phase ships green and independently valuable; nothing requires a
flag day. Sizes in house S/M/L/XL.

**P0 — Substrate (all N=1-safe, most N=1-valuable) — ✅ shipped**
(TFAC-577 #612 · TFAC-578 #614 · TFAC-579 #622 · TFAC-580 #621 ·
TFAC-581 #620, merged 2026-07-07/08; post-merge review fixes in #624 —
identity-fence reaction, resume-path ownership stamp, CAS emission
gate, `'draining'` crash recovery, entity-busy≠replay, meminfo
cross-clamp. The sections above carry the per-item deltas.)
1. Executor registry + heartbeat + persistent identity + boot_epoch;
   stamp real `executor_id`; TFAC-552's gated-flag/headroom follow-up
   lands on the row; TFAC-555 capacity endpoint reads it. (M) ✅
2. Ownership-scoped boot resets + leader-reaper skeleton (at N=1 it
   reaps only stale-epoch self rows — fixes today's restart
   double-execution window). (M) ✅
3. Correctness bundle §5.4: claiming pop, auto-run DB gate,
   became_atomic fence, `system_llm_runs` trace-id, `poll_seq` CAS. (M) ✅
4. `goose.Up` advisory lock; executor schema-version assert. (S) ✅
5. cgroup-aware `hostmem`. (S) ✅

**P1 — The split (first real fleet)**
6. `TF_ROLE` + per-role subsystem wiring + compose profile (N executors
   + LB) + self-host docs. (L)
7. Leader lease + brain gating (incl. EE `OnReady` workers) + standby
   takeover; trigger/PollSoon relay on `tf_ctl`; `/readyz` poller
   hard-check becomes lease-conditional (§8.3). (L)
8. WS backplane `tf_ws` + `ws_outbox` + presence table + cross-pod
   user-kick; `tf_bus` brain-bound sentinel relay (TFAC-592, §5.3 —
   leader-only LISTEN, lease-scoped). (L)
9. `conversation_signals` cross-pod control (cancel/interrupt/steer/permission)
   + resume-by-enqueue for parked runs. (L)
10. Fleet reaper (dead-executor requeue, attempts-capped) + drain flag +
    `tf_wake`. (M)

*Exit criteria: 1 control + N executors serve production traffic; kill
-9 an executor mid-run → its runs requeue and complete elsewhere;
rolling executor deploy with zero duplicated runs; browser on any pod
streams any run; steer/cancel/permission work cross-pod; leader kill →
standby takes over inside ~30 s with only free-304 catch-up cost.*

**P2 — Affinity**
11. Rendezvous placement + `preferred_executor_id` stamping + two-tier
    aging claim + `placement_overrides` + placement explainer. (M) ✅ shipped
    (TFAC-587): `internal/placement` (pure capacity-weighted rendezvous +
    resolver), the enqueue stamp (control computes the winner over live
    registry members; re-stamped each blueprint-step advance; cleared to NULL
    on every requeue/reset/reaper path, and re-stamped to the last
    engagement's executor on resume), the two-tier `ClaimNextConversation`
    (tier 1 = `preferred_executor_id = me`, tier 2 = aged past
    `TF_PLACEMENT_AGING_SEC` or preferred dead/gated/draining), the
    `placement_overrides` table (pin / hot-key `replicas=K`), and the
    explainer (`GET /api/fleet/placement` + `triagefactory instance
    placement`). Feature-flagged via `TF_PLACEMENT` (on by default in multi,
    forced off in local N=1); a disabled config makes the claim byte-identical
    to global-oldest, so the whole layer drops with all tests green.
12. Curator homes + re-home on death. (M) ✅ shipped, then removed with
    curator (TFAC-894).

**P3 — Fleet operations**
13. `instance_stats` sampler + retention + `/api/fleet` + Fleet UI +
    operator identity. (L)
14. Per-org fairness + per-org concurrency cap in the claim. (M)
15. TFAC-307 inheritances: cred cache, pooler validation + LISTEN conn
    split, agenthost upstream throttle/backoff. (M)
16. Optional `/metrics` exporter. (S)

**P4 — Unprivileged control plane** (in scope for the release; §7)
17. Classifier stage 2 removed; exact stage-1 ties → unassigned. (S)
18. Scorer / classifier / profiler become direct toolless LLM calls
    from Go — no SDK subprocess, no sandbox, no Node; spend
    accounting + `syslimit` semantics preserved. (M)
19. Control pods drop the sandbox caps. Shipped as an interim override
    file, then superseded by item 19b. (S)
19b. Retire `TF_ROLE=all` in multi (TFAC-637): multi always boots as
    control or executor; the default compose is the co-located split
    with control capless and only the executor referencing the
    hardening anchor; the fused in-process credential path (in-process
    run proxies, live-store clone tokens, in-process agenthost over
    stores) is deleted — sandboxed runs REQUIRE a prebuilt per-run
    network + credential sidecar. `all` survives as local mode's
    single-process shape only. (M)

**P5 — On demand**
20. Per-org brain sharding across control pods (same lease table,
    org-keyed rows). (L)
21. Hot-repo K-replication dial; heterogeneous-profile support per
    §6.4 — budget admission, eligibility-constrained claim,
    `instance_variants` + rootfs-cache eviction budget, pool labels
    (TFAC-408 §§4–5). (L)
22. Budgeted/resumable poll cycles — GitHub half shipped (TFAC-571),
    minus a durable cursor: the resume point is per-process, so a
    handoff restarts at the head of the repo list. Persisting it, and
    Jira parity, on demand. (S)

---

## 10. Non-goals

- Kubernetes operators, service meshes, external brokers (NATS/Redis/
  SQS) — Postgres carries the coordination load at every realistic
  fleet size this product targets; the recorded rationale and the
  per-channel crossover points are §5.0. Revisit only with evidence.
- Cross-region fleets, geo-placement.
- Autoscaling automation (operators add/drain machines; the dashboard
  tells them when — the join/leave/signal contract an external scaler
  needs is deliberately shipped anyway, §4.5).
- Per-sandbox scale-out (the executor is the unit, §2.2).
- Changing local mode in any way.

## 11. Decision log

Every question this spec originally left open was settled with the
epic owner on 2026-07-07; entry 9 was added by the post-ship P0 review
pass (2026-07-08). Reopening conditions noted per entry.

1. **EE boundary** — console EE, operability core, with the
   deployment-scope entitlement rule (§8.4). Reopens only with a
   packaging-strategy change.
2. **Operator identity** — CLI-managed user flag,
   `triagefactory operator add|remove|list <email>`, recorded in the
   access-change log (§8.3).
3. **Privileged control pods** — superseded 2026-07-11. Original
   entry: interim accepted, job-classes stay P4, reopen if a customer
   requires an unprivileged control tier. Revised: control ships
   unprivileged in the release via §7's de-sandbox plan — toolless
   system jobs become direct LLM calls (no Node), classifier stage 2
   is removed (exact ties → unassigned) — retiring the job-class
   endgame entirely rather than pulling it forward.
4. **Executor-loss retry** — `TF_MAX_CLAIM_ATTEMPTS` default 2, counted
   in *consecutive loss episodes* rather than lifetime claims (the
   dispatcher's episode doctrine: any claim that recorded a real
   outcome ends the episode, so a stopped-and-resumed conversation
   meets its first executor death at 1);
   duplicate external writes on retry are accepted (upsert semantics
   absorb them; `failure_kind='executor_lost'` audits the rest); no
   compensation logic. Retried token spend is bounded by the attempt
   cap and the org daily cost cap. Reopens only on evidence from a
   real fleet (§4.3).
5. **Fabric knobs** — defaults, all env-tunable: NOTIFY inline
   payload ≤ 6 KB, larger via outbox ref; `ws_outbox` TTL 60 s; acked
   `conversation_signals` purged after 24 h; registry tombstone GC at 7 days
   heartbeat-stale. Tuning under real load is routine ops, not open
   design (§5).
6. **TFAC-408 image keying** — two-layer, name → recipe → content
   hash; `instance_variants` keys on the hash (§6.4).
7. **Resume path at `TF_ROLE=all`** — always resume-by-enqueue; no
   in-process resume variant survives in any mode. The local
   short-circuit exists only for operations on live processes
   (steer/interrupt/cancel-live/permission), never for work creation
   (§5.2).
8. **Kubernetes as design basis** — rejected; permitted as an
   executor substrate only (§2.1, §5.0). It replaces the trivial
   layer (supervision, node heartbeat, drain) and none of the
   application-semantic layer; requiring it inverts the deployment
   floor. Reopens only if the unit of scale ever became run-per-pod,
   which §2.2 rejects independently.
9. **Crash recovery is matched to the work's lifetime, not
   standardized** (settled 2026-07-08, the P0 review pass; amended
   2026-08-06, the durability audit). Two patterns coexist
   deliberately: **ownership-scoped self-sweep** (runs — long-lived
   owned work where a false requeue duplicates an agent run's
   external writes, so only the owner may sweep, and dead owners wait
   for the reaper) and **staleness-based requeue** (pending_firings
   `'draining'` — a milliseconds-scale claim whose redelivery is
   absorbed by the (event, trigger) fence and the one-active index,
   so any process may recover it and no reaper dependency exists).
   `event_queue` is ownership-scoped **with a staleness backstop**:
   the owner's boot reset is the fast path, and an unscoped 10-minute
   sweep on the drain worker's floor scan covers the owner that is
   *replaced* rather than rebooted (scale-down, a fresh instance id
   after the lease moves). The original entry classified it
   ownership-scoped alone, on the assumption the owner always
   reboots; it does not, there is no reaper arm for this queue, and a
   routing claim is milliseconds-scale like a firing's — so the rows
   were stranded, and their events permanently unrouted. Deciding
   factor: cost of a wrong recovery vs cost of a stalled one.
   Reopens if a queue appears whose claims are both long-lived AND
   cheaply redeliverable.

## Related

- `docs/for-agents/specs/sandbox-fleet/` — sandbox profiles; §5's "profile as the
  scheduling unit" plugs into §6's placement labels (P5).
- `docs/benchmarks/sandbox-bench.md`, `docs/benchmarks/poll-bench.md` — the capacity numbers
  behind §0.
- `docs/security/isolation-tiers.md` — the tier ladder; this spec changes the
  *deployment* shape, never the org isolation model.
- `docs/ee-feature-packaging.md` — the module shape for §8.4.
- `docs/self-hosting/scaling.md` — gains the N-executor compose profile in
  P1.
