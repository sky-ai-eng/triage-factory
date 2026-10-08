# Tracked Events

Triage Factory monitors GitHub PRs, Jira issues and Linear issues for state changes and emits typed events when transitions are detected. Events power the triage queue, AI scoring, delegation triggers, and the dashboard.

## How it works

The tracker runs on a configurable poll interval (default: 5 minutes). Each cycle:

1. **Discover** — search queries find new items to track
2. **Register** — new items are stored in `tracked_items` with an initial snapshot
3. **Refresh** — all tracked items are batch-fetched (GitHub via GraphQL `nodes(ids:[...])`, Jira via `key IN (...)` JQL, Linear via GraphQL `issues` filtered by id, 50 at a time)
4. **Diff** — current snapshot is compared against the previous snapshot
5. **Emit** — typed events are recorded in the `events` table and published to the event bus

Events are emitted once per transition, not continuously. If a PR stays in the same state across multiple cycles, no events fire.

## GitHub PR Events

### Actionable (shown in triage queue by default)

| Event | ID | Trigger |
|-------|----|---------|
| **Changes Requested** | `github:pr:changes_requested` | A reviewer's latest review state changes to `CHANGES_REQUESTED` |
| **CI Failed** | `github:pr:ci_failed` | The head commit's `statusCheckRollup` transitions to `FAILURE` or `ERROR` |
| **Review Requested** | `github:pr:review_requested` | A user/team appears in the PR's `reviewRequests` that wasn't there before. Matches the session user directly or via any team they belong to (fetched from `GET /user/teams`, stored as `org/slug`). Detects both initial requests and re-requests after changes |
| **Merge Conflicts** | `github:pr:conflicts` | The PR's `mergeable` state transitions to `CONFLICTING` |
| **Ready for Review** | `github:pr:ready_for_review` | The PR's `isDraft` changes from `true` to `false` |
| **PR Approved** | `github:pr:approved` | A reviewer's latest review state changes to `APPROVED` |
| **Mentioned** | `github:pr:mentioned` | PR discovered via `mentions:{user}` search. Note: new @mentions on an already-tracked PR cannot be detected without parsing comment bodies |

### Informational (hidden by default, toggleable)

| Event | ID | Trigger |
|-------|----|---------|
| **CI Passed** | `github:pr:ci_passed` | The head commit's `statusCheckRollup` transitions to `SUCCESS` |
| **Authored PR** | `github:pr:opened` | First time an authored PR is discovered |
| **PR Merged** | `github:pr:merged` | The PR's `merged` field changes to `true` |
| **PR Body Updated** | `github:pr:body_updated` | The complete PR body changes, including clearing it |

## Jira Events

### Actionable

| Event | ID | Trigger |
|-------|----|---------|
| **Issue Assigned** | `jira:issue:assigned` | The `assignee` field changes to a non-empty value |
| **Issue Available** | `jira:issue:available` | An unassigned issue appears in the pickup queue, or an assigned issue becomes unassigned |
| **Priority Changed** | `jira:issue:priority_changed` | The `priority` field changes |
| **New Comment** | `jira:issue:commented` | The `comment.total` count increases (fires once per cycle regardless of how many comments were added) |

### Informational

| Event | ID | Trigger |
|-------|----|---------|
| **Status Changed** | `jira:issue:status_changed` | The `status` field changes (e.g. To Do → In Progress) |
| **Issue Completed** | `jira:issue:completed` | The `status` changes to Done, Closed, or Resolved |
| **Issue Unreachable** | `jira:issue:unreachable` | Jira will no longer resolve a tracked issue's key — see below |
| **Issue Body Updated** | `jira:issue:body_updated` | The complete issue description changes, including clearing it |

#### Body updates

Both body-update events compare a SHA-256 fingerprint of the complete source
body, before the 2,000-codepoint `entities.description` preview is truncated.
Jira's batch refresh requests `description`, so edits are detected even when an
issue no longer matches discovery's assignment/pickup queries. Jira ADF is
fingerprinted as canonical JSON: link and formatting edits count, while object
key order and JSON whitespace do not. GitHub's REST and GraphQL reads produce
the same fingerprint for the same markdown. Explicit `null` and an empty string
both mean cleared; an omitted field preserves the last observed revision and
preview.

