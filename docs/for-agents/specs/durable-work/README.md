# The Durable Work Contract

**Status: Draft.** The scope is agreed: internal durability is included; external-effect durability
is separate work. Approve the contract before creating the P0/P1 implementation tickets.

TF must remember unfinished work after a crash and prevent an old worker from changing work that
another worker now owns. Today, its queues and claim lifecycle solve these problems separately.
This design gives them shared recovery rules, implemented in `internal/db/workitem` and adopted
one table at a time.

**Adoption scope** (see §7.2 for sequencing):

- **Event routing:** `event_queue` adopts the shared work-item lifecycle (P1).
- **Deferred trigger firings:** `pending_firings` adopts the same lifecycle (P2).
- **Score-driven task re-evaluation:** a new `task_rederive_queue` makes this follow-on work
  durable and safe when scores change during evaluation (§4, P3).
- **Executor claims:** an adapter applies shared leases, ownership checks, and renewal
  to `claims`, with dispatcher takeover and stop/cancel settlement (D2–D4, P4).
- **Internal handoffs:** discovery seeding, pre-fire owner consolidation, and blueprint creation
  with its first step adopt the same-transaction obligation rule (§2).
- **Entity repair:** reactivation and closing use version-checked writes; reconciliation becomes
  an invariant check, subject to O3 (D1, P5).

The first three use the full work-item contract. Claims retain their own lifecycle; internal
handoffs and entity repair adopt the relevant transaction and fencing rules.

The central rule is simple: **when a database change creates an obligation, save that obligation
in the same transaction.** A worker can then find it, take temporary ownership, and either finish
it, retry it later, or leave it visibly parked for an operator.

Work may run more than once. Each work kind must therefore make repeated execution safe for its
internal database state. This guarantee does not extend to external actions such as posting a
GitHub comment. Section 3 defines that boundary.

The design uses database job queues, transactional outbox/inbox records, and fencing tokens
(numbered ownership receipts). River, Oban, and SQS visibility timeouts are reference patterns;
this work adds no queue service or orchestration dependency.

Each kind keeps its own typed table, foreign keys, stores, and conformance tests. There is no
shared polymorphic `jobs` table and no expansion of event sourcing. Local mode keeps its stated
single-instance exemptions. `conversation_signals` keeps its existing directed, acknowledged
notification contract and is not converted to a work-item queue.

## 1. Shared work-item rules

A **work item** is a database row recording an obligation. A **lease** gives one worker temporary
ownership of that row. Each acquisition returns a **receipt** containing a new generation number;
subsequent writes must present that receipt. This prevents a delayed worker from writing after
another worker has taken over, even if both workers have the same instance identity.

### 1.1 Shared columns

Every adopting table includes these columns alongside its typed payload and foreign keys.

| Column | Meaning |
|---|---|
| `status` | `ready \| leased \| done \| parked \| cancelled` |
| `attempt` | Charged execution attempts, including the current claim. Claim increments it; a verified non-failure deferral returns that charge (§1.6). |
| `max_attempts` | Attempt budget, copied from the kind's policy when the row is inserted. |
| `next_attempt_at` | Earliest retry time. NULL means eligible now. A future timestamp lets other items proceed. |
| `lease_generation` | Ownership generation. Increases on every acquisition, including reacquisition by the same owner in the same boot. |
| `lease_owner`, `lease_epoch` | Ownership provenance for display, debugging, and startup recovery. The generation is the write fence. |
| `leased_at`, `lease_expires_at` | Lease start and expiry. The lease can be renewed (§1.6). |
| `cancel_requested_at`, `cancel_requested_by`, `cancel_reason` | Cancellation intent: when it was asked for, by whom, and why. Setting them does not change status (§1.7), and whatever was set survives onto the terminal record. An operator control that cancels outright rather than requesting it — supersede (§1.4) — records the actor alone: there is no request to time, and `last_outcome` already says why. |
| `last_error`, `last_outcome` | Most recent attempt's error and typed outcome. |
| `unique_key` | Prevents duplicate admission (§1.4). |
| `superseded_by` | The row that replaced this one, recorded when an operator supersedes parked work (§1.4). |
| `first_enqueued_at` | Original enqueue time, preserved when an operator retries parked work. |
| `org_id`, `created_at`, `done_at` | Organization and lifecycle timestamps. |

### 1.2 Claiming and recovery

The normal path is `ready → leased → done`. A leased item can also:

