# The Durable Work Contract — design of record (DRAFT, rev 3.2)

**Status:** draft rev 3.2, 2026-09-16. Rev 1 reviewed (12 blockers, 8 hardening — all incorporated
in rev 2, one partial dissent on M8). Rev 2 reviewed (9 blockers R2-B1–B9, 4 clarifications
R2-H1–H4 — all incorporated in rev 3). Rev 3.1: the claims mixed-version cutover is collapsed per
deployment reality — no multi-mode deployment has ever shipped, so there is no rolling upgrade to
survive (user call, 2026-08-19). Rev 3.2: **`internal/reaper` is deleted** — takeover is a claim,
not a sweep (user call, 2026-09-16); its four chores are rehomed in D3. Per the rev 2 review's own
gate, P0/P1 tickets may be cut from this revision
once the header is blessed. Nothing here is settled until it says so.

**Problem statement.** The 2026-08 durability audits and the 15-ticket hardening wave showed one
root cause behind ~25 findings: **work that exists only as a side effect of a function call rather
than as a row with a lifecycle.** Every queue-shaped surface re-implements the same lifecycle
locally and each copy shipped its own bug set. This spec replaces the approximations with one
contract, implemented once, adopted table by table. The prize is that the *next* obligation costs a
small typed table that inherits retry, backoff, leases, fencing, recovery, parking, idempotency, an
operator surface, and metrics — instead of re-deriving all of them and re-shipping their bugs.

**Industry grounding** (patterns, never dependencies): Postgres job-queue semantics (River / Oban;
SQS visibility timeouts) → §1; transactional outbox/inbox → §2; durable execution's effect
memoization (Temporal / Restate; Stripe idempotent requests; retry-safety via caller-minted
identity) → §3; fencing tokens → §1.3/D1.

**Delivery model, stated once:** at-least-once execution over idempotent or journaled consumption,
everywhere. §1.5 makes each work kind *declare* how it survives duplicate execution; §3 makes
external effects either provably-deduplicated or explicitly at-least-once with parked ambiguity.

**Non-goals.** No third-party queue/orchestrator dependencies. No polymorphic `jobs` table — a
*shared shape*, one table per kind, keeping FKs, typed stores, dbtest discipline. No event-sourcing
expansion. Local-mode N=1 exemptions preserved, stated per surface. `conversation_signals` out of
scope (directed, acked doorbell outbox — different contract, sound today).

---

## 1. The work-item contract (`internal/db/workitem`)

### 1.1 Columns (the shared block)

| Column | Meaning |
|---|---|
| `status` | `ready \| leased \| done \| parked \| cancelled` |
| `attempt` | Leases taken, including the current one. Stamped at claim. |
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
            └── lease expiry: row claimable AS-IS (attempt++, generation++)
