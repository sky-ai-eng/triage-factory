# Horizontal scaling — execution-plane split

Design of record for scaling Triage Factory past one machine: a small
**control plane** (the org-brain singletons) plus N **shared-nothing
executors** (the gVisor sandbox hosts), coordinated entirely through
Postgres. Same binary, same mechanism from N=1 self-host to an N-pod
fleet; no k8s, no service mesh, no pod-to-pod RPC, no shared filesystem.

Status: **accepted design** (this is the "dedicated design session" the
epic called for). Tracked as **TFAC-71**. Builds on the run-queue /
live-run / steering line (TFAC-13 → TFAC-305 → TFAC-309), the memory
guardrail (TFAC-552), the curator storage design (TFAC-60/61), and the
sandbox-fleet profiles spec (`docs/specs/sandbox-fleet/`).

Scope note: multi-mode only. Local mode (Tier 4, one user, SQLite) is
structurally N=1: it always runs `TF_ROLE=all` and every mechanism below
degrades to a no-op there. Local behavior does not change.

---

## 0. When this matters, and how much

A single multi-mode host already goes far. Measured on the benches
(`docs/sandbox-bench.md`, `docs/poll-bench.md`):

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
    min( TF_MAX_CONCURRENT_RUNS,  (RAM_MB − 12288) / 512,  256 )
