# Auto-delegation V1 — Agent briefing

Starting point for any agent assigned to work on **SKY-140 through SKY-149**. These ten tickets compose a single architectural arc: _auto-iterating fixes for CI failures and similar events, with cross-run memory, safety rails, and scoped automation rules._

Read your assigned ticket first (via Linear), then come back here for the context that doesn't fit inside a single ticket body.

---

## The big picture

Triage Factory polls GitHub and Linear, turns state changes into events, and can delegate prompts (headless Claude Code runs) to handle those events. Today, delegation is entirely manual — users swipe cards to trigger runs.

This arc extends that model so:

1. Events can **auto-fire** matching prompts without user intervention (SKY-142, SKY-147).
2. Each run leaves a **durable memory file** so the next iteration on the same task knows what was tried and why (SKY-141).
3. CI failures re-fire **reliably** across retries, new commits, and newly-failing checks (SKY-140, SKY-144).
4. Agents can pull **full CI logs** into a scratch directory and read them with native tools (SKY-146).
5. Agents can **declare a task impossible**, tripping a per-task circuit breaker after repeated failures (SKY-148).
6. Trigger rules are **parameterizable** with predicates including the critical label-opt-in story (SKY-149).

Follow-ups: retention (SKY-145), bulk/parallel delegation (SKY-143 — design-only, exploratory).

The V1 demo story: _"A CI check fails on your PR. Triage Factory sees it, auto-delegates a CC run that pulls the failing logs, diagnoses the problem, commits a fix, and pushes. If CI fails again, a new run picks up where the last one left off using task memory. After 2 unsuccessful attempts it stops and surfaces the history to you for review."_

---

## Key design decisions (why things are the way they are)

### 1. Transition-based events, identity-based re-firing

Events fire on state changes, not on raw polls. The poller compares the current snapshot (`poller_state.state_json`) against the previous one and emits when something meaningful changed.

For CI failures specifically, after SKY-144 the "identity" is the per-check-run `id`, not the scalar `CIState` aggregate. This fixes two real bugs where the aggregate stayed `FAILURE` across retries and different-check failures, suppressing events we needed to fire. If you're touching CI event logic, **do not revert to scalar state transitions** — you'll re-introduce these bugs.

### 2. Fresh worktree per run; durable state in the DB

Worktrees are **destroyed after every run** (`internal/worktree/worktree.go`). Do not try to persist state in the worktree across runs — it won't work. The pattern for persistent state is:

1. Store it in the DB (source of truth)
2. Materialize it into the fresh worktree at startup as files
3. Agent reads/writes files normally
4. Spawner ingests new/modified files back to the DB on teardown

This is how SKY-141 (task memory) works, and it's how SKY-146 puts log archives under the managed scratch dir (`_tfac/`, `_scratch/` before it was renamed) inside the worktree.

### 3. Filesystem > tool proxy for agent data access

We do **not** build `--grep`, `--tail`, `--filter`, etc. flags on CLI commands the agent calls. We give agents files and let them use native Read/Grep/Glob tools. This applies to task memory (materialized markdown files) and CI logs (extracted archive directories).

The model has better tools for navigating files than we'd build into flags. Only exception: hard size caps that prevent pathological cases (e.g., 500 MB archive cap in SKY-146).

### 4. Stop hook doesn't fire in headless mode

Confirmed against Claude Code docs. Our pre-complete gate for SKY-141 is implemented externally via session-ID capture + `--resume`, **not** via the harness Stop hook. Same goes for `/loop` — it's a first-party CC feature but only works in interactive sessions, so it's not usable in our delegation path. Don't spend time trying to wire up hooks you saw in the docs; they won't fire from `claude -p`.

The resume helper built for SKY-141 is **reused by the open-run resume path** (waking a run whose turn ended without a conclusion). Design it as a reusable function in the spawner package, not inlined into any one caller.

### 5. Manual vs auto run distinction

Manual runs = human is in the loop and takes responsibility. Auto runs = system must enforce safety rails. This distinction is threaded through via `agent_runs.trigger_source` (`'manual' | 'auto'`, added in SKY-147).

The circuit breaker in SKY-148 only counts auto runs toward `consecutive_unsuccessful_runs`. Successful manual runs **do** reset the counter (success is success, regardless of origin), but failing manual runs are ignored. When implementing any counter/breaker logic, always filter on `trigger_source`.

### 6. "Only two acceptable reasons to stop"

