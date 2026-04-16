# Triage Factory data model — target state

**Status:** Design draft. Review before we rewrite the 158 sub-tickets.

## Philosophy

Four levels, each with its own identity and lifecycle. Every arrow is 1:many.

```
Entity          (PR #18, Jira SKY-123, Linear TAS-42)
  ↓
Events          (ci_check_failed @ 14:02, new_commits @ 14:15, review_changes_requested @ 15:30)
  ↓  (0 or 1 — only if a task_rule or prompt_trigger's predicate matches)
Task            (the situation that needs attention)
  ↓
Runs            (N per task — retries, or parallel prompts; serialized in v1)
```

**Core principles:**

- **Entities are durable.** They live from poller-first-seen until closed/merged. All other state hangs off them.
- **Events are immutable history.** Every poller detection and every system emission lands here. Append-only. No retroactive reshaping.
- **Tasks are ephemeral focus.** A task is "right now, this entity needs attention because of this event type." It starts when a rule or trigger matches, ends when closed.
- **Runs are the work.** One agent execution against one task. Memory gets written per-run but materialized per-entity (via the entity-link graph).
- **Links are first-class.** "PR #18 implements Jira SKY-123" is modeled in `entity_links`, not stuffed into metadata.
- **Parameterize, don't split.** One `new_commits` event type with rich metadata. Predicates filter. We don't proliferate event types like `own_new_commits_draft` to encode combinations — that's a Cartesian product waiting to happen.
- **No retroactive task creation.** A new task_rule or trigger applies to events going forward only. Historical events in the log are not re-evaluated.

## Tables

### `entities`

Long-lived source entities (PRs, issues, eventually Slack threads).

| column           | type          | notes                                                            |
| ---------------- | ------------- | ---------------------------------------------------------------- |
| `id`             | TEXT PK       | UUID                                                             |
| `source`         | TEXT NOT NULL | `github`, `jira`, `linear`, `slack`                              |
| `source_id`      | TEXT NOT NULL | `owner/repo#18`, `SKY-123`, etc.                                 |
| `kind`           | TEXT NOT NULL | `pr`, `issue`, `epic`, `message`                                 |
| `title`          | TEXT          | current title (updated on polls)                                 |
| `url`            | TEXT          | canonical link out                                               |
| `snapshot_json`  | TEXT          | opaque poller state: head_sha, CI, draft, assignee, labels, etc. |
| `state`          | TEXT NOT NULL | `active` / `closed`                                              |
| `created_at`     | DATETIME      | first polled/seen                                                |
| `last_polled_at` | DATETIME      | most recent poll that saw it                                     |
| `closed_at`      | DATETIME      | nullable                                                         |

**Unique index:** `(source, source_id)`.
**Index:** `(state)` for active-entity queries; `(source, last_polled_at)` for poller scheduling.

Replaces today's `tracked_items` entirely.

### `entity_links`

Relationships between entities. Cross-source or within-source.

| column           | type                                     | notes                                                                         |
| ---------------- | ---------------------------------------- | ----------------------------------------------------------------------------- |
| `from_entity_id` | TEXT FK → entities(id) ON DELETE CASCADE |                                                                               |
| `to_entity_id`   | TEXT FK → entities(id) ON DELETE CASCADE |                                                                               |
| `kind`           | TEXT NOT NULL                            | `implements` (PR → ticket), `parent` (epic → subtask), `related`              |
| `origin`         | TEXT NOT NULL                            | how we learned about the link: `branch_name`, `body_mention`, `agent`, `user` |
| `created_at`     | DATETIME                                 |                                                                               |

**Primary key:** `(from_entity_id, to_entity_id, kind)`.
**Index:** `(from_entity_id, kind)`, `(to_entity_id, kind)`.

Directional. Memory gathering and predicate evaluation walks both directions (union query). One-hop traversal for v1.

Link discovery sources:

- Branch name contains ticket key (e.g., `feature/SKY-123`) → auto-create `implements` link
- Ticket body/comments contain PR URL → auto-create
- Implementation agent writes the link explicitly when opening a PR (best source)
- User action in UI → `origin='user'`

### `events`

Every poller-detected change or system-emitted signal. Append-only.

| column          | type                   | notes                                                        |
| --------------- | ---------------------- | ------------------------------------------------------------ |
| `id`            | TEXT PK                | UUID                                                         |
| `entity_id`     | TEXT FK → entities(id) | nullable (system events without entity context)              |
| `event_type`    | TEXT NOT NULL          | `github:pr:ci_check_failed`, `system:prompt:auto_suspended`, etc. |
| `dedup_key`     | TEXT NOT NULL DEFAULT '' | open-set discriminator value (label name, status name); empty for events that dedup purely on event_type. See "Dedup key" in routing section. |
| `metadata_json` | TEXT                   | structured detail, shape defined by per-event-type Go struct |
| `created_at`    | DATETIME               |                                                              |

**Index:** `(entity_id, created_at DESC)`, `(event_type, created_at DESC)`.

Events are immutable once written. No idempotency: duplicate emissions from the poller (retries, etc.) produce duplicate rows, and that's fine — downstream routing is idempotent because bumping a task with the same metadata is a no-op.

### `events_catalog`

Read-only system registry of event types. Seeded on startup, never user-editable. Purely presentation metadata — behavioral and user-preference concerns live on `task_rules` and `prompt_triggers`; event cascades are hardcoded in `internal/events/cascades.go`.

| column        | type          | notes                                         |
| ------------- | ------------- | --------------------------------------------- |
| `id`          | TEXT PK       | the event_type string                         |
| `source`      | TEXT NOT NULL | `github`, `jira`, `linear`, `slack`, `system` |
| `category`    | TEXT NOT NULL | `pr`, `issue`, `review`, etc.                 |
| `label`       | TEXT NOT NULL | human-readable                                |
| `description` | TEXT NOT NULL | tooltip copy                                  |

Replaces today's `event_types`. Deliberately does **not** include `enabled`, `task_action`, `default_priority`, or `sort_order` — those concerns belong to `task_rules` and `prompt_triggers`. If nothing subscribes (no rule, no trigger, no hardcoded cascade), an event is effectively invisible; no global on/off switch needed.

### `task_rules`

Declarative rules for creating tasks from events. Independent of automation. A user who wants "surface these events as triage cards without auto-firing anything" configures a `task_rule` and no matching `prompt_trigger`.

| column                 | type                                                     | notes                                                              |
| ---------------------- | -------------------------------------------------------- | ------------------------------------------------------------------ |
| `id`                   | TEXT PK                                                  | UUID                                                               |
| `event_type`           | TEXT FK → events_catalog(id) ON DELETE RESTRICT NOT NULL | e.g., `github:pr:new_commits`                                      |
| `scope_predicate_json` | TEXT                                                     | typed per event type; null = match-all                             |
| `enabled`              | BOOLEAN NOT NULL                                         | master toggle                                                      |
| `name`                 | TEXT NOT NULL                                            | user-facing label                                                  |
| `default_priority`     | REAL NOT NULL DEFAULT 0.5                                | 0.0 – 1.0, AI scorer uses this as a prior for tasks from this rule |
| `sort_order`           | INTEGER NOT NULL DEFAULT 0                               | UI ordering of task categories in queue/board                      |
| `source`               | TEXT NOT NULL                                            | `system` (seeded) / `user`                                         |
| `created_at`           | DATETIME                                                 |                                                                    |
| `updated_at`           | DATETIME                                                 |                                                                    |