First observations, snapshot-less seeds, and existing snapshots without a body
fingerprint establish a baseline without emitting a body-update event. Later
transitions commit with the snapshot through the existing atomic CAS/outbox
path, so retries do not duplicate events. As with other snapshot diffs, multiple
edits between polls collapse to the final observed change.

Metadata includes `previous_body_hash`, `body_hash`, and source identity/context,
without either full body. The source's issue/PR `updated` timestamp is the
available time approximation, not a dedicated body-edit timestamp. Author and
assignee identify ownership, not who edited the body. No default task rule or
delegation trigger is added; handlers can opt in through the event catalog.

The preview remains a best-effort display/scoring mirror and can lag the event.
A consumer needing full, revision-consistent content must obtain that body and
check its fingerprint rather than treat the preview as a complete document.

#### Issue Unreachable

The one Jira event that doesn't come from the snapshot-diff, because there is no
new snapshot to diff: it reports that its own subject can no longer be read. Like
the terminal GitHub events it closes the entity and every task on it, so what it
takes to emit one is deliberately strict.

**It does not mean the issue was definitely deleted.** Jira answers a request for
an issue you can't see exactly the way it answers one for an issue that doesn't
exist — a 404, deliberately, so that existence isn't disclosed. Deletion is the
usual cause, but a permission-scheme change, a project move, or a
narrowed/rotated credential produce the identical answer, and nothing on our side
can tell them apart. The event is named for what was observed rather than what
probably happened. If one shows up for an issue you can still see in the browser,
check the credential's access before concluding anything was deleted.

Both causes leave the issue equally untrackable, which is why they share one
event type instead of splitting on a discriminator nothing can actually read.

A tracked issue simply missing from a poll's search results is **not** enough to
emit it. An issue can drop out of a search while still perfectly readable — an
index that hasn't caught up, an archived issue or project, a key that moved — so
absence only starts a clock. Once a key has gone unanswered for long enough, the
poller asks Jira about that one issue directly, and emits this event **only** on
a 404 from that request.

The other outcomes deliberately change nothing. An issue that resolves but never
appears in search results is logged as such and stays tracked — its entity is
being skipped by something other than unreachability, and closing it would
destroy live work. A confirmation that fails for any other reason is not evidence
either way, and is retried on a later cycle.

Metadata is the entity's last-known state (assignee, project, issue type, last
status, summary), since the source has nothing left to read. There is no
`dedup_key` — a key can only stop resolving once.

## Linear Events

A Linear team is the tracked-set unit, the way a Jira project is. Each armed
team gets two discovery queries per cycle: unassigned issues in its pickup
states, and issues assigned to the credential's own user outside its done
states. Under an app install the credential's user is the app user, which
nothing is assigned to, so the second query returns nothing.

### Actionable

| Event | ID | Trigger | `dedup_key` |
|-------|----|---------|-------------|
| **Issue Assigned** | `linear:issue:assigned` | The assignee changes to someone, or an issue is first seen through the assigned-to-credential query | — |
| **Issue Available** | `linear:issue:available` | The assignee is cleared | — |
| **Priority Changed** | `linear:issue:priority_changed` | `priority` changes | new priority label |
| **New Comment** | `linear:issue:commented` | The newest comment's id changes to a different comment (fires once per cycle regardless of how many comments were added; Linear exposes no comment count) | — |
| **Issue Became Atomic** | `linear:issue:became_atomic` | The last open sub-issue closes, on an issue not itself done | — |

### Informational

| Event | ID | Trigger | `dedup_key` |
|-------|----|---------|-------------|
| **Status Changed** | `linear:issue:status_changed` | The workflow state changes. A state renamed in Linear is not a change: states compare by id | new state name |
| **Issue Completed** | `linear:issue:completed` | The state enters one of the team's done states (fires beside `status_changed`) | — |
| **Issue Body Updated** | `linear:issue:body_updated` | The description changes, including clearing it | — |
| **Parent Changed** | `linear:issue:parent_changed` | The issue moves under another parent, or loses its parent | new parent's UUID, or `none` |
| **Identifier Changed** | `linear:issue:identifier_changed` | The issue answers under a new identifier: it moved to another team, or its team's key was renamed — see below | — |
| **Issue Unreachable** | `linear:issue:unreachable` | TF will no longer follow a tracked issue — see below | — |