Agents are taught in the envelope (SKY-148) that the only legitimate terminal states are:

1. Task completed
2. Task provably impossible (`status: "task_unsolvable"`)

This framing is essential because it gives the circuit breaker a clean signal — an agent running out of ideas should explicitly give up rather than loop forever on ambiguous failures. Don't soften this language.

Note the distinction between `task_unsolvable` (voluntary, agent-declared) and `failed` (involuntary, spawner-assigned when the run dies without valid JSON). Both count toward the breaker, but they're kept as distinct statuses because they have different debugging paths (`failed` usually means our prompt/envelope/env is broken; `task_unsolvable` means the agent genuinely can't).

### 7. `prompt_bindings` is untouched; `prompt_triggers` is new

`prompt_bindings` already exists and serves swipe UX (which prompt pre-fills the picker when a user taps delegate). **Do not modify it** in the auto-delegation work. `prompt_triggers` is a new table (SKY-142) for automation rules. Keeping them separate prevents swipe UX from regressing and keeps the concepts sharp.

### 8. No ALTER TABLE migrations

Per project convention: update `CREATE TABLE` in `internal/db/db.go` directly. Assume a full DB wipe is acceptable. Schema evolution is basically free — change the definition and move on. If you're tempted to add a backwards-compat shim, don't.

### 9. Local excludes, not committed gitignore

The managed scratch directory `_tfac/` (covering both `_tfac/entity-memory/` from SKY-141 and `_tfac/ci-logs/` from SKY-146) is added to `.git/info/exclude` at worktree creation, **not** to a committed `.gitignore`. We don't want to pollute the tracked repo with entries for our internal scratch dirs. One prefix exclude covers everything under it.

### 10. Build order

```bash
cd frontend && pnpm install --frozen-lockfile && pnpm run build && cd .. && go build -o ./triagefactory .
```

### 11. Same-task absorption: follow-up firings fold into a live run

When a trigger fires for an entity that **already has a live auto run**, the router doesn't always spawn a second run. The rule is **same-task absorption**: if the firing's _own task_ owns that live run, the new event folds into it — delivered as an **injection mid-run** (a fresh user message into the running conversation), and the standing claim carries over. Any _other_ firing — a different task's, or a busy state with no live run to fold into — defers onto `pending_firings` and runs when the entity frees up, exactly as before.

Two consequences if you're the agent working the run:

- **Your conversation can grow mid-run.** A run isn't a snapshot of one triggering event: more same-task events (a new Slack follow-up in the thread, a re-fired CI failure) can arrive as injected user messages while you're working. Treat a later injection as the current intent.
- **Absorbed means absorbed.** There's no separate compensation path for a folded-in event. If the run fails, that event fails with it, and the per-(entity, prompt) breaker owns repeated failure uniformly — the same posture as the original triggering event.