```

The claim (Postgres) selects `(status='ready' AND ripe) OR (status='leased' AND lease_expires_at <
now())` with `ORDER BY id FOR UPDATE SKIP LOCKED LIMIT n` (optionally org-interleaved, §1.6), and
stamps `attempt+1`, `lease_generation+1`, the lease triple. A selected row already at budget is
parked by the claimer; a selected row with `cancel_requested_at` set is settled `cancelled` by the
claimer. Parking and cancel-settling each have exactly one code path.

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

`complete/park/cancel-settle/requeue/renew` all carry `WHERE id=$1 AND status='leased' AND
lease_generation=$gen`. Zero rows ⇒ `ErrLeaseLost`: stop, write nothing else about this item, log
at info — the successor owns disposition.

**One narrow exception exists** — `DischargeUnclaimed` (§1.7a) — for obligations whose *purpose has
been satisfied out-of-band* before any claim. It is not a completion and requires no receipt; it is
a cancellation request plus an authorizing predicate, and it is the only legal way to acknowledge a
`ready` row without claiming it. (R2-B2)

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
write idempotent under replay via named domain fences; **`Journaled`** — external effects via §3.

**Lock and cancellation ordering inside `SingleTx`** (R2-H1): `Complete` locks the item row
(`FOR UPDATE`, verifying status+generation) **first**, then runs the closure's domain writes, then
flips terminal — one acquisition order, so a concurrent cancel-request writer (which touches only
the `cancel_requested_*` columns, never the lease columns) cannot deadlock it; a cancel request
that lands after the lock is observed by the holder at its *next* fenced write, per §1.7. The
conformance suite pins the ordering.

The suite includes a barrier crash test per strategy: crash between domain commit and completion,
reclaim, re-execute — no duplicate domain state. A kind that can't pass its declared strategy's
test doesn't ship.

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

### 1.7 Cancellation: request vs disposition

`cancel_requested_at/by` is a request any authorized actor may set on a `ready` or `leased` row; it
never flips status. A `ready` row with the request set is settled `cancelled` at claim time; a
`leased` holder observes it at its next fenced write or renewal and settles. Terminal `cancelled`
records who and why.

### 1.7a `DischargeUnclaimed` — acknowledging an obligation satisfied out-of-band (R2-B2)

Some obligations exist as *backstops*: if the primary path succeeds, the backstop's purpose is
already served before anyone claims it (§3.5's `effect_reconcile` is the canonical case). The
primary path holds no receipt for the backstop row, so §1.3 forbids completing it. The legal shape:

```
DischargeUnclaimed(tx, kind, unique_key, predicate) =
    set cancel_requested_at/by='discharged:<reason>'
    WHERE unique_key=$k AND status='ready' AND <kind-declared authorizing predicate>