- Return to `ready` after failure, with a retry time and a remaining attempt budget.
- Return to `ready` for expected waiting, refunding that attempt's charge (§1.6).
- Become `parked` because it exhausted its budget, appears to be poison work, or failed permanently.
- Become `cancelled` when a cancellation request is settled.

An operator can redrive a parked item to `ready`, or supersede it to `cancelled` (§1.4).
An expired `leased` item is claimable directly, without a separate reset or sweeper.

The Postgres claim query selects:

```sql
(status = 'ready' AND ripe)
OR (status = 'ready' AND cancel_requested_at IS NOT NULL)
OR (status = 'leased' AND lease_expires_at <= now())
```

Here, `ripe` means `next_attempt_at` is NULL or has arrived. The second arm exists because a
cancellation request must not wait for a deferred item's retry time: a `ready` item with a future
`next_attempt_at` has no holder to observe the request, and without this arm it would keep its
`unique_key` reserved and count toward ready depth until it ripened. Selection uses
`ORDER BY id FOR UPDATE SKIP LOCKED LIMIT n`, with optional per-org interleaving (§1.6).
Settling a selected row does not consume the worker's execution capacity; the claimer continues
to the next row. For each selected row, the claimer:

1. Settles a cancellation request if present. Cancellation takes precedence over the budget.
2. Otherwise parks the item if its attempt budget is already exhausted.
3. Otherwise increments `attempt` and `lease_generation`, stamps lease ownership and times, and
   returns the item to the worker.

Keep one implementation of each disposition. Claimer settlement is a locked acquisition path;
it does not authorize an expired holder to settle its own item.

Every acquisition returns a receipt:

```text
{item_id, lease_generation, lease_expires_at, attempt, unique_key}
```

A kind can add fields frozen at claim time, such as `processing_revision` (§4). The receipt is
the holder's authority for disposition writes and renewal. A stale generation matches no rows.
Recovery becomes possible on the next claim attempt; it does not depend on a cleanup worker.

SQLite uses `BEGIN IMMEDIATE` with an update-returning statement instead of `SKIP LOCKED`.
Its single-instance model permits this; generation behavior and conformance tests are identical.

No startup reset is required. An optional early handback can move rows matching
`lease_owner=me AND lease_epoch<myEpoch` to `ready` and increment their generation. If work can
leave surviving processes, startup must first confirm their teardown. D3 defines this ordering
for claims and their sandbox cells.

### 1.3 Checking ownership on every write

Every holder operation — `complete`, `park`, `cancel-settle`, `requeue`, `defer`, and `renew` —
requires this guard:

```sql
WHERE id = $1
  AND status = 'leased'
  AND lease_generation = $gen
  AND lease_expires_at > now()
```

Zero matching rows returns `ErrLeaseLost`. The worker stops, makes no further writes for the
item, and logs at info level. Expiry ends authority even if no successor has claimed yet.
Disposition belongs to the next claimer.

For expiry guards, `now()` means fresh database time at the guard, not the start time of a long
transaction. In Postgres, use `clock_timestamp()`. Validate ownership after acquiring the row lock.

- **`SingleTx`:** check again at the final disposition write. Failure rolls back the transaction's
  domain mutations as well as the disposition.
- **`FencedReplay`:** every constituent transaction checks the live receipt under lock, in addition
  to its domain replay fence.
- **Claims adapter:** apply the same generation and expiry rule to claim-owned writes.

### 1.4 Preventing duplicate admission

Each kind chooses how long its `unique_key` remains reserved:

- **`UniqueForever`:** the key is unique across all rows. Retain terminal rows, or preserve their
  keys in a tombstone ledger when pruning them.
- **`UniqueWhileUnsettled`:** a partial unique index covers
  `WHERE status IN ('ready','leased','parked')`. Parked work continues to reserve its key.

For a parked item, an operator must either:

- **Redrive** the same row with a fresh budget and incremented generation, preserving its history.
- **Supersede** it: set `cancelled` and record `superseded_by`, allowing replacement work.

Insert helpers return `(id, deduplicated)`. Existing domain fences remain authoritative;
`unique_key` complements them. Preventing duplicate rows does not make repeated execution safe.
That requires the next rule.

### 1.5 Making repeated execution safe

Every work kind declares one of two strategies:

- **`SingleTx`:** commit the domain mutation and item completion together through
  `Complete(ctx, receipt, func(tx) error)`.
- **`FencedReplay`:** use multiple transactions, with a named domain fence making each constituent
  write safe to repeat.