An issue first seen through the pickup query is seeded quietly, as a Jira
issue is: discovery records it without an event, and later changes are diffed
against that seed. One first seen through the assigned-to-credential query
emits `assigned` with its seed, because being found by that query is itself
the assignment. An issue first seen already in a done state is closed without
an event.

An issue with open sub-issues is a container, not a unit of work: assignment
and unassignment emit no `assigned`/`available` while any sub-issue is open,
and `became_atomic` is the event that surfaces it once the last one closes.
A sub-issue is open when its state is not a done state of any armed team; a
sub-issue in a team nobody tracks therefore counts as open.

Every Linear event's metadata carries the issue's identity block —
`issue_identifier`, `issue_id`, `linear_team_id`, `linear_team_key`,
`assignee`, `assignee_user_id`, `title` — plus the fields its event adds
(`old_status`/`new_status`, `old_priority`/`new_priority`,
`previous_body_hash`/`body_hash`, `old_parent`/`new_parent`, `final_status`,
`last_status`, `reason`, `comment_id`, `old_identifier`/`old_linear_team_id`/
`old_linear_team_key`). `linear_team_id` is what the router's team gate reads
(with `old_linear_team_id` on `identifier_changed`, see below), and
`assignee_user_id` is what assignee-centric routing joins against a
member's bound Linear identity.

#### Identity

An issue's identifier (`ENG-123`) is a display key: moving the issue to
another team gives it a new one, and renaming a team's key changes every
identifier in the team. Its UUID never changes. So a Linear entity's
`source_id` is the identifier — what everything displays — and its
`external_id` is the UUID, which is what TF matches it on. Entities are keyed
within a `scope`, the org's Linear workspace id, because identifiers repeat
across workspaces.

When a tracked issue answers under a new identifier, the entity is renamed in
the same cycle: its `source_id` and `url`, and the target of every artifact
recorded against it, move to the new identifier, and it keeps its tasks,
conversations and memory. A Linear artifact is keyed on the issue's UUID, so
its key does not move, and an artifact on another workspace's issue under the
same identifier is a different row that the rename does not touch.