**Index:** `(event_type) WHERE enabled = 1`.

**Seeded defaults (v1):**

| event_type                          | predicate              | why                                                              |
| ----------------------------------- | ---------------------- | ---------------------------------------------------------------- |
| `github:pr:ci_check_failed`         | `{AuthorIsSelf: true}` | failed checks on my own PRs need triage; ignore others' CI noise |
| `github:pr:review_changes_requested` | `{AuthorIsSelf: true}` | a reviewer is blocking my PR — I need to act                     |
| `github:pr:review_commented`        | `{AuthorIsSelf: true}` | someone left non-blocking comments on my PR — I should look      |
| `github:pr:review_requested`        | (null — match-all)     | someone asked for my review — always surface                     |
| `jira:issue:assigned`               | (null — match-all)     | assignment to me is intrinsic — always surface                   |

Users can disable or narrow any seeded rule, and adjust `default_priority` / `sort_order` to tune their queue. Predicate-driven events where the default scope isn't obvious (`new_commits`, `label_added`) ship with **no** default rule — user configures one if they want manual-triage surfacing, or a trigger's predicate implicitly handles it via the forgiving path.

### `prompt_triggers`

Automation rules. When a trigger's predicate matches an event, fire the bound prompt against the task that resulted (or implicitly create a task if no matching rule exists — see "Forgiving path" below).

| column                     | type                                                     | notes                                                                                                                                                    |
| -------------------------- | -------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `id`                       | TEXT PK                                                  | UUID                                                                                                                                                     |
| `prompt_id`                | TEXT FK → prompts(id) NOT NULL                           |                                                                                                                                                          |
| `trigger_type`             | TEXT NOT NULL                                            | `event` (v1 only accepts this)                                                                                                                           |
| `event_type`               | TEXT FK → events_catalog(id) ON DELETE RESTRICT NOT NULL |                                                                                                                                                          |
| `scope_predicate_json`     | TEXT                                                     | typed per event type; null = match-all                                                                                                                   |
| `breaker_threshold`        | INTEGER NOT NULL DEFAULT 4                               | consecutive failures on same (entity, prompt_id) before auto-suspending the trigger                                                                      |
| `cooldown_seconds`         | INTEGER NOT NULL DEFAULT 60                              | min time between auto-fires per entity                                                                                                                   |
| `min_autonomy_suitability` | REAL NOT NULL DEFAULT 0.0                                | 0.0 – 1.0; when > 0, fire is deferred until AI scorer produces a suitability score and it meets this threshold. Default 0.0 = fire immediately, no gate. |
| `enabled`                  | BOOLEAN NOT NULL                                         |                                                                                                                                                          |
| `created_at`               | DATETIME                                                 |                                                                                                                                                          |
| `updated_at`               | DATETIME                                                 |                                                                                                                                                          |

**Unique index:** `(prompt_id, event_type, trigger_type)` — one trigger per prompt-event combo.
**Index:** `(event_type) WHERE enabled = 1`, `(prompt_id, created_at)`.

**Implicit task creation (forgiving trigger path):** when a trigger fires and no `task_rule` matches, the task is created with `priority_score = 0.5` as a cold-start prior. The AI scorer overwrites this shortly after creation, so the prior only matters for the brief window before scoring completes.

### Task close paths

There is no generalized cascade map. Tasks close via three explicit paths:

1. **Run completion** — when a run terminates successfully on a task, that task closes (`close_reason=run_completed`). Already covered by the run lifecycle.
2. **Entity lifecycle** — when an entity transitions to `closed` (e.g., `pr:merged`, `jira:issue:completed`), the state machine closes every active task on that entity (`close_reason=entity_closed`). Uniform; doesn't enumerate event types. See `entities` state machine below.
3. **Inline narrow checks inside event handlers** — when a raw event arrives that *might* resolve a sibling task on the same entity, the handler does a targeted query and closes if appropriate. Each check is small, targeted, and lives next to the event that could trigger it. v1 examples:

   - `ci_check_passed` handler: query "any failing check-runs remain on this entity at the latest SHA?" — if no, close active `ci_check_failed` tasks on the entity (`close_reason=auto_closed_by_event`, `close_event_type=github:pr:ci_check_passed`).
   - `review_approved` / `review_commented` / `review_dismissed` handler: if this reviewer's most recent prior review on the same PR was `changes_requested` AND no other reviewer currently has an outstanding `changes_requested`, close active `review_changes_requested` tasks on the entity.
   - `review_submitted{reviewer_is_self: true}` handler: close any active `review_requested` task on the same entity (I submitted my review → the request is satisfied).
   - `jira:issue:assigned{assignee_is_self: false}` handler: close any active `jira:issue:assigned` or `jira:issue:available` task on the same entity. Covers two cases: an issue I was assigned got reassigned to someone else ("my" task is stale), and an unassigned issue in my pickup queue got claimed by someone else ("available" task is stale). The issue itself is still open in Jira, so entity lifecycle doesn't help here — this is strictly "my context on this changed."

Anything not covered by one of these paths stays open until the user dismisses it or a run handles it. We deliberately don't try to auto-close every situation — when in doubt, leave the task surfaced.

**Principle: events are per-action signals, not aggregate state transitions.** A review event fires once per individual review (different reviewers, re-reviews by the same person, all fire). A check-completion event fires once per check-run completion. `new_commits` fires once per push batch. The model favors low-level events with rich metadata over derived aggregate-state events, because aggregate state is whackamole — every edge case (dismissed reviews, superseded reviews, repo policies on approvals) becomes a question for the aggregator. We push that complexity into the small inline close-checks above, where it's scoped to one handler at a time.

**Splitting events: discriminator vs. attribute.** Per-action events are split when a discriminator on the underlying action changes whether/how the situation needs attention. Two cases:

- **Closed-enum discriminators** (CI conclusion: `failure` / `success`; review type: `approved` / `changes_requested` / `commented` / `dismissed`) → **split into separate event types.** The enum is small, stable, and typed event constants are self-documenting (`EventGitHubPRReviewApproved` reads better than `EventGitHubPRReviewReceived` + `Type: "approved"` predicate). So `ci_check_failed` / `ci_check_passed` are separate event types, as are `review_changes_requested` / `review_approved` / `review_commented` / `review_dismissed`.
- **Open-set discriminators** (label name, Jira status, Linear status, project-configurable priority value) → **single event type with `dedup_key`** (see "Dedup key" below). The set is user/repo-defined and unbounded; you can't pre-enumerate event types for it.

Attributes that don't change the *kind* of situation — reviewer identity, check name, repo, author — stay as metadata fields filterable by predicate. This keeps "parameterize don't split" intact for the Cartesian-product cases (`new_commits` doesn't fragment by author/draft/repo) while letting situation-changing closed-enum discriminators be first-class event types where dedup `(entity, event_type, dedup_key)` cleanly separates concerns.

**Dedup key (open-set discriminators).** Some events have a discriminator the schema can't enumerate at compile time (label name, status name). Adding a label `urgent` is a different situation than adding `wontfix` — both are `label_added` events but they shouldn't dedup together. To handle this, every event carries a `dedup_key TEXT` column (default empty string), and tasks dedup on `(entity_id, event_type, dedup_key)` instead of just `(entity_id, event_type)`.