```

e.g. three 64 GB executors ≈ ~300 concurrent runs, with the control
plane on a 2–4 GB pod.

---

## 1. What already exists (the substrate is queue-shaped)

The codebase has been deliberately pre-seamed for this split. Inventory
of what this design **reuses rather than builds**:

| Piece | Where | State |
| --- | --- | --- |
| Durable run queue, `FOR UPDATE SKIP LOCKED` claim | `internal/db/postgres/run_queue.go` (`ClaimNextRun`), `internal/delegate/dispatch.go` | Built (TFAC-13). Claim is already N-worker-safe; the dispatcher loop is per-process. |
| `Delegate()` = pure DB enqueue | `internal/delegate/delegate.go` → `EnqueueRun` | Built. Manual + auto delegation already work cross-machine unchanged — no spawner needed at the enqueue site. |
| Durable event queue for router-bound events | `internal/ingest/ingest.go`, `internal/db/postgres/event_queue.go` | Built. Single drain worker; `SKIP LOCKED` claim exists. |
| `runs.executor_id` ownership column | pg baseline (`runs`), stamped by `stampExecutor` (`process_registry.go`) | Written, consumed by nothing — the intended lease hook. |
| `RunController` indirection for cancel/steer/interrupt/permission | `internal/delegate/process_registry.go` | Built (TFAC-305). In-process impl resolves `s.procs`/`s.cancels`/`s.permPending`; the seam exists precisely so a DB-signaling impl can slot in. |
| Durable workspace snapshots + cross-host rehydrate | `internal/storage` (S3: `<org>/<blueprint_run>/workspace.tar`), `ensureWorkspace`/`workspaceRecoverable` (`resume.go`) | Built. A parked run can resume on a different machine today. |
| Self-contained multi-mode runs | TFAC-545: per-run clone with App installation token; per-run `llmproxy`/`gitproxy`/`egressproxy` on the veth IP; agenthost socket per run | Built. **Credentials and toolchain co-locate with whichever host executes the run** — nothing about a run's execution needs the API box. |
| Local admission control | `internal/hostmem` + `TF_DISPATCH_MEM_FLOOR_MB` (TFAC-552); `TF_MAX_CONCURRENT_RUNS` clamped to 256 | Built, per-process — exactly the right shape per-executor. Contract to preserve: **gating never mutates queue state**; a tight host simply stops claiming and work flows to headroom with zero coordination. |
| Replica-safe auth | RS256 JWKS verify (`internal/auth/verify`) + DB-backed sessions (`internal/sessions`) | Built. Any API replica can serve any browser; no sticky sessions. |
| Replica-safe delegation fences | `blueprint_runs_event_trigger_fence` UNIQUE `(triggering_event_id, trigger_id)`; task dedup partial index; `pending_firings` dedup index | Built. Two processes firing the same event/trigger produce exactly one run. |
| Advisory-lock precedents | `auth_provision.go`, `team_github_repos.go`, ee/slack | Built for single-operation correctness (not leadership). |
| Poll cursors + conditional-request state in DB | `poller_state`, repo pulls-poll-state (ETags) | Built. Poll position survives a process swap; a cold handoff re-lists as free 304s. |
| Cost/usage accounting + quotas | `llm_spend` view, `/api/usage/*`, daily + per-team caps, `system_llm_runs` | Built (TFAC-449). The **spend** layer of the dashboard exists; the **infrastructure** layer (this spec §8) does not. |
| Re-seedable per-org disk state | `internal/paths` under `TF_STATE_ROOT`; bounded evictable bare/worktree cache (TFAC-60); shared-RO curator worktrees (TFAC-61) | Built per-pod. Durable copies live in Postgres + S3; everything on executor disk is cache. |

And the inverse — what today **assumes exactly one process** (the
gap list this design closes):

1. **Boot recovery is global, not ownership-scoped.** Any process boot
   runs `ResetProcessingRuns` and `event_queue.ResetProcessing`, which
   flip **all** in-flight rows back to queued — a second replica booting
   (any rolling deploy) re-queues and re-executes work the first replica
   is still running. Highest-severity hazard.
2. **The tracker's snapshot diff is single-writer by silent assumption.**
   `UpdateSnapshotSystem` is a blind write (no version, no CAS); two
   concurrent pollers of one org duplicate events, clobber snapshots,
   and can *lose* transitions (the forward-only diff never re-emits).
3. **Per-entity event ordering is a single-worker property.** Close
   checks and route ordering rely on the event queue's one-worker global
   FIFO; `SKIP LOCKED` across N drainers would break same-entity order.
4. **The WS hub, permission broker, process registry, poll schedule,
   one-shot announce toasts, presence, and user-kick are all
   process-local.** A browser on pod A never sees events produced on pod
   B; `CloseUserConnections` (session revoke) only closes local sockets;
   cancel/steer/permission answers 409 unless they land on the owning
   process.
5. **`pending_firings` drain is only serialized in-memory** — the pop is
   a bare SELECT (claim lands later), the per-entity mutex is
   process-local, and the "one active auto-run per entity" gate is
   check-then-act. Cross-replica, two drains can double-pop and
   double-start runs on one entity (different triggers).
6. **Every replica would run every background job.** Pollers, scorer,
   classifier, profiler, reconciler, reapers all start unconditionally
   (`app.Run`), so N replicas = N× GitHub/Jira polling, N× Haiku spend,
   N× `system_llm_runs` accounting rows (append-only, no idempotency
   key), and the `syslimit` "global" cap silently becomes 8×N.
7. **Migrations race**: `goose.Up` has no lock; two booting replicas can
   collide.
8. Assorted per-process caches degrade quietly (token caches, reachable-
   repo cache, ip rate limiter ×N) — acceptable, documented below.

---

## 2. Topology

### 2.1 Roles

One binary, one new startup flag alongside `TF_MODE`:

```
TF_ROLE = all | control | executor        (default: all; local mode forces all)
```

- **`all`** — today's process: HTTP + WS + background brain + dispatcher
  + sandboxes. The default; the only shape local mode and small
  self-hosts ever see. All coordination mechanisms below still run, they
  just trivially self-resolve (the process holds every lease, placement
  always picks itself, NOTIFY loops back).
- **`control`** — serves HTTP/API/WS and *competes for leadership* of
  the background brain (§3). Never spawns user-run sandboxes (until the
  system-job endgame in §7, it still jails its own Haiku system jobs, so
  it keeps the privileged container caps).
- **`executor`** — runs the dispatcher, sandboxes, agenthost daemons,
  per-run proxies, and its own disk reapers against its own
  `TF_STATE_ROOT`. Serves no user HTTP (a local health endpoint only).
  Registers and heartbeats in the executor registry (§4).

Deployment floor stays compose: `postgres + gotrue + seaweedfs + 1..M
control + 1..N executor` behind any plain round-robin LB (control pods
only). Executors remain raw Docker / Fly Machines / VMs with
`SYS_ADMIN`+`NET_ADMIN`+unconfined seccomp+private cgroup — the same
caps the single container needs today; managed k8s remains a non-target.

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
| Curator turn execution | the session's **home executor** (§6.3) | shared-RO worktree + cwd cache locality |
| Migrations (`goose.Up`) | control pods only, under an advisory lock | executors assert schema version and wait |

### 2.4 Recommended shapes

| Shape | Control | Executors | What you get |
| --- | --- | --- | --- |
| **All-in-one** (default; local + small self-host) | `TF_ROLE=all`, one process | in-process | Today's box. Every mechanism self-resolves; nothing to operate. |
| **First fleet** | 1 | N | The main win: sandbox capacity scales independently and executor deploys never touch the API. The lone control pod trivially holds the brain lease. A control restart is a brief API blip + brain restart whose poll catch-up is mostly free 304s. |
| **HA** | 2–3 behind the LB | N | Every control pod serves API/WS (replica-safe already) and competes for the one brain lease; exactly one holds it, the rest are **working API replicas + warm standbys, not idle spares**. Leader loss → takeover ≤ ~TTL. Zero-downtime rolls both tiers: control one-at-a-time behind readiness, executors drain-and-replace. Postgres HA remains the floor — control HA cannot exceed the database's. |

Start at the smallest shape that meets the requirement and move down
the table only on evidence; the mechanisms are identical in all three,
so moving is a compose edit, not a migration.

The load-bearing move is the second column's first five rows: **the
entire org brain moves as one unit** with a single leader lease. That
one decision preserves four invariants at once — tracker single-writer,
event-queue FIFO, bus-local sentinels, process-true `syslimit` — and
makes leadership *coarse* (one lease), which is the cheapest thing that
can possibly work. Sharding the brain per-org across control pods is a
later refinement (§9 P4) that reuses the same lease table at a finer
key; the poll bench says one leader carries realistic multi-org load for
a long time.

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
  starts cold (every org due), but poll cursors + ETags are in the DB,
  so the catch-up cycle is mostly free 304s (`docs/poll-bench.md`).
- **Fencing where it counts, tolerance elsewhere.** Full fencing (every
  brain write carries the term) is overkill; the one genuinely dangerous
  overlap is the tracker snapshot RMW, and it gets its own guard: an
  `entities.poll_seq` CAS (`... SET snapshot_json=$1, poll_seq=poll_seq+1
  WHERE id=$2 AND poll_seq=$3`) so a straggler ex-leader's late write is
  a no-op instead of a regression. Everything else the brain writes is
  either fence-protected already (delegation), last-writer-wins-safe
  (scores, profiles), or idempotent (task dedup index).
- `goose.Up` wraps in `pg_advisory_lock` so M control pods can boot
  concurrently. Executors don't migrate; they compare the schema version
  at boot and wait/exit if behind (drain-first deploys are the rule for
  schema changes anyway, §5.5).

At `TF_ROLE=all` the single process always wins the lease — zero
behavior change.

---

## 4. The executor fleet

### 4.1 Identity and registry

Instance identity is **stable across restarts**: an id minted once and
persisted under `TF_STATE_ROOT` (so a rebooted executor recognizes *its
own* in-flight rows), plus a `boot_epoch` that increments per boot.
Mechanically:

1. **The id is a file; the file is the identity.** First boot mints a
   random id into `<TF_STATE_ROOT>/instance-id`; every boot re-reads
   it under an **exclusive flock held for the process lifetime**, so
   two processes pointed at one state root fail fast instead of
   silently sharing an identity. The id deliberately identifies the
   *state root*, not the machine or the process (hostnames are
   recycled in container platforms; PIDs are meaningless): ownership
   of rows is really ownership of the disk state — worktrees, caches —
   those rows reference, so identity must travel with the volume.
2. **The epoch is minted by the registry, not the file.** Boot
   registration is one statement — `INSERT … ON CONFLICT (id) DO
   UPDATE SET boot_epoch = instances.boot_epoch + 1, … RETURNING
   boot_epoch` — atomic and monotonic, immune to the volume
   snapshot/restore/clone weirdness that corrupts a file-local
   counter. Registration and epoch-mint are the same write.
3. **Claims stamp the pair.** `runs` (and event-queue claims) record a
   `claimed_epoch` next to `executor_id`. The boot self-sweep (§4.2)
   is then a pure predicate — reset `WHERE executor_id = me AND
   claimed_epoch < my epoch` — with no ordering dependence: rows from
   the current life can't match by construction, so the sweep is safe
   to run (or re-run) at any time, not only inside a carefully
   sequenced boot window.
4. **The heartbeat doubles as a split-identity fence.** Renewal is
   `UPDATE … WHERE id = me AND boot_epoch = mine`; matching zero rows
   means another process has re-registered this identity (a cloned
   volume, a duplicated state root across hosts — the case the flock
   can't see). The instance stops claiming and exits loudly rather
   than fight over ownership.
5. **Ephemeral state roots degrade to the reaper, never to
   corruption.** A pod with no persistent volume mints a fresh id
   each boot; its prior lives' rows are never self-swept, but the
   leader reaper collects them by heartbeat staleness like any dead
   executor's (§4.3), and a registry GC tombstones rows whose
   heartbeat is 7+ days stale (default). Persistent volumes are the recommendation
   for executors regardless (they carry the caches); correctness
   never depends on them.

At `TF_ROLE=all`/local the same mechanism runs against
`~/.triagefactory` and trivially yields one row, epoch bumping per
restart.

The registry is deliberately named for what it holds — **every TF
process in the deployment**, not just executors ("instance" is already
the shipped vocabulary: `runs.executor_id` stamps "a constant instance
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

1. **Ownership-scoped recovery** (P0, fixes hazard #1 even at N=1):
   `ResetProcessingRuns` / `ResetProcessing` become "reset rows where
   `executor_id = me AND boot_epoch < my current epoch`" — a booting
   process only sweeps *its own* orphans. Fleet-wide orphan recovery
   moves to the **leader reaper**: runs whose executor's heartbeat is
   stale past a threshold are requeued (`attempts`-capped, then failed
   with `failure_kind='executor_lost'`).
2. **Cross-process wake**: `EnqueueRun` also `NOTIFY tf_wake` so idle
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
  (`TF_RUN_MAX_ATTEMPTS`, default 2). Residual risk, accepted and
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
  partitioned executor can't write `run_messages` or heartbeats
  anyway — and without it, a requeued run and a zombie sandbox could
  both finish and double their external writes.
- **Graceful drain** (deploys, scale-down): set `draining=true` → the
  executor stops claiming; live runs finish or hibernate-on-idle
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

One rule governs every channel: **NOTIFY is a doorbell — never the
data, and never the only path.** A notification means "scan your
backing table now": `tf_ctl` consumers scan `run_signals` for unacked
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
   whole race class.** A run message NOTIFY rides the same commit as
   its `run_messages` INSERT; a `run_signals` doorbell rides the
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
minutes, not milliseconds — autovacuum health on `runs`/`event_queue`
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

### 5.1 WS fan-out

`websocket.Hub.Broadcast` gains a backplane: publish to `tf_ws` (insert
outbox row first when large, NOTIFY inside the same tx as the
underlying write so a reader can always fetch what the ref points to);
every control pod LISTENs and fans-in remote events to its local
sockets through the existing per-`(org,user)` scope filter. Origin-pod
id in the envelope prevents double-delivery to the producer's own
clients. Per-connection NOTIFY ordering keeps per-run message order.

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
run_signals (
  id           bigserial PRIMARY KEY,
  org_id       uuid NOT NULL,
  run_id       text NOT NULL,
  kind         text NOT NULL,      -- cancel | interrupt | steer | permission
  payload      jsonb,              -- steer text, permission decision, ...
  target       text NOT NULL,      -- executor id owning the run
  created_at   timestamptz NOT NULL,
  acked_at     timestamptz
)
```

Control-pod handler logic (`message` / `interrupt` / `cancel` /
`permissions/{id}`):

1. Local process registry hit? → in-process controller, exactly today's
   path (keeps `TF_ROLE=all` latency identical).
2. Else resolve `runs.executor_id` + executor liveness: live → insert
   signal + `NOTIFY tf_ctl`; owner LISTENs, applies via *its* local
   registry (including resolving `permPending`), acks.
3. No live owner → today's DB-only paths (`MarkCancelledIfActive`, 409
   for steer/interrupt), unchanged semantics.

**Parked/open runs stop being a control-plane special case entirely**:
`ResumeOpenRun` becomes *resume-by-enqueue* — a message to a hibernated
run enqueues its continuation as ordinary claimable work (preferred to
its last executor for the warm worktree, rehydratable anywhere from
S3). This retires the in-process resume goroutine **in every mode,
`TF_ROLE=all` included** — deliberately unlike the signal handlers
above, which do keep a local short-circuit. The rule that separates
them: **operations on live processes short-circuit locally; creation
of work always goes through the queue.** Steer/interrupt/permission
are only meaningful against an in-memory process handle, and their
interactive latency budget is real — the signal layer just routes to
the handle's owner. A resume has no live process; it *mints work*,
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
(`run_messages` + pending-input on the run), then the **same `runs`
row** transitions back to queued and is claimed like any other (its
original enqueue time makes it oldest-first, so wakes claim
promptly); the claiming executor's resume path rehydrates, spawns
with `--resume <session_id>`, and delivers the recorded message as
the turn input. No second runs row, and — keeping the standing
invariant — no new run status.

Acked signal rows are purged after 24 h (audit convenience window);
stale unacked signals expire harmlessly (the reaper owns the run's fate if the
owner died mid-signal).

### 5.3 Trigger relay

`Manager.Trigger(orgID)` (scorer/classifier/profiler/reconciler) and
`PollSoon(source, orgID)` grow a cross-process path: non-leader callers
publish `{trigger}` on `tf_ctl`; the leader routes to the in-process
manager. Callers that need this: config-save handlers on non-leader
control pods, the delegation spawner's classifier wait on executors,
the re-profile button. At `all`, loopback.

### 5.4 Correctness bundle (needed regardless of N)

Small fixes the fleet exposes but that are wrong-by-inspection today:

- `PopForEntity` becomes a claiming pop (`UPDATE ... SET status='draining'
  ... RETURNING` under `SKIP LOCKED`), retiring the process-local
  `drainLocks` as the only serialization.
- The "one active auto-run per entity" gate moves from check-then-act
  to a DB-enforced guard (partial unique index on active auto-runs per
  entity, or the claim above folded into one statement).
- The `became_atomic` suppression gets the same treatment (unique
  backing instead of SELECT-then-decide).
- `system_llm_runs` gains an idempotency key (the `agentproc` TraceID)
  so any overlap window can't double-count spend.
- `entities.poll_seq` CAS (§3).

### 5.5 Deploys and version skew

Heartbeats carry `version`; the dashboard surfaces skew. Rolling rule:
control pods first (they migrate, under the goose lock), then executors
drain-and-replace. Schema changes require drain-first executor deploys
— cheap to enforce because executors refuse to start against a newer
schema than their build.

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
  registry members, keyed `(org, repo)` for delegation and
  `(org, project)` for curator. No state to maintain, any pod computes
  it identically, joins/leaves reshuffle only the affected keys, and
  weights come from `instances.max_runs`. Top-K candidates fall out for
  free (hot-repo bounded replication: the first K candidates all count
  as "preferred", bounding a hot monorepo's cache to K pods on a cost
  dial — no hard ceiling).
- **`placement_overrides` table only for human intent**: manual pins,
  hot-key `replicas=K`, and nothing else (drain lives on the executor
  row). Checked before the hash; expected to stay nearly empty.
- **Explainability closes the gap** that made a routing table tempting:
  a `placement` read endpoint + CLI verb that shows, for any key, the
  computed candidate order and why (weights, overrides, liveness), plus
  a dashboard column. Determinism plus an explainer beats a mutable
  table plus a rebalancer.

### 6.2 Mechanics

- Control stamps `runs.preferred_executor_id` at enqueue (re-stamped on
  requeue — queue dwell is seconds-to-minutes, so membership staleness
  is bounded).
- Claim is two-tier: **(1)** my queued runs (`preferred = me`), **(2)**
  anyone's queued runs older than an aging threshold (~15–30 s) or
  whose preferred executor is dead/gated/draining. Tier 2 is the
  spillover that makes affinity lose to capacity: a saturated or gated
  owner never head-of-line-blocks its shard — work ages briefly, then
  flows to headroom, exactly the TFAC-552 doctrine extended fleet-wide.
- At N=1 the hash always returns self; tier 1 always hits. No-op.

### 6.3 Curator homing

Curator sessions are the one *stateful* placement client (sticky cwd +
shared-RO worktrees per `(org, project)`). A `curator_homes` row maps
`(org, project) → executor`, minted on first turn from the same
rendezvous; turns execute on the home via the ordinary queue with a
hard preference; if the home dies (heartbeat-stale), the next turn
re-homes the row and the new executor re-materializes worktrees through
TFAC-60's seed-on-demand. Re-homing costs a cold blobless clone —
acceptable, rare, and self-healing. The TFAC-60/61 economics (one
shared-RO worktree per (org,repo), bounded per-pod disk budget) are
exactly what affinity preserves at N>1: without it, N pods re-multiply
storage; with it, each key's cache lives on ~1 (or K) pods.

### 6.4 Heterogeneous sandboxes (the TFAC-408 interplay)

The sandbox-fleet spec (`docs/specs/sandbox-fleet/`) makes the sandbox
configurable per org: named **profiles** with an image (rootfs
variant), an egress policy, and a resource class. Two of those
dimensions are placement-inert: egress and rlimits resolve per-run
from the profile at spawn time — any executor enforces any policy,
zero executor state. The image and the memory class are not, and they
are exactly what "counts + memory" fails to capture:

- **Admission becomes budget-based the day two resource classes
  exist.** Today's `max_runs` and the ~512 MB/run rule assume uniform
  runs; a browser profile is several × that. The profile carries a
  `mem_budget_mb`, the run row is stamped with it at enqueue, the
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
  per-run in the proxies, never baked into an image.

None of this is needed while only the "base" profile exists — P1–P3
deliberately assume one variant and a uniform budget. The gate:
before TFAC-408's image or resource dimensions ship on a
multi-executor deployment, land budget admission, the
eligibility-constrained claim, and `instance_variants` with an
eviction budget (the §9 P4 item).

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

---

## 7. Background jobs at N (and the system-sandbox endgame)

Phase 1 keeps all system jobs (scorer/classifier/profiler batches —
Haiku, jailed in multi-mode) **on the leader**: sentinels stay
in-process, `syslimit`'s cap stays globally true because exactly one
process runs them, and nothing about their (already replica-tolerant:
last-writer-wins, delegation-fenced) writes changes. The cost: control
pods keep the privileged container caps to jail Haiku — and in the HA
shape that means *every* control pod, since any of them can become
leader. Settled (§11): the interim is accepted and job-classes stay
P4 — the epic ships as one release, so no intermediate release ever
exposes the interim; the reopening trigger is a fleet customer
requiring an unprivileged or managed-k8s control tier.

The endgame (P4, when wanted): generalize the run queue with a **job
class** so system jobs become claimable work on executors like any
other sandbox. That single move lets control pods drop
`SYS_ADMIN`/runsc entirely (a plain web tier), replaces `syslimit` with
fleet-wide queue accounting (the TFAC-457 cancellation already points
here), and gives system jobs the same fairness/placement machinery as
runs. Not needed for the first fleet.

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
  (§4.1), the runs table's existing timing columns, and one small
  sample table are the entire data model — no parallel pipeline.

### 8.2 Data model

Already present on `runs`: `started_at` (enqueue), `claimed_at`,
`completed_at`, `parked_at`, `duration_ms`, `num_turns`,
`total_cost_usd`, four token columns, `status/outcome/failure_kind`,
`executor_id`, `attempts`. Queue wait = `claimed_at − started_at`.
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
claim rates, headroom trends). A retention reaper trims it.

Prerequisite fix folded in: `hostmem` becomes **cgroup-aware**
(`memory.max`/`memory.current` when containerized) — `/proc/meminfo`
inside a container reports host-wide numbers, which silently mis-gates
admission and mis-reports capacity today.

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
- Per-role health: control keeps `/api/health` + gains readiness (LB
  rotation); executors expose local healthz; both already log
  structured JSON in multi-mode.

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

**P0 — Substrate (all N=1-safe, most N=1-valuable)**
1. Executor registry + heartbeat + persistent identity + boot_epoch;
   stamp real `executor_id`; TFAC-552's gated-flag/headroom follow-up
   lands on the row; TFAC-555 capacity endpoint reads it. (M)
2. Ownership-scoped boot resets + leader-reaper skeleton (at N=1 it
   reaps only stale-epoch self rows — fixes today's restart
   double-execution window). (M)
3. Correctness bundle §5.4: claiming pop, auto-run DB gate,
   became_atomic fence, `system_llm_runs` trace-id, `poll_seq` CAS. (M)
4. `goose.Up` advisory lock; executor schema-version assert. (S)
5. cgroup-aware `hostmem`. (S)

**P1 — The split (first real fleet)**
6. `TF_ROLE` + per-role subsystem wiring + compose profile (N executors
   + LB) + self-host docs. (L)
7. Leader lease + brain gating + standby takeover; trigger/PollSoon
   relay on `tf_ctl`. (L)
8. WS backplane `tf_ws` + `ws_outbox` + presence table + cross-pod
   user-kick. (L)
9. `run_signals` cross-pod control (cancel/interrupt/steer/permission)
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
    aging claim + `placement_overrides` + placement explainer. (M)
12. Curator homes + re-home on death. (M)

**P3 — Fleet operations**
13. `instance_stats` sampler + retention + `/api/fleet` + Fleet UI +
    operator identity. (L)
14. Per-org fairness + per-org concurrency cap in the claim. (M)
15. TFAC-307 inheritances: cred cache, pooler validation + LISTEN conn
    split, agenthost upstream throttle/backoff. (M)
16. Optional `/metrics` exporter. (S)

**P4 — On demand**
17. System-job job-classes on executors → control drops privileges;
    fleet-wide accounting replaces `syslimit`. (L)
18. Per-org brain sharding across control pods (same lease table,
    org-keyed rows). (L)
19. Hot-repo K-replication dial; heterogeneous-profile support per
    §6.4 — budget admission, eligibility-constrained claim,
    `instance_variants` + rootfs-cache eviction budget, pool labels
    (TFAC-408 §§4–5). (L)
20. Budgeted/resumable poll cycles (poll-bench follow-up). (M)

---

## 10. Non-goals

- Kubernetes operators, service meshes, external brokers (NATS/Redis/
  SQS) — Postgres carries the coordination load at every realistic
  fleet size this product targets; the recorded rationale and the
  per-channel crossover points are §5.0. Revisit only with evidence.
- Cross-region fleets, geo-placement.
- Autoscaling automation (operators add/drain machines; the dashboard
  tells them when).
- Per-sandbox scale-out (the executor is the unit, §2.2).
- Changing local mode in any way.

## 11. Decision log

Every question this spec originally left open was settled with the
epic owner on 2026-07-07. Reopening conditions noted per entry.

1. **EE boundary** — console EE, operability core, with the
   deployment-scope entitlement rule (§8.4). Reopens only with a
   packaging-strategy change.
2. **Operator identity** — CLI-managed user flag,
   `triagefactory operator add|remove|list <email>`, recorded in the
   access-change log (§8.3).
3. **Privileged control pods** — interim accepted; job-classes stay
   P4. The epic ships as one release, so no intermediate release
   exposes the interim. Reopens if a fleet customer requires an
   unprivileged / managed-k8s control tier (§7).
4. **Executor-loss retry** — `TF_RUN_MAX_ATTEMPTS` default 2;
   duplicate external writes on retry are accepted (upsert semantics
   absorb them; `failure_kind='executor_lost'` audits the rest); no
   compensation logic. Retried token spend is bounded by the attempt
   cap and the org daily cost cap. Reopens only on evidence from a
   real fleet (§4.3).
5. **Fabric knobs** — defaults, all env-tunable: NOTIFY inline
   payload ≤ 6 KB, larger via outbox ref; `ws_outbox` TTL 60 s; acked
   `run_signals` purged after 24 h; registry tombstone GC at 7 days
   heartbeat-stale. Tuning under real load is routine ops, not open
   design (§5).
6. **TFAC-408 image keying** — two-layer, name → recipe → content
   hash; `instance_variants` keys on the hash (§6.4).
7. **Resume path at `TF_ROLE=all`** — always resume-by-enqueue; no
   in-process resume variant survives in any mode. The local
   short-circuit exists only for operations on live processes
   (steer/interrupt/cancel-live/permission), never for work creation
   (§5.2).

## Related

- `docs/specs/sandbox-fleet/` — sandbox profiles; §5's "profile as the
  scheduling unit" plugs into §6's placement labels (P4).
- `docs/sandbox-bench.md`, `docs/poll-bench.md` — the capacity numbers
  behind §0.
- `docs/isolation-tiers.md` — the tier ladder; this spec changes the
  *deployment* shape, never the org isolation model.
- `docs/ee-feature-packaging.md` — the module shape for §8.4.
- `docs/self-host-setup.md` — gains the N-executor compose profile in
  P1.