The refresh emits `identifier_changed` first, ahead of anything else it found
(a move to another team usually changes the issue's workflow state too), in the
same commit as the new snapshot. Predicates can filter it on `linear_team_key`
and `old_linear_team_key`. It is the one Linear event the team gate passes for
a team that tracks either the issue's current Linear team or the one it left,
so a team still hears about an issue that moved to a team it does not track.
Its tasks stay on the entity after such a move, and every later event goes
only to the teams tracking the issue's new team.

If no rule arms the team the issue moved to, TF has nothing to follow it with:
the entity is renamed, `identifier_changed` is emitted, and it retires as
`unreachable` with reason `moved`. That event names the team the issue left,
which is the team that was tracking it. An issue that moved and was also
trashed or archived before a cycle saw it is handled the same way: renamed,
`identifier_changed`, then `unreachable` with reason `trashed` or `archived`.

A closed issue that reopens is matched by its UUID too, so it reactivates its
original entity even after a move. The cycle that reactivates it renames it
and still emits `identifier_changed`, naming the identifier and team it was
closed under.

A team key that is freed and reused can briefly name two issues: a new issue
gets `ENG-1` before TF has seen the old `ENG-1` move. The new one is skipped
until the old one's refresh renames it, and is picked up the cycle after.

#### Issue Unreachable

Like Jira's, this event closes the entity and every task on it, and it is
never inferred from an issue's absence. Its `reason` says why:

- `not_found`: Linear answers not-found for the issue. An issue missing from a
  batch read is asked about directly, by UUID, and only this answer retires
  it. Any other failure to read it is not evidence either way, and it is asked
  about again on a later cycle. At most 20 issues are asked about one at a
  time per cycle; the rest wait for the next.
- `trashed`: the issue is in the trash.
- `archived`: the issue is archived in a state outside its team's done states.
  Archived in a done state is the ordinary terminal path.
- `moved`: the issue moved to a team no rule arms (see Identity above).
- `scope_changed`: the org's Linear credential now belongs to another
  workspace. Every active issue from the previous workspace retires at the
  start of the next cycle, without Linear being asked: the credential cannot
  see the old workspace. The rows are not moved or reused — an issue in the
  new workspace that happens to share an old identifier is a different issue
  and gets its own entity — and binding the old workspace again finds them by
  UUID.

The event's metadata is the issue's last-known state. Its team comes from the
stored snapshot, or from Linear's answer when there is one, never from the
identifier's prefix. An entity with neither (no snapshot, and not found) names
no team: the event still closes the entity and its tasks, but the team gate
refuses it for every team, so no team's handlers receive it.

#### Team rules and the workspace

A team's Linear rules name team and workflow-state ids of the workspace they
were saved under. They apply only while the org's credential belongs to that
workspace: after a switch, the poller does not ask about them, the team's
settings do not list them, and the router's team gate does not read them. They
stay stored, though, and binding the old workspace again brings them back. A
save under the new workspace never touches them.

#### Rate limits

A request still rate limited after the client's 30 seconds of waiting ends the
org's cycle where it lands. Nothing further is sent to Linear that cycle, the
writes already made stand, and the poll-complete sentinel is not emitted. When
Linear said when the window resets, the org's next cycle is scheduled for then
instead of at its poll interval, and a settings save does not bring it forward.
A rate-limited request says nothing about the issues it asked for, so it never
leads to `unreachable`.

## Slack Events

Slack support is an Enterprise, multi-mode-only feature, configured per-org from **Settings → Slack** (operator setup: [self-hosting/slack.md](../self-hosting/slack.md)). Unlike GitHub and Jira, Slack events don't come from the snapshot-diff poller — they arrive over the app's Events API webhook or Socket Mode connection and are ingested as they happen.

| Event | ID | Trigger |
|-------|----|---------|
| **Message to bot** | `slack:message` | A human addressed the TF bot in a Slack channel — either an explicit @-mention, or a follow-up in a thread the bot already owns (an *engaged thread*) |

Mention-ness is metadata (`SlackMessageMetadata.Mentioned`), not a separate event type — the same taxonomy rule that only splits an event when the two cases are genuinely different situations. A handler can still narrow to explicit mentions with the predicate's `mentioned_only` flag, or to specific channels with `channel_in`.

### Engaged threads

A Slack thread is **engaged** when the bot is the reason it exists:

- its root message @-mentioned the bot, or
- a delegated run posted the root message itself.

Engagement is encoded on the thread's entity as `kind="thread"` (contrast `kind="message"`, a mid-thread summons that @-mentions the bot inside a thread someone else started). Closing the entity ends engagement.

**Every human message in an engaged thread is ingested** and published as `slack:message` (with `mentioned=false`), so a follow-up no longer has to re-@-mention the bot to be heard — when the mention *started* the thread, requiring a second @ was confusing. Explicit @-mentions keep working everywhere, engaged or not (published with `mentioned=true`).

Follow-up ingestion (the un-mentioned `message.channels` / `message.groups` deliveries) is deliberately narrow — the vast majority of channel traffic is dropped before it's even recorded. A follow-up publishes only when all of these hold:

- it's a plain reply or a reply also broadcast to the channel (subtype `""` or `thread_broadcast`) — edits, deletions, joins, and bot messages are dropped;
- it's inside a thread (has a `thread_ts`) — root-channel chatter is never ingested;
- it wasn't authored by the bot itself or any other bot;
- it doesn't explicitly @-mention the bot — that copy is owned by the twin `app_mention` delivery, so dropping it here avoids a double publish;
- the thread's entity already exists, is `kind="thread"`, and is still active.

## System Events

These are internal signals, not shown in the triage UI.

| Event | ID | Trigger |
|-------|----|---------|
| **Poll Complete** | `system:poll:completed` | A tracker refresh cycle finished and processed items. For GitHub, this fires only once a cycle fully wraps its round-robin repo cursor — a cycle interrupted by a rate-limit budget exhaustion saves its resume point and stays silent, so scoring/classification/profiling don't churn on a still-partial cold-start sync |
| **Scoring Complete** | `system:scoring:completed` | AI scoring finished for a batch of tasks |
| **Delegation Complete** | `system:delegation:completed` | An agent delegation run completed successfully |
| **Delegation Failed** | `system:delegation:failed` | An agent delegation run failed |
| **Task Auto-suspended** *(deprecated)* | `system:task:auto_suspended` | Per-task breaker trip; superseded by the per-(entity, prompt) breaker below and no longer emitted |
| **Prompt Auto-suspended** | `system:prompt:auto_suspended` | The per-(entity, prompt) breaker tripped after repeated run failures |
| **Delegation Blocked: Subtasks** | `system:task:delegation_blocked_by_subtasks` | Auto-delegation was skipped for a Jira issue because its parent has open subtasks |
| **Conversation Status** | `system:conversation:status` | A delegated run's status changed (mirrors the `conversation_update` websocket event) |
| **Conversation Activity** | `system:conversation:activity` | A delegated run invoked a tool (mirrors the `message` websocket event, `tool_use` messages only) |
| **Routing Disposition** | `system:routing:disposition` | `Router.HandleEvent` finished handling one event — frozen, source turned off by an org admin, taskless (no handler/owner/unroutable), task created/bumped, or an internal error. Lets an async event source (e.g. Slack) learn synchronously-unavailable routing outcomes |

## Snapshot fields

### GitHub PR Snapshot

The tracker stores these fields for each PR and diffs them between cycles:

- `number`, `title`, `author`, `repo`, `head_repo`, `url`
- `state` (OPEN, CLOSED, MERGED), `is_draft`, `merged`, `mergeable` (MERGEABLE, CONFLICTING, UNKNOWN)
- `head_ref`, `base_ref`, `head_sha`
- `additions`, `deletions`, `changed_files`
- `check_runs[]` — structured per-check-run data for the current head SHA, deduped by name (latest execution wins). Each entry: `id`, `name`, `status`, `conclusion`, `completed_at`, `details_url`, `workflow_run_id`. `details_url` is GitHub's `details_url` — the CI provider's "more info" link (for Actions: `/actions/runs/N/job/M`; for third-party CI: provider-defined), not GitHub's narrower check-run page URL.
- `review_requests[]` — pending reviewer identifiers: user logins for direct requests, `org/slug` for team requests
- `reviews[]` — latest review per reviewer (author, state, submitted_at)
- `review_count` — total number of reviews submitted
- `labels[]`, `comment_count`, `updated_at`

### Jira Issue Snapshot

- `key`, `summary`, `url`
- `status`, `assignee`, `priority`
- `labels[]`, `issue_type`, `parent_key`
- `comment_count`

### Linear Issue Snapshot

The entity itself carries `source_id` (the identifier), `external_id` (the
UUID, which it is matched on) and `scope` (the org's Linear workspace id).

- `id` (UUID), `identifier` (the entity's `source_id`), `title`, `url`
- `body_hash` — fingerprint of the raw markdown description; the description itself is mirrored onto the entity, capped at 2,000 codepoints
- `state` — `{id, name, type}`; compared by id
- `assignee` (display name), `assignee_user_id`
- `priority` (0 = none, 1 = urgent … 4 = low), `priority_label`
- `labels[]`
- `team_id`, `team_key`
- `parent_id`, `parent_identifier`
- `last_comment_id`, `last_comment_at`
- `open_child_count` — sub-issues not in an armed team's done states
- `created_at`, `updated_at`, `archived`, `trashed`

Turning Linear off for an org clears these snapshots, as it does for any
source. The factory belt places a Linear issue by its snapshot's `team_id`, so
while Linear is off its issues are not on the belt. They return on the first
cycle after Linear is turned back on: an entity with no snapshot is read by
UUID in the same batches as the rest and seeded without events. Jira issues
stay on the belt through a pause, because a Jira key's prefix is its project.

## Event lifecycle

1. **First seen** — when an item is first discovered, an initial event is emitted based on its current state (e.g. `review_requested` if the PR has pending review requests, `opened` if it's an authored PR, `mentioned` if discovered via mentions query)
2. **Transitions** — subsequent cycles compare snapshots and emit events for any field changes
3. **Terminal** — when an item reaches a terminal state (merged, closed, done), it's marked with `terminal_at` and excluded from future refresh cycles. Terminal items are retained indefinitely for dashboard statistics.
4. **Reactivation** — if a terminal item reappears in discovery (e.g. a closed PR is reopened), `terminal_at` is cleared and tracking resumes

## Configuration

Events can be enabled/disabled on the Event Types settings page. Disabling an event type hides it from the triage queue but does not stop the tracker from detecting it — events are still recorded and can trigger delegation rules.