These strategies cover database changes. Provider calls and arbitrary agent tools are outside
their guarantee (§3).

For `SingleTx`, `Complete` locks the work-item row first with `FOR UPDATE`, verifies status,
generation, and expiry, then runs the domain writes and records the final disposition. Cancellation
writers touch only `cancel_requested_*`, never lease columns. A cancellation committed before the
lock is observed before domain writes. A cancellation arriving behind a successful completion
finds a terminal row; it cannot cancel completed work retroactively. Test both lock orders.

Each kind must pass crash tests for its strategy. Use barriers to place crashes at these boundaries:

- For `SingleTx`, crash before and after the combined commit. The domain mutation and completion
  must both exist or both be absent.
- For `FencedReplay`, crash between a domain commit and item completion. Reclaim and re-execute;
  there must be no duplicate domain state.

A kind cannot ship until these tests pass.

### 1.6 Timing, retries, and expected waiting

Each kind supplies a policy:

```go
type Policy struct {
    MaxAttempts  int           // default 5
    Lease        time.Duration // default 60s
    RenewEvery   time.Duration // default Lease/3; long units MUST renew
    UnitDeadline time.Duration // ctx timeout per unit; always < Lease
    Backoff      BackoffSpec   // base 5s, cap 5m, ±25% jitter
    Unique       UniqueMode
    Fairness     FairnessSpec  // optional per-org interleaved claim; default FIFO
}
```

**Renewal.** `RenewLease(receipt)` requires the current generation and an unexpired lease, using
database time. It sets `lease_expires_at = now() + policy.Lease`; it never extends the old timestamp.
A late renewal returns `ErrLeaseLost`, even if no successor exists. The renewal RPC has its own
deadline. Where a worker has a self-fence watchdog, that timer runs independently on its local
monotonic clock (D3).

**Failure.** `Requeue(receipt, outcome, err)` accepts these typed outcomes:
`transient | dependency_down | poison_suspected | deadline | permanent`.
A `permanent` outcome parks immediately, regardless of remaining budget. Retrying the same
permanent validation failure or rejection would only consume more attempts. Parking reasons and
metrics use the typed outcome.

**Expected waiting.** Check known prerequisites before claiming where possible. For example, a
pending firing waits for the task's active run to finish. If a prerequisite race is discovered
after claim, or a newer score invalidates an evaluation, call
`Defer(receipt, reason, nextAttemptAt)` instead of `Requeue`.

In one fenced transaction, `Defer`:

1. Verifies the kind's declared deferral predicate without committing any domain mutation for
   that attempt. Cancellation takes precedence if requested.
2. Returns the current attempt's charge with `attempt-1`.
3. Returns the row to `ready` and sets a future `next_attempt_at`.

Generation never decreases; the next claim gets a new receipt. A database error, expired lease,
or unknown outcome cannot justify deferral and gets no refund. Repeated healthy waiting
cannot exhaust the failure budget. Deferred depth, age, and counts still reveal stuck prerequisites.

### 1.7 Cancellation

An authorized actor can set `cancel_requested_at/by` on a `ready` or `leased` row. This records a
request, without changing status.

A claimer settles a requested `ready` item as `cancelled` on its next pass, whether or not the
item's `next_attempt_at` has arrived (§1.2). A live lease holder observes the request at its next
fenced write or renewal and settles it. The terminal record retains who
requested cancellation and why.

### 1.8 Access, metrics, and operator controls

**Background workers** use admin-pool `...System` methods, with `org_id` bound by argument.
These methods are not request paths. Workers requiring the background-brain lease recheck its
term between batches.

**Operator controls** are request paths. Every kind declares one access policy:

- Org-admin-only, following `workHandler`: admin-pool access behind `RequireOrgAdminRole`,
  with no RLS backstop. Document and test that responsibility explicitly.
- A named visibility join for team- or user-scoped rows.

A kind reachable on the app pool must carry a policy set admitting `UPDATE`, not `INSERT` alone:
every ownership-guarded write is an `UPDATE`, and so is the conflict arm of admission. An
append-only set does not refuse the queue outright — the guarded writes match no rows, so it
drains nothing and reads as permanently empty.

Each kind must preserve its authorization rules through retention and parent deletion, and ship
handler authorization tests alongside its Postgres tests.

The shared package reports every disposition to a `Kind.Observer`; `internal/workmetrics` turns
those into the metrics below:

- **Gauges:** ready depth, leased count, parked count, oldest-ready age, deferred depth and age.
- **Counters:** claims, completions, parks by reason, deferrals, requeues by typed outcome, and
  expired-lease reclaims. Reclaims are a signal of interrupted work.

Each kind declares an oldest-ready-age objective and a steady-state target of zero parked items,
with alerts when those objectives are breached. Monitoring must distinguish an idle queue from
one that has stopped draining.

The parked-work panel (`/api/orgs/{org_id}/work`, the Parked work section of org settings) shows
parked work across every registered kind, with accurate total counts and per-row redrive and
supersede controls.

### 1.9 Required conformance tests

`internal/db/workitemtest` runs against both dialects for every adopting table. It covers:

- Lifecycle transitions and crash tests for the declared execution strategy.
- Reacquisition by the same owner, rejecting its old receipt.
- Takeover during execution, rejecting the previous worker's later writes.
- Every holder operation after expiry, including before a successor exists.
- Expiry while waiting for a row lock or inside a `SingleTx` closure.
- Parked-key uniqueness and redrive/supersede conflicts.
- Cancellation races and `SingleTx` lock ordering.
- Cancellation of a deferred `ready` item settles on the next claim pass, before its retry time.
- Repeated expected deferral without budget exhaustion; stale deferral cannot refund twice.
- Real failures and crashes consuming the attempt budget.
- Backoff monotonicity, per-org fairness, and metric-query correctness.

### 1.10 Package and table responsibilities

**A shared developer experience is part of the contract.** Every adopting work kind uses the
same lifecycle API and outcome types, metrics, operator controls, and conformance suite. Adding
a kind supplies its typed storage, business logic, and explicit policies; it must not require
another queue framework. Kind-specific wake sources, transaction boundaries, and authorization
remain explicit as described below and in §1.8 and §1.11.

`internal/db/workitem` owns:

- SQL builders for claim, renewal, disposition, requeue, and deferral. Builders use only the table
  name and shared columns; they never read or write kind-specific columns.
- The receipt type, policy enforcement, metric queries, and conformance suite.

Each adopting table owns:

- Its schema, payload, foreign keys, and domain reads/writes inside `Complete` closures.
- Insert statements that compose the package's admission helper.
- Required indexes: `(next_attempt_at, id) WHERE status='ready'`,
  `(id) WHERE status='ready' AND cancel_requested_at IS NOT NULL`,
  `(lease_expires_at) WHERE status='leased'`, and its chosen uniqueness index.

The package documents the required indexes; conformance tests assert they exist. Adopting
packages must use the shared SQL builders rather than reimplementing their lifecycle operations.

### 1.11 Adoption checklist

Each worker's package documentation and implementation ticket must name:

- Its owner: background-brain leader loop, executor dispatcher, or request path.
- Its wake source and lease-term checks between batches.
- Its database pool and access policy (§1.8).
- Its required partial indexes.
- Its Postgres tests, including a former leader or delayed worker attempting stale writes.

## 2. Save follow-on work atomically

> Any transaction whose state change implies internal follow-on work writes the work row — or
> an inbox receipt — in the same transaction.

A periodic sweep should check this invariant, not be the mechanism that delivers the work.
Any best-effort callback must name its recovery path or become a durable obligation.

Preserve the paths that already satisfy this rule: `events` with `event_queue`, atomic snapshot
CAS and enqueue (TFAC-777), close-transaction intent (TFAC-779), and recording the task's agent
claim with its engagement (TFAC-775).

Apply the rule to these remaining paths:

- **Discovery seed:** include the backfill obligation in the seed's compare-and-swap (CAS)
  transaction.
- **Pre-fire owner consolidation:** include it in the firing transaction, or abort the firing.
- **Score update:** enqueue re-evaluation with the score commit (§4).
- **Blueprint creation:** create the run and enqueue its first step together. The TFAC-763 sweep
  remains as an invariant checker.

## 3. External actions are separate work

This implementation adds no external-action journal (`effect_receipts`), reconciliation queue,
durable provider-operation IDs, provider-specific probes or retry policies, or recovery hold on
agent writes. The shared package has no external-effect strategy or unclaimed-backstop discharge
API to prepare for that work.

This boundary applies to all external channels: GitHub/Jira through `tfac exec`, `git push`, real
`gh` through the credential injector, EE integrations such as Slack, arbitrary allowed egress,
and future third-party tools.

Existing behavior, audit records, and provider protections remain in place. After a crash, an
external result may still be unknown, and reissuing the action may produce a duplicate. This is
an accepted limit; it does not block internal durability work or new integrations.

