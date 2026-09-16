# The Durable Work Contract — design of record (DRAFT, rev 3.3)

**Status:** draft rev 3.3, 2026-09-16. Rev 1 reviewed (12 blockers, 8 hardening — all incorporated
in rev 2, one partial dissent on M8). Rev 2 reviewed (9 blockers R2-B1–B9, 4 clarifications
R2-H1–H4 — all incorporated in rev 3). Rev 3.1: the claims mixed-version cutover is collapsed per
deployment reality — no multi-mode deployment has ever shipped, so there is no rolling upgrade to
survive (user call, 2026-08-19). Rev 3.2: **`internal/reaper` is deleted** — takeover is a claim,
not a sweep (user call, 2026-09-16); its four chores are rehomed in D3. Rev 3.3: **external-effect
durability is separate work**, outside this implementation (user call, 2026-09-16). This revision
also makes lease expiry authoritative for every holder write, defines a single takeover clock,
and separates expected deferral from failed attempts. The scope decision is settled; the remaining
contract stays draft. P0/P1 tickets may be cut once the contract is approved.

**Problem statement.** The 2026-08 durability audits and the 15-ticket hardening wave showed one
root cause behind ~25 findings: **work that exists only as a side effect of a function call rather
than as a row with a lifecycle.** Every queue-shaped surface re-implements the same lifecycle
locally and each copy shipped its own bug set. This spec replaces the approximations with one
contract, implemented once, adopted table by table. The prize is that the *next* obligation costs a
small typed table that inherits retry, backoff, leases, fencing, recovery, parking, idempotency, an
operator surface, and metrics — instead of re-deriving all of them and re-shipping their bugs.

**Industry grounding** (patterns, never dependencies): Postgres job-queue semantics (River / Oban;
SQS visibility timeouts) → §1; transactional outbox/inbox → §2; fencing tokens → §1.3/D1.

**Delivery model, stated once:** at-least-once execution with repeat-safe **internal state changes**.
§1.5 makes each work kind declare how its database mutations survive duplicate execution. Claim
recovery may resume an agent whose external actions have uncertain outcomes; this contract does
not promise to deduplicate those actions. §3 defines that scope boundary.

**Non-goals.** No third-party queue/orchestrator dependencies. No polymorphic `jobs` table — a
*shared shape*, one table per kind, keeping FKs, typed stores, dbtest discipline. No event-sourcing
expansion. No external-action journal, provider verification framework, or recovery hold on agent
writes. Local-mode N=1 exemptions preserved, stated per surface. `conversation_signals` out of
scope (directed, acked doorbell outbox — different contract, sound today).

---

## 1. The work-item contract (`internal/db/workitem`)

### 1.1 Columns (the shared block)

| Column | Meaning |
|---|---|
| `status` | `ready \| leased \| done \| parked \| cancelled` |
| `attempt` | Charged execution attempts, including the current claim. Claim increments it; a verified non-failure deferral returns that claim's charge (§1.6). |
| `max_attempts` | Per-row budget, from the kind's policy at insert. |
| `next_attempt_at` | NULL = claimable now; future = deferred. **This column is the retry model** — a retrying row defers; it never blocks the queue head. |
| `lease_generation` | **The fence.** Monotonic per row, incremented on *every* acquisition, including reclaim by the same owner in the same boot. |
| `lease_owner`, `lease_epoch` | Provenance (display/debug/boot fast-path), **not the fence**. |
| `leased_at`, `lease_expires_at` | The visibility timeout. Renewable (§1.6). |
| `cancel_requested_at`, `cancel_requested_by` | The cancellation *request* (§1.7). Never flips status directly. |
| `last_error`, `last_outcome` | Most recent attempt's failure + typed outcome (§1.6). |
| `unique_key` | Admission idempotency (§1.4). |
| `first_enqueued_at` | Preserved across operator redrives. |
| `org_id`, `created_at`, `done_at` | Standard. |