| event_type                       | dedup_key (set by emitter)         | effect                                                            |
| -------------------------------- | ---------------------------------- | ----------------------------------------------------------------- |
| `github:pr:ci_check_failed`      | `""` (empty)                       | one "CI failing" task per PR, bumped per failing check            |
| `github:pr:review_changes_requested` | `""`                           | one "blocked" task per PR, bumped per blocking review             |
| `github:pr:label_added`          | label name (`"self-review"`)       | one task per (PR, label) — different labels = different situations |
| `github:pr:label_removed`        | label name                         | one task per (PR, label) for the removal                          |
| `jira:issue:status_changed`      | new status name (`"In Review"`)    | one task per (issue, target status); bouncing back to same status bumps existing |
| `github:pr:new_commits`          | `""`                               | one "new commits" task per PR                                     |

Most events leave `dedup_key` empty — the event type itself is the unit of "what makes a situation distinct." Open-set events override at emission time. The split rule above (closed-enum discriminators get their own event_type) means `dedup_key` is rarely the right answer for those — only the genuinely open-set cases.

### `tasks`

An actionable situation — spawned by a rule or trigger match on an event, lives in the user's queue/board.

| column                 | type                            | notes                                                                                                                |
| ---------------------- | ------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `id`                   | TEXT PK                         | UUID                                                                                                                 |
| `entity_id`            | TEXT FK → entities(id) NOT NULL |                                                                                                                      |
| `event_type`           | TEXT NOT NULL                   | the type of event that spawned it — used for dedup                                                                   |
| `dedup_key`            | TEXT NOT NULL DEFAULT ''        | inherited from the primary event; participates in the partial unique index below                                     |
| `primary_event_id`     | TEXT FK → events(id) NOT NULL   | the specific event that spawned this task                                                                            |
| `status`               | TEXT NOT NULL                   | `queued` / `claimed` / `delegated` / `done` / `dismissed` / `snoozed`                                                |
| `priority_score`       | REAL                            | AI-assigned                                                                                                          |
| `ai_summary`           | TEXT                            | AI-generated card summary                                                                                            |
| `autonomy_suitability` | REAL                            | AI's estimate of how suitable this task is for autonomous agent handling (0-1)                                       |
| `priority_reasoning`   | TEXT                            | AI's justification                                                                                                   |
| `scoring_status`       | TEXT NOT NULL                   | `pending` / `in_progress` / `scored`                                                                                 |
| `severity`             | TEXT                            | derived from event/entity                                                                                            |
| `relevance_reason`     | TEXT                            | why this was surfaced                                                                                                |
| `source_status`        | TEXT                            | captured for undo (e.g., Jira ticket's prior status)                                                                 |
| `snooze_until`         | DATETIME                        | nullable                                                                                                             |
| `close_reason`         | TEXT                            | enum: `run_completed` / `user_claimed` / `user_dismissed` / `auto_closed_by_event` / `entity_closed`                 |
| `close_event_type`     | TEXT FK → events_catalog(id)    | nullable; populated when `close_reason = 'auto_closed_by_event'` to identify which event triggered the cascade close |
| `closed_at`            | DATETIME                        | nullable                                                                                                             |
| `created_at`           | DATETIME                        |                                                                                                                      |

**Partial unique index:** `(entity_id, event_type, dedup_key) WHERE status NOT IN ('done', 'dismissed')` — enforces at most one **active** task per `(entity, event_type, dedup_key)` triple across all non-terminal statuses. Most events use `dedup_key=''` so this collapses to "one task per `(entity, event_type)`"; open-set discriminator events (label_added, status_changed) get separate tasks per discriminator value. Bump semantics handled via `task_events` junction.

**Indexes:** `(status)`, `(entity_id)`, `(status, priority_score DESC)` for queue ordering.

### `task_events`

Junction: every event that contributed to a task. The `kind` says how.

| column       | type                                   | notes                           |
| ------------ | -------------------------------------- | ------------------------------- |
| `task_id`    | TEXT FK → tasks(id) ON DELETE CASCADE  |                                 |
| `event_id`   | TEXT FK → events(id) ON DELETE CASCADE |                                 |
| `kind`       | TEXT NOT NULL                          | `spawned` / `bumped` / `closed` |
| `created_at` | DATETIME                               |                                 |

**Primary key:** `(task_id, event_id)`.
**Index:** `(task_id)`, `(event_id)`.

Every task has exactly one `spawned` row (== `tasks.primary_event_id`, denormalized for fast lookup). `bumped` rows record subsequent matching events while the task was active (any non-terminal status). `closed` rows record the event that auto-closed the task (if applicable).

**Snooze wake-on-bump:** if an event bumps a task in `snoozed` status, the router also un-snoozes it (sets `status=queued`, clears `snooze_until`). The snooze's premise — "nothing new happening, check back later" — is invalidated by the new event.

### `runs`

One prompt execution against one task (Claude Code invocation).

| column           | type                           | notes                                                                                                                                                                                    |
| ---------------- | ------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `id`             | TEXT PK                        | UUID                                                                                                                                                                                     |
| `task_id`        | TEXT FK → tasks(id) NOT NULL   |                                                                                                                                                                                          |
| `prompt_id`      | TEXT FK → prompts(id) NOT NULL |                                                                                                                                                                                          |
| `trigger_id`     | TEXT FK → prompt_triggers(id)  | nullable for manual                                                                                                                                                                      |
| `trigger_type`   | TEXT NOT NULL                  | `manual` / `event`                                                                                                                                                                       |
| `status`         | TEXT NOT NULL                  | `cloning` / `fetching` / `worktree_created` / `agent_starting` / `running` / `yielded_to_user` / `pending_pr_review_approval` / `completed` / `failed` / `task_unsolvable` / `cancelled` |
| `model`          | TEXT                           |                                                                                                                                                                                          |
| `session_id`     | TEXT                           | Claude Code `session_id` for `--resume`                                                                                                                                                  |
| `worktree_path`  | TEXT                           |                                                                                                                                                                                          |
| `result_summary` | TEXT                           | agent's own summary of what it did                                                                                                                                                       |
| `stop_reason`    | TEXT                           |                                                                                                                                                                                          |
| `started_at`     | DATETIME                       |                                                                                                                                                                                          |
| `completed_at`   | DATETIME                       | nullable                                                                                                                                                                                 |
| `duration_ms`    | INTEGER                        | nullable                                                                                                                                                                                 |
| `num_turns`      | INTEGER                        | nullable                                                                                                                                                                                 |
| `total_cost_usd` | REAL                           | nullable                                                                                                                                                                                 |
| `memory_missing` | BOOLEAN NOT NULL DEFAULT 0     | true if write-before-finish gate exhausted                                                                                                                                               |

**Indexes:** `(task_id)`, `(prompt_id, started_at DESC)`, `(trigger_id)`, `(status)`.

Renamed from `agent_runs`. Artifacts promoted to their own table (below).

### `run_artifacts`

Structured output produced by a run — PRs opened, reviews posted, branches pushed, tickets transitioned, comments added.

| column          | type                        | notes                                                          |
| --------------- | --------------------------- | -------------------------------------------------------------- |
| `id`            | TEXT PK                     | UUID                                                           |
| `run_id`        | TEXT FK → runs(id) NOT NULL | ON DELETE CASCADE                                              |
| `kind`          | TEXT NOT NULL               | noun form, source-namespaced — see vocabulary below            |
| `url`           | TEXT                        | nullable — not all artifacts have URLs                         |
| `title`         | TEXT                        | human-readable label for UI display                            |
| `metadata_json` | TEXT                        | optional, kind-specific detail — free-form, no required fields |
| `is_primary`    | BOOLEAN NOT NULL DEFAULT 0  | the "headline" artifact for UI display                         |
| `created_at`    | DATETIME                    |                                                                |

**Partial unique index:** `(run_id) WHERE is_primary = 1` — exactly one headline per run, enforced at the DB layer.
**Indexes:** `(run_id)`, `(kind, created_at DESC)` for per-kind analytics.

Primary artifact is what the UI links to on the run card. Others are listed in the run detail drawer.

#### Kind vocabulary

`kind` names nouns (what the artifact IS), not verbs (what the agent did with it). Source-namespaced like event types:

- `github:pr` — a pull request
- `github:review` — a PR review (with body and/or inline comments)
- `github:branch` — a named branch (pushed, created, or referenced)
- `github:comment` — an issue or PR comment
- `github:commit` — a specific commit
- `jira:issue` — a Jira ticket
- `jira:comment` — a Jira comment
- `linear:issue` — a Linear ticket
- `linear:comment` — a Linear comment
- `link` — generic URL fallback when nothing else fits

`metadata_json` is free-form and kind-specific. Optional. The UI doesn't render it by default — if a prompt wants to capture extra context (PR number, branch name, issue key), it can, but there's no schema enforcement. Keeps the envelope simple.

#### Agent completion JSON

Runs terminate by the agent emitting a completion JSON. Agent output has exact parity with `run_artifacts` rows — four fields, no operation verb:

```json
{
  "status": "completed" | "failed" | "task_unsolvable",
  "summary": "what I did and why",
  "artifacts": [
    {
      "kind": "github:pr",
      "url": "https://github.com/owner/repo/pull/42",
      "title": "PR #42: implement feature X",
      "is_primary": true
    },
    {
      "kind": "github:branch",
      "url": null,
      "title": "feature/SKY-123"
    },
    {
      "kind": "github:comment",
      "url": "https://github.com/owner/repo/pull/42#issuecomment-555",
      "title": "Response to reviewer @alice"
    }
  ]
}
```

Spawner behavior:

- INSERT one row per artifact, filling in `id`, `run_id`, `created_at`.
- If the agent emits artifacts but flags none as primary, parser auto-promotes the first one.
- Partial unique index enforces at most one primary per run at the DB level.

### `run_messages`

Per-run log of assistant/tool messages. Unchanged from today's `agent_messages` except for the rename and the FK rename (`run_id` instead of `agent_run_id`).

### `run_memory`

Per-run memory files, queried by entity (via JOIN through runs/tasks) — entity denormalized for fast lookup.

| column       | type                            | notes                                       |
| ------------ | ------------------------------- | ------------------------------------------- |
| `id`         | TEXT PK                         | UUID                                        |
| `run_id`     | TEXT FK → runs(id) NOT NULL     |                                             |
| `entity_id`  | TEXT FK → entities(id) NOT NULL | denormalized for fast entity-scoped queries |
| `content`    | TEXT NOT NULL                   | agent-authored markdown                     |
| `created_at` | DATETIME                        |                                             |

**Indexes:** `(entity_id, created_at ASC)` for chronological materialization; `(run_id)`.

### `prompts`

Unchanged from today.

### `swipe_events`

User swipe interaction log. Stays separate from `events` — different subject (UI interaction, not entity state), different consumers (scorer, analytics — not router, triggers).

| column          | type                         | notes                                       |
| --------------- | ---------------------------- | ------------------------------------------- |
| `id`            | INTEGER PK AUTOINCREMENT     |                                             |
| `task_id`       | TEXT FK → tasks(id) NOT NULL |                                             |
| `action`        | TEXT NOT NULL                | `claim` / `delegate` / `dismiss` / `snooze` |
| `hesitation_ms` | INTEGER                      | time from card shown to swipe registered    |
| `created_at`    | DATETIME                     |                                             |

**Indexes:** `(task_id)`, `(action, created_at)` for per-action aggregates.

### `poller_state`, `repo_profiles`, `pending_reviews`, `pending_review_comments`, `preferences`

All unchanged structurally. Adapt FKs where they reference `task_id` or the renamed `agent_runs`.

---

## What goes away

- `tracked_items` → fully replaced by `entities`
- `agent_runs` → renamed to `runs`
- `agent_messages` → renamed to `run_messages`
- `task_memory` → renamed to `run_memory` with denormalized `entity_id`
- `event_types` → renamed to `events_catalog`; drops `enabled`, `task_action`, `default_priority`, `sort_order` (behavior moved to `task_rules` / `prompt_triggers`; priority + ordering moved to `task_rules`). Now a read-only system registry.
- **Aggregate-state events are gone.** `pr:ci_failed` / `pr:ci_passed` (aggregate) → replaced by per-check `pr:ci_check_failed` / `pr:ci_check_passed`. `pr:changes_requested` (aggregate) and `pr:approved` → replaced by per-action `pr:review_changes_requested` / `pr:review_approved` / `pr:review_commented` / `pr:review_dismissed`. `pr:changes_resolved` → gone. `pr:review_submitted` stays as a per-action event meaning "I reviewed someone's PR" (inverse of the per-reviewer events on my own PRs).
- **No derived events.** No `pr_mergeable`, no `ci_passing` aggregate event. Anywhere we'd be tempted to compute aggregate state into an event, we instead do an inline targeted query inside the relevant raw-event handler. See "Task close paths."
- **New per-action event types:** `pr:ci_check_failed{name, head_sha, ...}`, `pr:ci_check_passed{name, head_sha, ...}`, `pr:review_changes_requested{reviewer, ...}`, `pr:review_approved{reviewer, ...}`, `pr:review_commented{reviewer, ...}`, `pr:review_dismissed{reviewer, ...}`, `pr:label_added{label_name, ...}`, `pr:label_removed{label_name, ...}`.
- **Discriminator-split rule.** Event types split on discriminators that change whether/how the situation needs attention (conclusion, review type), not on attributes (reviewer, check name, repo, label name) that just narrow the same situation. See "Splitting events" principle in "Task close paths."
- **Cascade map gone.** Replaced by three close paths: run completion, entity lifecycle, and inline narrow checks inside specific event handlers. See "Task close paths" above.
- `tasks.consecutive_unsuccessful_runs` → dropped, breaker becomes a query
- `tasks.agent_confidence` → renamed to `autonomy_suitability`
- `runs.result_link` → dropped, promoted to `run_artifacts` table
- `prompt_bindings` (already removed prior to this refresh)

---

## Event routing

Three independent systems evaluated in order when an event arrives:

### 1. Task creation

On event arrival:

```
if event.entity_id is not null AND entity.status != 'active':
    skip task creation (don't spawn tasks on closed entities)

enabled_rules    = task_rules WHERE event_type = ? AND enabled = 1
enabled_triggers = prompt_triggers WHERE event_type = ? AND enabled = 1

matched = []
for rule in enabled_rules:
    if rule.predicate matches event.metadata:
        matched.append(rule)
for trigger in enabled_triggers:
    if trigger.predicate matches event.metadata:
        matched.append(trigger)

if matched is non-empty:
    find or create task via (entity_id, event_type, event.dedup_key) with dedup on non-terminal status
    enqueue AI scoring job for the task (always — scoring produces UI metadata regardless)
    for each matched trigger (subject to breaker + cooldown gates):
        if trigger.min_autonomy_suitability == 0:
            fire its prompt immediately
        // else: do nothing — the post-scoring handler re-derives and fires
```

**Post-scoring handler — re-derivation, not persisted intent** (runs when scorer writes `autonomy_suitability`):

Gated trigger fires are not persisted at event time. There's no `pending_trigger_fires` table, no in-memory queue of "what I was about to fire." Instead, the post-scoring handler **re-derives** the fire decision from durable state (task + primary event + current trigger config):

```
event = get(task.primary_event_id)
enabled_triggers = list_enabled_triggers WHERE event_type = event.event_type

for trigger in enabled_triggers:
    if trigger.min_autonomy_suitability == 0:
        continue  // fired at event time, not this handler's concern
    if !trigger.predicate.matches(event.metadata):
        continue
    if run_exists(task_id, trigger.id):
        continue  // idempotency rail — already fired somehow
    if scorer_succeeded AND task.autonomy_suitability >= trigger.min_autonomy_suitability:
        fire run  // subject to breaker + cooldown as always
    else if scorer_failed:
        fire anyway — scorer failure is a separate problem, let the breaker handle downstream failure
    else:
        record skip reason on task for UI ("auto-delegate skipped: suitability 0.18 < 0.3")
        task stays queued for user pickup
```

**Why re-derive beats persist:**

- **Restart-safe by construction.** If the server crashes mid-scoring, startup scans `tasks WHERE scoring_status IN ('pending', 'in_progress')` and re-enqueues scoring. When scoring lands, the handler re-derives — nothing to lose.
- **No new table.** Durable state (task, event, trigger config) is already sufficient.
- **Trigger config changes during scoring interval behave sensibly.** If the user disables a trigger mid-scoring, it won't fire — which is what they just said they wanted. If they add a new trigger mid-scoring, it _will_ fire on the now-scored task — technically retroactive but only over a seconds-long window, not the "new trigger resurfaces months-old events" problem we explicitly avoid.

**"Forgiving" semantics:** a trigger can create a task implicitly if no `task_rule` matches. No friction for the common case ("I want to auto-handle X"). The edge case — user deletes the last trigger for an event+predicate combo that had no explicit `task_rule` — is handled by a non-blocking inline banner on the prompts page after the delete: _"No rules or triggers are surfacing `github:pr:new_commits{AuthorIsSelf: true}` anymore. [Create task rule] [Dismiss]."_ Inline, non-modal, doesn't interrupt flow; still discoverable.

**Closed-entity guard:** a late event arriving after the entity is already closed (e.g., a delayed `new_commits` after a merge) never spawns a task. The entity lifecycle has already closed everything on this entity, and resurrecting it via a late event would be confusing.

**Autonomy-suitability gate:** `prompt_triggers.min_autonomy_suitability` lets users require AI scoring above a threshold before auto-fire. Default `0.0` = no gate, fire on predicate match. Users crank it up per-trigger via the edge-details UI on the prompts page when they want a safety valve on variable-complexity triggers (e.g., "auto-implement Jira tickets" with `min=0.5` to skip gnarly architecture work). Manual delegation never checks the gate — user explicitly chose.

### 2. Inline close checks

After task creation routing, the event handler may run a targeted close check specific to its event type — see "Task close paths" above for the v1 set. Each check queries narrowly and closes at most a small set of sibling tasks on the same entity. Entity-terminating events (`pr:merged`, etc.) go through the state machine separately (see `entities` below), which closes all active tasks on the entity with `close_reason=entity_closed`.

### 3. System bookkeeping

Events are always recorded in the `events` table, regardless of whether rules/triggers/inline checks matched. Durable log.

### Poller emission — diff against snapshot

Every event the poller emits comes from a diff against `entities.snapshot_json` (the previous snapshot) vs. fresh API state. Restart-safe (snapshot is durable), consistent across all event types. Examples:

- New check-run ID seen at the latest SHA with `conclusion: failure` → emit `ci_check_failed`. With `conclusion: success` → emit `ci_check_passed`. Same ID seen again on a later poll → no event.
- New `pull_request_review.id` seen → emit one of `review_changes_requested` / `review_approved` / `review_commented` / `review_dismissed` based on the review state. Same ID seen again → no event.
- Label name in fresh state but not in snapshot → emit `label_added`. Removed → `label_removed`.
- New commit SHA(s) at HEAD → emit `new_commits` with the count and head SHA.

The rule of thumb: **the poller emits the underlying real-world action exactly once when first observed.** Idempotent diff. No aggregation, no transition logic — that lives in inline close checks (see Task close paths) when needed at all.

---

## Predicate schema

All predicates are strictly typed in Go per event type. The DB stays flexible (`scope_predicate_json` is TEXT), but Go enforces the schema on read/write and serves it to the frontend via an introspection API.

### Structure

Per event type, three Go artifacts live in `internal/domain/events/`:

1. **Metadata struct** — all fields the poller captures for this event type. Used by agents, downstream logic, audit trails.
2. **Predicate struct** — a curated subset of metadata, every field optional (`*T`). Only fields a user would reasonably filter on. Hashes, URLs, free-form text — not predicate-worthy.
3. **Matches method** — evaluates predicate against metadata.

```go
// Metadata: what the poller saw. Immutable history.
type GitHubPRNewCommitsMetadata struct {
    Author       string `json:"author"`         // raw GitHub login (e.g., "aidan")
    AuthorIsSelf bool   `json:"author_is_self"` // derived at poll time: Author == session user
    IsDraft      bool   `json:"is_draft"`
    CommitCount  int    `json:"commit_count"`
    HeadSHA      string `json:"head_sha"`
    PrevHeadSHA  string `json:"prev_head_sha"`
    Repo         string `json:"repo"`
    PRNumber     int    `json:"pr_number"`
}

// Predicate: filter dimensions. Subset of metadata.
type GitHubPRNewCommitsPredicate struct {
    AuthorIsSelf *bool   `json:"author_is_self,omitempty"` // primary filter, seeded rules use this
    Author       *string `json:"author,omitempty"`         // exact-match, e.g., "dependabot[bot]"
    IsDraft      *bool   `json:"is_draft,omitempty"`
    Repo         *string `json:"repo,omitempty"`
}

func (p GitHubPRNewCommitsPredicate) Matches(m GitHubPRNewCommitsMetadata) bool { ... }
```

**Actor-identity pattern:** any event with an actor (PR author, reviewer, commenter) captures both the raw identity AND a derived `...IsSelf` bool. Reviews carry `Reviewer` + `ReviewerIsSelf`; comments carry `Commenter` + `CommenterIsSelf`; and so on. Storing both costs nothing, keeps metadata faithful to what actually happened, and lets predicates default to the ergonomic self-check while still supporting exact-match on identity.

**Labels as predicate fields:** every PR event carries the current `Labels []string` snapshot in metadata (not just label events — `new_commits`, `ci_check_failed`, `review_*`, etc. all include it). Predicates expose a `HasLabel *string` (or equivalent slice match) so triggers can scope to labeled PRs. This is what powers user-driven opt-in flows like the self-review label in Scenario 5 — the user adds a label to a PR, future PR events on that entity carry it, and predicates filter accordingly. No special "label state" event needed beyond `label_added` / `label_removed` for the moments the label first appears or disappears.

The `Labels` field on an event is the **snapshot at emission time** — the set of labels present when the event was observed, not the set of labels that caused the event. A `HasLabel: "self-review"` predicate on a `new_commits` event matches because the label was present when the commit landed, not because the label itself changed. This keeps the emission rule simple ("poller attaches whatever labels are currently on the entity") and avoids retroactive-matching confusion.

**Rule of thumb:**

> A field belongs on the predicate struct only if a user might reasonably want to filter on this. Bool flags, categorical enums, repo/team identifiers, actor handles, labels — yes. Hashes, timestamps, URLs, free-form text — no.

### Registry

```go
var EventSchemas = map[string]EventSchema{
    "github:pr:new_commits": {
        MetadataType:  reflect.TypeOf(GitHubPRNewCommitsMetadata{}),
        PredicateType: reflect.TypeOf(GitHubPRNewCommitsPredicate{}),
        Fields:        []FieldSchema{...}, // for frontend UI introspection
    },
    // ...
}

type FieldSchema struct {
    Name        string
    Type        string // "bool", "string", "int", "enum"
    EnumValues  []string
    Description string
    Required    bool // for metadata; always false for predicates
}
```

### API surface

- `GET /api/event-schemas` → full map of `event_type` → `FieldSchema` list
- `GET /api/event-schemas/{event_type}` → single type
- Used by the frontend to dynamically render a predicate editor with typed inputs (checkbox for bool fields, dropdown for enums, etc.)

### Validation

- When creating a `task_rule` or `prompt_trigger` via API, validate `scope_predicate_json` against the registered predicate type for the given `event_type`. Reject unknown fields or wrong types.
- Unmarshal into the concrete Go type; re-marshal for storage. Ensures what's stored is canonical.

### Evolution

Adding a new event type = add to `events/metadata.go`, `events/predicates.go`, register in `registry.go`. Downstream predicate editors update automatically via the schema endpoint.

---

## State machines

### entities

```
active  ──(merged|closed event)──▶  closed
```

### tasks

```
queued  ──(claim)──▶    claimed    ──(user mark done)──▶  done
queued  ──(delegate)──▶ delegated  ──(run terminal success)──▶  done
                                   ──(run failed + breaker tripped)──▶ queued (retained, breaker gates further auto-fire)
queued  ──(dismiss)──▶  dismissed
queued  ──(snooze)──▶   snoozed  ──(until reached)──▶    queued
                               ──(event bumps task)──▶    queued (wake-on-bump)
queued  ──(cascade match)──▶    done  (close_reason=auto_closed_by_event, close_event_type=X)
```

Note: `pending_pr_review_approval` and `yielded_to_user` are **run states**, not task states. While a run is in either, the task stays in `delegated`. The "needs your attention" UI view is derived from the latest run's status, not a dedicated task status.

### runs

```
cloning → fetching → worktree_created → agent_starting → running
running → yielded_to_user ──(user responds)──▶ running
running → pending_pr_review_approval ──(user submits to GitHub)──▶ completed
                                     ──(user discards)──▶ cancelled
running → completed | failed | task_unsolvable | cancelled
```

Terminal run states: `completed`, `failed`, `task_unsolvable`, `cancelled`. All others are intermediate.

User-input-required states (surfaced in the "needs attention" UI view):

- `yielded_to_user` — agent paused to ask a question (SKY-139)
- `pending_pr_review_approval` — agent produced a draft PR review, user needs to approve/submit

Future states follow the same naming: explicit about _what_ is pending (e.g., `pending_commit_push_approval`).

---

## Breaker (per-prompt-per-entity)

Not a column — a query. When a run finishes:

```sql
-- count consecutive non-successes at the tail of runs for (entity_id, prompt_id)
-- stopping at the first 'completed' row
WITH recent AS (
  SELECT r.status, r.started_at
  FROM runs r
  JOIN tasks t ON r.task_id = t.id
  WHERE t.entity_id = ?
    AND r.prompt_id = ?
    AND r.trigger_type = 'event'
  ORDER BY r.started_at DESC
  LIMIT 20
)
SELECT COUNT(*)
FROM recent
WHERE status IN ('failed', 'task_unsolvable')
  AND started_at > (
    SELECT COALESCE(MAX(started_at), '1970-01-01')
    FROM recent WHERE status = 'completed'
  );
```

Emits `system:prompt:auto_suspended` event with `{entity_id, prompt_id, trigger_id, consecutive_failures}` when the count transitions to `trigger.breaker_threshold` (default **4**; user-configurable per-trigger).

**Original framing note:** the breaker was initially conceived as `(event, entity, prompt)` — "repeated runs of the same agent on the same exact data." In practice this almost never trips because bump events produce new rows and the count resets every poll cycle. `(entity, prompt_id)` captures the real signal: this prompt can't fix this PR, regardless of which specific event rebumped the task.

Manual runs (`trigger_type = 'manual'`) never count toward the breaker. A successful `completed` run anchors the reset.

---

## Memory materialization

When a run starts, the spawner walks the entity graph and collects all memory:

```sql
WITH related AS (
  SELECT id FROM entities WHERE id = ?
  UNION
  SELECT to_entity_id FROM entity_links WHERE from_entity_id = ?
  UNION
  SELECT from_entity_id FROM entity_links WHERE to_entity_id = ?
)
SELECT rm.run_id, rm.content, rm.created_at, e.source, e.source_id
FROM run_memory rm
JOIN entities e ON rm.entity_id = e.id
WHERE rm.entity_id IN (SELECT id FROM related)
ORDER BY rm.created_at ASC;
```

Writes to `<cwd>/task_memory/<run_id>.md` (keeping the existing filename convention).

One-hop traversal in v1. Recursive CTE with depth cap if needed later.

---

## Open / resolved questions

**Q: When does an event become visible to the user?**
**A:** Only when a matching task_rule or prompt_trigger creates a task. No universal "every event is a task" mode. Event logs are queryable for debugging but don't clutter the triage queue by default.

**Q: Does disabling a task_rule affect existing tasks?**
**A:** No. Existing tasks stay. The rule only affects future events.

**Q: Can a user keep a task surfaced without automation?**
**A:** Yes — create a task_rule with the same predicate as the automation, leave it enabled when the trigger is disabled or deleted.

**Q: What if the user deletes the last prompt_trigger for an event_type+predicate combo?**
**A:** If no task_rule covers that combo, a non-blocking inline banner appears on the prompts page offering to materialize a task_rule from the deleted trigger's predicate. One click and an explicit rule is created. Non-modal so users don't reflexively dismiss.

**Q: Auto-close behavior user-editable?**
**A:** Not in v1. There's no cascade map; instead, a small set of inline close checks live inside specific event handlers (see "Task close paths"). The set is definitional, not a user preference. If users want to suppress an auto-close, they can disable the relevant `task_rule` so the task never gets created in the first place.

**Q: Event catalog `enabled` toggle?**
**A:** Gone. Absence of rules/triggers is the disabled state. Poller-level config (source not connected) handles "don't emit this source's events at all."

**Q: View-time filters in the UI?**
**A:** Orthogonal to everything above. The filter panel on the triage queue/board is a display filter — hides tasks without affecting creation. Users can filter by event_type, predicate, etc., but those are presentation preferences, not data-model concerns.

**Q: Retroactive task creation when adding a new rule/trigger?**
**A:** Never. Events are history; task creation happens at event arrival time only.

**Q: Event idempotency?**
**A:** None at v1. Duplicate events from poller retries produce duplicate rows. Bumping a task with identical metadata is a no-op, so downstream routing is idempotent even if event emission isn't.

**Q: Does auto-delegation wait for AI scoring?**
**A:** Only when the trigger opts in via `min_autonomy_suitability > 0`. Default `0.0` = fire immediately, no gate. Scoring always runs (for UI metadata) but doesn't block auto-fire unless the trigger requires it. If scorer fails on a gated trigger, we fire anyway and let the breaker handle any downstream failure.

**Q: Task gets bumped (new event) while a run is already in flight, or while scoring is pending?**
**A:** Known ugly edge case. No clean general solution — the run is already operating on a snapshot of entity state, and the bump signals that state has changed. We don't cancel or re-fire mid-run. The run completes against its original context; any divergence becomes the next cycle's problem (new event arrives after run completes → new task or bump). Park unless real user pain emerges.

---

## Scenario traces

Walk-throughs of the key flows. If any requires contortion, the model is wrong.

### Scenario 1 — New PR opens

1. Poller sees PR #18 for the first time.
2. INSERT `entities` (`github`, `owner/repo#18`, `pr`, state=`active`, snapshot). No events yet (discovery, not a diff).
3. Next poll: snapshot updates (`last_polled_at`). Still no events.

**Tables touched:** `entities`. No events, no tasks, no runs.

---

### Scenario 2 — Happy-path CI failure

1. Poller diff detects three new check-run completions on PR #18, all `failure` (e.g., `build`, `test`, `lint`).
2. Emits three `github:pr:ci_check_failed` events, one per check-run, with metadata `{name, check_run_id, head_sha, repo, author_is_self: true, ...}`.
3. INSERT three `events` rows (entity_id=PR18, event_type, metadata).
4. **Routing step 1 — task creation:** seeded `task_rule` "CI failed on my PR" with predicate `{AuthorIsSelf: true}` matches all three. User's `prompt_trigger` "CI Fix" with the same predicate matches all three.
5. First event creates task T1 via dedup on `(entity=PR18, event_type=ci_check_failed)`. The next two events bump T1 (insert `task_events` rows with `kind=bumped`) — they don't create new tasks.
6. **Routing step 2 — inline close check:** `ci_check_failed` has no close check (failures don't resolve other tasks). Skipped.
7. Trigger auto-delegate subscriber fires once on T1 (cooldown gates subsequent bumps within the cooldown window). INSERT `runs` (task_id, prompt_id=ci-fix, trigger_type=event, status=cloning).
8. Agent runs, fixes the underlying issue, commits, pushes. Writes `task_memory/<run_id>.md` before completion JSON.
9. Spawner ingests memory: INSERT `run_memory` (run_id, entity_id=PR18, content).
10. Spawner parses completion JSON: UPDATE `runs` status=completed, INSERT `run_artifacts` rows.
11. **Run-completion handler:** run terminated successfully. UPDATE `tasks` T1 status=done, close_reason=`run_completed`.
12. Next poll at the new SHA: three check-runs complete with `success`. Poller emits three `github:pr:ci_check_passed` events.
13. **Routing step 1 — task creation:** no rule or trigger matches `ci_check_passed` → no task created.
14. **Routing step 2 — inline close check:** the `ci_check_passed` handler queries "any failing check-runs remain on PR #18 at the latest SHA?" → no. Finds active `ci_check_failed` tasks on PR18. T1 is already terminal → close check is a no-op for this scenario.

**Tables touched:** all the central ones. No contortions. ✓

**Two separate close paths:**

- **Run-completion close** (step 11) — the task the run was on goes to done when the run succeeds terminally. Close reason: `run_completed`.
- **Inline close check** (step 14) — closes the failure task when the situation resolves externally. Only matters if no run succeeded first (user fixed manually, flaky test passed, coworker pushed). Close reason: `auto_closed_by_event` with `close_event_type=github:pr:ci_check_passed` populated.

---

### Scenario 3 — CI fails twice before task runs (bump case)

1. CI fails on SHA-A → task T1 created (as in Scenario 2 steps 1-6). Auto-delegate fires **immediately** on task creation (cooldown does not gate the first fire). Run R1 starts.
2. 30s later — while R1 is still running — user pushes SHA-B. The same checks re-run at the new SHA and fail. Poller emits a fresh batch of `ci_check_failed` events (new check-run IDs at the new SHA).
3. Routing: `(entity=PR18, event_type=ci_check_failed, dedup_key='')` has active T1 (any non-terminal status — queued, claimed, delegated, or snoozed). Dedup index prevents new task creation.
4. UPDATE `tasks` T1: bump metadata (latest head_sha, failing-check names) from the new events' snapshots.
5. INSERT `task_events` (T1, each new event, kind=bumped).
6. Auto-delegate subscriber evaluates the bump: trigger cooldown is 60s since T1 was created → skip. (If the bump arrives after the cooldown window, a second run R2 fires on the latest bumped state.)

**Key point:** cooldown gates *subsequent* fires of the same trigger, not the initial fire on a fresh task. Auto-delegate always fires immediately on task creation if the trigger's gates (predicate, breaker, autonomy) pass — cooldown only kicks in for re-fires triggered by bumps.

**Tables touched:** `tasks` (UPDATE), `task_events` (new `bumped` rows). ✓

---

### Scenario 4 — Breaker trips per-(entity, prompt)

1. Scenario 2 plays out, but the run fails.
2. UPDATE `runs` status=failed.
3. Breaker query: `(entity=PR18, prompt=ci-fix)` has 1 consecutive failure. Below threshold (4).
4. CI fails again. Event → bump T1.
5. Auto-delegate fires. New run. Also fails. Breaker count = 2.
6. Third attempt. Fails. Breaker count = 3.
7. Fourth attempt. Fails. Breaker count = 4 → trips.
8. Emit `system:prompt:auto_suspended` event with `{entity=PR18, prompt=ci-fix}`.
9. Auto-delegate subscriber, on next `ci_check_failed`, sees the count is already at threshold → skips fire, logs.
10. User clicks "retry" (manual). Manual runs don't count. Run succeeds → `completed` row becomes the new anchor → breaker resets for future events.

**Tables touched:** `runs`, `events` (auto_suspended). Breaker is a query, no column. ✓

---

### Scenario 5 — Self-review loop end-to-end

**Phase A: Implement Jira ticket**

**Prerequisite:** user has wired the seeded "Implement Jira ticket" prompt to `jira:issue:assigned` via a `prompt_trigger` of their own. We seed the prompt (useful default), but not the trigger — auto-implementing assigned Jiras is a big commitment and users should opt in explicitly.

1. Poller sees Jira SKY-123 assigned to user. Event `jira:issue:assigned` with metadata `{assignee_is_self: true, priority: P2, ...}`.
2. INSERT `entities` (jira, SKY-123, kind=issue) if not seen.
3. Routing: seeded `task_rule` "Jira assigned to self" matches (predicate `{assignee_is_self: true}`). The user's `prompt_trigger` "Implement Jira ticket" matches. Task created, trigger fires.
4. INSERT `tasks` (event_type=`jira:issue:assigned`, status=queued) + `runs` (prompt=`system-implement-jira-ticket`).
5. Agent creates branch `feature/SKY-123`, commits, pushes, opens PR #42.
6. Agent writes memory: "Implemented feature X, tests pass, opened PR #42."
7. Agent emits `run_artifacts`: `github:pr` (PR #42, primary), `github:branch` (feature/SKY-123). Run completes; task T1 marked done.

**Phase B: Poller picks up the new PR**

1. Next GitHub poll discovers PR #42. INSERT `entities` (github, owner/repo#42, pr).
2. Branch-name parser matches `feature/([A-Z]+-\d+)` → extracts `SKY-123` → INSERT `entity_links` (PR#42, Jira SKY-123, kind=implements, origin=branch_name).

**Phase C: User opts in to a self-review pass**

1. User adds the `self-review` label to PR #42 on GitHub.
2. Next poll: poller diffs labels, emits `github:pr:label_added` with metadata `{label_name: "self-review", author_is_self: true, is_draft: true, repo: "...", pr_number: 42}`.
3. Routing: user has a `prompt_trigger` "Self-review my draft PRs" with predicate `{label_name: "self-review", author_is_self: true}` → predicate matches → task created implicitly via forgiving path, trigger fires.
4. INSERT `tasks` (entity=PR42, event_type=label_added) + `runs` (prompt=`system-self-review-draft-pr`).
5. Agent reads memory for PR42 + linked Jira SKY-123 (one-hop entity graph) → sees "what the implementation run did."
6. Agent reviews the current diff, posts a `commented`-type review on GitHub with inline comments.
7. Agent emits `run_artifacts`: `github:review` (primary). Run completes; task done.

**Phase D: Address self-review comments (iterative)**

1. Poller detects the review posted by self on own PR → emits `github:pr:review_commented` event with metadata `{reviewer_is_self: true, author_is_self: true, comment_count: 5, labels: ["self-review"], ...}`.
2. Routing: user's `prompt_trigger` "Address self-review comments" with predicate `{reviewer_is_self: true, author_is_self: true, has_label: "self-review"}` matches. Task created, trigger fires.
3. New task on PR42 + run. Agent reads run_memory for PR42 AND Jira SKY-123 (via entity_links).
4. Agent addresses the comments, posts inline replies acknowledging them, commits fixes, pushes to `feature/SKY-123` (SKY-138 allows push to PR branch, not worktree). Writes memory. Task done.
5. Loop convergence: the agent's address-comments run doesn't post a fresh review (it just replies inline + pushes), so no new `review_commented` event fires. The cycle terminates naturally. If subsequent commits prompt a fresh self-review pass, the user removes and re-adds the label.

**Phase E: User opens the PR for external review**

1. User removes the `self-review` label and clicks "Mark ready for review" on GitHub (or via a passthrough button in our UI — no special routing significance).
2. Poller diff: emits `github:pr:label_removed{label_name: "self-review", ...}` and `github:pr:ready_for_review` (the standard transition out of draft, regardless of who initiated). Both routed normally. Neither triggers anything by default.

**Phase F: External reviewer requests changes**

1. External reviewer posts a `changes_requested` review. Poller emits `github:pr:review_changes_requested` with metadata `{reviewer: "alice", reviewer_is_self: false, author_is_self: true, repo: "...", pr_number: 42, ...}`.
2. Routing: seeded `task_rule` "Changes requested on my PR" with predicate `{AuthorIsSelf: true}` matches → task created. User's `prompt_trigger` "Respond to external review" with predicate `{reviewer_is_self: false, author_is_self: true}` matches → trigger fires. (The `self-review` label is gone now, and the trigger doesn't care — external review handling is independent of self-review history.)
3. INSERT `tasks` (entity=PR42, event_type=review_changes_requested) + `runs` (prompt=`system-respond-external-review`).
4. Agent reads run_memory for PR42 AND SKY-123 — full context from implementation through self-review, now responding to external feedback with complete history.
5. Agent addresses Alice's comments, pushes fixes. Run completes; task done.
6. Later: Alice re-reviews and approves. Poller emits `github:pr:review_approved{reviewer: "alice", ...}`. Routing step 2 (inline close check): the `review_approved` handler checks Alice's prior state — was `changes_requested` — and that no other reviewer is currently `changes_requested`. Closes the active `review_changes_requested` task on PR42 with `close_reason=auto_closed_by_event`.

**Tables touched across all phases:** entities, entity_links, events, tasks, task_events, runs, run_artifacts, run_memory. Event types are split on situation discriminators (`review_changes_requested` / `review_approved` / `review_commented`); reviewer identity stays as predicate-filterable metadata. The `self-review` label is the user's explicit opt-in to the self-review cycle — no provenance flags or special "manual ready" event types needed. ✓

---

### Scenario 6 — Dismiss, same event fires later

1. CI fails → T1 created → auto-fires → fails → breaker at 1.
2. User dismisses T1 (UPDATE status=dismissed).
3. CI fails again later. Routing: `(entity=PR18, event_type=ci_check_failed)` has no **active** task (T1 is dismissed — terminal). Task_rule still matches, trigger still matches → creates T2.
4. Auto-delegate checks breaker for `(entity=PR18, prompt=ci-fix)`. Count = 1 (T1's failed run still counts). Fires.
5. If T2 also fails, breaker = 2. Dismissing doesn't reset the budget — the breaker tracks agent capability, not user sentiment.

**Decision:** dismissing a task does **not** reset the breaker. User-led reset requires a manual successful run.

---

### Scenario 7 — PR merged (entity lifecycle close)

1. Poller: PR #42 merged. Emits `github:pr:merged`. INSERT `events`.
2. Routing step 1 (task creation): no rule or trigger matches → no task created.
3. Routing step 2 (inline close check): `pr:merged` has no inline close check — entity-terminating events go through the lifecycle handler instead.
4. **Entity lifecycle handler:** `pr:merged` triggers `entities.status: active → closed`. UPDATE `entities` PR42 status=closed, closed_at.
5. Lifecycle transition closes all active tasks on the entity: for each active task on PR42, UPDATE status=done, close_reason=`entity_closed`. INSERT `task_events` (each task, event, kind=closed).
6. Does **not** propagate to linked Jira SKY-123 — cross-entity propagation is off by default. Jira `completed` event will independently transition the Jira entity.

**Tables touched:** `events`, `entities` (status=closed), `tasks` (UPDATE for each closed), `task_events`. ✓

**Why lifecycle handles this, not an inline check:** "merge closes everything on the PR" isn't opinion — it's what "merged" _means_. Modeling it as a per-event-type close check would require enumerating every event_type the PR might have. Entity lifecycle handles it uniformly: closed entity → all its active tasks close, regardless of event_type.

---

### Scenario 8 — Jira parent with open subtasks (pre-delegate gate)

1. User swipes delegate on Jira SKY-100 (parent task).
2. Spawner's pre-delegate gate fetches current subtasks (SKY-101 done, SKY-102 in-progress assigned to user, SKY-103 new).
3. Gate checks configured `done_statuses`. SKY-102 and SKY-103 aren't done. Gate blocks.
4. Spawner returns error to frontend: "This ticket has 2 open subtasks. Complete or delegate those first."
5. No run created. For auto-delegation: emits `system:task:delegation_blocked_by_subtasks` event instead of erroring.

**Tables touched:** none — gate runs before any writes. ✓

---

### Scenario 9 — User deletes last trigger for an event+predicate combo

1. User created a `prompt_trigger` "Self-review on draft" with predicate `{author_is_self: true, is_draft: true}`. No task_rule exists for `new_commits`.
2. User decides they don't want auto self-review anymore. Goes to Prompts page, deletes the trigger.
3. Frontend detects: no `task_rule` matches this predicate, and this was the last trigger for `new_commits` with this predicate. Shows a non-blocking inline banner on the prompts page: _"No rules or triggers are surfacing `new_commits` on your own draft PRs anymore. [Create task rule] [Dismiss]."_
4. User clicks "Create task rule." API creates a `task_rule` with the same predicate. Banner disappears.
5. Going forward: `new_commits` events matching the predicate still create tasks, just with no auto-fire. User manually delegates or handles.

**Tables touched:** `prompt_triggers` (DELETE), `task_rules` (INSERT). ✓