Jails retain their current credential and network restrictions. Agents and sidecars gain no
extra access to verify provider state. Internal ownership checks and cell teardown remain
required. Transcript repair continues to report interrupted tool results as unknown without
blindly replaying the calls. These controls do not guarantee external-effect deduplication.

## 4. Re-evaluate tasks against the correct score revision

A score can change while a worker is evaluating it. The worker must not mark the newer score's
work complete on the strength of an older evaluation.

Add `tasks.score_revision BIGINT` and increment it in the same `UpdateTaskScores` statement that
writes the scores. Add `task_rederive_queue`, with a task foreign key and `UniqueWhileUnsettled`.

**Coverage is required:** every path that persists task scoring results must use this same
operation, including any manually initiated re-score. Saving results, incrementing the revision,
and recording the re-evaluation obligation commit together. Requesting a re-score does not itself
advance the revision; saving its results does. Before implementation, enumerate all score writers
and prove their coverage in both dialects. A future writer must satisfy the same requirement.

The queue tracks two revisions:

- **`requested_revision`:** the latest requested score revision, stored on the queue row. The
  score transaction raises it through an upsert, even if the item is currently leased. It never
  decreases.
- **`processing_revision`:** the revision frozen into the receipt when the worker claims the
  item. It cannot change within that generation.

Completion uses `SingleTx`. `Complete` locks the queue row and hands the closure the row as
locked. Before committing evaluation effects, compare `receipt.processing_revision` with that
locked row's `requested_revision`:

- If equal, no score has landed since the claim. Commit the evaluation and complete the item.
- If the row's value is higher, commit no evaluation effects. Defer under the same receipt fence
  (§1.6), keeping the higher `requested_revision` and refunding the current attempt charge.

Why this is race-safe: every score writer raises `requested_revision` on this same row, in the
same transaction as the score, so the write takes the row lock the completor already holds. A
concurrent score write therefore either committed before the lock (the completor sees the raised
value and defers) or waits behind it (the completor completes, and the writer's upsert then finds
a `done` row and inserts a fresh `ready` one for the new revision). The queue row is the
serialization point; `tasks.score_revision` is not. Do not compare against `tasks.score_revision`
read through any other path: a read taken before the row lock, or over a snapshot older than it,
can see the old revision and mark the newer obligation done.

Always compare the frozen receipt value. Comparing two mutable row values could mistake a newer
request for an evaluation that already happened. Drop `rederive_owed`.

**Lock order in the score transaction:** upsert every affected `task_rederive_queue` row before
writing any `tasks` row. The completor locks its queue row first and may write the task inside its
closure; a score writer that locked `tasks` first and then waited on the queue row would deadlock
with it. Both transactions taking the queue row first removes the cycle.

Barrier tests cover both orders of the leased-row upsert race: the new score commits before the
completion lock, and it commits after that lock. Tests also cover the score transaction's lock
order against a completor writing its task, every score-result write path, including a manual
re-score if that path exists, and prove that revision and obligation cannot be omitted or
committed separately from the scores.

## 5. Repairs, stops, and executor claims

### D1. Fence repair writes

A repair must use compare-and-swap against a version that the live path advances. For entities,
that version is `poll_seq`.

Reactivation writes the fresh snapshot and changes state in one transaction. Closing an entity
also needs a CAS guard.

O3 proposes moving terminal-state enforcement into tracker Phase 3, the part of each poll that
compares and records state. The check runs even when the snapshot has not changed: if a PR is
already merged but TF still has active work for it, that poll performs the missing cleanup or
records its durable obligation. It must not depend on another merged/closed transition event.
Use the same close semantics and version guards as the normal path, preserving task closure,
audit records, and stop/cancel intent. This does not synthesize historical events or new tasks.

This change can ship independently of the shared queue framework. Before removing the periodic
repair sweep, tests must cover pre-existing inconsistencies, unchanged snapshots, reopening
races, failed/partial cleanup, and records skipped or no longer reached by polling. Specify how
those records are handled without changing source-pause or untracking behavior. Keep the repair
path until that coverage is demonstrated; afterward retain a read-only checker that counts and
alerts on violations. O3 remains the choice to adopt this approach.

### D2. Record stop intent before settling the run

A request path writes `conversations.stop_requested` and its durable stop signal in one
transaction. It does not park the conversation itself.