```

— a cancellation *request* written in the primary path's own transaction, gated by a kind-declared
predicate over the obligation's subject (for `effect_reconcile`: the receipt row, locked in that
same tx, is `applied`). It fabricates no lease. If the row is already `leased` (a repairer claimed
it concurrently), the request lands anyway and the holder observes it per §1.7 — and the repairer's
own probe finds the applied receipt regardless, so both orders converge. `workitemtest` includes
the discharge-vs-concurrent-claim race in both orders.

### 1.8 Observability and the operator surface — two postures, stated separately (R2-B8)

**Worker posture:** background workers use admin-pool `...System` methods, org bound by argument,
never request-path. Brain-gated workers re-verify the lease term between batches.

**Operator posture:** the parked/redrive surface *is* request-path, and each kind declares its
authorization explicitly rather than inheriting a default: either **org-admin-only** following the
`failedEventsHandler` pattern (admin-pool store behind `RequireOrgAdminRole`, no RLS backstop —
stated, tested), or a **named visibility join** where rows are team- or user-scoped. Kinds whose
rows must be authorizable or auditable after their parent conversation is purged (effect receipts)
**denormalize team/actor onto the row** at insert. Every kind ships handler authorization tests
alongside its pgtest.

Metrics per table (emitted by the package): gauges — `ready` depth, `leased`, `parked`,
oldest-ready age; counters — claims, completions, parks by reason, requeues by typed outcome,
expired-lease reclaims (the crash canary). Paired with stated objectives per kind (oldest-ready age
target; parked==0 steady state) and alerts on objective breach — "queue stopped draining" and "idle
system" are distinguishable by construction. The parked surface generalizes TFAC-780's panel with
true counts and per-row redrive/supersede.

### 1.9 Conformance: a crash matrix

`internal/db/workitemtest`, both dialects, every adopting table: lifecycle basics; same-owner
stale-receipt loser; barrier crash per consumption strategy; reclaim-mid-execution with straggler
writes; **late-renewal-after-expiry loses** (R2-B7); parked-uniqueness + redrive/supersede
conflicts; cancel-request races; **`DischargeUnclaimed` vs concurrent claim, both orders**
(R2-B2); `SingleTx` lock ordering; backoff monotonicity; fairness interleaving; metrics-query
correctness.

### 1.10 What the package owns vs what the table owns (R2-H4)

The package owns and generates: the claim/renew/terminal/discharge SQL shapes parameterized by
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

> **Any transaction whose state change implies follow-on work writes the work row — or an inbox
> receipt — in the same transaction.**

Corollaries: sweeps demote from delivery mechanism to invariant checker; best-effort tails must
name what re-runs them or be promoted; existing conformants (events+event_queue, 777's atomic
CAS+enqueue, 779's close-tx intent, 775's stamp) are recognized, not rebuilt. Violations retired at
adoption: the discovery-seed backfill (joins the seed CAS tx), pre-fire owner consolidation (joins
the fire tx or aborts it), scores→re-derive (§4), mint→step (763's sweep remains as checker).

---

## 3. External effects: `effect_receipts`

### 3.1 Separation of concerns

`external_actions` stays an append-only, immutable, org-deduped **audit funnel**. `effect_receipts`
carries the **mutable execution lifecycle**; an applied receipt links to the audit row it produced.

### 3.2 The receipt

`org_id`, `conversation_id`, `claim_id` (server-bound, §3.4), **`team_id`, `actor_user_id`**
(denormalized at insert, §1.8), **`operation_id`** (§3.3), `request_digest` (canonicalized),
`provider`, `verb`, `channel` (§3.6), `target`, `status` (`intended | executing | applied |
uncertain | parked | superseded`), `execution_generation`, structured `provider_ids`,
`result_meta`, `ambiguity`, timestamps. Unique on `(org_id, operation_id)`.

Applied-duplicate replay: an invocation whose `operation_id` matches an `applied` receipt returns
the recorded result without calling the provider. Parameter mismatch (same `operation_id`,
different `request_digest`) is rejected — retry identity was violated upstream.

### 3.3 Operation identity: one id per effectful relay invocation (R2-B4)

**The sidecar's verb handler mints one operation UUID per effectful invocation**, before writing
intent, and reuses it only for **transport retries of that same invocation** (its own relay-RPC
retry loop). This is the unit the exec path can actually see and the unit that is actually retried
at the transport layer — one Bash tool call may issue several `tfac exec` commands, and the relay
never observes the model-level `tool_call_id`, so tool-call identity is **attribution, not
uniqueness**: `tool_call_id` + ordinal are stored *if* the toolhost protocol later carries them,
never required.

This unifies the runtimes (rev 2's native/SDK asymmetry is deleted): a later *agent-level*
re-issuance — the agent deciding again after a crash, on either runtime — is a **new operation** by
design. Cross-invocation dedup is not the key's job: the prior invocation's receipt is settled by
its own reconciler (§3.5), and the conversation repair pass surfaces unresolved receipts to the
agent by name so it does not blindly re-issue. There is **no payload-digest dedup across
invocations** — that would recreate the payload-equality identity bug; `request_digest` exists only
for mismatch rejection under a reused id.

### 3.4 Execution protocol

1. **Intent** — `status='intended'` committed before the call. In multi mode the intent write
   happens through a relay op whose claim identity is **bound server-side from the relay's own
   `RunInfo`** — extended to carry the claim id, stamped by the orchestrator at sidecar launch;
   nothing jail- or sidecar-supplied is trusted for identity — and the insert is claim-fenced. The
   active claim is revalidated at the credential-injection boundary for effectful verbs. The same
   transaction inserts the `effect_reconcile` backstop item (§3.5) with
   `next_attempt_at = now() + grace`.
2. **Executing** — `status='executing'`, `execution_generation+1`, fenced on current generation. A
   provider idempotency key or conditional write is passed wherever the verb supports one (§3.6);
   where unsupported, the verb is explicitly at-least-once.
3. **Applied** — success records `applied` + `provider_ids` + the audit link, and **discharges the
   reconcile backstop via §1.7a in the same transaction** (the receipt row, locked in that tx and
   `applied`, is the authorizing predicate). A failed applied-write leaves `executing`, which the
   reconciler resolves — the swallow is safe. A timeout or ambiguous response records `uncertain`
   with `ambiguity`; **never blind-retried**.
4. **Repair** — §3.5.

### 3.5 The reconciler: deferral, horizons, and a decision table (R2-B3)

The `effect_reconcile` item (a §1 kind, `UniqueWhileUnsettled` on the receipt) ripens only when the
happy path didn't discharge it. The brain-gated repair worker then proceeds in strict order:

1. **Defer while the originating claim is active.** If the claim is live (unreleased, lease
   unexpired), requeue with backoff — the owner may still be mid-call. The reconciler acts only on
   fenced/released claims.
2. **Wait out the horizons.** After the claim is provably fenced, wait the provider request
   deadline (an in-flight request issued before the fence can still land) plus the verb's declared
   consistency horizon (search/list eventual consistency). Both are per-verb constants in the
   capability matrix.
3. **Decide per verb, per state:**
   - `executing`/`uncertain`, verb has provider idempotency → **retry with the same key**; the
     provider dedups.
   - `executing`/`uncertain`, probeable verb → probe. **Found** ⇒ settle `applied` (link the found
     object). **Not found after the horizons** ⇒ for verbs with a *strong* absence proof (direct
     GET by id) the receipt may settle `uncertain→parked` with evidence, or retry only if the verb
     is naturally idempotent; for weak probes (search-based), **park**.
   - `intended` (never executed), claim dead → **the reconciler never executes the effect in v1.**
     If the conversation is live/resumable, the receipt is surfaced to the agent on resume (the
     agent re-decides — a new operation); if the conversation is terminal, **park** for operator
     disposition. Executing user-intent-bearing writes from a background worker with no agent in
     the loop is a decision this spec declines.
   - **Absence never produces `superseded`.** `superseded` is written only by an explicit later
     operation that replaces this one, or by operator disposition. (R2-B3)
4. Every parked receipt carries the probe evidence and lands on the §1.8 surface.

The reconciler probes with org-level read credentials (the poller path); it needs no per-run sealed
bundle because in v1 it never writes to providers except idempotent-key retries, which reuse the
verb's normal execution path and credential flow.

### 3.6 The capability matrix enumerates *channels*, not just verbs (R2-B5)

**P5's honest boundary: the receipt protocol covers the structured `tfac exec` verb surface only.**
The other write channels are enumerated as explicit residual risk with their existing mitigations,
not silently absorbed:

| Channel | Covered by receipts? | Residual posture |
|---|---|---|
| `tfac exec` verbs (GitHub REST via client, Jira) | **Yes** — §3.2–3.5 | — |
| `git push` (git proxy) | No | Existing branch-push dedup (`branch:<run>:<ref>:<sha>` external_actions key) + pushpolicy ref gate; multi-ref pushes enumerated per ref. Named residual: non-branch refs. |
| Real `gh` through the credential injector (REST + **GraphQL porcelain**) | No | `record_gh_write` post-hoc audit only. Named residual: GraphQL mutations are not classified by the injector — this is the known gh-is-GraphQL-first surface, tracked separately. An in-jail agent re-running `gh` after a crash is at-least-once by nature. |
| EE extension proxies (Slack) | No | Their own audit surface; out of scope here. |
| Arbitrary allowed egress | No | The egress allowlist is the mitigation; out of scope by design. |

The per-verb matrix (idempotency key / conditional / none; probe method + strength; consistency
horizon; natural idempotence; fallback class) is maintained in this spec as verbs onboard, and
every `at_least_once` verb says so in its tool description.

---

## 4. Re-derive, revision-safe (R2-B1)

`tasks.score_revision BIGINT`, bumped in the same `UpdateTaskScores` statement that writes scores.
The obligation row (`task_rederive_queue`, task FK, `UniqueWhileUnsettled`) carries **two**
revision fields:

- **`requested_revision`** — on the row, monotonic: the scores-write upsert raises it (including on
  a currently-`leased` row) in the same tx as the score commit. Never lowered.
- **`processing_revision`** — **stamped into the claim receipt at claim time and immutable for that
  generation.** This is what the worker evaluates.

Completion runs under `SingleTx`: the closure compares **`receipt.processing_revision`** (never the
mutable row value) to the task's current `score_revision`. Equal ⇒ the evaluation is current;
complete. Different ⇒ the evaluation is stale; **generation-fenced requeue** that preserves the
row's (higher) `requested_revision` — the newer score's obligation survives. The rev 2 bug — a
leased-row upsert making completion compare the *raised* target to the *raised* task and conclude
freshness for an evaluation of the old score — is structurally impossible: the compared value is
frozen in the receipt. `rederive_owed` is dropped. Barrier tests: the leased-row upsert race in
both orders (score-commit before completion-read and after).

---

## 5. Standing disciplines

**D1 — Repairs carry fencing tokens.** Sweep/reconciler writes CAS on a version the live path
advances (`poll_seq` for entities; reactivate writes the fresh snapshot in the same tx as the state
flip; close grows a CAS arm; the terminal reconciler demotes to a counting checker or folds into
tracker Phase 3 per O3).

**D2 — State records intent.** `conversations.stop_requested` written in the same tx as the
plain-stop park; claim gate refuses; user follow-up clears (explicit re-arm). Cancellation follows
§1.7's request-vs-disposition split everywhere.

**D3 — Claims adopt time semantics via an explicit adapter.** Claims adopt exactly:
`lease_expires_at` + `lease_generation`, renewal per §1.6's strict rule (free on every fenced
write + `RenewEvery` ticker), `deadline_at` (expiry = system stop through the stop machinery: D2
intent + kill signal + park open). Claims do **not** adopt `status`, `unique_key`, or the per-row
attempt model (episode accounting stays, with D4's vocabulary).

**Takeover ordering**, validated at boot: `renew_interval < self_fence_deadline < takeover_after`,
lease grants and expiry on **DB time**, self-fence on the holder's **local monotonic clock** since
last *successful* renewal. On self-fence: stop claiming, refuse writes and egress, kill sidecar and
sandbox **before** `takeover_after` can elapse. Defaults (O4): renew 20s / self-fence 45s /
takeover 75s / `TF_RUN_DEADLINE` 6h.

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
within takeover": an unreleased claim with `lease_expires_at + takeover_after < now()` (DB time)
does not count as driving. On selecting such a row, the dispatcher's claim transaction performs
the reaper's three dispositions as one code path each, mirroring §1.2's claimer-settles rule:

- blueprint `cancel_requested` → settle: park `open` / `system_cancelled`, blueprint `cancelled`,
  claim released `reaped`. No mint; keep scanning.
- loss episode ≥ `TF_MAX_CLAIM_ATTEMPTS` → terminal-fail exactly as today (`executor_lost`,
  blueprint `failed`, ended stamps), claim released `reaped`. No mint; keep scanning.
- otherwise → release the expired claim (`reaped`, generation++) and mint the successor **in the
  same transaction**. `preferred_executor_id` is not consulted: an expired lease is unowned.

`idx_claims_one_active` stays the sole mutual-exclusion primitive, which is why the release is a
statement *ahead of* the mint inside the transaction rather than a sibling CTE: a same-statement
uniqueness check still sees the pre-release tuple. The two settlement arms run on every
dispatcher tick regardless of free capacity (bookkeeping, not work); a mint requires a slot. That
asymmetry is deliberate: a takeover *is* placement, and queue-is-truth means a dead executor's
conversations are redriven when and where capacity exists, not flipped by a leader that cannot
run them. Dialect-uniform — SQLite's `ClaimNextConversation` adopts the same arms, so local mode
gains takeover semantics it never had (today it has boot reconcile only). The display projection's
derived `running` rung reads lease expiry, not `released_at` alone, so a dead engagement shows as
awaiting takeover rather than running. The visibility the reaper's prompt stamping used to provide
comes from a package gauge instead — active claims past takeover, alerted on age — so "dead and
not yet taken over" is a number, never a log line.

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
cell teardown sweep completes **first**, then claim release/fast-path handback. A successor claim
must never become eligible while a predecessor's sandbox can still emit writes or egress. Where
teardown cannot be confirmed, the release is deferred to `takeover_after`. Timelines for
duplicate-state-root (excluded by the flock, asserted) and old-executor-crash are test cases.

**D4 — One attempt vocabulary.** §1.2's definition for work items. For claims: episode accounting
stays; only `reaped` counts toward the takeover loss budget; `requeued_credentials` and
`requeued_boot` become their own outcomes and never extend a loss episode. Boot/credential outcome
changes land with the same migration (there is no stepped cutover — see D3).

---

## 6. What this explicitly does NOT change

Domain fences stay authoritative; conversations stay queue-is-truth derived; the eventbus and
`tf_ctl` stay lossy-by-design; CAS-before-publish ordering stands; `external_actions` stays
append-only; local-mode exemptions restated per surface, none silently widened. The uncovered
effect channels are named (§3.6), not absorbed.

---

## 7. Adoption plan

### 7.1 Migration posture (M8 dissent, adopted as a gate)

Every phase lands as forward migrations in both dialect trees. The standing rule (no multi
deployment; pg baseline freely editable) holds today; every phase re-checks deployment reality at
cut time — the moment a persistent multi deployment exists, expand→dual-read→backfill→switch→
contract plus previous-head upgrade tests become mandatory, and vocabulary swaps stop shipping in
one release. P4/P5 are written to that discipline from the start.

### 7.2 Phases

- **P0 — the contract.** `workitem` + `workitemtest` crash matrix + metrics/SLO emitters + the
  generalized parked surface with per-kind authorization postures (§1.8). Gate: M1/M2/M9 + R2-B2/
  B7 semantics demonstrated in the suite before any consumer ships.
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
  rehomed as D3 states, boot ordering. Resolves the claim-liveness, wall-clock, and durable-stop
  P1s.
- **P5 — effects.** `effect_receipts` + RunInfo claim binding + the channel/verb matrix + the
  reconciler with §3.5's decision table + repair-pass integration. Gates: the matrix covers every
  existing verb; every `at_least_once` verb says so in its tool description; the §3.6 residual
  channels are acknowledged in the ticket.
- **P6 — D1 completions.** Reactivate-writes-snapshot-in-tx, close CAS, reconciler demoted to a
  counting checker (O3).

Sequencing: 773/774/775 land first; P1 subsumes 761/765's mechanisms and must not race them.

### 7.3 Future kinds (sketch)

A webhook ingress lane remains the contract's forward test case: verified delivery → inbox row
(`UniqueForever` on provider delivery id, tombstoned) → routed by a contract worker. Checked
against the contract before P0 freezes; not designed here.

---

## 8. Open questions

- **O2** — probe markers: invisible HTML-comment `operation_id` marker in bot-authored
  comments/issues; accept the cosmetic cost?
- **O3** — reconciler end-state: fold enforcement into tracker Phase 3 + keep a counting checker
  (recommendation unchanged).
- **O4** — timing defaults: renew 20s / self-fence 45s / takeover 75s / `TF_RUN_DEADLINE` 6h;
  effect grace + per-verb horizons need first values at P5 cut.
- **O5** — `pending_firings` collapse into `event_queue`: deferred, revisit after P2 soak.
- **O6** — `score_revision` writers: `UpdateTaskScores` only, or does a manual re-score path need
  to bump it?
- **O7** — *(narrowed by R2-B4)* effectful verbs on the SDK runtime now share the same
  per-invocation identity; residual question is only whether any SDK-only verb should be gated to
  native regardless. Recommendation: no gate; the cutover is the plan of record.
- **O8** *(new)* — §3.5 declines reconciler-executed effects in v1 (`intended` + dead claim +
  terminal conversation ⇒ park). Confirm that operator-disposition-only posture, or nominate
  specific verbs (naturally idempotent ones) for reconciler execution in v2.
