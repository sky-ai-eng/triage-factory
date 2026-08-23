# Multi-team task model — Agent briefing

Read this before touching **task creation/routing** (`internal/routing/router.go`), the **queue/factory reads**, or **GitHub/Jira event → task** logic under multi-mode. It captures a model worked out across a long design pass. Some pieces are **decided**, some are explicitly **open/deferred** — both are marked. The motivating context: an org can have many teams sharing the same repos and Jira projects (think 800 people in 2 monorepos, or 6 teams on one Jira project), so "which team does this work belong to?" is the central question.

This supersedes the per-team-fan-out assumption baked into **SKY-295**. Where they conflict, this doc wins.

---

## The one principle everything hangs off

> **One task per `(entity, event_type, dedup_key)`. Interested parties are _visibility_, never _count_. Claim is exclusive. Team is ownership/visibility — never part of task identity.**

Three orthogonal axes — keep them separate in your head, they're the whole model:

| Axis | Carried by | Determines |
|---|---|---|
| **Deliverable** | `dedup_key` | the **count** of tasks |
| **Visibility** | the teams/people an event is relevant to | who can **see / claim** it |
| **Claim** | `claimed_by_*` (XOR) | who's **doing** it (consolidates *within* a deliverable) |

If you ever find yourself multiplying tasks by team, or making the bot a special kind of reviewer, stop — you're conflating axes.

---

## What `dedup_key` actually is (grounded — do not reinvent)

`dedup_key` is **set by the event emitter** (`internal/tracker/diff.go`), and today it is `""` for almost everything. It carries a value only for genuine open-set discriminators:

- `github:pr:label_added` / `label_removed` → the **label name**
- `jira:issue:status_changed` → the **status**
- `jira:issue:priority_changed` → the **priority**

Rules never set it. **Keep it emitter-set.** Do not add a "rule stamps its own dedup_key" capability — that path was explored and rejected as needless machinery. The count of tasks for one event = the number of distinct `dedup_key` values the emitter produces for it.

---

## Decided change #1: team comes OUT of task identity (the SKY-295 reversal)

Today: identity = `(entity_id, event_type, dedup_key, team_id)` → one event matching N teams' rules fans out to **N task rows**.

**Change:** drop `team_id` from the key. Identity = `(entity_id, event_type, dedup_key)`. Team becomes:

- a **visibility set** — the teams whose rules matched (store as a thin `task_teams(task_id, team_id)` association),
- collapsing to the **single owner team on claim** (drained to the claimer's team).

So: **unclaimed task → visible to all matched/tracking teams; claimed task → owner's team only.** Claim is a compare-and-swap on `claimed_by_*` (the XOR columns already exist) — first claimer (human **or** bot) wins; everyone else no-ops. The bot is just another claimer; there is **nothing to race over** because there's one row, not N.

**Why:** org-level repos and shared Jira projects mean many teams legitimately see one work item. Per-team fan-out duplicates the card on every team's board and makes multiple teams' auto-claim triggers race. "Many see it, one claims it" is the actual shared-backlog reality.

---

## Jira specifics

- `jira:issue:assigned` → one task (the assignee's).
- `jira:issue:available` (unassigned, pickup-status) → **one shared task**, visible to tracking teams, claim consolidates. This is the canonical shared-backlog case.
- Team scope comes from **`jira_project_status_rules`** (PK `(team_id, project_key)`) — already exists, already used by the Jira discovery deck (**SKY-367**). It's a team declaring "I track these projects."

---

## GitHub specifics

**Per-reviewer events.** `review_requested` must emit **one event per requested reviewer/team**, with `dedup_key = reviewer login` (individual) or `github-team slug` (team). This is the *same open-set-discriminator pattern as `label_added`* — the open set is "the requested reviewers." Not a review special-case.

- individual request → a per-person task (the reviewer's).
- github-team request → **one task for the mapped TF team**.

**The bot is NOT a flat reviewer and NOT in CODEOWNERS.** It auto-reviews **on behalf of each requested codeowner team that opted in**, triggered by that team's review request. So "which team is the bot acting for?" is always answered by **which github team's request triggered it** — never ambiguous. A PR touching 10 owned areas → GitHub requests 10 codeowner teams → up to 10 per-team tasks → the opted-in teams' bots review their slice, each unambiguously its own team. There is no "11th reviewer that doesn't know its team," because the bot isn't a reviewer — it's the automation behind the teams' own requests.

**`github-team ↔ TF-team` mapping** (admin-set): the keystone for GitHub, and it is **dumb string labels** — `TF team A ↔ @org/backend`. **No membership resolution, no nesting traversal** (that's the swamp; routing doesn't need it). It's **monorepo-native**: GitHub already requests the right codeowner teams per path from the repo's CODEOWNERS, so **TF never parses paths or CODEOWNERS itself** — it consumes GitHub's per-team review requests and maps the team name. This mapping is the **GitHub mirror of `jira_project_status_rules`**, and it closes three gaps at once: rule over-fan, task team-home, and bot review routing.

**Direct `@bot` request** (someone requests the bot itself, no team in the signal) → **org / org+per-repo default bot config** (the base fallback tier). Specific (team mapping) → general (org default), the same specific→general shape as the SKY-352 credential resolver.

**Bot identity** is the **SKY-352 resolver's** job, not new logic: App installation token → distinct bot identity; PAT → bot-is-you. The install tier decides.

### Two routing axes — reviewer vs entity-owner (they never cross-contaminate)

GitHub PR events split into two independent axes:
- **`review_requested` → the requested reviewer's team** (above) — a reviewer's personal obligation.
- **Everything else** (`ci_check_failed`, `pr:conflicts`, `review_changes_requested`, `review_commented`, `new_commits`) → the **entity's owning team** — the PR's author-centric lifecycle.

**Entity owning-team derivation** (first hit wins, resolved at task creation): `entities.owning_team_id` override → the team of the most recent prior **owned author-centric** task on the entity (review tasks and NULL-owned priors excluded) → the PR author's teams (one → owner; **multiple → no single owner: `team_id = NULL`, visible to all of them**; none/non-TF author → **no task** unless an explicit user-authored rule opts a team into the visibility set). `team_id = NULL` is the honest "unresolved owner" residual — it gates auto-delegation off (the bot never claims an unowned task) and resolves on the first human claim. The read-time **TF-origin** tier (the team of the run that opened the PR) was dropped in favor of a **write-time** `owning_team_id` stamp at PR-open time (SKY-427), which tier 1 then reads.

**The review-first trap (load-bearing):** the owning-team derivation **must exclude `review_requested` tasks.** If a review is the *first* event on a fresh entity, it creates a task on the *reviewer's* team — counting that would poison the derivation, so a later CI failure would route to the reviewer's team. Review activity is feedback *on* a PR by outsiders; it never establishes entity ownership. So review tasks feed **only** the reviewer axis. The payoff: a PR can carry a review task on the reviewer's team **and** a CI-failure task on the entity's owning team at the same time, with no conflict — different deliverables, different teams.

---

## Transferring ownership between teams

Teams hand work off with a directed **FROM → TO** transfer (both user-selected; FROM defaults to the current owner), **atomic**: set `entities.owning_team_id = TO` **and** re-home the entity's active *owning-team* tasks that are FROM's (`tasks.team_id` + `task_teams` → TO) in one transaction. **Reviewer tasks are not moved** — they're reviewer-personal. After transfer the owning-team derivation returns TO (tier 1 override), so future author-centric events route to TO durably (even after the moved task closes).

**Cheap by construction:** because entities are org-shared (not forked per team), transfer moves **no content** — events, prior runs, and memory are org-level, so TO inherits the full context for free; transfer just re-points attribution. (The forked-per-team model we rejected would have required copying entity + event log + memory — exactly the mess we avoided.)

---

## Augment vs consume — there is NO "augment mode"

Claim is exclusive for **everyone, bot included**. Do **not** add a review-specific "bot augments instead of consuming" carve-out (this was tried and rejected — it's special-casing and it's also just false, since claim consumes).

To get "**bot reviews AND humans review**," they must be **separate requests → separate tasks** (per-reviewer). A team's auto-review trigger must **predicate on the reviewer/team it handles** (e.g. `reviewer = bot`, or the specific team) — **never match-all** — so it claims only its own task and can't steal a human team's task. "**Want human eyes, no bot**" = that team simply doesn't enable auto-review. The intent lives entirely in *who is requested* + *what the trigger predicates on*, both uniform mechanisms.

---

## Local mode

"Local = multi at N=1." The real change: the **poller/backfill must emit per-reviewer events** (read the PR's requested-reviewer set; emit one per TF-known identity), replacing today's user-perspective "is review requested from me?" In local N=1 this degenerates to ~today's behavior through the general mechanism.

One honest capability tier: **local+PAT → bot is you** (one GitHub identity), so you cannot have "bot pass + your pass" as two distinct GitHub reviews — two TF tasks, one GitHub actor. **local+App (and multi) → distinct bot identity.** Again, the resolver tier decides; no per-install branching for *who the bot is*.

---

## Accepted divergences (do NOT special-case these away)

1. **TF/GitHub drift on consumed tasks.** A claimed/consumed task drops off TF's radar even if GitHub still wants something, until a fresh event (re-request) recreates it. Uniform across **all** event types — not review-specific.
2. **Membership dismissal.** A user reviewing a PR also dismisses their github-team's request (they're a member), so TF's sibling team task can be briefly moot. A `review_request_removed` event closes it; until then TF and GitHub can disagree.

These are inherent to event→task→claim. The general fix, if ever needed, is re-emitting on GitHub's re-request — not a per-event-type patch.

---

## Decided vs open

**Decided:**
- One task per `(entity, event_type, dedup_key)`; team = visibility/ownership; claim exclusive (SKY-295 reversal).
- `task_teams` visibility set, collapsing to owner on claim.
- Per-reviewer GitHub review events (`dedup_key` = reviewer/team).
- `github-team ↔ TF-team` mapping (string labels) as the twin of `jira_project_status_rules`.
- Bot reviews per requested codeowner team; org/repo default for direct `@bot`.
- Bot identity via the SKY-352 resolver.
- **Two routing axes:** `review_requested` → reviewer's team; all other GitHub PR events → entity's owning team (derivation: override → prior *owned author-centric* task → author-teams; an ambiguous multi-team author ⇒ `team_id = NULL` visible to all, a non-TF author ⇒ no task unless an explicit user rule opts a team in), with **review tasks excluded** from the owning-team derivation. Owner-only automation: only the resolved owner's triggers fire; a NULL owner fires nothing.
- **Acting team on claim** is derived (claimer's teams ∩ `task_teams`; bot = firing trigger's team) — not blocked on the team-picker.
- **Transfer** = directed FROM→TO, atomic (entity `owning_team_id` + FROM-owned tasks), reviewer tasks stay; cheap because entities are org-shared.

**Open / deferred:**
- "Bot pass + human pass both *required* on GitHub" (bot as a separate explicit reviewer with its own identity) — niche; deferred.
- Hard team-RLS vs soft (query-level) team filter — currently **hard RLS** (262/366/367 + SKY-368); soft was explored and judged ~equivalent-with-less-boundary, so kept hard for the defense-in-depth backstop. Revisit only if the state-dependent claim-RLS proves painful.
- Distribution policy when several teams auto-claim the same shared pool (CAS keeps it *safe*; "who *should* get it" is a separate knob).
- Whole "org == team" collapse — rejected for now (loses poll consolidation + inter-team sharing + claim-dedup); may revisit.

---

## How the tickets relate (cluster under SKY-242)

- **SKY-368** — the reversal: one task + `task_teams` + claim-CAS; the foundation. Supersedes **SKY-295**'s fan-out (kept: `became_atomic` suppression, bus-routed backfill).
- **SKY-369** — `github-team ↔ TF-team` mapping (the `jira_project_status_rules` twin).
- **SKY-370** — per-reviewer review events + route to the requested identity's team.
- **SKY-371** — org/repo default bot-review config (direct `@bot` fallback). *Low.*
- **SKY-372** — author-centric routing → entity owning team; adds `entities.owning_team_id`; encodes the review-first exclusion.
- **SKY-373** — transfer (directed FROM→TO).
- **SKY-294** — team-selection UX; *read-scoping now delegated to 366/367/368*, so it narrows to the picker + write-threading. (Its old `entities.project_id → projects.team_id` derivation is invalid — projects are optional.)
- **SKY-366** (merged) — factory entity↔team membership; consistent, reused by 372's owning-team lookup.
- **SKY-367** (merged) — Jira deck GET+POST team-scoping; consistent.
- **Order:** 368 + 369 (foundation, parallel) → 271 + 353 (prereqs) → 370 → 372 → 294 → 371 → 373.