The live holder, or the dispatcher handling expired/unclaimed work, parks the conversation
`open` and releases any claim in one transaction. Execution cannot be claimed while stop is
requested. User follow-up clears the request to re-arm the conversation. A plain stop leaves the
blueprint unchanged. Cancellation uses the same request/disposition separation (§1.7).

### D3. Give claims leases and takeover

Claims use an explicit adapter. They adopt:

- `lease_expires_at` and `lease_generation`.
- Strict renewal (§1.6), included in every fenced write and on a `RenewEvery` ticker.

Claims do not adopt work-item `status`, `unique_key`, or per-row attempt accounting. Their
existing loss-episode accounting remains; D4 defines the outcomes.

#### Timing

Validate this ordering at startup:

```text
renew_interval < self_fence_deadline < takeover_after
```

For claims, `policy.Lease = takeover_after`. Acquisition and successful renewal set
`lease_expires_at = database_now + takeover_after`. Takeover is eligible at that expiry, with no
additional delay.

Initial defaults: renewal every 20s, self-fence after 45s, and claim lease/takeover after 75s.
Validate these values in failure tests; configuration must preserve the ordering above.
Ordinary work items keep their 60s default lease. P4 in §7.2 records how these values differ
from the current reaper defaults and why.

If renewal succeeds at database time T and none succeeds afterward, the worker aims to
self-fence by roughly T+45s. A successor can take over at T+75s.

The watchdog uses the local monotonic clock, anchored conservatively to the request start of the
last successful acquisition or renewal. Network delay must not extend that bound. Renewal calls
have deadlines; the watchdog runs independently; a late response cannot revive a fenced claim.
Self-fencing stops new claims, refuses writes and egress, and kills the sidecar and sandbox.

A paused process can miss its cleanup deadline. Database generation and expiry checks must still
reject its writes. Test a paused holder returning after takeover. External requests already in
flight remain outside this contract (§3).

#### Run health

Healthy runs have no automatic total-duration limit. Claim age alone must not stop a run; this
contract adds no `deadline_at` or `TF_RUN_DEADLINE`. Preserve existing operation-specific timeouts
and explicit stop/cancel controls. The work-item `UnitDeadline` and renewal RPC deadlines bound
individual operations, not the lifetime of an agent conversation.

Lease renewal proves ownership and database connectivity, not agent progress. An executor can
keep renewing while its agent or tool is stuck, and ongoing activity does not prove useful
progress. O4 must define what detects and handles a stalled but renewing run, accounting for both
SDK and native runtimes, long-running tools, and deliberate waits. Do not treat renewal alone as
a complete health signal or substitute a fixed maximum run age for that decision. Any resulting
system stop uses D2's durable intent, kill signal, and parking the conversation `open`.

#### Takeover transaction

Remove `internal/reaper`. The dispatcher reclaims expired claims as part of acquiring work.
In `needsDrivingSQL`, an unreleased claim stops counting as an active driver when
`lease_expires_at <= now()` on database time.

The claim transaction applies these rules in order:

1. **Blueprint cancellation requested:** park the conversation `open` with `system_cancelled`,
   mark the blueprint `cancelled`, and release the claim as `reaped`. Do not create a successor.
2. **Plain stop requested:** park the conversation `open`, release the claim, and leave the
   blueprint unchanged. Do not create a successor. Apply the same stop settlement to queued
   conversations with no claim.
3. **Loss episode at or above `TF_MAX_CLAIM_ATTEMPTS`:** fail with `executor_lost`, mark the
   blueprint `failed`, stamp end times, and release the claim as `reaped`. No successor.
4. **Otherwise:** release the expired claim as `reaped`, increment its generation, and create
   the successor in the same transaction. Ignore `preferred_executor_id`; the expired lease
   no longer owns the work.

`idx_claims_one_active` remains the mutual-exclusion constraint. Release the old claim in a
statement before inserting the new one, not in a sibling CTE: a uniqueness check in the same
statement can still see the old active tuple.

Settlement runs on every dispatcher tick, even with no execution capacity. Starting a successor
requires a slot. Run settlement before capacity and memory gates; acquire only available
execution slots and return to the next tick when full. Never block the tick on the execution
semaphore. Test full capacity, memory-gated dispatch, and queued stop intent.

This places work only where capacity exists. Implement the same rules in SQLite's
`ClaimNextConversation`, so local mode can recover during operation as well as at startup.

#### Display and monitoring

Derive the display's `running` state from lease expiry, not just `released_at`. A dead engagement
must appear as awaiting takeover.