Kind-specific columns (FKs, payload) live beside the block, typed, with real foreign keys.

### 1.2 Status machine, the claim, and the receipt

```
insert → ready ── claim ──► leased ── complete ──► done
            ▲                 │ │ │
            │                 │ │ └─ cancel settle (request observed) ─► cancelled
            │                 │ └─── park (budget / poison / permanent) ─► parked ─ redrive ─► ready
            └── requeue (attempt<max): next_attempt_at=now()+backoff        └───── supersede ─► cancelled
            └── defer (expected wait): return current attempt charge; set next_attempt_at
            └── lease expiry: row claimable AS-IS (attempt++, generation++)
```

The claim (Postgres) selects `(status='ready' AND ripe) OR (status='leased' AND lease_expires_at <=
now())` with `ORDER BY id FOR UPDATE SKIP LOCKED LIMIT n` (optionally org-interleaved, §1.6), and
stamps `attempt+1`, `lease_generation+1`, the lease triple. A selected row already at budget is
parked by the claimer; a selected row with `cancel_requested_at` set is settled `cancelled` by the
claimer, with cancellation taking precedence over the budget. Parking and cancel-settling each
have exactly one code path.

Every claim returns a **receipt**: `{item_id, lease_generation, lease_expires_at, attempt,
unique_key}` — plus any kind-declared claim-frozen fields (e.g. §4's `processing_revision`). The
receipt is the only token that authorizes terminal writes and renewals.

Load-bearing properties: expired leases are claimable **directly in the claim query** (no sweeper
exists to orphan, mistune, or wedge; recovery latency = next claim), and same-owner ABA is
impossible (a straggler's receipt carries a stale generation; its writes match zero rows).

SQLite: `BEGIN IMMEDIATE` update-returning instead of `SKIP LOCKED` — correct under the standing
N=1 exemption; generations maintained identically so conformance is dialect-uniform.

**Boot behavior:** none required. The optional fast-path release (`lease_owner=me AND
lease_epoch<myEpoch` → `ready`, generation++) is subject to D3's boot ordering where the kind's
work can have surviving external presence (claims: cells torn down **before** release — §D3).

### 1.3 Terminal writes are generation-fenced; the loser contract is universal

Every **holder** write — `complete/park/cancel-settle/requeue/defer/renew` — requires
`WHERE id=$1 AND status='leased' AND lease_generation=$gen AND lease_expires_at > now()`.
Zero rows ⇒ `ErrLeaseLost`: stop, write nothing else about this item, log at info. Expiry itself
ends authority even when no successor has claimed yet; the next claimer owns disposition. The
claimer's budget/cancellation settlement in §1.2 is a separate locked acquisition path, never a
way for an expired holder to settle its own row.

For expiry guards, `now()` here means **fresh database time at the guard**, not a timestamp frozen
when a long transaction began (Postgres: `clock_timestamp()`). Validate after acquiring the row
lock. In `SingleTx`, repeat the expiry guard at the final disposition write; failure rolls back
the closure's mutations. In `FencedReplay`, each constituent transaction checks the live receipt
under lock as well as its domain replay fence. Claim-adapter writes inherit this rule.

### 1.4 Admission idempotency — and what it does *not* buy

`unique_key` modes per kind: **`UniqueForever`** (partial unique across all rows; retention
requires terminal rows kept or pruned into a tombstone ledger preserving the key) and
**`UniqueWhileUnsettled`** (partial unique `WHERE status IN ('ready','leased','parked')` — parked
rows are uniqueness-bearing; re-admission requires operator `redrive` — fresh budget, generation++,
history preserved — or `supersede` → `cancelled` with `superseded_by`). Insert helpers return
`(id, deduplicated)`. Domain fences stay authoritative; `unique_key` complements them. Admission
dedup does not make consumption idempotent — that is §1.5.

### 1.5 Consumption idempotency: every kind declares its strategy

One of: **`SingleTx`** — domain mutation + generation-fenced completion in one transaction via
`Complete(ctx, receipt, func(tx) error)`; **`FencedReplay`** — multi-tx handler, every constituent
write idempotent under replay via named domain fences. These strategies cover database effects;
provider calls and arbitrary agent tool execution are outside their guarantee (§3).

**Lock and cancellation ordering inside `SingleTx`** (R2-H1): `Complete` locks the item row
(`FOR UPDATE`, verifying status+generation+expiry) **first**, then runs the closure's domain writes,
then flips terminal — one acquisition order with the cancel-request writer (which touches only the
`cancel_requested_*` columns, never the lease columns). A request committed before the lock is
observed before domain writes; one arriving behind a successful completion finds a terminal row
and cannot cancel it retroactively. The conformance suite pins both orders.

The suite includes barrier crash tests per strategy: for `SingleTx`, crash before and after the
combined commit and prove domain mutation and completion either both exist or neither does; for
`FencedReplay`, crash between a domain commit and completion, reclaim, and re-execute without
duplicate domain state. A kind that can't pass its declared strategy's test doesn't ship.

### 1.6 Policy, renewal, and typed outcomes

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

**Renewal cannot resurrect an expired lease** (R2-B7): `RenewLease(receipt)` requires
`lease_generation=$gen AND lease_expires_at > now()` **on DB time**, and sets
`lease_expires_at = now() + policy.Lease` (never old-timestamp-plus-extension). A late renewal
returns `ErrLeaseLost` even if no successor has claimed — expiry alone ends authority. The renewal
RPC is itself deadline-bounded, and the holder's self-fence timer (where the kind has one, D3) runs
on the local monotonic clock independent of the renewal call's fate.

`Requeue(receipt, outcome, err)` takes a typed outcome: `transient | dependency_down |
poison_suspected | deadline | permanent` (R2-H2). **`permanent` parks immediately regardless of
remaining budget** — a validation failure or a 4xx-class rejection does not earn four more
identical attempts. Parking reasons and metrics key on the outcome.

**Expected waiting is not failure.** Workers check known prerequisites before claiming where
possible (for example, a pending firing waits for the task's active run to finish). A race found
after claim, or a newer score revision invalidating an evaluation, uses
`Defer(receipt, reason, nextAttemptAt)` instead of `Requeue`. In one fenced transaction it returns
the current attempt's charge (`attempt-1`), returns the row to `ready`, and sets a future
`next_attempt_at`. Generation never decreases: a later claim always gets a new receipt. The kind
declares the predicate that justifies deferral and checks it in that same transaction, without
committing any domain mutation for that attempt. A database error, expired lease, or unknown
outcome is not a deferral and never gets a refund. Cancellation wins if requested. Repeated healthy waiting cannot exhaust
the failure budget; deferred depth/age and a deferrals counter still expose a stuck prerequisite.

### 1.7 Cancellation: request vs disposition

`cancel_requested_at/by` is a request any authorized actor may set on a `ready` or `leased` row; it
never flips status. A `ready` row with the request set is settled `cancelled` at claim time; a
`leased` holder observes it at its next fenced write or renewal and settles. Terminal `cancelled`
records who and why.

### 1.8 Observability and the operator surface — two postures, stated separately (R2-B8)

**Worker posture:** background workers use admin-pool `...System` methods, org bound by argument,
never request-path. Brain-gated workers re-verify the lease term between batches.

**Operator posture:** the parked/redrive surface *is* request-path, and each kind declares its
authorization explicitly rather than inheriting a default: either **org-admin-only** following the
`failedEventsHandler` pattern (admin-pool store behind `RequireOrgAdminRole`, no RLS backstop —
stated, tested), or a **named visibility join** where rows are team- or user-scoped. Each kind
defines how that authorization survives its retention and parent-deletion rules. Every kind ships
handler authorization tests alongside its pgtest.

Metrics per table (emitted by the package): gauges — `ready` depth, `leased`, `parked`,
oldest-ready age, deferred depth/age; counters — claims, completions, parks by reason, deferrals,
requeues by typed outcome, expired-lease reclaims (the crash canary). Paired with stated objectives
per kind (oldest-ready age target; parked==0 steady state) and alerts on objective breach — "queue
stopped draining" and "idle
system" are distinguishable by construction. The parked surface generalizes TFAC-780's panel with
true counts and per-row redrive/supersede.

### 1.9 Conformance: a crash matrix

`internal/db/workitemtest`, both dialects, every adopting table: lifecycle basics; same-owner
stale-receipt loser; barrier crash per consumption strategy; reclaim-mid-execution with straggler
writes; **every holder write after expiry loses**, even before takeover; expiry while waiting for
a row lock and during a `SingleTx` closure; parked-uniqueness + redrive/supersede conflicts;
cancel-request races; repeated expected deferral without budget exhaustion; stale-receipt deferral
cannot refund twice; real failure and crash still consume budget; `SingleTx` lock ordering;
backoff monotonicity; fairness interleaving; metrics-query correctness.

### 1.10 What the package owns vs what the table owns (R2-H4)

The package owns and generates: the claim/renew/terminal/requeue/defer SQL shapes parameterized by
table name and the shared-block columns **only** (builders never touch kind columns); the receipt
type; the policy enforcement; the metrics queries; the conformance suite. The table owns: its
kind columns and their reads/writes inside `Complete` closures; its insert statements (composing
the package's admission helper); its indexes — the package *documents* the required partial
indexes (`(next_attempt_at, id) WHERE status='ready'`; `(lease_expires_at) WHERE
status='leased'`; the unique-mode index) and the conformance suite asserts they exist. The
boundary is: **semantics in the package, schema in the table, and no SQL string in an adopting
package reimplements a shape the builders provide.**

### 1.11 Worker adoption checklist

Every adopting worker states in its package doc and ticket: owner (brain-gated leader loop /
executor dispatcher / request path), wake source, lease-term re-verification between batches,
pool + authorization posture per §1.8, the partial indexes, and pgtest coverage including the
ex-leader/straggler case.

---

## 2. The obligation rule

> **Any transaction whose state change implies internal follow-on work writes the work row — or
> an inbox receipt — in the same transaction.**

Corollaries: sweeps demote from delivery mechanism to invariant checker; best-effort tails must
name what re-runs them or be promoted; existing conformants (events+event_queue, 777's atomic
CAS+enqueue, 779's close-tx intent, 775's stamp) are recognized, not rebuilt. Violations retired at
adoption: the discovery-seed backfill (joins the seed CAS tx), pre-fire owner consolidation (joins
the fire tx or aborts it), scores→re-derive (§4), mint→step (763's sweep remains as checker).

---

## 3. Scope boundary: external effects are separate work

**This implementation covers TF's internal obligations and state transitions.** It does not add
`effect_receipts`, an effect-reconciliation queue, durable provider-operation identities,
provider-specific probes or retry policies, or a recovery hold on agent writes. The shared package
has no external-effect strategy or unclaimed-backstop discharge API to build in anticipation of
that separate work.

The boundary includes `tfac exec` calls to GitHub/Jira, `git push`, real `gh` through the credential
injector, EE integrations such as Slack, arbitrary allowed egress, and future third-party tools.
Their existing behavior, audit records, and provider-specific protections remain in place. A
crash can still leave an external action's result unknown, and an agent reissuing it can still
produce a duplicate. That tradeoff is accepted for this work; it does not block internal durability
improvements or the launch of another integration.

**No new authority crosses the sandbox boundary.** Jails retain their current credential and
network restrictions. Neither an agent nor a sidecar gains extra provider access to verify an
external effect. Internal ownership fencing and existing cell teardown remain required, but they
are not an external-effect deduplication guarantee. Transcript repair keeps its current behavior:
report interrupted tool results as unknown; do not blindly replay the tool call.

The [third-party MCP converter (TFAC-875)](https://linear.app/sky-ai-eng/issue/TFAC-875) deliberately
admits tools whose effect semantics TF may not know. This contract therefore imposes no
per-provider verification matrix, tool classification requirement, or new connector admission gate.
Any external-effect durability design must be considered separately against that integration and
credential architecture. The external-effects design area is recorded on
[TFAC-760](https://linear.app/sky-ai-eng/issue/TFAC-760); it is not an implementation phase or
acceptance gate for this specification.

---

## 4. Re-derive, revision-safe (R2-B1)

`tasks.score_revision BIGINT`, bumped in the same `UpdateTaskScores` statement that writes scores.
The obligation row (`task_rederive_queue`, task FK, `UniqueWhileUnsettled`) carries **two**
revision fields:

- **`requested_revision`** — on the row, monotonic: the scores-write upsert raises it (including on
  a currently-`leased` row) in the same tx as the score commit. Never lowered.
- **`processing_revision`** — **stamped into the claim receipt at claim time and immutable for that
  generation.** This is what the worker evaluates.

Completion runs under `SingleTx`: before committing the evaluation's domain writes, the closure
compares **`receipt.processing_revision`** (never the mutable row value) to the task's current
`score_revision`. Equal ⇒ the evaluation is current; complete. Different ⇒ commit no evaluation
effects and **defer under the same receipt fence** (§1.6), preserving the row's higher
`requested_revision` — the newer score's obligation survives without charging a failed attempt.
The rev 2 bug — a leased-row upsert making completion compare the *raised* target to the *raised*
task and conclude freshness for an evaluation of the old score — is structurally impossible: the compared value is
frozen in the receipt. `rederive_owed` is dropped. Barrier tests: the leased-row upsert race in
both orders (score-commit before completion-read and after).

---

## 5. Standing disciplines

**D1 — Repairs carry fencing tokens.** Sweep/reconciler writes CAS on a version the live path
advances (`poll_seq` for entities; reactivate writes the fresh snapshot in the same tx as the state
flip; close grows a CAS arm; the terminal reconciler demotes to a counting checker or folds into
tracker Phase 3 per O3).

**D2 — State records intent.** A request path writes `conversations.stop_requested` and its durable
stop signal in one transaction; it does not park the conversation directly. The live holder, or
the dispatcher settling an expired/unclaimed conversation, parks it `open` and releases any claim
in one transaction. The claim gate refuses execution while stop is requested; user follow-up clears
the request (explicit re-arm). A plain stop leaves the blueprint unchanged. Cancellation follows
§1.7's request-vs-disposition split everywhere.

**D3 — Claims adopt time semantics via an explicit adapter.** Claims adopt exactly:
`lease_expires_at` + `lease_generation`, renewal per §1.6's strict rule (free on every fenced
write + `RenewEvery` ticker), `deadline_at` (expiry = system stop through the stop machinery: D2
intent + kill signal + park open). Claims do **not** adopt `status`, `unique_key`, or the per-row
attempt model (episode accounting stays, with D4's vocabulary).

**One takeover clock**, validated at boot: `renew_interval < self_fence_deadline < takeover_after`.
For the claims adapter, **`policy.Lease = takeover_after`**: acquisition and each successful renewal
set `lease_expires_at = database_now + takeover_after`. Takeover becomes eligible at that expiry;
there is no additional delay after expiry. The proposed defaults (O4) are renew 20s / self-fence
45s / claim lease and takeover 75s / `TF_RUN_DEADLINE` 6h. The ordinary work-item default lease
remains 60s.

With a successful renewal granted at DB time T and no later renewal: self-fencing aims to stop the
holder by roughly T+45s, and takeover becomes eligible at T+75s. The self-fence watchdog uses the
**local monotonic clock**, conservatively anchored to the request start of the last successful
acquisition/renewal; network delay must not extend the watchdog beyond that bound. Renewal calls
are bounded, the watchdog runs independently, and a late response cannot revive a fenced claim.
On self-fence: stop claiming, refuse writes and egress, and kill sidecar and sandbox. A process
pause can delay that cleanup, so internal correctness rests on the database's generation and
expiry checks, not on assuming a timer always fires on schedule. Test paused holders that return
after takeover. External requests already in flight retain the §3 boundary.

**Cutover — collapsed (rev 3.1).** No multi-mode deployment has ever shipped, so there is no
old-executor population and no rolling upgrade to survive: rev 3's three-step mixed-version
choreography (the NULL-lease legacy arm, the expired-AND-stale conjunction, the zero-count
retirement assertion) is **deleted, not deferred**. The shape that ships:

- Lease columns land in the Postgres baseline directly (standing rule: the baseline is freely
  editable until a persistent multi deployment exists). SQLite — the one deployment shape with
  real installed DBs — gets an ordinary forward migration; the backfill is trivial: terminal
  claims keep NULL lease columns (inert history), and a claim live across a local upgrade is a
  predecessor-boot leftover the existing boot self-sweep already handles.
- There is no reaper predicate because there is no reaper (below). The instance heartbeat exits
  claim liveness entirely and remains the registry's signal (instance GC, fleet dashboard,
  placement's dead-preferred-executor check) — one liveness signal per concern.
- Deployment-reality gate, restated: if a persistent multi deployment exists before this phase
  cuts, the rev 3 mixed-version choreography comes back as written — it was correct for that
  world and is dead weight in this one.

**Takeover is a claim, not a sweep (rev 3.2) — `internal/reaper` is deleted.** The contract's
rule holds for claims exactly as for work items: an expired lease is reclaimed *inside the claim
path*, never by a sweeper. `needsDrivingSQL`'s "no active claim" arm becomes "no active claim
within takeover": an unreleased claim with `lease_expires_at <= now()` (DB time)
does not count as driving. On selecting such a row, the dispatcher's claim transaction performs
the following dispositions in order, mirroring §1.2's claimer-settles rule:

- blueprint `cancel_requested` → settle: park `open` / `system_cancelled`, blueprint `cancelled`,
  claim released `reaped`. No mint; keep scanning.
- plain `stop_requested` → settle: park `open`, release the claim, leave the blueprint unchanged.
  No mint; keep scanning. The same stop disposition handles queued work with no claim.
- loss episode ≥ `TF_MAX_CLAIM_ATTEMPTS` → terminal-fail exactly as today (`executor_lost`,
  blueprint `failed`, ended stamps), claim released `reaped`. No mint; keep scanning.
- otherwise → release the expired claim (`reaped`, generation++) and mint the successor **in the
  same transaction**. `preferred_executor_id` is not consulted: an expired lease is unowned.

`idx_claims_one_active` stays the sole mutual-exclusion primitive, which is why the release is a
statement *ahead of* the mint inside the transaction rather than a sibling CTE: a same-statement
uniqueness check still sees the pre-release tuple. The settlement arms run on every
dispatcher tick regardless of free capacity (bookkeeping, not work); a mint requires a slot. The
tick must not block waiting on the execution semaphore: settlement runs before capacity/memory
gates, then execution claims acquire only available slots and yield to the next tick when full.
Conformance covers all slots occupied, memory-gated dispatch, and queued stop intent. That
asymmetry is deliberate: a takeover *is* placement, and queue-is-truth means a dead executor's
conversations are redriven when and where capacity exists, not flipped by a leader that cannot
run them. Dialect-uniform — SQLite's `ClaimNextConversation` adopts the same arms, so local mode
gains takeover semantics it never had (today it has boot reconcile only). The display projection's
derived `running` rung reads lease expiry, not `released_at` alone, so a dead engagement shows as
awaiting takeover rather than running. The visibility the reaper's prompt stamping used to provide
comes from a package gauge instead — active claims past expiry, alerted on age — so "dead and
not yet taken over" is a number, never a log line. Collect this database-derived gauge outside
the dispatcher loop so a wedged or absent dispatcher cannot suppress its own alarm.

The reaper's other chores, each with a named home:

- **Claim-desync janitor** (a terminal conversation with a dangling active claim — app-pool
  terminal writes commit the status flip and the claim release separately because `tf_app` holds
  no claims UPDATE grant) — **retired by D2, not swept.** Request paths write *intent*
  (`stop_requested` / cancel request), never terminal status; the holder settles at its next
  fenced write in the same transaction as its claim release, and a dead holder is settled by
  takeover. Terminal status then has exactly two writers — holder and takeover — both releasing
  the claim in the same transaction, so the desync cannot arise. The boot arm demotes to an
  invariant checker (count + alert, no write). Ticket gate: enumerate every app-pool terminal
  writer and convert each to an intent write.
- **Orphaned-at-mint blueprint runs** — retired by §2 (mint and first-step enqueue in one
  transaction); the boot arm stays as checker, the periodic copy is deleted.
- **Registry GC** (instances rows heartbeat-stale > 7d) — registry housekeeping, not claim
  liveness; moves to `internal/instance` as the registry's own brain-gated daily loop, behavior
  unchanged.
- **Config.** `TF_REAPER_STALE_SEC` retires; `takeover_after` takes its role under a
  claim-vocabulary name, and the boot validation becomes the D3 triple `renew < self-fence <
  takeover`. `TF_SELF_FENCE_SEC` stays as the *instance-wide* coarse fence (heartbeat write
  failing → kill every cell, stop claiming); per-claim renewal failure is the fine-grained fence
  beneath it; both are bounded strictly below takeover. `TF_MAX_CLAIM_ATTEMPTS` stays — it is the
  episode budget the takeover path consumes.

**Boot supersession ordering:** the instance-id flock proves a same-id predecessor *process* has
exited — but not that its **cells** (jails/sidecars) are dead. Therefore boot order is: predecessor
cell teardown sweep completes **first**, then claim release/fast-path handback. A fast-path release
must never make a successor eligible before predecessor cell teardown is confirmed. Where
teardown cannot be confirmed, fast-path release is withheld and the ordinary lease-expiry path
applies. This does not assert that waiting proves an external effect absent (§3). Timelines for
duplicate-state-root (excluded by the flock, asserted) and old-executor-crash are test cases.

**D4 — One attempt vocabulary.** §1.2's definition for work items. For claims: episode accounting
stays; only `reaped` counts toward the takeover loss budget; `requeued_credentials` and
`requeued_boot` become their own outcomes and never extend a loss episode. Boot/credential outcome
changes land with the same migration (there is no stepped cutover — see D3).

---

## 6. What this explicitly does NOT change

Domain fences stay authoritative; conversations stay queue-is-truth derived; the eventbus and
`tf_ctl` stay lossy-by-design; CAS-before-publish ordering stands; `external_actions` stays
append-only; local-mode exemptions restated per surface, none silently widened. External-effect
durability is excluded across all channels (§3); the existing security controls remain in force.

---

## 7. Adoption plan

### 7.1 Migration posture (M8 dissent, adopted as a gate)

Every phase lands as forward migrations in both dialect trees. The standing rule (no multi
deployment; pg baseline freely editable) holds today; every phase re-checks deployment reality at
cut time — the moment a persistent multi deployment exists, expand→dual-read→backfill→switch→
contract plus previous-head upgrade tests become mandatory, and vocabulary swaps stop shipping in
one release. P4 is written to that discipline from the start.

### 7.2 Phases

- **P0 — the contract.** `workitem` + `workitemtest` crash matrix + metrics/SLO emitters + the
  generalized parked surface with per-kind authorization postures (§1.8). Gate: the §1.9 lifecycle,
  expiry, deferral, and crash semantics demonstrated in the suite before any consumer ships.
- **P1 — `event_queue` retrofit.** Forward migrations with the **explicit backfill** (R2-B9):
  `pending→ready`; `failed→parked` (attempts/last_error preserved); **`processing→leased` with a
  synthetic already-expired lease** (`lease_generation=1`, `lease_expires_at=now()-1s`, owner
  carried from `executor_id`) so in-flight rows are immediately reclaimable through the normal
  claim path — no special recovery arm. CHECK/index replacement enumerated in the migration.
  Migration runs before workers start, which both deployment shapes guarantee today (single binary
  boots goose before the drain worker; multi runs migrate on boot the same way) — stated in the
  ticket, asserted in the runbook. **There is no kill switch: rollback is a forward fix.** The
  vocabulary swap is not old-worker-compatible, and a flag-retained legacy worker would need a
  compatibility adapter against the new schema — cost without benefit while both deployment shapes
  ship worker and schema together (single binary; no persistent multi). This is the honest version
  of rev 2's claim. Worker declares `FencedReplay` naming its three fences; per-org fairness arm;
  `UnitDeadline`; sweeper and boot-reset deleted; 780's panel moves to `parked`; the sibling-close
  `prepare` error arm rides the rewrite.
- **P2 — `pending_firings` conformance.** Explicit migration (R2-H3): status mapping
  (`pending→ready`, `draining→leased` with the same synthetic-expired-lease backfill), the active
  partial unique index rebuilt as the `UniqueWhileUnsettled` index (parked rows included), lease
  columns added, `RequeueStaleDraining` deleted. The task-transition tx documented per §1.5.
- **P3 — re-derive** per §4. `SingleTx` + both-orders revision barrier tests.
- **P4 — claims time-semantics** per D3/D4: lease columns + renewal + deadline, takeover folded
  into the dispatcher claim path in both dialects, `internal/reaper` deleted with its four chores
  rehomed as D3 states, boot ordering, settlement under full capacity, and independent stale-claim
  monitoring. Resolves the claim-liveness, wall-clock, and durable-stop P1s.
- **P5 — D1 completions.** Reactivate-writes-snapshot-in-tx, close CAS, reconciler demoted to a
  counting checker (O3).

P0 and P1 are the first delivery milestone: build the smallest shared contract against the real
event-queue adopter, prove crash recovery and upgrades, then migrate the remaining consumers.
The event queue already has transactional admission, replay fences, and recovery; it is the first
adopter because it is concrete and close to the target contract, not because it lacks durability.
Preserve those guarantees. Do not build provider journals or generic external-effect adapters as
part of the shared package.

Sequencing: 773/774/775 land first; P1 subsumes 761/765's mechanisms and must not race them.

### 7.3 Future kinds (sketch)

A webhook ingress lane remains the contract's forward test case: verified delivery → inbox row
(`UniqueForever` on provider delivery id, tombstoned) → routed by a contract worker. Checked
against the contract before P0 freezes; not designed here.

---

## 8. Open questions

- **O3** — reconciler end-state: fold enforcement into tracker Phase 3 + keep a counting checker
  (recommendation unchanged).
- **O4** — timing defaults: renew 20s / self-fence 45s / claim lease and takeover 75s /
  `TF_RUN_DEADLINE` 6h. The values remain proposed; D3 fixes what each duration measures.
- **O5** — `pending_firings` collapse into `event_queue`: deferred, revisit after P2 soak.
- **O6** — `score_revision` writers: `UpdateTaskScores` only, or does a manual re-score path need
  to bump it?