This replaced the old per-event-type `Additive` boolean (now deleted): absorption is one universal, task-scoped rule rather than a flag each new event type had to opt into, which also makes cross-task absorption (folding a follow-up into an _unrelated_ task's run) structurally impossible. Same task ⇒ same owning team, so no principal-boundary guard is needed. The gate lives in `internal/routing`'s `tryAutoDelegate` (`delegation.go`).

---

## Codebase orientation

Files you'll almost certainly need. **Verify each before editing** — line numbers drift:

| Path                               | Why                                                                                                                                                 |
| ---------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/ai/prompts/envelope.txt` | The agent contract. Injected into every delegated run. Read before editing any prompt behavior.                                                     |
| `internal/domain/prompt.go`        | `Prompt` and `PromptBinding` domain types.                                                                                                          |
| `internal/db/db.go`                | Schema. All table definitions live in `CREATE TABLE` statements here.                                                                               |
| `internal/db/event_types.go`       | Registered event types (CI failure, PR new commits, etc.).                                                                                          |
| `internal/db/prompts.go`           | Existing prompt DB helpers — template for new `prompt_triggers` helpers.                                                                            |
| `internal/tracker/tracker.go`      | Event emission and `CreateEvent` — the hook point for SKY-147.                                                                                      |
| `internal/tracker/diff.go`         | Snapshot-to-snapshot diff logic. SKY-140 and SKY-144 both edit this. CI failure emission was around lines 48-60 at writing time.                    |
| `internal/github/status.go`        | GitHub API client. Around line 91 is where check runs are fetched — see SKY-144 for what's already in the response that we're currently discarding. |
| `internal/delegate/spawner.go`     | Delegation entry point. `parseAgentResult` was around line 522 at writing time; completion handling around line 363.                                |
| `internal/worktree/worktree.go`    | Worktree lifecycle. `MakeRunCwd`, cleanup, `RemoveClaudeProjectDir`.                                                                                |

**Prompts live in both places.** System prompts exist as `.txt` files under `internal/ai/prompts/` AND as rows in the `prompts` table with `source = 'system'`. User prompts are DB-only. When you add a new system prompt (e.g., SKY-147's `ci-fix.txt`), you need both the file and the seed row in DB initialization.

**The envelope is always injected by the spawner.** It's not part of any prompt body. When editing `envelope.txt`, you're editing the thing every single delegation sees. Prompt bodies are just the mission.

---

## Cross-ticket shared infrastructure

Several tickets touch the same plumbing. You can assume you're the only agent making changes at any given point in time, but don't assume the ticket reflects the current state of the project. If you're working on something, be aware of current state so you don't write the same thing twice:

### Session ID capture + resume helper

- **Built in**: SKY-141
- **Reused by**: the open-run resume path (waking a run that ended a turn without a conclusion)
- Capture `session_id` from `claude -p --output-format json` stdout. Store on `agent_runs.session_id`. Build the resume helper as a standalone function in the spawner package so the resume path can call it directly later.

### Managed scratch directory convention

- **Built in**: SKY-141 (for `entity-memory/`) and SKY-146 (for `ci-logs/`)
- Subsumed under a single prefix in SKY-219 — `_scratch/` then, `_tfac/` now (the old name stays excluded so a worktree built by an older binary can't leak leftovers into a commit). One `.git/info/exclude` entry covers everything under it.

### `trigger_source` column

- **Added in**: SKY-147
- **Read by**: SKY-148 (counter logic) and UI (filtering)
- Populate it correctly for both manual and auto runs from day one (not just auto), so SKY-148's filter works cleanly.

### `task_memory` rows for all terminal states

- **Built in**: SKY-141 (including system-authored stub for involuntary `failed` runs)
- **Relied on by**: SKY-148 (the breaker UI links to task_memory entries including crashed-run stubs)
- Don't skip the stub-writing branch. Every terminal run must produce a `task_memory` row or downstream UI breaks.

### Reading prior memory: the `## Human feedback (post-run)` block

When you read materialized `_tfac/entity-memory/**/*.md` files from a prior run on the same entity (or a linked entity), you may see a section like:

```
## Human feedback (post-run)

**Outcome:** Human submitted the review with edits.
**Verdict changed:** agent drafted APPROVE, human submitted REQUEST_CHANGES.

**Body:** Edited.
> [final body verbatim]
>
> **Originally drafted as:**
> [original body verbatim]

**Comment edits:**

- `internal/db/db.go:447` — edited
  - Was: this index might not be necessary
  - Now: drop this index — it duplicates idx_runs_status
```

This section is **authoritative** about what the human actually wanted, and outranks the agent's own self-report above it. Treat it as a calibration signal, not a footnote. Specifically:

- A `**Verdict changed:**` line means the prior agent's reading of severity was off. **Reconsider** before mirroring the prior verdict — the human flipped APPROVE↔REQUEST_CHANGES for a reason.
- A comment edited with **substantial rewording** indicates a tone or framing the human prefers. Mirror that tone in your own comments.
- Absence of this block doesn't mean "no human input" — it just means the human hasn't submitted (yet) or the run pre-dated this feature (SKY-205).

The block is generated programmatically (no LLM in the loop). Format is fixed; if you need to parse it, the literal `## Human feedback (post-run)` heading is a stable anchor and the bold labels (`**Outcome:**`, `**Verdict:**` / `**Verdict changed:**`, `**Body:**`, `**Comment edits:**`) won't move.

### SKY-147 / SKY-148 ordering wrinkle

SKY-148 adds columns (`consecutive_unsuccessful_runs`, `auto_delegate_suspended`, `auto_delegate_globally_enabled`) that SKY-147's hook reads. Linear has 148 blocked by 147 because the breaker only _matters_ once auto-fire exists, but the **data layer** of 148 wants to land first so 147's gates read real columns.

Two valid orderings:

- **Option A** — Land SKY-148's schema + column writes first (even though the column values don't "matter" until auto-fire lands), then SKY-147 can read them cleanly.
- **Option B** — Land SKY-147 with NOP-until-exists fallbacks in the gate checks, then SKY-148 fills in real values.