Expose a database-derived gauge of active claims past expiry and alert on their age. Collect it
outside the dispatcher loop so a missing or stuck dispatcher cannot suppress its own alarm.

#### Other cleanup responsibilities

Removing the reaper requires assigning each of its other jobs:

- **Terminal conversations with unreleased claims:** prevent this inconsistent state through D2.
  Request paths cannot update claims (`tf_app` has no claims UPDATE grant), so they write intent
  only. The holder and dispatcher are the only settlement writers; each releases the claim in
  the same transaction as the conversation status change. The startup checker counts and alerts
  on violations without repairing them. Before implementation, enumerate every app-pool terminal
  writer and convert it to an intent write.
- **Blueprint runs created without a first conversation:** prevent this through the atomic
  creation rule (§2). Keep the startup invariant checker; delete the periodic repair sweep.
- **Stale instance records:** move garbage collection to `internal/instance`, in a daily loop
  gated by the background-brain lease. Continue deleting instances heartbeat-stale for over 7d.

Retire `TF_REAPER_STALE_SEC`; its replacement uses claim vocabulary and the `takeover_after`
meaning above. Keep `TF_SELF_FENCE_SEC` as the instance-wide fence: prolonged heartbeat-write
failure kills every cell and stops claiming. Per-claim renewal failure is a finer-grained fence.
Both self-fence deadlines must be strictly below takeover. Keep `TF_MAX_CLAIM_ATTEMPTS` as the
loss-episode budget.

The instance heartbeat remains responsible for registry garbage collection, fleet display, and
placement's dead-preferred-executor check. It no longer determines claim liveness.

#### Startup ordering

The instance-ID file lock (`flock`) proves the preceding process with that ID has exited. It
does not prove its jails and sidecars have exited.

Complete predecessor cell teardown before early claim release or handback. If teardown cannot
be confirmed, withhold the fast path and use ordinary lease expiry. Waiting alone does not prove
an external action absent (§3).

Test an old executor crashing and an attempt to reuse the same state root concurrently. Assert
that the file lock excludes the latter.

#### Claim migration

No multi-mode deployment has shipped. Until a persistent one exists, add lease columns directly
to the Postgres baseline; there is no older executor population to support.

SQLite has installed databases and needs a forward migration. Terminal claims keep NULL lease
columns as historical records. A live claim surviving an upgrade belongs to the previous boot
and is handled by startup self-recovery.

Recheck deployment reality before this phase begins. If a persistent multi deployment exists,
use the staged upgrade rules in §7.1. The transition must support legacy NULL leases, require
both lease expiry and a stale heartbeat while old executors remain, and verify that no active
claims still require legacy handling before retiring it.

### D4. Count loss episodes consistently

Work items use the attempt rules in §1.2 and §1.6. Claims keep episode-based accounting:
only `reaped` counts toward the takeover loss budget. `requeued_credentials` and `requeued_boot`
are separate outcomes and do not extend a loss episode.

Ship those outcome changes with the claim migration. The deployment rules in D3 determine
whether that migration needs a staged cutover.

## 6. Existing guarantees to preserve

- Domain replay fences remain authoritative.
- Whether a conversation needs driving remains derived from queue state.
- The eventbus and `tf_ctl` remain intentionally lossy notifications.
- Snapshot CAS still precedes publication.
- `external_actions` remains append-only.
- Local-mode exemptions must be stated per surface, not silently broadened.
- Existing security controls remain in force. External-effect durability is separate (§3).

## 7. Adoption plan

### 7.1 Migration rules

Plan schema changes for both dialects. SQLite requires forward migrations. Postgres can still
change its baseline while no persistent multi deployment exists. Recheck that condition before
every phase.

Once a persistent multi deployment exists, upgrades must proceed through compatible expansion,
dual reads, backfill, switching to the new representation, and removal of the old representation.
Include upgrade tests from the previous head. Do not change stored vocabularies in one release
while older workers still run. P4 follows the claim-specific conditions in D3.

### 7.2 Implementation phases

**Start with P0 and P1 together.** Build the smallest shared contract against the existing event
queue, prove crash recovery and upgrades, then migrate the remaining consumers. The event queue
already has transactional admission, replay fences, and recovery. Preserve them while adopting
the shared mechanism. Provider journals and generic external-effect adapters are not part of P0.

#### P0 — Shared package

Build `workitem`, `workitemtest`, metrics and objective alerts, and the shared parked-work controls
with per-kind authorization (§1.8). The lifecycle, expiry, deferral, and crash tests in §1.9 must
pass before any consumer ships.

