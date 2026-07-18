# Tracked Events

Triage Factory monitors GitHub PRs and Jira issues for state changes and emits typed events when transitions are detected. Events power the triage queue, AI scoring, delegation triggers, and the dashboard.

## How it works

The tracker runs on a configurable poll interval (default: 5 minutes). Each cycle:

1. **Discover** — search queries find new items to track
2. **Register** — new items are stored in `tracked_items` with an initial snapshot
3. **Refresh** — all tracked items are batch-fetched (GitHub via GraphQL `nodes(ids:[...])`, Jira via `key IN (...)` JQL)
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

## Slack Events

Slack support is an Enterprise, multi-mode-only feature, configured per-org from **Settings → Slack** (operator setup: [self-hosting/slack.md](../self-hosting/slack.md)). Unlike GitHub and Jira, Slack events don't come from the snapshot-diff poller — they arrive over the app's Events API webhook or Socket Mode connection and are ingested as they happen.

| Event | ID | Trigger |
|-------|----|---------|
| **Message to bot** | `slack:message` | A human addressed the TF bot in a Slack channel — either an explicit @-mention, or a follow-up in a thread the bot already owns (an *engaged thread*) |

`slack:message` was formerly `slack:mention`. Whether the message carried an explicit @-mention doesn't change whether the situation needs attention, so mention-ness is metadata (`SlackMessageMetadata.Mentioned`), not a separate event type — the same taxonomy rule that only splits an event when the two cases are genuinely different situations. A handler can still narrow to explicit mentions with the predicate's `mentioned_only` flag, or to specific channels with `channel_in`.

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
| **Run Status** | `system:run:status` | A delegated run's status changed (mirrors the `agent_run_update` websocket event) |
| **Run Activity** | `system:run:activity` | A delegated run invoked a tool (mirrors the `agent_message` websocket event, `tool_use` messages only) |
| **Routing Disposition** | `system:routing:disposition` | `Router.HandleEvent` finished handling one event — frozen, taskless (no handler/owner/unroutable), task created/bumped, or an internal error. Lets an async event source (e.g. Slack) learn synchronously-unavailable routing outcomes |

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

## Event lifecycle

1. **First seen** — when an item is first discovered, an initial event is emitted based on its current state (e.g. `review_requested` if the PR has pending review requests, `opened` if it's an authored PR, `mentioned` if discovered via mentions query)
2. **Transitions** — subsequent cycles compare snapshots and emit events for any field changes
3. **Terminal** — when an item reaches a terminal state (merged, closed, done), it's marked with `terminal_at` and excluded from future refresh cycles. Terminal items are retained indefinitely for dashboard statistics.
4. **Reactivation** — if a terminal item reappears in discovery (e.g. a closed PR is reopened), `terminal_at` is cleared and tracking resumes

## Configuration

Events can be enabled/disabled on the Event Types settings page. Disabling an event type hides it from the triage queue but does not stop the tracker from detecting it — events are still recorded and can trigger delegation rules.