Pick whichever is cleaner when you get there. Document the choice in the ticket comments.

---

## Recommended implementation order

Sequential, one dev:

1. **SKY-140** — `head_sha` tracking. Independent, small, foundational.
2. **SKY-144** — structured check runs + CI re-trigger fix. Needs 140.
3. **SKY-141** — task memory with write-before-finish gate. Ships value standalone (manual delegations benefit immediately).
4. **SKY-142** — `prompt_triggers` table. Independent.
5. **SKY-146** — log download CLI. Needs 144.
6. **SKY-148** — task_unsolvable + circuit breaker. Schema first per the ordering note above.
7. **SKY-147** — auto-delegation hook + default CI-fix trigger. Needs 141, 142, 144, 146, and ideally 148's schema.
8. **SKY-149** — scope predicates + label opt-in. Needs 147.

Follow-ups:

- **SKY-145** — retention (after 141 has been running a while)
- **SKY-143** — bulk/parallel (design-only, exploratory)

You can land 140, 141, 142 in any order relative to each other — all three are independently valuable and unblock downstream work.

---

## Gotchas and things that have burned us before

1. **Don't modify `prompt_bindings`.** It's for swipe UX; automation is a new table. Muddling them breaks the picker.

2. **Don't use `ALTER TABLE`.** Update `CREATE TABLE` directly and assume a full DB wipe.

3. **Verify the memory file actually exists before marking a run complete.** The write-before-finish gate in SKY-141 only works if the spawner re-checks the directory with `os.Stat`. Don't trust the agent's word that it wrote the file.

4. **The agent JSON response is parsed from the tail of the output**, not the entire stream. See `parseAgentResult` in `internal/delegate/spawner.go`. When adding fields (e.g., `status: "task_unsolvable"`), update the struct and the parser together.

5. **Worktree cleanup deletes `~/.claude/projects/<cwd-hash>/` too** (commit `ba1df6b`). Preserve that behavior; removing it leaks ghost sessions.

6. **Don't commit memories or scratch files.** Local exclude via `.git/info/exclude` at worktree creation. The envelope also tells the agent not to commit them — belt and suspenders.

7. **System prompts need BOTH a file AND a DB seed row.** If you add `internal/ai/prompts/ci-fix.txt`, you also need to seed a `prompts` row with `source = 'system'` at DB initialization. Missing either half results in a prompt that doesn't exist or a dangling reference.

8. **Check the Claude Code docs before assuming a harness feature works headless.** Confirmed non-headless: `/loop` (scheduled-tasks.md), Stop hooks (hooks.md). When in doubt, verify against https://code.claude.com/docs/en/ before designing against it.

9. **The CI-fix prompt must not force-push or merge.** The envelope forbids it at a platform level; the CI-fix prompt reiterates it. Don't weaken either guard. Pushing to the PR branch you're already on is the only permitted write.

10. **Stale references in Linear tickets.** These tickets were written on 2026-04-11. File paths, line numbers, and function names were correct at writing time but will drift. Before acting on any "edit the thing at file:line" instruction, grep/read the file to confirm the target is still there. If you find something has moved, update the ticket body in Linear as part of your work so the next agent doesn't hit the same stale reference.

---

## Agent return contract (for SKY-141 and SKY-148 in particular)

Current contract (in `internal/ai/prompts/envelope.txt`):

```json
{ "status": "completed", "summary": "...", "links": {} }
```

After SKY-148: `status` can also be `"task_unsolvable"`. The spawner-assigned `"failed"` state is never returned by the agent — it's set by the spawner when no valid JSON arrives.

After SKY-141: the agent is required to have written its run memory on disk **before** returning its completion JSON — at the fixed path `./_tfac/memory.md`, which the orchestrator reads at termination and files under the run's ids. The spawner verifies this and auto-resumes the session once with a correction message if the file is missing. This happens externally via `--resume`, not via hooks.

---

## If you're stuck or the ticket seems wrong

Don't silently work around a ticket that doesn't match reality. The design of this arc went through several iterations and the tickets are the artifact, but the codebase is the ground truth. If you find:

- A file reference that no longer exists or has been renamed
- A design decision that contradicts something you're reading in the code
- A dependency that turned out to be wrong
- A gotcha listed here that no longer applies

...then update the Linear ticket (and this briefing, if applicable) as part of your work. The next agent will thank you.

When the ticket body and the code disagree, **believe the code** and update the ticket — not the other way around.