#### P1 — Event queue

Migrate `event_queue` with these explicit mappings:

- `pending → ready`.
- `failed → parked`, preserving attempts and `last_error`.
- `processing → leased`, with an already-expired synthetic lease:
  `lease_generation=1`, `lease_expires_at=now()-1s`, and owner copied from `executor_id`.
  Normal claiming then recovers these rows without a special recovery path.

List every CHECK constraint and index replacement in the migration. Migrations run before
workers: the local binary runs goose before its drain worker, and multi mode migrates before
starting workers. State this requirement in the ticket and assert it in the runbook.

There is no legacy-worker kill switch. The vocabulary change is incompatible with the old worker,
so rollback requires a forward fix. Retaining an old worker behind a flag would also require a
schema adapter. With no persistent multi deployment, worker and schema ship together; if that
changes, §7.1 applies.

The worker uses `FencedReplay` and names its three domain fences. Add per-org fairness and
`UnitDeadline`; remove the sweeper and startup reset. Move the TFAC-780 panel to `parked` and fix
the sibling-close `prepare` error path as part of the rewrite.

#### P2 — Pending firings

Migrate `pending_firings`: `pending → ready`; `draining → leased` with the same synthetic expired
lease as P1. Add lease columns and rebuild the active partial unique index as
`UniqueWhileUnsettled`, including parked rows. Delete `RequeueStaleDraining` and document the
task-transition transaction required by §1.5.

Keep `pending_firings` and `event_queue` separate for this implementation. Revisit consolidation
only after both have operated under the shared framework and experience shows a concrete benefit.
Consolidation is not an adoption gate.

#### P3 — Score re-evaluation

Implement §4 with `SingleTx`, coverage of every score-result writer, and barrier tests for both
orders of the score-revision race.

#### P4 — Claims

Implement D3/D4: leases, renewal, dispatcher takeover in both dialects, reaper removal,
all reassigned cleanup jobs, and startup ordering. Include settlement under full capacity and
independent stale-claim monitoring. Resolve O4's stalled-run health policy; test that healthy
renewing runs are not stopped because of their age. This addresses claim ownership, recovery,
and durable stop intent without adding a blanket run-duration limit.

**Timing change, stated explicitly.** The takeover defaults are a deliberate step up from the
current multi-mode reaper defaults (self-fence 15s, stale-reap 30s, on a 4s instance heartbeat)
to self-fence 45s and takeover 75s on a 20s renewal. The old numbers priced a per-instance
heartbeat; the new ones price a per-claim renewal that also rides every fenced write. They also
weigh a false takeover of an agent run (sandbox killed, workspace re-cloned, transcript replayed,
visible to the user) as far costlier than an extra 45s before a genuinely dead run is retaken.
No deployment slows down: no multi-mode deployment has shipped, and local mode has no takeover
at all until restart today, so it goes from never to 75s. Revisit the values with the failure
tests D3 requires.

#### P5 — Entity repair

Implement D1: atomic snapshot updates on reactivation, CAS on close, and a counting checker for
the reconciler, subject to O3 and its repair-coverage gate. This phase is independently shippable;
its position in this list does not make it depend on P0–P4.

**Dependencies:** TFAC-773, TFAC-774, and TFAC-775 land first. P1 subsumes the mechanisms in
TFAC-761 and TFAC-765; coordinate those changes rather than developing competing implementations.

### 7.3 Future applicability check

Before P0 is finalized, check the contract against a future webhook ingress path:
verified delivery → inbox row → contract worker. The inbox uses `UniqueForever` on the provider's
delivery ID and retains tombstones. This is a design check, not an implementation phase here.

## 8. Open decisions

- **O3 — Entity reconciliation (decided):** enforcement moved into tracker Phase 3 and the
  repair sweep became a counting checker. The shape chosen: the poll records a close obligation
  (`system:entity:close_owed`) when it observes a terminal snapshot on an active entity with no
  close in flight, and the router closes from it under the same version-guarded transaction a
  real terminating transition takes; the tracker never closes inline. D1 defines what each poll
  must enforce; the coverage gate it named is what the checker's read-only posture rests on.
- **O4 — Stalled-run health:** which operation bounds or progress signals should detect and
  handle a stalled agent whose executor still renews its lease? Healthy runs have no total-duration
  cap. D3 sets initial lease timings and distinguishes ownership from progress; P4 must resolve
  the remaining health policy for both runtimes, including long tools and deliberate waits.
